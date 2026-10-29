package bench

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseVerifierTOML 覆盖必填命令、默认 cwd 与非法 cwd。
func TestParseVerifierTOML(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantCwd string
		wantErr bool
	}{
		{
			name: "正常配置",
			content: `[run]
cwd = "/workspace"
command = '''PYTHONPATH="/workspace" python -m pytest /tests/grading'''
[env]
WB_BENCH_GOLD_PATH = "/tests/gold/gold_answer.json"
`,
			wantCwd: "/workspace",
		},
		{
			name: "缺省 cwd",
			content: `[run]
command = "python -m pytest"`,
			wantCwd: "/workspace",
		},
		{
			name: "缺少 command",
			content: `[run]
cwd = "/workspace"`,
			wantErr: true,
		},
		{
			name: "cwd 非绝对路径",
			content: `[run]
cwd = "workspace"
command = "python -m pytest"`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "verifier.toml")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatalf("写入失败: %v", err)
			}
			spec, err := ParseVerifierTOML(path)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr && spec.Cwd != c.wantCwd {
				t.Fatalf("cwd=%s, want %s", spec.Cwd, c.wantCwd)
			}
		})
	}
}

// TestParseJudgeYAML 校验产物契约解析与 required 默认值。
func TestParseJudgeYAML(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantCount int
		wantErr   bool
		wantReq   bool
	}{
		{
			name: "默认 required 为 true",
			content: `version: 1
artifacts:
  - id: a
    path: output/a.md
    type: md
`,
			wantCount: 1,
			wantReq:   true,
		},
		{
			name: "显式非必填",
			content: `artifacts:
  - id: a
    path: output/a.md
    required: false
`,
			wantCount: 1,
			wantReq:   false,
		},
		{
			name:    "路径越界",
			content: "artifacts:\n  - id: a\n    path: ../evil.md\n",
			wantErr: true,
		},
		{
			name:    "没有产物",
			content: "version: 1\n",
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "judge.yaml")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatalf("写入失败: %v", err)
			}
			arts, err := ParseJudgeYAML(path)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if len(arts) != c.wantCount || arts[0].Required != c.wantReq {
				t.Fatalf("产物=%+v, want count=%d required=%v", arts, c.wantCount, c.wantReq)
			}
		})
	}
}
