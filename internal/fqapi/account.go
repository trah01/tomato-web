package fqapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// 番茄账号（网页端 passport 会话）。
//
// 登录流程走番茄官网同源 passport，不经过 sso.douyin.com（后者风控严格）：
//
//	GET /passport/web/get_qrcode/      -> {qrcode(PNG base64), token, expire_time}
//	GET /passport/web/check_qrconnect/ -> {data:{status:"new"|"scanned"|"confirmed", redirect_url}}
//
// 关键点：参数名是 next（不是 service）；aid 必须是 2503（1967 会返回「该应用无权限」）。
// 扫码确认后跟随 redirect_url 即可拿到 sessionid 等 Cookie，会话的 cookie jar 由
// 本结构持有，登录态直接用 /api/user/info/v2 校验。
//
// 登录后可用能力：
//   - 书架 /reading/bookapi/bookshelf/info/v:version/
//   - 阅读进度 /api/reader/book/progress（读）与 /api/reader/book/update_progress（写）
const (
	passportAID    = "2503"
	passportNext   = "https://fanqienovel.com/"
	userInfoPath   = "/api/user/info/v2"
	shelfInfoPath  = "/reading/bookapi/bookshelf/info/v:version/"
	shelfAddPath   = "/reading/bookapi/bookshelf/add/v:version"
	progressPath   = "/api/reader/book/progress"
	updateProgPath = "/api/reader/book/update_progress"
)

// UserInfo 账号基本信息。
type UserInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
	IsVip  bool   `json:"isVip"`
}

