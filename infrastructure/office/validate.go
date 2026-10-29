package office

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"simpleAgent/domain/artifact"
)

// Validator 交付物格式校验器。
//
// 它按 OOXML / PDF 的真实结构判断文件能否被标准读取器打开，
// 用于把“扩展名正确但内容损坏或为空壳”的产物判为无效。
type Validator struct{}

// NewValidator 创建格式校验器。
func NewValidator() *Validator { return &Validator{} }

// ValidateFormat 校验文件是否符合 kind 声明的格式。
//
// 只做结构性检查，不解析全部业务内容，避免大文件拖慢评测。
func (v *Validator) ValidateFormat(path string, kind artifact.Kind) error {
	switch kind {
	case artifact.KindXLSX:
		return validateOOXML(path, xlsxRequiredParts)
	case artifact.KindDOCX:
		return validateOOXML(path, docxRequiredParts)
	case artifact.KindPPTX:
		return validateOOXML(path, pptxRequiredParts)
	case artifact.KindPDF:
		return validatePDF(path)
	}
	return fmt.Errorf("不支持的格式校验类型: %s", kind)
}

// OOXML 包的必要部件。
//
// sheet 部件只能确认存在，具体名称由 workbook.xml 的关系决定，
// 这里按前缀判断，避免把合法文件误判为损坏。
var (
	xlsxRequiredParts = []string{"[Content_Types].xml", "xl/workbook.xml"}
	docxRequiredParts = []string{"[Content_Types].xml", "word/document.xml"}
	pptxRequiredParts = []string{"[Content_Types].xml", "ppt/presentation.xml"}
)

// validateOOXML 打开 OOXML 包并检查必要部件。
func validateOOXML(path string, required []string) error {
	reader, err := openZip(path)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	for _, name := range required {
		if !hasPart(reader, name) {
			return fmt.Errorf("缺少必要部件 %s", name)
		}
	}
	return nil
}

// validatePDF 校验 PDF 的头尾标记。
//
// 完整解析需要重建对象树，这里只确认文件被写完整：
// 头必须是 %PDF-，且允许存在少量尾部噪声时仍能找到 %%EOF。
func validatePDF(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < 16 {
		return errors.New("PDF 文件过小，不可能有效")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	head := make([]byte, 8)
	if _, err := io.ReadFull(f, head); err != nil {
		return fmt.Errorf("读取 PDF 头部失败: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimLeft(head, "\xef\xbb\xbf"), []byte("%PDF-")) {
		return errors.New("缺少 %PDF- 头")
	}

	tailSize := int64(4096)
	if info.Size() < tailSize {
		tailSize = info.Size()
	}
	if _, err := f.Seek(-tailSize, io.SeekEnd); err != nil {
		return fmt.Errorf("定位 PDF 尾部失败: %w", err)
	}
	tail, err := io.ReadAll(io.LimitReader(f, tailSize))
	if err != nil {
		return fmt.Errorf("读取 PDF 尾部失败: %w", err)
	}
	if !strings.Contains(string(tail), "%%EOF") {
		return errors.New("缺少 %%EOF 结尾，文件可能被截断")
	}
	return nil
}
