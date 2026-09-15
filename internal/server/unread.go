package server

import (
	"net/http"
	"strings"
)

// handleUnreadList returns the set of sessions flagged as having new/unread
// activity (GET /api/unread). Auth: APP token or web session.
func (s *Server) handleUnreadList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	m, err := s.store.ListUnread(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list unread failed")
		return
	}
	unread := make(map[string]bool, len(m))
	for id := range m {
		unread[id] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"unread": unread})
}

// handleUnreadMarkRead clears a session's unread flag (POST /api/unread/{id}).
// Called by either end (Web panel open / App session open); "read by any end"
// clears it for everyone.
func (s *Server) handleUnreadMarkRead(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/unread/")
	if id == "" || id == r.URL.Path || strings.Contains(id, "/") {
		writeErr(w, http.StatusBadRequest, "invalid session id")
		return
	}
	if err := s.store.MarkSessionRead(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "mark read failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
