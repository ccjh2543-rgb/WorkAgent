package memory

import (
	"context"
	"sort"
	"strings"
	"unicode"
)

// 关键词命中权重：标题命中比正文命中更能代表主题相关
const (
	scoreSectionHit = 3
	scoreContentHit = 2
	scoreExactHit   = 5
)

// Retriever 记忆相关性检索接口。
// 实现可能是纯内存的关键词检索，也可能是「SQLite 索引 + 向量」的混合检索。
type Retriever interface {
	// Rank 从候选中挑出与 query 最相关的 topK 条记忆，按相关度降序返回。
	// 依赖外部索引或向量服务的实现可能返回错误，由上层决定降级策略。
	Rank(ctx context.Context, query string, candidates []Fragment, topK int) ([]Fragment, error)
}

// KeywordRetriever 基于关键词命中的轻量检索器，不依赖任何外部服务。
// 它同时作为混合检索不可用时的降级实现。
type KeywordRetriever struct{}

// Rank 实现 Retriever 接口。
// 无任何关键词命中时，回退为按文件顺序返回前 topK 条：
// 记忆条目通常数量很少，冷启动阶段全部注入比完全不注入更有价值。
func (KeywordRetriever) Rank(ctx context.Context, query string, candidates []Fragment, topK int) ([]Fragment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 || len(candidates) == 0 {
		return nil, nil
	}
	tokens := Tokenize(query)
	if len(tokens) == 0 {
		return firstN(candidates, topK), nil
	}

	normalizedQuery := normalizeText(query)
	type scored struct {
		fragment Fragment
		score    int
		order    int
	}
	hits := make([]scored, 0, len(candidates))
	for i, f := range candidates {
		score := scoreFragment(tokens, normalizedQuery, f)
		if score == 0 {
			continue
		}
		hits = append(hits, scored{fragment: f, score: score, order: i})
	}
	if len(hits) == 0 {
		return firstN(candidates, topK), nil
	}

	// 同分时保持文件中的原始顺序，保证检索结果稳定可复现
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].order < hits[j].order
	})

	result := make([]Fragment, 0, min(topK, len(hits)))
	for _, h := range hits {
		if len(result) >= topK {
			break
		}
		result = append(result, h.fragment)
	}
	return result, nil
}

// scoreFragment 计算单条记忆的相关度得分
func scoreFragment(tokens []string, normalizedQuery string, f Fragment) int {
	section := normalizeText(f.Section)
	content := normalizeText(f.Content)
	score := 0
	for _, tk := range tokens {
		if strings.Contains(section, tk) {
			score += scoreSectionHit
		}
		if strings.Contains(content, tk) {
			score += scoreContentHit
		}
	}
	// 整句精确命中额外加权，用于拉开「主题匹配」与「零散词匹配」的差距
	if normalizedQuery != "" && strings.Contains(content, normalizedQuery) {
		score += scoreExactHit
	}
	return score
}

// Tokenize 把文本切分为检索词：
// 连续的汉字切成二元字组（单字时保留该字），连续的字母/数字按整词处理，
// 其它字符作为分隔符。汉字二元切分的目的，是在不引入分词库的前提下
// 让中文也能被检索到，同时该结果可直接喂给 FTS5 的 unicode61 分词器。
func Tokenize(text string) []string {
	lower := strings.ToLower(text)
	tokens := make([]string, 0, 16)
	var han []rune
	var latin []rune

	flushHan := func() {
		switch len(han) {
		case 0:
		case 1:
			tokens = append(tokens, string(han))
		default:
			for i := 0; i+1 < len(han); i++ {
				tokens = append(tokens, string(han[i:i+2]))
			}
		}
		han = han[:0]
	}
	flushLatin := func() {
		// 长度小于 2 的英文片段噪音太大，直接丢弃
		if len(latin) >= 2 {
			tokens = append(tokens, string(latin))
		}
		latin = latin[:0]
	}

	for _, r := range lower {
		switch {
		case unicode.Is(unicode.Han, r):
			flushLatin()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			latin = append(latin, r)
		default:
			flushHan()
			flushLatin()
		}
	}
	flushHan()
	flushLatin()
	return tokens
}

// TokenText 把文本转换成写入 FTS5 索引列的词元串（空格分隔）。
// 不能直接写入 MatchQuery 的结果：那样 OR 与引号本身也会被当成词元写进索引，
// 导致索引里混入无意义的 "or" 词条。写入与查询必须分开处理。
func TokenText(text string) string {
	return strings.Join(Tokenize(text), " ")
}

// MatchQuery 把文本转换成 FTS5 的 MATCH 查询串，例如：
//
//	`"架构" OR "构约" OR "约定"`
//
// FTS5 的 unicode61 分词器不切分中文，所以先在这里按二元字组切好再交给它。
// Tokenize 保证词元只含字母、数字与汉字，不含 FTS5 语法字符，
// 因此用双引号包裹后既是字面量匹配也不会引发语法错误。
// 没有任何可用词元时返回空串，调用方应视为「不检索」而不是「匹配全部」。
func MatchQuery(text string) string {
	tokens := Tokenize(text)
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(tokens))
	for _, tk := range tokens {
		quoted = append(quoted, `"`+tk+`"`)
	}
	return strings.Join(quoted, " OR ")
}

// normalizeText 归一化文本：转小写并去掉全部空白字符，
// 使「DDD 分层」与「ddd分层」可以互相匹配。
func normalizeText(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), ""))
}

// firstN 返回前 n 条记忆的副本，避免调用方改动底层切片
func firstN(fragments []Fragment, n int) []Fragment {
	if len(fragments) <= n {
		return append([]Fragment(nil), fragments...)
	}
	return append([]Fragment(nil), fragments[:n]...)
}
