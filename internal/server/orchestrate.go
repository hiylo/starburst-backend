package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"
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

// handleProjects lists OpenCode sessions grouped by working directory. Each
// project carries its directory as id plus its session count, so clients can
// drill down via /api/projects/{id}/sessions. Requires a web session or APP
// token.
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

	// 会话元数据的 directory 字段统一是工作区根（/workspaces，projectID 全为
	// 'global'），按它分组只能得到一个目录。项目列表改用上游 project 集合——
	// 每个项目带真实 worktree 子目录，这与用户对"不同项目在不同目录"的认知
	// 一致。会话数按会话 title 与 worktree 叶目录名的关联统计（尽力而为）。
	rawProjects, err := s.openCode.ListProjects(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}
	projectsRaw := struct {
		Projects []struct {
			ID       string `json:"id"`
			Worktree string `json:"worktree"`
		} `json:"projects"`
	}{}
	if b, err := json.Marshal(rawProjects); err == nil {
		_ = json.Unmarshal(b, &projectsRaw)
	}
	upstreamProjects := projectsRaw.Projects
	// 上游可能直接返回数组（而非 {projects: [...]}）。
	if len(upstreamProjects) == 0 {
		var arr []struct {
			ID       string `json:"id"`
			Worktree string `json:"worktree"`
		}
		if b, err := json.Marshal(rawProjects); err == nil {
			if err := json.Unmarshal(b, &arr); err == nil {
				upstreamProjects = arr
			}
		}
	}

	items, err := s.openCode.ListSessions(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}
	titlesByDir := sessionsByWorktreeLeaf(items)

	projects := make([]projectSummary, 0, len(upstreamProjects))
	for _, p := range upstreamProjects {
		wt := strings.TrimRight(p.Worktree, "/")
		if wt == "" || wt == "/" {
			continue // 跳过 global 根项目，只列真实子目录
		}
		leaf := path.Base(wt)
		projects = append(projects, projectSummary{
			ID:           wt,
			Directory:    wt,
			Title:        p.ID,
			SessionCount: titlesByDir[leaf],
		})
	}
	if len(projects) == 0 {
		// 上游没有 project 元数据时退回按会话目录分组。
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
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// sessionsByWorktreeLeaf buckets session titles by the leaf path segment of
// their directory (/workspaces/components → "components"). Title may or may not
// mention the project; counting sessions whose title contains the leaf name
// is a best-effort association and only affects the displayed count.
func sessionsByWorktreeLeaf(items []opencode.SessionInfo) map[string]int {
	out := make(map[string]int)
	for _, it := range items {
		leaf := path.Base(strings.TrimRight(sessionDir(it), "/"))
		out[leaf]++
		// title 也做一次软匹配，提高命中率。
		if it.Title != "" {
			if s := path.Base(strings.TrimRight(it.Title, "/")); s != "" && s != leaf {
				out[s]++
			}
		}
	}
	return out
}

// projectSummary groups the sessions of one working directory.
type projectSummary struct {
	ID           string `json:"id"`
	Directory    string `json:"directory"`
	Title        string `json:"title,omitempty"`
	SessionCount int    `json:"sessionCount"`
}

// handleProjectSessions returns only the sessions whose working directory equals
// the path segment, so this is a real filter rather than a re-listing.
// Requires a web session or APP token.
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
