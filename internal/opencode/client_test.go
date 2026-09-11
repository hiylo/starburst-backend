package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
