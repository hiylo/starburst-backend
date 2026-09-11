package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hiylo/opencode-backend/internal/auth"
	"github.com/hiylo/opencode-backend/internal/automation"
	"github.com/hiylo/opencode-backend/internal/config"
	"github.com/hiylo/opencode-backend/internal/opencode"
	"github.com/hiylo/opencode-backend/internal/push"
	"github.com/hiylo/opencode-backend/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	am := auth.NewManager(st)
	if _, err := am.Initialize(ctx, "admin"); err != nil {
		t.Fatalf("init auth: %v", err)
	}

	// Fake upstream OpenCode that reports healthy + one busy session.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"v9.9.9"}`))
		case "/session":
			_, _ = w.Write([]byte(`[{"id":"ses_a","slug":"alpha","title":"Alpha","directory":"/w","agent":"build","model":{"id":"m1"},"cost":0,"tokens":{"input":1,"output":1,"reasoning":1},"time":{"created":1000,"updated":2000}}]`))
		case "/session/status":
			_, _ = w.Write([]byte(`{"ses_a":{"type":"busy"}}`))
		case "/config":
			_, _ = w.Write([]byte(`{"version":"v9.9.9"}`))
		case "/session/ses_test123/message":
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"这是结果"}]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		ListenAddr:  "127.0.0.1:0",
		OpenCodeURL: upstream.URL,
		DBDriver:    "sqlite",
	}
	oc := opencode.New(upstream.URL)
	hub := push.NewHub()
	go hub.Run()

	srv := New(cfg, st, am, oc, hub)
	srv.SetAutomation(automation.NewEngine(st, time.Hour))
	srv.testMux = srv.routesMux()
	return srv
}

// do performs a request against the test mux.
func (s *Server) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.testMux.ServeHTTP(rec, req)
	return rec
}

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

func TestWebLoginFlow(t *testing.T) {
	s := newTestServer(t)

	// Wrong password -> 401.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"nope"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status %d", rec.Code)
	}

	// Correct password -> session id.
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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

	// Change password.
	rec = s.do(t, http.MethodPost, "/api/web/password", `{"newPassword":"newpass"}`, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("change password status %d: %s", rec.Code, rec.Body.String())
	}

	// Old password now fails, new works.
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works")
	}
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"newpass"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d", rec.Code)
	}
}

func TestTokenAuthFlow(t *testing.T) {
	s := newTestServer(t)

	// Login as web admin and create a token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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
	if len(proj.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(proj.Projects))
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
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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
	if sys.Backend != "opencode-backend" {
		t.Fatalf("bad backend %q", sys.Backend)
	}
	if sys.OpenCodeVersion != "v9.9.9" {
		t.Fatalf("bad version %q", sys.OpenCodeVersion)
	}
}
func TestTasksCRUD(t *testing.T) {
	s := newTestServer(t)

	// Login and create a token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"tasker"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Create task.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"do stuff","directory":"/w"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create task status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("no task id")
	}
	if created.Status != "" && created.Status != "queued" {
		t.Fatalf("unexpected initial status %q", created.Status)
	}

	// List includes it.
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list.Tasks))
	}

	// Get single.
	rec = s.do(t, http.MethodGet, "/api/tasks/"+created.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status %d", rec.Code)
	}
}

