package agent

import "testing"

// TestEstimateTokens 校验启发式估算在各输入下的上界行为（兜底实现）。
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"空串", "", 0},
		{"纯英文", "hello world", 3}, // 11 字符 / 4
		{"空格", "a", 1},            // 1 字符
		{"中文", "你好，世界！", 6},       // 6 宽字符
		{"中文句子", "深度学习与大模型", 8},   // 8 宽字符
		{"中英混排", "Go中文01", 3},     // 2 宽 + 4 拉丁 = 2+1
		{"尾随空格", "abc ", 1},       // 4 字符 / 4
		{"长拉丁", "aaaaaaaaaa", 3},  // 10/4=2.5 → 3
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EstimateTokens(tc.in); got != tc.want {
				t.Fatalf("EstimateTokens(%q) = %d, 期望 %d", tc.in, got, tc.want)
			}
		})
	}
}
