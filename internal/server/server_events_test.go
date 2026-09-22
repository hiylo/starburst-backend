package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hiylo/starburst-backend/internal/auth"
	"github.com/hiylo/starburst-backend/internal/config"
	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// TestTaskUnblockAndDependentPush verifies that blocking a dependent pushes a
// warning to every connected device, and that a human can unblock it manually.
func TestTaskUnblockAndDependentPush(t *testing.T) {
	s := newTestServer(t)
	backend := httptest.NewServer(s.testMux)
	t.Cleanup(backend.Close)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
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
	if _, err := am.Initialize(context.Background(), "S3cureAdmin!", true); err != nil {
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
	rec := srv.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
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

// TestSessionStatusAggClearsStaleBusy verifies the aggregated /session/status
// clears event-aggregation busy/retry residues for sessions the upstream
// snapshot no longer reports, while keeping genuinely-active sessions busy.
func TestSessionStatusAggClearsStaleBusy(t *testing.T) {
	ctx := context.Background()
	func() {
		// Upstream only reports one truly-busy session; the other session was
		// busy previously but is now gone from the snapshot.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/global/health":
				_, _ = w.Write([]byte(`{"healthy":true}`))
			case "/session/status":
				_, _ = w.Write([]byte(`{"ses_snap":{"type":"busy"}}`))
			case "/config":
				_, _ = w.Write([]byte(`{}`))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(upstream.Close)

		dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
		st, err := store.OpenFromConfig(ctx, "sqlite", dsn)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		am := auth.NewManager(st)
		if _, err := am.Initialize(ctx, "S3cureAdmin!", true); err != nil {
			t.Fatalf("init auth: %v", err)
		}
		cfg := &config.Config{ListenAddr: "127.0.0.1:0", OpenCodeURL: upstream.URL, DBDriver: "sqlite"}
		hub := push.NewHub()
		go hub.Run()
		srv := New(cfg, st, am, opencode.New(upstream.URL), hub)

		// Event aggregation residues: ses_snap (in snapshot), ses_stale_busy
		// (was busy, now stale), ses_stale_retry (was retrying, now stale),
		// and ses_active_busy (naively busy, has recent activity — must survive).
		srv.sessionStatuses.Store("ses_snap", "busy")
		srv.sessionStatuses.Store("ses_stale_busy", "busy")
		srv.sessionStatuses.Store("ses_stale_retry", "retry")
		srv.sessionStatuses.Store("ses_active_busy", "busy")
		srv.sessionActivity.Store("ses_active_busy", time.Now())

		rec := httptest.NewRecorder()
		srv.handleSessionStatusAgg(rec)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		typeOf := func(id string) string {
			if m, ok := out[id]; ok {
				if tt, ok := m["type"].(string); ok {
					return tt
				}
			}
			return ""
		}
		if got := typeOf("ses_snap"); got != "busy" {
			t.Fatalf("ses_snap = %q, want busy (snapshot authoritative)", got)
		}
		if got := typeOf("ses_stale_busy"); got != "" {
			t.Fatalf("ses_stale_busy = %q, want dropped", got)
		}
		if got := typeOf("ses_stale_retry"); got != "" {
			t.Fatalf("ses_stale_retry = %q, want dropped", got)
		}
		// ses_active_busy: not in snapshot but has recent activity → kept busy.
		if got := typeOf("ses_active_busy"); got != "busy" {
			t.Fatalf("ses_active_busy = %q, want busy", got)
		}
		// Cleanup pass also prunes the stale map entries.
		if _, ok := srv.sessionStatuses.Load("ses_stale_busy"); ok {
			t.Fatal("ses_stale_busy residue not cleared from aggregation map")
		}
		if _, ok := srv.sessionStatuses.Load("ses_stale_retry"); ok {
			t.Fatal("ses_stale_retry residue not cleared from aggregation map")
		}
	}()
}
