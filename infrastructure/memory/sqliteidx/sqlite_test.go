package sqliteidx

import (
	"context"
	"testing"

	"simpleAgent/infrastructure/memory"
)

// sampleFragments 测试用的记忆集合
var sampleFragments = []memory.Fragment{
	{Scope: memory.ScopeWorkspace, Section: "架构约定", Content: "严格 DDD 分层，domain 层不依赖外部包"},
	{Scope: memory.ScopeWorkspace, Section: "沟通偏好", Content: "优先中文输出，代码注释使用中文"},
	{Scope: memory.ScopeWorkspace, Section: "交付规范", Content: "提交前执行 go test ./..."},
}

// newTestIndex 在临时目录中创建索引，并在用例结束后关闭
func newTestIndex(t *testing.T) *Index {
	t.Helper()
	index, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("关闭索引失败: %v", err)
		}
	})
	return index
}

// TestNewRejectsEmptyDir 空目录属于非法入参
func TestNewRejectsEmptyDir(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("索引目录为空时应返回错误")
	}
}

// TestNewIsIdempotent 重复打开同一个索引文件不应报错，也不能丢数据
func TestNewIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := New(dir)
	if err != nil {
		t.Fatalf("首次打开索引失败: %v", err)
	}
	if err := first.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("关闭索引失败: %v", err)
	}

	second, err := New(dir)
	if err != nil {
		t.Fatalf("重复打开索引失败: %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("关闭索引失败: %v", err)
		}
	}()

	records, err := second.KeywordSearch(ctx, "架构约定", 5)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("重开索引后数据应保留, got %d 条", len(records))
	}
}

// TestSyncAndKeywordSearch 关键词检索应命中中文，并带上来源信息
func TestSyncAndKeywordSearch(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)

	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	records, err := index.KeywordSearch(ctx, "架构约定", 5)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("期望命中 1 条, got %d", len(records))
	}
	if records[0].Section != "架构约定" {
		t.Fatalf("命中章节不匹配: got %q", records[0].Section)
	}
	if records[0].Source != "MEMORY.md" {
		t.Fatalf("来源标识不匹配: got %q", records[0].Source)
	}
}

// TestKeywordSearchDoesNotIndexQuerySyntax 写入索引的必须是纯词元串，
// 否则 OR 会被当成词条，导致查询 "or" 时误命中全部记忆。
func TestKeywordSearchDoesNotIndexQuerySyntax(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)

	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	records, err := index.KeywordSearch(ctx, "or", 10)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("查询语法词不应命中记忆, got %d 条", len(records))
	}
}

// TestKeywordSearchEdgeCases 空查询与非法 topK 应直接返回空结果
func TestKeywordSearchEdgeCases(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)
	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	if got, err := index.KeywordSearch(ctx, "a b", 5); err != nil || got != nil {
		t.Fatalf("无可用词元时应返回空: got %+v, err=%v", got, err)
	}
	if got, err := index.KeywordSearch(ctx, "架构", 0); err != nil || got != nil {
		t.Fatalf("topK 为 0 时应返回空: got %+v, err=%v", got, err)
	}
}

