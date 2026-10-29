package server

import (
	"encoding/json"
	"net/http"

	appagent "simpleAgent/application/agent"
)

// 本文件实现 WorkBench 评估基准的桥接 HTTP 端点。
// simpleAgent 侧的可恢复循环见 application/agent/workbench.go；
// 这里仅负责把 HTTP 请求转成对 Agent.StartWorkbench / ContinueWorkbench 的调用，
// 并返回同一份结果。

// wbStartRequest POST /api/workbench/start 请求体。
type wbStartRequest struct {
	Task      string   `json:"task"`               // 任务指令
	ToolDescs []string `json:"tool_descs"`         // 可用工具的说明行
	Datetime  string   `json:"datetime,omitempty"` // 评估系统时间（可选）
	MaxIter   int      `json:"max_iter,omitempty"` // 单任务工具步数上限（可选）
	Mode      string   `json:"mode,omitempty"`     // 执行模式：空=单Agent循环；planner_worker=规划/执行分离
}

// wbContinueRequest POST /api/workbench/continue 请求体。
type wbContinueRequest struct {
	SessionID   string `json:"session_id"`  // workbench 会话 ID
	Observation string `json:"observation"` // 工具执行后的观察结果
}

// handleWorkbenchStart 启动一个 WorkBench 任务运行器，返回首轮结果。
func (s *Server) handleWorkbenchStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req wbStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Task == "" {
		writeErr(w, http.StatusBadRequest, "task 不能为空")
		return
	}
	var sid string
	var res appagent.WbResult
	var err error
	if req.Mode == "planner_worker" {
		sid, res, err = s.agent.StartPlannerWorker(r.Context(), req.Task, req.ToolDescs, req.Datetime, req.MaxIter)
	} else {
		sid, res, err = s.agent.StartWorkbench(r.Context(), req.Task, req.ToolDescs, req.Datetime, req.MaxIter)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     sid,
		"state":          res.State,
		"tool_name":      res.ToolName,
		"tool_arg":       res.ToolArg,
		"answer":         res.Answer,
		"function_calls": res.FunctionCalls,
	})
}

// handleWorkbenchContinue 注入工具 observation，继续前一个暂停的运行器循环。
func (s *Server) handleWorkbenchContinue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req wbContinueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "session_id 不能为空")
		return
	}
	var res appagent.WbResult
	var err error
	if s.agent.IsPlannerWorker(req.SessionID) {
		res, err = s.agent.ContinuePlannerWorker(r.Context(), req.SessionID, req.Observation)
	} else {
		res, err = s.agent.ContinueWorkbench(r.Context(), req.SessionID, req.Observation)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":          res.State,
		"tool_name":      res.ToolName,
		"tool_arg":       res.ToolArg,
		"answer":         res.Answer,
		"function_calls": res.FunctionCalls,
	})
}

// handleWorkbenchClose 释放一个运行器状态（可选的清理端点）。
func (s *Server) handleWorkbenchClose(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	if sessionID == "" {
		writeErr(w, http.StatusBadRequest, "session_id 不能为空")
		return
	}
	if s.agent.IsPlannerWorker(sessionID) {
		s.agent.ClosePlannerWorker(sessionID)
	} else {
		s.agent.CloseWorkbench(sessionID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
