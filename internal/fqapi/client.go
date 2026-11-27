package fqapi

import (
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	aid             = "1967"
	versionCode     = "68132"
	updateVersionCd = "68132"
	appName         = "novelapp"
	devicePlatform  = "android"
	userAgentApp    = "com.dragon.read.oversea.gp/68132 (Linux; U; Android 12; zh_CN; ANA-AN00; Build/V417IR;tt-ok/3.12.13.4-tiktok)"

	baseAPI = "https://api5-normal-sinfonlineb.fqnovel.com"
	baseWeb = "https://fanqienovel.com"

	registerURL = "https://log5-applog.fqnovel.com/service/2/device_register/"
	keyURL      = "https://reading.snssdk.com/reading/crypt/registerkey"

	masterKeyHex    = "ac25c67ddd8f38c1b37a2348828e222e"
	keySecretBase64 = "rCXGfd2POMGzeiNIgo4iLg=="
)

// APIError 表示上游返回的错误。
type APIError struct {
	Status int
	Body   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("fqapi: %s (status=%d body=%.200s)", e.Msg, e.Status, e.Body)
}

// Device 保存设备注册结果。
type Device struct {
	DeviceID   string `json:"device_id"`
	InstallID  string `json:"install_id"`
	CDID       string `json:"cdid"`
	Brand      string `json:"brand"`
	Model      string `json:"model"`
	OSVersion  string `json:"os_version"`
	DPI        string `json:"dpi"`
	Resolution string `json:"resolution"`

	spec *deviceSpec
}

// Cookie 拼装 install_id cookie。
func (d *Device) Cookie() string {
	return "store-region=cn-zj; store-region-src=did; install_id=" + d.InstallID
}

// Client 是番茄 API 客户端。
type Client struct {
	http          *http.Client
	signer        Signer
	mu            sync.RWMutex
	device        *Device
	key           string // 内容解密密钥（hex）
	cookie        string // 网页会话 cookie
	pool          *DevicePool
	homeResponses sync.Map // 公开首页数据，按上游 URL 缓存
}

// Options 用于配置 Client。
type Options struct {
	Timeout int // 秒
	Signer  Signer
	Device  *Device
}

// New 创建客户端。
func New(opt Options) *Client {
	timeout := 20
	if opt.Timeout > 0 {
		timeout = opt.Timeout
	}
	sg := opt.Signer
	if sg == nil {
		sg = NewPureSigner()
	}
	return &Client{
		http:   &http.Client{Timeout: time.Duration(timeout) * time.Second},
		signer: sg,
		device: opt.Device,
	}
}

// ---- 设备注册 ----

type registerResp struct {
	DeviceID  json.Number `json:"device_id"`
	InstallID json.Number `json:"install_id"`
}

// Register 生成随机设备并完成注册，成功后缓存到 Client。
func (c *Client) Register(ctx context.Context) (*Device, error) {
	dev := randomDevice()
	if err := c.registerDevice(ctx, dev); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.device = dev
	// 内容密钥与设备绑定，换设备后旧密钥已失效，必须重新申请。
	c.key = ""
	c.mu.Unlock()
	return dev, nil
}

// registerDevice 用给定的设备画像完成注册，只填充 dev 的 ID 字段，不改动 Client 状态。
//
// 设备池需要并发注册多台设备，因此注册与「当前设备」解耦。
func (c *Client) registerDevice(ctx context.Context, dev *Device) error {
	if dev == nil {
		return errors.New("设备信息为空")
	}
	now := time.Now().UnixMilli()

	body, err := json.Marshal(map[string]any{
		"magic_tag": "ss_app_log",
		"header":    dev.header(now),
		"_gen_time": now,
	})
	if err != nil {
		return err
	}

	q := url.Values{}
	q.Set("aid", aid)
	q.Set("version_code", versionCode)
	q.Set("channel", "googleplay")
	q.Set("package", "com.dragon.read.oversea.gp")
	q.Set("_rticket", strconv.FormatInt(now, 10))
	q.Set("use_store_region_cookie", "1")
	q.Set("okhttp_version", "4.2.137.76-fanqie")

	stub := strings.ToUpper(md5hex(string(body)))

	hdr := map[string]string{
		"User-Agent":                userAgentApp,
		"Accept":                    "application/json",
		"Content-Type":              "application/json",
		"log-encode-type":           "gzip",
		"x-ss-req-ticket":           strconv.FormatInt(now, 10),
		"x-ss-stub":                 stub,
		"x-vc-bdturing-sdk-version": "3.7.2.cn",
		"Cookie":                    dev.Cookie(),
	}
	for k, v := range c.signFor(q, dev.Cookie(), body) {
		hdr[k] = v
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registerURL+"?"+q.Encode(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var rr registerResp
	if err := json.Unmarshal(raw, &rr); err != nil {
		return &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "设备注册响应不是合法 JSON"}
	}
	if rr.DeviceID.String() == "" {
		return &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "设备注册未返回 device_id"}
	}

	dev.DeviceID = rr.DeviceID.String()
	dev.InstallID = rr.InstallID.String()
	return nil
}

