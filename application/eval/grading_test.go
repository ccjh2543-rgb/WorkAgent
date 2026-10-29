package eval

import (
	"testing"

	domaineval "simpleAgent/domain/eval"
)

// TestGradeFromVerifier 表驱动覆盖正常、全失败、空用例与超时。
func TestGradeFromVerifier(t *testing.T) {
	cases := []struct {
		name     string
		in       domaineval.VerifierResult
		wantRate float64
		wantCode domaineval.FailureCode
	}{
		{
			name:     "3/4 通过",
			in:       domaineval.VerifierResult{Tests: 4, Failures: 1},
			wantRate: 0.75,
		},
		{
			name:     "全部通过",
			in:       domaineval.VerifierResult{Tests: 2},
			wantRate: 1,
		},
		{
			name:     "全部失败",
			in:       domaineval.VerifierResult{Tests: 2, Failures: 2},
			wantRate: 0,
		},
		{
			name:     "无用例",
			in:       domaineval.VerifierResult{},
			wantRate: 0,
			wantCode: domaineval.FailVerifierError,
		},
		{
			name:     "评分器超时",
			in:       domaineval.VerifierResult{TimedOut: true},
			wantRate: 0,
			wantCode: domaineval.FailVerifierTimeout,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			grade, failure := GradeFromVerifier(c.in)
			if grade.Rate != c.wantRate {
				t.Fatalf("Rate=%v, want %v", grade.Rate, c.wantRate)
			}
			if c.wantCode == "" {
				if failure != nil {
					t.Fatalf("不应产生失败: %+v", failure)
				}
				return
			}
			if failure == nil || failure.Code != c.wantCode {
				t.Fatalf("失败码=%+v, want %s", failure, c.wantCode)
			}
		})
	}
}
