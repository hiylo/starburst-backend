package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/llm"
	"github.com/hiylo/starburst-backend/internal/store"
)

// fakeLLMServer answers /chat/completions with a fixed skeleton JSON, like a
// LiteLLM gateway configured for skeleton drafting.
func fakeLLMServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": content},
			}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDocGenerateXLSX(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)
	s.SetLLM(llm.New(fakeLLMServer(t,
		`{"type":"xlsx","title":"报价单","sheets":[{"name":"报价","rows":[["型号","价格"],["X","99"]]}]}`).URL,
		"key", "test-model"))

	rec := s.do(t, http.MethodPost, "/api/documents/generate",
		`{"type":"xlsx","prompt":"做一张两行的报价表"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status %d: %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		ID          int64  `json:"id"`
		DocType     string `json:"docType"`
		DownloadURL string `json:"downloadUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gen); err != nil || gen.ID == 0 {
		t.Fatalf("generate resp = %s", rec.Body.String())
	}
	if gen.DocType != "xlsx" {
		t.Fatalf("docType = %s", gen.DocType)
	}

	// 产物文件必须真实落在 docs-dir，且可用我们的解析器读回内容。
	path := filepath.Join(s.cfg.DocsDir, itoa2(gen.ID)+".xlsx")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("product file missing: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("product file empty")
	}

	// download 端点返回同一文件。
	dl := s.do(t, http.MethodGet, gen.DownloadURL, "", wh)
	if dl.Code != http.StatusOK || len(dl.Body.Bytes()) == 0 {
		t.Fatalf("download status %d", dl.Code)
	}
	if !strings.Contains(dl.Header().Get("Content-Type"), "spreadsheetml") {
		t.Fatalf("download content-type = %s", dl.Header().Get("Content-Type"))
	}

	// 详情与删除。
	det := s.do(t, http.MethodGet, "/api/documents/"+itoa2(gen.ID), "", wh)
	if det.Code != http.StatusOK {
		t.Fatalf("detail status %d", det.Code)
	}
	del := s.do(t, http.MethodDelete, "/api/documents/"+itoa2(gen.ID), "", wh)
	if del.Code != http.StatusOK {
		t.Fatalf("delete status %d", del.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("product file should be removed after delete: %v", err)
	}
}

func TestDocGenerateRequiresLLM(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/documents/generate",
		`{"type":"docx","prompt":"写一段话"}`, wh)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("generate without LLM status = %d, want 503", rec.Code)
	}
}

func TestDocGenerateValidatesType(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)
	s.SetLLM(llm.New(fakeLLMServer(t, `{"type":"docx","paragraphs":["x"]}`).URL, "key", "m"))

	rec := s.do(t, http.MethodPost, "/api/documents/generate",
		`{"type":"pdf","prompt":"x"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid type status = %d, want 400", rec.Code)
	}
}

func TestDocDocumentsAuth(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodGet, "/api/documents", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list status = %d, want 401", rec.Code)
	}
}

// seedDocDocument inserts a generated-document row directly (bypassing LLM).
func seedDocDocument(t *testing.T, s *Server, skeleton string) int64 {
	t.Helper()
	d := &store.DocDocument{Name: "原始报价", DocType: "xlsx", Prompt: "做报价表",
		Skeleton: skeleton, Status: "ready"}
	if err := s.store.CreateDocDocument(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d.ID
}

func TestDocRegenerateRevisesSkeleton(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)
	id := seedDocDocument(t, s, `{"type":"xlsx","title":"原始报价","sheets":[{"name":"S","rows":[["型号","价格"],["A","10"]]}]}`)
	// 产物文件先落盘，regenerate 应覆盖它。
	if err := os.WriteFile(filepath.Join(s.cfg.DocsDir, itoa2(id)+".xlsx"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.SetLLM(llm.New(fakeLLMServer(t,
		`{"type":"xlsx","title":"修订报价","sheets":[{"name":"S","rows":[["型号","价格"],["B","20"]]}]}`).URL,
		"key", "m"))

	rec := s.do(t, http.MethodPost, "/api/documents/regenerate",
		`{"docId":`+itoa2(id)+`,"instruction":"改成 B 型号","sessionId":"ses_test123"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate status %d: %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Name  string `json:"name"`
		DocType string `json:"docType"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gen); err != nil || gen.DocType != "xlsx" {
		t.Fatalf("regenerate resp = %s", rec.Body.String())
	}
	if gen.Name != "修订报价" {
		t.Fatalf("name not revised: %s", gen.Name)
	}

	// 磁盘产物必须被新渲染覆盖（不再是 "old"）。
	raw, err := os.ReadFile(filepath.Join(s.cfg.DocsDir, itoa2(id)+".xlsx"))
	if err != nil || string(raw) == "old" {
		t.Fatalf("product file not overwritten: %v", err)
	}
	// 骨架已更新，title 带修订版。
	d, err := s.store.GetDocDocument(context.Background(), id)
	if err != nil || !strings.Contains(d.Skeleton, "修订报价") {
		t.Fatalf("stored skeleton not revised: %+v, %v", d, err)
	}
}

func TestDocRegenerateErrors(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/documents/regenerate", `{"docId":1}`, wh)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("regenerate without LLM status = %d, want 503", rec.Code)
	}
	rec = s.do(t, http.MethodPost, "/api/documents/regenerate", `{"docId":99999}`, wh)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("regenerate missing doc with no LLM status = %d, want 503 (LLM gate first)", rec.Code)
	}

	s.SetLLM(llm.New(fakeLLMServer(t, `{"type":"xlsx","sheets":[]}`).URL, "k", "m"))
	rec = s.do(t, http.MethodPost, "/api/documents/regenerate", `{"docId":99999}`, wh)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("regenerate missing doc status = %d, want 404", rec.Code)
	}
}

func TestSessionContextExcerpt(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	got := s.sessionContextExcerpt(ctx, "ses_test123")
	if !strings.Contains(got, "这是结果") {
		t.Fatalf("excerpt = %q, want session message content", got)
	}
	if got := s.sessionContextExcerpt(ctx, ""); got != "" {
		t.Fatalf("empty session should yield empty excerpt, got %q", got)
	}
}
// TestDocEventPayload covers the `doc.event` push payload shape so broadcast
// wiring stays stable.
func TestDocEventPayload(t *testing.T) {
	d := &store.DocDocument{ID: 7, Name: "报告", DocType: "pptx", SizeBytes: 2048}
	b := docEventPayload("ready", d)
	var p struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		DocType string `json:"docType"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("payload not valid json: %v", err)
	}
	if p.ID != 7 || p.Name != "报告" || p.DocType != "pptx" || p.Status != "ready" {
		t.Fatalf("payload = %+v", p)
	}
}
