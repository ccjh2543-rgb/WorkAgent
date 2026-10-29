// Package planner 定义任务规划领域的核心模型。
//
// Task / Planner 承载任务拆解与进度跟踪的纯领域逻辑，不依赖任何外部包。
// 生成计划的 LLM 调用通过注入的 ChatModel 端口完成，见 GeneratePlan。
package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// Task 一项待执行或已完成的子任务
type Task struct {
	ID      int
	Content string
	Done    bool
	Result  string // 子任务执行返回的压缩结果摘要（Planner-Worker 模式使用）
	Failed  bool   // 子任务是否执行失败（Planner-Worker 模式使用，触发重规划）
}

// Planner 任务拆解与进度跟踪器
type Planner struct {
	Tasks []Task
}

// NewPlanner 创建计划器
func NewPlanner() *Planner {
	return &Planner{
		Tasks: make([]Task, 0),
	}
}

// GeneratePlan 通过注入的模型端口生成计划。
// client 为 nil 时无法生成计划，返回错误。
//
// 各调用方（生产/评估）把同一个 ChatModel 实例传入即可，
// 这里不再依赖具体 SDK，保持领域纯净。
func (p *Planner) GeneratePlan(ctx context.Context, model agent.ChatModel, goal string) error {
	if model == nil {
		return errors.New("计划生成需要模型端口")
	}
	sys := `你是任务规划器，请把用户目标拆解成有序的分步任务。
输出格式，每行一条任务，不要多余文字。
例子：
1. 查询今天天气
2. 根据天气给出出行建议
`
	reply, err := model.Generate(ctx, agent.ChatRequest{
		Model: "",
		Messages: []agent.ChatMessage{
			{Role: session.RoleSystem, Content: sys},
			{Role: session.RoleUser, Content: "目标：" + goal},
		},
		Temperature: 0,
	})
	if err != nil {
		return err
	}
	p.Replace(reply.Content)
	return nil
}

// Replace 用给定文本整体替换任务列表，一行一条任务，空行忽略。
// 采用整体替换而非追加，重复调用不会累积上一轮的任务。
func (p *Planner) Replace(planText string) {
	tasks := make([]Task, 0, 8)
	idx := 1
	for _, line := range strings.Split(planText, "\n") {
		content := strings.TrimSpace(line)
		if content == "" {
			continue
		}
		tasks = append(tasks, Task{ID: idx, Content: content, Done: false})
		idx++
	}
	p.Tasks = tasks
}

// PlanText 把当前任务列表渲染为可读文本，供人工确认与改写。
// 与 Replace 互为逆运算，保证人工改动是所见即所得的。
func (p *Planner) PlanText() string {
	lines := make([]string, 0, len(p.Tasks))
	for _, task := range p.Tasks {
		lines = append(lines, task.Content)
	}
	return strings.Join(lines, "\n")
}

// GetNextUnDoneTask 获取下一个未完成的任务；全部完成时返回 nil。
// 必须用下标返回切片元素的地址，range 得到的是副本，对其取址无法写回。
func (p *Planner) GetNextUnDoneTask() *Task {
	for i := range p.Tasks {
		if !p.Tasks[i].Done {
			return &p.Tasks[i]
		}
	}
	return nil
}

// MarkTaskDone 标记任务完成。
// 必须用下标修改切片元素：range 遍历拿到的是元素副本，改副本不会生效。
func (p *Planner) MarkTaskDone(taskID int) {
	for i := range p.Tasks {
		if p.Tasks[i].ID == taskID {
			p.Tasks[i].Done = true
			return
		}
	}
}

// IsAllTasksDone 判断任务是否全部完成
func (p *Planner) IsAllTasksDone() bool {
	for _, task := range p.Tasks {
		if !task.Done {
			return false
		}
	}
	return true
}

// MarkTaskFailed 标记任务执行失败（Planner-Worker 模式触发重规划用）。
func (p *Planner) MarkTaskFailed(taskID int) {
	for i := range p.Tasks {
		if p.Tasks[i].ID == taskID {
			p.Tasks[i].Failed = true
			return
		}
	}
}