// Device 返回当前设备（可能为 nil）。
func (c *Client) Device() *Device {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.device
}

// SetCookie 设置网页会话 cookie。
func (c *Client) SetCookie(ck string) {
	c.mu.Lock()
	c.cookie = ck
	c.mu.Unlock()
}

// Cookie 返回当前网页会话 cookie。
func (c *Client) Cookie() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cookie
}

// SetKey 手动设置内容解密密钥（hex）。
func (c *Client) SetKey(hexKey string) {
	c.mu.Lock()
	c.key = hexKey
	c.mu.Unlock()
}

// EnsureKey 注册设备并申请内容密钥，只执行一次。
func (c *Client) EnsureKey(ctx context.Context) error {
	c.mu.RLock()
	ok := c.device != nil && c.key != ""
	c.mu.RUnlock()
	if ok {
		return nil
	}
	if err := c.bootstrapPoolDevice(ctx); err != nil {
		return err
	}
	if c.Device() == nil {
		if _, err := c.Register(ctx); err != nil {
			return err
		}
	}
	k, err := c.registerKeyFor(ctx, c.Device())
	if err != nil {
		return err
	}
	c.SetKey(k)
	return nil
}

// registerKeyFor 用指定设备申请内容密钥（不读取 Client 当前设备）。
func (c *Client) registerKeyFor(ctx context.Context, dev *Device) (string, error) {
	if dev == nil || dev.DeviceID == "" {
		return "", errors.New("设备未注册")
	}
	secret, err := base64.StdEncoding.DecodeString(keySecretBase64)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 16)
	for i := range iv {
		iv[i] = byte(i * 7 % 256)
	}
	digits := fmt.Sprintf("%016x", mustAtoi64(dev.DeviceID))
	plain := []byte(digits)

	blk, err := aes.NewCipher(secret)
	if err != nil {
		return "", err
	}
	padded := pkcs7(plain, blk.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(ct, padded)
	content := base64.StdEncoding.EncodeToString(append(iv, ct...))

	payload := fmt.Sprintf(`{"content":%q,"keyver":1}`, content)
	stub := strings.ToUpper(md5hex(payload))

	q := url.Values{}
	q.Set("aid", aid)
	q.Set("device_id", dev.DeviceID)
	q.Set("iid", dev.InstallID)

	hdr := map[string]string{
		"User-Agent":      userAgentApp,
		"Content-Type":    "application/x-www-form-urlencoded",
		"x-ss-req-ticket": strconv.FormatInt(time.Now().UnixMilli(), 10),
		"x-ss-stub":       stub,
	}
	for k, v := range c.signFor(q, dev.Cookie(), []byte(payload)) {
		hdr[k] = v
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, keyURL+"?"+q.Encode(), strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var kr struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &kr); err != nil || kr.Data.Key == "" {
		return "", &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "内容密钥申请失败"}
	}
	return decryptContentKey(kr.Data.Key)
}

func decryptContentKey(encB64 string) (string, error) {
	key, err := hex.DecodeString(masterKeyHex)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 {
		return "", errors.New("密钥数据过短")
	}
	iv, ct := raw[:16], raw[16:]
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(pt, ct)

	pt, err = pkcs7Unpad(pt)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(pt)), nil
}

