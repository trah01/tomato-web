// Package sitesettings 保存站点自身的配置：访问密码、两步验证、番茄账号绑定方式。
//
// 配置写到磁盘（Docker 中放在数据卷里），密码只保存 PBKDF2 摘要，
// 登录态的会话 Cookie 用本文件内的随机密钥做 HMAC 签名，不依赖服务端内存。
package sitesettings

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// ModeBrowser 番茄凭据只存在访问者的浏览器里（localStorage）。
	ModeBrowser = "browser"
	// ModeServer 番茄凭据存在服务端（Docker 数据卷），所有访问者共用。
	ModeServer = "server"

	// MinPasswordLen 访问密码最少位数（不限制字符类型）。
	MinPasswordLen = 6

	pbkdf2Iter = 120000
	saltLen    = 16
)

// Access 访问密码配置。
type Access struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
	// Password 形如 pbkdf2$sha256$120000$salt$hash，不保存明文。
	Password string `json:"password"`
}

// TwoFA 两步验证配置（RFC 6238 TOTP）。
type TwoFA struct {
	Enabled bool   `json:"enabled"`
	Secret  string `json:"secret"`
}

// Fanqie 番茄账号绑定方式。
type Fanqie struct {
	Mode string `json:"mode"`
}

// AccessView 对外展示的访问密码状态（不含摘要）。
type AccessView struct {
	Enabled  bool   `json:"enabled"`
	Username string `json:"username"`
}

// TwoFAView 对外展示的两步验证状态（不含密钥）。
type TwoFAView struct {
	Enabled bool `json:"enabled"`
}

// FanqieView 对外展示的绑定方式。
type FanqieView struct {
	Mode string `json:"mode"`
}

// View 设置页需要的全部状态。
type View struct {
	Access AccessView `json:"access"`
	TwoFA  TwoFAView  `json:"twofa"`
	Fanqie FanqieView `json:"fanqie"`
}

type diskFile struct {
	Access     Access `json:"access"`
	TwoFA      TwoFA  `json:"twofa"`
	Fanqie     Fanqie `json:"fanqie"`
	SessionKey string `json:"sessionKey"`
}

// Settings 站点设置，读写都加锁，可直接被多个请求并发使用。
type Settings struct {
	mu   sync.RWMutex
	path string
	file diskFile
}

// New 创建设置对象；path 为空时仅保存在内存中。
func New(path string) *Settings {
	s := &Settings{path: path}
	s.file.Fanqie.Mode = ModeBrowser
	return s
}

// Load 读取磁盘配置；文件不存在视为首次运行，自动生成会话密钥。
func (s *Settings) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path != "" {
		raw, err := os.ReadFile(s.path)
		switch {
		case err == nil:
			var f diskFile
			if err := json.Unmarshal(raw, &f); err != nil {
				return fmt.Errorf("设置文件解析失败: %w", err)
			}
			s.file = f
		case errors.Is(err, os.ErrNotExist):
			// 首次运行
		default:
			return err
		}
	}
	if s.file.Fanqie.Mode != ModeServer {
		s.file.Fanqie.Mode = ModeBrowser
	}
	if s.file.SessionKey == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		s.file.SessionKey = base64.RawURLEncoding.EncodeToString(key)
		return s.saveLocked()
	}
	return nil
}

func (s *Settings) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(s.file, "", "  ")
	if err != nil {
		return err
	}
	// 含密码摘要与两步验证密钥，仅服务进程可读
	return os.WriteFile(s.path, raw, 0o600)
}

// View 返回不含敏感字段的当前设置。
func (s *Settings) View() View {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return View{
		Access: AccessView{Enabled: s.file.Access.Enabled, Username: s.file.Access.Username},
		TwoFA:  TwoFAView{Enabled: s.file.TwoFA.Enabled},
		Fanqie: FanqieView{Mode: s.file.Fanqie.Mode},
	}
}

// AccessRequired 是否需要密码才能访问站点。
func (s *Settings) AccessRequired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.Access.Enabled && s.file.Access.Username != "" && s.file.Access.Password != ""
}

// AccessUsername 返回当前访问账号名。
func (s *Settings) AccessUsername() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.Access.Username
}

// TwoFAEnabled 是否已开启两步验证。
func (s *Settings) TwoFAEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.TwoFA.Enabled && s.file.TwoFA.Secret != ""
}

// TwoFASecret 返回两步验证密钥（内部使用）。
func (s *Settings) TwoFASecret() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file.TwoFA.Secret
}