func TestTaskRequiresToken(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestTaskDependsOn(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"dep"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Upstream task starts queued.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"upstream"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create upstream: %d %s", rec.Code, rec.Body.String())
	}
	var up struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &up)

	// A dependent on a queued task must be pending, not queued.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"downstream","dependsOn":"`+up.ID+`"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create dependent: %d %s", rec.Code, rec.Body.String())
	}
	var dep struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		DependsOn string `json:"dependsOn"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dep)
	if dep.Status != "pending" {
		t.Fatalf("dependent status = %q, want pending", dep.Status)
	}
	if dep.DependsOn != up.ID {
		t.Fatalf("dependsOn = %q, want %q", dep.DependsOn, up.ID)
	}

	// A typo in dependsOn fails fast instead of parking a pending task forever.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","dependsOn":"task_missing"}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown dependsOn: got %d want 400", rec.Code)
	}

	// Canceling the upstream blocks its pending dependents.
	rec = s.do(t, http.MethodDelete, "/api/tasks/"+up.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel upstream: %d %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		OK      bool `json:"ok"`
		Blocked int  `json:"blocked"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &blocked)
	if !blocked.OK || blocked.Blocked != 1 {
		t.Fatalf("cancel should block 1 dependent, got %+v", blocked)
	}
	rec = s.do(t, http.MethodGet, "/api/tasks/"+dep.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("get dependent: %d", rec.Code)
	}
	var depAfter struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &depAfter)
	if depAfter.Status != "blocked" || depAfter.Error == "" {
		t.Fatalf("dependent = %+v, want blocked with a reason", depAfter)
	}

	// A dependency that already finished is rejected.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","dependsOn":"`+up.ID+`"}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("finished dependsOn: got %d want 400", rec.Code)
	}

	// A dependency that already succeeded runs immediately.
	if err := s.store.CreateTask(ctx, &store.Task{ID: "task_done", Prompt: "done"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.store.CompleteTask(ctx, "task_done", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"after done","dependsOn":"task_done"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after done: %d %s", rec.Code, rec.Body.String())
	}
	var after struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Status != "queued" {
		t.Fatalf("status after succeeded upstream = %q, want queued", after.Status)
	}
}

// TestTaskUnblockAndDependentPush verifies that blocking a dependent pushes a
// warning to every connected device, and that a human can unblock it manually.
func TestTaskUnblockAndDependentPush(t *testing.T) {
	s := newTestServer(t)
	backend := httptest.NewServer(s.testMux)
	t.Cleanup(backend.Close)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"push"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	wsURL := "ws" + strings.TrimPrefix(backend.URL, "http") + "/api/ws?token=" + tok.Token
	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// First frame is the subscription confirmation.
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read subscribed: %v", err)
	}
	var sub push.Message
	_ = json.Unmarshal(data, &sub)
	if sub.Type != "subscribed" {
		t.Fatalf("expected subscribed, got %s", sub.Type)
	}

	readEvent := func() (push.Message, map[string]any) {
		t.Helper()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read push event: %v", err)
		}
		var msg push.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		payload := map[string]any{}
		if len(msg.Payload) > 0 {
			_ = json.Unmarshal(msg.Payload, &payload)
		}
		return msg, payload
	}

	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"upstream"}`, th)
	var up struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &up)
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"downstream","dependsOn":"`+up.ID+`"}`, th)
	var dep struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dep)

	// Canceling the upstream blocks the dependent and pushes a warning.
	rec = s.do(t, http.MethodDelete, "/api/tasks/"+up.ID, "", th)
	var blockedResp struct {
		OK      bool `json:"ok"`
		Blocked int  `json:"blocked"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &blockedResp)
	if !blockedResp.OK || blockedResp.Blocked != 1 {
		t.Fatalf("cancel upstream: %+v", blockedResp)
	}
	msg, payload := readEvent()
	if msg.Type != "task.event" || msg.Severity != push.Warning {
		t.Fatalf("blocked event = %+v, want task.event/warning", msg)
	}
	if payload["id"] != dep.ID || payload["status"] != "blocked" || payload["upstream"] != up.ID {
		t.Fatalf("blocked payload = %+v", payload)
	}
	if payload["reason"] == "" {
		t.Fatal("blocked event must carry a reason")
	}

	// A human fixes the upstream out of band and unblocks the dependent.
	rec = s.do(t, http.MethodPost, "/api/tasks/"+dep.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d %s", rec.Code, rec.Body.String())
	}
	msg, payload = readEvent()
	if payload["id"] != dep.ID || payload["status"] != "queued" {
		t.Fatalf("unblock event = %+v", payload)
	}
	rec = s.do(t, http.MethodGet, "/api/tasks/"+dep.ID, "", th)
	var depAfter struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &depAfter)
	if depAfter.Status != "queued" || depAfter.Error != "" {
		t.Fatalf("dependent after unblock = %+v", depAfter)
	}

	// Unblocking a task that is not blocked is a conflict.
	rec = s.do(t, http.MethodPost, "/api/tasks/"+dep.ID, "", th)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unblock again: got %d want 409", rec.Code)
	}
	// Unknown tasks are not blocked either.
	rec = s.do(t, http.MethodPost, "/api/tasks/task_missing", "", th)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unblock missing: got %d want 409", rec.Code)
	}
}

func TestProjectsGroupAndFilter(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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

	// Projects are grouped by directory, not raw sessions.
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
	if len(proj.Projects) != 1 || proj.Projects[0].ID != "/w" || proj.Projects[0].SessionCount != 1 {
		t.Fatalf("projects = %+v, want one group for /w", proj.Projects)
	}

	// The drill-down returns only sessions of that directory.
	rec = s.do(t, http.MethodGet, "/api/projects/%2Fw", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("project sessions: %d %s", rec.Code, rec.Body.String())
	}
	var sess struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if len(sess.Sessions) != 1 || sess.Sessions[0].ID != "ses_a" {
		t.Fatalf("sessions = %+v, want ses_a only", sess.Sessions)
	}

	// An unknown directory yields an empty list, not an error.
	rec = s.do(t, http.MethodGet, "/api/projects/%2Fother", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("unknown dir: %d", rec.Code)
	}
	var empty struct {
		Sessions []struct{} `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &empty)
	if len(empty.Sessions) != 0 {
		t.Fatalf("unknown dir returned %d sessions", len(empty.Sessions))
	}

	// A missing id is a client error.
	if rec = s.do(t, http.MethodGet, "/api/projects/", "", th); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id: got %d want 400", rec.Code)
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
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("attempt 6: got %d want 429", rec.Code)
	}

	// A correct password resets the bucket.
	s.loginLimit.clear(clientKey(httptest.NewRequest(http.MethodPost, "/api/web/session", nil)))
	rec = s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login after clear: got %d want 200", rec.Code)
	}
}

