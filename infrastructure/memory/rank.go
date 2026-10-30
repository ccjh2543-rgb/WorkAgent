package memory

import (
	"sort"
	"time"
)

// 一天对应的秒数，用于时效衰减。
const daySeconds = 24 * 60 * 60

// kindWeight 记忆类型在检索重排中的权重。
//
// 权重刻意保持克制：检索相关性仍是主导因素，类型只在名次接近时起作用，
// 不能因为一条"决策"类型就把真正相关的普通事实挤下去。
func kindWeight(kind Kind) float64 {
	switch NormalizeKind(string(kind)) {
	case KindDecision:
		return 1.15
	case KindLesson:
		return 1.10
	case KindPreference:
		return 1.05
	case KindProcedure:
		return 0.95
	default:
		return 1.0
	}
}

// recencyFactor 时效因子：新记忆轻微加权，长期未更新的流程类内容轻微降权。
// 时间未知（历史数据）时返回 1.0，不做任何猜测。
func recencyFactor(updatedAt, now int64) float64 {
	if updatedAt <= 0 || now <= updatedAt {
		return 1.0
	}
	age := now - updatedAt
	switch {
	case age <= 30*daySeconds:
		return 1.05
	case age <= 180*daySeconds:
		return 1.0
	default:
		return 0.9
	}
}

// Rerank 在检索结果的基础上按类型与时效做小幅重排。
//
// 基础分取 1/(名次+1)：名次来自检索器，是相关性的唯一可信来源。
// 类型与时效只作为乘性微调，因此只有在相邻名次得分接近时才会交换顺序，
// 既能让重要记忆略微前移，又不会破坏检索本身的排序语义。
//
// 返回新切片，不修改入参；得分相同时保持原顺序，保证结果可复现。
func Rerank(fragments []Fragment, now int64) []Fragment {
	if len(fragments) < 2 {
		return fragments
	}
	type scored struct {
		fragment Fragment
		score    float64
		order    int
	}
	items := make([]scored, len(fragments))
	for i, f := range fragments {
		base := 1.0 / float64(i+1)
		items[i] = scored{
			fragment: f,
			score:    base * kindWeight(f.Kind) * recencyFactor(f.UpdatedAt, now),
			order:    i,
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].order < items[j].order
	})
	out := make([]Fragment, len(items))
	for i := range items {
		out[i] = items[i].fragment
	}
	return out
}

// RerankNow 以当前时间作为时效基准，供调用方省略时间参数。
func RerankNow(fragments []Fragment) []Fragment {
	return Rerank(fragments, time.Now().Unix())
}
