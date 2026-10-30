package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	domainagent "simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/planner"
	"simpleAgent/domain/session"
)

// 本文件把 Planner-Worker 执行路径改造成「可恢复」状态机，接入 WorkBench 外置工具桥接。
//
// 与同步版 RunPlannerWorker（plannerworker.go）的区别：
//   - 工具执行在外部工具处暂停，返回 WbResult{State:"tool"}；Python 评估器执行后
//     调 ContinuePlannerWorker 注入 observation 再恢复——完全复用现有 wb 协议。
//   - 每个 Worker 仍用独立的 workerCtx.mem，天然满足上下文隔离：只回传压缩摘要，
//     原始工具输出与中间 trace 不外泄。
//
// 通过接口层 /api/workbench/start 的 mode=planner_worker 路由到本实现。

// pwRuntime 单个 Planner-Worker 评估任务的运行期状态（跨 /continue 恢复）。
type pwRuntime struct {
	sid      string
	goal     string // 原始目标
	datetime string // 评估系统时间（注入 worker 提示）
	maxInner int
	stage    string // "plan" | "worker" | "decide" | "final"

	tasks   []plannerTask
	results []workerResult
	prior   string
	// workerSystem 每个 Worker 复用的系统提示词（worker 角色 + 工具表）
	workerSystem string
	// 当前 worker 的进行态（跨暂停保存）
	workerIdx int
	wc        *workerCtx
	res       *workerResult
	// 一批待执行工具（暂停于外部工具时保存），恢复后从 callIdx 继续
	pendingCalls []domainagent.ToolCall
	callIdx      int
	curTool      string // 当前暂停的工具名
	curArg       string // 当前暂停的工具参数
	// suspended 记录本步是否真正因为「外部工具延迟执行」而暂停。
	// pwAdvanceWorker 每步开始时清零，pwWBExecTool 遇到延迟工具时置位。
	// 驱动据此区分「暂停待外部恢复（wb 模式）」与「本地直执行完一批工具后继续」（office 模式）。
	suspended bool

	functionCalls  []WbFunctionCall // 已交出执行的工具调用（供评分）
	toolNames      []string         // 本任务注册的延迟工具名
	savedSessStore session.Repository
	// fullCtxMem 全上下文模式（普通 Plan 决策式变体）下 Worker 复用的共享记忆。
	// 非空时所有 Worker 直接读写该共享 memory（不复位、不裁剪），从而保留完整历史；
	// 为空则走隔离模式，每个 Worker 使用独立 wc.mem。
	fullCtxMem *domainagent.Memory
}

// pwRuntimes 全局 Planner-Worker 运行期注册表，按 workbench sessionID 索引。
var (
	pwMu       sync.Mutex
	pwRuntimes = map[string]*pwRuntime{}
)

func pwPut(sid string, rt *pwRuntime) {
	pwMu.Lock()
	defer pwMu.Unlock()
	pwRuntimes[sid] = rt
}

func pwGet(sid string) (*pwRuntime, bool) {
	pwMu.Lock()
	defer pwMu.Unlock()
	rt, ok := pwRuntimes[sid]
	return rt, ok
}

func pwDel(sid string) {
	pwMu.Lock()
	defer pwMu.Unlock()
	delete(pwRuntimes, sid)
}

// IsPlannerWorker 判断某 workbench 会话是否为 Planner-Worker 模式（供接口层路由）。
func (a *Agent) IsPlannerWorker(sid string) bool {
	_, ok := pwGet(sid)
	return ok
}

// StartPlannerWorker 以隔离式 Planner-Worker 模式启动一个 WorkBench 任务运行器。
func (a *Agent) StartPlannerWorker(ctx context.Context, task string, toolDescs []string, datetime string, maxInner int) (string, WbResult, error) {
	return a.startPW(ctx, task, toolDescs, datetime, maxInner, false, false)
}

// StartOfficePlannerWorker 以隔离式 Planner-Worker 模式驱动一个 Office 办公任务：
// 本地文件工具在进程内直接执行，非文件工具由外部延迟执行。
func (a *Agent) StartOfficePlannerWorker(ctx context.Context, task string, toolDescs []string, datetime string, maxInner int) (string, WbResult, error) {
	return a.startPW(ctx, task, toolDescs, datetime, maxInner, false, true)
}