// FanqieMode 返回番茄账号绑定方式。
func (s *Settings) FanqieMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file.Fanqie.Mode == ModeServer {
		return ModeServer
	}
	return ModeBrowser
}

// SetFanqieMode 切换番茄账号绑定方式。
func (s *Settings) SetFanqieMode(mode string) error {
	if mode != ModeBrowser && mode != ModeServer {
		return errors.New("绑定方式只能是 browser 或 server")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.file.Fanqie.Mode = mode
	return s.saveLocked()
}

// SetAccess 开启或关闭访问密码。
//
// enabled 为 true 时需要账号名，并且已经存在密码或本次提供新密码。
func (s *Settings) SetAccess(enabled bool, username, password string) error {
	username = strings.TrimSpace(username)

	s.mu.Lock()
	defer s.mu.Unlock()

	if !enabled {
		// 密码关闭后两步验证没有意义，一并清除，避免密钥继续留在磁盘上
		s.file.Access.Enabled = false
		s.file.TwoFA = TwoFA{}
		return s.saveLocked()
	}
	if username == "" {
		return errors.New("请填写访问账号")
	}
	if password != "" {
		if err := checkPasswordStrength(password); err != nil {
			return err
		}
		hashed, err := HashPassword(password)
		if err != nil {
			return err
		}
		s.file.Access.Password = hashed
	} else if s.file.Access.Password == "" {
		return fmt.Errorf("请设置至少 %d 位的访问密码", MinPasswordLen)
	}
	s.file.Access.Username = username
	s.file.Access.Enabled = true
	return s.saveLocked()
}

// CheckPassword 校验访问账号密码。
func (s *Settings) CheckPassword(username, password string) bool {
	s.mu.RLock()
	enabled := s.file.Access.Enabled
	wantUser := s.file.Access.Username
	hashed := s.file.Access.Password
	s.mu.RUnlock()

	if !enabled || hashed == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(username)), []byte(wantUser)) != 1 {
		return false
	}
	return VerifyPassword(hashed, password)
}

// SetTwoFA 开启或关闭两步验证。开启时 secret 必须非空。
func (s *Settings) SetTwoFA(enabled bool, secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if enabled {
		if secret == "" {
			return errors.New("缺少两步验证密钥")
		}
		if !s.file.Access.Enabled {
			return errors.New("请先开启访问密码，再开启两步验证")
		}
		s.file.TwoFA = TwoFA{Enabled: true, Secret: secret}
	} else {
		s.file.TwoFA = TwoFA{}
	}
	return s.saveLocked()
}

// ---- 密码摘要 ----

func checkPasswordStrength(password string) error {
	if len([]rune(password)) < MinPasswordLen {
		return fmt.Errorf("密码至少需要 %d 位", MinPasswordLen)
	}
	return nil
}

// HashPassword 生成 pbkdf2 摘要串。
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := pbkdf2SHA256([]byte(password), salt, pbkdf2Iter, 32)
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword 校验密码与摘要串是否匹配。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	var iter int
	if _, err := fmt.Sscanf(parts[2], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// pbkdf2SHA256 实现 PBKDF2-HMAC-SHA256（RFC 2898）。
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	if iter < 1 {
		iter = 1
	}
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	u := make([]byte, hashLen)
	t := make([]byte, hashLen)
	for block := 1; block <= blocks; block++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write([]byte{byte(block >> 24), byte(block >> 16), byte(block >> 8), byte(block)})
		u = mac.Sum(u[:0])
		copy(t, u)
		for i := 1; i < iter; i++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// ---- 登录会话签名 ----

// SignSession 生成带过期时间的会话票据：base64(payload).base64(hmac)。
func (s *Settings) SignSession(username string, ttl time.Duration) string {
	s.mu.RLock()
	key := s.file.SessionKey
	s.mu.RUnlock()

	payload := fmt.Sprintf("%s|%d", username, time.Now().Add(ttl).Unix())
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return body + "." + sign(key, body)
}

// VerifySession 校验会话票据，返回账号名与是否有效。
func (s *Settings) VerifySession(token string) (string, bool) {
	s.mu.RLock()
	key := s.file.SessionKey
	s.mu.RUnlock()

	if key == "" {
		return "", false
	}
	body, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(sig), []byte(sign(key, body))) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", false
	}
	username, exp, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", false
	}
	var deadline int64
	if _, err := fmt.Sscanf(exp, "%d", &deadline); err != nil {
		return "", false
	}
	if time.Now().Unix() > deadline {
		return "", false
	}
	return username, true
}

func sign(key, body string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
