package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
