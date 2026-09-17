package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleSync implements the device/team config sync endpoints. GET pulls the
// latest snapshot for a key; PUT pushes a new snapshot (last-write-wins) and
// returns the new revision. Both require a web session (admin) or an APP token.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		s.getSync(w, r)
	case http.MethodPut:
		s.putSync(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// getSync returns the latest bundle for ?key=..., or 404 when the key has never
// been pushed.
func (s *Server) getSync(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	bundle, err := s.store.GetSyncBundle(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no sync bundle for key")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "get sync bundle failed")
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

// putSync upserts a snapshot for the given key and returns the new revision.
func (s *Server) putSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key     string `json:"key"`
		Payload string `json:"payload"`
	}
	if !readBody(w, r, &req) {
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		writeErr(w, http.StatusBadRequest, "key is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rev, err := s.store.PutSyncBundle(ctx, req.Key, req.Payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "put sync bundle failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": req.Key, "revision": rev})
}
