package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

// TestKbCollectionsEndpoints covers the /api/kb/collections handler on the
// dual-channel auth: unauthenticated → 401, create → 200, duplicate → 409,
// list → 200.
func TestKbCollectionsEndpoints(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodGet, "/api/kb/collections", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d, want 401", rec.Code)
	}

	rec = s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"团队库","description":"需求文档"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
	}
	var col struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &col); err != nil || col.ID == 0 {
		t.Fatalf("create resp = %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"团队库"}`, wh)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409", rec.Code)
	}

	rec = s.do(t, http.MethodGet, "/api/kb/collections", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list struct {
		Collections []map[string]any `json:"collections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list.Collections) != 1 {
		t.Fatalf("list resp = %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodDelete, "/api/kb/collections/"+jsonInt(col.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestKbIngestUnsupportedOnSQLite pins the 503 contract: ingest needs a
// pgvector backing store, so the lightweight SQLite test deployment must reject
// it explicitly rather than silently accepting a document that can never be
// searched.
func TestKbIngestUnsupportedOnSQLite(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"库"}`, wh)
	var col struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &col)

	rec = s.do(t, http.MethodPost, "/api/kb/ingest",
		`{"collectionId":`+jsonInt(col.ID)+`,"name":"doc.md","content":"# 标题\n正文内容"}`, wh)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ingest on SQLite status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// TestKbSearchRequiresAuth ensures search is not anonymously reachable.
func TestKbSearchRequiresAuth(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/kb/search", `{"query":"测试"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("search unauthenticated status = %d, want 401", rec.Code)
	}
}

// TestKbStatsEndpoint covers the RAG outcome counters surface.
func TestKbStatsEndpoint(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	if rec := s.do(t, http.MethodGet, "/api/kb/stats", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stats = %d, want 401", rec.Code)
	}
	rec := s.do(t, http.MethodGet, "/api/kb/stats", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Rag map[string]int64 `json:"rag"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("stats json: %v", err)
	}
	if _, ok := body.Rag["spliced"]; !ok {
		t.Fatalf("stats missing spliced counter: %s", rec.Body.String())
	}
}

// TestRagCounterNoEmbedding pins that an unconfigured embedding bumps the
// skipNoEmbedding counter visible via /api/kb/stats.
func TestRagCounterNoEmbedding(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	req := httptest.NewRequest(http.MethodPost, "/api/opencode/session/s1/prompt_async",
		strings.NewReader(`{"parts":[{"type":"text","text":"问题"}]}`))
	_, spliced := s.applyRagSpliceToProxy(req)
	if spliced {
		t.Fatal("no embedding configured must not splice")
	}
	rec := s.do(t, http.MethodGet, "/api/kb/stats", "", wh)
	var body struct {
		Rag map[string]int64 `json:"rag"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Rag["skipNoEmbedding"] < 1 {
		t.Fatalf("skipNoEmbedding = %d, want >=1", body.Rag["skipNoEmbedding"])
	}
}

// TestKbIngestMultipartParsed ensures a multipart ingest is parsed (and then
// rejected by the pgvector gate on SQLite, i.e. 503 not 400).
func TestKbIngestMultipartParsed(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("collectionId", "1")
	_ = mw.WriteField("name", "需求.md")
	fw, _ := mw.CreateFormFile("file", "需求.md")
	_, _ = fw.Write([]byte("# 标题\n正文内容"))
	_ = mw.Close()

	rec := s.do(t, http.MethodPost, "/api/kb/ingest", buf.String(),
		mergeWith(map[string]string{"Content-Type": mw.FormDataContentType()}, wh))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("multipart ingest on SQLite = %d, want 503 (parsed then pgvector gate): %s", rec.Code, rec.Body.String())
	}
}

// TestDiversifyHits pins the per-document chunk cap used by retrieval: a long
// document must not crowd out other sources in the injected context.
func TestDiversifyHits(t *testing.T) {
	hits := []kbHit{
		{documentID: 1, score: 0.99},
		{documentID: 1, score: 0.95},
		{documentID: 1, score: 0.90},
		{documentID: 1, score: 0.85}, // 第 4 条同文档，应被裁剪
		{documentID: 2, score: 0.80},
		{documentID: 2, score: 0.70},
	}
	got := diversifyHits(hits, 3)
	// 期望：doc1 保留 3 条（0.99/0.95/0.90）+ doc2 全 2 条 = 5 条。
	if len(got) != 5 {
		t.Fatalf("diversified = %d, want 5", len(got))
	}
	doc1 := 0
	for _, h := range got {
		if h.documentID == 1 {
			doc1++
		}
	}
	if doc1 > 3 {
		t.Fatalf("doc1 contributed %d chunks, cap is 3", doc1)
	}
	// 保序：分数仍降序。
	for i := 1; i < len(got); i++ {
		if got[i-1].score < got[i].score {
			t.Fatalf("order broken at %d: %v < %v", i, got[i-1].score, got[i].score)
		}
	}
	// maxPerDoc<=0 或空列表 → 原样返回。
	if len(diversifyHits(nil, 3)) != 0 || len(diversifyHits(hits, 0)) != len(hits) {
		t.Fatal("degenerate cases should pass through")
	}
}

