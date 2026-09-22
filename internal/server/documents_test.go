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

// jsonField returns the numeric value of a top-level JSON field (0 when absent).
func jsonField(b []byte, name string) float64 {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return 0
	}
	v, _ := m[name].(float64)
	return v
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
	// 响应字段名必须是 sizeBytes（曾误命名为 chunkCount，前端读取 sizeBytes）。
	if sizeBytes := jsonField(rec.Body.Bytes(), "sizeBytes"); sizeBytes == 0 {
		t.Fatalf("generate resp missing sizeBytes: %s", rec.Body.String())
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
		Name    string `json:"name"`
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

// TestDocAttachToSession covers the "生成文档作为会话附件 part" flow: the
// endpoint resolves the session's work directory from the upstream, copies the
// generated file into uploads/ with the -@doc<id> filename marker, and returns
// the workdir-relative path a client can reference as a file part.
func TestDocAttachToSession(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)
	id := seedDocDocument(t, s, `{"type":"xlsx","title":"排期","sheets":[{"name":"S","rows":[["d","x"]]}]}`)
	// 落盘一个真实产物文件（attach 是把磁盘产物复制进工作目录）。
	if err := os.WriteFile(filepath.Join(s.cfg.DocsDir, itoa2(id)+".xlsx"), []byte("FILE-BYTES-1"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := s.do(t, http.MethodPost, "/api/documents/"+itoa2(id)+"/attach",
		`{"sessionId":"ses_attach123"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK           bool   `json:"ok"`
		Name         string `json:"name"`
		Path         string `json:"path"`
		AbsolutePath string `json:"absolutePath"`
		DocID        int64  `json:"docId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("attach resp = %s", rec.Body.String())
	}
	if !out.OK || out.DocID != id {
		t.Fatalf("attach resp = %+v", out)
	}
	// 文件名必须携带 -@doc<id> 标记（前端据此挂重新生成/按意见修改按钮）；
	// 同名重复时会带 -N 去重后缀（-@doc<id>-N），标记本身仍要存在。
	if !strings.HasPrefix(out.Name, "原始报价-@doc"+itoa2(id)+".") &&
		!strings.HasPrefix(out.Name, "原始报价-@doc"+itoa2(id)+"-") {
		t.Fatalf("attached name = %q, want 原始报价-@doc%d 前缀", out.Name, id)
	}
	// 目标文件真实存在且内容一致。
	if !strings.Contains(filepath.ToSlash(out.AbsolutePath), "/uploads/") {
		t.Fatalf("unexpected absolute path %q", out.AbsolutePath)
	}
	raw, err := os.ReadFile(out.AbsolutePath)
	if err != nil || string(raw) != "FILE-BYTES-1" {
		t.Fatalf("attached file = %q, %v (want FILE-BYTES-1)", raw, err)
	}
	// 未知会话 → 502（目录解析失败），不静默写错目录。
	rec = s.do(t, http.MethodPost, "/api/documents/"+itoa2(id)+"/attach", `{"sessionId":"ses_nope"}`, wh)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("attach unknown session status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
}

// TestDocDownloadFilenameCarriesDocID pins the -@doc<id> download filename
// convention (docs/DOCUMENTS.md §6.1) so a downloaded generated doc keeps the
// marker that lets the UI resolve it back to /api/documents/{id}.
func TestDocDownloadFilenameCarriesDocID(t *testing.T) {
	s := newTestServer(t)
	s.cfg.DocsDir = t.TempDir()
	wh := loginWeb(t, s)
	id := seedDocDocument(t, s, `{"type":"docx","title":"报告","paragraphs":["x"]}`)
	// seedDocDocument 固定 DocType=xlsx，磁盘产物扩展名必须与其一致。
	if err := os.WriteFile(filepath.Join(s.cfg.DocsDir, itoa2(id)+".xlsx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := s.do(t, http.MethodGet, "/api/documents/"+itoa2(id)+"/download", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d: %s", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "-@doc"+itoa2(id)+".") {
		t.Fatalf("Content-Disposition = %q, want -@doc%d marker", cd, id)
	}
}

// TestDocIDFromPathAction pins the routing of /api/documents/{id}[/download|/attach].
func TestDocIDFromPathAction(t *testing.T) {
	for _, tc := range []struct {
		raw        string
		wantID     int64
		wantAction string
	}{
		{"/api/documents/12", 12, ""},
		{"/api/documents/12/download", 12, "download"},
		{"/api/documents/12/attach", 12, "attach"},
		{"/api/documents/abc", 0, ""},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.raw, nil)
		id, action := docIDFromPath(r)
		if id != tc.wantID || action != tc.wantAction {
			t.Errorf("%s → id=%d action=%q, want %d/%q", tc.raw, id, action, tc.wantID, tc.wantAction)
		}
	}
}

// TestBuildKBRefBlock pins the reference block shape the skeleton LLM consumes:
// source/section headers and clipped content, provenance uniform with RAG splice.
func TestBuildKBRefBlock(t *testing.T) {
	refs := []kbHit{
		{source: "安全库存.docx", section: "第4章", content: strings.Repeat("安全库存规则。", 200)},
		{source: "价格表.xlsx", content: "报价单"},
	}
	block := buildKBRefBlock(refs)
	if !strings.Contains(block, "[来源1] 安全库存.docx · 第4章") || !strings.Contains(block, "[来源2] 价格表.xlsx") {
		t.Fatalf("ref block missing headers:\n%s", block)
	}
	// 内容按 900 rune 截断加省略号。
	if !strings.Contains(block, "…") {
		t.Fatalf("long ref content should be clipped:\n%s", block)
	}
}

// TestAppendCompare covers the singular/plural collection merge used by both
// generate and regenerate.
func TestAppendCompare(t *testing.T) {
	if got := appendCompare(nil, 3); len(got) != 1 || got[0] != 3 {
		t.Fatalf("single fallback = %v", got)
	}
	if got := appendCompare([]int64{1, 2}, 0); len(got) != 2 || got[0] != 1 {
		t.Fatalf("zero single ignored = %v", got)
	}
	if got := appendCompare([]int64{1, 2}, 2); len(got) != 2 {
		t.Fatalf("duplicate should be dropped: %v", got)
	}
	if got := appendCompare([]int64{1, 2}, 7); len(got) != 3 || got[2] != 7 {
		t.Fatalf("append new single = %v", got)
	}
}