func TestSystemRequiresAuth(t *testing.T) {
	s := newTestServer(t)

	if rec := s.do(t, http.MethodGet, "/api/system", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("system without auth: got %d want 401", rec.Code)
	}

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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

func TestRulesCRUDAndWebhook(t *testing.T) {
	s := newTestServer(t)

	// Web session (rules are admin-managed).
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}

	// Create a cron rule.
	rec = s.do(t, http.MethodPost, "/api/rules", `{"name":"nightly","kind":"cron","schedule":"5m","directory":"/w","prompt":"run tests","enabled":true}`, wh)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rule status %d: %s", rec.Code, rec.Body.String())
	}
	var rule struct {
		ID string `json:"ID"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)
	if rule.ID == "" {
		t.Fatalf("no rule id")
	}

	// List includes it.
	rec = s.do(t, http.MethodGet, "/api/rules", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules status %d", rec.Code)
	}
	var list struct {
		Rules []map[string]any `json:"rules"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(list.Rules))
	}

	// Rules require web session (not token).
	rec = s.do(t, http.MethodGet, "/api/rules", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rec.Code)
	}

	// Delete rule.
	rec = s.do(t, http.MethodDelete, "/api/rules/"+rule.ID, "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete rule status %d", rec.Code)
	}
	rec = s.do(t, http.MethodDelete, "/api/rules/"+rule.ID, "", wh)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on double delete, got %d", rec.Code)
	}
}

func TestWebhookFiresRule(t *testing.T) {
	s := newTestServer(t)

	// Login and create an HTTP rule via web session.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}

	rec = s.do(t, http.MethodPost, "/api/rules", `{"name":"hook","kind":"http","schedule":"/workspaces/opencode","prompt":"run on hook","enabled":true}`, wh)
	var rule struct {
		ID string `json:"ID"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)

	// Fire webhook for matching target.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/workspaces/opencode", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status %d: %s", rec.Code, rec.Body.String())
	}
	var fired struct {
		Fired bool `json:"fired"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fired)
	if !fired.Fired {
		t.Fatalf("webhook did not fire")
	}

	// A task must have been created (visible with an APP token).
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"hooker"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 task from webhook, got %d", len(list.Tasks))
	}

	// Non-matching target -> 404.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-match, got %d", rec.Code)
	}
}

