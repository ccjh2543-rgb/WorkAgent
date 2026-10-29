package session

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate 截断必须补齐标记并按 rune 边界对齐
func TestTruncate(t *testing.T) {
	t.Run("未超限时原样返回", func(t *testing.T) {
		if got := Truncate("短文本", 100); got != "短文本" {
			t.Fatalf("未超限不应改动: %q", got)
		}
	})

	t.Run("超限时截断并加标记", func(t *testing.T) {
		got := Truncate(strings.Repeat("a", 100), 10)
		if !strings.HasPrefix(got, strings.Repeat("a", 10)) {
			t.Fatalf("前缀应保留 10 个字符: %q", got)
		}
		if !strings.HasSuffix(got, truncatedSuffix) {
			t.Fatalf("应带上截断标记: %q", got)
		}
	})

	t.Run("多字节字符不被切成半个", func(t *testing.T) {
		// 每个汉字 3 字节，按 10 字节截断会落在第 4 个字中间
		got := Truncate(strings.Repeat("中", 10), 10)
		if !utf8.ValidString(got) {
			t.Fatalf("截断后必须仍是合法 UTF-8: %q", got)
		}
		if !strings.HasPrefix(got, "中中中") {
			t.Fatalf("应保留 3 个完整汉字: %q", got)
		}
	})

	t.Run("上限为 0 表示不限制", func(t *testing.T) {
		long := strings.Repeat("a", 100)
		if got := Truncate(long, 0); got != long {
			t.Fatal("上限为 0 时不应截断")
		}
	})
}

// TestSanitize 落盘前必须同时完成脱敏与截断
func TestSanitize(t *testing.T) {
	t.Run("脱敏优先于截断", func(t *testing.T) {
		got := Sanitize("api_key=abcdef12345678", 0)
		if strings.Contains(got, "abcdef12345678") {
			t.Fatalf("敏感信息未被掩掉: %q", got)
		}
	})

	t.Run("同时施加长度上限", func(t *testing.T) {
		got := Sanitize(strings.Repeat("a", 100), 10)
		if !strings.HasSuffix(got, truncatedSuffix) {
			t.Fatalf("应被截断: %q", got)
		}
	})

	t.Run("上限非正数时使用默认值", func(t *testing.T) {
		got := Sanitize("普通文本", -1)
		if got != "普通文本" {
			t.Fatalf("短文本不应被改动: %q", got)
		}
	})
}

// TestPreview 摘要必须压成单行并按字符数截断
func TestPreview(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		maxRunes int
		want     string
	}{
		{"短文本原样", "一行短文本", 10, "一行短文本"},
		{"换行被压成空格", "第一行\n第二行", 20, "第一行 第二行"},
		{"连续空白被折叠", "有   很多    空白", 20, "有 很多 空白"},
		{"超长被截断并加省略号", "一二三四五六", 3, "一二三…"},
		{"上限非正数时用默认值", "短文本", 0, "短文本"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Preview(tt.text, tt.maxRunes); got != tt.want {
				t.Fatalf("摘要不匹配: got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKindRollbackable 只有用户真实提问能作为回滚点
func TestKindRollbackable(t *testing.T) {
	tests := []struct {
		kind Kind
		want bool
	}{
		{KindPrompt, true},
		{KindAnswer, false},
		{KindToolCall, false},
		{KindToolResult, false},
		{KindGuidance, false},
		{Kind("未知种类"), false},
	}

	for _, tt := range tests {
		if got := tt.kind.Rollbackable(); got != tt.want {
			t.Fatalf("Kind(%q).Rollbackable() = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// TestTrimHistory 历史裁剪必须从最旧的开始丢，且至少保留最后一条
func TestTrimHistory(t *testing.T) {
	messages := []Message{
		{ID: 1, Role: RoleUser, Content: strings.Repeat("a", 10)},
		{ID: 2, Role: RoleAssistant, Content: strings.Repeat("b", 20)},
		{ID: 3, Role: RoleUser, Content: strings.Repeat("c", 30)},
	}

	tests := []struct {
		name      string
		maxRunes  int
		wantCount int
		wantFirst int64 // 期望保留下来的第一条消息的 ID
	}{
		{"预算充足全部保留", 100, 3, 1},
		{"预算只够最近两条", 50, 2, 2},
		{"预算只够最近一条", 45, 1, 3},
		{"预算极小仍保留最后一条", 1, 1, 3},
		{"预算为 0 表示不裁剪", 0, 3, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TrimHistory(messages, tt.maxRunes)
			if len(got) != tt.wantCount {
				t.Fatalf("保留条数不匹配: got %d, want %d", len(got), tt.wantCount)
			}
			if got[0].ID != tt.wantFirst {
				t.Fatalf("应保留最近的消息: got 首条 ID=%d, want %d", got[0].ID, tt.wantFirst)
			}
		})
	}
}

// TestTrimHistoryEmpty 空历史不应 panic
func TestTrimHistoryEmpty(t *testing.T) {
	if got := TrimHistory(nil, 100); len(got) != 0 {
		t.Fatalf("空历史应返回空, got %+v", got)
	}
}
