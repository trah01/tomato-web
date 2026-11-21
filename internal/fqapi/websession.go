package fqapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"
)

// WebSession 维护网页端会话（cookie + msToken），用于访问需要
// msToken/a_bogus 签名的搜索接口。
//
// 番茄网页端搜索 /api/author/search/search_book/v1 需要：
//  1. 有效会话 cookie（访问首页即可获得 novel_web_id 等）
//  2. msToken 参数
//  3. a_bogus 签名（由 webmssdk 生成，纯算极难）
//
// 因此在服务端启动时先访一次首页拿 cookie，并把 msToken 与 a_bogus
// 通过 /api/web/attach 由浏览器侧注入（浏览器里的 webmssdk 已加载，
// 可以直接调用它生成合法签名）。
type WebSession struct {
	mu      sync.RWMutex
	client  *http.Client
	cookies string
	msToken string
	aBogus  string
	ua      string
	lastErr string
}

// NewWebSession 创建网页会话。
func NewWebSession(timeout time.Duration) *WebSession {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		Proxy:           http.ProxyFromEnvironment,
	}
	return &WebSession{
		client: &http.Client{
			Timeout:   timeout,
			Jar:       jar,
			Transport: tr,
		},
		ua: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
	}
}

// Warmup 访问首页，获取会话 cookie。
func (w *WebSession) Warmup(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseWeb+"/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", w.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := w.client.Do(req)
	if err != nil {
		w.setErr(err.Error())
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	u, _ := url.Parse(baseWeb + "/")
	var parts []string
	for _, c := range w.client.Jar.Cookies(u) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	w.mu.Lock()
	w.cookies = strings.Join(parts, "; ")
	w.mu.Unlock()
	return nil
}

// Attach 注入浏览器侧提供的 msToken 与 a_bogus。
func (w *WebSession) Attach(msToken, aBogus, cookie string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if msToken != "" {
		w.msToken = msToken
	}
	if aBogus != "" {
		w.aBogus = aBogus
	}
	if cookie != "" {
		w.cookies = cookie
	}
}

// Status 返回会话状态。
func (w *WebSession) Status() map[string]any {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return map[string]any{
		"hasCookie":  w.cookies != "",
		"hasMsToken": w.msToken != "",
		"hasABogus":  w.aBogus != "",
		"lastError":  w.lastErr,
	}
}

func (w *WebSession) setErr(s string) {
	w.mu.Lock()
	w.lastErr = s
	w.mu.Unlock()
}

// Get 发起带会话的 GET 请求。
func (w *WebSession) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	w.mu.RLock()
	ms, ab, ck := w.msToken, w.aBogus, w.cookies
	w.mu.RUnlock()

	if ms == "" && ab == "" {
		return nil, fmt.Errorf("网页会话未注入 msToken/a_bogus，请先调用 /api/web/attach")
	}
	query.Set("msToken", ms)
	if ab != "" {
		query.Set("a_bogus", ab)
	}

	u := baseWeb + path + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", w.ua)
	req.Header.Set("Referer", baseWeb+"/")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if ck != "" {
		req.Header.Set("Cookie", ck)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		w.setErr(err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if len(raw) == 0 {
		msg := "网页接口返回空响应"
		w.setErr(msg)
		return nil, &APIError{Status: resp.StatusCode, Msg: msg}
	}
	var probe struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Code != 0 {
		w.setErr(fmt.Sprintf("业务错误 code=%d", probe.Code))
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw), Msg: fmt.Sprintf("业务错误 code=%d", probe.Code)}
	}
	return raw, nil
}
