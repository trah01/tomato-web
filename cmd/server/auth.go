package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/fanqieweb/internal/sitesettings"
)

// 站点访问控制：开启访问密码后，首页、阅读页与所有接口都需要先登录。
// 登录态用 HMAC 签名的会话 Cookie 保存在浏览器，两步验证为 TOTP（验证器 App）。

const (
	authCookieName = "fqweb_auth"
	authCookieTTL  = 30 * 24 * time.Hour

	maxLoginFails = 8
	loginFailWin  = 10 * time.Minute
	pending2FATTL = 10 * time.Minute
)

// loginGuard 限制同一来源的连续失败次数，避免被暴力猜密码。
type loginGuard struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count int
	first time.Time
}

func newLoginGuard() *loginGuard {
	return &loginGuard{fails: make(map[string]*failRecord)}
}

func (g *loginGuard) blocked(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.fails[key]
	if !ok {
		return false
	}
	if time.Since(rec.first) > loginFailWin {
		delete(g.fails, key)
		return false
	}
	return rec.count >= maxLoginFails
}

func (g *loginGuard) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.fails[key]
	if !ok || time.Since(rec.first) > loginFailWin {
		g.fails[key] = &failRecord{count: 1, first: time.Now()}
		return
	}
	rec.count++
}

func (g *loginGuard) reset(key string) {
	g.mu.Lock()
	delete(g.fails, key)
	g.mu.Unlock()
}

// ---- 中间件 ----

// requireAccess 在开启访问密码后拦截未登录的页面与接口请求。
func (s *server) requireAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.settings.AccessRequired() || s.authed(r) {
			next.ServeHTTP(w, r)
			return
		}
		switch r.URL.Path {
		case "/api/health", "/api/auth/login", "/api/auth/status", "/api/auth/logout", "/login", "/usage":
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":     "请先输入访问账号密码",
				"needLogin": true,
			})
			return
		}
		s.writeLoginPage(w)
	})
}

// authed 判断请求是否带着有效的登录 Cookie。
func (s *server) authed(r *http.Request) bool {
	ck, err := r.Cookie(authCookieName)
	if err != nil || ck.Value == "" {
		return false
	}
	user, ok := s.settings.VerifySession(ck.Value)
	if !ok {
		return false
	}
	return user == s.settings.AccessUsername()
}

func (s *server) setAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    s.settings.SignSession(s.settings.AccessUsername(), authCookieTTL),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(authCookieTTL / time.Second),
	})
}

func (s *server) clearAuthCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     authCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(fwd)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---- 登录接口 ----

// handleAuthStatus 返回站点是否需要登录、当前是否已登录。
// GET /api/auth/status
func (s *server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.settings.AccessRequired(),
		"authed":  s.authed(r),
		"twofa":   s.settings.TwoFAEnabled(),
	})
}

// handleAuthLogin 校验访问账号密码（开启两步验证时还需验证码）。
// POST /api/auth/login {"username":"..","password":"..","code":"123456"}
func (s *server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求内容不完整"})
		return
	}
	if !s.settings.AccessRequired() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "当前没有开启访问密码"})
		return
	}

	ip := clientIP(r)
	if s.loginGuard.blocked(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "尝试次数过多，请 10 分钟后再试",
		})
		return
	}
	if !s.settings.CheckPassword(req.Username, req.Password) {
		s.loginGuard.fail(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "账号或密码不正确"})
		return
	}
	if s.settings.TwoFAEnabled() {
		if strings.TrimSpace(req.Code) == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "请输入两步验证码", "need2fa": true,
			})
			return
		}
		if !sitesettings.VerifyTOTP(s.settings.TwoFASecret(), req.Code, time.Now()) {
			s.loginGuard.fail(ip)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "两步验证码不正确", "need2fa": true,
			})
			return
		}
	}

	s.loginGuard.reset(ip)
	s.setAuthCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleAuthLogout 退出站点登录。
// POST /api/auth/logout
func (s *server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	s.clearAuthCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 设置接口 ----

// handleSettings 返回设置页需要的状态（不含密码与密钥）。
// GET /api/settings
func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.settings.View())
}

// handleSettingsAccess 开启 / 关闭访问密码。
// POST /api/settings/access {"enabled":true,"username":"..","password":".."}
func (s *server) handleSettingsAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Enabled  bool   `json:"enabled"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求内容不完整"})
		return
	}
	if err := s.settings.SetAccess(req.Enabled, req.Username, req.Password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// 刚设置好密码时直接把当前浏览器标记为已登录，避免立刻被踢回登录页
	if s.settings.AccessRequired() {
		s.setAuthCookie(w, r)
	} else {
		s.clearAuthCookie(w, r)
	}
	s.logger.Printf("访问密码已%s（账号 %s）", onOff(req.Enabled), s.settings.View().Access.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.settings.View()})
}

// handleSettings2FAStart 生成两步验证密钥，等验证码确认后才真正开启。
// POST /api/settings/2fa/start
func (s *server) handleSettings2FAStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	if !s.settings.AccessRequired() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先开启访问密码"})
		return
	}
	secret, err := sitesettings.NewTOTPSecret()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成密钥失败", err)
		return
	}
	s.setPending2FA(secret)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"secret": secret,
		"uri":    sitesettings.TOTPProvisioningURI("番茄阅读器-网页版", s.settings.AccessUsername(), secret),
	})
}

// handleSettings2FAEnable 用当前验证码确认开启两步验证。
// POST /api/settings/2fa/enable {"code":"123456"}
func (s *server) handleSettings2FAEnable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求内容不完整"})
		return
	}
	secret, ok := s.pending2FA()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请先获取密钥再输入验证码"})
		return
	}
	if !sitesettings.VerifyTOTP(secret, req.Code, time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "验证码不正确，请确认手机时间准确后重试"})
		return
	}
	if err := s.settings.SetTwoFA(true, secret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.clearPending2FA()
	s.logger.Print("两步验证已开启")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.settings.View()})
}

// handleSettings2FADisable 关闭两步验证（需当前验证码）。
// POST /api/settings/2fa/disable {"code":"123456"}
func (s *server) handleSettings2FADisable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求内容不完整"})
		return
	}
	if !s.settings.TwoFAEnabled() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "当前没有开启两步验证"})
		return
	}
	if !sitesettings.VerifyTOTP(s.settings.TwoFASecret(), req.Code, time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "验证码不正确"})
		return
	}
	if err := s.settings.SetTwoFA(false, ""); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Print("两步验证已关闭")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.settings.View()})
}

// handleSettingsFanqie 切换番茄账号的绑定方式。
// POST /api/settings/fanqie {"mode":"browser"|"server"}
func (s *server) handleSettingsFanqie(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求内容不完整"})
		return
	}
	if err := s.settings.SetFanqieMode(req.Mode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.logger.Printf("番茄账号绑定方式已切换为 %s", req.Mode)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.settings.View()})
}

// ---- 两步验证临时密钥 ----

func (s *server) setPending2FA(secret string) {
	s.pendingMu.Lock()
	s.pendingSecret = secret
	s.pendingAt = time.Now()
	s.pendingMu.Unlock()
}

func (s *server) pending2FA() (string, bool) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingSecret == "" || time.Since(s.pendingAt) > pending2FATTL {
		return "", false
	}
	return s.pendingSecret, true
}

func (s *server) clearPending2FA() {
	s.pendingMu.Lock()
	s.pendingSecret = ""
	s.pendingMu.Unlock()
}

func onOff(v bool) string {
	if v {
		return "开启"
	}
	return "关闭"
}
