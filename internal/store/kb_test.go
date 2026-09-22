package store

import (
	"context"
	"errors"
	"testing"
)

// TestKnowledgeBaseCRUD covers the generic KB tables on SQLite: collection
// create/list/get/delete, duplicate-name conflict, document lifecycle and chunk
// replacement.
func TestKnowledgeBaseCRUD(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	col, err := st.CreateKBCollection(ctx, "团队知识库", "需求与排期文档")
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	if col.ID == 0 || col.Name != "团队知识库" {
		t.Fatalf("collection = %+v", col)
	}
	if _, err := st.CreateKBCollection(ctx, "团队知识库", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name should conflict, got %v", err)
	}

	got, err := st.GetKBCollection(ctx, col.ID)
	if err != nil || got.Name != "团队知识库" {
		t.Fatalf("get = %+v, %v", got, err)
	}

	doc := &KBDocument{CollectionID: col.ID, Name: "排期.md", MIME: "text/markdown", SizeBytes: 42, Status: "pending"}
	if err := st.CreateKBDocument(ctx, doc); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if doc.ID == 0 {
		t.Fatal("doc id not assigned")
	}

	if err := st.ReplaceKBDocumentChunks(ctx, doc, []*KBChunk{
		{Seq: 1, Title: "概述", Content: "第一条内容", Embedding: []float32{0.1, 0.2}},
		{Seq: 2, Title: "细则", Content: "第二条内容", Embedding: []float32{0.3, 0.4}},
	}); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}
	if err := st.UpdateKBDocumentResult(ctx, doc.ID, "indexed", 2, ""); err != nil {
		t.Fatalf("update result: %v", err)
	}

	persisted, err := st.GetKBDocument(ctx, doc.ID)
	if err != nil || persisted.Status != "indexed" || persisted.ChunkCount != 2 {
		t.Fatalf("persisted = %+v, %v", persisted, err)
	}

	docs, err := st.ListKBDocuments(ctx, col.ID, 10)
	if err != nil || len(docs) != 1 {
		t.Fatalf("list docs = %d, %v", len(docs), err)
	}

	// Replace again: old chunk set must be wiped.
	if err := st.ReplaceKBDocumentChunks(ctx, doc, []*KBChunk{
		{Seq: 1, Title: "唯一", Content: "仅剩一条", Embedding: []float32{0.5}},
	}); err != nil {
		t.Fatalf("re-replace chunks: %v", err)
	}
	if err := st.UpdateKBDocumentResult(ctx, doc.ID, "indexed", 1, ""); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteKBDocument(ctx, doc.ID); err != nil {
		t.Fatalf("delete doc: %v", err)
	}
	if _, err := st.GetKBDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted doc should 404, got %v", err)
	}

	if err := st.DeleteKBCollection(ctx, col.ID); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	if _, err := st.GetKBCollection(ctx, col.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted collection should 404, got %v", err)
	}
}

// TestSearchKBChunksUnsupportedOnSQLite pins the product decision that vector
// retrieval is PostgreSQL/pgvector only: on a lightweight SQLite deployment the
// KB search must fail loudly rather than degrade silently.
func TestSearchKBChunksUnsupportedOnSQLite(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, err := st.SearchKBChunks(ctx, nil, []float32{0.1, 0.2, 0.3}, 5)
	if !errors.Is(err, ErrRagUnsupported) {
		t.Fatalf("search on SQLite = %v, want ErrRagUnsupported", err)
	}
}

// TestKnowledgeBaseCounters covers the collection-level aggregate refresh that
// ReplaceKBDocumentChunks triggers.
func TestKnowledgeBaseCounters(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	col, err := st.CreateKBCollection(ctx, "计数库", "")
	if err != nil {
		t.Fatal(err)
	}
	doc := &KBDocument{CollectionID: col.ID, Name: "a.md", Status: "pending"}
	if err := st.CreateKBDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKBDocumentChunks(ctx, doc, []*KBChunk{
		{Seq: 1, Title: "a", Content: "c1", Embedding: []float32{0.1}},
		{Seq: 2, Title: "b", Content: "c2", Embedding: []float32{0.2}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetKBCollection(ctx, col.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentCount != 1 || got.ChunkCount != 2 {
		t.Fatalf("counters = docs:%d chunks:%d, want 1/2", got.DocumentCount, got.ChunkCount)
	}
}
