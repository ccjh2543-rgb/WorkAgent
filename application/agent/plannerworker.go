package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	domainagent "simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/planner"
	"simpleAgent/domain/session"
)

// Planner-Worker 模式：把「规划」与「执行」拆成两级角色并做上下文隔离。
//
//  - Planner（用主模型）：接收原始目标，拆解有序子任务清单；只拿到各子任务的
//    最终结果摘要，负责汇总、（失败）重规划，输出最终答案。
//  - Worker（用同一个主模型，靠提示词与裁剪区分角色）：每个子任务一个独立精简
//    上下文（当前指令 + 少量前置摘要），看不到全局执行轨迹；只回传压缩后的结果
//    摘要，原始工具输出与中间 trace 不回传。
//
// 本文件是新增的独立执行路径，与 Run 的自循环并存，不改动生产路径。子任务串行
// 执行；Worker 内部不落库、不进全局 memory，满足隔离需求。

// pwPriorBudgetRatio 前置结果摘要可占上下文预算的比例上限（防无限增长）。
const pwPriorBudgetRatio = 0.3

// pwMaxWorkers 单任务最多执行的 worker 数（条件驱动决策循环的防死循环网关）。
const pwMaxWorkers = 16

// workerCtx 单个子任务运行的内存上下文，与全局 a.memory/a.planner 完全隔离。
type workerCtx struct {
	mem         *domainagent.Memory // 仅承载：指令 + 前置摘要 + 工具往返
	system      string              // Worker 版系统提示词（含工具表）
	instruction string              // 当前子任务指令
	prior       string              // 前置结果摘要（已按预算截断）
	datetime    string              // 评估系统当前时间（WorkBench 注入），非空时加入提示
	loop        int                 // 已进行的 LLM 轮数
	loopLimit   int                 // 单子任务最大轮数
	consecFail  int                 // 连续工具失败/被拒次数
	noTrim      bool                // 全上下文模式：不裁剪 worker 记忆（保留完整历史）
}

// workerResult 单个子任务的执行结果：只回传结构化摘要，不回传原始 trace。
type workerResult struct {
	TaskID      int
	Content     string // 任务原文（供 Planner 校验）
	OK          bool
	Status      string // 收尾状态：ok / partial / failed
	Description string // 该子任务做了什么
	Result      string // 该子任务得到的结果/数据
	Loops       int
}

// Summary 供 Planner 拼接展示的结构化摘要（描述与结果合并，失败态带状态前缀）。
func (r workerResult) Summary() string {
	var sb strings.Builder
	if r.Status != "" && r.Status != "ok" {
		sb.WriteString("[" + r.Status + "] ")
	}
	if r.Description != "" {
		sb.WriteString(r.Description)
		if r.Result != "" {
			sb.WriteString("：")
		}
	}
	if r.Result != "" {
		sb.WriteString(r.Result)
	}
	if sb.Len() == 0 {
		return "（无结果）"
	}
	return sb.String()
}

// workerOutput 模型结构化收尾的 JSON 形状。
type workerOutput struct {
	Status      string `json:"status"`
	Description string `json:"description"`
	Result      string `json:"result"`
}

// parseWorkerResult 解析 Worker 收尾回复的结构化 JSON。
// 剥掉 ```json 围栏、取首个 JSON 对象解析；任何失败都降级为 status=ok、
// description 为空、result=原文，保证旧格式回复也能被消费，不阻塞链路。
// ok=false 表示解析失败（已降级）。
func parseWorkerResult(reply string) (status, desc, result string, ok bool) {
	text := strings.TrimSpace(reply)
	// 取首个 '{' 到最后一个 '}'，去掉 ```json 围栏与前后杂文
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	} else {
		return "ok", "", strings.TrimSpace(reply), false
	}
	if j := strings.LastIndex(text, "}"); j > 0 {
		text = text[:j+1]
	} else {
		return "ok", "", strings.TrimSpace(reply), false
	}

	var out workerOutput
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return "ok", "", strings.TrimSpace(reply), false
	}
	status = out.Status
	if status == "" {
		status = "ok"
	}
	return status, strings.TrimSpace(out.Description), strings.TrimSpace(out.Result), true
}

// workerFinalJSON prompt 片段：约束 Worker 在收尾时输出结构化 JSON。
const workerFinalJSON = "当你完成当前子任务、不再需要调用工具时，只输出一个 JSON 对象（禁止输出任何其他文字）：\n" +
	`{"status":"ok|partial|failed","description":"你做了什么","result":"该子任务的结果/数据"}` +
	"\n- status：ok（完整完成）/ partial（部分完成）/ failed（失败）\n" +
	"- 需要更多数据时才用 !TOOL: 调用工具。"

