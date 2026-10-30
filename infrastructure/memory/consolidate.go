package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultArchiveAge 流程类记忆的归档年龄。
// 超过该年龄且从未被更新的流程类记忆最容易过期，归档后仍可检索，只是不再默认注入。
const DefaultArchiveAge = 180 * 24 * time.Hour

// archiveDirName 归档目录名，位于记忆目录下。
const archiveDirName = "archive"

// ConsolidateReport 一次记忆整合的结果。
type ConsolidateReport struct {
	Deduped   int `json:"deduped"`   // 跨章节去重掉的重复条目
	Archived  int `json:"archived"`  // 归档的过期流程条目
	Remaining int `json:"remaining"` // 整合后保留的条目数
}

// Consolidate 对记忆文档做确定性整合：跨章节正文去重 + 归档过期流程类记忆。
//
// 只处理可以机器判定的情况，不做语义改写或自动生成新事实：
// 记忆是跨会话的长期事实，自动"润色"比不整合更危险。
// 返回整合后的报告与被归档的条目，调用方负责把归档条目写入归档文件。
func Consolidate(doc *Document, now int64, maxAge time.Duration) (ConsolidateReport, []Fragment) {
	report := ConsolidateReport{}
	if doc == nil {
		return report, nil
	}
	seen := make(map[string]struct{})
	archived := make([]Fragment, 0)
	keptSections := make([]Section, 0, len(doc.Sections))

	for _, section := range doc.Sections {
		kept := make([]Fragment, 0, len(section.Fragments))
		for _, fragment := range section.Fragments {
			// 去重口径：空白归一化后的正文。同一事实写在两个章节也只保留第一条。
			key := normalizeContent(fragment.Content)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				report.Deduped++
				continue
			}
			// 只归档带时间元数据的流程类记忆；历史条目无法判断新旧，原样保留。
			if fragment.Kind == KindProcedure && fragment.UpdatedAt > 0 &&
				now-fragment.UpdatedAt > int64(maxAge/time.Second) {
				seen[key] = struct{}{}
				archived = append(archived, fragment)
				report.Archived++
				continue
			}
			seen[key] = struct{}{}
			kept = append(kept, fragment)
			report.Remaining++
		}
		if len(kept) > 0 {
			section.Fragments = kept
			keptSections = append(keptSections, section)
		}
	}
	doc.Sections = keptSections
	return report, archived
}

// normalizeContent 把正文归一化为去重键：合并连续空白、去掉首尾空白。
func normalizeContent(content string) string {
	return strings.Join(strings.Fields(content), " ")
}

// Consolidate 执行一次记忆整合作业，并把归档条目写入记忆目录下的 archive 子目录。
//
// 顺序很重要：先写归档文件，再原子替换主记忆文件。归档失败时直接返回错误，
// 主文件保持原样，避免"条目既不在主文件也不在归档"的丢失。
func (s *WorkspaceStore) Consolidate(ctx context.Context, now time.Time, maxAge time.Duration) (ConsolidateReport, error) {
	if err := ctx.Err(); err != nil {
		return ConsolidateReport{}, err
	}
	if maxAge <= 0 {
		maxAge = DefaultArchiveAge
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.loadLocked()
	if err != nil {
		return ConsolidateReport{}, err
	}
	report, archived := Consolidate(doc, now.Unix(), maxAge)
	if report.Deduped == 0 && report.Archived == 0 {
		return report, nil
	}
	if len(archived) > 0 {
		if err := s.writeArchive(now, archived); err != nil {
			return ConsolidateReport{}, err
		}
	}
	if err := s.writeAtomic(doc.Render()); err != nil {
		return ConsolidateReport{}, err
	}
	return report, nil
}

// writeArchive 把归档条目写入带时间戳的归档文件，便于人工回溯。
func (s *WorkspaceStore) writeArchive(now time.Time, fragments []Fragment) error {
	dir := filepath.Join(filepath.Dir(s.path), archiveDirName)
	if err := os.MkdirAll(dir, memoryDirPerm); err != nil {
		return fmt.Errorf("创建归档目录失败: %w", err)
	}
	archive := &Document{
		Title: "归档记忆",
		Sections: []Section{{
			Title:     "已归档",
			Fragments: fragments,
		}},
	}
	name := fmt.Sprintf("MEMORY-%s.md", now.Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(archive.Render()), memoryFilePerm); err != nil {
		return fmt.Errorf("写入归档文件失败: %w", err)
	}
	return nil
}
