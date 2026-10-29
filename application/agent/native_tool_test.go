package agent

import (
	"context"
	"testing"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// fakeNativeModel 声明支持原生 tool calling 的假模型：
// 首轮返回一个原生工具调用，收到配对结果后返回最终答案。
type fakeNativeModel struct {
	requests  [][]domainagent.ChatMessage // 每次请求的消息快照
	withTools []int                       // 每次请求声明的工具数量
	round     int
}

func (f *fakeNativeModel) SupportsNativeTools() bool { return true }

func (f *fakeNativeModel) Generate(_ context.Context, req domainagent.ChatRequest) (domainagent.ChatResponse, error) {
	f.requests = append(f.requests, cloneMessages(req.Messages))
	f.withTools = append(f.withTools, len(req.Tools))
	f.round++
	if f.round == 1 {
		return domainagent.ChatResponse{
			ToolCalls: []domainagent.ToolCall{{ID: "call_1", Name: "calc", Arg: "2,3"}},
		}, nil
	}
	return domainagent.ChatResponse{Content: "结果是 5"}, nil
}

// TestStepConsumesNativeToolCall 校验原生 tool calling 全链路：
//   - 模型只给原生调用、正文为空时，不会被误判成最终答案；
//   - 工具结果按调用 ID 配对回注（assistant.tool_calls ↔ tool.tool_call_id）；
//   - 请求里确实带上了声明给模型的工具。
func TestStepConsumesNativeToolCall(t *testing.T) {
	fake := &fakeNativeModel{}
	a := NewAgentWithOptions(fake, Options{WorkspaceRoot: t.TempDir(), StateRoot: t.TempDir()})
	defer a.Close()

	answer, err := a.Run(context.Background(), "native-session", "算一下 2 加 3")
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if answer != "结果是 5" {
		t.Fatalf("最终答案 = %q，期望 %q（原生调用被误当成了最终答案？）", answer, "结果是 5")
	}
	if len(fake.requests) < 2 {
		t.Fatalf("模型被调用 %d 次，期望至少 2 次（调用轮 + 收尾轮）", len(fake.requests))
	}
	if fake.withTools[0] == 0 {
		t.Fatalf("首轮请求未声明任何工具，期望声明支持原生调用的工具")
	}

	// 第二轮请求里必须同时出现「带调用 ID 的 assistant 消息」与「同 ID 的 tool 结果」
	second := fake.requests[1]
	var callIDs, resultIDs []string
	for _, msg := range second {
		if msg.Role == session.RoleAssistant {
			for _, call := range msg.ToolCalls {
				callIDs = append(callIDs, call.ID)
			}
		}
		if msg.Role == session.RoleTool {
			resultIDs = append(resultIDs, msg.ToolCallID)
		}
	}
	if len(callIDs) != 1 || callIDs[0] != "call_1" {
		t.Fatalf("第二轮请求里的 assistant 调用 ID = %v，期望 [call_1]", callIDs)
	}
	if len(resultIDs) != 1 || resultIDs[0] != "call_1" {
		t.Fatalf("第二轮请求里的 tool 结果 ID = %v，期望 [call_1]", resultIDs)
	}
}
