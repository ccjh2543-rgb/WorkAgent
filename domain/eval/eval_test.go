package eval

import (
	"encoding/json"
	"testing"
	"time"
)

// TestStatusIsTerminal 覆盖全部状态的终态判断。
func TestStatusIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		StatusCompleted:   true,
		StatusFailed:      true,
		StatusLimit:       true,
		StatusCancelled:   true,
		StatusNeedsInput:  true,
		Status("running"): false,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Fatalf("Status(%s).IsTerminal()=%v, want %v", s, got, want)
		}
	}
}

// TestReportJSONRoundTrip 校验报告落盘字段稳定、可反序列化。
func TestReportJSONRoundTrip(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	in := Report{
		SchemaVersion: SchemaVersion,
		RunID:         "20261026T153000-3f9c",
		TaskID:        "demo-task",
		Mode:          "plan",
		Status:        StatusCompleted,
		StartedAt:     now,
		FinishedAt:    now.Add(time.Second),
		DurationMS:    1000,
		Grades:        Grades{Rule: &Grade{Passed: 3, Total: 4, Rate: 0.75}, Total: 0.75},
		Efficiency:    Efficiency{LLMCalls: 2, MaxWindowTokens: 100, DurationMS: 1000},
		Failures:      []Failure{{Code: FailArtifactMissing, Message: "缺少产物"}},
		Verifier:      VerifierResult{Command: "pytest", Cwd: "/workspace", ExitCode: 1},
		Environment:   EnvironmentInfo{ExecutorMode: "local", DatasetVersion: "v1.0", Model: "m"},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var out Report
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if out.SchemaVersion != SchemaVersion || out.RunID != in.RunID || out.Status != StatusCompleted {
		t.Fatalf("字段丢失: %+v", out)
	}
	if out.Grades.Rule == nil || out.Grades.Rule.Passed != 3 {
		t.Fatalf("评分字段丢失: %+v", out.Grades)
	}
	if len(out.Failures) != 1 || out.Failures[0].Code != FailArtifactMissing {
		t.Fatalf("失败码丢失: %+v", out.Failures)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("解析原始 JSON 失败: %v", err)
	}
	for _, key := range []string{"schema_version", "run_id", "task_id", "status", "artifacts", "grades", "efficiency", "failures", "verifier", "environment"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("报告缺少字段 %s", key)
		}
	}
}
