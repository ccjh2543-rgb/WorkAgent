package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
	"simpleAgent/domain/tool"
)

// 本文件实现 WorkBench 评估基准的桥接运行器。
//
// 评估模式下复用本体真实推理内核 step()，唯一差异是「执行工具」这一步：
// wbExec 返回 errToolDeferred，step 暂停，把工具交给 WorkBench（沙箱数据库与
// 26 个工具只在 Python 侧存在），等 observation 回注后再继续。

// deferredTool 把 WorkBench 侧执行的工具注册进本体 toolReg，让模型能在真实
// 提示词的工具列表里看到它。实际执行由 Run 核心暂停后交回 WorkBench。
type deferredTool struct {
	name string
	desc string
}

func (d *deferredTool) Name() string        { return d.name }
func (d *deferredTool) Description() string { return d.desc }
func (d *deferredTool) Run(string) (string, error) {
	return "", errToolDeferred
}

// WbFunctionCall 记录一次工具调用（用于 WorkBench 评分）。
type WbFunctionCall struct {
	Name string `json:"name"` // 工具名
	Args string `json:"args"` // 原始参数串（JSON），由 WorkBench 解析为 kwargs
}

// WbResult 一轮 StartWorkbench/ContinueWorkbench 之后返回的结果。
// State 取值：tool=等待外部执行工具；final=已给出最终答案；limit=超过步数上限。
type WbResult struct {
	State         string           `json:"state"`
	ToolName      string           `json:"tool_name,omitempty"`
	ToolArg       string           `json:"tool_arg,omitempty"`
	Answer        string           `json:"answer,omitempty"`
	FunctionCalls []WbFunctionCall `json:"function_calls"`
	Metrics       WbMetrics        `json:"metrics"`
}

// wbRuntime 单个 WorkBench 任务的运行器状态。
type wbRuntime struct {
	progress       *runProgress       // 复用真实 Run 推理内核的跨恢复状态
	toolNames      []string           // 本任务注册进 toolReg 的延迟工具名
	functionCalls  []WbFunctionCall   // 已暂停交出的工具调用
	savedSessStore session.Repository // 评估期间挂起的会话持久化，结束后恢复
}

// wbRegistry 全局运行器注册表，按 workbench sessionID 保存进行中的任务状态。
var (
	wbMu       sync.Mutex
	wbRuntimes = map[string]*wbRuntime{}
)

// newWbSessionID 生成一个 workbench 会话 ID。
func newWbSessionID() (string, error) {
	return newSessionID()
}

func wbGet(sid string) (*wbRuntime, bool) {
	wbMu.Lock()
	defer wbMu.Unlock()
	rt, ok := wbRuntimes[sid]
	return rt, ok
}

func wbPut(sid string, rt *wbRuntime) {
	wbMu.Lock()
	defer wbMu.Unlock()
	wbRuntimes[sid] = rt
}

func wbDel(sid string) {
	wbMu.Lock()
	defer wbMu.Unlock()
	delete(wbRuntimes, sid)
}

// splitToolDesc 把 WorkBench 传来的工具描述行 "{name} - {description}"
// 拆成工具名与描述。
func splitToolDesc(line string) (name, desc string) {
	line = strings.TrimSpace(line)
	idx := strings.Index(line, " - ")
	if idx > 0 {
		return strings.TrimSpace(line[:idx]), strings.TrimSpace(line[idx+3:])
	}
	return line, ""
}

// readOnlyToolHints 只读（纯查询）工具的强信号动词。
var readOnlyToolHints = []string{
	"search", "get", "find", "list", "read", "info", "look", "query",
	"peek", "preview", "show", "view", "detail", "describe", "check",
	"fetch", "retrieve", "count", "inspect", "describe",
}

// sideEffectToolHints 有副作用的工具强信号动词。
var sideEffectToolHints = []string{
	"forward", "delete", "reply", "send", "update", "create", "write",
	"move", "archive", "remove", "mark", "schedule", "cancel", "add",
	"edit", "invite", "perform", "set", "approve", "reject", "assign",
	"close", "open", "draft", "submit", "publish",
}

// wbToolPolicy 依据工具名推断 WorkBench 延迟工具的风险等级。
func wbToolPolicy(name string) tool.ToolPolicy {
	tokens := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, tok := range tokens {
		for _, v := range sideEffectToolHints {
			if tok == v {
				return tool.ToolPolicy{Risk: tool.RiskWriteExternal, AutoApprove: true}
			}
		}
	}
	for _, tok := range tokens {
		for _, v := range readOnlyToolHints {
			if tok == v {
				return tool.ToolPolicy{Risk: tool.RiskReadOnly, AutoApprove: true}
			}
		}
	}
	return tool.ToolPolicy{Risk: tool.RiskWriteExternal, AutoApprove: true}
}

// isWorkbenchError 判断回注的 observation 是否为外部工具执行失败/不存在的标记。
func isWorkbenchError(observation string) bool {
	return strings.Contains(observation, "errored") || strings.Contains(observation, "not found")
}

