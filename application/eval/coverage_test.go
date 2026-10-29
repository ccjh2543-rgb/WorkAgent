package eval

import (
	"reflect"
	"testing"
)

// TestAnalyzeCoverage 校验任务集可执行性统计与写能力缺口识别。
func TestAnalyzeCoverage(t *testing.T) {
	cases := []struct {
		name     string
		tasks    []TaskSpec
		writable []string
		want     CoverageSummary
	}{
		{
			name: "全部可执行且能力覆盖",
			tasks: []TaskSpec{
				{ID: "a", Artifacts: []DeclaredArtifact{{Type: "xlsx"}, {Type: "md"}}},
				{ID: "b", Artifacts: []DeclaredArtifact{{Type: "json"}}},
			},
			writable: []string{"xlsx", "md", "json"},
			want: CoverageSummary{
				Total: 2, Valid: 2, Invalid: 0,
				Issues:        []CoverageIssue{},
				ArtifactTypes: map[string]int{"xlsx": 1, "md": 1, "json": 1},
			},
		},
		{
			name: "存在不可执行任务",
			tasks: []TaskSpec{
				{ID: "ok", Artifacts: []DeclaredArtifact{{Type: "md"}}},
				{ID: "bad", LoadErr: "缺少 tests/verifier.toml"},
			},
			writable: []string{"md"},
			want: CoverageSummary{
				Total: 2, Valid: 1, Invalid: 1,
				Issues:        []CoverageIssue{{TaskID: "bad", Reason: "缺少 tests/verifier.toml"}},
				ArtifactTypes: map[string]int{"md": 1},
			},
		},
		{
			name: "同类产物按任务去重",
			tasks: []TaskSpec{
				{ID: "a", Artifacts: []DeclaredArtifact{{Type: "md"}, {Type: "md"}, {Type: "MD"}}},
			},
			writable: []string{"md"},
			want: CoverageSummary{
				Total: 1, Valid: 1, Invalid: 0,
				Issues:        []CoverageIssue{},
				ArtifactTypes: map[string]int{"md": 1},
			},
		},
		{
			name: "暴露写能力缺口",
			tasks: []TaskSpec{
				{ID: "a", Artifacts: []DeclaredArtifact{{Type: ".DOCX"}, {Type: "pptx"}, {Type: "docx"}}},
				{ID: "b", Artifacts: []DeclaredArtifact{{Type: "xlsx"}}},
			},
			writable: []string{"xlsx"},
			want: CoverageSummary{
				Total: 2, Valid: 2, Invalid: 0,
				Issues:               []CoverageIssue{},
				ArtifactTypes:        map[string]int{"docx": 1, "pptx": 1, "xlsx": 1},
				MissingArtifactTypes: []string{"docx", "pptx"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AnalyzeCoverage(tc.tasks, tc.writable)
			if got.Total != tc.want.Total || got.Valid != tc.want.Valid || got.Invalid != tc.want.Invalid {
				t.Fatalf("计数不符: got=%d/%d/%d want=%d/%d/%d",
					got.Total, got.Valid, got.Invalid, tc.want.Total, tc.want.Valid, tc.want.Invalid)
			}
			if !reflect.DeepEqual(got.Issues, tc.want.Issues) {
				t.Fatalf("无效任务明细不符: got=%v want=%v", got.Issues, tc.want.Issues)
			}
			if !reflect.DeepEqual(got.ArtifactTypes, tc.want.ArtifactTypes) {
				t.Fatalf("产物类型统计不符: got=%v want=%v", got.ArtifactTypes, tc.want.ArtifactTypes)
			}
			if !reflect.DeepEqual(got.MissingArtifactTypes, tc.want.MissingArtifactTypes) {
				t.Fatalf("写能力缺口不符: got=%v want=%v", got.MissingArtifactTypes, tc.want.MissingArtifactTypes)
			}
		})
	}
}
