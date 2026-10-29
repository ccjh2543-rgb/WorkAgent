// 任务集覆盖度分析：在真正跑评测之前先回答“50 个任务是否都能被执行”。
package eval

import (
	"sort"
	"strings"
)

// CoverageIssue 一个无法执行的任务及其原因。
type CoverageIssue struct {
	TaskID string // 任务 ID
	Reason string // 读取失败原因（缺少契约文件、格式非法等）
}

// CoverageSummary 任务集覆盖度统计结果。
type CoverageSummary struct {
	Total         int             // 任务总数
	Valid         int             // 可执行任务数
	Invalid       int             // 不可执行任务数
	Issues        []CoverageIssue // 不可执行任务明细
	ArtifactTypes map[string]int  // 产物类型 → 需要该类型的任务数（按任务去重）
	// MissingArtifactTypes 需要产出但当前写能力不覆盖的类型，按字典序去重。
	MissingArtifactTypes []string
}

// AnalyzeCoverage 统计任务集可执行性与交付能力缺口。
//
// writable 由调用方注入“当前真正支持的写格式”，本层不硬编码 Agent 能力；
// 产物类型按任务去重统计，避免同一任务多个同类产物放大数量。
func AnalyzeCoverage(tasks []TaskSpec, writable []string) CoverageSummary {
	writeSet := make(map[string]bool, len(writable))
	for _, t := range writable {
		if k := normalizeArtifactType(t); k != "" {
			writeSet[k] = true
		}
	}
	summary := CoverageSummary{
		Total:         len(tasks),
		Issues:        make([]CoverageIssue, 0),
		ArtifactTypes: make(map[string]int),
	}
	missing := make(map[string]bool)
	for _, task := range tasks {
		if strings.TrimSpace(task.LoadErr) != "" {
			summary.Invalid++
			summary.Issues = append(summary.Issues, CoverageIssue{TaskID: task.ID, Reason: task.LoadErr})
			continue
		}
		summary.Valid++
		seen := make(map[string]bool)
		for _, art := range task.Artifacts {
			kind := normalizeArtifactType(art.Type)
			if kind == "" || seen[kind] {
				continue
			}
			seen[kind] = true
			summary.ArtifactTypes[kind]++
			if !writeSet[kind] {
				missing[kind] = true
			}
		}
	}
	for kind := range missing {
		summary.MissingArtifactTypes = append(summary.MissingArtifactTypes, kind)
	}
	sort.Strings(summary.MissingArtifactTypes)
	return summary
}

// normalizeArtifactType 归一化产物类型：去空白、去点、小写。
func normalizeArtifactType(t string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(t), "."))
}
