package tools

import (
	"context"
	"strings"
	"testing"

	"simpleAgent/infrastructure/memory"
)

// stubSearcher 记录调用参数并返回固定结果，用于验证工具的入参处理与渲染。
type stubSearcher struct {
	fragments []memory.Fragment
	err       error
	gotQuery  string
	gotTopK   int
}

func (s *stubSearcher) Search(_ context.Context, query string, topK int) ([]memory.Fragment, error) {
	s.gotQuery = query
	s.gotTopK = topK
	return s.fragments, s.err
}

// TestMemorySearchToolRun 验证检索结果按「章节/类型」渲染。
func TestMemorySearchToolRun(t *testing.T) {
	searcher := &stubSearcher{fragments: []memory.Fragment{
		{Section: "架构约定", Kind: memory.KindDecision, Content: "严格 DDD 分层"},
		{Section: "沟通偏好", Kind: memory.KindPreference, Content: "优先中文输出"},
	}}
	tool := NewMemorySearchTool(searcher)

	out, err := tool.Run(`{"query":"分层","limit":2}`)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if searcher.gotQuery != "分层" || searcher.gotTopK != 2 {
		t.Fatalf("检索参数未透传: query=%q topK=%d", searcher.gotQuery, searcher.gotTopK)
	}
	for _, want := range []string{"找到 2 条相关记忆", "- [架构约定/decision] 严格 DDD 分层", "- [沟通偏好/preference] 优先中文输出"} {
		if !strings.Contains(out, want) {
			t.Fatalf("输出缺少 %q，实际:\n%s", want, out)
		}
	}
}

// TestMemorySearchToolDefaultsAndClamp 验证条数默认值与上限。
func TestMemorySearchToolDefaultsAndClamp(t *testing.T) {
	searcher := &stubSearcher{}
	tool := NewMemorySearchTool(searcher)

	if _, err := tool.Run(`{"query":"约定"}`); err != nil {
		t.Fatalf("默认条数检索失败: %v", err)
	}
	if searcher.gotTopK != defaultMemorySearchLimit {
		t.Fatalf("默认条数应为 %d，实际 %d", defaultMemorySearchLimit, searcher.gotTopK)
	}

	if _, err := tool.Run(`{"query":"约定","limit":999}`); err != nil {
		t.Fatalf("超限条数检索失败: %v", err)
	}
	if searcher.gotTopK != maxMemorySearchLimit {
		t.Fatalf("条数应被限制到 %d，实际 %d", maxMemorySearchLimit, searcher.gotTopK)
	}
}

// TestMemorySearchToolEmptyQuery 验证空查询被拒绝。
func TestMemorySearchToolEmptyQuery(t *testing.T) {
	tool := NewMemorySearchTool(&stubSearcher{})
	if _, err := tool.Run(`{"query":"   "}`); err == nil {
		t.Fatal("空查询应返回错误")
	}
}

// TestMemorySearchToolNoResult 验证无命中时的稳定文案。
func TestMemorySearchToolNoResult(t *testing.T) {
	tool := NewMemorySearchTool(&stubSearcher{})
	out, err := tool.Run(`{"query":"不存在"}`)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if out != "没有找到相关记忆。" {
		t.Fatalf("无命中文案不符: %q", out)
	}
}
