// Package artifact 定义评测交付物（Artifact）的领域模型。
//
// 该包只包含交付物的类型推断、路径安全约束与基础校验规则，
// 不依赖文件系统以外的任何外部包，也不关心产物由哪个 Agent 或工具产生。
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Kind 交付物类型，取值与 judge.yaml 的 type 语义保持一致。
type Kind string

const (
	KindXLSX     Kind = "xlsx"
	KindDOCX     Kind = "docx"
	KindPPTX     Kind = "pptx"
	KindPDF      Kind = "pdf"
	KindCSV      Kind = "csv"
	KindJSON     Kind = "json"
	KindMarkdown Kind = "md"
	KindTXT      Kind = "txt"
	KindText     Kind = "text"
	KindImage    Kind = "image"
	KindOther    Kind = "other"
)

// kindByExt 扩展名到交付物类型的映射。
var kindByExt = map[string]Kind{
	".xlsx": KindXLSX,
	".xlsm": KindXLSX,
	".docx": KindDOCX,
	".pptx": KindPPTX,
	".pdf":  KindPDF,
	".csv":  KindCSV,
	".json": KindJSON,
	".md":   KindMarkdown,
	".txt":  KindTXT,
	".png":  KindImage,
	".jpg":  KindImage,
	".jpeg": KindImage,
	".webp": KindImage,
	".gif":  KindImage,
}

// mimeByKind 交付物类型对应的 MIME。
var mimeByKind = map[Kind]string{
	KindXLSX:     "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	KindDOCX:     "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	KindPPTX:     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	KindPDF:      "application/pdf",
	KindCSV:      "text/csv",
	KindJSON:     "application/json",
	KindMarkdown: "text/markdown",
	KindTXT:      "text/plain",
	KindText:     "text/plain",
	KindImage:    "image/*",
	KindOther:    "application/octet-stream",
}

// DetectKind 按文件扩展名推断交付物类型，未知扩展名返回 KindOther。
func DetectKind(path string) Kind {
	ext := strings.ToLower(filepath.Ext(path))
	if k, ok := kindByExt[ext]; ok {
		return k
	}
	return KindOther
}

// KindFromDeclared 把 judge.yaml 声明的 type 归一为 Kind。
//
// 声明值未知时返回 KindOther，由基础校验记录 mime_match 失败，
// 而不是在解析阶段直接拒绝整个任务包。
func KindFromDeclared(declared string) Kind {
	v := strings.ToLower(strings.TrimSpace(declared))
	if v == "" {
		return KindOther
	}
	switch v {
	case "xlsx", "excel":
		return KindXLSX
	case "docx", "word":
		return KindDOCX
	case "pptx", "ppt", "powerpoint":
		return KindPPTX
	case "pdf":
		return KindPDF
	case "csv":
		return KindCSV
	case "json":
		return KindJSON
	case "md", "markdown":
		return KindMarkdown
	case "txt", "text", "plain_text":
		return KindTXT
	case "image", "png", "jpg", "jpeg":
		return KindImage
	}
	return Kind(v)
}

// MIME 返回交付物类型对应的 MIME，未知类型返回二进制流 MIME。
func MIME(k Kind) string {
	if v, ok := mimeByKind[k]; ok {
		return v
	}
	return mimeByKind[KindOther]
}