// ShelfBook 书架条目。
type ShelfBook struct {
	BookID        string `json:"bookId"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	Cover         string `json:"cover"`
	ChapterTitle  string `json:"chapterTitle"`
	LastReadItem  string `json:"lastReadItemId"`
	SerialCount   int    `json:"serialCount"`
	CreationState string `json:"creationStatus"`
	ReadTimestamp int64  `json:"readTimestamp"`
}

// Progress 单本书的云端阅读进度。
type Progress struct {
	BookID        string `json:"bookId"`
	ItemID        string `json:"itemId"`
	ReadTimestamp int64  `json:"readTimestamp"`
}

// Account 管理番茄账号会话，会话持久化到磁盘。
type Account struct {
	mu        sync.RWMutex
	client    *http.Client
	jar       *cookiejar.Jar
	user      *UserInfo
	storePath string
	ua        string
	lastErr   string
}

// NewAccount 创建账号管理器。storePath 为空则不持久化。
func NewAccount(storePath string) *Account {
	jar, _ := cookiejar.New(nil)
	a := &Account{
		jar:       jar,
		storePath: storePath,
		client:    &http.Client{Timeout: 25 * time.Second, Jar: jar},
		ua:        "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}
	return a
}

// ---- 会话持久化 ----

type savedSession struct {
	User   *UserInfo `json:"user"`
	Domain string    `json:"domain"`
	Cookie string    `json:"cookie"`
}

// Load 从磁盘恢复会话，返回仍在有效期的用户（失败或未登录返回 nil）。
func (a *Account) Load(ctx context.Context) *UserInfo {
	if a.storePath == "" {
		return nil
	}
	raw, err := os.ReadFile(a.storePath)
	if err != nil {
		return nil
	}
	var sv savedSession
	if err := json.Unmarshal(raw, &sv); err != nil || sv.Cookie == "" {
		return nil
	}
	a.setCookieHeader(sv.Cookie)
	// 校验：Cookie 可能已过期
	u, err := a.fetchUserInfo(ctx)
	if err != nil || u == nil {
		return nil
	}
	a.mu.Lock()
	a.user = u
	a.mu.Unlock()
	return u
}

// AttachCookie 用外部提供的 Cookie 恢复会话，供「仅当前浏览器保存」模式使用。
//
// 与 Load 的区别：不读磁盘、不写磁盘，Cookie 由调用方（浏览器 localStorage）提供。
// 校验失败时返回 nil，会话保持未登录状态。
func (a *Account) AttachCookie(ctx context.Context, raw string) *UserInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	a.setCookieHeader(raw)
	u, err := a.fetchUserInfo(ctx)
	if err != nil || u == nil {
		return nil
	}
	a.mu.Lock()
	a.user = u
	a.mu.Unlock()
	return u
}

// Save 把当前会话写入磁盘。
func (a *Account) Save() error {
	if a.storePath == "" {
		return nil
	}
	a.mu.RLock()
	ck := a.cookieHeader()
	u := a.user
	a.mu.RUnlock()

	if ck == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(a.storePath), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(savedSession{User: u, Domain: "fanqienovel.com", Cookie: ck}, "", "  ")
	if err != nil {
		return err
	}
	// 含登录凭据，仅本人可读
	return os.WriteFile(a.storePath, raw, 0o600)
}

// cookieHeader 汇总当前 jar 中的 Cookie。
func (a *Account) cookieHeader() string {
	u, _ := url.Parse(baseWeb + "/")
	var parts []string
	for _, c := range a.jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// setCookieHeader 把 "k=v; k=v" 写回 jar。
func (a *Account) setCookieHeader(raw string) {
	u, _ := url.Parse(baseWeb + "/")
	var cs []*http.Cookie
	for _, seg := range strings.Split(raw, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		k, v, ok := strings.Cut(seg, "=")
		if !ok {
			continue
		}
		cs = append(cs, &http.Cookie{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v), Path: "/"})
	}
	if len(cs) > 0 {
		a.jar.SetCookies(u, cs)
	}
}

// LoginWithCookie 用浏览器复制的 Cookie 建立会话。
//
// 在 fanqienovel.com 登录后按 F12 → Network → 任选一个请求，
// 复制 Request Headers 中 Cookie 整行的值即可（需包含 sessionid）。
func (a *Account) LoginWithCookie(ctx context.Context, raw string) (*UserInfo, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "sessionid") {
		return nil, fmt.Errorf("Cookie 中缺少 sessionid，请复制完整的一行")
	}
	a.setCookieHeader(raw)

	u, err := a.fetchUserInfo(ctx)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("Cookie 已失效或不属于有效账号")
	}
	a.mu.Lock()
	a.user = u
	a.mu.Unlock()
	if err := a.Save(); err != nil {
		a.setErr("会话已建立但保存失败: " + err.Error())
	}
	return u, nil
}

// Logout 清除会话与本地存储。
func (a *Account) Logout() error {
	a.mu.Lock()
	a.jar, _ = cookiejar.New(nil)
	a.client.Jar = a.jar
	a.user = nil
	a.mu.Unlock()
	if a.storePath != "" {
		if err := os.Remove(a.storePath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// ---- HTTP 基础 ----

func (a *Account) webHeaders(req *http.Request) {
	req.Header.Set("User-Agent", a.ua)
	req.Header.Set("Referer", baseWeb+"/")
	req.Header.Set("Origin", baseWeb)
	req.Header.Set("Accept", "application/json, text/plain, */*")
}

func (a *Account) do(req *http.Request) ([]byte, int, error) {
	a.webHeaders(req)
	resp, err := a.client.Do(req)
	if err != nil {
		a.setErr(err.Error())
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

func (a *Account) get(ctx context.Context, fullURL string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return nil, 0, err
	}
	return a.do(req)
}

func (a *Account) postJSON(ctx context.Context, fullURL string, body any) ([]byte, int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, strings.NewReader(string(buf)))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	return a.do(req)
}

func (a *Account) setErr(s string) {
	a.mu.Lock()
	a.lastErr = s
	a.mu.Unlock()
}

// ---- 登录 ----

// passportQuery 拼 passport 公共参数。
func passportQuery() url.Values {
	q := url.Values{}
	q.Set("aid", passportAID)
	q.Set("app_name", "novelapp")
	q.Set("version_code", "57700")
	q.Set("device_platform", "web")
	q.Set("channel", "novel")
	q.Set("sdk_version", "1.6.1")
	q.Set("passport_sdk_version", "2.0.0")
	q.Set("new_user", "0")
	return q
}

// QRCode 扫码登录票据。
type QRCode struct {
	Token  string `json:"token"`
	Image  string `json:"image"` // data:image/png;base64,...
	Text   string `json:"text"`  // 二维码指向的链接（备用）
	Expire int64  `json:"expireAt"`
}

// StartQRLogin 获取登录二维码。
func (a *Account) StartQRLogin(ctx context.Context) (*QRCode, error) {
	q := passportQuery()
	q.Set("next", passportNext)
	u := baseWeb + "/passport/web/get_qrcode/?" + q.Encode()

	raw, status, err := a.get(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, &APIError{Status: status, Msg: "获取二维码失败：服务端返回空响应"}
	}

	var jr struct {
		Data struct {
			Token       string      `json:"token"`
			QRToken     string      `json:"qr_token"`
			QRCode      string      `json:"qrcode"`
			QRCodeURL   string      `json:"qrcode_index_url"`
			ExpireTime  json.Number `json:"expire_time"`
			ErrorCode   json.Number `json:"error_code"`
			Description string      `json:"description"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, &APIError{Status: status, Body: string(raw), Msg: "二维码响应解析失败"}
	}
	d := jr.Data
	if code := d.ErrorCode.String(); code != "" && code != "0" {
		return nil, &APIError{Status: status, Body: string(raw), Msg: "获取二维码失败：" + firstNonEmptyStr(d.Description, "错误码 "+code)}
	}
	token := firstNonEmptyStr(d.Token, d.QRToken)
	if token == "" {
		return nil, &APIError{Status: status, Body: string(raw), Msg: "获取二维码失败：缺少 token"}
	}

	img := ""
	if d.QRCode != "" {
		img = "data:image/png;base64," + d.QRCode
	} else {
		img = d.QRCodeURL
	}
	exp, _ := d.ExpireTime.Int64()
	return &QRCode{Token: token, Image: img, Text: d.QRCodeURL, Expire: exp}, nil
}

