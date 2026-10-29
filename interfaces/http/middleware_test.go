package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestProtectRequiresAuthAndOrigin 验证受保护接口的认证与 Origin 组合。
func TestProtectRequiresAuthAndOrigin(t *testing.T) {
	a := newTestAuth(false, time.Hour)
	s := &Server{auth: a, opts: Options{AuthToken: testToken, AllowedOrigins: []string{"http://allowed.example"}}}
	handler := s.protect(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	cases := []struct {
		name   string
		method string
		origin string
		bearer bool
		want   int
	}{
		{name: "无凭证拒绝", method: http.MethodGet, want: http.StatusUnauthorized},
		{name: "有效凭证放行", method: http.MethodGet, bearer: true, want: http.StatusNoContent},
		{name: "同源放行", method: http.MethodGet, origin: "http://example.com", bearer: true, want: http.StatusNoContent},
		{name: "跨站拒绝", method: http.MethodPost, origin: "http://evil.example", bearer: true, want: http.StatusForbidden},
		{name: "白名单放行", method: http.MethodPost, origin: "http://allowed.example", bearer: true, want: http.StatusNoContent},
		{name: "预检无需凭证", method: http.MethodOptions, origin: "http://allowed.example", want: http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/api/tools", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.bearer {
				req.Header.Set("Authorization", "Bearer "+testToken)
			}
			rec := httptest.NewRecorder()
			handler(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("状态码期望 %d，实际 %d: %s", tc.want, rec.Code, rec.Body.String())
			}
			if rec.Code == http.StatusUnauthorized {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("解析错误响应失败: %v", err)
				}
				if body["code"] != "AUTH_REQUIRED" {
					t.Fatalf("错误码应为 AUTH_REQUIRED，实际 %v", body)
				}
			}
		})
	}
}

// TestSecurityHeaders 验证安全响应头与 API 禁止缓存。
func TestSecurityHeaders(t *testing.T) {
	s := &Server{opts: Options{RequestMaxBytes: 1024}}
	handler := s.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	headers := rec.Result().Header
	if headers.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 nosniff")
	}
	if headers.Get("X-Frame-Options") != "DENY" {
		t.Fatal("缺少 X-Frame-Options")
	}
	if headers.Get("Content-Security-Policy-Report-Only") == "" {
		t.Fatal("缺少 CSP 报告头")
	}
	if headers.Get("Cache-Control") != "no-store" {
		t.Fatal("API 响应必须禁止缓存")
	}
}

// TestBodyLimit 验证请求体超限返回 413。
func TestBodyLimit(t *testing.T) {
	s := &Server{opts: Options{RequestMaxBytes: 10}}
	handler := s.bodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	over := httptest.NewRequest(http.MethodPost, "/api/skills", strings.NewReader("01234567890"))
	overRec := httptest.NewRecorder()
	handler.ServeHTTP(overRec, over)
	if overRec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应返回 413，实际 %d", overRec.Code)
	}

	okReq := httptest.NewRequest(http.MethodPost, "/api/skills", strings.NewReader("0123456789"))
	okRec := httptest.NewRecorder()
	handler.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK {
		t.Fatalf("未超限应放行，实际 %d", okRec.Code)
	}
}

// TestRecoverMiddleware 验证 panic 被兜底为 500 JSON。
func TestRecoverMiddleware(t *testing.T) {
	s := &Server{}
	handler := s.recoverMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应返回 500，实际 %d", rec.Code)
	}
}

// TestWSConnectionLimit 验证超过并发连接上限时拒绝握手，且计数会回退。
func TestWSConnectionLimit(t *testing.T) {
	s := &Server{wsConns: maxWSConns}
	handler := s.wsLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应返回 429，实际 %d", rec.Code)
	}
	if s.wsConns != maxWSConns {
		t.Fatalf("拒绝后计数应回退，实际 %d", s.wsConns)
	}
}
