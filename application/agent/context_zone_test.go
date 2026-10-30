package agent

import (
	"context"
	"strings"
	"testing"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// zoneStubModel 最小 ChatModel 桩：只返回固定文本，不产生工具调用。
type zoneStubModel struct {
	reply string
}

func (m zoneStubModel) Generate(context.Context, domainagent.ChatRequest) (domainagent.ChatResponse, error) {
	return domainagent.ChatResponse{Content: m.reply}, nil
}

// newZoneTestAgent 在临时工作区装配 Agent，避免污染仓库内的状态目录。
func newZoneTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := NewAgentWithOptions(zoneStubModel{reply: "ok"}, Options{WorkspaceRoot: t.TempDir()})
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("关闭 Agent 失败: %v", err)
		}
	})
	return a
}

// TestPrepareContextPutsTaskAnchorAtTail 验证任务锚点位于消息序列末尾。
// 这是前缀缓存的前提：锚点每轮变化，放在前面会让整段历史失效。
func TestPrepareContextPutsTaskAnchorAtTail(t *testing.T) {
	a := newZoneTestAgent(t)
	a.SetContextBudget(200000)
	a.planner.Replace("整理报价单\n导出 xlsx")
	a.memory.Add(session.KindPrompt, session.RoleUser, "把报价单整理成 xlsx")
	a.memory.Add(session.KindToolResult, session.RoleUser, strings.Repeat("明细行 ", 200))

	msgs, err := a.prepareContext(context.Background())
	if err != nil {
		t.Fatalf("装配上下文失败: %v", err)
	}
	if len(msgs) < 3 {
		t.Fatalf("消息条数过少: %d", len(msgs))
	}
	if msgs[0].Content != a.systemPrompt {
		t.Fatalf("首条消息应为系统提示词")
	}
	last := msgs[len(msgs)-1]
	// 系统提示词里也会提到 <task_state> 标签名，因此用锚点正文特征判断
	if !strings.Contains(last.Content, "以下是当前任务进度") {
		t.Fatalf("末条消息应为任务锚点，实际: %q", last.Content)
	}
	for i := 0; i < len(msgs)-1; i++ {
		if strings.Contains(msgs[i].Content, "以下是当前任务进度") {
			t.Fatalf("任务锚点不应出现在第 %d 条消息", i)
		}
	}
}

// TestPrepareContextStablePrefix 验证连续两轮的稳定前缀逐字节一致。
func TestPrepareContextStablePrefix(t *testing.T) {
	a := newZoneTestAgent(t)
	a.SetContextBudget(200000)
	a.memBlock = "<memory>\n- [偏好] 优先中文输出\n</memory>"
	a.memory.Add(session.KindPrompt, session.RoleUser, "读取报价单")

	first, err := a.prepareContext(context.Background())
	if err != nil {
		t.Fatalf("首轮装配失败: %v", err)
	}
	a.memory.Add(session.KindToolResult, session.RoleUser, "工具输出")
	second, err := a.prepareContext(context.Background())
	if err != nil {
		t.Fatalf("次轮装配失败: %v", err)
	}

	stable := 2 // 系统提示词 + 记忆块
	if len(first) < stable || len(second) < stable {
		t.Fatalf("消息条数不足以比较稳定前缀")
	}
	for i := 0; i < stable; i++ {
		if first[i].Role != second[i].Role || first[i].Content != second[i].Content {
			t.Fatalf("稳定前缀第 %d 条发生变化", i)
		}
	}
}

// TestSetContextBudgetClamps 验证预算覆盖的边界处理。
func TestSetContextBudgetClamps(t *testing.T) {
	a := newZoneTestAgent(t)

	a.SetContextBudget(0)
	if a.contextBudget != defaultContextBudget {
		t.Fatalf("非正数应恢复默认预算，实际 %d", a.contextBudget)
	}
	a.SetContextBudget(1)
	if a.contextBudget != minContextBudget {
		t.Fatalf("过小预算应提升到下限 %d，实际 %d", minContextBudget, a.contextBudget)
	}
	a.SetContextBudget(64000)
	if a.contextBudget != 64000 {
		t.Fatalf("正常预算应原样生效，实际 %d", a.contextBudget)
	}
}

// TestContextReportZonesSumToTotal 验证分区用量之和等于总量，且工具占比在 0-1 之间。
func TestContextReportZonesSumToTotal(t *testing.T) {
	a := newZoneTestAgent(t)
	a.SetContextBudget(200000)
	a.memBlock = "<memory>\n- [架构] 严格 DDD 分层\n</memory>"
	a.planner.Replace("步骤一\n步骤二")
	a.memory.Add(session.KindPrompt, session.RoleUser, "整理数据")
	a.memory.Add(session.KindToolResult, session.RoleUser, strings.Repeat("数据 ", 50))

	if _, err := a.prepareContext(context.Background()); err != nil {
		t.Fatalf("装配上下文失败: %v", err)
	}
	report := a.ContextSnapshot()
	if report == nil {
		t.Fatal("应生成上下文报告")
	}
	sum := report.PolicyTokens + report.MemoryTokens + report.AnchorTokens +
		report.SummaryTokens + report.WindowTokens
	if sum != report.TotalTokens {
		t.Fatalf("分区之和 %d 与总量 %d 不一致", sum, report.TotalTokens)
	}
	if report.AnchorTokens == 0 {
		t.Fatal("存在计划时任务锚点不应为 0")
	}
	if report.MemoryTokens == 0 {
		t.Fatal("存在记忆块时记忆分区不应为 0")
	}
	if report.ToolOutputShare <= 0 || report.ToolOutputShare > 1 {
		t.Fatalf("工具输出占比应在 (0,1]，实际 %f", report.ToolOutputShare)
	}
}
