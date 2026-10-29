package agent

import (
	"context"
	"strings"
	"testing"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// fakePWModel 脚本化 ChatModel：区分 决策/汇总/worker 三种提示，并记录每次调用收到的消息，
// 供测试断言 Worker 上下文隔离（一个 worker 看不到其它 worker 的指令与全局轨迹）。
type fakePWModel struct {
	seen        []string // 每次 Generate 收到的全量内容拼接（便于断言）
	seenRaw     [][]domainagent.ChatMessage
	decideCalls int // 统计决策调用次数，驱动「run, run, final」
}

func (f *fakePWModel) Generate(_ context.Context, req domainagent.ChatRequest) (domainagent.ChatResponse, error) {
	f.seenRaw = append(f.seenRaw, cloneMessages(req.Messages))
	all := strings.Join(f.contentAll(req.Messages), "\n")
	f.seen = append(f.seen, all)
	// 决策阶段（条件驱动循环）：先给两个 task，再 final
	for _, m := range req.Messages {
		if m.Role == session.RoleSystem && strings.Contains(m.Content, "任务规划-决策者") {
			f.decideCalls++
			switch f.decideCalls {
			case 1:
				return text(`{"action":"run","task":"计算 2 加 3","reason":"第一步"}`), nil
			case 2:
				return text(`{"action":"run","task":"计算 4 加 5","reason":"第二步"}`), nil
			default:
				return text(`{"action":"final","task":"","reason":"完成"}`), nil
			}
		}
	}
	// 汇总阶段
	if strings.Contains(all, "请综合下面各子任务的结果") {
		return text("最终：任务完成"), nil
	}
	// worker 阶段：先出工具调用，收到 user 角色的 <tool_output>（注入的观察）后再给摘要。
	// 仅按 user 消息判定，避免 system 提示词里自带的 <tool_output> 字样误触发。
	hasTool := false
	for _, m := range req.Messages {
		if m.Role == session.RoleUser && strings.Contains(m.Content, "<tool_output") {
			hasTool = true
		}
	}
	if !hasTool {
		return text("!TOOL:calc|2,3"), nil
	}
	return text("子任务完成摘要"), nil
}

// text 把纯文本包装成模型回答（本假模型不产生原生工具调用）。
func text(content string) domainagent.ChatResponse {
	return domainagent.ChatResponse{Content: content}
}

// contentAll 拼接所有消息正文，便于断言。
func (f *fakePWModel) contentAll(msgs []domainagent.ChatMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

// cloneMessages 深拷贝消息切片，避免后续被 Agent 侧修改。
func cloneMessages(msgs []domainagent.ChatMessage) []domainagent.ChatMessage {
	out := make([]domainagent.ChatMessage, len(msgs))
	copy(out, msgs)
	return out
}

// TestPlannerWorkerWorkbenchResumable 驱动可恢复的 Planner-Worker 桥接：
// 验证 start→tool 暂停→多次 continue→final 的完整闭环，并断言 Worker 上下文隔离。
func TestPlannerWorkerWorkbenchResumable(t *testing.T) {
	fake := &fakePWModel{}
	a := NewAgent(fake)
	defer a.Close()

	ctx := context.Background()
	sid, res, err := a.StartPlannerWorker(ctx,
		"请拆成子任务并计算",
		[]string{"calc - 一个计算器"},
		"2023-12-01 09:00:00", 10)
	if err != nil {
		t.Fatalf("StartPlannerWorker 失败: %v", err)
	}
	if res.State != "tool" {
		t.Fatalf("首轮应暂停于工具，got state=%q", res.State)
	}
	// 依次注入观察直到 final
	for res.State == "tool" {
		res, err = a.ContinuePlannerWorker(ctx, sid, "5")
		if err != nil {
			t.Fatalf("ContinuePlannerWorker(sid=%s) 失败: %v", sid, err)
		}
	}
	if res.State != "final" {
		t.Fatalf("预期 final，got %q", res.State)
	}
	if !strings.Contains(res.Answer, "任务完成") {
		t.Fatalf("最终答案异常: %q", res.Answer)
	}
	if a.IsPlannerWorker(sid) {
		t.Fatalf("final 后运行器应已被清理")
	}

	// 隔离断言：worker 请求（含「当前子任务」的调用）里不应同时出现两个不同子任务的指令；
	// 汇总/决策阶段合法引用所有子任务结果，不在检查范围。
	for _, msgs := range fake.seenRaw {
		// 防回归：任何 system 消息的 content 不得为空（空 content 会被 deepseek 以 422 拒绝）
		for _, m := range msgs {
			if m.Role == session.RoleSystem && strings.TrimSpace(m.Content) == "" {
				t.Fatalf("出现空 system 消息 content")
			}
		}
		s := strings.Join(fake.contentAll(msgs), "|")
		if !strings.Contains(s, "当前子任务：") {
			continue
		}
		if strings.Contains(s, "计算 2 加 3") && strings.Contains(s, "计算 4 加 5") {
			t.Fatalf("出现上下文泄漏：同一 worker 请求同时含两个子任务指令: %s", s)
		}
	}
}
