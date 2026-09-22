package llm

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestChatStreamNoRetryAfterPartialOutput(t *testing.T) {
	// 上游先推送一个增量，随后推一个超过 scanner 缓冲上限（2MB）的超长行触发
	// bufio.ErrTooLong 读错误：已产生 delta 后的流中断不得重试，否则已通过
	// onDelta 推送的 token 会被重复输出一次。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		fl.Flush()
		big := bytes.Repeat([]byte("x"), 3*1024*1024)
		_, _ = w.Write(append(append([]byte("data: "), big...), '\n'))
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "m")
	c.SetMaxRetries(3)
	var got []string
	_, err := c.CompleteJSONStream(context.Background(), "sys", "user", func(s string) error {
		got = append(got, s)
		return nil
	})
	if err == nil {
		t.Fatalf("expected mid-stream error, got nil")
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 delta delivered, got %d", len(got))
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 upstream call (no retry after partial output), got %d", calls.Load())
	}
}

func TestChatStreamRetriesBeforeAnyDelta(t *testing.T) {
	// 首次调用直接 500（尚未产生任何 delta），应触发重试；第二次调用成功输出。
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "key", "m")
	c.SetMaxRetries(3)
	text, err := c.CompleteJSONStream(context.Background(), "sys", "user", func(string) error { return nil })
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if text != "ok" {
		t.Fatalf("text = %q, want ok", text)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 upstream calls (retry before output), got %d", calls.Load())
	}
}

func TestBackoffDelayClampsRetryAfter(t *testing.T) {
	// Retry-After 无上限（如 86400s）时也必须被 clamp 到 backoffMax，避免退避到一天。
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "86400")
	if d := backoffDelay(1, resp); d != backoffMax {
		t.Fatalf("Retry-After 86400s = %v, want clamped to %v", d, backoffMax)
	}
	resp.Header.Set("Retry-After", "5")
	if d := backoffDelay(1, resp); d != 5*time.Second {
		t.Fatalf("Retry-After 5s = %v, want 5s", d)
	}
}
