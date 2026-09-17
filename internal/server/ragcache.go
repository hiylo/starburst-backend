package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// retrievalCache returns the stable context + sources for an identical
// question deterministically (same key -> same retrieval), and skips a
// repeated embedding + similarity scan. Bounded LRU-ish eviction keeps memory
// flat: identical queries hit, distinct queries rotate out.
type retrievalCache struct {
	mu    sync.Mutex
	max   int
	m     map[string]retrievalHit
	order []string
}

// retrievalHit is one cached retrieval result.
type retrievalHit struct {
	projectID int64
	context   string
	sources   []map[string]any
}

func newRetrievalCache(max int) *retrievalCache {
	return &retrievalCache{max: max, m: map[string]retrievalHit{}}
}

func (c *retrievalCache) get(key string) (retrievalHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.m[key]
	if !ok {
		return retrievalHit{}, false
	}
	// move to front (most recently used)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			break
		}
	}
	return h, true
}

func (c *retrievalCache) put(key string, h retrievalHit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	c.m[key] = h
	if len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
}

// invalidateProject drops every cached retrieval for a project. It is called
// after the project's knowledge-base index is rebuilt, so an identical question
// re-runs retrieval against the fresh chunks instead of a stale cache.
func (c *retrievalCache) invalidateProject(projectID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, h := range c.m {
		if h.projectID == projectID {
			delete(c.m, k)
		}
	}
	out := c.order[:0]
	for _, k := range c.order {
		if _, ok := c.m[k]; ok {
			out = append(out, k)
		}
	}
	c.order = out
}

// ragRetrievalCache is the process-wide retrieval cache (bounded, LRU-ish).
var ragRetrievalCache = newRetrievalCache(128)

// retrievalCacheKey hashes project + question + limit into a stable cache key.
// Limit is part of the key so a top-10 hit is never served to a top-20 request
// (the cached sources would be a strict subset and silently under-recall).
func retrievalCacheKey(projectID int64, question string, limit int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d", projectID, question, limit)))
	return hex.EncodeToString(sum[:])
}