// QRStatus 轮询结果。
type QRStatus struct {
	State      string `json:"state"` // waiting | scanned | confirmed
	Message    string `json:"message"`
	Redirected string `json:"-"`
}

// PollQRLogin 查询一次扫码状态。
func (a *Account) PollQRLogin(ctx context.Context, token string) (*QRStatus, error) {
	q := passportQuery()
	q.Set("next", passportNext)
	q.Set("token", token)
	u := baseWeb + "/passport/web/check_qrconnect/?" + q.Encode()

	raw, _, err := a.get(ctx, u)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return &QRStatus{State: "waiting", Message: "等待扫码…"}, nil
	}

	var jr struct {
		Data struct {
			Status      string      `json:"status"`
			RedirectURL string      `json:"redirect_url"`
			ErrorCode   json.Number `json:"error_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		// 轮询期间的杂散响应不作为错误处理
		return &QRStatus{State: "waiting", Message: "等待扫码…"}, nil
	}
	d := jr.Data
	st := strings.ToLower(d.Status)
	redirect := firstNonEmptyStr(d.RedirectURL, "")

	if redirect != "" || matchesAny(st, "confirm", "success", "done", "ok", "accept") {
		return &QRStatus{State: "confirmed", Message: "已确认，正在完成登录…", Redirected: redirect}, nil
	}
	if matchesAny(st, "scan") {
		return &QRStatus{State: "scanned", Message: "已扫码，请在手机上点击「确认登录」"}, nil
	}
	return &QRStatus{State: "waiting", Message: "等待扫码…"}, nil
}

// FinishQRLogin 跟随跳转建立会话，并校验登录态。
func (a *Account) FinishQRLogin(ctx context.Context, redirect string) (*UserInfo, error) {
	targets := []string{}
	if redirect != "" {
		targets = append(targets, redirect)
	}
	if redirect != passportNext {
		targets = append(targets, passportNext)
	}

	var lastErr error
	for _, t := range targets {
		if t == "" {
			continue
		}
		if _, _, err := a.get(ctx, t); err != nil {
			lastErr = err
			continue
		}
		if u, err := a.fetchUserInfo(ctx); err == nil && u != nil {
			a.mu.Lock()
			a.user = u
			a.mu.Unlock()
			if err := a.Save(); err != nil {
				a.setErr("会话已建立但保存失败: " + err.Error())
			}
			return u, nil
		}
	}
	if u, err := a.fetchUserInfo(ctx); err == nil && u != nil {
		a.mu.Lock()
		a.user = u
		a.mu.Unlock()
		_ = a.Save()
		return u, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("扫码已确认，但未能建立会话")
	}
	a.setErr(lastErr.Error())
	return nil, lastErr
}

// fetchUserInfo 调用 /api/user/info/v2 校验并返回用户信息。
func (a *Account) fetchUserInfo(ctx context.Context) (*UserInfo, error) {
	raw, status, err := a.get(ctx, baseWeb+userInfoPath)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, &APIError{Status: status, Msg: "未登录"}
	}
	var jr struct {
		Data struct {
			ID     json.Number `json:"id"`
			Name   string      `json:"name"`
			Avatar string      `json:"avatar"`
			IsVip  bool        `json:"isVip"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, &APIError{Status: status, Body: string(raw), Msg: "用户信息解析失败"}
	}
	id, _ := jr.Data.ID.Int64()
	if id <= 1 {
		return nil, nil
	}
	return &UserInfo{
		ID:     jr.Data.ID.String(),
		Name:   jr.Data.Name,
		Avatar: jr.Data.Avatar,
		IsVip:  jr.Data.IsVip,
	}, nil
}

