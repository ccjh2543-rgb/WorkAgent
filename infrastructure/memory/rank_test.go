package memory

import (
	"fmt"
	"testing"
)

// TestRerankPromotesDecisionOnNearTie 验证类型加权只在名次接近时改变顺序。
func TestRerankPromotesDecisionOnNearTie(t *testing.T) {
	frags := make([]Fragment, 10)
	for i := range frags {
		frags[i] = Fragment{Content: fmt.Sprintf("fact-%d", i), Kind: KindFact}
	}
	frags[9] = Fragment{Content: "decision-9", Kind: KindDecision}

	got := Rerank(frags, 1000)
	if got[8].Content != "decision-9" || got[9].Content != "fact-8" {
		t.Fatalf("名次接近时决策类应前移，实际顺序: %q, %q", got[8].Content, got[9].Content)
	}
}

// TestRerankKeepsRelevanceDominant 验证相关性仍然主导：第一名不会被第二名挤掉。
func TestRerankKeepsRelevanceDominant(t *testing.T) {
	frags := []Fragment{
		{Content: "最相关的事实", Kind: KindFact},
		{Content: "次相关的决策", Kind: KindDecision},
	}
	got := Rerank(frags, 1000)
	if got[0].Content != "最相关的事实" {
		t.Fatalf("相关性第一的条目不应被类型加权挤到后面，实际首条: %q", got[0].Content)
	}
}

// TestRerankDemotesStaleProcedure 验证长期未更新的流程类记忆会被轻微降权。
func TestRerankDemotesStaleProcedure(t *testing.T) {
	now := int64(200 * daySeconds)
	frags := make([]Fragment, 9)
	for i := range frags {
		frags[i] = Fragment{Content: fmt.Sprintf("fresh-%d", i), Kind: KindFact, UpdatedAt: now}
	}
	frags[7] = Fragment{Content: "stale-procedure", Kind: KindProcedure, UpdatedAt: 1}
	frags[8] = Fragment{Content: "fresh-fact", Kind: KindFact, UpdatedAt: now}

	got := Rerank(frags, now)
	if got[7].Content != "fresh-fact" || got[8].Content != "stale-procedure" {
		t.Fatalf("过期流程类应后移，实际顺序: %q, %q", got[7].Content, got[8].Content)
	}
}

// TestRerankShortInput 验证空输入与单条输入原样返回。
func TestRerankShortInput(t *testing.T) {
	if got := Rerank(nil, 1); len(got) != 0 {
		t.Fatalf("空输入应返回空切片，实际 %d 条", len(got))
	}
	one := []Fragment{{Content: "唯一", Kind: KindLesson}}
	if got := Rerank(one, 1); len(got) != 1 || got[0].Content != "唯一" {
		t.Fatalf("单条输入应原样返回")
	}
}
