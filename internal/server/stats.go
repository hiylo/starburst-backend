package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// statsCacheTTL 是统计接口的短 TTL 缓存窗口：TaskStats + TokenUsageByAudit +
// CountArchives 每请求都是三次全表聚合，App/控制台高频刷新时会放大 DB 压力。
const statsCacheTTL = 10 * time.Second

// statsEntry 缓存一次统计聚合的三个结果。
type statsEntry struct {
	at         time.Time
	taskStats  *store.TaskStats
	tokenUsage []*store.TokenUsage
	archives   int
}

// statsCache 按 Server 实例缓存：生产单实例即单条目；测试各建 Server 互不干扰。
var statsCache sync.Map // *Server → *statsEntry

// handleStats reports usage statistics. Requires a web session (admin) or an APP token.
// Read-only usage data is safe to expose to the App without the admin web login.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
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

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if e, ok := cachedStats(s); ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"tasks":          e.taskStats,
			"tokenUsage":     e.tokenUsage,
			"archives":       e.archives,
			"maxConcurrency": s.maxConcurrency,
		})
		return
	}

	taskStats, err := s.store.TaskStats(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "task stats failed")
		return
	}
	tokenUsage, err := s.store.TokenUsageByAudit(ctx, 20)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "token usage failed")
		return
	}
	archives, err := s.store.CountArchives(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "archive count failed")
		return
	}
	storeStats(s, taskStats, tokenUsage, archives)

	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":          taskStats,
		"tokenUsage":     tokenUsage,
		"archives":       archives,
		"maxConcurrency": s.maxConcurrency,
	})
}

// cachedStats 返回 TTL 窗口内的统计聚合；命中时跳过三次全表查询。
func cachedStats(s *Server) (*statsEntry, bool) {
	v, ok := statsCache.Load(s)
	if !ok {
		return nil, false
	}
	e := v.(*statsEntry)
	if time.Since(e.at) < statsCacheTTL {
		return e, true
	}
	statsCache.Delete(s)
	return nil, false
}

// storeStats 写入本次统计聚合供后续请求复用。
func storeStats(s *Server, taskStats *store.TaskStats, tokenUsage []*store.TokenUsage, archives int) {
	statsCache.Store(s, &statsEntry{at: time.Now(), taskStats: taskStats, tokenUsage: tokenUsage, archives: archives})
}
