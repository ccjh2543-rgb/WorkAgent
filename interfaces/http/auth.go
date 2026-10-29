package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"simpleAgent/infrastructure/audit"
)

// sessionCookieName 浏览器会话 Cookie 名。Cookie 只保存随机会话 ID，不保存原始 Token。
const sessionCookieName = "sa_sid"

// 登录限流阈值：同一 IP 每分钟 5 次、每小时 20 次。
const (
	loginPerMinute = 5
	loginPerHour   = 20
)

// Auth 负责 HTTP/WS 的凭证校验、浏览器会话与登录限流。
//
// 支持两种客户端：
//   - 浏览器：Token 登录后拿到 HttpOnly Cookie，后续请求自动携带；
//   - CLI/脚本：直接使用 Authorization: Bearer <token>。
type Auth struct {
	token       string
	allowNoAuth bool
	sessionTTL  time.Duration
	now         func() time.Time // 可注入时钟，便于测试会话过期

	mu       sync.Mutex
	sessions map[string]authSession
	limiter  *loginLimiter
	audit    *audit.Logger
}

// authSession 服务端内存中的登录会话。
type authSession struct {
	expiresAt time.Time
	remote    string // 登录来源 IP，仅用于审计，不参与鉴权
}

// newAuth 构造认证器。sessionTTL <= 0 时使用 24 小时默认值。
func newAuth(opts Options) *Auth {
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Auth{
		token:       opts.AuthToken,
		allowNoAuth: opts.AllowNoAuth,
		sessionTTL:  ttl,
		now:         time.Now,
		sessions:    make(map[string]authSession),
		limiter:     &loginLimiter{entries: make(map[string]*loginWindow)},
	}
}

// Authenticate 判断请求是否已通过认证。
// 免认证模式直接放行；否则要求有效 Bearer Token 或未过期的会话 Cookie。
func (a *Auth) Authenticate(r *http.Request) bool {
	if a == nil {
		return false
	}
	if a.allowNoAuth {
		return true
	}
	return a.checkBearer(r) || a.checkSession(r)
}

// checkBearer 校验 Authorization: Bearer <token>。
func (a *Auth) checkBearer(r *http.Request) bool {
	if a.token == "" {
		return false
	}
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	return tokenEqual(parts[1], a.token)
}

// checkSession 校验浏览器会话 Cookie，并顺带清理过期会话。
func (a *Auth) checkSession(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	sess, ok := a.sessions[cookie.Value]
	if !ok {
		return false
	}
	if !now.Before(sess.expiresAt) {
		delete(a.sessions, cookie.Value)
		return false
	}
	a.pruneSessionsLocked(now)
	return true
}

// Login 处理 POST /api/auth/login：校验 Token 并签发会话 Cookie。
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 POST")
		return
	}
	ip := clientIP(r)
	if !a.limiter.allow(ip, a.now()) {
		a.audit.Log(audit.Event{Type: "auth", Action: "rate_limited", Source: ip, Detail: "login"})
		writeAPIErr(w, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁")
		return
	}
	// 免认证模式下无需登录，直接返回成功，避免前端卡在登录框
	if a.allowNoAuth {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "BAD_REQUEST", "请求体不是合法 JSON")
		return
	}
	if !tokenEqual(strings.TrimSpace(body.Token), a.token) {
		a.audit.Log(audit.Event{Type: "auth", Action: "deny", Source: ip, Detail: "login"})
		writeAPIErr(w, http.StatusUnauthorized, "AUTH_REQUIRED", "需要登录")
		return
	}

	sid, err := randomSessionID()
	if err != nil {
		writeAPIErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "创建会话失败")
		return
	}
	now := a.now()
	a.mu.Lock()
	a.sessions[sid] = authSession{expiresAt: now.Add(a.sessionTTL), remote: ip}
	a.pruneSessionsLocked(now)
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.sessionTTL.Seconds()),
		Secure:   r.TLS != nil,
	})
	a.audit.Log(audit.Event{Type: "auth", Action: "login", Source: ip, Detail: "session"})
	w.WriteHeader(http.StatusNoContent)
}

// Logout 处理 POST /api/auth/logout：删除服务端会话并清除 Cookie。
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAPIErr(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 POST")
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	a.audit.Log(audit.Event{Type: "auth", Action: "logout", Source: clientIP(r)})
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Secure:   r.TLS != nil,
	})
	w.WriteHeader(http.StatusNoContent)
}

// Status 处理 GET /api/auth/status：公开接口，供前端决定是否展示登录框。
func (a *Auth) Status(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeAPIErr(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "仅支持 GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": a.Authenticate(r)})
}

// pruneSessionsLocked 清理过期会话，防止长时间运行后内存持续增长。调用方必须持锁。
func (a *Auth) pruneSessionsLocked(now time.Time) {
	for sid, sess := range a.sessions {
		if !now.Before(sess.expiresAt) {
			delete(a.sessions, sid)
		}
	}
}

// tokenEqual 以常量时间比较两个 Token。
// 先做 SHA-256 再比较，避免长度差异泄露信息。
func tokenEqual(given, want string) bool {
	if given == "" || want == "" {
		return false
	}
	g := sha256.Sum256([]byte(given))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// randomSessionID 生成 32 字节随机会话 ID。
func randomSessionID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// clientIP 从 RemoteAddr 取出 IP；不信任 X-Forwarded-For，避免伪造来源绕过限流。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// loginLimiter 固定窗口登录限流器：按 IP 限制每分钟与每小时尝试次数。
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginWindow
}

// loginWindow 单个 IP 的窗口计数。
type loginWindow struct {
	minuteKey   string
	minuteCount int
	hourKey     string
	hourCount   int
	lastSeen    time.Time
}

// allow 判断该 IP 本次登录尝试是否放行。
func (l *loginLimiter) allow(ip string, now time.Time) bool {
	minuteKey := now.Format("2006-01-02T15:04")
	hourKey := now.Format("2006-01-02T15")

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*loginWindow)
	}
	entry, ok := l.entries[ip]
	if !ok {
		entry = &loginWindow{}
		l.entries[ip] = entry
	}
	if entry.minuteKey != minuteKey {
		entry.minuteKey = minuteKey
		entry.minuteCount = 0
	}
	if entry.hourKey != hourKey {
		entry.hourKey = hourKey
		entry.hourCount = 0
	}
	if entry.minuteCount >= loginPerMinute || entry.hourCount >= loginPerHour {
		return false
	}
	entry.minuteCount++
	entry.hourCount++
	entry.lastSeen = now

	// 粗略清理：条目过多时删除窗口已过期的 IP，避免被大量来源撑爆内存
	if len(l.entries) > 4096 {
		for key, candidate := range l.entries {
			if candidate.hourKey != hourKey && candidate.minuteKey != minuteKey {
				delete(l.entries, key)
			}
		}
	}
	return true
}
