package server

import (
	"log"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"

	"simpleAgent/infrastructure/audit"
)

// maxWSConns 单进程允许的并发 WebSocket 连接上限，防止连接耗尽。
const maxWSConns = 16

// contentSecurityPolicy 报告模式的安全策略。
//
// 先使用 Report-Only，避免前端内联脚本在升级初期被直接拦截；
// 待前端资源全部外置后可切换为强制模式。
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'"

// protect 给业务接口加上认证与 Origin 校验。
func (s *Server) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.originAllowed(r) {
			s.audit.Log(audit.Event{Type: "auth", Action: "origin_denied", Source: clientIP(r), Detail: r.Header.Get("Origin")})
			writeAPIErr(w, http.StatusForbidden, "ORIGIN_DENIED", "请求来源不被允许")
			return
		}
		// 预检请求不携带凭证，允许通过但只返回 CORS 头
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if s.auth == nil || !s.auth.Authenticate(r) {
			s.audit.Log(audit.Event{Type: "auth", Action: "deny", Source: clientIP(r), Detail: r.Method + " " + r.URL.Path})
			writeAPIErr(w, http.StatusUnauthorized, "AUTH_REQUIRED", "需要登录")
			return
		}
		next(w, r)
	}
}

// protectHandler 给 http.Handler（如 WS 握手）加上认证与 Origin 校验。
func (s *Server) protectHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.originAllowed(r) {
			s.audit.Log(audit.Event{Type: "auth", Action: "origin_denied", Source: clientIP(r), Detail: r.Header.Get("Origin")})
			writeAPIErr(w, http.StatusForbidden, "ORIGIN_DENIED", "请求来源不被允许")
			return
		}
		if s.auth == nil || !s.auth.Authenticate(r) {
			s.audit.Log(audit.Event{Type: "auth", Action: "deny", Source: clientIP(r), Detail: "ws handshake"})
			writeAPIErr(w, http.StatusUnauthorized, "AUTH_REQUIRED", "需要登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// wsLimit 限制并发 WebSocket 连接数；超限直接拒绝握手。
func (s *Server) wsLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&s.wsConns, 1) > maxWSConns {
			atomic.AddInt64(&s.wsConns, -1)
			writeAPIErr(w, http.StatusTooManyRequests, "TOO_MANY_CONNECTIONS", "连接数已达上限")
			return
		}
		defer atomic.AddInt64(&s.wsConns, -1)
		next.ServeHTTP(w, r)
	})
}

// originAllowed 判断请求 Origin 是否可信。
//
// 无 Origin 头（curl/CLI/同进程调用）视为非浏览器客户端，交由凭证校验；
// 有 Origin 时必须与 Host 同源，或命中显式白名单。
func (s *Server) originAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(parsed.Host, r.Host) {
		return true
	}
	for _, allowed := range s.opts.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), strings.TrimRight(origin, "/")) {
			return true
		}
	}
	return false
}

// securityHeaders 设置基础安全响应头，并对 API 响应禁用缓存。
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("X-Frame-Options", "DENY")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Content-Security-Policy-Report-Only", contentSecurityPolicy)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			header.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware 只对同源或白名单 Origin 返回 CORS 头。
// 默认不返回 Access-Control-Allow-Origin，避免任意站点携带凭证调用。
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" && s.originAllowed(r) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, PATCH, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				w.Header().Set("Access-Control-Max-Age", "600")
			}
		}
		next.ServeHTTP(w, r)
	})
}

// bodyLimit 限制请求体大小，超限直接返回 413。
func (s *Server) bodyLimit(next http.Handler) http.Handler {
	max := s.opts.RequestMaxBytes
	if max <= 0 {
		max = 1 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > max {
			writeAPIErr(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "请求体超过上限")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, max)
		}
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware 兜底 panic，避免单个请求把整个服务带崩。
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				writeAPIErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// writeAPIErr 输出带稳定错误码的 JSON 错误，便于前端区分认证、限流与业务失败。
func writeAPIErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "error": msg})
}