// startPW 以决策式 replan 状态机启动一个 WorkBench 任务运行器。
// 环境准备与旧 StartWorkbench 一致：
//   - fullCtx=true：普通 Plan 决策式变体，Worker 复用共享 a.memory（全上下文不隔离），
//     与 planner-worker 唯一差异就是「是否隔离」，便于公平对比窗口/决策式 replan 的效果。
//   - fullCtx=false：隔离式 Planner-Worker（每个 Worker 独立 wc.mem）。
//   - officeMode=true：本地文件工具（read_file/list_dir/search_files/write_file）
//     wbExec 放行到 toolReg.ExecuteTool 在进程内直执行，其余工具仍由外部延迟执行；
//     用于 Office 办公任务评测（agent 在本地工作区读写真实文件）。
//
// 三种路径共用 pwWorkbenchDrive（规划→逐子任务→pwDecideNext 决策校验→下一步）。
func (a *Agent) startPW(ctx context.Context, task string, toolDescs []string, datetime string, maxInner int, fullCtx, officeMode bool) (string, WbResult, error) {
	a.wbMu.Lock()
	defer a.wbMu.Unlock()

	sid, err := newWbSessionID()
	if err != nil {
		return "", WbResult{}, fmt.Errorf("生成 workbench 会话 ID 失败: %w", err)
	}
	if maxInner <= 0 {
		maxInner = 20
	}
	rt := &pwRuntime{
		sid:            sid,
		goal:           task,
		datetime:       datetime,
		maxInner:       maxInner,
		stage:          "plan",
		functionCalls:  make([]WbFunctionCall, 0, 16),
		savedSessStore: a.sessStore,
	}
	// 评估不落盘：挂起会话持久化，结束时恢复
	a.sessStore = nil
	a.sessionID = sid

	a.workbenchMode = true
	a.wbToolAllowSet = make(map[string]bool)
	a.sysTemplate = domainagent.WorkbenchSystemPrompt

	// 评估模式强制禁用工具输出折叠
	a.savedFoldHead = a.foldHeadRunes
	a.savedFoldTail = a.foldTailRunes
	a.foldHeadRunes = math.MaxInt / 2
	a.foldTailRunes = math.MaxInt / 2

	// 干净上下文（决策式状态机自身维护 worker 上下文，不依赖全局 memory）
	a.memory.Replace(nil)
	a.planner = planner.NewPlanner()
	a.resetCompaction()
	a.memBlock = ""

	// 把评估工具以延迟工具注册进本体
	for _, line := range toolDescs {
		name, desc := splitToolDesc(line)
		if name == "" {
			continue
		}
		a.wbToolAllowSet[name] = true
		if a.toolReg.Lookup(name) != nil {
			continue
		}
		a.toolReg.RegisterWithPolicy(&deferredTool{name: name, desc: desc}, wbToolPolicy(name))
		rt.toolNames = append(rt.toolNames, name)
	}
	// Office 模式：本地文件工具已在 initFileTools 注册为真实工具（非 deferredTool），
	// 上面的循环不会把它们加进白名单。这里补入白名单，使 workbenchMode 下
	// renderSystemPrompt 的 GetAllToolsDescFiltered 能渲染它们的描述。
	if officeMode {
		for name := range a.wbFileTools {
			if a.toolReg.Lookup(name) != nil {
				a.wbToolAllowSet[name] = true
			}
		}
	}
	if len(rt.toolNames) > 0 || officeMode {
		a.renderSystemPrompt()
	}
	// 固化 Worker 系统提示词（worker 角色块 + 含工具表的评估系统提示词）
	rt.workerSystem = a.pwBuildSystem("worker")

	// 全上下文模式：把共享 a.memory 交给运行器作为跨 worker 的持续记忆
	if fullCtx {
		rt.fullCtxMem = a.memory
	}

	// 评估模式：工具执行交由外部并触发暂停。
	// Office 模式放行本地文件工具（直执行真实文件）；其余工具不回传给外部执行，
	// 而是作为工具错误回注，让模型改用可用的文件工具重试。
	a.wbExec = func(name, arg string) (string, error) {
		if officeMode {
			if a.wbFileTools[name] {
				return a.toolReg.ExecuteTool(name, arg)
			}
			return "", fmt.Errorf("工具 %s 在当前办公任务中不可用，只能使用本地文件工具：%s。请改用这些工具继续完成当前步骤", name, officeToolHint)
		}
		return "", errToolDeferred
	}
	// 观测：每个任务独立统计上下文压缩/折叠指标
	a.metrics.Reset()
	pwPut(sid, rt)

	res := a.pwWorkbenchDrive(ctx, rt)
	res.Metrics = a.metricsSnapshot()
	logWbMetrics(sid, res.Metrics)
	if res.State == "final" || res.State == "limit" {
		a.pwEnd(sid)
	}
	return sid, res, nil
}

