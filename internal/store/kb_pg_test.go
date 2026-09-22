//go:build pgtest

package store

import (
	"context"
	"testing"
)

// TestPostgresKBSearch exercises the generic knowledge-base path on pgvector:
// the knowledge_base migration creates kb_* tables + the HNSW index, chunks
// persist via the ?::vector cast and cosine retrieval orders by similarity.
func TestPostgresKBSearch(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	col, err := st.CreateKBCollection(ctx, "团队知识库", "需求文档")
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}
	doc := &KBDocument{CollectionID: col.ID, Name: "需求.md", MIME: "text/markdown", SizeBytes: 20, Status: "indexed"}
	if err := st.CreateKBDocument(ctx, doc); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if err := st.ReplaceKBDocumentChunks(ctx, doc, []*KBChunk{
		{Seq: 1, Title: "库存规则", Content: "安全库存等于日均出库量乘备货周期。", Embedding: oneHot(EmbedDim, 0)},
		{Seq: 2, Title: "订单规则", Content: "订单在付款后三十分钟内自动下发。", Embedding: oneHot(EmbedDim, 1)},
		{Seq: 3, Title: "结算规则", Content: "月度账单在下月五号前出具。", Embedding: oneHot(EmbedDim, 2)},
	}); err != nil {
		t.Fatalf("replace chunks: %v", err)
	}

	got, err := st.SearchKBChunks(ctx, []int64{col.ID}, oneHot(EmbedDim, 0), 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Title != "库存规则" || got[0].DocumentID != doc.ID {
		t.Fatalf("top result = %+v, want 库存规则 of doc %d", got[0], doc.ID)
	}
	if got[0].Source != "需求.md" {
		t.Fatalf("joined source = %q, want 需求.md (no per-chunk name lookups)", got[0].Source)
	}
	if got[0].Similarity <= 0.99 {
		t.Fatalf("identical-vector similarity %v should be ~1.0", got[0].Similarity)
	}

	// 空 collectionIDs = 全库检索；跨集合检索也应命中。
	all, err := st.SearchKBChunks(ctx, nil, oneHot(EmbedDim, 1), 5)
	if err != nil || len(all) == 0 {
		t.Fatalf("global search = %d, %v", len(all), err)
	}
	if all[0].Title != "订单规则" {
		t.Fatalf("global top = %q", all[0].Title)
	}
}

// TestPostgresKBCascadeDeletes pins that deleting a document/collection also
// removes its chunks, so global search never surfaces orphaned fragments.
func TestPostgresKBCascadeDeletes(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	col, err := st.CreateKBCollection(ctx, "级联库", "")
	if err != nil {
		t.Fatal(err)
	}
	doc := &KBDocument{CollectionID: col.ID, Name: "c.md", Status: "indexed"}
	if err := st.CreateKBDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKBDocumentChunks(ctx, doc, []*KBChunk{
		{Seq: 1, Title: "a", Content: "甲", Embedding: oneHot(EmbedDim, 3)},
		{Seq: 2, Title: "b", Content: "乙", Embedding: oneHot(EmbedDim, 4)},
	}); err != nil {
		t.Fatal(err)
	}

	// 删除文档 → 其 chunk 必须消失。
	if err := st.DeleteKBDocument(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := st.CountKBChunks(ctx, col.ID); err != nil || n != 0 {
		t.Fatalf("chunks after doc delete = %d, %v", n, err)
	}
	// 集合文档计数被刷新。
	got, err := st.GetKBCollection(ctx, col.ID)
	if err != nil || got.DocumentCount != 0 {
		t.Fatalf("collection counters after doc delete = %+v, %v", got, err)
	}

	// 重建后再删集合 → 文档与 chunk 一起消失，全局检索无孤儿。
	doc2 := &KBDocument{CollectionID: col.ID, Name: "d.md", Status: "indexed"}
	if err := st.CreateKBDocument(ctx, doc2); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKBDocumentChunks(ctx, doc2, []*KBChunk{
		{Seq: 1, Title: "x", Content: "丙", Embedding: oneHot(EmbedDim, 5)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteKBCollection(ctx, col.ID); err != nil {
		t.Fatal(err)
	}
	if all, err := st.SearchKBChunks(ctx, nil, oneHot(EmbedDim, 5), 5); err != nil || len(all) != 0 {
		t.Fatalf("global search after collection delete = %d, %v (want 0 orphan)", len(all), err)
	}
}