// RunPlannerWorker 以 Planner-Worker 模式执行任务，返回最终答案。
// 与 Run 并存：共用会话、记忆、工具与审批，但执行编排完全不同。
func (a *Agent) RunPlannerWorker(ctx context.Context, sessionID, userQuery string) (string, error) {
	a.cleanupOnce.Do(func() {
		if err := a.cleanupExpiredSessions(ctx); err != nil {
			fmt.Printf("[过期会话清理失败]:%v\n", err)
		}
	})

	a.sessionID = sessionID
	if a.sessionID == "" {
		generated, err := newSessionID()
		if err != nil {
			return "", err
		}
		a.sessionID = generated
	}
	if err := a.restoreHistory(ctx); err != nil {
		fmt.Printf("[会话历史载入失败，降级为空上下文]:%v\n", err)
		a.memory.Replace(nil)
	}
	if err := a.refreshMemoryBlock(ctx, userQuery); err != nil {
		fmt.Printf("[记忆加载失败，跳过注入]:%v\n", err)
		a.memBlock = ""
	}
	a.appendMessage(ctx, session.KindPrompt, session.RoleUser, userQuery)

	// 条件驱动决策循环：每一步 Planner 依据已完成的结构化结果决定下一步（run/final）
	a.emit(domainagent.AgentEvent{Type: domainagent.EventPlanStart, Message: userQuery})
	results := make([]workerResult, 0, 8)
	prior := ""
	taskSeq := 0
	for {
		if len(results) >= pwMaxWorkers {
			// 防死循环：达到 worker 上限强制收尾
			break
		}
		decision, err := a.pwDecideNext(ctx, userQuery, results)
		if err != nil {
			return "", err
		}
		if decision.Action != "run" {
			// 条件不满足 / 无需更多写操作：不再起 worker，直接收尾
			break
		}
		taskSeq++
		t := planner.Task{ID: taskSeq, Content: decision.Task}
		r, err := a.pwRunOne(ctx, t, prior)
		if err != nil {
			fmt.Printf("[子任务 %d 执行出错]:%v\n", t.ID, err)
			r.OK = false
			r.Status = "failed"
		}
		a.storeSubtaskResult(ctx, r)
		results = append(results, r)
		prior = appendPriorSummary(prior, r, a.countTokens(ctx, prior), a.contextBudget)
	}

	// 收束阶段：Planner 依据各子任务结果摘要汇总最终答案
	return a.pwAggregateAndRecord(ctx, userQuery, results)
}

// pwDecideNext 让 Planner 依据已完成子任务的结构化结果决定下一步（两条路径复用）。
// 把 workerResult 映射为 planner.ResultItem 后调用领域的 DecideNext。
func (a *Agent) pwDecideNext(ctx context.Context, goal string, results []workerResult) (planner.Decision, error) {
	p := planner.NewPlanner()
	items := make([]planner.ResultItem, 0, len(results))
	for _, r := range results {
		items = append(items, planner.ResultItem{
			Status:      r.Status,
			Description: r.Description,
			Result:      r.Result,
		})
	}
	return p.DecideNext(ctx, a.chatModel, goal, items)
}

// pwRunOne 运行一个子任务的迷你循环，返回压缩后的结果摘要。
func (a *Agent) pwRunOne(ctx context.Context, t plannerTask, prior string) (workerResult, error) {
	wc := &workerCtx{
		mem:         domainagent.NewMemory(),
		system:      a.pwBuildSystem("worker"),
		instruction: t.Content,
		prior:       prior,
		loopLimit:   a.maxLoop,
	}
	a.emit(domainagent.AgentEvent{Type: domainagent.EventSubtaskStart, TaskID: t.ID, Message: t.Content})

	res := workerResult{TaskID: t.ID, Content: t.Content, Loops: 0}
	failed := false
	for {
		if wc.loop >= wc.loopLimit {
			res.OK = false
			failed = true
			break
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
			return res, err
		}
		a.traceLLM(messages, reply)

		calls := mergeToolCalls(reply.ToolCalls, domainagent.ParseToolCalls(reply.Content))
		if len(calls) == 0 {
			// Worker 收尾：解析结构化 JSON
			status, desc, result, _ := parseWorkerResult(reply.Content)
			res.Status = status
			res.Description = session.Sanitize(desc, a.maxMessageBytes)
			res.Result = session.Sanitize(result, a.maxMessageBytes)
			res.OK = status != "failed"
			break
		}

		// 执行本轮发起的工具调用
		wc.mem.AddEntry(domainagent.Entry{
			Kind:      session.KindToolCall,
			Role:      session.RoleAssistant,
			Content:   reply.Content,
			ToolCalls: reply.ToolCalls,
		})
		for _, c := range calls {
			if wc.consecFail >= a.maxToolFailures {
				res.OK = false
				failed = true
				break
			}
			if err := a.pwRunTool(ctx, wc, c); err != nil {
				return res, err
			}
		}
		if failed {
			break
		}
	}

	status := "完成"
	if !res.OK {
		status = "失败"
	}
	a.emit(domainagent.AgentEvent{Type: domainagent.EventSubtaskDone, TaskID: t.ID, Result: res.Summary(), Message: status})
	return res, nil
}

