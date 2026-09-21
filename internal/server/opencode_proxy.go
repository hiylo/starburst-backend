package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode"
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
//
// GET /config/providers 是唯一的例外，必须放行：App 与 Web 的模型下拉框都以它为
// 数据源。它原本会随 /config 一族一起被挡成 admin-only，导致 APP token 拿不到
// 模型列表；回程的凭据剥离（providerCredentialPath）才是这里正确的防线。
func proxyIsSensitive(method, path string) bool {
	if method == http.MethodGet && path == "/config/providers" {
		return false
	}
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
	// /global/config 与 /config 同族：含 provider key / 模型等敏感全局配置，必须 admin-only。
	if path == "/global/config" || strings.HasPrefix(path, "/global/config/") {
		return true
	}
	if path == "/config" || strings.HasPrefix(path, "/config/") {
		return true
	}
	return false
}

// providerCredentialPath reports whether an upstream response is a provider /
// config payload that embeds plaintext credentials.
//
// opencode 的 /config/providers 与 /provider 会在每个 provider 的 key 字段里
// 原样回传明文 API Key，而这两个接口是模型下拉框的数据源，App 与 Web 都必须能
// 读——所以访问控制上不能封，只能在回程把凭据字段抹掉。/auth/* 不在此列：那里
// 存的就是密钥本身，且已被 proxyIsSensitive 限制为管理员专属。
func providerCredentialPath(path string) bool {
	// /mcp 的配置（含第三方 API 的 Authorization 头）同样是明文密钥，一并走剥离。
	for _, prefix := range []string{"/config", "/provider", "/mcp"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// maxSanitizedBodyBytes caps how much of an upstream JSON response is buffered
// for redaction. Beyond it we refuse to relay at all rather than risk passing
// credentials through unredacted.
const maxSanitizedBodyBytes = 16 << 20

// maxProxyRequestBytes caps the body the mirror proxy forwards. Attachments are
// inlined as base64 (a 10 MiB file is ~13.4 MiB of JSON), so the cap leaves room
// for that while still refusing unbounded uploads.
const maxProxyRequestBytes = 64 << 20

// credentialKey normalizes a JSON field name (lower-case, separators dropped)
// and reports whether it carries a secret.
//
// 用后缀而不是全名比对：上游会把 ANTHROPIC_API_KEY、aws_secret_access_key 这类
// 名字原样带出来，只比全名会漏。这里宁可多抹——App 与 Web 的 DTO 字段全部带默认
// 值，少一个字段只是不显示，多泄一个密钥是事故；且只 blank 非空字符串，所以
// token 上限、成本这类数字字段不受影响。
var credentialSuffixes = []string{
	"key", "token", "secret", "password", "passwd", "credential",
	"authorization", "bearer", "signature",
}

func credentialKey(name string) bool {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	s := b.String()
	if s == "" {
		return false
	}
	for _, suffix := range credentialSuffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// redactCredentials blanks every credential-looking string in a decoded JSON
// value and returns how many were removed.
func redactCredentials(v any) int {
	switch t := v.(type) {
	case map[string]any:
		removed := 0
		for k, val := range t {
			if s, ok := val.(string); ok && s != "" && credentialKey(k) {
				t[k] = ""
				removed++
				continue
			}
			removed += redactCredentials(val)
		}
		return removed
	case []any:
		removed := 0
		for _, val := range t {
			removed += redactCredentials(val)
		}
		return removed
	}
	return 0
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
	if !s.requireWeb(r) && proxyIsSensitive(r.Method, upstreamPath) {
		writeErr(w, http.StatusForbidden, "operation requires admin web session")
		return
	}

	// 目录约束：透传的 x-starburst-directory / x-opencode-directory 头与 directory
	// query 必须通过 validateWorkDirectory，避免 APP token（或普通调用方）指定
	// /etc、/root 等系统目录再走 /session/{id}/shell、/file/content 等越界。
	// 与 /api/tasks、/api/rules 共用同一套判据，缺失目录则不构成越权（nil）。
	if err := validateRequestDirectories(r); err != nil {
		writeErr(w, http.StatusForbidden, "directory out of scope: "+err.Error())
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

	// provider 目录回传的是明文密钥：命中凭据路径的响应必须整体缓冲、抹掉凭据字段
	// 再回程，此时上游给的 Content-Length / Content-Encoding 已失效。
	// sanitize 需在发上游请求前判定，以便按需改请求头（见下）。
	sanitize := providerCredentialPath(upstreamPath)
	if sanitize {
		// 浏览器默认带 Accept-Encoding: gzip，上游会据此返回 gzip 压缩正文
		// （首字节 0x1f = gzip 魔数），缓冲解析必失败（502）。剥离该头让上游回
		// identity 编码，整段 JSON 可直接解析抹凭据；回程的 Content-Encoding 本来
		// 就会丢弃，不影响客户端。
		hdr.Del("Accept-Encoding")
	}

	// Cancel the upstream connection when the client disconnects.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 请求体上限：附件以 base64 内嵌（10 MiB 附件 ≈ 13.4 MiB JSON），留出余量
	// 但拒绝无界请求体。已知长度先给干净的 413，分块传输由 MaxBytesReader 兜底。
	if r.ContentLength > maxProxyRequestBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProxyRequestBytes)

	resp, err := s.openCode.Do(ctx, r.Method, upstreamPath, r.URL.Query(), r.Body, hdr)
	if err != nil {
		log.Printf("opencode proxy %s %s: %v", r.Method, upstreamPath, err)
		writeErr(w, http.StatusBadGateway, "upstream opencode request failed")
		return
	}
	defer resp.Body.Close()

	contentType := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(contentType, "text/event-stream")
	// 上面已按凭据路径提前判定 sanitize 并在请求头剥离 Accept-Encoding；
	// SSE 路径不可能命中 /config、/provider，这里只是收敛语义。
	if isSSE {
		sanitize = false
	}

	// Relay the upstream response headers verbatim (dropping hop-by-hop).
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) || (sanitize && (k == "Content-Length" || k == "Content-Encoding")) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering for SSE

	if sanitize {
		relaySanitizedJSON(w, resp, upstreamPath)
		return
	}
	// message 响应裁剪：上游 /session/{id}/message 的每条 info.summary.diffs 是
	// 会话文件变更的全量 diff，可单条数 MB（聊天界面并不使用该字段——diff 走
	// /session/{id}/diff，这里只做展示性回传）。缓冲删掉后再回程，避免数 MB
	// 下载 + 浏览器 JSON 解析把「加载对话」拖到很久。
	if r.Method == http.MethodGet && isMessagePath(upstreamPath) {
		relayTrimMessageDiffs(w, resp, upstreamPath)
		return
	}
	w.WriteHeader(resp.StatusCode)

	// SSE: stream with a flush per read so events are delivered immediately.
	// Non-SSE responses just copy through. 每次写前设写 deadline：客户端停读
	// （不消费但 TCP 未断）时写会超时失败并立即退出，避免 handler 无限阻塞挂起。
	// 依赖 statusWriter.Unwrap 让 NewResponseController 能触达底层连接；即使拿
	// 不到 deadline 能力，写失败路径仍能保证断开。不改动响应头/压缩语义。
	if fl, ok := w.(http.Flusher); ok && isSSE {
		rc := http.NewResponseController(w)
		buf := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				_ = rc.SetWriteDeadline(time.Now().Add(sseIdleTimeout))
				if _, werr := w.Write(buf[:n]); werr != nil {
					return // client gone or not draining; stop relaying
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

// relaySanitizedJSON buffers an upstream credential-path response, blanks its
// credential fields and writes the result. Anything it cannot prove safe is
// refused rather than relayed — a passthrough on the failure path would defeat
// the redaction.
func relaySanitizedJSON(w http.ResponseWriter, resp *http.Response, path string) {
	blob, err := io.ReadAll(io.LimitReader(resp.Body, maxSanitizedBodyBytes+1))
	if err != nil {
		log.Printf("opencode proxy %s: read credential payload: %v", path, err)
		writeErr(w, http.StatusBadGateway, "upstream opencode request failed")
		return
	}
	// 防御性：正常情况下代理已对凭据路径剥离 Accept-Encoding（上游返回 identity），
	// 但若上游无视请求头仍回 gzip（或未来走别的通道），直接解析会撞上 gzip 魔数
	// 0x1f 而失败。这里按 Content-Encoding 就地解压，保证解析的是明文 JSON。
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip") {
		if gz, gerr := gzip.NewReader(bytes.NewReader(blob)); gerr == nil {
			if ub, uerr := io.ReadAll(io.LimitReader(gz, maxSanitizedBodyBytes+1)); uerr == nil {
				blob = ub
			}
			_ = gz.Close()
		}
	}
	if int64(len(blob)) > maxSanitizedBodyBytes {
		log.Printf("opencode proxy %s: credential payload over %d bytes, refusing to relay", path, maxSanitizedBodyBytes)
		writeErr(w, http.StatusBadGateway, "upstream response could not be sanitized")
		return
	}
	if len(blob) == 0 {
		// 空响应（204 / 条件请求的 304）没有正文可抹，只回状态码。
		w.WriteHeader(resp.StatusCode)
		return
	}
	var decoded any
	if err := json.Unmarshal(blob, &decoded); err != nil {
		log.Printf("opencode proxy %s: credential payload is not JSON: %v", path, err)
		writeErr(w, http.StatusBadGateway, "upstream response could not be sanitized")
		return
	}
	if n := redactCredentials(decoded); n > 0 {
		log.Printf("opencode proxy %s: redacted %d credential field(s)", path, n)
	}
	out, err := json.Marshal(decoded)
	if err != nil {
		log.Printf("opencode proxy %s: re-encode credential payload: %v", path, err)
		writeErr(w, http.StatusBadGateway, "upstream response could not be sanitized")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// isMessagePath reports whether an upstream path is a GET /session/{id}/message
// listing (the endpoint whose responses can balloon to multi-MB because of
// per-message info.summary.diffs).
func isMessagePath(path string) bool {
	if !strings.HasPrefix(path, "/session/") || !strings.HasSuffix(path, "/message") {
		return false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(path, "/session/"), "/message")
	return rest != "" && !strings.Contains(rest, "/")
}

// maxMessageTrimBytes bounds how much of a message response we buffer for
// summary.diffs trimming. Larger than a single message page (single-message
// diffs can reach several MB, and a 30-message page should stay well under 64MB).
const maxMessageTrimBytes = 64 << 20

// relayTrimMessageDiffs buffers an upstream message response, strips each
// message's info.summary.diffs (the session-wide file diff list that the chat
// view never renders) and writes the shrunk JSON back, so a page that would
// otherwise weigh multiple MB is reduced to the parts the client actually uses.
func relayTrimMessageDiffs(w http.ResponseWriter, resp *http.Response, path string) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMessageTrimBytes+1))
	if err != nil {
		log.Printf("opencode proxy %s: read message payload: %v", path, err)
		writeErr(w, http.StatusBadGateway, "upstream opencode request failed")
		return
	}
	// 原始（可能仍为 gzip 压缩）字节就已超过缓冲上限：不裁剪，保留上游的
	// Content-Length / Content-Encoding 原样透传，避免截断丢消息。
	if int64(len(raw)) > maxMessageTrimBytes {
		log.Printf("opencode proxy %s: message payload over %d bytes, relay verbatim", path, maxMessageTrimBytes)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	gzipEncoded := strings.Contains(strings.ToLower(resp.Header.Get("Content-Encoding")), "gzip")
	decoded := raw
	if gzipEncoded {
		gz, gerr := gzip.NewReader(bytes.NewReader(raw))
		if gerr != nil {
			// 解不开就原样透传（原 Content-Encoding/Content-Length 仍有效）。
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}
		ub, uerr := io.ReadAll(io.LimitReader(gz, maxMessageTrimBytes+1))
		_ = gz.Close()
		if uerr != nil || int64(len(ub)) > maxMessageTrimBytes {
			// 解压后超限：原样透传压缩字节，不删 Content-Encoding/Content-Length，
			// 否则浏览器按旧头解析会被截断或报解码错误。
			log.Printf("opencode proxy %s: decompressed message over %d bytes, relay verbatim", path, maxMessageTrimBytes)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(raw)
			return
		}
		decoded = ub
	}

	var v any
	if err := json.Unmarshal(decoded, &v); err != nil {
		// 非 JSON（不太可能）：原样透传原始字节。
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	n := trimSummaryDiffs(v)
	if n == 0 {
		// 没有可裁剪的 diffs：原样透传原始字节，保持字节与响应头不变。
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}
	out, err := json.Marshal(v)
	if err != nil {
		log.Printf("opencode proxy %s: re-encode trimmed message: %v", path, err)
		writeErr(w, http.StatusBadGateway, "upstream response could not be trimmed")
		return
	}
	log.Printf("opencode proxy %s: trimmed summary.diffs from %d message(s)", path, n)
	// 裁剪后 body 长度已变，Content-Length 作废；若原先是 gzip 压缩的，解压重序列化
	// 后已是明文，Content-Encoding 同样作废，否则浏览器按 gzip 解压明文报解码失败。
	w.Header().Del("Content-Length")
	if gzipEncoded {
		w.Header().Del("Content-Encoding")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// trimSummaryDiffs removes info.summary.diffs from every message in a decoded
// message response (which may be a top-level array or a {messages:[...]} object),
// returning how many messages had the field removed.
func trimSummaryDiffs(v any) int {
	n := 0
	var arr []any
	switch val := v.(type) {
	case []any:
		arr = val
	case map[string]any:
		if ms, ok := val["messages"].([]any); ok {
			arr = ms
		}
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		info, ok := m["info"].(map[string]any)
		if !ok {
			continue
		}
		if sum, ok := info["summary"].(map[string]any); ok {
			if _, exists := sum["diffs"]; exists {
				delete(sum, "diffs")
				n++
			}
		}
	}
	return n
}