// TestKbIngestReplaceSameName pins the replace contract: a JSON ingest with
// replace=true deletes the existing same-name document (and its chunks) before
// creating the new one. On SQLite the replace lookup still runs (doc is gone),
// then the pgvector gate rejects with 503 — proving the replace step happened
// before the capability check.
func TestKbIngestReplaceSameName(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"替换库"}`, wh)
	var col struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &col)

	// 直插一条同名文档，模拟已存在版本。
	old := &store.KBDocument{CollectionID: col.ID, Name: "规则.md", MIME: "text/markdown", SizeBytes: 9, Status: "indexed"}
	if err := s.store.CreateKBDocument(context.Background(), old); err != nil {
		t.Fatal(err)
	}

	rec = s.do(t, http.MethodPost, "/api/kb/ingest",
		`{"collectionId":`+jsonInt(col.ID)+`,"name":"规则.md","content":"新版本内容","replace":true}`, wh)
	// SQLite 无 pgvector → 替换已删旧文档，随后 503。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("replace ingest on SQLite status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if _, err := s.store.GetKBDocument(context.Background(), old.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old document should be replaced (deleted), got %v", err)
	}
}

// TestKbDocumentDeleteInvalidatesCache pins that deleting a document drops the
// cached retrieval entries (a later search must not cite a deleted chunk).
func TestKbDocumentDeleteInvalidatesCache(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"缓存库"}`, wh)
	var col struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &col)
	d := &store.KBDocument{CollectionID: col.ID, Name: "a.md", Status: "indexed"}
	if err := s.store.CreateKBDocument(context.Background(), d); err != nil {
		t.Fatal(err)
	}

	key := kbSearchCacheKey("测试查询", []int64{col.ID}, 5, 0.5)
	kbCache.put(key, []kbHit{{source: "a.md", content: "旧", score: 0.9}})
	if _, ok := kbCache.get(key); !ok {
		t.Fatal("precondition: cache entry should exist")
	}

	rec = s.do(t, http.MethodDelete, "/api/kb/documents/"+itoa2(d.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status = %d", rec.Code)
	}
	if _, ok := kbCache.get(key); ok {
		t.Fatal("cache entry should be invalidated after document delete")
	}
}

// TestKbDocumentsPaginationEndpoint pins the total/offset surface of the
// documents listing so the Web UI pagination renders correct ranges.
func TestKbDocumentsPaginationEndpoint(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"分页"}`, wh)
	var col struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &col)
	for i := 0; i < 3; i++ {
		_ = s.store.CreateKBDocument(context.Background(), &store.KBDocument{CollectionID: col.ID, Name: "d.md", Status: "indexed"})
	}

	rec = s.do(t, http.MethodGet, "/api/kb/documents?collectionId="+jsonInt(col.ID)+"&limit=2&offset=2", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var body struct {
		Documents []any `json:"documents"`
		Total     int64 `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body.Total != 3 {
		t.Fatalf("total = %d, want 3", body.Total)
	}
	if len(body.Documents) != 1 {
		t.Fatalf("offset page = %d docs, want 1", len(body.Documents))
	}
}

// TestKbCollectionPatch pins the PATCH endpoint: rename/re-describe, empty body
// → 400, duplicate name → 409, unknown id → 404.
func TestKbCollectionPatch(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"旧名","description":"d"}`, wh)
	var col struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &col)

	rec = s.do(t, http.MethodPatch, "/api/kb/collections/"+itoa2(col.ID), `{"name":"新名","description":"新描述"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Name != "新名" || out.Description != "新描述" {
		t.Fatalf("patched = %+v", out)
	}

	// 空 body → 400
	if rec := s.do(t, http.MethodPatch, "/api/kb/collections/"+itoa2(col.ID), `{}`, wh); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d", rec.Code)
	}
	// 重名 → 409
	if rec := s.do(t, http.MethodPost, "/api/kb/collections", `{"name":"又一名"}`, wh); rec.Code != http.StatusOK {
		t.Fatalf("seed collection status = %d", rec.Code)
	}
	if rec := s.do(t, http.MethodPatch, "/api/kb/collections/"+itoa2(col.ID), `{"name":"又一名"}`, wh); rec.Code != http.StatusConflict {
		t.Fatalf("dup name patch = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	// 不存在 → 404
	if rec := s.do(t, http.MethodPatch, "/api/kb/collections/99999", `{"name":"x"}`, wh); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id patch = %d, want 404", rec.Code)
	}
}