// ContinuePlannerWorker 注入一行工具 observation 后恢复暂停的 Planner-Worker 运行器。
func (a *Agent) ContinuePlannerWorker(ctx context.Context, sessionID, observation string) (WbResult, error) {
	a.wbMu.Lock()
	defer a.wbMu.Unlock()

	rt, ok := pwGet(sessionID)
	if !ok {
		return WbResult{}, fmt.Errorf("planner-workbench 会话不存在: %s", sessionID)
	}
	// 回注 observation 到当前 worker 的上下文
	if isWorkbenchError(observation) {
		rt.wc.consecFail++
	} else {
		rt.wc.consecFail = 0
	}
	rt.wc.injectToolResult(rt.curTool, renderToolOutput(rt.curTool, observation))
	rt.curTool, rt.curArg = "", ""

	res := a.pwWorkbenchDrive(ctx, rt)
	res.Metrics = a.metricsSnapshot()
	if res.State == "final" || res.State == "limit" {
		a.pwEnd(sessionID)
		logWbMetrics(sessionID, res.Metrics)
	}
	return res, nil
}

// logWbMetrics 打印单任务的上下文压缩/折叠指标，便于核对窗口预算是否合理。
func logWbMetrics(sid string, m WbMetrics) {
	fmt.Printf("[指标] planner-worker 会话 %s 压缩轮次=%d 并入摘要条目=%d 折叠工具输出=%d worker丢弃条目=%d 最大请求token=%d LLM调用=%d\n",
		sid, m.CompactionRounds, m.CompactedEntries, m.FoldedToolResults, m.DroppedWorkerEntries, m.MaxWindowTokens, m.LLMCalls)
}

// ClosePlannerWorker 释放某个运行器状态（任务被中断时调用）。
func (a *Agent) ClosePlannerWorker(sessionID string) {
	a.wbMu.Lock()
	defer a.wbMu.Unlock()
	a.pwEnd(sessionID)
}

// pwWorkbenchDrive 以 headless 方式推进 Planner-Worker 状态机，直到工具暂停、
// 给出最终答案或超限。
func (a *Agent) pwWorkbenchDrive(ctx context.Context, rt *pwRuntime) WbResult {
	for {
		switch rt.stage {
		case "plan":
			// 首次决策：无任何结果，由 Planner 给出第一步
			decision, err := a.pwDecideNext(ctx, rt.goal, rt.results)
			if err != nil {
				return WbResult{State: "final", Answer: "ERROR: " + err.Error(), FunctionCalls: rt.functionCalls}
			}
			if decision.Action != "run" {
				rt.stage = "final"
				continue
			}
			rt.appendTask(decision.Task)
			rt.prepareWorker(0)
			rt.stage = "worker"

		case "worker":
			done, err := a.pwAdvanceWorker(ctx, rt)
			if err != nil {
				return WbResult{State: "final", Answer: "ERROR: " + err.Error(), FunctionCalls: rt.functionCalls}
			}
			if !done {
				if rt.suspended {
					// 真正暂停于外部延迟工具：交还控制权，待外部 /continue 恢复
					return WbResult{
						State: "tool", ToolName: rt.curTool, ToolArg: rt.curArg,
						FunctionCalls: rt.functionCalls,
					}
				}
				// 未暂停（office 本地直执行完一批文件工具）且 worker 尚未收尾：
				// 继续推进下一轮模型调用，无需外部介入。
				continue
			}
			// 当前 worker 完成
			rt.recordResult()
			if len(rt.results) >= pwMaxWorkers {
				// 防死循环：达到 worker 上限强制收尾
				rt.stage = "final"
				continue
			}
			// 不再武断继续预置任务：转决策阶段，由 Planner 判断是否需要下一步
			rt.stage = "decide"

		case "decide":
			decision, err := a.pwDecideNext(ctx, rt.goal, rt.results)
			if err != nil {
				return WbResult{State: "final", Answer: "ERROR: " + err.Error(), FunctionCalls: rt.functionCalls}
			}
			if decision.Action != "run" {
				// 条件不满足 / 无需更多写操作：不再起 write worker，直接收尾
				rt.stage = "final"
				continue
			}
			rt.appendTask(decision.Task)
			rt.workerIdx = len(rt.tasks) - 1
			rt.prepareWorker(rt.workerIdx)
			rt.stage = "worker"

		case "final":
			ans, err := a.pwWBAggregate(ctx, rt.goal, rt.results)
			if err != nil {
				return WbResult{State: "final", Answer: "ERROR: " + err.Error(), FunctionCalls: rt.functionCalls}
			}
			return WbResult{State: "final", Answer: ans, FunctionCalls: rt.functionCalls}
		}
	}
}

