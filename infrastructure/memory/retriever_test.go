package memory

import (
	"context"
	"testing"
)

// tokensEqual 比较两个词元切片是否完全一致
func tokensEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTokenize 覆盖中英文混排的分词行为
func TestTokenize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"纯英文按词切分", "DDD domain layer", []string{"ddd", "domain", "layer"}},
		{"中文按二元字组切分", "架构约定", []string{"架构", "构约", "约定"}},
		{"单个汉字保留", "层", []string{"层"}},
		{"中英混排", "DDD分层", []string{"ddd", "分层"}},
		{"单字母噪音被丢弃", "a b c", []string{}},
		{"标点作为分隔符", "MEMORY.md 读写", []string{"memory", "md", "读写"}},
		{"空字符串", "", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Tokenize(tt.input); !tokensEqual(got, tt.want) {
				t.Fatalf("分词结果不匹配: got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatchQuery 验证 FTS5 查询串的构造，词元必须被引号包裹以避免语法错误
func TestMatchQuery(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"中文二元字组", "架构", `"架构"`},
		{"多个词元用 OR 连接", "架构约定", `"架构" OR "构约" OR "约定"`},
		{"英文小写化", "DDD", `"ddd"`},
		{"无可用词元返回空串", "a b", ""},
		{"空串返回空串", "", ""},
		{"标点被剥离", `"架构约定"`, `"架构" OR "构约" OR "约定"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MatchQuery(tt.input); got != tt.want {
				t.Fatalf("查询串不匹配: got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKeywordRetrieverRank 覆盖检索排序、topK 截断与无命中回退
func TestKeywordRetrieverRank(t *testing.T) {
	ctx := context.Background()
	retriever := KeywordRetriever{}

	candidates := []Fragment{
		{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格分层，不依赖外部包"},
		{Scope: ScopeWorkspace, Section: "其他杂项", Content: "架构上采用三层结构"},
		{Scope: ScopeWorkspace, Section: "沟通偏好", Content: "优先中文输出"},
	}

	t.Run("标题命中权重高于正文命中", func(t *testing.T) {
		got, err := retriever.Rank(ctx, "架构约定", candidates, 3)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("期望命中 2 条, got %d", len(got))
		}
		if got[0].Section != "架构约定" {
			t.Fatalf("标题命中的记忆应排在首位, got %q", got[0].Section)
		}
	})

	t.Run("topK 截断", func(t *testing.T) {
		got, err := retriever.Rank(ctx, "架构约定", candidates, 1)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("topK=1 应只返回 1 条, got %d", len(got))
		}
	})

	t.Run("无命中时回退为文件顺序", func(t *testing.T) {
		got, err := retriever.Rank(ctx, "完全不相关的词 qwerty", candidates, 2)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("期望回退返回 2 条, got %d", len(got))
		}
		if got[0] != candidates[0] || got[1] != candidates[1] {
			t.Fatalf("回退时应保持文件顺序, got %+v", got)
		}
	})

	t.Run("返回值与底层切片解耦", func(t *testing.T) {
		got, err := retriever.Rank(ctx, "架构约定", candidates, 3)
		if err != nil {
			t.Fatalf("检索失败: %v", err)
		}
		got[0].Content = "被调用方篡改"
		if candidates[0].Content == "被调用方篡改" {
			t.Fatal("检索结果不应与原始候选切片共享底层数组")
		}
	})
}

// TestKeywordRetrieverRankEdgeCases 非法入参不应 panic
func TestKeywordRetrieverRankEdgeCases(t *testing.T) {
	ctx := context.Background()
	retriever := KeywordRetriever{}

	got, err := retriever.Rank(ctx, "架构", nil, 3)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if got != nil {
		t.Fatalf("空候选应返回 nil, got %+v", got)
	}

	got, err = retriever.Rank(ctx, "架构", []Fragment{{Content: "架构约定"}}, 0)
	if err != nil {
		t.Fatalf("检索失败: %v", err)
	}
	if got != nil {
		t.Fatalf("topK 为 0 应返回 nil, got %+v", got)
	}
}

// TestKeywordRetrieverCancelledContext 已取消的 ctx 必须显式返回错误
func TestKeywordRetrieverCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (KeywordRetriever{}).Rank(ctx, "架构", []Fragment{{Content: "架构约定"}}, 3); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
}
