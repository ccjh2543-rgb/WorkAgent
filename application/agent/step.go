package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	domainagent "simpleAgent/domain/agent"
	dapproval "simpleAgent/domain/approval"
	"simpleAgent/domain/session"
	"simpleAgent/domain/tool"
)

// defaultMaxToolFailures 连续工具失败或被拒的默认次数上限
const defaultMaxToolFailures = 3

// defaultMaxReadOnlyStalls 只读工具「连续成功」的停滞阈值。
const defaultMaxReadOnlyStalls = 3

// errToolDeferred 工具执行被延迟到外部（WorkBench 场景）的哨兵错误。
// step 检测到它时把模型请求的工具置入 runProgress 并暂停，等外部注入 observation。
var errToolDeferred = errors.New("tool execution deferred to external")

// runProgress 一次 Run / WorkBench 任务的跨恢复循环状态。
// 生产 Run 在一个函数内跑到底；WorkBench 每次 Continue 恢复时读取它继续推进。
type runProgress struct {
	loopCount           int                    // 已进行的思考轮数
	loopLimit           int                    // 最大思考轮数，人工介入可延长
	consecutiveFailures int                    // 连续工具失败/被拒次数
	readOnlyStalls      int                    // 连续「只读工具成功」次数，用于判定探测停滞
	pendingToolCalls    []domainagent.ToolCall // 待执行工具队列
	finalAnswer         string                 // RunFinal 时携带的最终答案
	toolOutput          domainagent.Entry      // 待回注给模型的工具结果；Content 为空表示无
	pausedTool          string                 // RunPaused 时暂存的工具名
	pausedArg           string                 // RunPaused 时暂存的工具参数
}

// toolFeedback 构造一条回注给模型的工具反馈消息。
//
// callID 非空表示这是原生 tool calling 的结果：按 tool 角色回注并带上调用 ID，
// 与 assistant 消息里的工具调用配对，模型能明确知道「这条结果属于哪次调用」。
// callID 为空（文本协议）时按普通 user 消息回注，与改造前完全一致。
// content 由调用方决定（工具真实输出需先经 renderToolOutput 包上 <tool_output> 边界）。
func toolFeedback(callID, content string) domainagent.Entry {
	if callID == "" {
		return domainagent.Entry{Kind: session.KindToolResult, Role: session.RoleUser, Content: content}
	}
	return domainagent.Entry{
		Kind:       session.KindToolResult,
		Role:       session.RoleTool,
		Content:    content,
		ToolCallID: callID,
	}
}

// execFn 工具执行回调。生产模式为 toolReg 本机执行；WorkBench 模式返回 errToolDeferred 触发暂停。
type execFn func(name, arg string) (string, error)

// runTool 通过当前执行通道执行一次工具调用。
func (a *Agent) runTool(name, arg string) (string, error) {
	if a.wbExec != nil {
		return a.wbExec(name, arg)
	}
	return a.toolReg.ExecuteTool(name, arg)
}

