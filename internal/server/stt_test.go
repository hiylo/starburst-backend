package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sttTestToken creates a web session and an APP token, returning the header
// map needed to call token-protected endpoints.
func sttTestToken(t *testing.T, s *Server) map[string]string {
	t.Helper()

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"admin"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status %d", rec.Code)
	}
	var login struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("decode login: %v", err)
	}

	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"phone"}`,
		map[string]string{"X-Web-Session": login.Session})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token status %d: %s", rec.Code, rec.Body.String())
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	return map[string]string{"Authorization": "Bearer " + tok.Token}
}

func TestSTTDisabledWithoutEngine(t *testing.T) {
	s := newTestServer(t)
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodGet, "/api/stt", "", th)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/stt/sessions", "", th)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status %d", rec.Code)
	}
	// Chunk forwarding is refused before any engine call.
	rec = s.do(t, http.MethodPost, "/api/stt/sessions/abc12345/chunks", "pcm", th)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("chunks status %d", rec.Code)
	}
}

func TestSTTProxyStreamsChunks(t *testing.T) {
	var mu sync.Mutex
	var chunks []string
	var gotFinish bool
	var gotDelete bool

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"status":"ok","model":"zipformer","sample_rate":16000,"sessions":1}`))
		case r.URL.Path == "/sessions" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"session_id":"abc12345abcdef67","sample_rate":16000,"channels":1,"sample_width":2}`))
		case strings.HasSuffix(r.URL.Path, "/chunks") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			chunks = append(chunks, string(body))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"session_id":"abc12345abcdef67","text":"昨天是","final":false,"bytes":6400,"seconds":0.4}`))
		case strings.HasSuffix(r.URL.Path, "/finish") && r.Method == http.MethodPost:
			mu.Lock()
			gotFinish = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"session_id":"abc12345abcdef67","text":"昨天是周一","final":true}`))
		case strings.HasPrefix(r.URL.Path, "/sessions/") && r.Method == http.MethodDelete:
			mu.Lock()
			gotDelete = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"deleted":true,"session_id":"abc12345abcdef67"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()

	s := newTestServer(t)
	s.SetSTT(engine.URL, 5*time.Second)
	th := sttTestToken(t, s)

	// Status surfaces the engine health payload for the client's fallback logic.
	rec := s.do(t, http.MethodGet, "/api/stt", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var st struct {
		Enabled       bool           `json:"enabled"`
		MaxChunkBytes int            `json:"maxChunkBytes"`
		Engine        map[string]any `json:"engine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !st.Enabled || st.MaxChunkBytes <= 0 || st.Engine["model"] != "zipformer" {
		t.Fatalf("unexpected status body: %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/stt/sessions", "", th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body.String())
	}
	var sess struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if sess.SessionID == "" {
		t.Fatalf("no session id in %s", rec.Body.String())
	}

	for i := 0; i < 3; i++ {
		payload := "chunk-payload-" + strconv.Itoa(i)
		rec = s.do(t, http.MethodPost, "/api/stt/sessions/"+sess.SessionID+"/chunks", payload, th)
		if rec.Code != http.StatusOK {
			t.Fatalf("chunk %d status %d: %s", i, rec.Code, rec.Body.String())
		}
		var out struct {
			Text  string `json:"text"`
			Final bool   `json:"final"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if out.Text != "昨天是" || out.Final {
			t.Fatalf("unexpected partial %q final=%v", out.Text, out.Final)
		}
	}

	rec = s.do(t, http.MethodPost, "/api/stt/sessions/"+sess.SessionID+"/finish", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("finish status %d: %s", rec.Code, rec.Body.String())
	}
	var fin struct {
		Text  string `json:"text"`
		Final bool   `json:"final"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fin); err != nil {
		t.Fatalf("decode finish: %v", err)
	}
	if fin.Text != "昨天是周一" || !fin.Final {
		t.Fatalf("unexpected final %q final=%v", fin.Text, fin.Final)
	}

	rec = s.do(t, http.MethodDelete, "/api/stt/sessions/"+sess.SessionID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(chunks) != 3 {
		t.Fatalf("engine saw %d chunks, want 3", len(chunks))
	}
	for i, got := range chunks {
		want := "chunk-payload-" + strconv.Itoa(i)
		if got != want {
			t.Fatalf("chunk %d = %q, want %q", i, got, want)
		}
	}
	if !gotFinish || !gotDelete {
		t.Fatalf("engine callbacks missed: finish=%v delete=%v", gotFinish, gotDelete)
	}
}

func TestSTTProxyRejectsOversizedChunk(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer engine.Close()

	s := newTestServer(t)
	s.cfg.STTMaxChunkBytes = 1024
	s.SetSTT(engine.URL, 5*time.Second)
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodPost, "/api/stt/sessions/abc12345/chunks", strings.Repeat("x", 1025), th)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if seen != 0 {
		t.Fatalf("oversized chunk reached the engine (%d calls)", seen)
	}
}

func TestSTTProxyBadGateway(t *testing.T) {
	s := newTestServer(t)
	s.SetSTT("http://127.0.0.1:1", 200*time.Millisecond)
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodGet, "/api/stt", "", th)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/stt/sessions", "", th)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("create status %d", rec.Code)
	}
}

func TestSTTRequiresToken(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer engine.Close()

	s := newTestServer(t)
	s.SetSTT(engine.URL, 5*time.Second)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/stt"},
		{http.MethodPost, "/api/stt/sessions"},
		{http.MethodPost, "/api/stt/sessions/abc12345/chunks"},
		{http.MethodDelete, "/api/stt/sessions/abc12345"},
	}
	for _, c := range cases {
		rec := s.do(t, c.method, c.path, "pcm", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status %d: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
}

func TestSTTSessionPathValidation(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer engine.Close()

	s := newTestServer(t)
	s.SetSTT(engine.URL, 5*time.Second)
	th := sttTestToken(t, s)

	for _, path := range []string{
		"/api/stt/sessions/",
		"/api/stt/sessions/UPPER123/chunks",
		"/api/stt/sessions/abc12345/delete",
		"/api/stt/sessions/abc12345/chunks/extra",
		// Raw ".." segments are cleaned by ServeMux before the handler runs;
		// the percent-encoded form survives to the validator.
		"/api/stt/sessions/abc12345%2F../chunks",
	} {
		rec := s.do(t, http.MethodPost, path, "pcm", th)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s status %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	// GET is not a valid verb for a session endpoint.
	rec := s.do(t, http.MethodGet, "/api/stt/sessions/abc12345/chunks", "", th)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get status %d", rec.Code)
	}
}
