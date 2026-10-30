package memory

import (
	"strings"
	"testing"
)

// TestParseDocument 覆盖记忆文件解析的各种输入形态
func TestParseDocument(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantTitle     string
		wantSections  int
		wantFragments int
		wantFirst     Fragment
	}{
		{
			name:          "空文件使用默认标题",
			raw:           "",
			wantTitle:     defaultTitle,
			wantSections:  0,
			wantFragments: 0,
		},
		{
			name:          "标准结构",
			raw:           "# 项目记忆\n\n## 架构约定\n- 严格 DDD 分层\n- domain 层不依赖外部包\n\n## 沟通偏好\n- 优先中文输出\n",
			wantTitle:     "项目记忆",
			wantSections:  2,
			wantFragments: 3,
			wantFirst:     Fragment{Scope: ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层"},
		},
		{
			name:          "章节之前的内容归入未分类",
			raw:           "# 项目记忆\n- 没有章节的记忆\n",
			wantTitle:     "项目记忆",
			wantSections:  1,
			wantFragments: 1,
			wantFirst:     Fragment{Scope: ScopeWorkspace, Section: defaultSectionTitle, Content: "没有章节的记忆"},
		},
		{
			name:          "非列表行不被丢弃",
			raw:           "# 项目记忆\n\n## 架构约定\n这是一段自由格式说明\n",
			wantTitle:     "项目记忆",
			wantSections:  1,
			wantFragments: 1,
			wantFirst:     Fragment{Scope: ScopeWorkspace, Section: "架构约定", Content: "这是一段自由格式说明"},
		},
		{
			name:          "只有标题",
			raw:           "# 我的记忆\n",
			wantTitle:     "我的记忆",
			wantSections:  0,
			wantFragments: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := ParseDocument(tt.raw)
			if doc.Title != tt.wantTitle {
				t.Fatalf("文档标题不匹配: got %q, want %q", doc.Title, tt.wantTitle)
			}
			if len(doc.Sections) != tt.wantSections {
				t.Fatalf("章节数不匹配: got %d, want %d", len(doc.Sections), tt.wantSections)
			}
			fragments := doc.AllFragments()
			if len(fragments) != tt.wantFragments {
				t.Fatalf("记忆条数不匹配: got %d, want %d", len(fragments), tt.wantFragments)
			}
			if tt.wantFragments > 0 && fragments[0] != tt.wantFirst {
				t.Fatalf("首条记忆不匹配: got %+v, want %+v", fragments[0], tt.wantFirst)
			}
		})
	}
}

// TestDocumentRenderRoundTrip 验证解析与序列化互为逆运算
func TestDocumentRenderRoundTrip(t *testing.T) {
	raw := "# 项目记忆\n\n## 架构约定\n- 严格 DDD 分层\n- domain 层不依赖外部包\n\n## 沟通偏好\n- 优先中文输出\n"

	first := ParseDocument(raw)
	second := ParseDocument(first.Render())

	if first.Title != second.Title {
		t.Fatalf("标题在往返后发生变化: %q -> %q", first.Title, second.Title)
	}
	firstFragments := first.AllFragments()
	secondFragments := second.AllFragments()
	if len(firstFragments) != len(secondFragments) {
		t.Fatalf("记忆条数在往返后发生变化: %d -> %d", len(firstFragments), len(secondFragments))
	}
	for i := range firstFragments {
		if firstFragments[i] != secondFragments[i] {
			t.Fatalf("第 %d 条记忆在往返后发生变化: %+v -> %+v", i, firstFragments[i], secondFragments[i])
		}
	}
}