// step 推进一轮 Run 推理状态机，作为生产 Run 与 WorkBench 复用的公共内核。
//
// headless 表示无人在回路（评估场景）：循环耗尽或连续失败时直接以 RunLimit 收束，
// 不再调用 requestGuidance 请求人工指令；生产（headless=false）保持原有人工介入逻辑。
func (a *Agent) step(ctx context.Context, p *runProgress, headless bool) (domainagent.RunOutcome, error) {
	// 批量续做：上一回复还有待执行工具时，不进入 LLM 轮。
	if len(p.pendingToolCalls) > 0 {
		if p.toolOutput.Content != "" {
			a.appendEntry(ctx, p.toolOutput)
			p.toolOutput = domainagent.Entry{}
		}
		return a.executeOneTool(ctx, p, headless)
	}

	// 闸门三：循环次数耗尽仍未收敛
	if p.loopCount >= p.loopLimit {
		if headless {
			return domainagent.RunLimit, nil
		}
		guidance, err := a.requestGuidance(ctx,
			fmt.Sprintf("已达到最大循环次数 %d，仍未给出最终答案", p.loopLimit))
		if err != nil {
			return domainagent.RunContinue, err
		}
		if guidance == "" {
			p.finalAnswer = "任务已由人工中止，未产生最终答案。"
			return domainagent.RunFinal, nil
		}
		a.appendMessage(ctx, session.KindGuidance, session.RoleUser, guidance)
		p.consecutiveFailures = 0
		p.loopLimit++
	}

	p.loopCount++
	fmt.Printf("\n===== Agent 第 %d 轮思考 =====\n", p.loopCount)
	a.emit(domainagent.AgentEvent{Type: domainagent.EventRound, Message: fmt.Sprintf("第 %d 轮思考", p.loopCount)})

	// 上一轮产生的工具结果作为用户消息注入；内容已包进 <tool_output> 边界
	if p.toolOutput.Content != "" {
		a.appendEntry(ctx, p.toolOutput)
		p.toolOutput = domainagent.Entry{}
	}

	resp, err := a.callLLM(ctx)
	if err != nil {
		return domainagent.RunContinue, err
	}

	// 懒触发规划：模型判定任务复杂、需拆解时输出 !PLAN:目标
	if strings.HasPrefix(resp.Content, "!PLAN:") {
		goal := strings.TrimSpace(strings.TrimPrefix(resp.Content, "!PLAN:"))
		if err := a.doPlan(ctx, goal); err != nil {
			return domainagent.RunContinue, err
		}
		return domainagent.RunContinue, nil
	}

	// 工具调用优先取模型的原生结构化调用（带调用 ID）；模型改走文本协议时兜底解析。
	// 两者都为空才算「没有动作」，此时 Content 才是最终答案——只看 Content 会把
	// 「原生调用 + 空正文」误判成最终答案，任务被当场当成已完成。
	calls := mergeToolCalls(resp.ToolCalls, domainagent.ParseToolCalls(resp.Content))
	responseKind := session.KindAnswer
	if len(calls) > 0 {
		responseKind = session.KindToolCall
	}
	a.appendEntry(ctx, domainagent.Entry{
		Kind:      responseKind,
		Role:      session.RoleAssistant,
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})

	if len(calls) == 0 {
		// 格式兜底：模型若绕过 !TOOL: 协议、改用伪 XML 表达工具调用，
		// 不能把它当成最终答案。此时注入纠正消息让它重写。
		if domainagent.IsPseudoXMLToolCall(resp.Content) {
			fmt.Printf("[检测到伪 XML 工具调用，注入纠正]:%v\n", resp.Content)
			p.readOnlyStalls++
			p.toolOutput = toolFeedback("", "你刚才用 XML 形式（如 <invoke>/<tool_call>/<parameter>）表达了工具调用，"+
				"但本环境只接受 !TOOL: 文本协议。请按「!TOOL:工具名|参数」重写你要执行的动作，"+
				"继续推进任务，不要询问或解释。")
			return domainagent.RunContinue, nil
		}
		p.finalAnswer = resp.Content
		return domainagent.RunFinal, nil
	}
	p.pendingToolCalls = calls
	return a.executeOneTool(ctx, p, headless)
}

// mergeToolCalls 合并原生工具调用与文本协议调用，返回本轮要执行的完整队列。
//
// 正常情况下两者只会命中一个；若模型同时给出（例如原生调用后又在正文里补一句
// !TOOL:），以原生调用为准——它带调用 ID、能与结果配对，文本那份视为重复描述。
func mergeToolCalls(native []domainagent.ToolCall, parsed []domainagent.ToolCall) []domainagent.ToolCall {
	if len(native) > 0 {
		return native
	}
	return parsed
}

