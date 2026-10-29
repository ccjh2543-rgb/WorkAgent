package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	appagent "simpleAgent/application/agent"
	"simpleAgent/domain/tool"
	"simpleAgent/infrastructure/audit"
	"simpleAgent/infrastructure/tools"
)

// storedSkill 落在磁盘上的技能定义（skills.json）
type storedSkill struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	CommandTemplate string `json:"command_template"`
	Risk            string `json:"risk"` // read_only / write_local / write_external / destructive
}

// SkillStore 技能的持久化与运行时册管理。
// 真源是 skills.json；启动时 registerInto 一次性注册进 Agent，
// 运行中用 add/remove 热更新。
type SkillStore struct {
	path   string
	mu     sync.Mutex
	skills []storedSkill
}

// NewSkillStore 打开（或创建空）技能配置
func NewSkillStore(path string) (*SkillStore, error) {
	s := &SkillStore{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load 从磁盘读取技能列表；文件不存在视为空列表
func (s *SkillStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.skills = nil
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取技能配置失败: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.skills = nil
		return nil
	}
	if err := json.Unmarshal(data, &s.skills); err != nil {
		return fmt.Errorf("解析技能配置失败: %w", err)
	}
	return nil
}

// list 返回当前全部技能定义（只读副本）
func (s *SkillStore) list() []storedSkill {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]storedSkill, len(s.skills))
	copy(out, s.skills)
	return out
}

// registerInto 把全部已持久化技能注册进 Agent（启动时调用）
func (s *SkillStore) registerInto(myAgent *appagent.Agent, ws *tools.Workspace, auditLog *audit.Logger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, skill := range s.skills {
		// 命令技能一律按最高风险注册；历史配置里的 read_only 声明不可信
		cmd, err := tools.NewCommandSkill(skill.toSpec(), ws, tool.RiskDestructive)
		if err != nil {
			return err
		}
		cmd.SetAudit(auditLog)
		if err := myAgent.RegisterSkill(cmd, cmd.Policy()); err != nil {
			return err
		}
	}
	return nil
}

// add 新增一个技能：注册进 Agent 并持久化
func (s *SkillStore) add(myAgent *appagent.Agent, ws *tools.Workspace, spec tools.SkillSpec, risk tool.RiskLevel, auditLog *audit.Logger) error {
	cmd, err := tools.NewCommandSkill(spec, ws, risk)
	if err != nil {
		return err
	}
	cmd.SetAudit(auditLog)
	if err := myAgent.RegisterSkill(cmd, cmd.Policy()); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.skills = append(s.skills, storedSkill{
		Name:            spec.Name,
		Description:     spec.Description,
		CommandTemplate: spec.CommandTemplate,
		Risk:            riskLabel(risk),
	})
	if err := s.save(); err != nil {
		// 持久化失败则回滚内存注册，避免只活在本进程里的假技能
		_ = myAgent.UnregisterSkill(spec.Name)
		return err
	}
	return nil
}

// remove 删除一个技能：先从 Agent 卸载，再从持久化列表移除
func (s *SkillStore) remove(myAgent *appagent.Agent, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	found := false
	kept := s.skills[:0]
	for _, skill := range s.skills {
		if skill.Name == name {
			found = true
			continue
		}
		kept = append(kept, skill)
	}
	if !found {
		return fmt.Errorf("未找到技能: %s", name)
	}
	if err := myAgent.UnregisterSkill(name); err != nil {
		return err
	}
	s.skills = kept
	return s.save()
}

// save 把当前技能列表写入磁盘（原子替换）
func (s *SkillStore) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return fmt.Errorf("创建技能配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s.skills, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化技能配置失败: %w", err)
	}
	if err := writeAtomic(s.path, data); err != nil {
		return fmt.Errorf("写入技能配置失败: %w", err)
	}
	return nil
}

// writeAtomic 以原子方式写文件：先写同目录临时文件再 Rename，避免进程中断留下半成品
func writeAtomic(target string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".skills-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// riskLabel 把枚举映射为持久化使用的字符串标识
func riskLabel(level tool.RiskLevel) string {
	switch level {
	case tool.RiskReadOnly:
		return "read_only"
	case tool.RiskWriteLocal:
		return "write_local"
	case tool.RiskWriteExternal:
		return "write_external"
	default:
		return "destructive"
	}
}

func (sl storedSkill) toSpec() tools.SkillSpec {
	return tools.SkillSpec{
		Name:            sl.Name,
		Description:     sl.Description,
		CommandTemplate: sl.CommandTemplate,
	}
}
