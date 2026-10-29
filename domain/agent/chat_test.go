package agent

import (
	"testing"

	"simpleAgent/domain/session"
)

// TestNormalizeToolPairing 表驱动校验工具调用配对修正：
// 残缺的配对（压缩/裁剪把调用与结果切到两侧）必须被修成合法序列，
// 否则 OpenAI 兼容服务端会因「tool 消息找不到对应 tool_calls」拒绝整个请求。
func TestNormalizeToolPairing(t *testing.T) {
	assistantWith := func(content string, ids ...string) ChatMessage {
		calls := make([]ToolCall, 0, len(ids))
		for _, id := range ids {
			calls = append(calls, ToolCall{ID: id, Name: "read_file", Arg: `{"path":"a"}`})
		}
		return ChatMessage{Role: session.RoleAssistant, Content: content, ToolCalls: calls}
	}
	toolMsg := func(id, content string) ChatMessage {
		return ChatMessage{Role: session.RoleTool, Content: content, ToolCallID: id}
	}
	userMsg := func(content string) ChatMessage {
		return ChatMessage{Role: session.RoleUser, Content: content}
	}

	cases := []struct {
		name string
		in   []ChatMessage
		want []ChatMessage
	}{
		{
			name: "无工具调用时原样返回",
			in:   []ChatMessage{userMsg("你好"), {Role: session.RoleAssistant, Content: "在的"}},
			want: []ChatMessage{userMsg("你好"), {Role: session.RoleAssistant, Content: "在的"}},
		},
		{
			name: "配对完整时保持原样",
			in:   []ChatMessage{assistantWith("", "c1"), toolMsg("c1", "内容")},
			want: []ChatMessage{assistantWith("", "c1"), toolMsg("c1", "内容")},
		},
		{
			name: "结果缺失的调用被摘掉（连同空正文消息）",
			in:   []ChatMessage{assistantWith("", "c1"), userMsg("下一句")},
			want: []ChatMessage{userMsg("下一句")},
		},
		{
			name: "孤儿工具结果降级为普通用户消息",
			in:   []ChatMessage{toolMsg("gone", "旧结果")},
			want: []ChatMessage{userMsg("旧结果")},
		},
		{
			name: "空 ID 的工具结果同样降级（从持久化历史恢复的形态）",
			in:   []ChatMessage{{Role: session.RoleTool, Content: "旧结果"}},
			want: []ChatMessage{userMsg("旧结果")},
		},
		{
			name: "批量调用里只保留有结果的那些",
			in:   []ChatMessage{assistantWith("说明", "c1", "c2"), toolMsg("c2", "结果2")},
			want: []ChatMessage{assistantWith("说明", "c2"), toolMsg("c2", "结果2")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeToolPairing(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("消息条数 = %d，期望 %d（got=%+v）", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i].Role != tc.want[i].Role || got[i].Content != tc.want[i].Content ||
					got[i].ToolCallID != tc.want[i].ToolCallID {
					t.Fatalf("第 %d 条消息 = %+v，期望 %+v", i, got[i], tc.want[i])
				}
				if len(got[i].ToolCalls) != len(tc.want[i].ToolCalls) {
					t.Fatalf("第 %d 条消息的调用数 = %d，期望 %d", i, len(got[i].ToolCalls), len(tc.want[i].ToolCalls))
				}
				for j := range tc.want[i].ToolCalls {
					if got[i].ToolCalls[j].ID != tc.want[i].ToolCalls[j].ID {
						t.Fatalf("第 %d 条消息第 %d 个调用 ID = %q，期望 %q",
							i, j, got[i].ToolCalls[j].ID, tc.want[i].ToolCalls[j].ID)
					}
				}
			}
		})
	}
}

// TestRenderToolCallsRoundTrip 校验原生调用渲染成文本后可被文本协议解析回同样的工具名与参数。
func TestRenderToolCallsRoundTrip(t *testing.T) {
	calls := []ToolCall{
		{ID: "c1", Name: "read_file", Arg: `{"path":"a.go"}`},
		{ID: "c2", Name: "write_file", Arg: `{"path":"b.go","content":"x"}`},
	}
	rendered := RenderToolCalls(calls)
	parsed := ParseToolCalls(rendered)
	if len(parsed) != len(calls) {
		t.Fatalf("解析出 %d 个调用，期望 %d：%q", len(parsed), len(calls), rendered)
	}
	for i, want := range calls {
		if parsed[i].Name != want.Name || parsed[i].Arg != want.Arg {
			t.Fatalf("第 %d 个调用 = %+v，期望 %+v", i, parsed[i], want)
		}
	}
}

// TestRenderToolCallsEmpty 校验空调用集渲染为空串（落盘时会据此跳过写库）。
func TestRenderToolCallsEmpty(t *testing.T) {
	if got := RenderToolCalls(nil); got != "" {
		t.Fatalf("RenderToolCalls(nil) = %q，期望空串", got)
	}
}
