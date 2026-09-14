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

	"github.com/hiylo/startburst-backend/internal/automation"
	"github.com/hiylo/startburst-backend/internal/push"
	"github.com/hiylo/startburst-backend/internal/store"
)

// handleTasks lists (GET), creates (POST) and purges finished (DELETE)
// orchestration tasks. All require a valid APP token.
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
	case http.MethodDelete:
		s.purgeFinishedTasks(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// purgeFinishedTasks deletes terminal tasks (succeeded/failed/canceled) older
// than ?olderThan= seconds (default 0 = all finished). A finished task still
// referenced by a dependent is kept. Returns {"deleted", "kept"}.
func (s *Server) purgeFinishedTasks(w http.ResponseWriter, r *http.Request) {
	olderThan := 0 * time.Second
	if v := r.URL.Query().Get("olderThan"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			olderThan = time.Duration(n) * time.Second
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	deleted, kept, err := s.store.PurgeFinishedTasks(ctx, olderThan, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "purge finished tasks failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted, "kept": kept})
}

// handleTasksStatus lists tasks filtered by ?status=.
// listTasks returns one page of tasks as {"tasks", "total", "limit", "offset"}.
// ?status= filters, ?limit= (default 50, max 500) and ?offset= (default 0)
// page; total is the unfiltered-by-page count so a client knows whether a
// next page exists.
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

// createTask accepts {"name"?,"prompt","sessionId"?,"directory"?,"dependsOn"?,
// "scheduledAt"?,"cron"?} and enqueues it. Scheduling (scheduledAt = one-shot
// future, cron = recurring) marks the task "scheduled"; the scheduler later
// promotes it to queued. When dependsOn points at an existing non-terminal task
// the new task is created as pending and is only re-queued once upstream succeeds.
func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Prompt      string `json:"prompt"`
		SessionID   string `json:"sessionId"`
		Directory   string `json:"directory"`
		DependsOn   string `json:"dependsOn"`
		ScheduledAt string `json:"scheduledAt"`
		Cron        string `json:"cron"`
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

	var scheduledAt *time.Time
	if req.ScheduledAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.ScheduledAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "scheduledAt must be RFC3339")
			return
		}
		scheduledAt = &parsed
	}
	if req.Cron != "" {
		if _, err := automation.ParseCron(req.Cron); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid cron: "+err.Error())
			return
		}
	}

	t := &store.Task{
		ID:          newTaskID(),
		SessionID:   req.SessionID,
		Directory:   req.Directory,
		Name:        req.Name,
		Prompt:      req.Prompt,
		DependsOn:   req.DependsOn,
		ScheduledAt: scheduledAt,
		Cron:        req.Cron,
	}

	// Determine initial status. Scheduling takes precedence over dependency:
	// a scheduled task is gated by time, not by an upstream task.
	status := store.TaskQueued
	switch {
	case req.Cron != "":
		status = store.TaskScheduled
	case scheduledAt != nil && scheduledAt.After(time.Now()):
		status = store.TaskScheduled
	default:
		// Validate the dependency before creating so a typo fails fast instead
		// of leaving a task pending forever.
		if req.DependsOn != "" {
			up, err := s.store.GetTask(ctx, req.DependsOn)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "dependsOn task not found")
				return
			}
			switch up.Status {
			case store.TaskSucceeded:
				// Already done: run immediately.
			case store.TaskQueued, store.TaskRunning, store.TaskPending, store.TaskScheduled:
				status = store.TaskPending
			default:
				writeErr(w, http.StatusBadRequest, "dependsOn task already finished with status "+up.Status)
				return
			}
		}
	}

	if err := s.store.CreateTaskWithStatus(ctx, t, status); err != nil {
		writeErr(w, http.StatusInternalServerError, "create task failed")
		return
	}
	if status == store.TaskScheduled {
		s.pushTaskEvent(store.TaskScheduled, map[string]any{"id": t.ID, "status": store.TaskScheduled})
	}
	// Echo the stored row: the INSERT stamps created_at/updated_at/available_at
	// in SQL, so the in-memory struct would report zero times.
	created, err := s.store.GetTask(ctx, t.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "create task failed")
		return
	}
	writeJSON(w, http.StatusCreated, created)
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
			// Scheduled tasks (one-shot or recurring) are not covered by
			// CancelTask, so try the scheduled-specific path.
			sc, err2 := s.store.CancelScheduledTask(ctx, id)
			if err2 != nil {
				writeErr(w, http.StatusInternalServerError, "cancel failed")
				return
			}
			if !sc {
				writeErr(w, http.StatusConflict, "task already finished")
				return
			}
			s.pushTaskEvent(store.TaskCanceled, map[string]any{"id": id, "status": store.TaskCanceled})
			writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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