// executeOneTool 从待执行队列中取出队首工具，完成审批与执行后返回。
func (a *Agent) executeOneTool(ctx context.Context, p *runProgress, headless bool) (domainagent.RunOutcome, error) {
	if len(p.pendingToolCalls) == 0 {
		return domainagent.RunContinue, nil
	}
	call := p.pendingToolCalls[0]
	toolName, toolArg := call.Name, call.Arg
	p.pendingToolCalls = p.pendingToolCalls[1:]
	a.emit(domainagent.AgentEvent{Type: domainagent.EventToolCall, ToolName: toolName, Args: toolArg})

	// 参数契约校验：工具声明了参数规格时先按规格校验，不合规直接回注错误让模型重写。
	// 放在审批之前，既不必把脏参数送进人工确认，也避免工具拿缺省值静默跑出错误结果。
	if vErr := a.toolReg.ValidateArgs(toolName, toolArg); vErr != nil {
		p.consecutiveFailures++
		fmt.Printf("[工具参数校验失败]:%v\n", vErr)
		a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: toolName, Args: toolArg, Result: vErr.Error(), Message: "工具参数校验失败"})
		p.toolOutput = toolFeedback(call.ID, fmt.Sprintf("工具参数校验失败：%v。请按该工具描述中的参数规格重写这次调用。", vErr))
		return a.resolveFailureGate(ctx, p, headless)
	}

	// 闸门二：工具审批
	approval, err := a.approveToolCall(ctx, toolName, toolArg, p.loopCount)
	if err != nil {
		return domainagent.RunContinue, err
	}

	doExecute := true
	switch approval.Decision {
	case dapproval.DecisionReject:
		doExecute = false
		p.consecutiveFailures++
		fmt.Printf("[工具被拒绝]:%s\n", toolName)
		a.emit(domainagent.AgentEvent{Type: domainagent.EventToolRejected, ToolName: toolName, Args: toolArg, Message: approval.Reason})
		p.toolOutput = toolFeedback(call.ID, rejectionFeedback(approval.Reason))
	case dapproval.DecisionModify:
		toolArg = approval.Arguments
		doExecute = true
	default: // DecisionApprove
		doExecute = true
	}

	if doExecute {
		toolResult, toolErr := a.runTool(toolName, toolArg)
		if toolErr == errToolDeferred {
			// 评估模式：工具交由外部执行，暂停返回
			p.pausedTool, p.pausedArg = toolName, toolArg
			return domainagent.RunPaused, nil
		}
		if toolErr != nil {
			p.consecutiveFailures++
			fmt.Printf("[工具执行失败]:%v\n", toolErr)
			a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: toolName, Args: toolArg, Result: toolErr.Error(), Message: "工具执行失败"})
			p.toolOutput = toolFeedback(call.ID, fmt.Sprintf("工具执行失败：%v。请调整参数或换一种方式。", toolErr))
		} else {
			p.consecutiveFailures = 0
			fmt.Printf("[工具执行完毕]:%s\n", toolResult)
			a.emit(domainagent.AgentEvent{Type: domainagent.EventToolResult, ToolName: toolName, Args: toolArg, Result: toolResult, Message: "工具执行完成"})
			a.advanceStall(ctx, p, toolName, true)
			p.toolOutput = toolFeedback(call.ID, renderToolOutput(toolName, toolResult))
			if task := a.planner.GetNextUnDoneTask(); task != nil {
				a.planner.MarkTaskDone(task.ID)
			}
		}
	}

	return a.resolveFailureGate(ctx, p, headless)
}

// resolveFailureGate 闸门三：连续工具失败、参数校验失败或被拒达到上限时收束运行。
//
// 未达上限直接返回 RunContinue，由调用方继续下一轮；达到上限时 headless 场景以
// RunLimit 收束，生产场景则请求人工指令（人工中止则给出终止性最终答案）。
// 参数校验失败与工具执行失败共用这一处出口，避免同一段收束逻辑重复两遍。
func (a *Agent) resolveFailureGate(ctx context.Context, p *runProgress, headless bool) (domainagent.RunOutcome, error) {
	if p.consecutiveFailures < a.maxToolFailures {
		return domainagent.RunContinue, nil
	}
	if headless {
		return domainagent.RunLimit, nil
	}
	guidance, err := a.requestGuidance(ctx,
		fmt.Sprintf("连续 %d 次工具操作被拒绝、参数校验失败或执行失败", p.consecutiveFailures))
	if err != nil {
		return domainagent.RunContinue, err
	}
	if guidance == "" {
		p.finalAnswer = "任务已由人工中止，未产生最终答案。"
		return domainagent.RunFinal, nil
	}
	a.appendMessage(ctx, session.KindGuidance, session.RoleUser, guidance)
	p.consecutiveFailures = 0
	return domainagent.RunContinue, nil
}

