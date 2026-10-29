// Package tokenizer 实现 DeepSeek 官方离线分词器（infrastructure 层）。
//
// 它读取 HF 仓库 deepseek-ai/DeepSeek-V4-Flash-0731 发布的 tokenizer.json，
// 在纯 Go 中复刻模型文件的预处理（ByteLevel BPE），实现 domain/agent.Tokenizer
// 端口。选型说明：本工程刻意使用纯 Go（如 modernc.org/sqlite）规避 CGO，因此
// 不引入需要链接 libtokenizers 的 cgo 绑定库，改为自实现。
//
// 局限与取舍：service 端 usage.prompt_tokens 是按 encode_messages 拼装后的
// 输入串再分词的长度，其中多消息的格式头（角色、换行等）开销未被本实现计入，
// 由调用方在预算侧保留安全边距吸收。
package tokenizer

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// ServiceModel 出厂时随 tokenizer.json 打包的模型标识，仅作日志/诊断用。
const ServiceModel = "deepseek-ai/DeepSeek-V4-Flash-0731"

// tokenizerJSON 仅承载本实现需要的最小字段。
type tokenizerJSON struct {
	Model struct {
		Vocab  map[string]int `json:"vocab"`
		Merges []string       `json:"merges"`
	} `json:"model"`
}

// DeepSeekTokenizer 纯 Go 的 ByteLevel BPE 分词器实现。
// 使用单一实例，内部 map 只读，因此并发安全。
type DeepSeekTokenizer struct {
	// symbolToID 单码位符号 -> token id（用于初始字节编码）
	symbolToID map[uint32]int
	// idToToken 按 token id 反查其符号串（用于在合并时构造键）
	idToToken map[int]string
	// pairRank 相邻 token 对 -> 合并优先级（值越小越先合并）
	pairRank map[[2]string]int
	// pairResult 相邻 token 对 -> 合并结果 token 的 id
	pairResult map[[2]string]int
}

// 三个预分词正则，复刻 tokenizer.json 中 pre_tokenizer 的 Sequence。
var (
	reSplitNumber = regexp.MustCompile(`\p{N}{1,3}`)
	reSplitCJK    = regexp.MustCompile(`[一-龥぀-ゟ゠-ヿ]+`)
	// 主正则对原文件中的 `\s+(?!\S)` 做了无 lookahead 等价改写：
	// `\s+(?!\S)` 与末尾 `\s+` 实际都可被最后一个分支 `\s+` 覆盖，因此省略。
	reSplitMain = regexp.MustCompile(`[^\r\n\p{L}\p{P}\p{S}]?[\p{L}\p{M}]+| ?[\p{P}\p{S}]+[\r\n]*|\s*[\r\n]+|\s+`)
)

// NewDeepSeekTokenizer 从 tokenizer.json 字节构建分词器。
func NewDeepSeekTokenizer(data []byte) (*DeepSeekTokenizer, error) {
	var raw tokenizerJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("解析 tokenizer.json 失败: %w", err)
	}
	if len(raw.Model.Vocab) == 0 || len(raw.Model.Merges) == 0 {
		return nil, fmt.Errorf("tokenizer.json 缺少 vocab 或 merges")
	}
	t := &DeepSeekTokenizer{
		symbolToID: make(map[uint32]int, len(raw.Model.Vocab)),
		idToToken:  make(map[int]string, len(raw.Model.Vocab)),
		pairRank:   make(map[[2]string]int, len(raw.Model.Merges)),
		pairResult: make(map[[2]string]int, len(raw.Model.Merges)),
	}
	for sym, id := range raw.Model.Vocab {
		t.idToToken[id] = sym
		if rs := []rune(sym); len(rs) == 1 {
			t.symbolToID[uint32(rs[0])] = id
		}
	}
	for i, m := range raw.Model.Merges {
		parts := strings.SplitN(m, " ", 2)
		if len(parts) != 2 {
			continue
		}
		key := [2]string{parts[0], parts[1]}
		t.pairRank[key] = i
		if id, ok := raw.Model.Vocab[parts[0]+parts[1]]; ok {
			t.pairResult[key] = id
		}
	}
	return t, nil
}

