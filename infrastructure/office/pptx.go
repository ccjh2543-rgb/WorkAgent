package office

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Slide 一页幻灯片的文本内容。
type Slide struct {
	// Index 幻灯片序号，从 1 开始，按 presentation.xml.rels 顺序。
	Index int
	// Texts 按出现顺序排列的文本段落。
	Texts []string
	// Notes 备注页文本（若存在）。
	Notes []string
}

// Presentation 从 pptx 中提取的演示文稿内容。
type Presentation struct {
	Slides []Slide
}

// Text 把演示文稿渲染为便于阅读的文本。
func (p Presentation) Text() string {
	var sb strings.Builder
	for _, s := range p.Slides {
		fmt.Fprintf(&sb, "[第 %d 页]\n", s.Index)
		for _, t := range s.Texts {
			sb.WriteString(t)
			sb.WriteString("\n")
		}
		if len(s.Notes) > 0 {
			sb.WriteString("[备注]\n")
			for _, t := range s.Notes {
				sb.WriteString(t)
				sb.WriteString("\n")
			}
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ReadPPTX 按页提取 pptx 的文本内容。
//
// 备注页在文件内以 notesSlideN 命名，无法从编号直接对齐，
// 因此先按关系文件建立 slideN → notesSlideM 的映射，再逐页提取。
func ReadPPTX(file string) (Presentation, error) {
	reader, err := openZip(file)
	if err != nil {
		return Presentation{}, err
	}
	defer func() { _ = reader.Close() }()

	slidePaths, err := readSlideOrder(reader)
	if err != nil {
		return Presentation{}, err
	}
	presentation := Presentation{Slides: make([]Slide, 0, len(slidePaths))}
	for i, slidePath := range slidePaths {
		data, err := readPart(reader, slidePath)
		if err != nil {
			return Presentation{}, err
		}
		texts, err := parseSlideTexts(data)
		if err != nil {
			return Presentation{}, err
		}
		slide := Slide{Index: i + 1, Texts: texts}

		baseDir := pathDir(slidePath)
		relsName := baseDir + "/_rels/" + baseName(slidePath) + ".rels"
		if rels, relErr := readRelationships(reader, relsName); relErr == nil {
			for _, target := range rels {
				if !strings.Contains(strings.ToLower(target), "notesslide") {
					continue
				}
				notesPath := resolveTarget(baseDir, target)
				if notesData, err := readPart(reader, notesPath); err == nil {
					if notesTexts, err := parseSlideTexts(notesData); err == nil {
						slide.Notes = notesTexts
					}
				}
			}
		}
		presentation.Slides = append(presentation.Slides, slide)
	}
	if len(presentation.Slides) == 0 {
		return Presentation{}, fmt.Errorf("%w: 没有幻灯片", errNoPart)
	}
	return presentation, nil
}

// readSlideOrder 返回按演示顺序排列的幻灯片包内路径。
func readSlideOrder(reader *zip.ReadCloser) ([]string, error) {
	data, err := readPart(reader, "ppt/presentation.xml")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoPart, err)
	}
	var presentation struct {
		SlideList struct {
			IDs []struct {
				RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
			} `xml:"sldId"`
		} `xml:"sldIdLst"`
	}
	if err := xml.Unmarshal(data, &presentation); err != nil {
		return nil, fmt.Errorf("解析 ppt/presentation.xml 失败: %w", err)
	}
	rels, err := readRelationships(reader, "ppt/_rels/presentation.xml.rels")
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(presentation.SlideList.IDs))
	for i, s := range presentation.SlideList.IDs {
		target := rels[s.RID]
		if target == "" {
			target = fmt.Sprintf("slides/slide%d.xml", i+1)
		}
		paths = append(paths, resolveTarget("ppt", target))
	}
	if len(paths) == 0 {
		// 关系表缺失时按文件名兜底排序，避免整份文档读不出来
		for _, f := range reader.File {
			name := strings.TrimPrefix(f.Name, "/")
			if strings.HasPrefix(name, "ppt/slides/slide") && strings.HasSuffix(name, ".xml") {
				paths = append(paths, name)
			}
		}
		sort.SliceStable(paths, func(i, j int) bool { return slideNumber(paths[i]) < slideNumber(paths[j]) })
	}
	return paths, nil
}

// slideNumber 从 slideN.xml 中解析 N，解析失败返回一个很大的值排在最后。
func slideNumber(name string) int {
	base := baseName(name)
	digits := strings.TrimSuffix(strings.TrimPrefix(base, "slide"), ".xml")
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 1 << 30
	}
	return n
}

// parseSlideTexts 提取一页中所有 a:t 文本。
func parseSlideTexts(data []byte) ([]string, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	texts := make([]string, 0, 16)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("解析幻灯片失败: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "t" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &start); err != nil {
			return nil, fmt.Errorf("读取幻灯片文本失败: %w", err)
		}
		text = strings.TrimSpace(text)
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts, nil
}

// pathDir 返回包内路径的目录部分。
func pathDir(name string) string {
	idx := strings.LastIndex(name, "/")
	if idx < 0 {
		return "."
	}
	return name[:idx]
}

// baseName 返回包内路径的文件名部分。
func baseName(name string) string {
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		return name[idx+1:]
	}
	return name
}
