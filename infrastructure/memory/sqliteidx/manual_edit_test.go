package sqliteidx

import (
	"context"
	"crypto/sha256"
	"os"
	"strings"
	"testing"

	"simpleAgent/infrastructure/memory"
)

// stubEmbedder 返回由文本哈希派生的确定性向量，避免流程测试依赖外部 embedding 服务
type stubEmbedder struct{}

func (stubEmbedder) Model() string { return "stub-model" }

func (stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		sum := sha256.Sum256([]byte(text))
		vectors[i] = []float32{float32(sum[0]), float32(sum[1]), float32(sum[2])}
	}
	return vectors, nil
}

// 统计用的固定 SQL 语句，测试中也保持参数化与常量化的查询写法
var (
	countMemoriesSQL = `SELECT COUNT(*) FROM memories`
	countFTSSQL      = `SELECT COUNT(*) FROM memories_fts`
	countVectorsSQL  = `SELECT COUNT(*) FROM embeddings`
	countOrphanSQL   = `SELECT COUNT(*) FROM embeddings
		WHERE text_hash NOT IN (SELECT text_hash FROM memories)`
)

// TestManualEditTakesEffectOnNextSearch 验证「手动编辑 MEMORY.md 后，下一次检索即生效」。
// 这是真源 + 派生索引架构的核心不变式：索引必须始终跟随真源，
// 覆盖新增、删除、清空三种手动编辑场景。
func TestManualEditTakesEffectOnNextSearch(t *testing.T) {
	ctx := context.Background()
	store := memory.NewWorkspaceStore(t.TempDir())

	index, err := New(store.Dir())
	if err != nil {
		t.Fatalf("创建索引失败: %v", err)
	}
	t.Cleanup(func() {
		if err := index.Close(); err != nil {
			t.Errorf("关闭索引失败: %v", err)
		}
	})

	retriever, err := memory.NewHybridRetriever(index, stubEmbedder{}, store.Path())
	if err != nil {
		t.Fatalf("创建混合检索器失败: %v", err)
	}
	loader := memory.NewLoader(store, retriever, 0, 0)

	readBlock := func(query string) string {
		t.Helper()
		block, loadErr := loader.BuildContext(ctx, query)
		if loadErr != nil {
			t.Fatalf("装配记忆失败: %v", loadErr)
		}
		return block
	}
	countRows := func(sqlText string) int {
		t.Helper()
		var count int
		if queryErr := index.db.QueryRowContext(ctx, sqlText).Scan(&count); queryErr != nil {
			t.Fatalf("统计行数失败: %v", queryErr)
		}
		return count
	}

	// 第一步：通过正常写入路径建立初始记忆
	initial := []memory.Fragment{
		{Section: "安全要求", Content: "禁止在代码里硬编码密钥和 token"},
		{Section: "沟通偏好", Content: "优先中文输出"},
	}
	for _, f := range initial {
		if err := store.Append(ctx, f.Section, f.Content); err != nil {
			t.Fatalf("写入记忆失败: %v", err)
		}
	}
	first := readBlock("安全要求")
	if !strings.Contains(first, "禁止在代码里硬编码密钥") {
		t.Fatalf("初始记忆应被注入, got:\n%s", first)
	}
	if got := countRows(countMemoriesSQL); got != len(initial) {
		t.Fatalf("索引条数不匹配: got %d, want %d", got, len(initial))
	}

	// 第二步：模拟用户直接用编辑器改写 MEMORY.md —— 删掉「安全要求」，新增「性能要求」
	edited := "# 项目记忆\n\n## 沟通偏好\n- 优先中文输出\n\n## 性能要求\n- 单次检索延迟低于 200ms\n"
	if err := os.WriteFile(store.Path(), []byte(edited), 0o640); err != nil {
		t.Fatalf("手动改写记忆文件失败: %v", err)
	}

	second := readBlock("性能要求")
	if !strings.Contains(second, "单次检索延迟低于 200ms") {
		t.Fatalf("手动新增的记忆应立即生效, got:\n%s", second)
	}
	if strings.Contains(second, "禁止在代码里硬编码密钥") {
		t.Fatalf("手动删除的记忆不应再被注入, got:\n%s", second)
	}
	if got := countRows(countMemoriesSQL); got != 2 {
		t.Fatalf("索引应跟随真源更新, got %d 条, want 2", got)
	}
	if got := countRows(countFTSSQL); got != 2 {
		t.Fatalf("关键词索引应跟随真源更新, got %d 条, want 2", got)
	}
	// 已删除记忆的向量也必须清理，否则索引里会残留无意义的数据
	if got := countRows(countOrphanSQL); got != 0 {
		t.Fatalf("已删除记忆的向量应被清理, got %d 条", got)
	}
	// 被删除的记忆不能通过索引被检索到。
	// 这里必须用被删记忆独有的短语：用「安全要求」会命中新增章节「性能要求」里的「要求」二字，
	// 那属于正常的关键词模糊匹配，不是索引残留。
	deleted, err := index.KeywordSearch(ctx, "禁止在代码里硬编码密钥", 5)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(deleted) != 0 {
		t.Fatalf("已删除的记忆不应被检索到, got %d 条", len(deleted))
	}

	// 第三步：用户把记忆全部清空，索引也必须清空，且不再注入任何记忆
	if err := os.WriteFile(store.Path(), nil, 0o640); err != nil {
		t.Fatalf("清空记忆文件失败: %v", err)
	}
	if block := readBlock("任意问题"); block != "" {
		t.Fatalf("真源清空后不应注入记忆, got:\n%s", block)
	}
	if got := countRows(countMemoriesSQL); got != 0 {
		t.Fatalf("真源清空后索引应被清空, got %d 条", got)
	}
	if got := countRows(countVectorsSQL); got != 0 {
		t.Fatalf("真源清空后向量应被清空, got %d 条", got)
	}
}

// TestSyncToleratesFormatOnlyEdit 仅调整排版（如重排章节顺序外的空白、标题）不应报错
func TestSyncToleratesFormatOnlyEdit(t *testing.T) {
	ctx := context.Background()
	store := memory.NewWorkspaceStore(t.TempDir())
	if err := store.Append(ctx, "架构约定", "严格 DDD 分层"); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	index := newTestIndex(t)
	doc, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if err := index.Sync(ctx, store.Path(), doc.AllFragments()); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	// 只改标题，不改任何记忆正文
	if err := os.WriteFile(store.Path(), []byte("# 我的项目记忆\n\n## 架构约定\n- 严格 DDD 分层\n"), 0o640); err != nil {
		t.Fatalf("改写记忆文件失败: %v", err)
	}
	edited, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("读取记忆失败: %v", err)
	}
	if err := index.Sync(ctx, store.Path(), edited.AllFragments()); err != nil {
		t.Fatalf("同步索引失败: %v", err)
	}

	records, err := index.KeywordSearch(ctx, "架构约定", 5)
	if err != nil {
		t.Fatalf("关键词检索失败: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("记忆集合未变，索引内容应保留, got %d 条", len(records))
	}
}

// TestCloseIsIdempotentOnNilIndex 未初始化时关闭不应 panic
func TestCloseIsIdempotentOnNilIndex(t *testing.T) {
	var index *Index
	if err := index.Close(); err != nil {
		t.Fatalf("nil 索引关闭应返回 nil, got %v", err)
	}
}
