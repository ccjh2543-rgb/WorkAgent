package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	domainagent "simpleAgent/domain/agent"
	"simpleAgent/domain/session"
)

// maxCompactionSummaryBytes 单条压缩摘要的落盘上限。
// 摘要本身已经在预算内，这里只防御异常输入；截断在 JSON 编码前完成，
// 保证恢复时仍能解析出合法载荷。
const maxCompactionSummaryBytes = 128 * 1024

// compactionPayload 压缩记录的持久化载荷。
type compactionPayload struct {
	Summary string `json:"summary"`
}

// persistCompaction 把当前摘要追加到会话树，供下次 Run 直接复用。
//
// 记录挂在当前 HEAD 之后，因此回滚到更早的消息会自动让它失效；
// 分叉则会把记录一起复制到新分支，行为与普通消息一致。
// 落盘失败只记录日志：压缩结果仍在本轮内存中生效，不影响任务继续。
func (a *Agent) persistCompaction(ctx context.Context) {
	if a.sessStore == nil || strings.TrimSpace(a.compaction.summary) == "" {
		return
	}
	payload := compactionPayload{
		Summary: session.Truncate(a.compaction.summary, maxCompactionSummaryBytes),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("[压缩摘要序列化失败]:%v\n", err)
		return
	}
	message := session.Message{
		Kind:    session.KindCompaction,
		Role:    session.RoleSystem,
		Content: string(data),
	}
	if err := a.sessStore.Append(ctx, a.sessionID, message); err != nil {
		fmt.Printf("[压缩摘要落盘失败]:%v\n", err)
		return
	}
	a.persistedCompaction = a.compaction.compactedCount
}

// restoreCompaction 从历史消息中恢复摘要。
//
// 压缩记录出现的位置就是覆盖边界：它之前的消息都可以被摘要替代，
// 因此每遇到一条记录就清空已累积的条目，只保留记录之后的新消息。
// 多条记录时后一条覆盖前一条，符合「增量摘要」的生成方式。
func restoreCompaction(messages []session.Message) ([]domainagent.Entry, string) {
	entries := make([]domainagent.Entry, 0, len(messages))
	summary := ""
	for _, message := range messages {
		if message.Kind == session.KindCompaction {
			var payload compactionPayload
			if err := json.Unmarshal([]byte(message.Content), &payload); err == nil {
				if trimmed := strings.TrimSpace(payload.Summary); trimmed != "" {
					summary = trimmed
					entries = entries[:0]
				}
			}
			continue
		}
		entries = append(entries, domainagent.Entry{
			Kind:    message.Kind,
			Role:    message.Role,
			Content: message.Content,
		})
	}
	return entries, summary
}
