package bench

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 解压上限，防止 tar 炸弹占满磁盘。
const (
	maxTarFiles = 20000
	maxTarBytes = int64(1 << 30) // 1 GiB
)

// ExtractTarGz 把工作区归档安全解压到 dest。
//
// 拒绝绝对路径、父目录跳转、符号链接/硬链接/设备文件，
// 并限制文件数与解压总大小；任何异常都不会在工作区外产生文件。
func ExtractTarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("打开归档失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 gzip 失败: %w", err)
	}
	defer func() { _ = gz.Close() }()

	absDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absDest, 0o755); err != nil {
		return fmt.Errorf("创建工作区失败: %w", err)
	}

	tr := tar.NewReader(gz)
	var files int
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 tar 条目失败: %w", err)
		}
		files++
		if files > maxTarFiles {
			return fmt.Errorf("归档文件数超过上限 %d", maxTarFiles)
		}
		if err := extractEntry(tr, hdr, absDest, &total); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry 解压单个 tar 条目并校验路径与类型。
func extractEntry(r io.Reader, hdr *tar.Header, dest string, total *int64) error {
	name := filepath.FromSlash(strings.TrimPrefix(hdr.Name, "./"))
	if name == "" || name == "." {
		return nil
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, string(filepath.Separator)) {
		return fmt.Errorf("归档包含绝对路径: %s", hdr.Name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("归档包含越界路径: %s", hdr.Name)
	}
	target := filepath.Join(dest, clean)
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("归档条目越出目标目录: %s", hdr.Name)
	}

	switch hdr.Typeflag {
	case tar.TypeDir:
		return os.MkdirAll(target, 0o755)
	case tar.TypeReg, tar.TypeRegA:
		*total += hdr.Size
		if *total > maxTarBytes {
			return fmt.Errorf("归档解压总大小超过上限 %d 字节", maxTarBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if hdr.Mode&0o111 != 0 {
			mode = 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return err
		}
		if _, err := io.CopyN(out, r, hdr.Size); err != nil {
			_ = out.Close()
			return fmt.Errorf("写入 %s 失败: %w", hdr.Name, err)
		}
		return out.Close()
	case tar.TypeSymlink, tar.TypeLink:
		return fmt.Errorf("归档包含链接文件，已拒绝: %s", hdr.Name)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("归档包含设备/管道文件，已拒绝: %s", hdr.Name)
	default:
		return fmt.Errorf("归档包含不支持的文件类型 %d: %s", hdr.Typeflag, hdr.Name)
	}
}