// pwRunTool 在 Worker 上下文里执行单个工具并回注结果，处理审批与失败计数。
// 接收整个 ToolCall 而非拆开的名称与参数：结果要按调用 ID 配对回注。
func (a *Agent) pwRunTool(ctx context.Context, wc *workerCtx, call domainagent.ToolCall) error {
	name, arg := call.Name, call.Arg
	a.emit(domainagent.AgentEvent{Type: domainagent.EventToolCall, ToolName: name, Args: arg})

	// 参数契约校验：与 step.executeOneTool 同一道闸门，不合规先回注错误、不进审批。
	if vErr := a.toolReg.ValidateArgs(name, arg); vErr != nil {
		wc.consecFail++
		fmt.Printf("[子任务工具参数校验失败]:%v\n", vErr)
		a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: name, Args: arg, Result: vErr.Error(), Message: "工具参数校验失败"})
		wc.injectToolResult(call.ID, fmt.Sprintf("工具参数校验失败：%v。请按该工具描述中的参数规格重写这次调用。", vErr))
		return nil
	}

	approval, err := a.approveToolCall(ctx, name, arg, wc.loop)
	if err != nil {
		return err
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
		result, err := a.toolReg.ExecuteTool(name, arg)
		if err != nil {
			wc.consecFail++
			fmt.Printf("[子任务工具执行失败]:%v\n", err)
			wc.injectToolResult(call.ID, fmt.Sprintf("工具执行失败：%v。请调整参数或换一种方式。", err))
		} else {
			wc.consecFail = 0
			a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: name, Args: arg, Result: result, Message: "工具执行完成"})
			wc.injectToolResult(call.ID, renderToolOutput(name, result))
		}
	}
	return nil
}

// injectToolResult 把工具结果（折叠后）注入 Worker 上下文。
// callID 非空时按原生 tool 消息注入，与 assistant 的调用配对。
func (wc *workerCtx) injectToolResult(callID, content string) {
	wc.mem.AddEntry(toolFeedback(callID, content))
}

// pwMessages 组装发给 Worker 模型的请求消息：Worker 系统提示词 → 前置摘要 → 当前指令 → 历史。
func (a *Agent) pwMessages(wc *workerCtx) []domainagent.ChatMessage {
	head := []domainagent.ChatMessage{{Role: session.RoleSystem, Content: wc.system}}
	if wc.prior != "" {
		head = append(head, domainagent.ChatMessage{Role: session.RoleSystem, Content: renderSummary(wc.prior)})
	}
	if wc.datetime != "" {
		head = append(head, domainagent.ChatMessage{Role: session.RoleSystem, Content: "当前系统时间：" + wc.datetime})
	}
	head = append(head, domainagent.ChatMessage{Role: session.RoleUser, Content: "当前子任务：" + wc.instruction})
	entries := foldEntries(wc.mem.Entries(), a.foldHeadRunes, a.foldTailRunes)
	messages := make([]domainagent.ChatMessage, 0, len(head)+len(entries))
	messages = append(messages, head...)
	for _, e := range entries {
		messages = append(messages, e.Message())
	}
	// 裁剪窗口可能把一次工具调用的「请求」与「结果」切到两侧，统一修正配对
	return domainagent.NormalizeToolPairing(messages)
}

