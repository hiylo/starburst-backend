package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
		"current":                current,
		"candidate":              req.Path,
		"cachedProjects":         cached,
		"candidateExists":        false,
		"candidateSize":          0,
		"candidateSizeTruncated": false,
	}
	if req.Path != "" {
		// 与 rebuild 同套路径校验：拒绝系统目录与 `..` 越界，避免对危险目录
		// 做全树遍历把 CPU/IO 打满。
		if err := validateWorkDirectory(req.Path); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid path: "+err.Error())
			return
		}
		if fi, err := os.Stat(req.Path); err == nil {
			probe["candidateExists"] = true
			probe["candidateSize"] = fi.Size()
			if fi.IsDir() {
				size, truncated := dirSize(req.Path)
				probe["candidateSize"] = size
				probe["candidateSizeTruncated"] = truncated
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
	// 新旧路径归一化后比较（/foo 与 /foo/ 视图一致）；删除旧目录前同样做路径
	// 校验并拒绝空串与文件系统根目录，防止曾设置的危险目录被递归删除。
	oldClean := filepath.Clean(old)
	newClean := filepath.Clean(req.Path)
	if req.DeleteOld && oldClean != newClean {
		if strings.TrimSpace(oldClean) == "" || oldClean == "/" {
			writeErr(w, http.StatusBadRequest, "旧缓存目录无效（不能为空或文件系统根目录）")
			return
		}
		if err := validateWorkDirectory(oldClean); err != nil {
			writeErr(w, http.StatusBadRequest, "旧缓存目录无效: "+err.Error())
			return
		}
		if err := os.RemoveAll(oldClean); err != nil {
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

// dirSize 限制单次目录体积统计的上限：超过大小/深度即截断并返回 truncated，
// 防止对超大/网络挂载目录的全树遍历把 CPU/IO 打满（probe 是交互式预检，
// 不需要精确值）。
const (
	dirSizeMaxBytes = 2 << 30 // 2GB
	dirSizeMaxDepth = 6       // 6 层
)

// dirSize returns the total bytes under dir, walking at most dirSizeMaxDepth
// levels and stopping once dirSizeMaxBytes is exceeded. The second return
// reports whether the result was truncated (limits hit).
func dirSize(dir string) (total int64, truncated bool) {
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= dirSizeMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if total >= dirSizeMaxBytes {
			truncated = true
			return filepath.SkipAll
		}
		if fi, err := d.Info(); err == nil {
			total += fi.Size()
			if total >= dirSizeMaxBytes {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	return total, truncated
}
