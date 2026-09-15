package server

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
)

// OpenCodeProxyPrefix is the HTTP prefix under which the backend mirrors the
// upstream OpenCode REST API verbatim.
//
// Prefix convention (the APP switches base URL by this prefix):
//
//	/api/opencode/<path>  ↔  opencode /<path>
//
// For example GET /api/opencode/session/{id}/message is relayed to the upstream
// as GET /session/{id}/message, and POST /api/opencode/api/session/{id}/prompt
// reaches opencode's V2 prompt endpoint verbatim. Method, query parameters,
// request body and response body are passed through unchanged, so the APP can
// keep its original opencode paths and only redirect its base URL at the
// backend. The APP token only authorizes entry to the backend; the relayed
// request carries the backend's own upstream credentials (see opencode.Client).
const OpenCodeProxyPrefix = "/api/opencode"

// proxyIsSensitive reports whether an upstream operation is sensitive enough
// that it must not be reachable through an APP token alone. Provider-key writes
// (/auth/*), opening a terminal (/pty) and global teardown (/global/dispose)
// are admin-only; plain reads and permission replies stay open so the APP can
// approve permission requests remotely.
func proxyIsSensitive(path string) bool {
	if strings.HasPrefix(path, "/auth/") {
		return true
	}
	if path == "/auth" {
		return true
	}
	if path == "/pty" || strings.HasPrefix(path, "/pty/") {
		return true
	}
	if path == "/global/dispose" || strings.HasPrefix(path, "/global/dispose/") {
		return true
	}
	if path == "/config" || strings.HasPrefix(path, "/config/") {
		return true
	}
	return false
}

// hopByHopHeaders must not be relayed to the upstream (RFC 9110 §7.6.1): each
// refers to the current connection pair, not the destination server.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
	"Host",
}

// isHopByHopHeader reports whether k must be stripped before relaying upstream.
func isHopByHopHeader(k string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// handleOpenCodeProxy is the OpenCode mirror proxy. The registered route is the
// whole OpenCodeProxyPrefix subtree; any method, any opencode path under it is
// forwarded to the upstream server and the upstream response (headers, status,
// body) is relayed back verbatim. SSE responses (e.g. GET /global/event) are
// streamed through with a flush after every read so events reach the APP in
// real time.
//
// Entry is authenticated with a backend APP token or a web session
// (requireToken || requireWeb), so the web UI's AI workbench can operate the
// mirror without a separately configured APP token. The client Authorization
// header is intentionally NOT forwarded: it belongs to the backend, and the
// upstream receives the credentials configured on the opencode.Client
// (SetAuthToken) instead. Directory scoping headers (x-starburst-directory /
// x-opencode-directory) and query parameters pass through untouched.
func (s *Server) handleOpenCodeProxy(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireToken(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	var upstreamPath string
	switch {
	case r.URL.Path == OpenCodeProxyPrefix, r.URL.Path == OpenCodeProxyPrefix+"/":
		upstreamPath = "/"
	case strings.HasPrefix(r.URL.Path, OpenCodeProxyPrefix+"/"):
		upstreamPath = "/" + strings.TrimPrefix(r.URL.Path, OpenCodeProxyPrefix+"/")
	default:
		writeErr(w, http.StatusNotFound, "not found")
		return
	}

	// 高危上游操作只允许管理员（web session）调用：APP token 一旦泄露即
	// 等同持有 opencode 全权——写 provider key、开 PTY、全局 dispose 都不该
	// 由设备 token 直接触发。读取与 permission reply（App 远程批准）保留。
	if !s.requireWeb(r) && proxyIsSensitive(upstreamPath) {
		writeErr(w, http.StatusForbidden, "operation requires admin web session")
		return
	}

	// 原接口增强（endpoint 不变）：GET /session/status 时合并上游快照与采集器
	// 事件聚合状态——上游快照偶发漏掉部分 busy 会话，事件聚合更完整准确。
	if r.Method == http.MethodGet && upstreamPath == "/session/status" {
		s.handleSessionStatusAgg(w)
		return
	}

	// Relay headers to the upstream, skipping hop-by-hop and the client
	// Authorization (replaced by the backend's own upstream credentials).
	hdr := make(http.Header, len(r.Header))
	for k, vv := range r.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, "Authorization") {
			continue
		}
		hdr[k] = append([]string(nil), vv...)
	}

	// Cancel the upstream connection when the client disconnects.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, err := s.openCode.Do(ctx, r.Method, upstreamPath, r.URL.Query(), r.Body, hdr)
	if err != nil {
		log.Printf("opencode proxy %s %s: %v", r.Method, upstreamPath, err)
		writeErr(w, http.StatusBadGateway, "upstream opencode request failed")
		return
	}
	defer resp.Body.Close()

	// Relay the upstream response headers verbatim (dropping hop-by-hop).
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering for SSE
	w.WriteHeader(resp.StatusCode)

	// SSE: stream with a flush per read so events are delivered immediately.
	// Non-SSE responses just copy through.
	if fl, ok := w.(http.Flusher); ok && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return // client gone
				}
				fl.Flush()
			}
			if rerr != nil {
				return
			}
		}
	}
	_, _ = io.Copy(w, resp.Body)
}
