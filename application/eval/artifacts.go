package eval

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"simpleAgent/domain/artifact"
	domaineval "simpleAgent/domain/eval"
)

// excludedDirs 工作区快照与产物采集时跳过的内部目录。
var excludedDirs = map[string]bool{
	".git":          true,
	".simpleagent":  true,
	"__pycache__":   true,
	"node_modules":  true,
	".pytest_cache": true,
}

// excludedFiles 不作为交付物的文件（最终答案单独作为答案索引）。
var excludedFiles = map[string]bool{
	"final_answer.md": true,
}

// CollectArtifacts 采集声明交付物与工作区新增/修改文件。
//
// workspace 是工作区根目录；verifierLogs 是评分器日志目录，用于解析
// 由评分器生成到日志目录的证据文件（例如 artifact_text/）。
// before/after 是运行前后的工作区快照，可为 nil。
// validator 是二进制格式校验器，可为 nil（此时只记录 skipped 的 format_check）。
//
// 返回的失败只包含产物维度的归因（artifact_missing / artifact_invalid），
// 是否致命由调用方结合任务状态判断。
func CollectArtifacts(workspace, verifierLogs string, declared []DeclaredArtifact, before, after map[string]FileMeta, validator artifact.FormatValidator) ([]artifact.Artifact, []domaineval.Failure) {
	arts := make([]artifact.Artifact, 0, len(declared)+len(after))
	failures := make([]domaineval.Failure, 0)
	declaredPaths := make(map[string]bool, len(declared))

	for _, d := range declared {
		art := artifact.Artifact{
			DeclaredID:   d.ID,
			Path:         d.Path,
			DeclaredType: d.Type,
			Required:     d.Required,
			Source:       artifact.SourceDeclared,
			Producer:     "agent",
			Root:         artifact.RootWorkspace,
		}
		root := workspace
		// 评分器可能把证据文件写到日志目录（如 artifact_text/），工作区缺失时回退。
		if verifierLogs != "" && !fileExists(workspace, d.Path) && fileExists(verifierLogs, d.Path) {
			root = verifierLogs
			art.Root = artifact.RootVerifierLogs
		}
		art.ValidateBasic(root, validator)
		arts = append(arts, art)

		if rel, err := artifact.SafeRelPath(d.Path); err == nil {
			declaredPaths[rel] = true
		}
		if !art.Valid {
			code := domaineval.FailArtifactInvalid
			if !checkPassed(art.Checks, "exists") {
				code = domaineval.FailArtifactMissing
			}
			if d.Required {
				failures = append(failures, domaineval.Failure{
					Code:    code,
					Scope:   "artifact",
					Message: fmt.Sprintf("声明产物 %s（%s）不可用: %s", d.ID, d.Path, firstFailedDetail(art.Checks)),
				})
			}
		}
	}

	// 未声明的产物：运行前后快照出现新增或内容变化的文件。
	for rel := range after {
		if shouldSkipPath(rel) || declaredPaths[rel] {
			continue
		}
		prev, existed := before[rel]
		if existed && prev.Hash == after[rel].Hash && prev.Size == after[rel].Size {
			continue
		}
		art := artifact.Artifact{
			Path:     rel,
			Source:   artifact.SourceWorkspaceDiff,
			Producer: "agent",
			Root:     artifact.RootWorkspace,
		}
		art.ValidateBasic(workspace, validator)
		if !checkPassed(art.Checks, "exists") {
			continue
		}
		arts = append(arts, art)
	}

	sortArtifacts(arts)
	return arts, failures
}

// sortArtifacts 按 required 降序、declared_id 升序、path 升序稳定排序。
func sortArtifacts(arts []artifact.Artifact) {
	sort.SliceStable(arts, func(i, j int) bool {
		if arts[i].Required != arts[j].Required {
			return arts[i].Required
		}
		if arts[i].DeclaredID != arts[j].DeclaredID {
			return arts[i].DeclaredID < arts[j].DeclaredID
		}
		return arts[i].Path < arts[j].Path
	})
}

// shouldSkipPath 判断相对路径是否属于排除目录或排除文件。
func shouldSkipPath(rel string) bool {
	rel = strings.TrimPrefix(strings.ReplaceAll(rel, "\\", "/"), "./")
	if excludedFiles[rel] {
		return true
	}
	for _, seg := range strings.Split(rel, "/") {
		if excludedDirs[seg] {
			return true
		}
	}
	return false
}

// fileExists 判断 root 下是否存在该相对路径的普通文件。
func fileExists(root, rel string) bool {
	clean, err := artifact.SafeRelPath(rel)
	if err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(clean)))
	return err == nil && info.Mode().IsRegular()
}

// checkPassed 读取指定检查项是否通过。
func checkPassed(checks []artifact.Check, name string) bool {
	for _, c := range checks {
		if c.Name == name {
			return c.Passed
		}
	}
	return false
}

// firstFailedDetail 返回首个未通过检查项的说明，用于产物失败归因。
//
// 若全部检查都通过（理论上不会进入失败分支），返回“未通过”兜底。
func firstFailedDetail(checks []artifact.Check) string {
	for _, c := range checks {
		if c.Passed {
			continue
		}
		if c.Detail != "" {
			return c.Name + ": " + c.Detail
		}
		return c.Name + ": 未通过"
	}
	return "未通过"
}

// checkDetail 读取指定检查项的说明，便于失败归因。
func checkDetail(checks []artifact.Check, name string) string {
	for _, c := range checks {
		if c.Name == name {
			if c.Detail != "" {
				return c.Detail
			}
			if c.Passed {
				return "通过"
			}
			return "未通过"
		}
	}
	return "未执行"
}