// ---- 会话状态 ----

// Status 返回当前账号状态。
func (a *Account) Status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := map[string]any{
		"loggedIn":  a.user != nil,
		"lastError": a.lastErr,
	}
	if a.user != nil {
		out["user"] = a.user
	}
	return out
}

// User 返回已登录用户（未登录为 nil）。
func (a *Account) User() *UserInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.user
}

// LoggedIn 判断是否已登录。
func (a *Account) LoggedIn() bool {
	return a.User() != nil
}

// Cookie 返回当前会话 Cookie 串（供 Client 复用，获取登录态下的正文）。
func (a *Account) Cookie() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cookieHeader()
}

// ---- 云端书架与阅读进度 ----

// appQuery 构造网页端 APP 接口公共参数。
func appQuery() url.Values {
	return url.Values{
		"aid":                 {"1967"},
		"app_name":            {"novelapp"},
		"version_code":        {"57700"},
		"update_version_code": {"57700"},
		"device_platform":     {"web"},
	}
}

// requireLogin 未登录时返回统一错误。
func (a *Account) requireLogin() error {
	if !a.LoggedIn() {
		return fmt.Errorf("尚未登录番茄账号")
	}
	return nil
}

// readProgressMap 读取云端阅读进度，返回 bookID -> Progress。
func (a *Account) readProgressMap(ctx context.Context) (map[string]Progress, error) {
	out := map[string]Progress{}
	raw, status, err := a.get(ctx, baseWeb+progressPath)
	if err != nil {
		return nil, err
	}
	var jr struct {
		Code *int          `json:"code"`
		Data []ProgressRec `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil || jr.Code == nil || *jr.Code != 0 {
		return nil, &APIError{Status: status, Msg: "读取云端阅读进度失败"}
	}
	for _, p := range jr.Data {
		if p.BookID == "" {
			continue
		}
		out[p.BookID] = Progress{
			BookID:        p.BookID,
			ItemID:        p.ItemID,
			ReadTimestamp: numToInt64(p.ReadTimestamp),
		}
	}
	return out, nil
}

type ProgressRec struct {
	BookID        string      `json:"book_id"`
	ItemID        string      `json:"item_id"`
	ReadTimestamp json.Number `json:"read_timestamp"`
}

// ReadProgress 返回云端阅读进度列表。
func (a *Account) ReadProgress(ctx context.Context) ([]Progress, error) {
	if err := a.requireLogin(); err != nil {
		return nil, err
	}
	m, err := a.readProgressMap(ctx)
	if err != nil {
		return nil, err
	}
	list := make([]Progress, 0, len(m))
	for _, p := range m {
		list = append(list, p)
	}
	// 最近阅读优先
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].ReadTimestamp > list[j-1].ReadTimestamp; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	return list, nil
}

// ReadHistory 返回按最近阅读时间排列的书籍和当前章节。
func (a *Account) ReadHistory(ctx context.Context) ([]ShelfBook, error) {
	progress, err := a.ReadProgress(ctx)
	if err != nil {
		return nil, err
	}
	books := make([]ShelfBook, 0, len(progress))
	for _, p := range progress {
		if p.ItemID != "" && p.ItemID != "0" {
			books = append(books, ShelfBook{BookID: p.BookID, LastReadItem: p.ItemID, ReadTimestamp: p.ReadTimestamp})
		}
	}
	if len(books) != 0 {
		a.fillChapterTitles(ctx, books)
	}
	var missing []string
	for _, book := range books {
		if book.Title == "" {
			missing = append(missing, book.BookID)
		}
	}
	if len(missing) != 0 {
		info := a.bookSimpleInfo(ctx, missing)
		for i := range books {
			if books[i].Title == "" {
				book := info[books[i].BookID]
				books[i].Title, books[i].Author, books[i].Cover = book.Title, book.Author, book.Cover
			}
		}
	}
	return books, nil
}

func (a *Account) shelfBookIDs(ctx context.Context) ([]string, error) {
	if err := a.requireLogin(); err != nil {
		return nil, err
	}

	q := appQuery()
	q.Set("iid", "0")
	raw, status, err := a.get(ctx, baseWeb+shelfInfoPath+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, &APIError{Status: status, Msg: "书架接口返回空响应（可能登录态已失效）"}
	}

	var jr struct {
		Code *int   `json:"code"`
		Msg  string `json:"message"`
		Data struct {
			Shelf []struct {
				BookID string `json:"book_id"`
			} `json:"book_shelf_info"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, &APIError{Status: status, Body: string(raw), Msg: "书架响应解析失败"}
	}
	if status != http.StatusOK || jr.Code == nil || *jr.Code != 0 {
		return nil, &APIError{Status: status, Body: string(raw), Msg: firstNonEmptyStr(jr.Msg, "获取书架失败")}
	}
	ids := make([]string, 0, len(jr.Data.Shelf))
	for _, book := range jr.Data.Shelf {
		if book.BookID != "" {
			ids = append(ids, book.BookID)
		}
	}
	return ids, nil
}

