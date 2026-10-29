package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveTokenizerPath 表驱动校验 tokenizer 资产路径解析：
// 显式配置优先；未配置时必须落到仓库内的真实文件，而不是依赖进程工作目录。
func TestResolveTokenizerPath(t *testing.T) {
	cases := []struct {
		name    string
		envPath string
		want    string // 期望的返回值；为空表示期望“仓库根 + 默认资产路径”
	}{
		{name: "显式配置优先", envPath: filepath.Join("tmp", "custom-tokenizer.json"), want: filepath.Join("tmp", "custom-tokenizer.json")},
		{name: "未配置时回退仓库根", envPath: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIMPLEAGENT_TOKENIZER_PATH", tc.envPath)
			got := resolveTokenizerPath()
			want := tc.want
			if want == "" {
				root := findRepoRoot()
				if root == "" {
					t.Skip("当前目录不在仓库内，跳过仓库根断言")
				}
				want = filepath.Join(root, tokenizerAssetPath)
				if _, err := os.Stat(got); err != nil {
					t.Fatalf("默认解析出的 tokenizer 路径 %s 不存在: %v", got, err)
				}
			}
			if got != want {
				t.Fatalf("resolveTokenizerPath() = %q，期望 %q", got, want)
			}
		})
	}
}

// TestFindRepoRoot 校验向上查找 go.mod 能找到当前仓库根，
// 且结果里确实存在 go.mod（避免返回任意父目录）。
func TestFindRepoRoot(t *testing.T) {
	root := findRepoRoot()
	if root == "" {
		t.Skip("当前目录不在仓库内")
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("仓库根 %s 下没有 go.mod: %v", root, err)
	}
}
