package server

import (
	"context"
	"net/http"
	"time"

	"github.com/hiylo/startburst-backend/internal/store"
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
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
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

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	created := make([]string, 0, len(req.Targets))
	for _, tg := range req.Targets {
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
