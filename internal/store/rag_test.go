package store

import (
	"context"
	"errors"
	"testing"
)

func TestVectorStringFormat(t *testing.T) {
	for _, tc := range []struct {
		in   []float32
		want string
	}{
		{nil, "[]"},
		{[]float32{}, "[]"},
		{[]float32{0.5, -0.25, 1, 0}, "[0.5,-0.25,1,0]"},
	} {
		if got := VectorString(tc.in); got != tc.want {
			t.Errorf("VectorString(%v) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestSearchRagChunksUnsupportedOnSQLite pins the product decision that a
// lightweight SQLite deployment has no vector retrieval at all: search must fail
// loudly instead of silently falling back to an in-process comparison that
// nothing in the UI advertises.
func TestSearchRagChunksUnsupportedOnSQLite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	chunks := []*RagChunk{
		{ModuleID: 1, Kind: "entity", Title: "用户表", Content: "用户账号信息",
			SourceFile: "User.java", SourceLine: 12, Embedding: []float32{1, 0, 0}},
	}
	if err := st.ReplaceProjectChunks(ctx, 7, chunks); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := st.SearchRagChunks(ctx, 7, 0, []float32{1, 0, 0}, 2)
	if !errors.Is(err, ErrRagUnsupported) {
		t.Fatalf("SearchRagChunks err = %v, want ErrRagUnsupported", err)
	}
	if got != nil {
		t.Fatalf("SearchRagChunks returned %d chunks on SQLite, want none", len(got))
	}
}

// TestRagChunkLifecycle covers the storage half of the RAG surface, which stays
// available without pgvector: chunks are written, counted and rebuilt. Only
// similarity search is unavailable on SQLite.
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
	if n, err := st.CountProjectChunks(ctx, 7); err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3", n, err)
	}

	// Replace rebuilds the project's set rather than appending to it.
	if err := st.ReplaceProjectChunks(ctx, 7, chunks[:1]); err != nil {
		t.Fatalf("re-replace: %v", err)
	}
	if n, err := st.CountProjectChunks(ctx, 7); err != nil || n != 1 {
		t.Fatalf("count after replace = %d, %v; want 1", n, err)
	}

	if err := st.DeleteProjectChunks(ctx, 7); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := st.CountProjectChunks(ctx, 7); n != 0 {
		t.Fatalf("after delete count = %d, want 0", n)
	}
}

// TestPGVectorInstalledFalseOnSQLite documents the gate that hides every
// vector-dependent intel entry in lightweight deployments.
func TestPGVectorInstalledFalseOnSQLite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	installed, err := st.PGVectorInstalled(ctx)
	if err != nil {
		t.Fatalf("PGVectorInstalled: %v", err)
	}
	if installed {
		t.Error("PGVectorInstalled = true on SQLite, want false")
	}
}