// DecryptChapter 解密章节正文。
func DecryptChapter(encB64, keyHex string) (string, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 {
		return "", errors.New("章节密文过短")
	}
	iv, ct := raw[:16], raw[16:]
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(pt, ct)
	pt, err = pkcs7Unpad(pt)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// ---- 业务接口 ----

// BatchFull 批量拉取章节密文。
//
// 与纯算通道同样处理设备轮换：命中 403/风控空响应时换设备重试。
func (c *Client) BatchFull(ctx context.Context, itemIDs []string, bookID string) (map[string]string, error) {
	out := make(map[string]string)
	err := c.withDeviceRotation(ctx, 3, func() error {
		m, err := c.batchFullOnce(ctx, itemIDs, bookID)
		if err == nil {
			for id, v := range m {
				out[id] = v
			}
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// batchFullOnce 是一次批量取章尝试。
func (c *Client) batchFullOnce(ctx context.Context, itemIDs []string, bookID string) (map[string]string, error) {
	dev := c.Device()
	if dev == nil {
		return nil, errors.New("设备未注册")
	}
	q := url.Values{}
	q.Set("aid", aid)
	q.Set("app_name", appName)
	q.Set("channel", "0")
	q.Set("device_platform", devicePlatform)
	q.Set("device_id", dev.DeviceID)
	q.Set("device_type", dev.Model)
	q.Set("os_version", dev.OSVersion)
	q.Set("version_code", versionCode)
	q.Set("update_version_code", updateVersionCd)
	q.Set("book_id", bookID)
	q.Set("item_ids", strings.Join(itemIDs, ","))
	q.Set("novel_text_type", "1")
	q.Set("req_type", "1")

	now := time.Now().UnixMilli()
	hdr := map[string]string{
		"User-Agent":                userAgentApp,
		"Accept":                    "application/json; charset=utf-8,application/x-protobuf",
		"Accept-Encoding":           "gzip",
		"Cookie":                    dev.Cookie(),
		"X-Xs-From-Web":             "0",
		"X-Vc-Bdturing-Sdk-Version": "3.7.2.cn",
		"X-Reading-Request":         fmt.Sprintf("%d-1106737160", now),
		"Sdk-Version":               "2",
		"X-Tt-Store-Region-Src":     "did",
		"X-Tt-Store-Region":         "cn-zj",
		"Lc":                        "101",
		"X-Ss-Req-Ticket":           strconv.FormatInt(now, 10),
		"Passport-Sdk-Version":      "50564",
	}
	for k, v := range c.signFor(q, dev.Cookie(), nil) {
		hdr[k] = v
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseAPI+"/reading/reader/batch_full/v?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, gzErr := gzip.NewReader(resp.Body)
		if gzErr == nil {
			defer gz.Close()
			reader = gz
		}
	}
	raw, _ := io.ReadAll(reader)
	if len(raw) == 0 {
		return nil, &APIError{Status: resp.StatusCode, Msg: "网关返回空响应（签名未通过风控）"}
	}

	var jr struct {
		Code int `json:"code"`
		Data map[string]struct {
			Content string `json:"content"`
			Title   string `json:"title"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &jr); err != nil {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "响应解析失败"}
	}
	if jr.Code != 0 {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: fmt.Sprintf("业务错误 code=%d", jr.Code)}
	}

	out := make(map[string]string, len(jr.Data))
	for id, v := range jr.Data {
		out[id] = v.Content
	}
	return out, nil
}

// ---- 网页端接口（免签，用于搜索与目录） ----

// WebBook 包含书籍元信息与目录。
type WebBook struct {
	BookID   string        `json:"bookId"`
	Title    string        `json:"title"`
	Author   string        `json:"author"`
	Chapters []ChapterMeta `json:"chapters"`
}

// WebBookInfo 通过网页接口取书籍元信息与目录（免签）。
func (c *Client) WebBookInfo(ctx context.Context, bookID string) (*WebBook, error) {
	u := baseWeb + "/api/reader/directory/detail?bookId=" + url.QueryEscape(bookID)
	var jr struct {
		Data struct {
			BookName   string `json:"bookName"`
			Author     string `json:"author"`
			VolumeList []struct {
				VolumeName string `json:"volumeName"`
			} `json:"volumeList"`
			ChapterListWithVolume [][]struct {
				ItemID        string `json:"itemId"`
				Title         string `json:"title"`
				IsChapterLock bool   `json:"isChapterLock"`
			} `json:"chapterListWithVolume"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, u, &jr); err != nil {
		return nil, err
	}
	book := &WebBook{BookID: bookID, Title: jr.Data.BookName, Author: jr.Data.Author}
	idx := 1
	for _, vol := range jr.Data.ChapterListWithVolume {
		for _, ch := range vol {
			book.Chapters = append(book.Chapters, ChapterMeta{
				ItemID: ch.ItemID,
				Title:  ch.Title,
				Index:  idx,
				Locked: ch.IsChapterLock,
			})
			idx++
		}
	}
	return book, nil
}

// WebDirectory 通过网页接口取目录（免签）。
func (c *Client) WebDirectory(ctx context.Context, bookID string) ([]ChapterMeta, error) {
	b, err := c.WebBookInfo(ctx, bookID)
	if err != nil {
		return nil, err
	}
	return b.Chapters, nil
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Referer", baseWeb+"/")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, v); err != nil {
		return &APIError{Status: resp.StatusCode, Body: string(raw), Msg: "JSON 解析失败"}
	}
	return nil
}

// ---- 小工具 ----

func pkcs7(b []byte, size int) []byte {
	n := size - len(b)%size
	out := make([]byte, len(b)+n)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, errors.New("空数据")
	}
	n := int(b[len(b)-1])
	if n == 0 || n > len(b) || n > aes.BlockSize {
		return nil, errors.New("填充非法")
	}
	return b[:len(b)-n], nil
}

func mustAtoi64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// ChapterMeta 章节元信息。
type ChapterMeta struct {
	ItemID string `json:"itemId"`
	Title  string `json:"title"`
	Index  int    `json:"index"`
	Locked bool   `json:"locked"`
}

var _ = md5.Sum
