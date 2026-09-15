package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// newTestServer spins up a fake OpenCode HTTP server.
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestPingHealthy(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"version":"0.1.0"}`))
	})
	c := New(srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestPingUnhealthy(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":false}`))
	})
	c := New(srv.URL)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatalf("expected error for unhealthy upstream")
	}
}

func TestPingNon200(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	c := New(srv.URL)
	if err := c.Ping(context.Background()); err == nil {
		t.Fatalf("expected error for non-200")
	}
}

func TestListSessionsRichMetadata(t *testing.T) {
	// /session returns a list of rich session objects; /session/status
	// returns busy flags keyed by id.
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			body := []map[string]any{
				{
					"id": "ses_a", "slug": "alpha", "title": "Alpha work", "directory": "/w/a",
					"agent": "build", "model": map[string]any{"id": "m1"},
					"cost": 5, "tokens": map[string]any{"input": 100, "output": 20, "reasoning": 10},
					"time": map[string]any{"created": 1000, "updated": 2000},
				},
				{
					"id": "ses_b", "slug": "beta", "title": "Beta work", "directory": "/w/b",
					"agent": "build", "model": map[string]any{"id": "m2"},
					"cost": 0, "tokens": map[string]any{"input": 50, "output": 5, "reasoning": 2},
					"time": map[string]any{"created": 1000, "updated": 2000},
				},
			}
			_ = json.NewEncoder(w).Encode(body)
		case "/session/status":
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{
				"ses_a": {"type": "busy"},
				"ses_b": {"type": "idle"},
			})
		default:
			http.NotFound(w, r)
		}
	})
	c := New(srv.URL)
	sessions, err := c.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	// Find ses_a and verify rich fields.
	var a *SessionInfo
	for i := range sessions {
		if sessions[i].ID == "ses_a" {
			a = &sessions[i]
		}
	}
	if a == nil {
		t.Fatalf("ses_a not found")
	}
	if a.Title != "Alpha work" || a.Model != "m1" || a.Agent != "build" {
		t.Fatalf("rich metadata missing: %+v", a)
	}
	if !a.Busy {
		t.Fatalf("ses_a should be busy")
	}
	if a.InputTokens != 100 || a.OutputTokens != 20 || a.Cost != 5 {
		t.Fatalf("usage fields wrong: %+v", a)
	}
}

func TestListSessionsError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	})
	c := New(srv.URL)
	if _, err := c.ListSessions(context.Background()); err == nil {
		t.Fatalf("expected error on non-200")
	}
}

func TestGetVersion(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1.2.3"}`))
	})
	c := New(srv.URL)
	v, err := c.GetVersion(context.Background())
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if v != "v1.2.3" {
		t.Fatalf("got %q want %q", v, "v1.2.3")
	}
}

func TestBaseURLTrailingSlash(t *testing.T) {
	c := New("http://example.com:4096/")
	if c.BaseURL() != "http://example.com:4096" {
		t.Fatalf("trailing slash not trimmed: %q", c.BaseURL())
	}
}

func TestExportMarkdown(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
		{Role: "user", Content: ""}, // empty should be skipped
	}
	out := ExportMarkdown("ses_1", msgs)
	if !strings.Contains(out, "# Session ses_1") {
		t.Fatalf("missing header")
	}
	if !strings.Contains(out, "## User") || !strings.Contains(out, "hello") {
		t.Fatalf("missing user block")
	}
	if !strings.Contains(out, "## Assistant") || !strings.Contains(out, "hi there") {
		t.Fatalf("missing assistant block")
	}
	if strings.Count(out, "## User") != 1 {
		t.Fatalf("empty user message not skipped: %s", out)
	}
}

// TestFetchSessionMessagesRealShape ensures the parser handles the actual
// OpenCode message envelope ({info.role, parts[].text}) rather than the
// legacy {role, content[]} shape, so archives are not empty.
func TestFetchSessionMessagesRealShape(t *testing.T) {
	body := `[
		{"info":{"id":"msg_1","role":"user"},
		 "parts":[{"type":"text","text":"hello"}]},
		{"info":{"id":"msg_2","role":"assistant"},
		 "parts":[{"type":"text","text":"hi there"}]},
		{"info":{"id":"msg_3","role":"tool"},
		 "parts":[{"type":"tool","tool":"bash"}]}
	]`
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/session/ses_1/message" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	})
	c := New(srv.URL)
	msgs, err := c.FetchSessionMessages(context.Background(), "ses_1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (tool part filtered), got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Fatalf("bad first message: %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "hi there" {
		t.Fatalf("bad second message: %+v", msgs[1])
	}
}

func TestStreamEventsParsesSSE(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/global/event" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"a\"}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"b\"}\n\n"))
		fl.Flush()
	})
	c := New(srv.URL)

	var events []string
	ctx := context.Background()
	err := c.StreamEvents(ctx, func(ev SSEEvent) error {
		events = append(events, string(ev.Data))
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(events), events)
	}
	if events[0] != `{"type":"a"}` || events[1] != `{"type":"b"}` {
		t.Fatalf("unexpected payloads: %v", events)
	}
}

