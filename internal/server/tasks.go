package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/hiylo/opencode-backend/internal/push"
	"github.com/hiylo/opencode-backend/internal/store"
)

// handleTasks lists (GET) and creates (POST) orchestration tasks.
// Both require a valid APP token.
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listTasks(w, r)
	case http.MethodPost:
		s.createTask(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleTasksStatus lists tasks filtered by ?status=.
// listTasks returns one page of tasks as {"tasks", "total", "limit", "offset"}.
// ?status= filters, ?limit= (default 50, max 500) and ?offset= (default 0)
// page; total is the count matching the status filter so a client knows
// whether a next page exists.
func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tasks, err := s.store.ListTasks(ctx, status, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list tasks failed")
		return
	}
	total, err := s.store.CountTasks(ctx, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "count tasks failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":  tasks,
		"total":  total,
		"limit":  limit,
		"offset": offset,
	})
}

// createTask accepts {"prompt","sessionId"?,"directory"?,"dependsOn"?} and
// enqueues it. When dependsOn points at an existing non-terminal task the new
// task is created as pending and is only re-queued once the upstream succeeds.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt    string `json:"prompt"`
		SessionID string `json:"sessionId"`
		Directory string `json:"directory"`
		DependsOn string `json:"dependsOn"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Validate the dependency before creating so a typo fails fast instead of
	// leaving a task pending forever.
	status := store.TaskQueued
	if req.DependsOn != "" {
		up, err := s.store.GetTask(ctx, req.DependsOn)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "dependsOn task not found")
			return
		}
		switch up.Status {
		case store.TaskSucceeded:
			// Already done: run immediately.
		case store.TaskQueued, store.TaskRunning, store.TaskPending:
			status = store.TaskPending
		default:
			writeErr(w, http.StatusBadRequest, "dependsOn task already finished with status "+up.Status)
			return
		}
	}

	t := &store.Task{
		ID:        newTaskID(),
		SessionID: req.SessionID,
		Directory: req.Directory,
		Prompt:    req.Prompt,
		DependsOn: req.DependsOn,
	}
	if err := s.store.CreateTaskWithStatus(ctx, t, status); err != nil {
		writeErr(w, http.StatusInternalServerError, "create task failed")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// handleTaskByID reads (GET), cancels (DELETE) or manually unblocks (POST) a
// single task.
func (s *Server) handleTaskByID(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	id := r.URL.Path[len("/api/tasks/"):]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing task id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		t, err := s.store.GetTask(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "task not found")
			return
		}
		writeJSON(w, http.StatusOK, t)
	case http.MethodDelete:
		canceled, err := s.store.CancelTask(ctx, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "cancel failed")
			return
		}
		if !canceled {
			writeErr(w, http.StatusConflict, "task already finished")
			return
		}
		// A canceled task can never succeed, so release its waiters as blocked.
		if n := s.blockDependents(ctx, id, "upstream task canceled"); n > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "blocked": n})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case http.MethodPost:
		// Manual unblock: a human fixed the upstream problem directly, so the
		// blocked dependent is re-queued without re-running the upstream.
		changed, err := s.store.UnblockTask(ctx, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "unblock failed")
			return
		}
		if !changed {
			writeErr(w, http.StatusConflict, "task is not blocked")
			return
		}
		s.pushTaskEvent(store.TaskQueued, map[string]any{
			"id":     id,
			"status": store.TaskQueued,
			"reason": "manual unblock",
		})
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// blockDependents marks the waiters of an aborted task as blocked and pushes one
// event per blocked task so every connected device is notified.
func (s *Server) blockDependents(ctx context.Context, id, reason string) int {
	ids, err := s.store.BlockDependents(ctx, id, reason)
	if err != nil {
		log.Printf("block dependents of %s: %v", id, err)
		return 0
	}
	for _, depID := range ids {
		s.pushTaskEvent(store.TaskBlocked, map[string]any{
			"id":       depID,
			"status":   store.TaskBlocked,
			"upstream": id,
			"reason":   reason,
		})
	}
	return len(ids)
}

// pushTaskEvent fans a task lifecycle event out to every connected client. The
// executor pushes these for tasks it runs; the HTTP layer pushes them for state
// changes it causes itself (cancel, manual unblock).
func (s *Server) pushTaskEvent(status string, payload map[string]any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.hub.Broadcast(push.Message{
		Type:     "task.event",
		Payload:  b,
		Severity: push.SeverityFor(status),
	})
}

func newTaskID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("task_%s", hex.EncodeToString(buf))
}
