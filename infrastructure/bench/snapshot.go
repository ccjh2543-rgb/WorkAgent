package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	appeval "simpleAgent/application/eval"
	"simpleAgent/domain/artifact"
)

// snapshotExcludedDirs 快照时跳过的内部目录。
var snapshotExcludedDirs = map[string]bool{
	".git":          true,
	".simpleagent":  true,
	"__pycache__":   true,
	"node_modules":  true,
	".pytest_cache": true,
}

// SnapshotWorkspace 遍历工作区并计算每个普通文件的大小、mtime 与 SHA-256。
//
// 路径统一使用斜杠分隔的相对路径；内部目录与链接文件被跳过。
func SnapshotWorkspace(ctx context.Context, root string) (map[string]appeval.FileMeta, error) {
	out := make(map[string]appeval.FileMeta)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if path != root && snapshotExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		hash, hashErr := artifact.HashFile(path)
		if hashErr != nil {
			return hashErr
		}
		out[filepath.ToSlash(rel)] = appeval.FileMeta{
			Size:    info.Size(),
			ModTime: info.ModTime(),
			Hash:    hash,
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("工作区快照失败: %w", err)
	}
	return out, nil
}

// copyTree 递归复制目录（用于把 tests 复制到运行目录，避免污染数据集）。
func copyTree(src, dst string) error {
	src = filepath.Clean(src)
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			if d.Name() == "__pycache__" || d.Name() == ".pytest_cache" {
				return filepath.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, ierr := d.Info(); ierr == nil && info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
}
