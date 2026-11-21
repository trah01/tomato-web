package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/fanqieweb/internal/fqapi"
	"github.com/local/fanqieweb/internal/sitesettings"
)

// fanqieCookieHeader 浏览器把「仅当前浏览器保存」的番茄 Cookie 放在这个请求头里。
const fanqieCookieHeader = "X-FQ-Cookie"

// 浏览器会话在服务端内存中的闲置保留时间与最大数量。
const (
	poolIdleTTL = 24 * time.Hour
	poolMaxSize = 64
	qrIdleTTL   = 5 * time.Minute
)

// fanqieAccounts 按设置决定番茄账号存在哪里：
//
//   - server 模式：服务端唯一会话，落盘到数据卷，所有访问者共用。
//   - browser 模式：会话由访问者的浏览器 localStorage 提供，服务端只为
//     这些 Cookie 维护内存中的临时会话，不落盘、不共享。
type fanqieAccounts struct {
	settings *sitesettings.Settings
	server   *fqapi.Account
	logger   *log.Logger

	mu       sync.Mutex
	sessions map[string]*poolEntry // cookie 摘要 -> 会话
	pending  map[string]*qrEntry   // 扫码 token -> 会话
}

type poolEntry struct {
	acct *fqapi.Account
	used time.Time
}

type qrEntry struct {
	acct *fqapi.Account
	used time.Time
}

func newFanqieAccounts(server *fqapi.Account, settings *sitesettings.Settings, logger *log.Logger) *fanqieAccounts {
	return &fanqieAccounts{
		settings: settings,
		server:   server,
		logger:   logger,
		sessions: make(map[string]*poolEntry),
		pending:  make(map[string]*qrEntry),
	}
}

// Mode 返回当前绑定方式。
func (f *fanqieAccounts) Mode() string {
	if f == nil || f.settings == nil {
		return sitesettings.ModeServer
	}
	return f.settings.FanqieMode()
}

// BrowserMode 是否为「仅当前浏览器保存」。
func (f *fanqieAccounts) BrowserMode() bool {
	return f.Mode() == sitesettings.ModeBrowser
}

// Server 返回服务端共享会话（server 模式使用）。
func (f *fanqieAccounts) Server() *fqapi.Account {
	return f.server
}

// Account 返回当前请求应使用的番茄账号；browser 模式下浏览器没带 Cookie 时返回 nil。
func (f *fanqieAccounts) Account(ctx context.Context, r *http.Request) *fqapi.Account {
	if !f.BrowserMode() {
		return f.server
	}
	return f.Session(ctx, r.Header.Get(fanqieCookieHeader))
}

// Session 按 Cookie 取浏览器会话（不含请求头，登录接口内部使用）。
func (f *fanqieAccounts) Session(ctx context.Context, cookie string) *fqapi.Account {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil
	}
	key := cookieKey(cookie)

	f.mu.Lock()
	if e, ok := f.sessions[key]; ok {
		e.used = time.Now()
		acct := e.acct
		f.mu.Unlock()
		return acct
	}
	f.mu.Unlock()

	acct := fqapi.NewAccount("")
	if acct.AttachCookie(ctx, cookie) == nil {
		// Cookie 已失效：不再占用内存，按未登录处理
		return nil
	}
	f.remember(key, acct)
	return acct
}

// NewLoginSession 为一次登录流程创建会话对象。
//
// browser 模式下每次登录都是独立对象；扫码流程需要跨请求复用，
// 由 RememberQR / QRSession 按 token 暂存。
func (f *fanqieAccounts) NewLoginSession() *fqapi.Account {
	if !f.BrowserMode() {
		return f.server
	}
	return fqapi.NewAccount("")
}

// Join 把登录成功的浏览器会话登记到内存池。
func (f *fanqieAccounts) Join(acct *fqapi.Account) {
	if acct == nil || !f.BrowserMode() {
		return
	}
	cookie := acct.Cookie()
	if cookie == "" {
		return
	}
	f.remember(cookieKey(cookie), acct)
}

// ResponseCookie 在 browser 模式下把 Cookie 交给浏览器保存（server 模式返回空）。
func (f *fanqieAccounts) ResponseCookie(acct *fqapi.Account) string {
	if acct == nil || !f.BrowserMode() {
		return ""
	}
	return acct.Cookie()
}

// Drop 退出登录后清理内存中的浏览器会话。
func (f *fanqieAccounts) Drop(cookie string) {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return
	}
	key := cookieKey(cookie)
	f.mu.Lock()
	delete(f.sessions, key)
	f.mu.Unlock()
}

// RememberQR 暂存扫码中的会话，等轮询确认后继续使用。
func (f *fanqieAccounts) RememberQR(token string, acct *fqapi.Account) {
	if token == "" || acct == nil || !f.BrowserMode() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweepLocked()
	f.pending[token] = &qrEntry{acct: acct, used: time.Now()}
}

// QRSession 取回扫码中的会话；找不到时回退到服务端共享会话。
func (f *fanqieAccounts) QRSession(token string) *fqapi.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.pending[token]; ok {
		e.used = time.Now()
		return e.acct
	}
	return nil
}

// ForgetQR 扫码结束后清理暂存。
func (f *fanqieAccounts) ForgetQR(token string) {
	f.mu.Lock()
	delete(f.pending, token)
	f.mu.Unlock()
}

func (f *fanqieAccounts) remember(key string, acct *fqapi.Account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[key] = &poolEntry{acct: acct, used: time.Now()}
	f.sweepLocked()
}

// sweepLocked 清理闲置会话，必要时淘汰最久未使用的条目（调用方持锁）。
func (f *fanqieAccounts) sweepLocked() {
	now := time.Now()
	for key, e := range f.sessions {
		if now.Sub(e.used) > poolIdleTTL {
			delete(f.sessions, key)
		}
	}
	for key, e := range f.pending {
		if now.Sub(e.used) > qrIdleTTL {
			delete(f.pending, key)
		}
	}
	for len(f.sessions) > poolMaxSize {
		var oldestKey string
		var oldest time.Time
		for key, e := range f.sessions {
			if oldest.IsZero() || e.used.Before(oldest) {
				oldestKey, oldest = key, e.used
			}
		}
		if oldestKey == "" {
			break
		}
		delete(f.sessions, oldestKey)
	}
}

// cookieKey 用 Cookie 的摘要作为内存键，避免把凭据本身当 map key。
func cookieKey(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:])
}
