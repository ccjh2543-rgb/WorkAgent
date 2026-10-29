package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

// newTestAuth 构造用于测试的认证器。
func newTestAuth(allowNoAuth bool, ttl time.Duration) *Auth {
	return newAuth(Options{AuthToken: testToken, AllowNoAuth: allowNoAuth, SessionTTL: ttl})
}

// TestBearerAuthentication 表驱动验证 Bearer Token 的通过/拒绝分支。
func TestBearerAuthentication(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "正确 Token", header: "Bearer " + testToken, want: true},
		{name: "大小写不敏感方案", header: "bearer " + testToken, want: true},
		{name: "错误 Token", header: "Bearer wrong-token", want: false},
		{name: "缺失头", header: "", want: false},
		{name: "格式错误", header: testToken, want: false},
		{name: "多余片段", header: "Bearer " + testToken + " extra", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newTestAuth(false, time.Hour)
			req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := a.Authenticate(req); got != tc.want {
				t.Fatalf("Authenticate=%v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestLoginStatusLogout 验证登录签发 Cookie、状态查询与退出登录的完整链路。
func TestLoginStatusLogout(t *testing.T) {
	a := newTestAuth(false, time.Hour)
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"`+testToken+`"}`))
	login.RemoteAddr = "192.0.2.10:12345"
	loginRec := httptest.NewRecorder()
	a.Login(loginRec, login)

	if loginRec.Code != http.StatusNoContent {
		t.Fatalf("登录应返回 204，实际 %d: %s", loginRec.Code, loginRec.Body.String())
	}
	cookies := loginRec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("应签发会话 Cookie，实际 %+v", cookies)
	}
	sid := cookies[0].Value
	if sid == "" || sid == testToken {
		t.Fatal("Cookie 中必须是随机会话 ID，而不是原始 Token")
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("Cookie 安全属性不正确: %+v", cookies[0])
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/api/auth/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	statusRec := httptest.NewRecorder()
	a.Status(statusRec, statusReq)
	var status struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &status); err != nil {
		t.Fatalf("解析状态响应失败: %v", err)
	}
	if !status.Authenticated {
		t.Fatal("携带有效会话时应返回已登录")
	}

	logoutReq := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logoutReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	logoutRec := httptest.NewRecorder()
	a.Logout(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("退出应返回 204，实际 %d", logoutRec.Code)
	}

	afterReq := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	afterReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	if a.Authenticate(afterReq) {
		t.Fatal("退出后会话必须失效")
	}
}

// TestLoginRejectsWrongToken 验证错误 Token 统一返回 401 且不暴露差异。
func TestLoginRejectsWrongToken(t *testing.T) {
	a := newTestAuth(false, time.Hour)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"wrong"}`))
	req.RemoteAddr = "192.0.2.11:1234"
	rec := httptest.NewRecorder()
	a.Login(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误 Token 应返回 401，实际 %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析错误响应失败: %v", err)
	}
	if body["code"] != "AUTH_REQUIRED" {
		t.Fatalf("错误码应为 AUTH_REQUIRED，实际 %v", body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("登录失败不应签发 Cookie")
	}
}

// TestLoginRateLimit 验证同一 IP 超过每分钟阈值后返回 429。
func TestLoginRateLimit(t *testing.T) {
	a := newTestAuth(false, time.Hour)
	for i := 0; i < loginPerMinute; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"wrong"}`))
		req.RemoteAddr = "192.0.2.12:1234"
		rec := httptest.NewRecorder()
		a.Login(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次尝试不应触发限流", i+1)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"wrong"}`))
	req.RemoteAddr = "192.0.2.12:1234"
	rec := httptest.NewRecorder()
	a.Login(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超过阈值应返回 429，实际 %d", rec.Code)
	}
}

// TestSessionExpiry 验证会话超过 TTL 后失效。
func TestSessionExpiry(t *testing.T) {
	a := newTestAuth(false, time.Minute)
	now := time.Now()
	a.now = func() time.Time { return now }

	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"token":"`+testToken+`"}`))
	req.RemoteAddr = "192.0.2.13:1234"
	rec := httptest.NewRecorder()
	a.Login(rec, req)
	sid := rec.Result().Cookies()[0].Value

	now = now.Add(2 * time.Minute)
	expired := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	expired.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sid})
	if a.Authenticate(expired) {
		t.Fatal("过期会话必须失效")
	}
}

// TestAllowNoAuthBypassesCredentials 验证免认证模式仅绕过凭证，不改变其它配置。
func TestAllowNoAuthBypassesCredentials(t *testing.T) {
	a := newTestAuth(true, time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/api/tools", nil)
	if !a.Authenticate(req) {
		t.Fatal("免认证模式应直接放行")
	}
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
	login.RemoteAddr = "192.0.2.14:1234"
	rec := httptest.NewRecorder()
	a.Login(rec, login)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("免认证登录应直接成功，实际 %d", rec.Code)
	}
}
