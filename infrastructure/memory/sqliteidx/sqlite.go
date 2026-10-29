// Package sqliteidx 基于 SQLite 实现 memory.Index。
//
// 设计取舍：
//   - 关键词检索走 FTS5。FTS5 的 unicode61 分词器不切分中文，因此中文按
//     二元字组预处理后再写入索引列，查询时用同样的方式构造 MATCH 串。
//   - 向量检索不使用 sqlite-vec 等 C 扩展，而是在 Go 侧暴力计算余弦相似度。
//     记忆条目通常在几百到几千条量级，全量扫描的开销远小于引入扩展的代价。
//   - 索引是 Markdown 真源的派生数据，结构不兼容时直接丢弃重建。
package sqliteidx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	// 纯 Go 的 SQLite 驱动，无需 CGO
	_ "modernc.org/sqlite"

	"simpleAgent/infrastructure/memory"
)

const (
	// schemaVersion 索引结构版本，与库中记录不一致时丢弃整个索引重建
	schemaVersion = "1"
	// dbFileName 索引文件名，与 MEMORY.md 同目录
	dbFileName = "index.db"
	// indexDirPerm 索引目录权限
	indexDirPerm = 0o750
	// busyTimeoutMS 数据库繁忙时的等待时间，避免并发访问直接失败
	busyTimeoutMS = 5000
)

// schema 语句在首次建库或重建时执行
var schema = []string{
	`CREATE TABLE IF NOT EXISTS memories (
		text_hash TEXT PRIMARY KEY,
		section   TEXT NOT NULL,
		content   TEXT NOT NULL,
		source    TEXT NOT NULL,
		synced_at INTEGER NOT NULL
	)`,
	`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
		tokens,
		text_hash UNINDEXED,
		tokenize = 'unicode61'
	)`,
	`CREATE TABLE IF NOT EXISTS embeddings (
		text_hash TEXT NOT NULL,
		model     TEXT NOT NULL,
		dim       INTEGER NOT NULL,
		vector    BLOB NOT NULL,
		PRIMARY KEY (text_hash, model)
	)`,
}

// Index 基于 SQLite 的记忆索引实现。
//
// 并发安全：公开方法由互斥锁串行化。Sync 内部是「读集合 → 比较 → 整体重建」
// 的逻辑事务，仅靠 database/sql 的连接池无法保证互斥，因此这里显式加锁；
// 同时把连接数限制为 1，避免 SQLite 单文件写入产生 SQLITE_BUSY。
type Index struct {
	db *sql.DB
	mu sync.Mutex
}

