package eval

import (
	"os"
	"path/filepath"
	"testing"

	domaineval "simpleAgent/domain/eval"
)

// TestCollectArtifacts 表驱动覆盖声明缺失、声明有效、日志根回退与快照 diff。
func TestCollectArtifacts(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	verifierLogs := filepath.Join(root, "logs", "verifier")
	mustWrite := func(dir, rel, content string) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("创建目录失败: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	mustWrite(workspace, "output/result.json", `{"ok":true}`)
	mustWrite(workspace, "output/notes.md", "补充说明")
	mustWrite(workspace, "final_answer.md", "答案")
	mustWrite(workspace, "input/data.csv", "原始输入")
	mustWrite(verifierLogs, "artifact_text/final_answer.md", "证据")

	declared := []DeclaredArtifact{
		{ID: "result", Path: "output/result.json", Type: "json", Required: true},
		{ID: "evidence", Path: "artifact_text/final_answer.md", Type: "md", Required: true},
		{ID: "missing", Path: "output/missing.xlsx", Type: "xlsx", Required: true},
		{ID: "optional", Path: "output/optional.md", Type: "md", Required: false},
	}
	before := map[string]FileMeta{"input/data.csv": {Size: 12, Hash: "h1"}}
	after := map[string]FileMeta{
		"input/data.csv":     {Size: 12, Hash: "h1"},
		"output/result.json": {Size: 11, Hash: "h2"},
		"output/notes.md":    {Size: 12, Hash: "h4"},
		"final_answer.md":    {Size: 6, Hash: "h3"},
	}

	arts, failures := CollectArtifacts(workspace, verifierLogs, declared, before, after, nil)

	byID := map[string]struct {
		root  string
		valid bool
	}{}
	for _, a := range arts {
		byID[a.DeclaredID] = struct {
			root  string
			valid bool
		}{a.Root, a.Valid}
	}
	cases := []struct {
		id        string
		wantRoot  string
		wantValid bool
	}{
		{"result", "workspace", true},
		{"evidence", "verifier_logs", true},
		{"missing", "workspace", false},
		{"optional", "workspace", false},
	}
	for _, c := range cases {
		got, ok := byID[c.id]
		if !ok {
			t.Fatalf("缺少产物 %s", c.id)
		}
		if got.root != c.wantRoot || got.valid != c.wantValid {
			t.Fatalf("产物 %s root=%s valid=%v, want %s/%v", c.id, got.root, got.valid, c.wantRoot, c.wantValid)
		}
	}

	// 未声明的快照新增文件应被采集，输入文件与 final_answer.md 应被排除。
	var diffPaths []string
	for _, a := range arts {
		if a.Source == "workspace_diff" {
			diffPaths = append(diffPaths, a.Path)
		}
	}
	if len(diffPaths) != 1 || diffPaths[0] != "output/notes.md" {
		t.Fatalf("快照 diff 采集异常: %v", diffPaths)
	}

	// required 缺失必须产生 artifact_missing，可选产物缺失不应报错。
	var missingCount int
	for _, f := range failures {
		if f.Code == domaineval.FailArtifactMissing {
			missingCount++
		}
	}
	if missingCount != 1 {
		t.Fatalf("artifact_missing 数量=%d, want 1, failures=%+v", missingCount, failures)
	}
}

// TestCollectArtifactsNoDeclared 没有声明产物时也不应崩溃。
func TestCollectArtifactsNoDeclared(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "output"), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "output", "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	arts, failures := CollectArtifacts(workspace, "", nil, nil, map[string]FileMeta{"output/a.md": {Size: 1, Hash: "h"}}, nil)
	if len(arts) != 1 || len(failures) != 0 {
		t.Fatalf("产物=%d 失败=%d, want 1/0", len(arts), len(failures))
	}
}