// pwAppendTask 把决策器给出的下一个任务追加进任务列表。
func (rt *pwRuntime) appendTask(content string) {
	rt.tasks = append(rt.tasks, plannerTask{ID: len(rt.tasks) + 1, Content: content})
}

// pwAdvanceWorker 推进当前 worker 一步：恢复未跑完的工具批，或发起新一轮模型调用。
// 返回 done=true 表示该 worker 已产出最终摘要；false 表示暂停于外部工具待恢复。
func (a *Agent) pwAdvanceWorker(ctx context.Context, rt *pwRuntime) (bool, error) {
	wc := rt.wc
	res := rt.res

	// 每步开始时清除暂停标记：本步是否真暂停取决于后续工具是否遇到延迟执行。
	rt.suspended = false

	// 恢复场景：上一批工具还有未跑完的
	if rt.pendingCalls != nil {
		for rt.callIdx < len(rt.pendingCalls) {
			c := rt.pendingCalls[rt.callIdx]
			suspended := a.pwWBExecTool(ctx, rt, wc, c)
			rt.callIdx++
			if suspended {
				return false, nil
			}
		}
		rt.pendingCalls = nil
		rt.callIdx = 0
	}

	// 循环上限
	if wc.loop >= wc.loopLimit {
		res.OK = false
		return true, nil
	}
	wc.loop++
	res.Loops = wc.loop

	a.trimWorkerMemory(ctx, wc)
	messages := a.pwMessages(wc)
	reply, err := a.chatModel.Generate(ctx, domainagent.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		Temperature: 0.2,
		Tools:       a.toolDecls,
	})
	if err != nil {
		return false, err
	}
	a.traceLLM(messages, reply)

	calls := mergeToolCalls(reply.ToolCalls, domainagent.ParseToolCalls(reply.Content))
	if len(calls) == 0 {
		// Worker 收尾：解析结构化 JSON（与同步版 pwRunOne 一致）
		status, desc, result, _ := parseWorkerResult(reply.Content)
		res.Status = status
		res.Description = session.Sanitize(desc, a.maxMessageBytes)
		res.Result = session.Sanitize(result, a.maxMessageBytes)
		res.OK = status != "failed"
		return true, nil
	}

	// 记录工具调用批次，逐个执行（可能暂停）
	wc.mem.AddEntry(domainagent.Entry{
		Kind:      session.KindToolCall,
		Role:      session.RoleAssistant,
		Content:   reply.Content,
		ToolCalls: reply.ToolCalls,
	})
	rt.pendingCalls = calls
	rt.callIdx = 0
	for rt.callIdx < len(calls) {
		if wc.consecFail >= a.maxToolFailures {
			res.OK = false
			return true, nil
		}
		c := calls[rt.callIdx]
		suspended := a.pwWBExecTool(ctx, rt, wc, c)
		rt.callIdx++
		if suspended {
			return false, nil
		}
	}
	rt.pendingCalls = nil
	return false, nil
}

// pwWBExecTool 在 Worker 上下文执行单个工具；外部工具暂停返回 true（待 /continue）。
// 接收整个 ToolCall：结果要按调用 ID 配对回注。
func (a *Agent) pwWBExecTool(ctx context.Context, rt *pwRuntime, wc *workerCtx, call domainagent.ToolCall) bool {
	name, arg := call.Name, call.Arg
	a.emit(domainagent.AgentEvent{Type: domainagent.EventToolCall, ToolName: name, Args: arg})

	// 参数契约校验：与 step.executeOneTool 同一道闸门，不合规先回注错误、不进审批。
	// 外部延迟工具未声明规格，此处自动放行，行为不变。
	if vErr := a.toolReg.ValidateArgs(name, arg); vErr != nil {
		wc.consecFail++
		fmt.Printf("[子任务工具参数校验失败]:%v\n", vErr)
		a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: name, Args: arg, Result: vErr.Error(), Message: "工具参数校验失败"})
		wc.injectToolResult(call.ID, fmt.Sprintf("工具参数校验失败：%v。请按该工具描述中的参数规格重写这次调用。", vErr))
		return false
	}

	approval, err := a.approveToolCall(ctx, name, arg, wc.loop)
	if err != nil {
		wc.consecFail++
		wc.injectToolResult(call.ID, fmt.Sprintf("工具审批失败：%v。请调整或换一种方式。", err))
		return false
	}
	doExecute := true
	switch approval.Decision {
	case dapproval.DecisionReject:
		doExecute = false
		wc.consecFail++
		wc.injectToolResult(call.ID, rejectionFeedback(approval.Reason))
	case dapproval.DecisionModify:
		arg = approval.Arguments
	}
	if doExecute {
		result, toolErr := a.runTool(name, arg)
		if toolErr == errToolDeferred {
			// 评估模式：工具交由外部执行，暂停返回
			rt.suspended = true
			rt.functionCalls = append(rt.functionCalls, WbFunctionCall{Name: name, Args: arg})
			rt.curTool, rt.curArg = name, arg
			return true
		}
		if toolErr != nil {
			wc.consecFail++
			fmt.Printf("[子任务工具执行失败]:%v\n", toolErr)
			wc.injectToolResult(call.ID, fmt.Sprintf("工具执行失败：%v。请调整参数或换一种方式。", toolErr))
		} else {
			wc.consecFail = 0
			a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: name, Args: arg, Result: result, Message: "工具执行完成"})
			wc.injectToolResult(call.ID, renderToolOutput(name, result))
		}
	}
	return false
}

