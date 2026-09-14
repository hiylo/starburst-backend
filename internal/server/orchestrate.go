package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hiylo/startburst-backend/internal/opencode"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// projectIDFromPath extracts the project directory from /api/projects/{dir}.
// EscapedPath is used so an encoded slash (%2F) inside an absolute directory
// does not get mistaken for a path separator.
func projectIDFromPath(r *http.Request) (string, error) {
	raw := r.URL.EscapedPath()[len("/api/projects/"):]
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "", fmt.Errorf("missing project id")
	}
	return url.PathUnescape(raw)
}

// sessionDir returns the working directory that identifies a session's project,
// falling back to path when directory is empty.
func sessionDir(it opencode.SessionInfo) string {
	if it.Directory != "" {
		return it.Directory
	}
	return it.Path
}

// handleProjects lists OpenCode sessions grouped by working directory. Each
// project carries its directory as id plus its session count, so clients can
// drill down via /api/projects/{id}/sessions. Requires a valid APP token.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	items, err := s.openCode.ListSessions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}

	projects := make([]projectSummary, 0)
	seen := make(map[string]int)
	for _, it := range items {
		dir := sessionDir(it)
		if i, ok := seen[dir]; ok {
			projects[i].SessionCount++
			continue
		}
		seen[dir] = len(projects)
		projects = append(projects, projectSummary{ID: dir, Directory: dir, SessionCount: 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// projectSummary groups the sessions of one working directory.
type projectSummary struct {
	ID           string `json:"id"`
	Directory    string `json:"directory"`
	SessionCount int    `json:"sessionCount"`
}

// handleProjectSessions returns only the sessions whose working directory equals
// the path segment, so this is a real filter rather than a re-listing.
// Requires a valid APP token.
func (s *Server) handleProjectSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	id, err := projectIDFromPath(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	items, err := s.openCode.ListSessions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}

	sessions := make([]opencode.SessionInfo, 0)
	for _, it := range items {
		if sessionDir(it) == id {
			sessions = append(sessions, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}
