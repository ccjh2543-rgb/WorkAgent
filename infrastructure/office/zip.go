// Package office 提供 Office 文档的标准库编解码实现。
//
// 本包只依赖 Go 标准库：xlsx/docx/pptx 按 OOXML 的 zip + XML 结构读写，
// pdf 做文本层提取。编码结果面向评测评分器（openpyxl/python-docx 等），
// 不追求完整的办公软件特性。
package office

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// zip 读取上限，用于防御解压炸弹。
const (
	// maxZipEntries 单个包允许的最大条目数。
	maxZipEntries = 5000
	// maxZipUncompressed 单个包解压后的总字节上限。
	maxZipUncompressed = 512 << 20
	// maxPartBytes 单个 XML 部件读取上限。
	maxPartBytes = 128 << 20
)

// openZip 打开 Office 包并检查条目数量与解压后大小。
func openZip(file string) (*zip.ReadCloser, error) {
	reader, err := zip.OpenReader(file)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 zip/OOXML 文件: %w", err)
	}
	if len(reader.File) > maxZipEntries {
		_ = reader.Close()
		return nil, fmt.Errorf("压缩包条目过多（%d），已拒绝", len(reader.File))
	}
	var total uint64
	for _, f := range reader.File {
		total += f.UncompressedSize64
		if total > maxZipUncompressed {
			_ = reader.Close()
			return nil, fmt.Errorf("压缩包解压后超过 %d MB，已拒绝", maxZipUncompressed>>20)
		}
	}
	return reader, nil
}

// zipFile 按名称查找包内部件。
func zipFile(reader *zip.ReadCloser, name string) (*zip.File, error) {
	want := path.Clean(strings.TrimPrefix(name, "/"))
	for _, f := range reader.File {
		if path.Clean(strings.TrimPrefix(f.Name, "/")) == want {
			return f, nil
		}
	}
	return nil, fmt.Errorf("缺少部件 %s", name)
}

// readPart 读取包内部件的全部内容。
func readPart(reader *zip.ReadCloser, name string) ([]byte, error) {
	f, err := zipFile(reader, name)
	if err != nil {
		return nil, err
	}
	if f.UncompressedSize64 > maxPartBytes {
		return nil, fmt.Errorf("部件 %s 超过读取上限", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("打开部件 %s 失败: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxPartBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取部件 %s 失败: %w", name, err)
	}
	if len(data) > maxPartBytes {
		return nil, fmt.Errorf("部件 %s 超过读取上限", name)
	}
	return data, nil
}

// hasPart 判断包内是否存在指定部件。
func hasPart(reader *zip.ReadCloser, name string) bool {
	_, err := zipFile(reader, name)
	return err == nil
}

// resolveTarget 把关系文件里的 Target 解析为包内路径。
//
// target 可能写成 worksheets/sheet1.xml、/xl/worksheets/sheet1.xml，
// 也可能带 ../ 相对路径；统一按 rels 所在目录解析。
func resolveTarget(baseDir, target string) string {
	target = strings.TrimSpace(strings.ReplaceAll(target, "\\", "/"))
	if target == "" {
		return ""
	}
	if strings.HasPrefix(target, "/") {
		return path.Clean(strings.TrimPrefix(target, "/"))
	}
	return path.Clean(path.Join(baseDir, target))
}

// errNoPart 表示包内缺少必要部件。
var errNoPart = errors.New("OOXML 包缺少必要部件")