// doPlan 由模型主动触发规划：生成任务拆解、上报事件并进入计划确认闸门。
func (a *Agent) doPlan(ctx context.Context, goal string) error {
	if err := a.planner.GeneratePlan(ctx, a.chatModel, goal); err != nil {
		return err
	}
	a.emit(domainagent.AgentEvent{Type: domainagent.EventPlanReady, Message: a.planner.PlanText()})
	return a.confirmPlan(ctx)
}

// confirmPlan 闸门一：把拆解结果交给人工过目，支持放行、否决与整体改写
func (a *Agent) confirmPlan(ctx context.Context) error {
	if len(a.planner.Tasks) == 0 {
		return nil
	}
	approval, err := a.approver.Approve(ctx, dapproval.ApprovalRequest{
		Kind:      dapproval.KindPlan,
		Arguments: a.planner.PlanText(),
		Summary:   fmt.Sprintf("已拆解出 %d 步任务，确认后开始执行", len(a.planner.Tasks)),
		Risk:      tool.RiskWriteLocal,
	})
	if err != nil {
		return err
	}
	switch approval.Decision {
	case dapproval.DecisionReject:
		return fmt.Errorf("用户否决了执行计划: %s", strings.TrimSpace(approval.Reason))
	case dapproval.DecisionModify:
		a.planner.Replace(approval.Arguments)
		fmt.Println("[计划已被人工改写]:")
		for _, task := range a.planner.Tasks {
			fmt.Println("任务", task.ID, ":", task.Content)
		}
	}
	return nil
}

// approveToolCall 闸门二：按工具策略决定是否需要人工审批
func (a *Agent) approveToolCall(ctx context.Context, toolName, toolArg string, round int) (dapproval.ApprovalResult, error) {
	policy := a.toolReg.Policy(toolName)
	if policy.AutoApprove {
		return dapproval.ApprovalResult{Decision: dapproval.DecisionApprove}, nil
	}
	return a.approver.Approve(ctx, dapproval.ApprovalRequest{
		Kind:      dapproval.KindTool,
		ToolName:  toolName,
		Arguments: toolArg,
		Summary:   a.describeToolCall(toolName, toolArg),
		Risk:      policy.Risk,
		Round:     round,
	})
}

// describeToolCall 生成给人看的操作说明。
func (a *Agent) describeToolCall(toolName, toolArg string) string {
	if t := a.toolReg.Lookup(toolName); t != nil {
		if desc, ok := t.(tool.ActionDescriber); ok {
			if detail := strings.TrimSpace(desc.Describe(toolArg)); detail != "" {
				return detail
			}
		}
	}
	return fmt.Sprintf("即将调用工具 %s", toolName)
}

// requestGuidance 闸门三：把当前的困境交给人工，返回新的执行指令。
func (a *Agent) requestGuidance(ctx context.Context, reason string) (string, error) {
	a.emit(domainagent.AgentEvent{Type: domainagent.EventStuck, Message: reason})
	approval, err := a.approver.Approve(ctx, dapproval.ApprovalRequest{
		Kind:    dapproval.KindStuck,
		Summary: reason,
		Risk:    tool.RiskReadOnly,
	})
	if err != nil {
		return "", err
	}
	if approval.Decision != dapproval.DecisionModify {
		return "", nil
	}
	return strings.TrimSpace(approval.Arguments), nil
}

// rejectionFeedback 把拒绝理由转成回注对话的文案。
func rejectionFeedback(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "该操作被用户拒绝。请换一种方式或直接给出结论。"
	}
	return "该操作被用户拒绝。原因：" + reason + "。请根据该原因换一种方式，或直接给出结论。"
}