func (a *Account) HasShelfBook(ctx context.Context, bookID string) (bool, error) {
	ids, err := a.shelfBookIDs(ctx)
	return slices.Contains(ids, bookID), err
}

func (a *Account) AddShelfBook(ctx context.Context, bookID string) error {
	if err := a.requireLogin(); err != nil {
		return err
	}
	q := appQuery()
	q.Set("iid", "0")
	raw, status, err := a.postJSON(ctx, baseWeb+shelfAddPath+"?"+q.Encode(), map[string]any{
		"identify_data": []map[string]any{{
			"book_id": bookID, "book_type": 0, "asterisked": false, "modify_time": time.Now().UnixMilli(),
		}},
		"add_book_source": 0,
	})
	if err != nil {
		return err
	}
	var result struct {
		Code *int `json:"code"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &result) != nil || result.Code == nil || *result.Code != 0 {
		return &APIError{Status: status, Msg: "番茄书架未确认添加"}
	}
	return nil
}

// VShelf 返回云端书架（含当前章节与阅读进度）。
func (a *Account) VShelf(ctx context.Context) ([]ShelfBook, error) {
	ids, err := a.shelfBookIDs(ctx)
	if err != nil {
		return nil, err
	}
	books := make([]ShelfBook, 0, len(ids))
	if len(ids) == 0 {
		return books, nil
	}
	progress, _ := a.readProgressMap(ctx)

	for _, id := range ids {
		p := progress[id]
		books = append(books, ShelfBook{
			BookID:        id,
			LastReadItem:  p.ItemID,
			ReadTimestamp: p.ReadTimestamp,
		})
	}

	// 补齐书名/作者/封面：失败不影响书架本身
	simple := a.bookSimpleInfo(ctx, ids)
	for i := range books {
		if s, ok := simple[books[i].BookID]; ok {
			books[i].Title = s.Title
			books[i].Author = s.Author
			books[i].Cover = s.Cover
			books[i].SerialCount = s.SerialCount
			books[i].CreationState = s.CreationState
		}
	}

	// 补齐当前章节标题
	a.fillChapterTitles(ctx, books)
	return books, nil
}

type simpleInfo struct {
	Title         string
	Author        string
	Cover         string
	SerialCount   int
	CreationState string
}

func (a *Account) bookSimpleInfo(ctx context.Context, ids []string) map[string]simpleInfo {
	out := map[string]simpleInfo{}
	raw, _, err := a.postJSON(ctx, baseWeb+"/api/book/simple/info", map[string]any{"book_ids": ids})
	if err != nil || len(raw) == 0 {
		return out
	}
	var jr struct {
		Data struct {
			BookList []struct {
				BookID        string      `json:"book_id"`
				BookName      string      `json:"book_name"`
				AuthorName    string      `json:"author_name"`
				ThumbURL      string      `json:"thumb_url"`
				SerialCount   json.Number `json:"serial_count"`
				CreationState string      `json:"creation_status"`
			} `json:"bookList"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return out
	}
	for _, b := range jr.Data.BookList {
		out[b.BookID] = simpleInfo{
			Title:         b.BookName,
			Author:        b.AuthorName,
			Cover:         b.ThumbURL,
			SerialCount:   int(numToInt64(b.SerialCount)),
			CreationState: b.CreationState,
		}
	}
	return out
}