// trimWorkerMemory 折叠超长工具输出，并在超出预算时丢弃最旧条目，控制单子任务上下文。
func (a *Agent) trimWorkerMemory(ctx context.Context, wc *workerCtx) {
	// 全上下文模式（普通 Plan 决策式变体）：保留完整共享历史，不做裁剪
	if wc.noTrim {
		return
	}
	entries := foldEntries(wc.mem.Entries(), a.foldHeadRunes, a.foldTailRunes)
	var kept []domainagent.Entry
	used := 0
	for i := len(entries) - 1; i >= 0; i-- {
		size := a.countTokens(ctx, entries[i].Content) + messageOverheadTokens
		if used+size > pwWorkerMessageBudget(a.contextBudget) && len(kept) >= 1 {
			break
		}
		used += size
		kept = append([]domainagent.Entry{entries[i]}, kept...)
	}
	// 观测：记录 worker 上下文超预算被丢弃的条目数
	if dropped := len(entries) - len(kept); dropped > 0 {
		a.metrics.droppedWorkerEntries.Add(int64(dropped))
	}
	wc.mem.Replace(kept)
}

// pwWorkerMessageBudget 单子任务上下文里留给历史消息的 token 预算（其余给固定头）。
func pwWorkerMessageBudget(contextBudget int) int {
	b := int(float64(contextBudget) * 0.4)
	if b < 2000 {
		return 2000
	}
	return b
}

// appendPriorSummary 把单个子任务结果摘要并入累积前置信息，超预算时从最旧丢弃。
// 返回新的前置摘要串。
func appendPriorSummary(prior string, r workerResult, priorTokens, contextBudget int) string {
	sum := r.Summary()
	line := "子任务" + strconv.Itoa(r.TaskID) + ": " + sum
	if r.OK && strings.TrimSpace(sum) == "" {
		line = "子任务" + strconv.Itoa(r.TaskID) + ": （无结果）"
	}
	budget := int(float64(contextBudget) * pwPriorBudgetRatio)
	if priorTokens+tokenCountApprox(line) <= budget {
		if prior == "" {
			return line
		}
		return prior + "\n" + line
	}
	// 超预算：仅保留最新的若干条
	parts := strings.Split(prior, "\n")
	parts = append(parts, line)
	for total := 0; total <= budget && len(parts) > 1; {
		total += tokenCountApprox(parts[0])
		if total > budget {
			break
		}
		parts = parts[1:]
	}
	return strings.Join(parts, "\n")
}

// tokenCountApprox 前置摘要用的轻量 token 估算（字符数折算，不引入依赖）。
func tokenCountApprox(s string) int {
	return len([]rune(s))/3 + 1
}

// pwAggregateAndRecord 依据各子任务结果摘要汇总最终答案，并落库。
func (a *Agent) pwAggregateAndRecord(ctx context.Context, goal string, results []workerResult) (string, error) {
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
	answer := strings.TrimSpace(reply.Content)
	a.appendMessage(ctx, session.KindAnswer, session.RoleAssistant, answer)
	a.emit(domainagent.AgentEvent{Type: domainagent.EventFinal, Result: answer})
	return answer, nil
}

// storeSubtaskResult 把子任务结果摘要写入主会话（不进全局 memory，避免污染思维线）。
func (a *Agent) storeSubtaskResult(ctx context.Context, r workerResult) {
	if a.sessStore == nil {
		return
	}
	label := "成功"
	if !r.OK {
		label = "失败"
	}
	content := fmt.Sprintf("[%s] %s\n%s", label, r.Content, r.Summary())
	err := a.sessStore.Append(ctx, a.sessionID, session.Message{
		Kind:    session.KindSubtaskResult,
		Role:    session.RoleAssistant,
		Content: session.Sanitize(content, a.maxMessageBytes),
	})
	if err != nil {
		fmt.Printf("[子任务结果落盘失败]:%v\n", err)
	}
}

// pwBuildSystem 在已渲染的系统提示词前加 Planner/Worker 角色块。
func (a *Agent) pwBuildSystem(role string) string {
	switch role {
	case "worker":
		return "你是执行子任务的 Worker。只完成给定的子任务，不要越界做别的。\n" +
			"你可以调用工具获取/操作数据，但要聚焦在当前子任务。\n" +
			"完成子任务时按如下 JSON 输出结果，不含思考过程、工具原始输出或客套话：\n" + workerFinalJSON + "\n\n" + a.systemPrompt
	default:
		return "你是任务规划与汇总的 Planner。负责拆解子任务、检查各子任务结果并对原始目标给出最终答案。\n\n" + a.systemPrompt
	}
}

// plannerTask 规划器 Task 的别名，减少游走于两个包的类型命名噪音。
type plannerTask = planner.Task

// copyTasks 复制一份任务清单指针（避免共享底层切片）。
func copyTasks(in []planner.Task) []plannerTask {
	out := make([]planner.Task, len(in))
	copy(out, in)
	return out
}
