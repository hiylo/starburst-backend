package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleBatch enqueues one task per target for a single prompt.
// Body: {"prompt":"...", "targets":[{"directory":"/a"},{"sessionId":"ses_..."}]}
// Requires an APP token. Returns created task ids.
func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}

	var req struct {
		Prompt  string `json:"prompt"`
		Targets []struct {
			Directory string `json:"directory"`
			SessionID string `json:"sessionId"`
		} `json:"targets"`
		// IntelProjectIDs 非空：并行触发测试智能 run-all 回归（与 prompt/targets 互斥）。
		IntelProjectIDs []int64 `json:"intelProjectIds"`
	}
	if !readBody(w, r, &req) {
		return
	}

	// 一次性批量任务数必须有限：单个请求灌入上千任务会撑爆 DB 与 worker
	// 队列，也放大超时窗口内的部分成功语义。
	const maxBatchTargets = 100

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// intel-run 批量：对每个项目触发 run-all 回归，返回 test run id。
	if len(req.IntelProjectIDs) > 0 {
		if req.Prompt != "" || len(req.Targets) > 0 {
			writeErr(w, http.StatusBadRequest, "intelProjectIds 与 prompt/targets 互斥")
			return
		}
		if len(req.IntelProjectIDs) > maxBatchTargets {
			writeErr(w, http.StatusBadRequest, "too many projects, max 100 per batch")
			return
		}
		runIDs := make([]int64, 0, len(req.IntelProjectIDs))
		for _, pid := range req.IntelProjectIDs {
			run, err := s.enqueueIntelRunAll(ctx, pid, false)
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Sprintf("触发 intel 回归失败(项目 %d): %v", pid, err))
				return
			}
			runIDs = append(runIDs, run.ID)
		}
		writeJSON(w, http.StatusCreated, map[string]any{"runIds": runIDs, "count": len(runIDs)})
		return
	}

	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if len(req.Targets) == 0 {
		writeErr(w, http.StatusBadRequest, "targets must not be empty")
		return
	}
	if len(req.Targets) > maxBatchTargets {
		writeErr(w, http.StatusBadRequest, "too many targets, max 100 per batch")
		return
	}

	created := make([]string, 0, len(req.Targets))
	for _, tg := range req.Targets {
		// 每个目标目录都会成为对应任务里 agent 的工作目录，逐个校验（一处不合法
		// 即整批拒绝，避免只建成一半任务）。
		if err := validateWorkDirectory(tg.Directory); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid directory: "+err.Error())
			return
		}
		if tg.SessionID != "" && !isValidSessionID(tg.SessionID) {
			writeErr(w, http.StatusBadRequest, "invalid sessionId")
			return
		}
		t := &store.Task{
			ID:        newTaskID(),
			SessionID: tg.SessionID,
			Directory: tg.Directory,
			Prompt:    req.Prompt,
		}
		if err := s.store.CreateTask(ctx, t); err != nil {
			writeErr(w, http.StatusInternalServerError, "create task failed")
			return
		}
		created = append(created, t.ID)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"created": created,
		"count":   len(created),
	})
}
