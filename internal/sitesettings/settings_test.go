package sitesettings

import (
	"testing"
	"time"
)

func TestPasswordHashAndVerify(t *testing.T) {
	hashed, err := HashPassword("abcdef")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hashed == "abcdef" || !VerifyPassword(hashed, "abcdef") {
		t.Fatal("密码摘要应可校验，且不包含明文")
	}
	if VerifyPassword(hashed, "abcdeF") {
		t.Fatal("不同密码不应通过校验")
	}
}

func TestAccessRequiresStrongEnoughPassword(t *testing.T) {
	s := New("")
	if err := s.SetAccess(true, "admin", "12345"); err == nil {
		t.Fatal("少于 6 位应被拒绝")
	}
	if err := s.SetAccess(true, "admin", "123456"); err != nil {
		t.Fatalf("6 位纯数字应通过: %v", err)
	}
	if !s.AccessRequired() || !s.CheckPassword("admin", "123456") {
		t.Fatal("开启后应能用该账号密码登录")
	}
	if s.CheckPassword("admin", "1234567") {
		t.Fatal("错误密码不应通过")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := New("")
	if err := s.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	token := s.SignSession("admin", time.Hour)
	if user, ok := s.VerifySession(token); !ok || user != "admin" {
		t.Fatal("会话票据应可校验")
	}
	if _, ok := s.VerifySession(token + "x"); ok {
		t.Fatal("被篡改的票据不应通过")
	}
	if _, ok := s.VerifySession(s.SignSession("admin", -time.Minute)); ok {
		t.Fatal("过期票据不应通过")
	}
}

func TestTOTPVector(t *testing.T) {
	// RFC 6238 附录 B：密钥 "12345678901234567890" 的 SHA1 用例
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	cases := map[int64]string{
		59:         "287082",
		1111111109: "081804",
		1234567890: "005924",
	}
	for unix, want := range cases {
		got, err := TOTPCode(secret, time.Unix(unix, 0))
		if err != nil {
			t.Fatalf("TOTPCode(%d): %v", unix, err)
		}
		if got != want {
			t.Fatalf("TOTPCode(%d) = %s，期望 %s", unix, got, want)
		}
	}
	if !VerifyTOTP(secret, "287082", time.Unix(59, 0)) {
		t.Fatal("VerifyTOTP 应接受同一窗口的验证码")
	}
	if VerifyTOTP(secret, "000000", time.Unix(59, 0)) {
		t.Fatal("VerifyTOTP 不应接受错误验证码")
	}
}
