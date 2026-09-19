package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// workflowStep is one step of a multi-step orchestration.
type workflowStep struct {
	Name          string `json:"name"` // 步骤名（写到任务 name）
	Prompt        string `json:"prompt"`
	Directory     string `json:"directory"` // 为空则用工作流默认目录
	TimeoutSec    int    `json:"timeoutSeconds"`
	Priority      int    `json:"priority"`
}

// workflowRequest creates a chained sequence of tasks sharing one workflow id.
type workflowRequest struct {
	Name       string         `json:"name"`
	Directory  string         `json:"directory"`
	Steps      []workflowStep `json:"steps"`
}

// handleWorkflowCreate creates one task per step, each depending on the
// previous (task[i].dependsOn = task[i-1].id), so the steps run strictly in
// sequence. All tasks share the same workflow_id for the orchestration page to
// render the chain and live status. Requires a web session or APP token.
func (s *Server) handleWorkflowCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req workflowRequest
	if !readBody(w, r, &req) {
		return
	}
	if len(req.Steps) == 0 {
		writeErr(w, http.StatusBadRequest, "steps are required")
		return
	}
	if err := validateWorkDirectory(req.Directory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid directory: "+err.Error())
		return
	}
	for i, st := range req.Steps {
		if st.Prompt == "" {
			writeErr(w, http.StatusBadRequest, "step "+strconv.Itoa(i+1)+" has empty prompt")
			return
		}
		if err := validateWorkDirectory(st.Directory); err != nil {
			writeErr(w, http.StatusBadRequest, "step "+strconv.Itoa(i+1)+" invalid directory: "+err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	workflowID := newTaskID()
	created := make([]string, 0, len(req.Steps))
	var prevID string
	for i, st := range req.Steps {
		directory := st.Directory
		if directory == "" {
			directory = req.Directory
		}
		t := &store.Task{
			ID:         newTaskID(),
			Directory:  directory,
			Name:       st.Name,
			Prompt:     st.Prompt,
			DependsOn:  prevID,
			Priority:   st.Priority,
			TimeoutSec: st.TimeoutSec,
			WorkflowID: workflowID,
		}
		status := store.TaskQueued
		if prevID != "" {
			status = store.TaskPending
		}
		if err := s.store.CreateTaskWithStatus(ctx, t, status); err != nil {
			writeErr(w, http.StatusInternalServerError, "create workflow failed")
			return
		}
		if i == 0 {
			s.pushTaskEvent(store.TaskQueued, map[string]any{"id": t.ID, "status": store.TaskQueued, "workflowId": workflowID})
		}
		created = append(created, t.ID)
		prevID = t.ID
	}

	s.pushTaskEvent("workflow.created", map[string]any{
		"workflowId": workflowID,
		"tasks":      created,
		"name":       req.Name,
	})
	writeJSON(w, http.StatusOK, map[string]any{"workflowId": workflowID, "tasks": created})
}

// handleWorkflowList returns recent orchestrations with a step status rollup
// (GET /api/workflows). Requires a web session or APP token.
func (s *Server) handleWorkflowList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ws, err := s.store.ListWorkflowSummaries(ctx, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list workflows failed")
		return
	}
	if ws == nil {
		ws = []*store.WorkflowSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": ws})
}

// handleWorkflowSteps serves one orchestration: GET /api/workflow/{id} (steps
// with live status), POST /api/workflow/{id}/cancel (cancel whole workflow) and
// POST /api/workflow/{id}/rerun (restart from the first unfinished step).
// Requires a web session or APP token.
func (s *Server) handleWorkflowSteps(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/workflow/")
	if id == "" || id == r.URL.Path {
		writeErr(w, http.StatusBadRequest, "missing workflow id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	action := ""
	if strings.HasSuffix(id, "/cancel") {
		action = "cancel"
		id = strings.TrimSuffix(id, "/cancel")
	} else if strings.HasSuffix(id, "/rerun") {
		action = "rerun"
		id = strings.TrimSuffix(id, "/rerun")
	}
	switch action {
	case "cancel":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		n, err := s.store.CancelWorkflow(ctx, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "cancel workflow failed")
			return
		}
		s.pushTaskEvent("workflow.canceled", map[string]any{"workflowId": id, "canceled": n})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "canceled": n})
		return
	case "rerun":
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		n, err := s.store.RerunWorkflowFromFailed(ctx, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "rerun workflow failed")
			return
		}
		s.pushTaskEvent("workflow.rerun", map[string]any{"workflowId": id, "reset": n})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reset": n})
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tasks, err := s.store.ListTasksByWorkflow(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list workflow steps failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"steps": tasks})
}

// handleTaskStats returns the aggregated task statistics window
// (GET /api/tasks/stats?days=N, default 7). Requires a web session or token.
func (s *Server) handleTaskStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	stats, err := s.store.TaskStatsDetailed(ctx, days)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task stats failed")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}