package agent

// 上下文预算与压缩的纯函数部分。
// 编排（何时压缩、如何调用摘要模型）在 application 层实现，这里只放
// 可独立测试的 token 估算与窗口计算。

// 宽字符的起始码位。
//
// 中文标点(U+3000)、韩文音节(U+AC00)、全角符号(U+FF00)、
// CJK 扩展区(U+20000+) 都在此之后，因此用一个阈值就能覆盖全部。
const wideRuneStart = 0x2E80

// charsPerTokenLatin 拉丁字符的 token 折算比例，约 4 字符 1 token
const charsPerTokenLatin = 4

// EstimateTokens 对文本 token 数做启发式估算（兜底实现）。
//
// 当没有装配精确 tokenizer（domain/agent.Tokenizer）或计数出错时使用。
// 按字符类别分别估算并取上界：
// 宽字符（汉字、假名、韩文、中文标点、全角符号）约 1 字符 1 token，
// 其余字符约 4 字符 1 token。
//
// 刻意偏高估算——高估只是提前触发压缩，低估会让请求被 API 直接拒绝。
// 注意必须把中文标点也算作宽字符：按 1/4 token 处理会系统性低估中文文本，
// 一段以标点为主的文本能低估好几倍。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	wide, rest := 0, 0
	for _, r := range text {
		if r >= wideRuneStart {
			wide++
			continue
		}
		rest++
	}
	return wide + (rest+charsPerTokenLatin-1)/charsPerTokenLatin
}
