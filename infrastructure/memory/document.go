// Package memory 实现工作区级持久记忆：Markdown 记忆文件的解析、读写、
// 相关性检索，以及注入 Prompt 前的上下文装配。
//
// 记忆以人类可读、可直接编辑的 Markdown 文件保存，默认路径为
// <项目根目录>/.simpleagent/memory/MEMORY.md，因此记忆不是黑盒数据，
// 用户可以随时打开文件增删改条目。
package memory

import (
	"fmt"
	"strconv"
	"strings"
)

// Scope 记忆作用域，决定记忆的生效范围与可写性。
// 当前仅实现工作区级（项目隔离），云端画像属于只读作用域，暂未实现。
type Scope string

const (
	// ScopeWorkspace 工作区记忆：仅当前项目生效，切换到别的工作目录不会加载
	ScopeWorkspace Scope = "workspace"
)

// 文档结构默认值
const (
	defaultTitle        = "项目记忆"
	defaultSectionTitle = "未分类"
)

// Kind 记忆的语义类型，决定检索重排时的优先级与后续过期策略。
type Kind string

const (
	// KindFact 项目事实，可被新事实推翻
	KindFact Kind = "fact"
	// KindPreference 用户偏好，长期有效
	KindPreference Kind = "preference"
	// KindDecision 已做出的决策，带理由
	KindDecision Kind = "decision"
	// KindLesson 失败教训，长期有效
	KindLesson Kind = "lesson"
	// KindProcedure 操作步骤，容易随版本过期
	KindProcedure Kind = "procedure"
)

// ParseKind 把用户/模型给出的类型字符串解析为 Kind。
// 空串按事实处理；无法识别的取值返回错误，避免把拼写错误静默当成事实。
func ParseKind(raw string) (Kind, error) {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return KindFact, nil
	}
	switch Kind(trimmed) {
	case KindFact, KindPreference, KindDecision, KindLesson, KindProcedure:
		return Kind(trimmed), nil
	default:
		return "", fmt.Errorf("未知的记忆类型 %q，可选值：fact / preference / decision / lesson / procedure", raw)
	}
}

// NormalizeKind 用于内部兜底：空值或非法值一律按事实处理，不返回错误。
func NormalizeKind(raw string) Kind {
	if kind, err := ParseKind(raw); err == nil {
		return kind
	}
	return KindFact
}

// 记忆来源，用于区分人工写入、模型写入与历史数据。
const (
	// SourceUser 用户显式提出或手工编辑
	SourceUser = "user"
	// SourceAgent Agent 通过 remember 工具写入
	SourceAgent = "agent"
)

// Fragment 一条记忆片段，对应 Markdown 文件中的一个列表项，
// 是最小的读写与检索单位。
//
// 除正文外还携带少量可比对的元数据：类型、来源、写入时间与取代关系。
// 元数据以行尾 HTML 注释保存，不破坏人类可读性，也兼容没有元数据的历史条目。
type Fragment struct {
	Scope   Scope  // 所属作用域
	Section string // 所属二级标题，如"架构约定"
	Content string // 记忆正文

	Kind       Kind   // 语义类型；空值表示历史数据，检索时按事实处理
	Source     string // 来源：user / agent；空值表示历史数据
	ID         string // 稳定标识，默认取正文哈希前缀
	UpdatedAt  int64  // 最近写入或确认时间（Unix 秒）；0 表示未知
	Supersedes string // 本条取代的旧记录 ID；空表示没有取代关系
}

// metaComment 渲染行尾元数据注释；没有任何元数据时返回空串。
func (f Fragment) metaComment() string {
	parts := make([]string, 0, 5)
	if f.ID != "" {
		parts = append(parts, "id:"+f.ID)
	}
	if f.Kind != "" {
		parts = append(parts, "kind:"+string(f.Kind))
	}
	if f.Source != "" {
		parts = append(parts, "src:"+f.Source)
	}
	if f.UpdatedAt > 0 {
		parts = append(parts, "at:"+strconv.FormatInt(f.UpdatedAt, 10))
	}
	if f.Supersedes != "" {
		parts = append(parts, "supersedes:"+f.Supersedes)
	}
	if len(parts) == 0 {
		return ""
	}
	return " <!-- " + strings.Join(parts, " ") + " -->"
}

// splitMetadata 把「正文 + 行尾元数据注释」拆开。
//
// 只有注释里出现已知键时才按元数据处理，否则原样保留为正文，
// 避免把用户正文里恰好出现的 HTML 注释误删。
func splitMetadata(content string) (string, map[string]string) {
	trimmed := strings.TrimSpace(content)
	if !strings.HasSuffix(trimmed, "-->") {
		return content, nil
	}
	start := strings.LastIndex(trimmed, "<!--")
	if start < 0 {
		return content, nil
	}
	fields := make(map[string]string)
	for _, part := range strings.Fields(trimmed[start+4 : len(trimmed)-3]) {
		key, value, ok := strings.Cut(part, ":")
		if !ok || value == "" {
			continue
		}
		switch key {
		case "id", "kind", "src", "at", "supersedes":
			fields[key] = value
		}
	}
	if len(fields) == 0 {
		return content, nil
	}
	body := strings.TrimSpace(trimmed[:start])
	if body == "" {
		return content, nil
	}
	return body, fields
}

