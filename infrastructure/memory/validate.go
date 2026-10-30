package memory

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// MaxCandidateRunes 单条长期记忆的正文长度上限（按字符计）。
// 记忆是每次运行都要注入上下文的高频内容，单条过长会挤占对话窗口，
// 因此超限直接拒绝，而不是静默截断——截断会让事实变得不可信。
const MaxCandidateRunes = 2000

// MaxSectionRunes 记忆章节名的长度上限。
const MaxSectionRunes = 60

// secretPatterns 常见的凭证形态。命中任意一条即拒绝写入：
// 记忆文件是明文、可被检索、会进入模型上下文，不能承载任何密钥。
var secretPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"私钥块", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"OpenAI 风格密钥", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`)},
	{"AWS 访问密钥", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"密码赋值", regexp.MustCompile(`(?i)\b(password|passwd|pwd)\s*[:=]\s*\S+`)},
	{"令牌赋值", regexp.MustCompile(`(?i)\b(token|api[_-]?key|secret)\s*[:=]\s*[A-Za-z0-9_\-]{8,}`)},
}

// ValidateCandidate 校验一条待写入的长期记忆。
//
// 校验目标不是"内容对不对"，而是"该不该进入长期记忆"：
// 超长内容挤占上下文、控制字符破坏 Markdown 结构、凭证一旦写入
// 就会通过检索进入每一次模型请求，且很难彻底清除。
func ValidateCandidate(content string) error {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return errors.New("记忆内容不能为空")
	}
	if n := len([]rune(trimmed)); n > MaxCandidateRunes {
		return fmt.Errorf("记忆内容过长（%d 字符），上限为 %d 字符；请拆分成多条原子事实", n, MaxCandidateRunes)
	}
	for _, r := range trimmed {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) {
			return errors.New("记忆内容包含控制字符")
		}
	}
	for _, p := range secretPatterns {
		if p.re.MatchString(trimmed) {
			return fmt.Errorf("记忆内容疑似包含%s，已拒绝写入；请改为描述性表述，不要记录具体凭证", p.name)
		}
	}
	return nil
}

// ValidateSection 校验记忆章节名：为空表示归入「未分类」，由调用方决定；
// 非空时禁止换行与超长，避免把 Markdown 结构写坏。
func ValidateSection(section string) error {
	s := strings.TrimSpace(section)
	if s == "" {
		return nil
	}
	if n := len([]rune(s)); n > MaxSectionRunes {
		return fmt.Errorf("记忆章节名过长（%d 字符），上限为 %d 字符", n, MaxSectionRunes)
	}
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("记忆章节名不能包含换行")
	}
	return nil
}
