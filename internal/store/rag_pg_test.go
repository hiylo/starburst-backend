//go:build pgtest

package store

import (
	"context"
	"testing"
)

// TestPostgresRagSearch exercises the pgvector path: the vector extension is
// installed by the intel_rag migration, embeddings persist via the ?::vector
// cast and cosine retrieval orders by similarity.
func TestPostgresRagSearch(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	chunks := []*RagChunk{
		{ModuleID: 1, Kind: "entity", Title: "用户表", Content: "用户账号信息",
			SourceFile: "User.java", SourceLine: 12,
			Embedding: oneHot(1024, 0)},
		{ModuleID: 1, Kind: "entity", Title: "订单表", Content: "订单信息",
			SourceFile: "Order.java", SourceLine: 8,
			Embedding: oneHot(1024, 1)},
		{ModuleID: 1, Kind: "endpoint", Title: "GET /api/users", Content: "用户列表接口",
			SourceFile: "UserController.java", SourceLine: 45,
			Embedding: oneHot(1024, 2)},
	}
	if err := st.ReplaceProjectChunks(ctx, 1, chunks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := st.SearchRagChunks(ctx, 1, 0, oneHot(1024, 0), 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Title != "用户表" {
		t.Fatalf("top result %q want 用户表", got[0].Title)
	}
	if got[0].Similarity <= 0.99 {
		t.Fatalf("identical vector similarity %v should be ~1.0", got[0].Similarity)
	}

	n, err := st.CountProjectChunks(ctx, 1)
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

func oneHot(dim, idx int) []float32 {
	v := make([]float32, dim)
	v[idx] = 1
	return v
}