// Check 单项校验结果。Skipped 表示当前模块尚未接入该检查（例如二进制格式校验）。
type Check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Artifact 一个交付物的完整描述。
//
// Path 始终是相对工作区（或交付物根目录）的路径；Root 记录实际解析根，
// 便于区分工作区产物与评分器生成到日志目录的证据文件。
type Artifact struct {
	DeclaredID   string    `json:"declared_id,omitempty"`
	Path         string    `json:"path"`
	Root         string    `json:"root,omitempty"`
	Kind         Kind      `json:"kind"`
	DeclaredType string    `json:"declared_type,omitempty"`
	MIME         string    `json:"mime"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Producer     string    `json:"producer"`
	Source       string    `json:"source"`
	Required     bool      `json:"required"`
	Valid        bool      `json:"valid"`
	Checks       []Check   `json:"checks"`
	CreatedAt    time.Time `json:"created_at"`
}

// 交付物来源常量。
const (
	SourceDeclared      = "declared"
	SourceWorkspaceDiff = "workspace_diff"
)

// 交付物解析根常量。
const (
	RootWorkspace    = "workspace"
	RootVerifierLogs = "verifier_logs"
)

// binaryKinds 需要用真实解析器做格式校验的二进制类型。
var binaryKinds = map[Kind]bool{
	KindXLSX: true,
	KindDOCX: true,
	KindPPTX: true,
	KindPDF:  true,
}

// FormatValidator 二进制交付物的格式校验端口。
//
// 实现位于 infrastructure/office：它按 OOXML/PDF 的真实结构判断文件
// 是否可打开，从而把“扩展名对但内容损坏”的产物识别出来。
// 领域层只依赖该接口，不引入 zip、XML 等外部包。
type FormatValidator interface {
	// ValidateFormat 校验 path 是否符合 kind 声明的格式；nil 表示通过。
	ValidateFormat(path string, kind Kind) error
}

// ValidateBasic 对交付物做基础校验并回填哈希、大小、类型与检查列表。
//
// root 是交付物解析根目录，art.Path 必须是其内部的相对路径。
// 路径越界、文件缺失或不是普通文件时 Valid=false，但不会返回 error，
// 便于报告保留一条“缺失但已声明”的记录。
//
// validator 为 nil 时，二进制类型只记录 skipped 的 format_check。
func (a *Artifact) ValidateBasic(root string, validator FormatValidator) {
	a.CreatedAt = time.Now()
	a.Checks = a.Checks[:0]
	a.Valid = false
	a.Size = 0
	a.SHA256 = ""
	a.Kind = DetectKind(a.Path)
	a.MIME = MIME(a.Kind)
	if a.Producer == "" {
		a.Producer = "agent"
	}

	rel, err := SafeRelPath(a.Path)
	if err != nil {
		a.Checks = append(a.Checks, Check{Name: "inside_workspace", Passed: false, Detail: err.Error()})
		a.Checks = append(a.Checks, Check{Name: "exists", Passed: false, Detail: "路径非法，未检查文件"})
		return
	}
	a.Path = rel

	full := filepath.Join(root, filepath.FromSlash(rel))
	inside, err := IsInside(root, full)
	if err != nil || !inside {
		detail := "真实路径越出根目录"
		if err != nil {
			detail = err.Error()
		}
		a.Checks = append(a.Checks, Check{Name: "inside_workspace", Passed: false, Detail: detail})
		a.Checks = append(a.Checks, Check{Name: "exists", Passed: false, Detail: "路径越界，未检查文件"})
		return
	}
	a.Checks = append(a.Checks, Check{Name: "inside_workspace", Passed: true})

	info, err := os.Lstat(full)
	if err != nil {
		a.Checks = append(a.Checks, Check{Name: "exists", Passed: false, Detail: err.Error()})
		return
	}
	if !info.Mode().IsRegular() {
		a.Checks = append(a.Checks, Check{Name: "exists", Passed: false, Detail: "不是普通文件"})
		return
	}
	a.Checks = append(a.Checks, Check{Name: "exists", Passed: true})
	a.Size = info.Size()
	if info.Size() <= 0 {
		a.Checks = append(a.Checks, Check{Name: "non_empty", Passed: false, Detail: "文件为空"})
	} else {
		a.Checks = append(a.Checks, Check{Name: "non_empty", Passed: true})
	}

	sum, err := HashFile(full)
	if err != nil {
		a.Checks = append(a.Checks, Check{Name: "sha256", Passed: false, Detail: err.Error()})
	} else {
		a.SHA256 = sum
		a.Checks = append(a.Checks, Check{Name: "sha256", Passed: true})
	}
	a.Valid = a.SHA256 != "" && info.Size() > 0

	if declared := KindFromDeclared(a.DeclaredType); declared != KindOther {
		ok := declared == a.Kind || (declared == KindTXT && a.Kind == KindText)
		a.Checks = append(a.Checks, Check{
			Name: "mime_match", Passed: ok,
			Detail: fmt.Sprintf("声明类型=%s 实际类型=%s", declared, a.Kind),
		})
		if !ok {
			a.Valid = false
		}
	}

	if a.Kind == KindJSON && info.Size() > 0 {
		if err := validateJSONFile(full); err != nil {
			a.Checks = append(a.Checks, Check{Name: "json_parse", Passed: false, Detail: err.Error()})
			a.Valid = false
		} else {
			a.Checks = append(a.Checks, Check{Name: "json_parse", Passed: true})
		}
	}

	if binaryKinds[a.Kind] {
		a.appendFormatCheck(full, validator)
	}
}

// appendFormatCheck 追加二进制格式校验结果。
//
// validator 为 nil 时记录 skipped：既不误判为通过，也不因缺少校验器
// 把已经存在的产物判为无效。
func (a *Artifact) appendFormatCheck(full string, validator FormatValidator) {
	if validator == nil {
		a.Checks = append(a.Checks, Check{
			Name: "format_check", Passed: false, Skipped: true,
			Detail: "未注入格式校验器，跳过二进制格式校验",
		})
		return
	}
	if err := validator.ValidateFormat(full, a.Kind); err != nil {
		a.Checks = append(a.Checks, Check{Name: "format_check", Passed: false, Detail: err.Error()})
		a.Valid = false
		return
	}
	a.Checks = append(a.Checks, Check{Name: "format_check", Passed: true})
}

// SafeRelPath 校验并归一化交付物的相对路径。
//
// 拒绝绝对路径、父目录跳转与空路径，统一使用斜杠分隔。
func SafeRelPath(p string) (string, error) {
	raw := strings.TrimSpace(p)
	if raw == "" {
		return "", fmt.Errorf("交付物路径为空")
	}
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") {
		return "", fmt.Errorf("交付物路径必须是相对路径: %s", raw)
	}
	clean := filepath.Clean(filepath.FromSlash(raw))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("交付物路径越界: %s", raw)
	}
	return filepath.ToSlash(clean), nil
}

// IsInside 判断 target 是否位于 root 目录内部（含 root 本身）。
func IsInside(root, target string) (bool, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return false, err
	}
	if rel == "." {
		return true, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, nil
	}
	return true, nil
}

// HashFile 流式计算文件 SHA-256，避免整文件读入内存。
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// validateJSONFile 校验 JSON 文件可解析，不保留解码后的内容。
func validateJSONFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	return nil
}