// TestDocumentAdd 验证追加语义：自动建章节、重复内容去重
func TestDocumentAdd(t *testing.T) {
	doc := &Document{Title: defaultTitle}

	if !doc.Add("架构约定", "严格 DDD 分层") {
		t.Fatal("首次写入应成功")
	}
	if doc.Add("架构约定", "严格 DDD 分层") {
		t.Fatal("完全重复的内容应被跳过")
	}
	if !doc.Add("架构约定", "domain 层不依赖外部包") {
		t.Fatal("同章节追加新内容应成功")
	}
	if !doc.Add("沟通偏好", "优先中文输出") {
		t.Fatal("新章节写入应成功")
	}
	if !doc.Add("", "无章节内容") {
		t.Fatal("空章节应自动归入默认章节")
	}

	if len(doc.Sections) != 3 {
		t.Fatalf("章节数不匹配: got %d, want 3", len(doc.Sections))
	}
	if got := doc.Sections[2].Title; got != defaultSectionTitle {
		t.Fatalf("空章节未归入默认章节: got %q", got)
	}
	if got := len(doc.AllFragments()); got != 4 {
		t.Fatalf("记忆条数不匹配: got %d, want 4", got)
	}
}

// TestDocumentMetadataRoundTrip 验证带元数据的记忆可以原样往返。
func TestDocumentMetadataRoundTrip(t *testing.T) {
	doc := &Document{Title: defaultTitle}
	doc.AddRecord(Fragment{
		Section:    "架构约定",
		Content:    "严格 DDD 分层",
		Kind:       KindDecision,
		Source:     SourceAgent,
		ID:         "deadbeef",
		UpdatedAt:  1700000000,
		Supersedes: "cafebabe",
	})

	raw := doc.Render()
	wantComment := "<!-- id:deadbeef kind:decision src:agent at:1700000000 supersedes:cafebabe -->"
	if !strings.Contains(raw, wantComment) {
		t.Fatalf("渲染结果缺少元数据注释，实际:\n%s", raw)
	}

	fragments := ParseDocument(raw).AllFragments()
	if len(fragments) != 1 {
		t.Fatalf("元数据应被解析回单条记忆，实际 %d 条", len(fragments))
	}
	f := fragments[0]
	if f.Content != "严格 DDD 分层" || f.Kind != KindDecision || f.Source != SourceAgent ||
		f.ID != "deadbeef" || f.UpdatedAt != 1700000000 || f.Supersedes != "cafebabe" {
		t.Fatalf("元数据往返后不一致: %+v", f)
	}
}

// TestParseDocumentKeepsPlainComment 验证普通 HTML 注释不会被误当作元数据。
func TestParseDocumentKeepsPlainComment(t *testing.T) {
	raw := "# 项目记忆\n\n## 架构约定\n- 普通内容 <!-- 只是备注 -->\n"
	fragments := ParseDocument(raw).AllFragments()
	if len(fragments) != 1 {
		t.Fatalf("期望 1 条记忆，实际 %d 条", len(fragments))
	}
	if fragments[0].Content != "普通内容 <!-- 只是备注 -->" {
		t.Fatalf("普通注释应保留在正文中，实际 %q", fragments[0].Content)
	}
	if fragments[0].Kind != "" {
		t.Fatalf("普通注释不应产生元数据，实际类型 %q", fragments[0].Kind)
	}
}

// TestAddRecordDefaults 验证带元数据追加的默认值与去重口径。
func TestAddRecordDefaults(t *testing.T) {
	doc := &Document{Title: defaultTitle}
	if !doc.AddRecord(Fragment{Section: "架构约定", Content: "domain 层不依赖外部包"}) {
		t.Fatal("首次写入应成功")
	}
	if doc.AddRecord(Fragment{Section: "架构约定", Content: "domain 层不依赖外部包", Kind: KindDecision}) {
		t.Fatal("同章节同正文应去重")
	}
	f := doc.AllFragments()[0]
	if f.Kind != KindFact {
		t.Fatalf("类型为空时应按事实处理，实际 %q", f.Kind)
	}
	if f.Scope != ScopeWorkspace {
		t.Fatalf("作用域应默认为工作区，实际 %q", f.Scope)
	}
}