// callLLM 请求大模型。请求消息由 prepareContext 组装。
// Tools 带上当前可声明的原生工具（无则退化为纯文本请求）。
func (a *Agent) callLLM(ctx context.Context) (domainagent.ChatResponse, error) {
	messages, err := a.prepareContext(ctx)
	if err != nil {
		return domainagent.ChatResponse{}, err
	}
	reply, err := a.chatModel.Generate(ctx, domainagent.ChatRequest{
		Model:       a.model,
		Messages:    messages,
		Temperature: 0.2,
		Tools:       a.toolDecls,
	})
	if err != nil {
		return domainagent.ChatResponse{}, err
	}
	// 诊断落盘：把发给模型的确切消息与原始回答一并记录到 trace 文件
	a.traceLLM(messages, reply)
	return reply, nil
}

// refreshMemoryBlock 按当前用户请求刷新待注入的工作区记忆块。
func (a *Agent) refreshMemoryBlock(ctx context.Context, query string) error {
	if a.memLoad == nil {
		a.memBlock = ""
		return nil
	}
	block, err := a.memLoad.BuildContext(ctx, query)
	if err != nil {
		return err
	}
	a.memBlock = block
	return nil
}

// advanceStall 依据最近一次工具执行的读写属性与成败，推进停滞计数。
func (a *Agent) advanceStall(ctx context.Context, p *runProgress, toolName string, ok bool) {
	if ok && a.toolReg.Policy(toolName).Risk == tool.RiskReadOnly {
		p.readOnlyStalls++
	} else {
		p.readOnlyStalls = 0
	}
	if p.readOnlyStalls >= defaultMaxReadOnlyStalls {
		p.readOnlyStalls = 0
		a.appendMessage(ctx, session.KindGuidance, session.RoleUser,
			fmt.Sprintf("检测到你已连续 %d 次仅做信息查询且均成功（只读工具）。"+
				"若当前掌握的信息已足以完成任务，请立即执行目标操作（发送/转发/删除/回复等），或直接给出最终答案；"+
				"若确实仍缺关键信息，请只说明缺少什么，并一步到位地再查一次，不要重复已经做过的查询。",
				defaultMaxReadOnlyStalls))
	}
}

// appendMessage 追加一条消息到会话上下文，并同步落盘。
func (a *Agent) appendMessage(ctx context.Context, kind session.Kind, role session.Role, content string) {
	a.appendEntry(ctx, domainagent.Entry{Kind: kind, Role: role, Content: content})
}

// appendEntry 追加一条完整上下文条目（可携带原生工具调用配对信息），并同步落盘。
//
// 落盘只保留文本：会话表是按「一问一答的可读记录」设计的，没有工具调用字段。
// 原生工具调用的消息正文往往为空，此时把调用渲染成 !TOOL: 文本再落盘，
// 历史记录才不会出现空行；重启恢复后这些条目退化为普通文本消息，
// 由 NormalizeToolPairing 保证不会变成非法的孤儿 tool 消息。
func (a *Agent) appendEntry(ctx context.Context, entry domainagent.Entry) {
	a.memory.AddEntry(entry)
	if a.sessStore == nil {
		return
	}
	content := entry.Content
	if content == "" && len(entry.ToolCalls) > 0 {
		content = domainagent.RenderToolCalls(entry.ToolCalls)
	}
	if content == "" {
		return
	}
	message := session.Message{
		Kind:    entry.Kind,
		Role:    entry.Role,
		Content: session.Sanitize(content, a.maxMessageBytes),
	}
	if err := a.sessStore.Append(ctx, a.sessionID, message); err != nil {
		fmt.Printf("[会话落盘失败]:%v\n", err)
	}
}

// restoreHistory 从会话库恢复当前分支的历史到上下文。
func (a *Agent) restoreHistory(ctx context.Context) error {
	a.compaction.reset()
	if a.sessStore == nil {
		return nil
	}
	messages, err := a.sessStore.LoadBranch(ctx, a.sessionID, a.maxHistoryMessages)
	if err != nil {
		return err
	}
	trimmed := session.TrimHistory(messages, a.maxHistoryRunes)
	a.memory.Replace(domainagent.ToEntries(trimmed))
	if len(trimmed) > 0 {
		fmt.Printf("[已恢复会话 %s 的 %d 条历史消息]\n", a.sessionID, len(trimmed))
	}
	return nil
}