// Count 返回 text 分词后的 token 数。
func (t *DeepSeekTokenizer) Count(_ context.Context, text string) (int, error) {
	ids, err := t.TokenIDs(text)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// TokenIDs 返回 text 分词后的 token id 列表（用于诊断与对照）。
func (t *DeepSeekTokenizer) TokenIDs(text string) ([]int, error) {
	var ids []int
	for _, tok := range splitPretoken(text) {
		ids = append(ids, t.bpe(byteEncode(tok))...)
	}
	return ids, nil
}

// splitPretoken 依次应用三段 Split 预分词，再把每个 token 做 ByteLevel 字节编码。
func splitPretoken(text string) []string {
	tokens := []string{text}
	tokens = splitAll(tokens, reSplitNumber)
	tokens = splitAll(tokens, reSplitCJK)
	tokens = splitAll(tokens, reSplitMain)
	return attachLeadingSpace(tokens)
}

// attachLeadingSpace 复刻原正则中 `\s+(?!\S)` 的效果：Go 的 regexp 不支持
// lookahead，`\s+` 会把整段连续空格都吞掉，而参考实现会为紧随的非空白 token
// 留下一个前导空格（供 `[^\r\n\p{L}\p{P}\p{S}]?[\p{L}\p{M}]+` 分支附着），
// 从而影响 BPE 合并结果。这里把「全部由空格组成、长度>=2」的 token 的最后一个
// 空格移到下一 token 首部，还原参考分段。
func attachLeadingSpace(tokens []string) []string {
	for i := 0; i < len(tokens)-1; i++ {
		t := tokens[i]
		if len(t) < 2 || t[len(t)-1] != ' ' {
			continue
		}
		allSpace := true
		for j := 0; j < len(t); j++ {
			if t[j] != ' ' {
				allSpace = false
				break
			}
		}
		if !allSpace {
			continue
		}
		next := tokens[i+1]
		if next == "" {
			continue
		}
		tokens[i] = t[:len(t)-1]
		tokens[i+1] = " " + next
	}
	return tokens
}

// splitAll 对每个 token 应用正则再切分（behavior=Isolated：切出的子 token 不再合并）。
func splitAll(tokens []string, re *regexp.Regexp) []string {
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		locs := re.FindAllStringIndex(tok, -1)
		if len(locs) == 0 {
			out = append(out, tok)
			continue
		}
		prev := 0
		for _, loc := range locs {
			if loc[0] > prev {
				out = append(out, tok[prev:loc[0]])
			}
			out = append(out, tok[loc[0]:loc[1]])
			prev = loc[1]
		}
		if prev < len(tok) {
			out = append(out, tok[prev:])
		}
	}
	return out
}

// byteToUnicode 复刻 tokenizers 库的 bytes_to_unicode，建立“字节 <-> 单码位符号”双射。
// 由 buildByteToUnicode 一次性计算并缓存。
var byteToUnic = buildByteToUnicode()

func buildByteToUnicode() [256]rune {
	var m [256]rune
	contains := func(s []int, v int) bool {
		for _, x := range s {
			if x == v {
				return true
			}
		}
		return false
	}
	bs := []int{}
	for i := int('!'); i <= int('~'); i++ {
		bs = append(bs, i)
	}
	for i := int('¡'); i <= int('¬'); i++ { // ¡-¬ 即 U+00A1..U+00AC
		bs = append(bs, i)
	}
	for i := int('®'); i <= int('ÿ'); i++ { // ®-ÿ 即 U+00AE..U+00FF
		bs = append(bs, i)
	}
	cs := append([]int(nil), bs...)
	n := 0
	for b := 0; b < 256; b++ {
		if !contains(bs, b) {
			bs = append(bs, b)
			cs = append(cs, 256+n)
			n++
		}
	}
	for i, b := range bs {
		m[b] = rune(cs[i])
	}
	return m
}

// byteEncode 把一个预分词 token 的每个字节映射为单码位符号串。
func byteEncode(tok string) string {
	buf := make([]rune, len(tok))
	for i, b := range []byte(tok) {
		buf[i] = byteToUnic[b]
	}
	return string(buf)
}

// bpe 对单字节符号序列执行 BPE 合并，返回合并后的 token id 列表（长度即 token 数）。
// 采用 minbpe 的“每轮合并最低优先级对的所有出现”算法。
func (t *DeepSeekTokenizer) bpe(encoded string) []int {
	ids := make([]int, 0, len(encoded))
	for _, s := range encoded {
		if id, ok := t.symbolToID[uint32(s)]; ok {
			ids = append(ids, id)
		} else {
			ids = append(ids, -1)
		}
	}
	for len(ids) >= 2 {
		bestIdx, bestRank := -1, math.MaxInt
		for i := 0; i < len(ids)-1; i++ {
			if ids[i] < 0 || ids[i+1] < 0 {
				continue
			}
			key := [2]string{t.idToToken[ids[i]], t.idToToken[ids[i+1]]}
			if r, ok := t.pairRank[key]; ok && r < bestRank {
				bestRank = r
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		key := [2]string{t.idToToken[ids[bestIdx]], t.idToToken[ids[bestIdx+1]]}
		mergedID, ok := t.pairResult[key]
		if !ok {
			break
		}
		newIDs := make([]int, 0, len(ids))
		for i := 0; i < len(ids); {
			if i < len(ids)-1 && t.idToToken[ids[i]] == key[0] && t.idToToken[ids[i+1]] == key[1] {
				newIDs = append(newIDs, mergedID)
				i += 2
			} else {
				newIDs = append(newIDs, ids[i])
				i++
			}
		}
		ids = newIDs
	}
	return ids
}
