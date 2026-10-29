// Package evalstore 负责把评测报告与汇总落盘到运行目录。
//
// 目录布局：<root>/<run_id>/<task_id>/<mode>/report.json
//
//	<root>/<run_id>/summary.json
package evalstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	appeval "simpleAgent/application/eval"
	domaineval "simpleAgent/domain/eval"
)

// FileStore 基于文件系统的报告仓储。
type FileStore struct {
	Root string
}

// New 创建文件仓储，root 为运行根目录。
func New(root string) *FileStore {
	return &FileStore{Root: root}
}

// Save 写入单任务报告。
func (s *FileStore) Save(ctx context.Context, report domaineval.Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := reportPath(s.Root, report.RunID, report.TaskID, report.Mode)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, report)
}

// Load 读取单任务报告。
func (s *FileStore) Load(ctx context.Context, runID, taskID, mode string) (domaineval.Report, error) {
	if err := ctx.Err(); err != nil {
		return domaineval.Report{}, err
	}
	data, err := os.ReadFile(reportPath(s.Root, runID, taskID, mode))
	if err != nil {
		return domaineval.Report{}, fmt.Errorf("读取报告失败: %w", err)
	}
	var report domaineval.Report
	if err := json.Unmarshal(data, &report); err != nil {
		return domaineval.Report{}, fmt.Errorf("解析报告失败: %w", err)
	}
	return report, nil
}

// List 列出某个运行下的全部报告，按任务 ID 与模式排序。
func (s *FileStore) List(ctx context.Context, runID string) ([]domaineval.Report, error) {
	runDir := filepath.Join(s.Root, sanitize(runID))
	var reports []domaineval.Report
	err := filepath.WalkDir(runDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || d.Name() != "report.json" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		var report domaineval.Report
		if jsonErr := json.Unmarshal(data, &report); jsonErr != nil {
			return fmt.Errorf("解析报告 %s 失败: %w", path, jsonErr)
		}
		reports = append(reports, report)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].TaskID != reports[j].TaskID {
			return reports[i].TaskID < reports[j].TaskID
		}
		return reports[i].Mode < reports[j].Mode
	})
	return reports, nil
}

// SaveSummary 写入本次运行汇总。
func (s *FileStore) SaveSummary(ctx context.Context, runID string, summary appeval.Summary) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(s.Root, sanitize(runID), "summary.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeJSON(path, summary)
}

// LoadSummary 读取本次运行汇总，供 --bench report 使用。
func (s *FileStore) LoadSummary(ctx context.Context, runID string) (appeval.Summary, error) {
	if err := ctx.Err(); err != nil {
		return appeval.Summary{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.Root, sanitize(runID), "summary.json"))
	if err != nil {
		return appeval.Summary{}, fmt.Errorf("读取运行汇总失败: %w", err)
	}
	var summary appeval.Summary
	if err := json.Unmarshal(data, &summary); err != nil {
		return appeval.Summary{}, fmt.Errorf("解析运行汇总失败: %w", err)
	}
	return summary, nil
}

// reportPath 拼接报告文件路径。
func reportPath(root, runID, taskID, mode string) string {
	return filepath.Join(root, sanitize(runID), sanitize(taskID), sanitize(mode), "report.json")
}

// sanitize 阻止路径穿越：任务 ID 与模式都来自任务包，仍需兜底。
func sanitize(v string) string {
	clean := filepath.Base(filepath.Clean(v))
	if clean == "." || clean == ".." || clean == string(filepath.Separator) {
		return "_"
	}
	return clean
}

// writeJSON 以缩进格式原子写入 JSON（先写临时文件再重命名）。
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
