// Package server 提供 Agent 的 HTTP/WS 服务层（DDD interfaces 层）。
//
// 它把 CLI 的能力搬到 Web 上：
//   - REST：会话列表/创建、历史读取、回滚、分叉、工具列表、技能管理、健康检查
//   - WS：多轮对话 + 实时事件流 + 「人在回路」在线审批
//   - 静态页托管：原生 HTML/JS 单页由 Go 直接提供，无需 Node 构建
//
// 并发模型：Agent 不并发安全，因此服务端持共享 Agent + 一把全局 run 锁，
// 同一时刻只执行一个会话；会话的读操作（列表/历史/分支/回滚/分叉）可随时并发。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	appagent "simpleAgent/application/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
	"simpleAgent/infrastructure/memory"
	"simpleAgent/infrastructure/sqlitestore"
	"simpleAgent/infrastructure/tools"
)

//go:embed static
var staticFS embed.FS

// Options HTTP 服务的安全与运行模式配置。
//
// 由 interfaces/cli 装配层从环境变量读取后传入，服务层不直接读环境变量，
// 便于测试时构造不同安全组合。
type Options struct {
	AuthToken       string        // 访问 Token；AllowNoAuth 为 true 时可为空
	AllowedOrigins  []string      // 额外允许的 Origin 白名单（同源始终允许）
	AllowNoAuth     bool          // 仅 loopback 可用的免认证调试开关
	Loopback        bool          // 监听地址是否为本机回环（用于判定 headless 是否允许自动审批）
	Headless        bool          // 无人值守模式
	AllowSkillAdmin bool          // 是否允许登录用户管理技能
	AllowSkillExec  bool          // Headless 下是否允许执行命令技能
	RequestMaxBytes int64         // 请求体上限
	SessionTTL      time.Duration // 登录会话有效期
	ApprovalTimeout time.Duration // 人工审批超时，超时按拒绝处理
	Audit           *audit.Logger // 审计日志；为空时由服务自行创建
}

// Server 持有 HTTP/WS 服务所需的全部状态
type Server struct {
	agent     *appagent.Agent
	workspace *tools.Workspace
	opts      Options
	auth      *Auth
	audit     *audit.Logger
	ownsAudit bool
	runMu     sync.Mutex
	busy      bool  // 是否有会话正在执行
	wsConns   int64 // 当前活跃 WS 连接数（atomic）
	skills    *SkillStore
}

// NewServer 构建服务。projectDir 用于派生工作区与技能配置文件位置。
func NewServer(myAgent *appagent.Agent, projectDir string, opts Options) (*Server, error) {
	ws, err := tools.NewWorkspace(projectDir)
	if err != nil {
		return nil, fmt.Errorf("创建工作区失败: %w", err)
	}
	sk, err := NewSkillStore(filepath.Join(memory.WorkspaceDir(projectDir), "skills.json"))
	if err != nil {
		return nil, fmt.Errorf("初始化技能存储失败: %w", err)
	}
	// 只有「显式 headless + 本机回环」才启用全自动审批；
	// 其他情况保持 Agent 的默认 DenyApprover，避免不可逆操作被静默执行。
	if opts.Headless && opts.Loopback {
		fmt.Println("[安全警告] headless 模式已启用全自动审批（仅限本机回环）")
		myAgent.SetApprover(dapproval.AutoApprover{})
	}

	auditLog := opts.Audit
	ownsAudit := false
	if auditLog == nil {
		// 未由 CLI 注入时自行创建，保证独立运行时也有审计
		path := filepath.Join(memory.WorkspaceDir(projectDir), "audit", "audit.jsonl")
		var auditErr error
		auditLog, auditErr = audit.Open(path)
		if auditErr != nil {
			return nil, fmt.Errorf("初始化审计日志失败: %w", auditErr)
		}
		ownsAudit = true
	}
	auth := newAuth(opts)
	auth.audit = auditLog
	return &Server{
		agent:     myAgent,
		workspace: ws,
		opts:      opts,
		auth:      auth,
		audit:     auditLog,
		ownsAudit: ownsAudit,
		skills:    sk,
	}, nil
}

