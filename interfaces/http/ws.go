package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"

	domainagent "simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/infrastructure/audit"
)

// wsIdleTimeout 单条连接允许的最长静默时间，超时后关闭，避免半开连接长期占用。
const wsIdleTimeout = 10 * time.Minute

// defaultWSMaxPayload 未配置请求体上限时的 WS 单帧上限。
const defaultWSMaxPayload = 1 << 20

// inboundMsg 浏览器 -> 服务端的消息
type inboundMsg struct {
	Type      string `json:"type"`      // chat / approval / stop
	SessionID string `json:"sessionID"` // chat 时指定目标会话
	Message   string `json:"message"`   // chat 时用户输入
	RequestID string `json:"requestID"` // approval 时对应哪次确认
	Decision  string `json:"decision"`  // approve / reject / modify
	Reason    string `json:"reason"`    // reject/modify 的理由
	Arguments string `json:"arguments"` // modify 后的新参数/新计划
}

// outMsg 服务端 -> 浏览器的消息
type outMsg struct {
	Type      string                     `json:"type"` // status / event / approval_request / final / error
	SessionID string                     `json:"sessionID,omitempty"`
	Message   string                     `json:"message,omitempty"`
	Answer    string                     `json:"answer,omitempty"`
	RequestID string                     `json:"requestID,omitempty"`
	Req       *dapproval.ApprovalRequest `json:"req,omitempty"`
	Event     *domainagent.AgentEvent    `json:"event,omitempty"`
}

// wsClient 一条 WebSocket 连接。读写分属不同协程，写方向需要互斥。
type wsClient struct {
	server  *Server
	conn    *websocket.Conn
	writeMu sync.Mutex
	// runMu 同一连接上的对话串行执行：审批器与取消句柄都是连接级单槽，
	// 并发运行会互相覆盖；不同连接之间互不影响，可并行执行不同会话。
	runMu sync.Mutex
	// 运行期状态，由 readLoop(读) 与 run 协程(写) 共享，用 appMu 保护
	appMu    sync.Mutex
	approver *wsApprover
	cancel   context.CancelFunc
}

// handleWebSocket 每个 WebSocket 连接的处理入口
func (s *Server) handleWebSocket(ws *websocket.Conn) {
	max := s.opts.RequestMaxBytes
	if max <= 0 {
		max = defaultWSMaxPayload
	}
	ws.MaxPayloadBytes = int(max)

	c := &wsClient{server: s, conn: ws}
	// 连接断开时取消正在执行的会话，避免审批等待永久挂起
	defer c.stopRun()
	c.readLoop()
}

// readLoop 循环接收浏览器消息并按类型分发
func (c *wsClient) readLoop() {
	for {
		// 每条消息前重置读取超时；长时间静默的连接会被关闭
		_ = c.conn.SetReadDeadline(time.Now().Add(wsIdleTimeout))
		var msg inboundMsg
		if err := websocket.JSON.Receive(c.conn, &msg); err != nil {
			return // 连接关闭或异常，结束本连接
		}
		switch msg.Type {
		case "chat":
			go c.handleChat(msg)
		case "approval":
			c.routeApproval(msg)
		case "stop":
			c.stopRun()
		}
	}
}

// send 以独占方式向浏览器写一条消息
func (c *wsClient) send(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return websocket.JSON.Send(c.conn, v)
}

// handleChat 处理一次对话请求。
//
// 每个会话从 SessionManager 取独立实例：同一会话串行、不同会话可并行。
// 会话 ID 为空时先生成一个，再按该 ID 获取实例，避免把新会话都落到同一个实例上。
func (c *wsClient) handleChat(msg inboundMsg) {
	c.runMu.Lock()
	defer c.runMu.Unlock()

	s := c.server
	if s.manager == nil {
		c.send(outMsg{Type: "error", Message: "会话管理器未初始化"})
		return
	}
	sessionID := strings.TrimSpace(msg.SessionID)
	if sessionID == "" {
		id, err := s.agent.CreateSession()
		if err != nil {
			c.send(outMsg{Type: "error", Message: err.Error()})
			return
		}
		sessionID = id
	}
	sessionAgent, release, err := s.manager.Acquire(sessionID)
	if err != nil {
		c.send(outMsg{Type: "error", Message: err.Error()})
		return
	}
	defer release()

	// 本次运行的取消句柄，供用户「stop」中断
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	baseApprover := newWSApprover(c, s.opts.ApprovalTimeout)

	c.appMu.Lock()
	c.approver = baseApprover
	c.cancel = cancel
	c.appMu.Unlock()
	defer func() {
		c.appMu.Lock()
		c.approver = nil
		c.cancel = nil
		c.appMu.Unlock()
	}()

	// 审批与事件接到当前会话实例上，不影响其他会话
	sessionAgent.SetApprover(audit.WrapApprover(baseApprover, s.audit, "ws"))
	sessionAgent.SetOnEvent(func(ev domainagent.AgentEvent) {
		c.send(outMsg{Type: "event", SessionID: ev.SessionID, Event: &ev})
	})
	defer func() {
		// 运行结束后回落到安全默认值：没有连接就没有审批通道，副作用操作必须拒绝
		sessionAgent.SetApprover(dapproval.DenyApprover{})
		sessionAgent.SetOnEvent(nil)
	}()

	answer, err := sessionAgent.Run(ctx, sessionID, msg.Message)
	if err != nil {
		// 用户主动停止会以 context.Canceled 结尾，给出友好提示而非红色报错
		if errors.Is(err, context.Canceled) {
			c.send(outMsg{Type: "status", Message: "已停止本次执行"})
		} else {
			c.send(outMsg{Type: "error", Message: err.Error()})
		}
		return
	}
	c.send(outMsg{Type: "final", SessionID: sessionAgent.SessionID(), Answer: answer})
}

