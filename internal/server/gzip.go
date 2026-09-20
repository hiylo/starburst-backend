package server

// HTTP 响应 gzip 压缩中间件。
//
// 目标：App（走 VPN 隧道的移动网络）与 Web 控制台分发的 JSON/静态资源体积很大
// （会话列表、消息、事件、providers 数十 KB ~ 数 MB），明文裸传在带宽受限的
// VPN 下是主要延迟来源之一。客户端带 Accept-Encoding: gzip 时对可压缩响应就地
// gzip，端点代码完全无感知。SSE（text/event-stream）必须保持逐事件 flush，
// 这里显式跳过，绝不对事件流做缓冲式压缩。
//
// 安全约束：
//   - 在 WriteHeader 时做决定（此时 handler 已设好 Content-Type / Content-Encoding，
//     头必须在此刻写定），只压缩可压缩类型，绝不二次压缩已有 Content-Encoding 的响应。
//   - 204 / 304 / 1xx 无正文状态码跳过，避免产生「gzip 编码的空体」。
//   - HEAD 请求无正文，直接跳过。
//   - sync.Pool 复用 gzip.Writer，避免每个压缩响应都分配一次。

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if v := strings.TrimSpace(strings.Split(part, ";")[0]); v == "gzip" || v == "x-gzip" {
			return true
		}
	}
	return false
}

func compressibleContentType(ct string) bool {
	if ct == "" {
		return false
	}
	return strings.Contains(ct, "text/") ||
		strings.Contains(ct, "application/json") ||
		strings.Contains(ct, "application/javascript") ||
		strings.Contains(ct, "image/svg+xml")
}

// gzipResponseWriter 在首次 WriteHeader（显式或隐式）时决定是否压缩，随后把正文
// 全部写入 gzip 流。handler 直接 Write 而没显式 WriteHeader 也很常见，必须兜住。
type gzipResponseWriter struct {
	http.ResponseWriter
	compress    bool
	wroteHeader bool
	gz          *gzip.Writer
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	ct := g.Header().Get("Content-Type")
	if code >= 200 && code != http.StatusNoContent && code != http.StatusNotModified &&
		g.Header().Get("Content-Encoding") == "" &&
		!strings.Contains(ct, "text/event-stream") &&
		compressibleContentType(ct) {
		g.compress = true
		g.gz = gzipWriterPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
		g.Header().Del("Content-Length")
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Add("Vary", "Accept-Encoding")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.compress {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush 转发到下层：SSE 路径未启用压缩（gz==nil），直接透传逐事件 flush；
// 压缩路径先刷 gzip.Writer 的 flate 缓冲（否则渐进式响应会在结束瞬间才全部
// 落地），再刷下层保证真实送达。
func (g *gzipResponseWriter) Flush() {
	if g.compress && g.gz != nil {
		_ = g.gz.Flush()
	}
	if fl, ok := g.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap 暴露下层 ResponseWriter，让 http.NewResponseController(w) 的
// SetWriteDeadline 等能力穿过 gzip 包装层；否则 SSE 慢客户端超时回收因
// ErrNotSupported 全部失效（死代码）。
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

// Close 写 gzip 尾部并归还 writer 到池。无正文的 204/304/1xx 已在 WriteHeader 跳过
// 压缩，不会出现「gzip 编码的空体」。
func (g *gzipResponseWriter) Close() {
	if g.compress && g.gz != nil {
		_ = g.gz.Close()
		gzipWriterPool.Put(g.gz)
		g.gz = nil
	}
}

// Hijack 让 WebSocket 升级能穿过 gzip 包装层。App 端 OkHttp/Ktor 的 WS 握手默认带
// Accept-Encoding: gzip，没有该方法时 gorilla upgrader 会因
// 「response does not implement http.Hijacker」升级失败（HTTP 500），推送通道对 App
// 永久不可用。升级发生在任何 WriteHeader 之前，压缩开关未被触发（gz==nil），
// 因此 Hijack 后归还原始连接、此时直接透传即可。
func (g *gzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// gzipMiddleware 包装整个路由：仅对接受 gzip 的客户端且可压缩响应启用；SSE 原样透传。
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer gw.Close()
		next.ServeHTTP(gw, r)
	})
}