func TestStreamEventsMultiLineData(t *testing.T) {
	// An SSE event whose data spans multiple "data:" lines should be
	// concatenated and emitted once at the blank-line boundary.
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\n"))
		_, _ = w.Write([]byte("data: \"multi\"}\n\n"))
		fl.Flush()
	})
	c := New(srv.URL)

	var events []string
	ctx := context.Background()
	_ = c.StreamEvents(ctx, func(ev SSEEvent) error {
		events = append(events, string(ev.Data))
		return nil
	})
	if len(events) != 1 {
		t.Fatalf("expected 1 concatenated event, got %d", len(events))
	}
	if events[0] != `{"type":"multi"}` {
		t.Fatalf("bad payload: %q", events[0])
	}
}

func TestStreamEventsNon200(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	})
	c := New(srv.URL)
	if err := c.StreamEvents(context.Background(), func(SSEEvent) error { return nil }); err == nil {
		t.Fatalf("expected error for non-200")
	}
}

// ===== typed API surface tests =====

func TestSetAuthTokenAttachedToRequests(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-secret" {
			t.Errorf("auth header = %q, want upstream token", got)
		}
		_, _ = w.Write([]byte(`{"healthy":true,"version":"v1"}`))
	})
	c := New(srv.URL)
	c.SetAuthToken("upstream-secret")
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestCreateSessionSendsDirectoryAndBody(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("directory") != "/w/p" {
			t.Fatalf("directory query missing: %s", r.URL.RawQuery)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["title"] != "New" || body["parentID"] != "ses_par" {
			t.Fatalf("bad body %v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"ses_new","title":"New"}`))
	})
	c := New(srv.URL)
	raw, err := c.CreateSession(context.Background(), "New", "ses_par", "/w/p")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.Contains(string(raw), "ses_new") {
		t.Fatalf("unexpected body %s", raw)
	}
}

func TestListAllSessionsRootsQuery(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/experimental/session" || r.URL.Query().Get("roots") != "true" {
			t.Fatalf("unexpected %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"id":"s1"}]`))
	})
	c := New(srv.URL)
	raw, err := c.ListAllSessions(context.Background(), "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(string(raw), "s1") {
		t.Fatalf("bad body %s", raw)
	}
}

func TestListMessagesPaginationQuery(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/session/ses_1/message" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "10" || r.URL.Query().Get("before") != "msg_9" {
			t.Fatalf("bad query %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"id":"msg_8"}]`))
	})
	c := New(srv.URL)
	raw, err := c.ListMessages(context.Background(), "ses_1", 10, "msg_9")
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if !strings.Contains(string(raw), "msg_8") {
		t.Fatalf("bad body %s", raw)
	}
}

func TestPromptV2SendsDirectoryAndWorkspace(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/session/ses_1/prompt" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("directory") != "/w" || q.Get("workspace") != "ws_1" {
			t.Fatalf("bad query %s", r.URL.RawQuery)
		}
		var body V2PromptRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Prompt.Text != "请解释这段代码" || body.Delivery != "steer" {
			t.Fatalf("bad body %+v", body)
		}
		_, _ = w.Write([]byte(`{"data":{"id":"msg_1","admittedSeq":1}}`))
	})
	c := New(srv.URL)
	req := V2PromptRequest{
		ID:       "msg_1",
		Prompt:   V2Prompt{Text: "请解释这段代码"},
		Delivery: "steer",
		Resume:   true,
	}
	raw, err := c.PromptV2(context.Background(), "ses_1", req, "/w", "ws_1")
	if err != nil {
		t.Fatalf("prompt v2: %v", err)
	}
	if !strings.Contains(string(raw), "admittedSeq") {
		t.Fatalf("bad body %s", raw)
	}
}

func TestTypedCallNon2xxError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusUnauthorized)
	})
	c := New(srv.URL)
	if err := c.DeleteSession(context.Background(), "ses_1"); err == nil {
		t.Fatalf("expected error on 401")
	}
}

func TestDoPassesMethodPathQueryBody(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/s1/prompt_async" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("directory") != "/w" {
			t.Fatalf("query lost: %s", r.URL.RawQuery)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"hi":1}` {
			t.Fatalf("body %s", body)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Fatalf("Authorization must not be relayed by Do, got %q", auth)
		}
		if r.Header.Get("X-Starburst-Directory") != "/w" {
			t.Fatalf("custom header not relayed")
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := New(srv.URL)
	hdr := http.Header{"X-Starburst-Directory": {"/w"}, "Authorization": {"Bearer app-token"}}
	resp, err := c.Do(context.Background(), http.MethodPost, "/session/s1/prompt_async",
		url.Values{"directory": {"/w"}}, strings.NewReader(`{"hi":1}`), hdr)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