// fillChapterTitles 通过 /api/bookshelf/multidetail 补齐当前章节标题。
func (a *Account) fillChapterTitles(ctx context.Context, books []ShelfBook) {
	type ref struct {
		BookID string `json:"book_id"`
		ItemID string `json:"item_id"`
	}
	refs := make([]ref, 0, len(books))
	for _, b := range books {
		item := b.LastReadItem
		if item == "" {
			item = "0"
		}
		refs = append(refs, ref{BookID: b.BookID, ItemID: item})
	}
	raw, _, err := a.postJSON(ctx, baseWeb+"/api/bookshelf/multidetail", map[string]any{"books": refs})
	if err != nil || len(raw) == 0 {
		return
	}
	var jr struct {
		Data struct {
			DetailList []struct {
				BookID       string      `json:"book_id"`
				BookName     string      `json:"book_name"`
				AuthorName   string      `json:"author_name"`
				ThumbURL     string      `json:"thumb_url"`
				ItemShowName string      `json:"item_show_title"`
				ItemID       string      `json:"item_id"`
				SerialCount  json.Number `json:"serial_count"`
				CreateState  string      `json:"creation_status"`
			} `json:"detail_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return
	}
	idx := make(map[string]int, len(books))
	for i, b := range books {
		idx[b.BookID] = i
	}
	for _, d := range jr.Data.DetailList {
		i, ok := idx[d.BookID]
		if !ok {
			continue
		}
		if d.BookName != "" {
			books[i].Title = d.BookName
		}
		if d.AuthorName != "" {
			books[i].Author = d.AuthorName
		}
		if d.ThumbURL != "" {
			books[i].Cover = d.ThumbURL
		}
		books[i].ChapterTitle = d.ItemShowName
		if d.ItemID != "" {
			books[i].LastReadItem = d.ItemID
		}
		if n := int(numToInt64(d.SerialCount)); n > 0 {
			books[i].SerialCount = n
		}
		if d.CreateState != "" {
			books[i].CreationState = d.CreateState
		}
	}
}

// UpdateProgress 把阅读进度上报到番茄云端（手机 App 即可续读）。
func (a *Account) UpdateProgress(ctx context.Context, bookID, itemID string, order int) error {
	if err := a.requireLogin(); err != nil {
		return err
	}
	if bookID == "" || itemID == "" {
		return fmt.Errorf("缺少 bookId 或 itemId")
	}
	body := map[string]any{
		"book_id":        bookID,
		"item_id":        itemID,
		"read_progress":  order,
		"index":          order,
		"read_timestamp": time.Now().Unix(),
		"genre_type":     0,
	}
	raw, status, err := a.postJSON(ctx, baseWeb+updateProgPath, body)
	if err != nil {
		return err
	}
	var jr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &jr) == nil && jr.Code != 0 {
		return &APIError{Status: status, Body: string(raw), Msg: firstNonEmptyStr(jr.Message, "进度上报失败")}
	}
	return nil
}

// ---- 小工具 ----

func matchesAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
