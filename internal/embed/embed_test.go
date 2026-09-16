package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientDisabled(t *testing.T) {
	c := New("", "", "bge-m3")
	if c.Enabled() {
		t.Fatal("client with empty URL should be disabled")
	}
	if _, err := c.Embed(context.Background(), "hello"); err != errDisabled {
		t.Fatalf("expected errDisabled, got %v", err)
	}
}

func TestEmbedSingle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if req.Model != "bge-m3" {
			t.Errorf("unexpected model %q", req.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "test-key", "bge-m3")
	if !c.Enabled() {
		t.Fatal("client should be enabled")
	}
	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vec) != 3 {
		t.Fatalf("expected 3 dims, got %d", len(vec))
	}
	if vec[0] != 0.1 || vec[2] != 0.3 {
		t.Fatalf("unexpected vector %v", vec)
	}
}

func TestEmbedBatchOrdered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(req.Input) != 2 {
			t.Fatalf("expected batch of 2, got %d", len(req.Input))
		}
		// Return out of order to verify index-based reconstruction.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[2.0]},{"index":0,"embedding":[1.0]}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", "m")
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("embed batch: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	if vecs[0][0] != 1.0 || vecs[1][0] != 2.0 {
		t.Fatalf("vectors out of order: %v", vecs)
	}
}

func TestEmbedHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "m")
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error for non-200")
	}
}
