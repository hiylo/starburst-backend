package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthEndpoint(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodGet, "/api/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("health status %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["status"] != "ok" || out["upstream"] != true {
		t.Fatalf("unexpected health body: %s", rec.Body.String())
	}
}

// TestSystemReportsVectorCapability verifies /api/system exposes the
// vectorCapable capability the UI uses to show/hide 智能测试: on SQLite the
// flag must be false (no pgvector), so 轻量化部署 hides the entry instead of
// offering a half-broken feature.
func TestSystemReportsVectorCapability(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodGet, "/api/system", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("system status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		DB            string `json:"db"`
		PGVector      bool   `json:"pgvector"`
		VectorCapable bool   `json:"vectorCapable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if out.DB != "sqlite" {
		t.Fatalf("test server should use sqlite, got %q", out.DB)
	}
	if out.PGVector {
		t.Error("sqlite must not report pgvector installed")
	}
	if out.VectorCapable {
		t.Error("sqlite deployment must not be vectorCapable (no pgvector)")
	}
}

func TestWebLoginFlow(t *testing.T) {
	s := newTestServer(t)

	// Wrong password -> 401.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"nope"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status %d", rec.Code)
	}

	// Correct password -> session id.
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status %d: %s", rec.Code, rec.Body.String())
	}
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if login.Session == "" {
		t.Fatalf("no session id")
	}
	h := map[string]string{"X-Web-Session": login.Session}

	// A second browser is signed in with the same password.
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("second login status %d: %s", rec.Code, rec.Body.String())
	}
	var other struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &other)
	otherH := map[string]string{"X-Web-Session": other.Session}

	// Changing the password without the current one is rejected.
	rec = s.do(t, http.MethodPost, "/api/web/password", `{"newPassword":"newStr0ngPass"}`, h)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing current password status %d: %s", rec.Code, rec.Body.String())
	}

	// Change password.
	rec = s.do(t, http.MethodPost, "/api/web/password",
		`{"currentPassword":"S3cureAdmin!","newPassword":"newStr0ngPass"}`, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password status %d: %s", rec.Code, rec.Body.String())
	}

	// The caller's own session survives; the other one is revoked.
	if rec = s.do(t, http.MethodGet, "/api/system", "", h); rec.Code != http.StatusOK {
		t.Fatalf("own session should survive a password change, got %d", rec.Code)
	}
	if rec = s.do(t, http.MethodGet, "/api/system", "", otherH); rec.Code != http.StatusUnauthorized {
		t.Fatalf("other session should be revoked, got %d", rec.Code)
	}

	// Old password now fails, new works.
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works")
	}
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"newStr0ngPass"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d", rec.Code)
	}
}

func TestTokenAuthFlow(t *testing.T) {
	s := newTestServer(t)

	// Login as web admin and create a token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}

	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"phone"}`, wh)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token status %d: %s", rec.Code, rec.Body.String())
	}
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if !strings.HasPrefix(tok.Token, "ocb_") {
		t.Fatalf("bad token prefix: %q", tok.Token)
	}

	// Use token to read projects.
	th := map[string]string{"Authorization": "Bearer " + tok.Token}
	rec = s.do(t, http.MethodGet, "/api/projects", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("projects status %d: %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		Projects []map[string]any `json:"projects"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &proj)
	if len(proj.Projects) == 0 {
		t.Fatalf("expected projects, got none")
	}

	// Projects without token -> 401.
	rec = s.do(t, http.MethodGet, "/api/projects", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}

	// Revoke token then it must be rejected.
	toks, err := s.auth.ListTokens(context.Background())
	if err != nil || len(toks) != 1 {
		t.Fatalf("list tokens: %v n=%d", err, len(toks))
	}
	if err := s.auth.RevokeToken(context.Background(), toks[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rec = s.do(t, http.MethodGet, "/api/projects", "", th)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token still accepted: %d", rec.Code)
	}
}

func TestSystemEndpointWithToken(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"sys"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	rec = s.do(t, http.MethodGet, "/api/system", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("system status %d", rec.Code)
	}
	var sys struct {
		Backend         string `json:"backend"`
		OpenCodeVersion string `json:"opencodeVersion"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sys)
	if sys.Backend != "starburst-backend" {
		t.Fatalf("bad backend %q", sys.Backend)
	}
	if sys.OpenCodeVersion != "v9.9.9" {
		t.Fatalf("bad version %q", sys.OpenCodeVersion)
	}
}
func TestLoginRateLimit(t *testing.T) {
	s := newTestServer(t)

	// Five wrong passwords are allowed (each returns 401)...
	for i := 0; i < 5; i++ {
		rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"nope"}`, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d want 401", i+1, rec.Code)
		}
	}
	// ...the sixth is throttled.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 6: got %d want 429", rec.Code)
	}

	// A correct password resets the bucket.
	s.loginLimit.clear(clientKey(httptest.NewRequest(http.MethodPost, "/api/web/session", nil)))
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after clear: got %d want 200", rec.Code)
	}
}

func TestSystemRequiresAuth(t *testing.T) {
	s := newTestServer(t)

	if rec := s.do(t, http.MethodGet, "/api/system", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("system without auth: got %d want 401", rec.Code)
	}

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if rec = s.do(t, http.MethodGet, "/api/system", "", map[string]string{"X-Web-Session": login.Session}); rec.Code != http.StatusOK {
		t.Fatalf("system with web session: got %d want 200", rec.Code)
	}

	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"sys"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	if rec = s.do(t, http.MethodGet, "/api/system", "", map[string]string{"Authorization": "Bearer " + tok.Token}); rec.Code != http.StatusOK {
		t.Fatalf("system with token: got %d want 200", rec.Code)
	}
}

// /api/system 不再回传内网上游地址（任何有效 token 都能读），仅服务端日志留痕。
func TestSystemEndpointDoesNotLeakOpenCodeURL(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	rec := s.do(t, http.MethodGet, "/api/system", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("system status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "opencodeURL") {
		t.Fatalf("opencodeURL leaked in /api/system: %s", rec.Body.String())
	}
}

// TestQueryTokenOnlyOnUpgradeEndpoints pins the credential narrowing: ?token=
// authenticates the WS/SSE handshakes, where a browser cannot set headers, and
// is ignored on every other route (query strings land in access logs and
// browser history).
func TestQueryTokenOnlyOnUpgradeEndpoints(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"qtok"}`,
		map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)

	if rec = s.do(t, http.MethodGet, "/api/system?token="+tok.Token, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("system with query token: got %d want 401", rec.Code)
	}

	if _, ok := s.tokenFromRequest(httptest.NewRequest(http.MethodGet, "/api/ws?token="+tok.Token, nil)); !ok {
		t.Error("WS handshake must still accept ?token=")
	}
	if _, ok := s.tokenFromRequest(httptest.NewRequest(http.MethodGet, "/api/stream?token="+tok.Token, nil)); !ok {
		t.Error("SSE handshake must still accept ?token=")
	}
}