// BuildPriorSummaries 把已完成任务的 Result 摘要拼成给后续 Worker 的前置信息。
// 仅包含已完成的子任务结果，调用方可再用 token 预算截断（防无限增长）。
func (p *Planner) BuildPriorSummaries() string {
	var sb strings.Builder
	for _, task := range p.Tasks {
		if !task.Done || task.Result == "" {
			continue
		}
		sb.WriteString("子任务")
		sb.WriteString(strconv.Itoa(task.ID))
		sb.WriteString(": ")
		sb.WriteString(task.Result)
		sb.WriteString("\n")
	}
	return strings.TrimSuffix(sb.String(), "\n")
}

// Decision 规划器针对「当前已完成进度」给出的下一步决策。
type Decision struct {
	Action string `json:"action"` // "run" 继续执行下一个 worker；"final" 直接汇总收尾
	Task   string `json:"task"`   // action=run 时：给下一个 worker 的唯一任务（可内联已判定条件与数值）
	Reason string `json:"reason"` // 决策理由（final 时用于说明为何不再操作）
}

// ResultItem 传给决策器的「已完成子任务」最小结构化输入。
type ResultItem struct {
	Status      string
	Description string
	Result      string
}

// decideSys 决策器系统提示词：只允许 run/final，逼近「读结果 → 判断条件 → 决定下一步」。
const decideSys = `你是「任务规划-决策者」。基于原始目标与已完成子任务的结构化结果，决定是否还需要一个 worker。
规则：
- 还需要进一步操作 → 输出 {"action":"run","task":"给下一个 worker 的唯一任务，可内联已算出的数值或条件结论","reason":"..."}
- 条件不满足、无需更多写操作、或已有结果足以直接回答原始目标 → 输出 {"action":"final","task":"","reason":"..."}
- 只允许 action 为 run 或 final；只输出一个 JSON 对象，不要额外文字。`

// DecideNext 依据已完成子任务的结构化结果，决定下一步动作。
func (p *Planner) DecideNext(ctx context.Context, model agent.ChatModel, goal string, results []ResultItem) (Decision, error) {
	if model == nil {
		return Decision{}, errors.New("决策生成需要模型端口")
	}
	var sb strings.Builder
	sb.WriteString("原始目标：" + goal + "\n\n已完成子任务：\n")
	if len(results) == 0 {
		sb.WriteString("（尚无任何子任务结果，请给出第一步任务）\n")
	}
	for i, r := range results {
		if r.Status == "" {
			r.Status = "ok"
		}
		sb.WriteString(strconv.Itoa(i+1) + ". [" + r.Status + "] " + r.Description + "：")
		sb.WriteString(r.Result + "\n")
	}
	reply, err := model.Generate(ctx, agent.ChatRequest{
		Model:       "",
		Messages:    []agent.ChatMessage{{Role: session.RoleSystem, Content: decideSys}, {Role: session.RoleUser, Content: sb.String()}},
		Temperature: 0,
	})
	if err != nil {
		return Decision{}, err
	}
	d, ok := parseDecision(reply.Content)
	if !ok || d.Action == "" || (d.Action != "run" && d.Action != "final") {
		// 解析失败或非法动作：保守收尾，避免无脑写操作
		return Decision{Action: "final", Reason: "决策结果无法解析，保守结束"}, nil
	}
	return d, nil
}

// parseDecision 剥掉 ```json 围栏、取首个 JSON 对象解析为 Decision。
// 解析失败返回 ok=false。
func parseDecision(reply string) (Decision, bool) {
	text := strings.TrimSpace(reply)
	if i := strings.Index(text, "{"); i >= 0 {
		text = text[i:]
	} else {
		return Decision{}, false
	}
	if j := strings.LastIndex(text, "}"); j > 0 {
		text = text[:j+1]
	} else {
		return Decision{}, false
	}
	var d Decision
	if err := json.Unmarshal([]byte(text), &d); err != nil {
		return Decision{}, false
	}
	return d, true
}
