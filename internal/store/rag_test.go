package store

import (
	"context"
	"testing"
)

func TestVectorStringRoundTrip(t *testing.T) {
	in := []float32{0.5, -0.25, 1.0, 0.0}
	s := VectorString(in)
	got, err := parseVector(s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != len(in) {
		t.Fatalf("len %d want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("idx %d: got %v want %v", i, got[i], in[i])
		}
	}
}

func TestCosineSimilarity(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{1, 0, 0}
	if cosine(a, b) != 1.0 {
		t.Fatalf("identical vectors should be 1.0, got %v", cosine(a, b))
	}
	orth := []float32{0, 1, 0}
	if cosine(a, orth) != 0.0 {
		t.Fatalf("orthogonal vectors should be 0.0, got %v", cosine(a, orth))
	}
	opp := []float32{-1, 0, 0}
	if cosine(a, opp) != -1.0 {
		t.Fatalf("opposite vectors should be -1.0, got %v", cosine(a, opp))
	}
}

func TestRagChunkLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	chunks := []*RagChunk{
		{ModuleID: 1, Kind: "entity", Title: "用户表", Content: "用户账号信息",
			SourceFile: "User.java", SourceLine: 12, Embedding: []float32{1, 0, 0}},
		{ModuleID: 1, Kind: "endpoint", Title: "GET /api/users", Content: "用户列表接口",
			SourceFile: "UserController.java", SourceLine: 45, Embedding: []float32{0.9, 0.1, 0}},
		{ModuleID: 2, Kind: "entity", Title: "订单表", Content: "订单信息",
			SourceFile: "Order.java", SourceLine: 8, Embedding: []float32{0, 1, 0}},
	}
	if err := st.ReplaceProjectChunks(ctx, 7, chunks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	n, err := st.CountProjectChunks(ctx, 7)
	if err != nil || n != 3 {
		t.Fatalf("count = %d, %v", n, err)
	}

	// Query for the "用户" table: should rank the 用户表 chunk first.
	got, err := st.SearchRagChunks(ctx, 7, 0, []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Title != "用户表" {
		t.Fatalf("top result %q want 用户表", got[0].Title)
	}

	// Module filter returns only that module's chunks.
	mod, err := st.SearchRagChunks(ctx, 7, 2, []float32{0, 0, 1}, 5)
	if err != nil {
		t.Fatalf("search module: %v", err)
	}
	if len(mod) != 1 || mod[0].Title != "订单表" {
		t.Fatalf("module filter: %+v", mod)
	}

	if err := st.DeleteProjectChunks(ctx, 7); err != nil {
		t.Fatalf("delete: %v", err)
	}
	n, _ = st.CountProjectChunks(ctx, 7)
	if n != 0 {
		t.Fatalf("after delete count = %d", n)
	}
}
