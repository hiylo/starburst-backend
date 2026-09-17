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
	context string
	sources []map[string]any
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

// ragRetrievalCache is the process-wide retrieval cache (bounded, LRU-ish).
var ragRetrievalCache = newRetrievalCache(128)

// retrievalCacheKey hashes project + question into a stable cache key.
func retrievalCacheKey(projectID int64, question string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s", projectID, question)))
	return hex.EncodeToString(sum[:])
}
