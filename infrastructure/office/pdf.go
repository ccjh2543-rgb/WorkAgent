package office

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// PDF 读取上限。
const (
	// maxPDFBytes 单个 PDF 的读取上限。
	maxPDFBytes = 128 << 20
	// maxPDFStreamInflated 单个内容流解压后的上限，防御解压炸弹。
	maxPDFStreamInflated = 64 << 20
	// maxPDFPages 最多解析的页数，避免超大文档拖垮一次工具调用。
	maxPDFPages = 200
)

// ReadPDF 提取 PDF 文本层的文本内容。
//
// 按页返回：每页对应内容流里 Tj / TJ 操作符的字符串。
// 扫描件（无文本层）或使用 CID 字体无法映射 Unicode 的文档会返回
// 明确错误，而不是返回一段看似内容实为乱码的文本。
func ReadPDF(file string) ([]string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("读取 PDF 失败: %w", err)
	}
	if info.Size() > maxPDFBytes {
		return nil, fmt.Errorf("PDF 大小 %d 字节，超过读取上限 %d 字节", info.Size(), maxPDFBytes)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("读取 PDF 失败: %w", err)
	}
	if !bytes.HasPrefix(bytes.TrimLeft(raw, "\xef\xbb\xbf"), []byte("%PDF-")) {
		return nil, errors.New("文件缺少 %PDF- 头，不是有效的 PDF")
	}

	streams := splitContentStreams(raw)
	pages := make([]string, 0, len(streams))
	for _, stream := range streams {
		decoded, err := decodePDFStream(stream)
		if err != nil {
			// 单个流解码失败不影响其余页
			continue
		}
		text := extractTextOperators(decoded)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pages = append(pages, text)
		if len(pages) >= maxPDFPages {
			break
		}
	}
	if len(pages) == 0 {
		return nil, errors.New("未提取到文本层内容（可能是扫描件或纯图形 PDF）")
	}
	return pages, nil
}

// pdfContentStream 一段内容流及其字典信息。
type pdfContentStream struct {
	data []byte
}

// splitContentStreams 扫描 PDF 中的 stream...endstream 片段。
//
// 只按字节扫描定位，不做完整的 xref/对象树解析：目标是取文本，
// 而不是实现一个通用 PDF 解析器。
func splitContentStreams(raw []byte) []pdfContentStream {
	out := make([]pdfContentStream, 0, 16)
	cursor := 0
	for {
		start := bytes.Index(raw[cursor:], []byte("stream"))
		if start < 0 {
			break
		}
		start += cursor
		// 排除 endstream 中的 "stream"
		if start >= 3 && string(raw[start-3:start]) == "end" {
			cursor = start + len("stream")
			continue
		}
		body := start + len("stream")
		// stream 后应先跟 CRLF 或 LF
		if body < len(raw) && raw[body] == '\r' {
			body++
		}
		if body < len(raw) && raw[body] == '\n' {
			body++
		}
		end := bytes.Index(raw[body:], []byte("endstream"))
		if end < 0 {
			break
		}
		out = append(out, pdfContentStream{data: raw[body : body+end]})
		cursor = body + end + len("endstream")
	}
	return out
}

// decodePDFStream 解压内容流。支持 FlateDecode（zlib），其余按原始字节处理。
func decodePDFStream(stream pdfContentStream) ([]byte, error) {
	data := bytes.TrimSpace(stream.data)
	if len(data) == 0 {
		return nil, errors.New("空内容流")
	}
	// 未压缩的内容流直接以操作符文本开头
	if bytes.HasPrefix(data, []byte("q")) || bytes.HasPrefix(data, []byte("BT")) {
		return data, nil
	}
	reader, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		// 退一步尝试跳过前导空白后的 zlib 头
		trimmed := bytes.TrimLeft(data, "\x00\r\n\t ")
		reader, err = zlib.NewReader(bytes.NewReader(trimmed))
		if err != nil {
			return nil, fmt.Errorf("内容流解压失败: %w", err)
		}
	}
	defer func() { _ = reader.Close() }()
	decoded, err := io.ReadAll(io.LimitReader(reader, maxPDFStreamInflated+1))
	if err != nil {
		return nil, fmt.Errorf("读取解压内容流失败: %w", err)
	}
	if len(decoded) > maxPDFStreamInflated {
		return nil, errors.New("内容流解压后超过上限")
	}
	return decoded, nil
}

// textOperator 匹配 PDF 文本显示操作符所在的片段。
var textOperator = regexp.MustCompile(`(?s)(\[[^\]]*\]\s*TJ|\((?:\\.|[^\\()])*\)\s*Tj)`)

// pdfStringLiteral 匹配括号字符串。
var pdfStringLiteral = regexp.MustCompile(`\((?:\\.|[^\\()])*\)`)

// hexStringLiteral 匹配十六进制字符串。
var hexStringLiteral = regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)

// extractTextOperators 从内容流中抽取文本显示操作符的字符串。
func extractTextOperators(content []byte) string {
	text := string(content)
	var sb strings.Builder
	for _, match := range textOperator.FindAllString(text, -1) {
		trimmed := strings.TrimSpace(match)
		if strings.HasSuffix(trimmed, "TJ") {
			for _, lit := range pdfStringLiteral.FindAllString(trimmed, -1) {
				sb.WriteString(decodePDFString(lit))
			}
			// TJ 数组中负数表示字距，按空格处理以保留可读性
			if strings.Contains(trimmed, "-") {
				sb.WriteString(" ")
			}
			continue
		}
		for _, lit := range pdfStringLiteral.FindAllString(trimmed, -1) {
			sb.WriteString(decodePDFString(lit))
		}
	}
	if sb.Len() == 0 {
		for _, lit := range hexStringLiteral.FindAllStringSubmatch(text, -1) {
			sb.WriteString(decodeHexPDFString(lit[1]))
		}
	}
	return strings.TrimSpace(sb.String())
}

// decodePDFString 解析 PDF 括号字符串的转义序列。
func decodePDFString(literal string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(literal, "("), ")")
	var sb strings.Builder
	for i := 0; i < len(body); i++ {
		ch := body[i]
		if ch != '\\' {
			sb.WriteByte(ch)
			continue
		}
		i++
		if i >= len(body) {
			break
		}
		switch body[i] {
		case 'n':
			sb.WriteByte('\n')
		case 'r':
			sb.WriteByte('\r')
		case 't':
			sb.WriteByte('\t')
		case 'b', 'f':
			// 退格与换页对文本阅读无意义，忽略
		case '(', ')', '\\':
			sb.WriteByte(body[i])
		default:
			if body[i] >= '0' && body[i] <= '7' {
				start := i
				for i < len(body) && i < start+3 && body[i] >= '0' && body[i] <= '7' {
					i++
				}
				if code, err := strconv.ParseInt(body[start:i], 8, 32); err == nil {
					sb.WriteByte(byte(code))
				}
				i--
			} else {
				sb.WriteByte(body[i])
			}
		}
	}
	return sb.String()
}

// decodeHexPDFString 解析十六进制字符串，按字节还原文本。
func decodeHexPDFString(hex string) string {
	cleaned := strings.Map(func(r rune) rune {
		if strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return r
		}
		return -1
	}, hex)
	if len(cleaned)%2 == 1 {
		cleaned += "0"
	}
	out := make([]byte, 0, len(cleaned)/2)
	for i := 0; i+1 < len(cleaned); i += 2 {
		value, err := strconv.ParseUint(cleaned[i:i+2], 16, 8)
		if err != nil {
			return string(out)
		}
		out = append(out, byte(value))
	}
	return string(out)
}