// routeApproval 把用户的审批应答转发给当前正在等待确认的 approver
func (c *wsClient) routeApproval(msg inboundMsg) {
	c.appMu.Lock()
	app := c.approver
	c.appMu.Unlock()
	if app == nil {
		return
	}
	app.submit(msg.RequestID, wsApprovalResp{
		Decision:  msg.Decision,
		Reason:    msg.Reason,
		Arguments: msg.Arguments,
	})
}

// stopRun 中断当前正在执行的会话
func (c *wsClient) stopRun() {
	c.appMu.Lock()
	cancel := c.cancel
	c.appMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// wsApprovalResp 浏览器审批应答的结构化表示
type wsApprovalResp struct {
	Decision  string
	Reason    string
	Arguments string
}

// wsApprover 实现 dapproval.Approver：把确认请求推送到浏览器并等待应答。
// 一次确认对应一个 requestID，approve 期间挂在 waits 表里等待 submit 分发。
type wsApprover struct {
	c       *wsClient
	timeout time.Duration
	mu      sync.Mutex
	seq     uint32
	waits   map[string]chan wsApprovalResp
}

// newWSApprover 创建绑定到某条连接的审批器。timeout <= 0 时使用 2 分钟默认值。
func newWSApprover(c *wsClient, timeout time.Duration) *wsApprover {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &wsApprover{c: c, timeout: timeout, waits: make(map[string]chan wsApprovalResp)}
}

// Approve 实现 dapproval.Approver 接口
func (a *wsApprover) Approve(ctx context.Context, req dapproval.ApprovalRequest) (dapproval.ApprovalResult, error) {
	if a.c == nil || a.c.conn == nil {
		return dapproval.ApprovalResult{}, errors.New("WebSocket 未连接，无法进行人工确认")
	}
	id := a.mkID()
	ch := make(chan wsApprovalResp, 1)
	a.mu.Lock()
	a.waits[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waits, id)
		a.mu.Unlock()
	}()

	if err := a.c.send(outMsg{Type: "approval_request", RequestID: id, Req: &req}); err != nil {
		return dapproval.ApprovalResult{}, fmt.Errorf("推送审批请求失败: %w", err)
	}

	timer := time.NewTimer(a.timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return dapproval.ApprovalResult{}, fmt.Errorf("等待人工确认已取消: %w", ctx.Err())
	case <-timer.C:
		// 超时按拒绝处理，且不返回 error，让模型继续调整而不是中止整次运行
		return dapproval.ApprovalResult{Decision: dapproval.DecisionReject, Reason: "审批等待超时，已按拒绝处理"}, nil
	case resp := <-ch:
		return a.toResult(resp), nil
	}
}

// submit 把来自浏览器的应答路由给对应 requestID 的等待者。
// 使用非阻塞写入，避免重复应答阻塞读协程。
func (a *wsApprover) submit(id string, resp wsApprovalResp) {
	a.mu.Lock()
	ch, ok := a.waits[id]
	a.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// mkID 生成本次确认的递增标识
func (a *wsApprover) mkID() string {
	a.mu.Lock()
	a.seq++
	id := fmt.Sprintf("req-%d", a.seq)
	a.mu.Unlock()
	return id
}

// toResult 把浏览器决策映射为 dapproval 的 ApprovalResult
func (a *wsApprover) toResult(resp wsApprovalResp) dapproval.ApprovalResult {
	switch strings.ToLower(strings.TrimSpace(resp.Decision)) {
	case "reject", "no", "n":
		return dapproval.ApprovalResult{Decision: dapproval.DecisionReject, Reason: resp.Reason}
	case "modify", "edit", "e":
		return dapproval.ApprovalResult{Decision: dapproval.DecisionModify, Arguments: resp.Arguments, Reason: resp.Reason}
	default:
		return dapproval.ApprovalResult{Decision: dapproval.DecisionApprove}
	}
}
