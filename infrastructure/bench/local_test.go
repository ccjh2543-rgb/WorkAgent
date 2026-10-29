package bench

import (
	"path/filepath"
	"strings"
	"testing"

	appeval "simpleAgent/application/eval"
)

// TestSplitShell 覆盖引号、续行与重定向符号。
func TestSplitShell(t *testing.T) {
	got, ok := splitShell("PYTHONPATH=\"/workspace:/tests/grading\" python -m pytest \\\n /tests/grading > /logs/verifier/a.txt 2>&1")
	if !ok {
		t.Fatalf("分词失败")
	}
	want := []string{"PYTHONPATH=/workspace:/tests/grading", "python", "-m", "pytest", "/tests/grading", ">", "/logs/verifier/a.txt", "2>&1"}
	if len(got) != len(want) {
		t.Fatalf("tokens=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token[%d]=%q, want %q", i, got[i], want[i])
		}
	}
	if _, ok := splitShell(`python -m pytest "unclosed`); ok {
		t.Fatalf("未闭合引号应返回失败")
	}
}

// TestBuildVerifierCommand 校验容器路径到本地路径的翻译与 PYTHONPATH 重写。
func TestBuildVerifierCommand(t *testing.T) {
	base := t.TempDir()
	env := appeval.Environment{
		Workspace: filepath.Join(base, "workspace"),
		Tests:     filepath.Join(base, "tests"),
		Logs:      filepath.Join(base, "logs"),
	}
	spec := appeval.VerifierSpec{
		Cwd: "/workspace",
		Command: `PYTHONPATH="/workspace:/tests/grading" python -m pytest /tests/grading \
  --junitxml=/logs/verifier/results.xml > /logs/verifier/test_output.txt 2>&1`,
		Env: map[string]string{
			"WB_BENCH_GOLD_PATH":    "/tests/gold/gold_answer.json",
			"WB_BENCH_FIXTURES_DIR": "/tests/gold/fixtures",
		},
	}
	argv, junit, envList, err := NewLocalExecutor().buildVerifierCommand(env, spec)
	if err != nil {
		t.Fatalf("构建命令失败: %v", err)
	}
	if argv[0] != "python" || argv[1] != "-m" || argv[2] != "pytest" {
		t.Fatalf("argv 异常: %v", argv)
	}
	if !strings.HasPrefix(junit, env.Logs) || !strings.HasSuffix(junit, "results.xml") {
		t.Fatalf("junit 路径异常: %s", junit)
	}
	joined := strings.Join(envList, "\n")
	if !strings.Contains(joined, "WB_BENCH_GOLD_PATH="+filepath.ToSlash(filepath.Join(env.Tests, "gold", "gold_answer.json"))) {
		t.Fatalf("gold 路径未翻译:\n%s", joined)
	}
	if !strings.Contains(joined, "PYTHONPATH=") {
		t.Fatalf("缺少 PYTHONPATH:\n%s", joined)
	}
	// Windows 下 PYTHONPATH 必须以分号分隔，且不能把盘符冒号拆开。
	sep := string(filepath.ListSeparator)
	if sep == ";" && !strings.Contains(joined, "PYTHONPATH="+filepath.Join(env.Workspace)+";") {
		t.Fatalf("PYTHONPATH 分隔符异常:\n%s", joined)
	}
}

// TestTranslatePythonPath 校验占位符与容器前缀翻译。
func TestTranslatePythonPath(t *testing.T) {
	env := appeval.Environment{Workspace: "/local/ws", Tests: "/local/tests", Logs: "/local/logs"}
	got := translatePythonPath(env, "/workspace:/tests/grading:${PYTHONPATH:-}")
	parts := strings.Split(got, string(filepath.ListSeparator))
	if len(parts) < 2 {
		t.Fatalf("翻译结果异常: %s", got)
	}
	if filepath.ToSlash(parts[0]) != "/local/ws" {
		t.Fatalf("工作区未翻译: %s", got)
	}
}
