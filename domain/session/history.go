package session

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultMaxMessageBytes 单条消息落盘的最大字节数。
	// 一次超长输出就能把库撑大，因此落盘前必须设上限。
	DefaultMaxMessageBytes = 64 * 1024

	// truncatedSuffix 截断标记，便于事后识别内容不完整
	truncatedSuffix = "\n...[内容过长已截断]"

	// defaultPreviewRunes 摘要默认长度
	defaultPreviewRunes = 40

	// DefaultFoldHeadRunes / DefaultFoldTailRunes 工具输出折叠时保留的首尾长度
	DefaultFoldHeadRunes = 400
	DefaultFoldTailRunes = 200
)

// Preview 返回文本的单行摘要，用于在分支列表里辨认内容。
// maxRunes 非正数时使用默认长度。
func Preview(text string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = defaultPreviewRunes
	}
	flattened := []rune(strings.Join(strings.Fields(text), " "))
	if len(flattened) <= maxRunes {
		return string(flattened)
	}
	return string(flattened[:maxRunes]) + "…"
}

// Sanitize 对即将落盘的消息正文做脱敏与长度限制。
// maxBytes 非正数时使用 DefaultMaxMessageBytes。
func Sanitize(text string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageBytes
	}
	return Truncate(Redact(text), maxBytes)
}

// Truncate 按字节上限截断文本，并按 rune 边界对齐，避免切出半个多字节字符。
func Truncate(text string, maxBytes int) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + truncatedSuffix
}

// FoldToolResult 折叠过长的工具输出，保留首尾、中间省略。
//
// 折叠与截断的区别：截断只留开头，折叠还保留结尾。
// 工具输出（命令结果、文件内容、接口响应）的关键信息常常在末尾，
// 只留开头会让模型拿到一个「看起来正常但结论缺失」的结果。
//
// headRunes / tailRunes 非正数时分别使用默认值。
func FoldToolResult(content string, headRunes, tailRunes int) string {
	if headRunes <= 0 {
		headRunes = DefaultFoldHeadRunes
	}
	if tailRunes <= 0 {
		tailRunes = DefaultFoldTailRunes
	}
	runes := []rune(content)
	if len(runes) <= headRunes+tailRunes {
		return content
	}
	omitted := len(runes) - headRunes - tailRunes
	return fmt.Sprintf("%s\n...[已折叠省略 %d 字符]...\n%s",
		string(runes[:headRunes]), omitted, string(runes[len(runes)-tailRunes:]))
}

// TrimHistory 按字符预算裁剪历史，从最新的消息往回保留。
//
// 至少保留最后一条：历史被裁空比略微超出预算更糟，
// 模型完全失去上下文会让多轮对话彻底失效。
func TrimHistory(messages []Message, maxRunes int) []Message {
	if maxRunes <= 0 || len(messages) == 0 {
		return messages
	}
	used := 0
	start := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		size := utf8.RuneCountInString(messages[i].Content)
		// start < len(messages) 表示已经装下过至少一条，此后才允许因超预算而停
		if used+size > maxRunes && start < len(messages) {
			break
		}
		used += size
		start = i
	}
	return messages[start:]
}
