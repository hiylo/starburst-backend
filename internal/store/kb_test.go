package store

import (
	"context"
	"errors"
	"fmt"
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

	docs, err := st.ListKBDocuments(ctx, col.ID, 10, 0)
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
	// 文档删除必须级联清掉其 chunk。
	if n, err := st.CountKBChunks(ctx, col.ID); err != nil || n != 0 {
		t.Fatalf("chunks after doc delete = %d, %v (want 0)", n, err)
	}

	if err := st.DeleteKBCollection(ctx, col.ID); err != nil {
		t.Fatalf("delete collection: %v", err)
	}
	if _, err := st.GetKBCollection(ctx, col.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted collection should 404, got %v", err)
	}
	// 集合删除必须级联清掉其文档与 chunk。
	if n, err := st.CountKBChunks(ctx, col.ID); err != nil || n != 0 {
		t.Fatalf("chunks after collection delete = %d, %v (want 0)", n, err)
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

// TestKBDocumentsPagination covers offset paging: ListKBDocuments must skip
// offset rows and CountKBDocuments report the real total.
func TestKBDocumentsPagination(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	col, err := st.CreateKBCollection(ctx, "分页库", "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		d := &KBDocument{CollectionID: col.ID, Name: fmt.Sprintf("doc%d.md", i), Status: "pending"}
		if err := st.CreateKBDocument(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	total, err := st.CountKBDocuments(ctx, col.ID)
	if err != nil || total != 5 {
		t.Fatalf("total = %d, %v (want 5)", total, err)
	}
	// 第 3 页临界：limit=2 offset=4 只应剩 1 条；offset 越界返回空。
	if docs, err := st.ListKBDocuments(ctx, col.ID, 2, 0); err != nil || len(docs) != 2 {
		t.Fatalf("page0 = %d, %v", len(docs), err)
	}
	if docs, err := st.ListKBDocuments(ctx, col.ID, 2, 4); err != nil || len(docs) != 1 {
		t.Fatalf("page4 = %d, %v", len(docs), err)
	}
	if docs, err := st.ListKBDocuments(ctx, col.ID, 2, 99); err != nil || len(docs) != 0 {
		t.Fatalf("overflow page = %d, %v", len(docs), err)
	}
}

// TestFindKBDocumentByNameAndSuffixDelete covers the replace-building blocks:
// by-name resolution and generation-marker cascade delete.
func TestFindKBDocumentByNameAndSuffixDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	col, err := st.CreateKBCollection(ctx, "替换库", "")
	if err != nil {
		t.Fatal(err)
	}
	d := &KBDocument{CollectionID: col.ID, Name: "排期.md", Status: "indexed"}
	if err := st.CreateKBDocument(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKBDocumentChunks(ctx, d, []*KBChunk{
		{Seq: 1, Title: "a", Content: "需求", Embedding: []float32{0.1}},
		{Seq: 2, Title: "b", Content: "排期", Embedding: []float32{0.2}},
	}); err != nil {
		t.Fatal(err)
	}

	found, err := st.FindKBDocumentByName(ctx, col.ID, "排期.md")
	if err != nil || found == nil || found.ID != d.ID {
		t.Fatalf("find by name = %+v, %v", found, err)
	}
	if _, err := st.FindKBDocumentByName(ctx, col.ID, "不存在.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing doc should ErrNotFound, got %v", err)
	}

	// 集合内再放一版相同 docId 标记的生成产物，删除后缀后仅保留指定 doc。
	g1 := &KBDocument{CollectionID: col.ID, Name: "汇报-@doc9", Status: "indexed"}
	if err := st.CreateKBDocument(ctx, g1); err != nil {
		t.Fatal(err)
	}
	g2 := &KBDocument{CollectionID: col.ID, Name: "迭代-@doc9", Status: "indexed"}
	if err := st.CreateKBDocument(ctx, g2); err != nil {
		t.Fatal(err)
	}
	removed, err := st.DeleteKBDocumentsByNameSuffix(ctx, col.ID, "-@doc9")
	if err != nil || removed != 2 {
		t.Fatalf("suffix delete removed = %d, %v (want 2)", removed, err)
	}
	if _, err := st.GetKBDocument(ctx, g1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("g1 should be gone, got %v", err)
	}
	if got, err := st.GetKBDocument(ctx, d.ID); err != nil || got.Name != "排期.md" {
		t.Fatalf("unrelated doc must stay: %+v, %v", got, err)
	}
}

// TestUpdateKBCollection covers rename/re-describe: empty name keeps old one,
// duplicate name → ErrConflict, absent id → ErrNotFound.
func TestUpdateKBCollection(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	col, err := st.CreateKBCollection(ctx, "原名", "d")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateKBCollection(ctx, "别的", "")

	got, err := st.UpdateKBCollection(ctx, col.ID, "  新名  ", "新描述")
	if err != nil || got.Name != "新名" || got.Description != "新描述" {
		t.Fatalf("update = %+v, %v", got, err)
	}
	// 空 name → 保持原名。
	got, err = st.UpdateKBCollection(ctx, col.ID, "  ", "仅改描述")
	if err != nil || got.Name != "新名" || got.Description != "仅改描述" {
		t.Fatalf("keep-name update = %+v, %v", got, err)
	}
	// 重名 → ErrConflict。
	if _, err := st.UpdateKBCollection(ctx, col.ID, "别的", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup name should ErrConflict, got %v", err)
	}
	_ = other
	// 未知道 → ErrNotFound。
	if _, err := st.UpdateKBCollection(ctx, 99999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id should ErrNotFound, got %v", err)
	}
}
