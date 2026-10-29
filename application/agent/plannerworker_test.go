package agent

import (
	"testing"

	"simpleAgent/domain/planner"
)

// TestAppendPriorSummary 校验前置结果摘要的累积与超预算裁剪行为。
func TestAppendPriorSummary(t *testing.T) {
	cases := []struct {
		name     string
		prior    string
		r        workerResult
		priorTok int
		budget   int
		want     string
	}{
		{
			name:     "空前置首条追加",
			prior:    "",
			r:        workerResult{TaskID: 1, OK: true, Result: "hello"},
			priorTok: 0,
			budget:   300000,
			want:     "子任务1: hello",
		},
		{
			name:     "成功但无摘要标记",
			prior:    "",
			r:        workerResult{TaskID: 2, OK: true, Result: "   "},
			priorTok: 0,
			budget:   300000,
			want:     "子任务2: （无结果）",
		},
		{
			name:     "超预算截断只留最新",
			prior:    "子任务1: first\n子任务2: second",
			r:        workerResult{TaskID: 3, OK: true, Result: "third"},
			priorTok: 200000, // 已接近预算，追加会超
			budget:   300000,
			want:     "子任务3: third",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := appendPriorSummary(tc.prior, tc.r, tc.priorTok, tc.budget); got != tc.want {
				t.Fatalf("appendPriorSummary() = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// TestTokenCountApprox 校验前置摘要用的轻量估算函数。
func TestTokenCountApprox(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 1},    // 0/3+1
		{"abc", 2}, // 3/3+1
		{"你好", 1},  // 2/3+1
	}
	for _, tc := range cases {
		if got := tokenCountApprox(tc.in); got != tc.want {
			t.Fatalf("tokenCountApprox(%q) = %d, 期望 %d", tc.in, got, tc.want)
		}
	}
}

// TestPwWorkerMessageBudget 校验 Worker 历史消息预算计算（含最小下限）。
func TestPwWorkerMessageBudget(t *testing.T) {
	cases := []struct {
		budget int
		want   int
	}{
		{10000, 4000},  // 10000*0.4
		{1000, 2000},   // 小于下限，取 2000
		{50000, 20000}, // 50000*0.4
	}
	for _, tc := range cases {
		if got := pwWorkerMessageBudget(tc.budget); got != tc.want {
			t.Fatalf("pwWorkerMessageBudget(%d) = %d, 期望 %d", tc.budget, got, tc.want)
		}
	}
}

// TestCopyTasks 校验任务清单复制不共享底层切片。
func TestCopyTasks(t *testing.T) {
	src := []planner.Task{{ID: 1, Content: "a"}, {ID: 2, Content: "b"}}
	dst := copyTasks(src)
	if len(dst) != len(src) {
		t.Fatalf("复制后长度不一致: got %d want %d", len(dst), len(src))
	}
	// 修改源切片不应影响复制结果
	src[0].Content = "changed"
	if dst[0].Content != "a" {
		t.Fatalf("复制结果被源共享: got %q", dst[0].Content)
	}
}

// TestParseWorkerResult 校验 Worker 收尾结构化 JSON 的解析与降级行为。
func TestParseWorkerResult(t *testing.T) {
	cases := []struct {
		name   string
		reply  string
		status string
		desc   string
		result string
		ok     bool
	}{
		{"完整JSON", `{"status":"ok","description":"已计算求和","result":"5"}`, "ok", "已计算求和", "5", true},
		{"缺status默认ok", `{"description":"x","result":"y"}`, "ok", "x", "y", true},
		{"partial状态", `{"status":"partial","description":"部分删除","result":"删了3条"}`, "partial", "部分删除", "删了3条", true},
		{"带json围栏", "```json\n{\"status\":\"failed\",\"description\":\"出错\",\"result\":\"\"}\n```", "failed", "出错", "", true},
		{"前有注释后带杂文", `好的如下：{"status":"ok","description":"建任务","result":"完成"} 完毕`, "ok", "建任务", "完成", true},
		{"非法文本降级", "我没有工具了，结果如下", "ok", "", "我没有工具了，结果如下", false},
		{"无花括号降级", "直接给结果", "ok", "", "直接给结果", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, desc, result, ok := parseWorkerResult(tc.reply)
			if status != tc.status || desc != tc.desc || result != tc.result || ok != tc.ok {
				t.Fatalf("parseWorkerResult(%q) = (%q, %q, %q, %v), 期望 (%q, %q, %q, %v)",
					tc.reply, status, desc, result, ok, tc.status, tc.desc, tc.result, tc.ok)
			}
		})
	}
}