func TestBatchCreatesMultipleTasks(t *testing.T) {
	s := newTestServer(t)

	// Login + token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"batch"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Batch create for two targets.
	rec = s.do(t, http.MethodPost, "/api/batch",
		`{"prompt":"add logging","targets":[{"directory":"/a"},{"sessionId":"ses_1"}]}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Count != 2 {
		t.Fatalf("expected 2 tasks, got %d", out.Count)
	}

	// Verify two tasks in store.
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(list.Tasks))
	}

	// Empty targets rejected.
	rec = s.do(t, http.MethodPost, "/api/batch", `{"prompt":"x","targets":[]}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty targets, got %d", rec.Code)
	}

	// No token rejected.
	rec = s.do(t, http.MethodPost, "/api/batch", `{"prompt":"x","targets":[{"directory":"/a"}]}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestAuditLogging(t *testing.T) {
	s := newTestServer(t)

	// Login + token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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

	// Audit should have an entry for this token call.
	rec = s.do(t, http.MethodGet, "/api/audit", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status %d", rec.Code)
	}
	var out struct {
		Audit []struct {
			TokenName string `json:"TokenName"`
			Path      string `json:"Path"`
		} `json:"audit"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Audit) == 0 {
		t.Fatalf("expected audit entries")
	}
	// The newest entry should be our projects call.
	last := out.Audit[0]
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
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
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

func TestStreamRelayRequiresToken(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodGet, "/api/stream", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestStreamRelaysEvents(t *testing.T) {
	// Build a server whose upstream /global/event emits two events then ends.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/global/event" {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("data: {\"type\":\"a\"}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("data: {\"type\":\"b\"}\n\n"))
			fl.Flush()
			return
		}
		// health/config for bootstrapping
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session/status":
			_, _ = w.Write([]byte(`{}`))
		case "/config":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	am := auth.NewManager(st)
	if _, err := am.Initialize(context.Background(), "admin"); err != nil {
		t.Fatalf("init auth: %v", err)
	}
	cfg := &config.Config{ListenAddr: "127.0.0.1:0", OpenCodeURL: upstream.URL, DBDriver: "sqlite"}
	hub := push.NewHub()
	go hub.Run()
	srv := New(cfg, st, am, opencode.New(upstream.URL), hub)

	// Host the backend on a real server so the stream can flush.
	srv.testMux = srv.routesMux()
	backend := httptest.NewServer(srv.testMux)
	t.Cleanup(backend.Close)

	// Login, create token.
	rec := srv.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = srv.do(t, http.MethodPost, "/api/tokens", `{"name":"streamer"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)

	// Request the stream over HTTP and read the relayed events.
	req, _ := http.NewRequest(http.MethodGet, backend.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status %d", resp.StatusCode)
	}

	// Read the first chunk which should contain connected preamble + events.
	deadline := time.Now().Add(5 * time.Second)
	body := ""
	for time.Now().Before(deadline) {
		buf := make([]byte, 512)
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			body += string(buf[:n])
		}
		if strings.Contains(body, `"type":"b"`) {
			break
		}
		if rerr != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `"type":"a"`) || !strings.Contains(body, `"type":"b"`) {
		t.Fatalf("relayed body missing events: %q", body)
	}
	if !strings.Contains(body, "event: connected") {
		t.Fatalf("missing connected preamble: %q", body)
	}
}

func TestWebhookSecretProtection(t *testing.T) {
	s := newTestServer(t)
	s.cfg.WebhookSecret = "topsecret"

	// Login + create http rule.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	s.do(t, http.MethodPost, "/api/rules", `{"name":"hook","kind":"http","schedule":"/x","prompt":"p","enabled":true}`, wh)

	// Without secret -> 401.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret, got %d", rec.Code)
	}

	// Wrong secret -> 401.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", map[string]string{"X-Webhook-Secret": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong secret, got %d", rec.Code)
	}

	// Correct secret -> fired.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", map[string]string{"X-Webhook-Secret": "topsecret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct secret, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestTaskListPagination covers the paged task listing: limit/offset params,
// the total count, clamping of out-of-range values and the status filter.
func TestTaskListPagination(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"page"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	for i := 0; i < 5; i++ {
		rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"page task"}`, th)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create task %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	type page struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	get := func(query string) page {
		t.Helper()
		rec = s.do(t, http.MethodGet, "/api/tasks?"+query, "", th)
		if rec.Code != http.StatusOK {
			t.Fatalf("list ?%s: %d %s", query, rec.Code, rec.Body.String())
		}
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		return p
	}

	p1 := get("limit=2&offset=0")
	if len(p1.Tasks) != 2 || p1.Total != 5 || p1.Limit != 2 || p1.Offset != 0 {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := get("limit=2&offset=2")
	if len(p2.Tasks) != 2 || p2.Offset != 2 || p2.Total != 5 {
		t.Fatalf("page 2: %+v", p2)
	}
	for _, a := range p1.Tasks {
		for _, b := range p2.Tasks {
			if a.ID == b.ID {
				t.Fatalf("pages overlap on %s", a.ID)
			}
		}
	}
	p3 := get("limit=2&offset=4")
	if len(p3.Tasks) != 1 || p3.Total != 5 {
		t.Fatalf("page 3: %+v", p3)
	}

	// Defaults: limit 50, offset 0, whole list in one page here.
	d := get("")
	if len(d.Tasks) != 5 || d.Total != 5 || d.Limit != 50 || d.Offset != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	if p := get("limit=9999"); p.Limit != 50 {
		t.Fatalf("limit above 500 must fall back to 50, got %+v", p)
	}
	if p := get("limit=0"); p.Limit != 50 {
		t.Fatalf("non-positive limit must fall back to 50, got %+v", p)
	}
	if p := get("limit=1&offset=-5"); p.Offset != 0 || len(p.Tasks) != 1 {
		t.Fatalf("negative offset must fall back to 0, got %+v", p)
	}
	if p := get("status=queued&limit=1"); p.Total != 5 || len(p.Tasks) != 1 {
		t.Fatalf("filtered page: %+v", p)
	}
	if p := get("status=blocked&limit=1"); p.Total != 0 || len(p.Tasks) != 0 {
		t.Fatalf("empty filtered page: %+v", p)
	}
}
