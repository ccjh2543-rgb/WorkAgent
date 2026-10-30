package agent

// ContextReport 一次上下文装配的分区用量与压缩事实。
//
// 它回答三个问题：上下文被哪一层吃掉、压缩省了多少、工具输出占比是否过高。
// 分区口径与 prepareContext 的装配顺序一一对应：
//   - Policy   系统提示词（规则与工具契约）
//   - Memory   长期记忆检索块
//   - Anchor   任务锚点（目标、计划与进度）
//   - Summary  压缩摘要
//   - Window   最近消息窗口（含工具往返）
type ContextReport struct {
	PolicyTokens     int     `json:"policy_tokens"`     // 系统提示词占用
	MemoryTokens     int     `json:"memory_tokens"`     // 记忆块占用
	AnchorTokens     int     `json:"anchor_tokens"`     // 任务锚点占用
	SummaryTokens    int     `json:"summary_tokens"`    // 压缩摘要占用
	WindowTokens     int     `json:"window_tokens"`     // 最近窗口占用
	TotalTokens      int     `json:"total_tokens"`      // 本次请求总量
	BudgetTokens     int     `json:"budget_tokens"`     // 当前上下文预算
	Messages         int     `json:"messages"`          // 消息条数
	ToolOutputShare  float64 `json:"tool_output_share"` // 工具结果占总量比例，0-1
	FoldedEntries    int     `json:"folded_entries"`    // 本轮被折叠的条目数
	CompactedEntries int     `json:"compacted_entries"` // 已并入摘要的条目总数
	CompactionRounds int     `json:"compaction_rounds"` // 本次装配触发的压缩轮数
}
