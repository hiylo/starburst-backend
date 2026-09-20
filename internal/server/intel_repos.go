package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// handleIntelReposProbe pre-flights a repos_dir change: it reports the current
// cache location, how many projects are cached under it, and what already
// exists at the candidate path so the UI can show a two-step confirm before a
// destructive rebuild.
func (s *Server) handleIntelReposProbe(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	current := s.intelReposDir(ctx)
	cached, err := countCacheDirs(current)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计缓存失败: "+err.Error())
		return
	}
	probe := map[string]any{
		"current":         current,
		"candidate":       req.Path,
		"cachedProjects":  cached,
		"candidateExists": false,
		"candidateSize":   0,
	}
	if req.Path != "" {
		if fi, err := os.Stat(req.Path); err == nil {
			probe["candidateExists"] = true
			probe["candidateSize"] = fi.Size()
			if fi.IsDir() {
				probe["candidateSize"] = dirSize(req.Path)
			}
		}
	}
	writeJSON(w, http.StatusOK, probe)
}

// handleIntelReposRebuild applies a repos_dir change: persist the new path and,
// when deleteOld+confirm, remove the old cache directory so disk does not keep
// growing.
func (s *Server) handleIntelReposRebuild(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Path      string `json:"path"`
		DeleteOld bool   `json:"deleteOld"`
		Confirm   bool   `json:"confirm"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if !req.Confirm {
		writeErr(w, http.StatusBadRequest, "confirm is required for a destructive change")
		return
	}
	if req.Path == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	if err := validateWorkDirectory(req.Path); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid path: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	old := s.intelReposDir(ctx)
	if err := s.store.SetSetting(ctx, "intel.repos_dir", req.Path); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败: "+err.Error())
		return
	}
	if req.DeleteOld && old != req.Path {
		if err := os.RemoveAll(old); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除旧缓存失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reposDir": req.Path})
}

// countCacheDirs counts subdirectories under a repos_dir cache (each is one
// cloned project).
func countCacheDirs(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n, nil
}

// dirSize returns the total bytes under dir (bounded, best-effort).
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}
