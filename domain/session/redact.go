package session

import "regexp"

// secretRule 一条敏感信息识别规则
type secretRule struct {
	label string
	re    *regexp.Regexp
	// keepName 为 true 时保留「键名=」前缀，只掩掉后面的值，
	// 便于事后看出被脱敏的是什么字段
	keepName bool
}

// secretRules 敏感信息识别规则。
//
// 顺序有讲究：必须先匹配「键名=值」的赋值形式，再匹配独立出现的密钥串。
// 否则 API_KEY=sk-xxx 会先被通用密钥规则吃掉键名，丢失字段语义。
var secretRules = []secretRule{
	{
		// 前缀 [A-Za-z0-9_]* 用来吃掉键名前面的限定词，
		// 例如 MEMORY_EMBEDDING_API_KEY 也要整体保留下来。
		// 不加 \b 是因为 _ 属于单词字符，加了反而匹配不到 API_KEY。
		label:    "密钥",
		re:       regexp.MustCompile(`(?i)([A-Za-z0-9_]*(?:api[_-]?key|apikey|access[_-]?key|secret|password|passwd|token))\s*[:=]\s*["']?[A-Za-z0-9._\-]{8,}["']?`),
		keepName: true,
	},
	{
		label: "密钥",
		re:    regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}\b`),
	},
	{
		label: "AccessKey",
		re:    regexp.MustCompile(`\bLTAI[A-Za-z0-9]{12,}\b`),
	},
	{
		label: "令牌",
		re:    regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-]{16,}`),
	},
	{
		label: "手机号",
		re:    regexp.MustCompile(`\b1[3-9]\d{9}\b`),
	},
}

// Redact 把文本中的敏感信息替换为脱敏占位符。
//
// 只在消息落盘前调用：送给大模型的副本必须保持原文，
// 否则会影响推理质量。代价是历史被回放时模型看到的是脱敏后的内容，
// 这是刻意的安全取舍。
func Redact(text string) string {
	if text == "" {
		return text
	}
	for _, rule := range secretRules {
		placeholder := "[已脱敏:" + rule.label + "]"
		if rule.keepName {
			text = rule.re.ReplaceAllString(text, "$1="+placeholder)
			continue
		}
		text = rule.re.ReplaceAllString(text, placeholder)
	}
	return text
}