// RegisterStoredSkills 把持久化的自定义技能注册进 Agent。
// 应在一切对话开始前调用（仍在 NewAgent 之后），保证首版提示词就包含它们。
//
// headless 模式默认禁止命令技能执行；只有显式 AllowSkillExec 才注册，
// 避免无人值守评测环境里出现任意命令执行通道。
func (s *Server) RegisterStoredSkills() error {
	if s.opts.Headless && !s.opts.AllowSkillExec {
		fmt.Println("[安全提示] headless 模式未开启技能执行，已跳过持久化命令技能")
		return nil
	}
	return s.skills.registerInto(s.agent, s.workspace, s.audit)
}

// Serve 启动 HTTP 服务并阻塞，直到 ctx 被取消后优雅退出。
func (s *Server) Serve(ctx context.Context, addr string) error {
	if s.ownsAudit {
		defer func() {
			if err := s.audit.Close(); err != nil {
				fmt.Printf("[审计日志关闭失败] %v\n", err)
			}
		}()
	}
	mux := http.NewServeMux()
	s.routes(mux)

	// 中间件由外到内：panic 兜底 → 安全响应头 → CORS/Origin → 请求体上限 → 路由
	handler := s.recoverMiddleware(s.securityHeaders(s.corsMiddleware(s.bodyLimit(mux))))

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 不设置全局 WriteTimeout：WS 是长连接，超时由审批与连接层各自控制
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shCtx)
	}()
	fmt.Printf("Web 控制台已启动：http://%s\n", addr)
	err := httpSrv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil // ctx 触发的优雅关闭，不算错误
	}
	return err
}

// routes 注册所有 HTTP 路由。
//
// 公开路由只有静态页、健康检查与登录相关接口；所有业务 API 与 WS
// 都必须经过认证与 Origin 校验，避免把 Agent 暴露给任意网页或局域网。
func (s *Server) routes(mux *http.ServeMux) {
	mux.Handle("/static/", s.staticAssets())
	mux.Handle("/", s.indexHandler())
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/auth/login", s.auth.Login)
	mux.HandleFunc("/api/auth/status", s.auth.Status)
	mux.HandleFunc("/api/auth/logout", s.protect(s.auth.Logout))
	mux.HandleFunc("/api/tools", s.protect(s.handleTools))
	mux.HandleFunc("/api/skills", s.protect(s.handleSkills))
	mux.HandleFunc("/api/skills/", s.protect(s.handleSkillDelete))
	mux.HandleFunc("/api/conversations", s.protect(s.handleConversations))
	mux.HandleFunc("/api/sessions/", s.protect(s.handleSessions))
	// WorkBench 评估桥接端点
	mux.HandleFunc("/api/workbench/start", s.protect(s.handleWorkbenchStart))
	mux.HandleFunc("/api/workbench/continue", s.protect(s.handleWorkbenchContinue))
	mux.HandleFunc("/api/workbench/{session_id}", s.protect(s.handleWorkbenchClose))
	// WS 握手同样要求凭证与 Origin 校验，防止恶意网页驱动本地 Agent
	mux.Handle("/ws", s.protectHandler(s.wsLimit(&websocket.Server{Handler: s.handleWebSocket})))
}

// staticAssets 托管编译进二进制的静态资源（CSS/JS/图片）。
// 内嵌 FS 的根已是 static 子目录，因此用 StripPrefix 去掉 /static/ 前缀，
// 避免 FileServer 在 static 里再找一层 static 导致 404。
func (s *Server) staticAssets() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("内嵌静态资源缺失: " + err.Error())
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// indexHandler 返回单页 HTML。根路径统一指向 index.html。
func (s *Server) indexHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("内嵌静态资源缺失: " + err.Error())
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			http.Error(w, "index.html 缺失: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(data)
	})
}

// handleHealth 健康检查
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleTools 返回已注册的工具/技能列表
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	writeJSON(w, http.StatusOK, s.agent.ListTools())
}

