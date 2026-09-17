package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare object",
			in:   `{"name":"x","kind":"cron"}`,
			want: `{"name":"x","kind":"cron"}`,
		},
		{
			name: "fenced code block",
			in:   "```json\n{\"name\":\"x\"}\n```",
			want: `{"name":"x"}`,
		},
		{
			name: "fenced without language",
			in:   "```\n{\"name\":\"x\"}\n```",
			want: `{"name":"x"}`,
		},
		{
			name: "prose around object",
			in:   "好的，规则如下：{\"name\":\"x\",\"kind\":\"cron\"} 请确认。",
			want: `{"name":"x","kind":"cron"}`,
		},
		{
			name: "array",
			in:   `[{"a":1},{"a":2}]`,
			want: `[{"a":1},{"a":2}]`,
		},
		{
			name: "no json",
			in:   "没有任何内容",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractJSON(tc.in); got != tc.want {
				t.Fatalf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDisabledClient(t *testing.T) {
	c := New("", "", "")
	if c.Enabled() {
		t.Fatal("client with no config should be disabled")
	}
	if _, err := c.Complete(nil, "", ""); err != errDisabled {
		t.Fatalf("Complete on disabled client = %v, want errDisabled", err)
	}
	var v any
	if err := c.CompleteJSON(nil, "", "", &v); err != errDisabled {
		t.Fatalf("CompleteJSON on disabled client = %v, want errDisabled", err)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		err        error
		want       bool
	}{
		{"http 429 rate limit", 429, nil, true},
		{"http 500 server error", 500, nil, true},
		{"http 503 server error", 503, nil, true},
		{"http 400 client error", 400, nil, false},
		{"http 401 client error", 401, nil, false},
		{"http 404 client error", 404, nil, false},
		{"4xx with transport error", 400, errors.New("boom"), false},
		{"network error no status", 0, errors.New("dial tcp: refused"), true},
		{"nil status and err", 0, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.statusCode, tc.err); got != tc.want {
				t.Fatalf("isRetryable(%d, %v) = %v, want %v", tc.statusCode, tc.err, got, tc.want)
			}
		})
	}
}

func TestSetMaxRetriesZeroDisablesRetry(t *testing.T) {
	// A server that always answers 500 (retryable) with a failing endpoint:
	// with retries disabled the first failure must be returned immediately
	// rather than looping on backoff.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "test-model")
	c.SetMaxRetries(0)
	start := time.Now()
	_, err := c.Complete(context.Background(), "sys", "user")
	if err == nil {
		t.Fatal("expected error from failing server")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("with retries disabled the call took %v, expected fast failure", time.Since(start))
	}
}