// prepareWorker 初始化第 idx 个 worker 的运行期状态（重置上下文）。
func (rt *pwRuntime) prepareWorker(idx int) {
	rt.workerIdx = idx
	t := rt.tasks[idx]

	mem := domainagent.NewMemory()
	noTrim := false
	if rt.fullCtxMem != nil {
		// 全上下文模式：复用跨 worker 共享记忆，保证后续子任务能看到完整历史
		mem = rt.fullCtxMem
		noTrim = true
		// 当前子任务指令作为普通用户消息入共享历史，避免隔离头的重复膨胀
		mem.Add(session.KindPrompt, session.RoleUser, "当前子任务："+t.Content)
	}

	rt.wc = &workerCtx{
		mem:         mem,
		system:      rt.workerSystem,
		instruction: t.Content,
		prior:       rt.prior,
		datetime:    rt.datetime,
		loopLimit:   rt.maxInner,
		noTrim:      noTrim,
	}
	rt.res = &workerResult{TaskID: t.ID, Content: t.Content}
	rt.pendingCalls = nil
	rt.callIdx = 0
	rt.curTool, rt.curArg = "", ""
}

// recordResult 把当前 worker 结果存入 results，并把它并入前置摘要。
func (rt *pwRuntime) recordResult() {
	rt.results = append(rt.results, *rt.res)
	priorTokens := tokenCountApprox(rt.prior)
	// 与主上下文预算保持一致，避免硬编码口径漂移
	rt.prior = appendPriorSummary(rt.prior, *rt.res, priorTokens, defaultContextBudget)
}

// pwWBAggregate 依据各子任务结果摘要汇总最终答案（headless，不落盘、不回写全局 memory）。
func (a *Agent) pwWBAggregate(ctx context.Context, goal string, results []workerResult) (string, error) {
	var sb strings.Builder
	for _, r := range results {
		status := "成功"
		if !r.OK {
			status = "失败"
		}
		fmt.Fprintf(&sb, "子任务%d[%s] %s\n结果摘要：%s\n", r.TaskID, status, r.Content, r.Summary())
	}
	prompt := "请综合下面各子任务的结果，针对原始目标给出最终答案。\n目标：" + goal + "\n\n" + sb.String()
	reply, err := a.chatModel.Generate(ctx, domainagent.ChatRequest{
		Model:       a.model,
		Messages:    []domainagent.ChatMessage{{Role: session.RoleUser, Content: prompt}},
		Temperature: 0.2,
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(reply.Content), nil
}

// pwEnd 收尾：恢复会话持久化、退出评估模式、反注册延迟工具并清理运行器。
func (a *Agent) pwEnd(sid string) {
	rt, ok := pwGet(sid)
	if !ok {
		a.wbExec = nil
		return
	}
	if rt.toolNames != nil {
		for _, name := range rt.toolNames {
			a.toolReg.Unregister(name)
		}
	}
	a.workbenchMode = false
	a.wbToolAllowSet = nil
	a.sysTemplate = domainagent.AgentSys
	a.foldHeadRunes = a.savedFoldHead
	a.foldTailRunes = a.savedFoldTail
	a.savedFoldHead, a.savedFoldTail = 0, 0
	a.renderSystemPrompt()
	a.sessStore = rt.savedSessStore
	pwDel(sid)
	a.wbExec = nil
}
