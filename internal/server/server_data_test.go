package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProjectsGroupAndFilter(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"proj"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Projects are grouped by real working directory from /experimental/session.
	rec = s.do(t, http.MethodGet, "/api/projects", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects: %d", rec.Code)
	}
	var proj struct {
		Projects []struct {
			ID           string `json:"id"`
			Directory    string `json:"directory"`
			SessionCount int    `json:"sessionCount"`
		} `json:"projects"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &proj)
	if len(proj.Projects) != 2 || proj.Projects[0].ID != "/w" || proj.Projects[0].SessionCount != 1 {
		t.Fatalf("projects = %+v, want two groups /w and /other each with 1", proj.Projects)
	}

	// The drill-down filters sessions by the selected real directory.
	rec = s.do(t, http.MethodGet, "/api/projects/%2Fw", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("project sessions: %d %s", rec.Code, rec.Body.String())
	}
	var sess struct {
		Directory string `json:"directory"`
		Sessions  []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess.Directory != "/w" {
		t.Fatalf("directory = %q, want /w", sess.Directory)
	}
	if len(sess.Sessions) != 1 || sess.Sessions[0].ID != "ses_a" {
		t.Fatalf("sessions = %+v, want ses_a", sess.Sessions)
	}

	// Another directory exact-filters to its own session.
	rec = s.do(t, http.MethodGet, "/api/projects/%2Fother", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("other dir: %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess.Directory != "/other" || len(sess.Sessions) != 1 || sess.Sessions[0].ID != "ses_b" {
		t.Fatalf("other dir: directory=%q sessions=%+v", sess.Directory, sess.Sessions)
	}

	// A missing id is a client error.
	if rec = s.do(t, http.MethodGet, "/api/projects/", "", th); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: got %d want 400", rec.Code)
	}
}

func TestAuditLogging(t *testing.T) {
	s := newTestServer(t)

	// Login + token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"audit-phone"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)

	// Make a token-authenticated call.
	th := map[string]string{"Authorization": "Bearer " + tok.Token}
	rec = s.do(t, http.MethodGet, "/api/projects", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects status %d", rec.Code)
	}

	// Audit should have an entry for this token call (async flush → poll).
	entries := waitForAuditEntries(t, s, wh, 1)
	audit := entries
	if len(audit) == 0 {
		t.Fatalf("expected audit entries")
	}
	// The newest entry should be our projects call.
	last := audit[0]
	if last.Path != "/api/projects" {
		t.Fatalf("last audit path %q want /api/projects", last.Path)
	}
	if last.TokenName != "audit-phone" {
		t.Fatalf("audit token name %q", last.TokenName)
	}

	// Audit requires web session.
	rec = s.do(t, http.MethodGet, "/api/audit", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rec.Code)
	}
}

func TestArchiveSessionFlow(t *testing.T) {
	s := newTestServer(t)

	// Login + token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"archiver"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Archive a session (fake upstream returns a message list).
	rec = s.do(t, http.MethodPost, "/api/archives", `{"sessionId":"ses_test123","format":"markdown"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("archive status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("no archive id")
	}

	// List.
	rec = s.do(t, http.MethodGet, "/api/archives", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	var list struct {
		Archives []map[string]any `json:"archives"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Archives) != 1 {
		t.Fatalf("expected 1 archive, got %d", len(list.Archives))
	}

	// Get full content.
	rec = s.do(t, http.MethodGet, "/api/archives/"+created.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status %d", rec.Code)
	}
	var arch struct {
		Content string `json:"Content"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &arch)
	if !strings.Contains(arch.Content, "这是结果") {
		t.Fatalf("archive content missing assistant text: %q", arch.Content)
	}

	// Delete.
	rec = s.do(t, http.MethodDelete, "/api/archives/"+created.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d", rec.Code)
	}
	rec = s.do(t, http.MethodGet, "/api/archives/"+created.ID, "", th)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", rec.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	s := newTestServer(t)

	// Login + token, then make a couple of token calls to seed audit.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"stats-phone"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.do(t, http.MethodGet, "/api/projects", "", th)
	rec = s.do(t, http.MethodGet, "/api/projects", "", th)

	// Create a task too.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","directory":"/w"}`, th)

	// 审计是异步批量落库的：先等该 token 的 3 条调用（2×projects + 1×tasks）
	// 进入审计，否则 tokenUsage 断言会拿到空。
	waitForAuditEntries(t, s, wh, 3)

	// Stats.
	rec = s.do(t, http.MethodGet, "/api/stats", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tasks struct {
			Total int `json:"Total"`
		} `json:"tasks"`
		TokenUsage []map[string]any `json:"tokenUsage"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Tasks.Total != 1 {
		t.Fatalf("expected 1 task, got %d", out.Tasks.Total)
	}
	if len(out.TokenUsage) == 0 {
		t.Fatalf("expected token usage entries")
	}
	if out.TokenUsage[0]["calls"].(float64) < 2 {
		t.Fatalf("expected >=2 calls, got %v", out.TokenUsage[0]["calls"])
	}

	// Stats requires web session.
	rec = s.do(t, http.MethodGet, "/api/stats", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rec.Code)
	}
}

// auditEntryView is the JSON view of an audit_log row used in tests.
type auditEntryView struct {
	TokenName string `json:"TokenName"`
	Path      string `json:"Path"`
}

// waitForAuditEntries polls /api/audit (web-session headers) until at least
// min entries are visible or a deadline passes. Audit is flushed asynchronously
// (StartAuditFlusher batches every ~500ms), so assertions must not assume the
// row is visible immediately.
func waitForAuditEntries(t *testing.T, s *Server, wh map[string]string, min int) []auditEntryView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var out struct {
		Audit []auditEntryView `json:"audit"`
	}
	for {
		rec := s.do(t, http.MethodGet, "/api/audit", "", wh)
		if rec.Code == http.StatusOK {
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if len(out.Audit) >= min {
				return out.Audit
			}
		}
		if time.Now().After(deadline) {
			return out.Audit
		}
		time.Sleep(100 * time.Millisecond)
	}
}
