package bench

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSanitizeImageName 校验任务 ID 到镜像名片段的转换规则。
func TestSanitizeImageName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通小写", "calendar-dida-sync-state", "calendar-dida-sync-state"},
		{"含下划线", "task_a", "task_a"},
		{"大写字母被替换", "Task-ABC", "ask"},
		{"非法字符替换", "task@1 /x", "task-1--x"},
		{"首尾非法字符裁剪", "--task--", "task"},
		{"全非法字符", "@@@", "task"},
		{"空串", "", "task"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeImageName(tc.in); got != tc.want {
				t.Fatalf("sanitizeImageName(%q)=%q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestContainerName 校验容器名唯一且稳定。
func TestContainerName(t *testing.T) {
	a := containerName("task-a", `F:\runs\run1\task-a\plan\workspace`)
	b := containerName("task-a", `F:\runs\run1\task-a\plan\workspace`)
	c := containerName("task-a", `F:\runs\run2\task-a\plan\workspace`)

	if a != b {
		t.Fatalf("相同工作区应得到相同容器名: %q vs %q", a, b)
	}
	if a == c {
		t.Fatalf("不同工作区应得到不同容器名: %q", a)
	}
	if !strings.HasPrefix(a, "sa-bench-task-a-") {
		t.Fatalf("容器名前缀不符合预期: %q", a)
	}
}

// TestPassthroughEnv 校验只透传白名单内的非空变量。
func TestPassthroughEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_MODEL", "deepseek-v4-flash")
	t.Setenv("SIMPLEAGENT_AUTH_TOKEN", "should-not-leak")

	got := passthroughEnv()
	want := []string{"OPENAI_API_KEY=sk-test", "OPENAI_MODEL=deepseek-v4-flash"}
	if len(got) != len(want) {
		t.Fatalf("透传变量数量=%d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项=%q，期望 %q", i, got[i], want[i])
		}
	}
	for _, kv := range got {
		if strings.Contains(kv, "should-not-leak") {
			t.Fatalf("白名单外的变量被透传: %s", kv)
		}
	}
}

// TestSortedKeys 校验 map 键按字典序输出，保证 docker 参数可复现。
func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]string{"b": "2", "a": "1", "c": "3"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("排序结果=%v，期望 a,b,c", got)
	}
}

// TestTailText 校验超长输出只保留尾部。
func TestTailText(t *testing.T) {
	if got := tailText("  short  ", 100); got != "short" {
		t.Fatalf("短文本应仅去空白，得到 %q", got)
	}
	long := strings.Repeat("x", 50)
	got := tailText(long, 10)
	if !strings.HasPrefix(got, "...") || len(got) != 13 {
		t.Fatalf("长文本截尾结果不符合预期: %q", got)
	}
}

// TestImageTag 校验镜像标签随构建输入内容变化，且同一输入稳定。
func TestImageTag(t *testing.T) {
	dir := t.TempDir()
	dockerfile := filepath.Join(dir, "Dockerfile")
	archive := filepath.Join(dir, "workspace.tar.gz")
	if err := os.WriteFile(dockerfile, []byte("FROM python:3.12-slim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, []byte("archive-v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewDockerExecutor(DockerExecutorOptions{})

	first, err := e.imageTag("Task_A", dockerfile, archive)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.imageTag("Task_A", dockerfile, archive)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("相同输入应得到相同标签: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, defaultImageTag+"task-a:") {
		t.Fatalf("标签格式不符合预期: %q", first)
	}

	if err := os.WriteFile(archive, []byte("archive-v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := e.imageTag("Task_A", dockerfile, archive)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatalf("工作区归档变化后标签必须变化，仍为 %q", changed)
	}
}
