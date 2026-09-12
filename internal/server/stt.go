package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// defaultChunkBytes caps a single audio chunk when the config does not set
// one. It matches the engine's own limit.
const defaultChunkBytes = 2 * 1024 * 1024

// sttEngine forwards audio chunks to the streaming recognition engine that
// runs on the NAS. The backend itself never decodes audio: after authenticating
// the caller and capping chunk size, the protocol, status codes and transcripts
// are passed through untouched.
type sttEngine struct {
	url     string
	client  *http.Client
	timeout time.Duration
}

// newSTTEngine builds the engine proxy. Idle connections are reused so each
// 200ms audio chunk does not pay for a fresh TCP handshake.
func newSTTEngine(baseURL string, timeout time.Duration) *sttEngine {
	return &sttEngine{
		url:     strings.TrimRight(baseURL, "/"),
		timeout: timeout,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Execute runs one request against the engine and returns its HTTP status and
// raw body. The caller decides how to surface the result to the client.
func (e *sttEngine) Execute(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	target := e.url
	if path != "" {
		target = e.url + path
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("build stt request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read stt response: %w", err)
	}
	return resp.StatusCode, data, nil
}

// handleSTTStatus reports whether the recognition engine is reachable so the
// client can decide between server-side and on-device recognition.
func (s *Server) handleSTTStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if s.stt == nil {
		writeErr(w, http.StatusServiceUnavailable, "stt not configured")
		return
	}

	status, data, err := s.stt.Execute(r.Context(), http.MethodGet, "/health", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stt engine unreachable")
		return
	}
	if status < 200 || status >= 300 {
		writeErr(w, http.StatusBadGateway, "stt engine unhealthy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       true,
		"maxChunkBytes": s.maxChunkBytes(),
		"engine":        engineInfo(data),
	})
}

// handleSTTCreate opens a recognition session on the engine.
func (s *Server) handleSTTCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if s.stt == nil {
		writeErr(w, http.StatusServiceUnavailable, "stt not configured")
		return
	}

	status, data, err := s.stt.Execute(r.Context(), http.MethodPost, "/sessions", nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stt engine unreachable")
		return
	}
	writeRaw(w, proxyStatus(status), data)
}

// handleSTTSession proxies POST /api/stt/sessions/{id}/chunks and /finish, and
// DELETE to drop a session early. Raw PCM16LE mono 16kHz bytes travel in the
// request body and each response carries the transcript so far.
func (s *Server) handleSTTSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost, http.MethodDelete:
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if s.stt == nil {
		writeErr(w, http.StatusServiceUnavailable, "stt not configured")
		return
	}

	id, action, ok := splitSTTPath(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad stt session path")
		return
	}
	enginePath := "/sessions/" + id + action

	if r.Method == http.MethodPost {
		size, body, err := s.readSTTChunk(r)
		if err != nil {
			code := http.StatusBadRequest
			if errors.As(err, &chunkTooLarge{}) {
				code = http.StatusRequestEntityTooLarge
			}
			writeErr(w, code, err.Error())
			return
		}
		if size == 0 && action == "/chunks" {
			writeErr(w, http.StatusBadRequest, "empty chunk")
			return
		}
		status, data, err := s.stt.Execute(r.Context(), http.MethodPost, enginePath, body)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "stt engine unreachable")
			return
		}
		// On finish the engine returns the final accumulated transcript, which
		// often still contains trailing syllable repeats (streaming ASR flushes
		// the last word a second time). Dedup it here so the text that reaches
		// the input box is already clean instead of relying on the slow LLM
		// refine round-trip that most users won't wait for.
		if action == "/finish" && status == http.StatusOK {
			before := string(data)
			data = dedupEngineTranscript(data)
			if string(data) != before {
				log.Printf("stt finish dedup: %q -> %q", clip(before, 300), clip(string(data), 300))
			}
		}
		writeRaw(w, proxyStatus(status), data)
		return
	}

	status, data, err := s.stt.Execute(r.Context(), http.MethodDelete, enginePath, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "stt engine unreachable")
		return
	}
	writeRaw(w, proxyStatus(status), data)
}

// chunkTooLarge marks an oversized audio chunk so the handler can return 413.
type chunkTooLarge struct{ limit int }

// clip shortens a string for logging, appending an ellipsis when truncated.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// dedupEngineTranscript applies local transcript cleanup to the engine's finish
// response JSON, rewriting only the "text" field while preserving every other
// field (session_id, final, bytes, ...). It is best-effort: any parse failure
// leaves the response untouched so the engine payload is never corrupted.
func dedupEngineTranscript(data []byte) []byte {
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(data, &resp); err != nil {
		return data
	}
	raw, ok := resp["text"]
	if !ok {
		return data
	}
	var original string
	if err := json.Unmarshal(raw, &original); err != nil {
		return data
	}
	cleaned := dedupTranscript(original)
	if cleaned == original {
		return data
	}
	fixed, err := json.Marshal(cleaned)
	if err != nil {
		return data
	}
	resp["text"] = fixed
	out, err := json.Marshal(resp)
	if err != nil {
		return data
	}
	return out
}

func (e chunkTooLarge) Error() string {
	return fmt.Sprintf("chunk too large, max %d bytes", e.limit)
}

// maxChunkBytes returns the configured chunk cap, falling back to the default
// so a zero-value config still behaves sanely.
func (s *Server) maxChunkBytes() int {
	if n := s.cfg.STTMaxChunkBytes; n > 0 {
		return n
	}
	return defaultChunkBytes
}

// readSTTChunk reads the raw audio body, rejecting anything above the limit.
func (s *Server) readSTTChunk(r *http.Request) (int, []byte, error) {
	limit := s.maxChunkBytes()
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return 0, nil, errors.New("read audio failed")
	}
	if len(body) > limit {
		return 0, nil, chunkTooLarge{limit: limit}
	}
	return len(body), body, nil
}

// splitSTTPath parses /api/stt/sessions/{id} and
// /api/stt/sessions/{id}/{chunks|finish} into an id and an engine action.
func splitSTTPath(path string) (id, action string, ok bool) {
	const prefix = "/api/stt/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := path[len(prefix):]
	if rest == "" {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 2)
	id = parts[0]
	if len(parts) > 1 {
		action = "/" + parts[1]
	}
	if !validSTTID(id) || (action != "" && action != "/chunks" && action != "/finish") {
		return "", "", false
	}
	return id, action, true
}

// validSTTID only accepts the lowercase hex ids the engine issues, so a
// malicious path cannot reach an arbitrary engine endpoint.
func validSTTID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'f') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

// proxyStatus maps an engine status to the one exposed to clients: 2xx and 4xx
// pass through, 503 keeps its meaning, and any other 5xx becomes a 502.
func proxyStatus(status int) int {
	switch {
	case status >= 200 && status < 300:
		return status
	case status == http.StatusServiceUnavailable:
		return http.StatusServiceUnavailable
	case status >= 400 && status < 500:
		return status
	default:
		return http.StatusBadGateway
	}
}

// engineInfo decodes the engine /health payload so it can be embedded in the
// status response.
func engineInfo(data []byte) map[string]any {
	info := map[string]any{}
	if err := json.Unmarshal(data, &info); err != nil {
		return map[string]any{}
	}
	return info
}

// writeRaw emits an engine response body verbatim.
func writeRaw(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