// Section 一个二级标题及其下的全部记忆片段。
type Section struct {
	Title     string
	Fragments []Fragment
}

// Document 一个完整的记忆文件，由若干章节组成。
type Document struct {
	Title    string
	Sections []Section
}

// ParseDocument 把 Markdown 文本解析为记忆文档。
//
// 解析规则：
//   - "# 标题" 作为文档标题，缺失时使用默认标题
//   - "## 标题" 开启一个新章节
//   - "- " / "* " / "+ " 开头的行作为记忆条目
//   - 其它非空行不会丢弃，同样作为记忆条目归入当前章节，
//     以保证用户手写的自由格式内容不丢失
func ParseDocument(raw string) *Document {
	doc := &Document{Title: defaultTitle}
	hasSection := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "## "):
			doc.Sections = append(doc.Sections, Section{Title: strings.TrimSpace(trimmed[3:])})
			hasSection = true
		case strings.HasPrefix(trimmed, "# "):
			if title := strings.TrimSpace(trimmed[2:]); title != "" {
				doc.Title = title
			}
		default:
			content := stripBullet(trimmed)
			if content == "" {
				continue
			}
			body, meta := splitMetadata(content)
			if !hasSection {
				doc.Sections = append(doc.Sections, Section{Title: defaultSectionTitle})
				hasSection = true
			}
			idx := len(doc.Sections) - 1
			frag := Fragment{
				Scope:   ScopeWorkspace,
				Section: doc.Sections[idx].Title,
				Content: body,
			}
			if meta != nil {
				frag.Kind = NormalizeKind(meta["kind"])
				frag.Source = meta["src"]
				frag.ID = meta["id"]
				frag.Supersedes = meta["supersedes"]
				if at, err := strconv.ParseInt(meta["at"], 10, 64); err == nil {
					frag.UpdatedAt = at
				}
			}
			doc.Sections[idx].Fragments = append(doc.Sections[idx].Fragments, frag)
		}
	}
	return doc
}

// Render 把记忆文档序列化为 Markdown 文本，格式与 ParseDocument 互为逆运算。
func (d *Document) Render() string {
	if d == nil {
		return ""
	}
	title := d.Title
	if title == "" {
		title = defaultTitle
	}
	var sb strings.Builder
	sb.WriteString("# " + title + "\n")
	for _, sec := range d.Sections {
		sb.WriteString("\n")
		sectionTitle := sec.Title
		if sectionTitle == "" {
			sectionTitle = defaultSectionTitle
		}
		sb.WriteString("## " + sectionTitle + "\n")
		for _, f := range sec.Fragments {
			sb.WriteString("- " + f.Content + f.metaComment() + "\n")
		}
	}
	return sb.String()
}

// AllFragments 展开文档中的全部记忆片段，保持文件中的原始顺序。
func (d *Document) AllFragments() []Fragment {
	if d == nil {
		return nil
	}
	fragments := make([]Fragment, 0, len(d.Sections))
	for _, sec := range d.Sections {
		fragments = append(fragments, sec.Fragments...)
	}
	return fragments
}

// Add 向指定章节追加一条记忆。
// 章节不存在时自动创建；若同一章节中已存在完全相同的正文则返回 false，
// 避免自动写入链路反复追加造成记忆膨胀。
func (d *Document) Add(section, content string) bool {
	return d.AddRecord(Fragment{Section: section, Content: content})
}

// AddRecord 追加一条带元数据的记忆。
//
// 与 Add 的区别只在元数据：类型为空时按事实处理，作用域固定为工作区。
// 去重口径仍是「同一章节内正文完全相同」，重复写入直接跳过，
// 避免同一事实因为多次自动写入而膨胀。
func (d *Document) AddRecord(f Fragment) bool {
	section := strings.TrimSpace(f.Section)
	if section == "" {
		section = defaultSectionTitle
	}
	content := strings.TrimSpace(f.Content)
	if content == "" {
		return false
	}
	for i := range d.Sections {
		if d.Sections[i].Title != section {
			continue
		}
		for _, existing := range d.Sections[i].Fragments {
			if existing.Content == content {
				return false
			}
		}
		f.Scope = ScopeWorkspace
		f.Section = section
		f.Content = content
		if f.Kind == "" {
			f.Kind = KindFact
		}
		d.Sections[i].Fragments = append(d.Sections[i].Fragments, f)
		return true
	}
	f.Scope = ScopeWorkspace
	f.Section = section
	f.Content = content
	if f.Kind == "" {
		f.Kind = KindFact
	}
	d.Sections = append(d.Sections, Section{
		Title:     section,
		Fragments: []Fragment{f},
	})
	return true
}

// stripBullet 去掉列表项前缀，返回纯正文。
func stripBullet(line string) string {
	for _, prefix := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return line
}
