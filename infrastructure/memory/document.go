// Package memory 实现工作区级持久记忆：Markdown 记忆文件的解析、读写、
// 相关性检索，以及注入 Prompt 前的上下文装配。
//
// 记忆以人类可读、可直接编辑的 Markdown 文件保存，默认路径为
// <项目根目录>/.simpleagent/memory/MEMORY.md，因此记忆不是黑盒数据，
// 用户可以随时打开文件增删改条目。
package memory

import "strings"

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

// Fragment 一条记忆片段，对应 Markdown 文件中的一个列表项，
// 是最小的读写与检索单位。
type Fragment struct {
	Scope   Scope  // 所属作用域
	Section string // 所属二级标题，如"架构约定"
	Content string // 记忆正文
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
			if !hasSection {
				doc.Sections = append(doc.Sections, Section{Title: defaultSectionTitle})
				hasSection = true
			}
			idx := len(doc.Sections) - 1
			doc.Sections[idx].Fragments = append(doc.Sections[idx].Fragments, Fragment{
				Scope:   ScopeWorkspace,
				Section: doc.Sections[idx].Title,
				Content: content,
			})
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
			sb.WriteString("- " + f.Content + "\n")
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
	if section == "" {
		section = defaultSectionTitle
	}
	for i := range d.Sections {
		if d.Sections[i].Title != section {
			continue
		}
		for _, f := range d.Sections[i].Fragments {
			if f.Content == content {
				return false
			}
		}
		d.Sections[i].Fragments = append(d.Sections[i].Fragments, Fragment{
			Scope:   ScopeWorkspace,
			Section: section,
			Content: content,
		})
		return true
	}
	d.Sections = append(d.Sections, Section{
		Title: section,
		Fragments: []Fragment{{
			Scope:   ScopeWorkspace,
			Section: section,
			Content: content,
		}},
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
