package server

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("{\"a\":\"hello world\"}\n", 1000))
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: x\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	mux.HandleFunc("/empty204", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return gzipMiddleware(mux)
}

func gzipReq(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	testRoutes().ServeHTTP(rec, req)
	return rec
}

func TestGzipJSONResponse(t *testing.T) {
	rec := gzipReq(t, "/json")
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if rec.Body.Len() >= 1000*24 {
		t.Fatalf("gzip response not smaller: %d bytes", rec.Body.Len())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	plain, _ := io.ReadAll(zr)
	if !strings.Contains(string(plain), "hello world") {
		t.Fatal("decompressed body missing content")
	}
}

func TestGzipSSEPassthrough(t *testing.T) {
	rec := gzipReq(t, "/sse")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE should not be compressed, got Content-Encoding %q", got)
	}
	if body := rec.Body.String(); body != "data: x\n\n" {
		t.Fatalf("SSE body = %q", body)
	}
}

func TestGzipNoContentNotCompressed(t *testing.T) {
	rec := gzipReq(t, "/empty204")
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("204 should not be compressed, got %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 body should be empty, got %d bytes", rec.Body.Len())
	}
}

func TestGzipWithoutAcceptEncoding(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/json", nil)
	rec := httptest.NewRecorder()
	testRoutes().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("no Accept-Encoding should not compress, got %q", got)
	}
}

func TestGzipHeadSkipped(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/json", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	testRoutes().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("HEAD should not compress, got %q", got)
	}
}

// 压缩的渐进式响应：Flush() 必须把 gzip.Writer 的 flate 缓冲刷到下游，
// 否则「边生成边输出」在压缩路径下直到 Close 才全量落地（客户端全程空白）。
func TestGzipFlushStreamsPartialData(t *testing.T) {
	handler := gzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, strings.Repeat("d", 130))
		fl.Flush()
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	// 只写 130 字节 + Flush：修复前 body 只有 gzip 头（10 字节），
	// 修复后 Flush 刷出已编码块（显著 > 20 字节）。
	if rec.Body.Len() <= 20 {
		t.Fatalf("gzip Flush did not stream partial data: body=%d bytes", rec.Body.Len())
	}
}