// New 打开或创建索引。dir 为索引所在目录（通常与 MEMORY.md 同级），
// 目录不存在时自动创建。
func New(dir string) (*Index, error) {
	if dir == "" {
		return nil, errors.New("记忆索引目录不能为空")
	}
	if err := os.MkdirAll(dir, indexDirPerm); err != nil {
		return nil, fmt.Errorf("创建记忆索引目录失败: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)",
		filepath.Join(dir, dbFileName), busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开记忆索引失败: %w", err)
	}
	db.SetMaxOpenConns(1)

	index := &Index{db: db}
	if err := index.migrate(); err != nil {
		// 迁移失败时必须关闭已打开的连接，否则文件句柄泄漏
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

// Sync 用真源解析出的记忆列表刷新索引。
// 先比较记忆哈希集合，集合一致时直接返回，避免每次检索都重建索引。
func (i *Index) Sync(ctx context.Context, source string, fragments []memory.Fragment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	defer i.mu.Unlock()

	want := make([]string, 0, len(fragments))
	deduped := make([]memory.Fragment, 0, len(fragments))
	seen := make(map[string]struct{}, len(fragments))
	for _, f := range fragments {
		hash := memory.TextHash(f.Section, f.Content)
		if _, ok := seen[hash]; ok {
			continue
		}
		seen[hash] = struct{}{}
		want = append(want, hash)
		deduped = append(deduped, f)
	}
	sort.Strings(want)

	has, err := i.storedHashes(ctx)
	if err != nil {
		return err
	}
	if slices.Equal(has, want) {
		return nil
	}
	return i.rebuild(ctx, source, deduped)
}

// PendingTexts 返回当前模型下还没有向量的记忆
func (i *Index) PendingTexts(ctx context.Context, model string) ([]memory.PendingText, error) {
	if model == "" {
		return nil, errors.New("embedding 模型名不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := i.db.QueryContext(ctx, `
		SELECT m.section, m.content
		FROM memories m
		LEFT JOIN embeddings e ON e.text_hash = m.text_hash AND e.model = ?
		WHERE e.text_hash IS NULL
		ORDER BY m.rowid`, model)
	if err != nil {
		return nil, fmt.Errorf("查询待向量化记忆失败: %w", err)
	}
	defer rows.Close()

	pending := make([]memory.PendingText, 0, 16)
	for rows.Next() {
		var section, content string
		if err := rows.Scan(&section, &content); err != nil {
			return nil, fmt.Errorf("解析待向量化记忆失败: %w", err)
		}
		pending = append(pending, memory.PendingText{
			Hash: memory.TextHash(section, content),
			Text: memory.EmbedText(section, content),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历待向量化记忆失败: %w", err)
	}
	return pending, nil
}

// SaveVectors 批量保存向量，map 的 key 为 PendingText.Hash
func (i *Index) SaveVectors(ctx context.Context, model string, vectors map[string][]float32) error {
	if model == "" {
		return errors.New("embedding 模型名不能为空")
	}
	if len(vectors) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// 先整体校验再写入，避免部分写入造成索引状态不一致
	for hash, vec := range vectors {
		if hash == "" {
			return errors.New("向量对应的记忆哈希不能为空")
		}
		if len(vec) == 0 {
			return fmt.Errorf("记忆向量为空: hash=%s", hash)
		}
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启向量写入事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	for hash, vec := range vectors {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO embeddings(text_hash, model, dim, vector) VALUES(?, ?, ?, ?)
			ON CONFLICT(text_hash, model) DO UPDATE SET dim = excluded.dim, vector = excluded.vector`,
			hash, model, len(vec), memory.EncodeVector(vec)); err != nil {
			return fmt.Errorf("写入记忆向量失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交向量写入事务失败: %w", err)
	}
	return nil
}

// KeywordSearch 关键词检索，按 FTS5 的 bm25 相关度降序返回
func (i *Index) KeywordSearch(ctx context.Context, query string, topK int) ([]memory.MemoryRecord, error) {
	if topK <= 0 {
		return nil, nil
	}
	match := memory.MatchQuery(query)
	if match == "" {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// bm25() 越小越相关，取负后变为越大越相关，便于统一按降序排序
	rows, err := i.db.QueryContext(ctx, `
		SELECT m.section, m.content, m.source, -bm25(memories_fts) AS score
		FROM memories_fts
		JOIN memories m ON m.text_hash = memories_fts.text_hash
		WHERE memories_fts MATCH ?
		ORDER BY score DESC
		LIMIT ?`, match, topK)
	if err != nil {
		return nil, fmt.Errorf("关键词检索失败: %w", err)
	}
	defer rows.Close()

	records := make([]memory.MemoryRecord, 0, topK)
	for rows.Next() {
		var (
			section, content, source string
			score                    float64
		)
		if err := rows.Scan(&section, &content, &source, &score); err != nil {
			return nil, fmt.Errorf("解析关键词检索结果失败: %w", err)
		}
		records = append(records, newRecord(section, content, source, score))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历关键词检索结果失败: %w", err)
	}
	return records, nil
}

// VectorSearch 向量检索，在 Go 侧全量计算余弦相似度后取 topK。
// 维度与查询向量不一致的记忆（通常来自换过 embedding 模型）会被跳过。
func (i *Index) VectorSearch(ctx context.Context, model string, queryVector []float32, topK int) ([]memory.MemoryRecord, error) {
	if model == "" {
		return nil, errors.New("embedding 模型名不能为空")
	}
	if topK <= 0 || len(queryVector) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rows, err := i.db.QueryContext(ctx, `
		SELECT m.section, m.content, m.source, e.vector
		FROM embeddings e
		JOIN memories m ON m.text_hash = e.text_hash
		WHERE e.model = ?`, model)
	if err != nil {
		return nil, fmt.Errorf("读取向量索引失败: %w", err)
	}
	defer rows.Close()

	records := make([]memory.MemoryRecord, 0, 64)
	for rows.Next() {
		var (
			section, content, source string
			blob                     []byte
		)
		if err := rows.Scan(&section, &content, &source, &blob); err != nil {
			return nil, fmt.Errorf("解析向量索引记录失败: %w", err)
		}
		vector, err := memory.DecodeVector(blob)
		if err != nil {
			return nil, fmt.Errorf("解码记忆向量失败: %w", err)
		}
		if len(vector) != len(queryVector) {
			continue
		}
		records = append(records, newRecord(section, content, source,
			float64(memory.CosineSimilarity(queryVector, vector))))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历向量索引失败: %w", err)
	}

	sort.SliceStable(records, func(a, b int) bool {
		return records[a].Score > records[b].Score
	})
	if len(records) > topK {
		records = records[:topK]
	}
	return records, nil
}

// Close 关闭底层数据库连接
func (i *Index) Close() error {
	if i == nil || i.db == nil {
		return nil
	}
	return i.db.Close()
}

// storedHashes 读取索引中已存在的记忆哈希集合（升序）
func (i *Index) storedHashes(ctx context.Context) ([]string, error) {
	rows, err := i.db.QueryContext(ctx, `SELECT text_hash FROM memories ORDER BY text_hash`)
	if err != nil {
		return nil, fmt.Errorf("读取索引哈希失败: %w", err)
	}
	defer rows.Close()

	hashes := make([]string, 0, 64)
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, fmt.Errorf("解析索引哈希失败: %w", err)
		}
		hashes = append(hashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历索引哈希失败: %w", err)
	}
	return hashes, nil
}

// rebuild 整体重建索引。记忆条数在千级以内，整体重建比增量 diff 更简单可靠。
func (i *Index) rebuild(ctx context.Context, source string, fragments []memory.Fragment) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启索引重建事务失败: %w", err)
	}
	// 提交成功后 Rollback 返回 ErrTxDone，可安全忽略
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `DELETE FROM memories`); err != nil {
		return fmt.Errorf("清空记忆索引失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memories_fts`); err != nil {
		return fmt.Errorf("清空关键词索引失败: %w", err)
	}

	now := time.Now().Unix()
	for _, f := range fragments {
		hash := memory.TextHash(f.Section, f.Content)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memories(text_hash, section, content, source, synced_at) VALUES(?, ?, ?, ?, ?)`,
			hash, f.Section, f.Content, source, now); err != nil {
			return fmt.Errorf("写入记忆索引失败: %w", err)
		}
		tokens := memory.TokenText(memory.EmbedText(f.Section, f.Content))
		if tokens == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO memories_fts(tokens, text_hash) VALUES(?, ?)`, tokens, hash); err != nil {
			return fmt.Errorf("写入关键词索引失败: %w", err)
		}
	}

	// 真源中已删除的记忆，其向量一并清理，避免索引残留与无意义的外部调用
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM embeddings WHERE text_hash NOT IN (SELECT text_hash FROM memories)`); err != nil {
		return fmt.Errorf("清理失效向量失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交索引重建事务失败: %w", err)
	}
	return nil
}

// migrate 建表并在结构版本不匹配时丢弃重建
func (i *Index) migrate() error {
	if _, err := i.db.Exec(`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("创建索引元数据表失败: %w", err)
	}

	var version string
	err := i.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	switch {
	case err == nil && version != schemaVersion:
		// 结构版本不一致：索引可由真源重建，直接丢弃最省事也最安全
		if err := i.dropAll(); err != nil {
			return err
		}
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("读取索引结构版本失败: %w", err)
	}

	for _, stmt := range schema {
		if _, err := i.db.Exec(stmt); err != nil {
			return fmt.Errorf("初始化索引结构失败: %w", err)
		}
	}
	if _, err := i.db.Exec(`
		INSERT INTO meta(key, value) VALUES('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, schemaVersion); err != nil {
		return fmt.Errorf("写入索引结构版本失败: %w", err)
	}
	return nil
}

// dropAll 丢弃全部索引对象
func (i *Index) dropAll() error {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS memories`,
		`DROP TABLE IF EXISTS memories_fts`,
		`DROP TABLE IF EXISTS embeddings`,
		`DROP TABLE IF EXISTS meta`,
	} {
		if _, err := i.db.Exec(stmt); err != nil {
			return fmt.Errorf("丢弃旧索引结构失败: %w", err)
		}
	}
	return nil
}

// newRecord 组装索引返回的记忆记录
func newRecord(section, content, source string, score float64) memory.MemoryRecord {
	return memory.MemoryRecord{
		Fragment: memory.Fragment{
			Scope:   memory.ScopeWorkspace,
			Section: section,
			Content: content,
		},
		Source: source,
		Score:  score,
	}
}
