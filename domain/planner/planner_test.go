package planner

import "testing"

// TestParseDecision 校验决策 JSON 的解析与降级行为。
func TestParseDecision(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  Decision
		ok    bool
	}{
		{"run", `{"action":"run","task":"算增长率","reason":"需要"}`, Decision{Action: "run", Task: "算增长率", Reason: "需要"}, true},
		{"final", `{"action":"final","task":"","reason":"条件不满足"}`, Decision{Action: "final", Reason: "条件不满足"}, true},
		{"带围栏", "```json\n{\"action\":\"run\",\"task\":\"建任务\"}\n```", Decision{Action: "run", Task: "建任务"}, true},
		{"前有杂文", `好的：{"action":"final","task":""} 完毕`, Decision{Action: "final"}, true},
		{"非法文本", "没想好", Decision{}, false},
		{"缺action", `{"task":"x"}`, Decision{Task: "x"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseDecision(tc.reply)
			if ok != tc.ok {
				t.Fatalf("parseDecision(%q) ok=%v, 期望 %v", tc.reply, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseDecision(%q) = %+v, 期望 %+v", tc.reply, got, tc.want)
			}
		})
	}
}
