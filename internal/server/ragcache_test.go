package server

import "testing"

func TestRetrievalCache(t *testing.T) {
	c := newRetrievalCache(2)
	hit := retrievalHit{context: "ctx-a", sources: []map[string]any{{"title": "a"}}}

	c.put("k1", hit)
	c.put("k2", retrievalHit{context: "ctx-b"})

	if got, ok := c.get("k1"); !ok || got.context != "ctx-a" {
		t.Fatalf("get k1 = %+v, %v", got, ok)
	}
	// Accessing k1 makes it most-recent; a third key evicts k2 (oldest).
	c.get("k1")
	c.put("k3", retrievalHit{context: "ctx-c"})
	if _, ok := c.get("k2"); ok {
		t.Error("k2 should have been evicted")
	}
	if _, ok := c.get("k1"); !ok {
		t.Error("k1 should survive")
	}
}

func TestRetrievalCacheKeyStable(t *testing.T) {
	a := retrievalCacheKey(7, "为什么接口会空")
	b := retrievalCacheKey(7, "为什么接口会空")
	if a != b {
		t.Errorf("key not stable for identical input: %s vs %s", a, b)
	}
	c := retrievalCacheKey(8, "为什么接口会空")
	if a == c {
		t.Error("key should differ across projects")
	}
	if len(a) != 64 {
		t.Errorf("key length = %d, want 64", len(a))
	}
}
