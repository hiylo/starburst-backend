package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/opencode"
)

// newProxyEnv builds a Server whose openCode client points at the given fake
// upstream, registers only the mirror proxy route, and provisions an APP token.
func newProxyEnv(t *testing.T, upstream http.HandlerFunc) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	us := httptest.NewServer(upstream)
	t.Cleanup(us.Close)
	s.openCode = opencode.New(us.URL)

	raw, err := s.auth.CreateToken(context.Background(), "proxy-test")
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(OpenCodeProxyPrefix+"/", s.handleOpenCodeProxy)
	mux.HandleFunc(OpenCodeProxyPrefix, s.handleOpenCodeProxy)
	s.testMux = mux
	return s, raw
}

func TestOpenCodeProxyRequiresToken(t *testing.T) {
	s, _ := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached without APP token")
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/session", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

func TestOpenCodeProxyPassthrough(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotDirHdr, gotAuth string
	var gotBody string
	upstream := func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotDirHdr = r.Header.Get("x-starburst-directory")
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("X-Upstream", "yes")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mirrored":true}`))
	}
	s, tok := newProxyEnv(t, upstream)

	rec := s.do(t, http.MethodPost, OpenCodeProxyPrefix+"/session/ses_1/prompt_async?x=1",
		`{"messageID":"msg_1","parts":[]}`,
		map[string]string{
			"Authorization":          "Bearer " + tok,
			"Content-Type":           "application/json",
			"X-Starburst-Directory": "/w/proj",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/session/ses_1/prompt_async" {
		t.Fatalf("upstream path = %q", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("upstream method = %q", gotMethod)
	}
	if gotQuery != "x=1" {
		t.Fatalf("upstream query = %q", gotQuery)
	}
	if gotDirHdr != "/w/proj" {
		t.Fatalf("directory header not forwarded: %q", gotDirHdr)
	}
	if gotAuth != "" {
		t.Fatalf("APP token must not leak upstream, got %q", gotAuth)
	}
	if !strings.Contains(gotBody, "msg_1") {
		t.Fatalf("request body not forwarded: %q", gotBody)
	}
	if rec.Header().Get("X-Upstream") != "yes" {
		t.Fatalf("upstream response header not relayed")
	}
	if !strings.Contains(rec.Body.String(), `"mirrored":true`) {
		t.Fatalf("upstream body not relayed: %s", rec.Body.String())
	}
}

func TestOpenCodeProxyUpstreamErrorRelayed(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/session", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want upstream 500 relayed, got %d", rec.Code)
	}
}

func TestOpenCodeProxySSEStream(t *testing.T) {
	upstream := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept header not forwarded: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"type\":\"a\"}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: {\"type\":\"b\"}\n\n"))
		fl.Flush()
	}
	s, tok := newProxyEnv(t, upstream)
	rec := s.do(t, http.MethodGet, OpenCodeProxyPrefix+"/global/event", "",
		map[string]string{
			"Authorization": "Bearer " + tok,
			"Accept":        "text/event-stream",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type not relayed: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `{"type":"a"}`) || !strings.Contains(rec.Body.String(), `{"type":"b"}`) {
		t.Fatalf("SSE events not relayed: %q", rec.Body.String())
	}
}

func TestOpenCodeProxyBadPath(t *testing.T) {
	s, tok := newProxyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be reached for non-proxy path")
	})
	rec := s.do(t, http.MethodGet, "/api/not-opencode", "",
		map[string]string{"Authorization": "Bearer " + tok})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for non-proxy path, got %d", rec.Code)
	}
}