// TestSyncSkipsRebuildWhenContentUnchanged 内容未变化时不应重建索引
func TestSyncSkipsRebuildWhenContentUnchanged(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)

	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	// 把同步时间戳改写为哨兵值：若第二次 Sync 真的跳过了重建，该值不会被覆盖
	if _, err := index.db.ExecContext(ctx, `UPDATE memories SET synced_at = 0`); err != nil {
		t.Fatalf("改写同步时间戳失败: %v", err)
	}
	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("重复同步失败: %v", err)
	}

	var untouched int
	if err := index.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE synced_at = 0`).Scan(&untouched); err != nil {
		t.Fatalf("统计同步时间戳失败: %v", err)
	}
	if untouched != len(sampleFragments) {
		t.Fatalf("内容未变化时不应重建索引, 剩余哨兵记录 %d 条", untouched)
	}
}

// TestSyncRebuildsOnContentChange 真源删除记忆后，索引与检索结果都要同步删除
func TestSyncRebuildsOnContentChange(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)

	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	if err := index.Sync(ctx, "MEMORY.md", sampleFragments[:2]); err != nil {
		t.Fatalf("同步删减后的记忆失败: %v", err)
	}

	var count int
	if err := index.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
		t.Fatalf("统计记忆条数失败: %v", err)
	}
	if count != 2 {
		t.Fatalf("索引条数应跟随真源, got %d, want 2", count)
	}

	records, err := index.KeywordSearch(ctx, "提交前执行", 5)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("已删除的记忆不应被检索到, got %d 条", len(records))
	}
}

// TestSyncRejectsCancelledContext 已取消的 ctx 必须显式返回错误
func TestSyncRejectsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := newTestIndex(t).Sync(ctx, "MEMORY.md", sampleFragments); err == nil {
		t.Fatal("ctx 已取消时应返回错误")
	}
}

// TestPendingTextsAndSaveVectors 向量写入后不应再出现在待处理列表
func TestPendingTextsAndSaveVectors(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)
	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	pending, err := index.PendingTexts(ctx, "test-model")
	if err != nil {
		t.Fatalf("查询待向量化记忆失败: %v", err)
	}
	if len(pending) != len(sampleFragments) {
		t.Fatalf("待向量化数量不匹配: got %d, want %d", len(pending), len(sampleFragments))
	}
	for _, p := range pending {
		if p.Hash == "" || p.Text == "" {
			t.Fatalf("待向量化项不完整: %+v", p)
		}
	}

	// 用显式哈希构造向量，避免依赖 PendingTexts 的返回顺序
	vectors := map[string][]float32{
		memory.TextHash(sampleFragments[0].Section, sampleFragments[0].Content): {1, 0},
		memory.TextHash(sampleFragments[1].Section, sampleFragments[1].Content): {0, 1},
		memory.TextHash(sampleFragments[2].Section, sampleFragments[2].Content): {-1, 0},
	}
	if err := index.SaveVectors(ctx, "test-model", vectors); err != nil {
		t.Fatalf("保存向量失败: %v", err)
	}

	remaining, err := index.PendingTexts(ctx, "test-model")
	if err != nil {
		t.Fatalf("查询待向量化记忆失败: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("向量已齐全，不应再有待处理项: got %d", len(remaining))
	}

	// 另一个模型名下仍然是没有向量的，说明向量按模型隔离
	otherModel, err := index.PendingTexts(ctx, "other-model")
	if err != nil {
		t.Fatalf("查询待向量化记忆失败: %v", err)
	}
	if len(otherModel) != len(sampleFragments) {
		t.Fatalf("向量应按模型隔离, got %d, want %d", len(otherModel), len(sampleFragments))
	}
}

// TestVectorSearch 按余弦相似度降序返回，并处理维度不一致的情况
func TestVectorSearch(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)
	if err := index.Sync(ctx, "MEMORY.md", sampleFragments); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	vectors := map[string][]float32{
		memory.TextHash(sampleFragments[0].Section, sampleFragments[0].Content): {1, 0},  // 与查询同向
		memory.TextHash(sampleFragments[1].Section, sampleFragments[1].Content): {0, 1},  // 与查询正交
		memory.TextHash(sampleFragments[2].Section, sampleFragments[2].Content): {-1, 0}, // 与查询反向
	}
	if err := index.SaveVectors(ctx, "test-model", vectors); err != nil {
		t.Fatalf("保存向量失败: %v", err)
	}

	records, err := index.VectorSearch(ctx, "test-model", []float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("向量检索失败: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("期望返回 2 条, got %d", len(records))
	}
	if records[0].Content != sampleFragments[0].Content {
		t.Fatalf("最相似的记忆应排首位, got %q", records[0].Content)
	}
	if records[1].Content != sampleFragments[1].Content {
		t.Fatalf("第二相似的记忆不匹配, got %q", records[1].Content)
	}

	// 维度不一致（通常是换过 embedding 模型）的记忆应被跳过而不是报错
	mismatched, err := index.VectorSearch(ctx, "test-model", []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("维度不一致时不应报错: %v", err)
	}
	if len(mismatched) != 0 {
		t.Fatalf("维度不一致的记录应被跳过, got %d 条", len(mismatched))
	}
}

// TestSaveVectorsValidatesInput 非法入参必须返回错误
func TestSaveVectorsValidatesInput(t *testing.T) {
	ctx := context.Background()
	index := newTestIndex(t)

	tests := []struct {
		name    string
		model   string
		vectors map[string][]float32
	}{
		{"模型名为空", "", map[string][]float32{"hash": {1}}},
		{"哈希为空", "m", map[string][]float32{"": {1}}},
		{"向量为空", "m", map[string][]float32{"hash": {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := index.SaveVectors(ctx, tt.model, tt.vectors); err == nil {
				t.Fatal("参数不合法时应返回错误")
			}
		})
	}

	// 空 map 属于无操作，不应报错
	if err := index.SaveVectors(ctx, "m", nil); err != nil {
		t.Fatalf("空向量集合不应报错: %v", err)
	}
}

// TestPendingTextsRejectsEmptyModel 模型名是向量表的主键之一，不能为空
func TestPendingTextsRejectsEmptyModel(t *testing.T) {
	if _, err := newTestIndex(t).PendingTexts(context.Background(), ""); err == nil {
		t.Fatal("模型名为空时应返回错误")
	}
}

// TestVectorSearchRejectsEmptyModel 模型名非法时不应静默返回空结果
func TestVectorSearchRejectsEmptyModel(t *testing.T) {
	if _, err := newTestIndex(t).VectorSearch(context.Background(), "", []float32{1}, 5); err == nil {
		t.Fatal("模型名为空时应返回错误")
	}
}
