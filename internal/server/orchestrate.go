package server

import (
	"context"
	"encoding/json"
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
	// CheckOrigin 默认全放行，否则无法服务非浏览器客户端（App/CLI 不带 Origin）。
	// 收到浏览器 Origin 时按同源策略校验，避免跨站 WebSocket 猜测性探测。
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true // 非浏览器客户端
		}
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return u.Host == r.Host
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

// handleProjects lists OpenCode sessions grouped by real working directory.
// The plain /session endpoint normalizes every session to projectID 'global'
// and a flat root directory (/workspaces), so grouping by it yields one group.
// /experimental/session returns each session's real directory; we group on that
// and then merge in the /project worktrees that have no sessions so empty
// projects are still visible. Requires a web session or APP token.
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// 1) 全量详细会话（每个带真实 directory）。
	raw, err := s.openCode.ListAllSessionsDetailed(ctx, "")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}
	var sessions []opencode.SessionInfo
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &sessions)
	}

	// 2) 按真实目录统计会话数。
	order := make([]string, 0, len(sessions))
	counts := make(map[string]int)
	for _, it := range sessions {
		dir := strings.TrimRight(sessionDir(it), "/")
		if dir == "" {
			continue
		}
		if _, ok := counts[dir]; !ok {
			order = append(order, dir)
		}
		counts[dir]++
	}

	// 3) 合并 /project worktree（无会话的项目也显示，目录导航价值）。
	seen := make(map[string]bool, len(order))
	for _, d := range order {
		seen[d] = true
	}
	if rawProjects, err := s.openCode.ListProjects(ctx); err == nil {
		var upstream []struct {
			ID       string `json:"id"`
			Worktree string `json:"worktree"`
		}
		if b, err := json.Marshal(rawProjects); err == nil {
			if err := json.Unmarshal(b, &upstream); err != nil {
				upstream = nil
			}
		}
		for _, p := range upstream {
			wt := strings.TrimRight(p.Worktree, "/")
			if wt == "" || wt == "/" || seen[wt] {
				continue
			}
			seen[wt] = true
			order = append(order, wt)
		}
	}

	// 4) 组装输出。带会话的目录排前面。
	projects := make([]projectSummary, 0, len(order))
	noSession := make([]projectSummary, 0)
	for _, d := range order {
		ps := projectSummary{ID: d, Directory: d, SessionCount: counts[d]}
		if counts[d] > 0 {
			projects = append(projects, ps)
		} else {
			noSession = append(noSession, ps)
		}
	}
	projects = append(projects, noSession...)
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// projectSummary groups the sessions of one working directory.
type projectSummary struct {
	ID           string `json:"id"`
	Directory    string `json:"directory"`
	Title        string `json:"title,omitempty"`
	SessionCount int    `json:"sessionCount"`
}

// handleProjectSessions returns the sessions whose real working directory
// equals the selected project directory, scoped via /experimental/session
// which (unlike the plain /session endpoint) preserves each session's true
// directory. Requires a web session or APP token.
func (s *Server) handleProjectSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	id, err := projectIDFromPath(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	raw, err := s.openCode.ListAllSessionsDetailed(ctx, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}
	var sessions []opencode.SessionInfo
	if b, err := json.Marshal(raw); err == nil {
		_ = json.Unmarshal(b, &sessions)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"directory": id,
		"sessions":  sessions,
	})
}