// handleConversations 列表或新建对话
func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		conversations, err := s.agent.ListConversations(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, conversations)
	case http.MethodPost:
		id, err := s.agent.CreateSession()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"sessionID": id})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// handleSessions 会话子路由：历史 / 分支 / 回滚 / 分叉
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	// 路径形如 /api/sessions/{id}/branches
	rest := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeErr(w, http.StatusBadRequest, "缺少会话 ID")
		return
	}
	sessionID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		// 读取该分支历史
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		messages, err := s.agent.LoadHistory(r.Context(), sessionID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, messages)
	case "branches":
		if !requireMethod(w, r, http.MethodGet) {
			return
		}
		branches, err := s.agent.ListBranches(r.Context(), sessionID)
		if err != nil {
			// 新建的空会话尚未落库（第一条消息写入时才创建），分支栏不应报错
			if errors.Is(err, sqlitestore.ErrSessionNotFound) {
				writeJSON(w, http.StatusOK, []any{})
				return
			}
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, branches)
	default:
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		s.handleSessionAction(w, r, sessionID, action)
	}
}

// handleSessionAction 处理 rollback 与 fork
func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request, sessionID, action string) {
	var body struct {
		Index int64 `json:"index"`
	}
	// 允许空 body（fork 不传序号时复制当前 HEAD）
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	switch action {
	case "rollback":
		if body.Index <= 0 {
			writeErr(w, http.StatusBadRequest, "回滚需要合法的消息序号")
			return
		}
		archiveID, err := s.agent.RollbackSession(r.Context(), sessionID, body.Index)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"archiveID": archiveID})
	case "fork":
		newSessionID, err := s.agent.ForkSession(r.Context(), sessionID, body.Index)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"sessionID": newSessionID})
	default:
		writeErr(w, http.StatusNotFound, "未知的会话操作: "+action)
	}
}

// handleSkills 查询技能明细或新建技能。
// 写操作需要显式开启技能管理；headless 下还要求显式允许技能执行。
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.skills.list())
	case http.MethodPost:
		if !s.opts.AllowSkillAdmin {
			writeAPIErr(w, http.StatusForbidden, "SKILL_ADMIN_DISABLED", "技能管理已关闭，请设置 SIMPLEAGENT_ALLOW_SKILL_ADMIN=1")
			return
		}
		if s.opts.Headless && !s.opts.AllowSkillExec {
			writeAPIErr(w, http.StatusForbidden, "SKILL_EXEC_DISABLED", "headless 模式禁止执行命令技能")
			return
		}
		var body struct {
			Name            string `json:"name"`
			Description     string `json:"description"`
			CommandTemplate string `json:"command_template"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
			return
		}
		spec := tools.SkillSpec{Name: body.Name, Description: body.Description, CommandTemplate: body.CommandTemplate}
		// 命令技能一律按最高风险处理：客户端声明的风险级别不可信
		if err := s.skills.add(s.agent, s.workspace, spec, tool.RiskDestructive, s.audit); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.audit.Log(audit.Event{Type: "skill", Action: "add", Source: clientIP(r), Detail: spec.Name})
		writeJSON(w, http.StatusCreated, spec)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
	}
}

// handleSkillDelete 删除指定技能，需要显式开启技能管理。
func (s *Server) handleSkillDelete(w http.ResponseWriter, r *http.Request) {
	if !s.opts.AllowSkillAdmin {
		writeAPIErr(w, http.StatusForbidden, "SKILL_ADMIN_DISABLED", "技能管理已关闭，请设置 SIMPLEAGENT_ALLOW_SKILL_ADMIN=1")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 DELETE")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/skills/")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少技能名")
		return
	}
	name, _ = strconv.Unquote(`"` + name + `"`)
	if err := s.skills.remove(s.agent, name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit.Log(audit.Event{Type: "skill", Action: "remove", Source: clientIP(r), Detail: name})
	w.WriteHeader(http.StatusNoContent)
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	writeErr(w, http.StatusMethodNotAllowed, "仅支持 "+method)
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