// StartWorkbench 启动一个新的 WorkBench 任务运行器。
//
// 自「普通 Plan 搬运决策器」改造起，普通模式（mode=""）不再走原 step/wbDrive
// 自规划循环，而是与 planner-worker 共用 pwWorkbenchDrive（规划→逐子任务→
// pwDecideNext 决策校验→下一步），仅保留「全上下文不隔离」差异：
// Worker 复用共享 a.memory，跨子任务持续累积，能看见完整历史。
func (a *Agent) StartWorkbench(ctx context.Context, task string, toolDescs []string, datetime string, maxInner int) (string, WbResult, error) {
	return a.startPW(ctx, task, toolDescs, datetime, maxInner, true, false)
}

// StartOfficeWorkbench 以全上下文决策式模式驱动一个 Office 办公任务：
// 本地文件工具在进程内直接执行，非文件工具由外部延迟执行。
// 与 StartWorkbench(Plan) 唯一差异是 officeMode：文件工具走 toolReg.ExecuteTool 直执行。
func (a *Agent) StartOfficeWorkbench(ctx context.Context, task string, toolDescs []string, datetime string, maxInner int) (string, WbResult, error) {
	return a.startPW(ctx, task, toolDescs, datetime, maxInner, true, true)
}

// ContinueWorkbench 注入一行工具 observation 后继续前一个暂停的运行器循环。
func (a *Agent) ContinueWorkbench(ctx context.Context, sessionID, observation string) (WbResult, error) {
	a.wbMu.Lock()
	defer a.wbMu.Unlock()

	rt, ok := wbGet(sessionID)
	if !ok {
		return WbResult{}, fmt.Errorf("workbench 会话不存在: %s", sessionID)
	}
	a.advanceStall(ctx, rt.progress, rt.progress.pausedTool, !isWorkbenchError(observation))
	// 外部工具结果按文本协议回注：其调用由 WorkBench 侧发起，没有原生 tool_call_id
	rt.progress.toolOutput = toolFeedback("", renderToolOutput("", observation))
	res := a.wbDrive(ctx, rt)
	if res.State == "final" || res.State == "limit" {
		a.endWorkbench(sessionID)
	}
	return res, nil
}

// CloseWorkbench 释放某个运行器状态（任务被中断时调用）。
func (a *Agent) CloseWorkbench(sessionID string) {
	a.wbMu.Lock()
	defer a.wbMu.Unlock()
	a.endWorkbench(sessionID)
}

// endWorkbench 收尾：恢复会话持久化、退出评估模式、反注册延迟工具并清理运行器。
func (a *Agent) endWorkbench(sid string) {
	rt, ok := wbGet(sid)
	if ok {
		if rt.toolNames != nil {
			for _, name := range rt.toolNames {
				a.toolReg.Unregister(name)
			}
		}
		// 退出评估模式：恢复生产系统提示词模板与全量工具渲染
		a.workbenchMode = false
		a.wbToolAllowSet = nil
		a.sysTemplate = domainagent.AgentSys
		// 恢复生产环境的工具输出折叠阈值
		a.foldHeadRunes = a.savedFoldHead
		a.foldTailRunes = a.savedFoldTail
		a.savedFoldHead, a.savedFoldTail = 0, 0
		a.renderSystemPrompt()
		a.sessStore = rt.savedSessStore
		wbDel(sid)
	}
	a.wbExec = nil
}

// wbDrive 以 headless 方式推进真实推理内核 step，直到工具暂停、给出最终答案或超限。
func (a *Agent) wbDrive(ctx context.Context, rt *wbRuntime) WbResult {
	for {
		outcome, err := a.step(ctx, rt.progress, true)
		if err != nil {
			// LLM 调用失败：作为最终答案上报错误，避免无界循环
			return WbResult{State: "final", Answer: "ERROR: " + err.Error(), FunctionCalls: fmtCalls(rt.functionCalls)}
		}
		switch outcome {
		case domainagent.RunPaused:
			rt.functionCalls = append(rt.functionCalls,
				WbFunctionCall{Name: rt.progress.pausedTool, Args: rt.progress.pausedArg})
			return WbResult{
				State: "tool", ToolName: rt.progress.pausedTool, ToolArg: rt.progress.pausedArg,
				FunctionCalls: fmtCalls(rt.functionCalls),
			}
		case domainagent.RunFinal:
			return WbResult{State: "final", Answer: rt.progress.finalAnswer, FunctionCalls: fmtCalls(rt.functionCalls)}
		case domainagent.RunLimit:
			return WbResult{State: "limit", FunctionCalls: fmtCalls(rt.functionCalls)}
		}
		// RunContinue → 继续下一轮
	}
}

// fmtCalls 返回 functionCalls 的浅拷贝，避免调用方误改运行器内部状态。
func fmtCalls(calls []WbFunctionCall) []WbFunctionCall {
	out := make([]WbFunctionCall, len(calls))
	copy(out, calls)
	return out
}
