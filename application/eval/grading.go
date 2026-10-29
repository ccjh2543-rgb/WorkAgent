package eval

import (
	"fmt"
	"math"

	domaineval "simpleAgent/domain/eval"
)

// GradeFromVerifier 把官方评分器结果转换为规则评分与评分器维度失败。
//
// passed = tests - failures - errors - skipped；tests 为 0 时评分为 0，
// 并视为评分器未产生有效结果（不伪造通过）。
func GradeFromVerifier(v domaineval.VerifierResult) (domaineval.Grade, *domaineval.Failure) {
	if v.TimedOut {
		return domaineval.Grade{}, &domaineval.Failure{
			Code: domaineval.FailVerifierTimeout, Scope: "verifier",
			Message: "评分器执行超时",
		}
	}
	if v.Tests <= 0 {
		return domaineval.Grade{}, &domaineval.Failure{
			Code: domaineval.FailVerifierError, Scope: "verifier",
			Message: "评分器未产生任何测试用例（可能缺少 JUnit XML 或导入失败）",
		}
	}
	passed := v.Tests - v.Failures - v.Errors - v.Skipped
	if passed < 0 {
		passed = 0
	}
	rate := float64(passed) / float64(v.Tests)
	// 消除浮点尾差，保证报告可读。
	rate = math.Round(rate*10000) / 10000
	return domaineval.Grade{Passed: passed, Total: v.Tests, Rate: rate}, nil
}

// FailureMessages 抽取失败用例的名称与摘要，最多保留 limit 条。
func FailureMessages(v domaineval.VerifierResult, limit int) []string {
	out := make([]string, 0, limit)
	for _, c := range v.Cases {
		if c.Status != "failure" && c.Status != "error" {
			continue
		}
		if len(out) >= limit {
			break
		}
		if c.Detail != "" {
			out = append(out, fmt.Sprintf("%s: %s", c.Name, c.Detail))
		} else {
			out = append(out, c.Name)
		}
	}
	return out
}
