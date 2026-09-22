package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// kbSearchCache caches knowledge-base retrieval results so the same question
// asked again (a common pattern in a multi-turn conversation) skips the
// embedding round trip and the pgvector scan. It is invalidated in full on
// every KB mutation (ingest / replace / delete), because a stale hit would cite
// a chunk that no longer exists or has been superseded.
//
// Keyed by (query, collection scope, topK, minScore): the scope is part of the
// key so a splice narrowed to one collection never serves a hit retrieved from
// every collection.
type kbSearchCache struct {
	mu    sync.Mutex
	max   int
	m     map[string][]kbHit
	order []string
}

func newKBSearchCache(max int) *kbSearchCache {
	return &kbSearchCache{max: max, m: map[string][]kbHit{}}
}

// get returns a copy of the cached hits so callers cannot mutate the cache.
func (c *kbSearchCache) get(key string) ([]kbHit, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hits, ok := c.m[key]
	if !ok {
		return nil, false
	}
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			c.order = append(c.order, key)
			break
		}
	}
	out := make([]kbHit, len(hits))
	copy(out, hits)
	return out, true
}

func (c *kbSearchCache) put(key string, hits []kbHit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; !ok {
		c.order = append(c.order, key)
	}
	stored := make([]kbHit, len(hits))
	copy(stored, hits)
	c.m[key] = stored
	if len(c.order) > c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.m, oldest)
	}
}

// invalidateAll drops every cached retrieval. Called after any write to the KB
// (document ingest/replace/delete, collection delete) so the next search sees
// the new chunk set.
func (c *kbSearchCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = map[string][]kbHit{}
	c.order = c.order[:0]
}

// kbCache is the process-wide KB retrieval cache (bounded, LRU-ish).
var kbCache = newKBSearchCache(128)

// kbSearchCacheKey hashes the retrieval parameters into a stable cache key.
// Collection ids are sorted so [2,1] and [1,2] share one entry.
func kbSearchCacheKey(query string, collectionIDs []int64, topK int, minScore float64) string {
	ids := append([]int64(nil), collectionIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var sb strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&sb, "%d,", id)
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%.4f", sb.String(), query, topK, minScore)))
	return hex.EncodeToString(sum[:])
}
