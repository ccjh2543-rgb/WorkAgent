package tokenizer

import (
	"context"
	"testing"
)

// 合成的最小 ByteLevel BPE：字节符号 a/b/c/空格 + 两级合并 ab、abc。
const miniVocab = `{"model":{"vocab":{"a":10,"b":11,"c":12," ":13,"ab":14,"abc":15},"merges":["a b","ab c"]}}`

func mustMiniTokenizer(t *testing.T) *DeepSeekTokenizer {
	t.Helper()
	tk, err := NewDeepSeekTokenizer([]byte(miniVocab))
	if err != nil {
		t.Fatalf("NewDeepSeekTokenizer: %v", err)
	}
	return tk
}

func TestCount_BasicMerge(t *testing.T) {
	tk := mustMiniTokenizer(t)
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"b", 1},
		{"ab", 1},
		{"abc", 1}, // a b -> ab，ab c -> abc
		{"abcabc", 2},
	}
	for _, tc := range cases {
		got, err := tk.Count(context.Background(), tc.in)
		if err != nil {
			t.Fatalf("Count(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("Count(%q) = %d, 期望 %d", tc.in, got, tc.want)
		}
	}
}

// TestCount_Concurrent 校验单一实例并发计数不 panic（内部 map 只读）。
func TestCount_Concurrent(t *testing.T) {
	tk := mustMiniTokenizer(t)
	ctx := context.Background()
	const n = 64
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := tk.Count(ctx, "abc abc"); err != nil {
				t.Errorf("Count: %v", err)
			}
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
