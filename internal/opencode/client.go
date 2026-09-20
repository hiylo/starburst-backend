package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// maxResponseBody 限制普通 JSON API（config/sessions 列表等编排式查询）
// 缓冲上游响应的最大内存。镜像代理走流式转发，不受此限制。
const maxResponseBody = 16 << 20 // 16 MiB

// maxMessagesBody 限制 FetchSessionMessages 读取会话消息导出时的最大字节数；
// 该接口数据量天然较大，相比 maxResponseBody 单独放宽，但仍是硬上限——解码按
// 流式进行，超限时读到的正文被截断并以错误失败，不会无界占内存。
const maxMessagesBody = 64 << 20 // 64 MiB

// maxSSEEventSize 限制单个 SSE 事件跨行累积的最大字节数，超限丢弃该事件。
const maxSSEEventSize = 16 << 20 // 16 MiB

// Client wraps HTTP access to a local OpenCode server. It provides both
// orchestration-style queries (list/aggregate sessions, status, config) and a
// raw passthrough ([Do]) that powers the /api/opencode mirror proxy.
type Client struct {
	baseURL string
	// httpClient bounds single orchestration round trips.
	httpClient *http.Client
	// streamClient is timeout-free so SSE/passthrough streams can run long.
	streamClient *http.Client
	// authToken, when set, is attached as "Authorization: Bearer" on every
	// upstream request (e.g. when opencode serve runs with --auth-token).
	// 使用 atomic.Value 存储 string，避免 SetAuthToken 与并发请求间的数据竞争。
	authToken atomic.Value
}

// New creates a Client talking to the given OpenCode base URL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		streamClient: &http.Client{},
	}
}

// SetAuthToken configures an optional Bearer token attached to every upstream
// request. Empty clears it. The token is never logged or persisted.
func (c *Client) SetAuthToken(token string) {
	c.authToken.Store(strings.TrimSpace(token))
}

// applyAuth attaches the configured upstream Bearer token when set.
func (c *Client) applyAuth(req *http.Request) {
	token, _ := c.authToken.Load().(string)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// Health is the response of GET /global/health.
type Health struct {
	Healthy bool   `json:"healthy"`
	Version string `json:"version,omitempty"`
}

// Ping checks whether the OpenCode server is reachable and healthy.
func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/global/health", nil)
	if err != nil {
		return err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opencode health returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var h Health
	if err := json.Unmarshal(body, &h); err != nil {
		return fmt.Errorf("opencode health: parse response: %w", err)
	}
	if !h.Healthy {
		return fmt.Errorf("opencode reports unhealthy")
	}
	return nil
}

// SessionInfo is a single OpenCode session with rich metadata plus a live
// busy flag, used by the console's session/project view.
type SessionInfo struct {
	ID        string `json:"id"`
	Slug      string `json:"slug,omitempty"`
	Title     string `json:"title,omitempty"`
	Directory string `json:"directory,omitempty"`
	Path      string `json:"path,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
	Busy      bool   `json:"busy"`
	// Token/usage summary.
	InputTokens     int   `json:"inputTokens"`
	OutputTokens    int   `json:"outputTokens"`
	ReasoningTokens int   `json:"reasoningTokens"`
	Cost            int   `json:"cost"`
	CreatedMs       int64 `json:"createdMs"`
	UpdatedMs       int64 `json:"updatedMs"`
}

// ListSessions returns the OpenCode sessions with metadata from GET /session,
// merged with live busy state from GET /session/status.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	// Fetch session metadata list.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode sessions: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opencode sessions returned %s", resp.Status)
	}

	type rawSession struct {
		ID        string `json:"id"`
		Slug      string `json:"slug"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Path      string `json:"path"`
		Agent     string `json:"agent"`
		Model     struct {
			ID string `json:"id"`
		} `json:"model"`
		Cost   int `json:"cost"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
		} `json:"tokens"`
		Time struct {
			Created int64 `json:"created"`
			Updated int64 `json:"updated"`
		} `json:"time"`
	}
	var raws []rawSession
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, fmt.Errorf("opencode sessions: parse: %w", err)
	}

	// Fetch busy states.
	busy := map[string]bool{}
	if sreq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session/status", nil); err == nil {
		c.applyAuth(sreq)
		if sresp, err := c.httpClient.Do(sreq); err == nil {
			sbody, _ := io.ReadAll(io.LimitReader(sresp.Body, 4*1024*1024))
			sresp.Body.Close()
			var statuses map[string]struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(sbody, &statuses) == nil {
				for id, st := range statuses {
					busy[id] = st.Type == "busy"
				}
			}
		}
	}

	out := make([]SessionInfo, 0, len(raws))
	for _, r := range raws {
		out = append(out, SessionInfo{
			ID:              r.ID,
			Slug:            r.Slug,
			Title:           r.Title,
			Directory:       r.Directory,
			Path:            r.Path,
			Agent:           r.Agent,
			Model:           r.Model.ID,
			Busy:            busy[r.ID],
			InputTokens:     r.Tokens.Input,
			OutputTokens:    r.Tokens.Output,
			ReasoningTokens: r.Tokens.Reasoning,
			Cost:            r.Cost,
			CreatedMs:       r.Time.Created,
			UpdatedMs:       r.Time.Updated,
		})
	}
	return out, nil
}

// GetVersion fetches the OpenCode server version string from /global/health.
func (c *Client) GetVersion(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/global/health", nil)
	if err != nil {
		return "", err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("opencode health: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opencode health returned %s", resp.Status)
	}
	var h Health
	if err := json.Unmarshal(body, &h); err != nil {
		return "", fmt.Errorf("opencode health: parse: %w", err)
	}
	return h.Version, nil
}

// BaseURL returns the configured OpenCode base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// ResolveURL builds an absolute URL against the OpenCode server for path p.
func (c *Client) ResolveURL(p string) string {
	u, err := url.Parse(c.baseURL + p)
	if err != nil {
		return c.baseURL + p
	}
	return u.String()
}

// Do issues a raw upstream request for the mirror proxy. method, path, query
// and body are passed through untouched; hdr carries the client headers to
// relay verbatim (the Authorization header is always dropped and replaced by
// the proxy's own upstream credentials, if any). The caller owns resp.Body and
// must close it. A timeout-free client is used so SSE streams can run
// indefinitely.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body io.Reader, hdr http.Header) (*http.Response, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	for k, vv := range hdr {
		if strings.EqualFold(k, "Authorization") {
			continue
		}
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	c.applyAuth(req)
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return resp, nil
}

// request is the shared JSON plumbing behind the typed API methods. It builds
// the request, attaches the upstream credentials, and decodes the 2xx response
// body into out. out may be nil, *[]byte, *json.RawMessage, or any value
// json.Unmarshal accepts. Non-2xx responses surface as an error with the
// upstream status and a snippet of the body.
func (c *Client) request(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("%s %s: encode request body: %w", method, path, err)
		}
		rd = bytes.NewReader(raw)
	}

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s %s returned %s: %s", method, path, resp.Status, truncateBytes(raw, 512))
	}
	switch o := out.(type) {
	case nil:
	case *[]byte:
		*o = raw
	case *json.RawMessage:
		*o = json.RawMessage(raw)
	default:
		if err := json.Unmarshal(raw, o); err != nil {
			return fmt.Errorf("%s %s: parse response: %w", method, path, err)
		}
	}
	return nil
}

func truncateBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.TrimSpace(string(b))
}

// getRaw performs GET and returns the raw upstream JSON body.
func (c *Client) getRaw(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodGet, path, query, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// postRaw performs POST and returns the raw upstream JSON body.
func (c *Client) postRaw(ctx context.Context, path string, query url.Values, body any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodPost, path, query, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// patchRaw performs PATCH and returns the raw upstream JSON body.
func (c *Client) patchRaw(ctx context.Context, path string, query url.Values, body any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodPatch, path, query, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// putRaw performs PUT and returns the raw upstream JSON body.
func (c *Client) putRaw(ctx context.Context, path string, query url.Values, body any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodPut, path, query, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// delRaw performs DELETE; any 2xx status is considered success.
func (c *Client) delRaw(ctx context.Context, path string) error {
	return c.request(ctx, http.MethodDelete, path, nil, nil, nil)
}

// Message is a single message in a session export.
type Message struct {
	Role    string
	Content string
}

// FetchSessionMessages retrieves the message list of a session from
// GET /session/{id}/message and flattens content parts to text.
func (c *Client) FetchSessionMessages(ctx context.Context, sessionID string) ([]Message, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session/"+sessionID+"/message", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("fetch messages: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}

	// 上游响应是单个大 JSON 数组，用 Decoder 按元素流式解码，
	// 避免 io.ReadAll 把整个响应（上限 maxMessagesBody）一次性读进内存。
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxMessagesBody))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("parse messages: expected JSON array, got %v", tok)
	}

	var out []Message
	for dec.More() {
		var m struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("parse messages: 上游响应超过 %d MiB 上限被截断", maxMessagesBody>>20)
			}
			return nil, fmt.Errorf("parse messages: %w", err)
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteString("\n")
			}
		}
		content := strings.TrimSpace(sb.String())
		if content == "" {
			continue
		}
		out = append(out, Message{Role: m.Info.Role, Content: content})
	}
	return out, nil
}

func ExportMarkdown(sessionID string, msgs []Message) string {
	var sb strings.Builder
	sb.WriteString("# Session " + sessionID + "\n\n")
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		label := "User"
		if m.Role == "assistant" {
			label = "Assistant"
		} else if m.Role == "tool" {
			label = "Tool"
		}
		sb.WriteString("## " + label + "\n\n")
		sb.WriteString(m.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimSpace(sb.String())
}

// SSEEvent is a single raw event from the OpenCode global event stream.
// The backend relays these verbatim; clients parse the payload themselves.
type SSEEvent struct {
	// Data is the raw SSE "data:" line (JSON) as emitted by the upstream.
	Data []byte
}

// StreamEvents opens the global SSE event stream and calls onEvent for each
// "data:" line received. It blocks until the stream ends or ctx is canceled.
// The upstream URL is GET /global/event with Accept: text/event-stream.
func (c *Client) StreamEvents(ctx context.Context, onEvent func(SSEEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/global/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.applyAuth(req)

	// Long-lived connection: no overall timeout, rely on ctx + heartbeats.
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("open event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("open event stream: %s: %s", resp.Status, string(raw))
	}

	// SSE framing: read lines, accumulate "data:" payloads, emit on blank line.
	// 单事件跨行累积设 maxSSEEventSize 上限：超大/畸形流不再无界占内存。超限时
	// 丢弃该事件（与 App 端 SseFrameDecoder 的超限跳过对齐），不透传给 onEvent。
	var (
		buf     []byte
		dropped bool
	)
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimCRLF(line)
			if len(trimmed) > 0 && trimmed[0] == 'd' && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				if dropped {
					// 事件已超限：跳过剩余 data 行，不再累积内存。
					continue
				}
				if len(buf)+len(payload) > maxSSEEventSize {
					dropped = true
					buf = buf[:0]
					continue
				}
				buf = append(buf, payload...)
				continue
			}
			// blank line = event boundary
			if len(trimmed) == 0 {
				if len(buf) > 0 && !dropped {
					if err := onEvent(SSEEvent{Data: append([]byte(nil), buf...)}); err != nil {
						return err
					}
				}
				// 事件结束，无论是否被丢弃都复位，下个事件重新累积。
				buf = buf[:0]
				dropped = false
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				// flush trailing event if any
				if len(buf) > 0 && !dropped {
					if err := onEvent(SSEEvent{Data: append([]byte(nil), buf...)}); err != nil {
						return err
					}
				}
				return nil
			}
			return rerr
		}
	}
}

func trimCRLF(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	return b
}

// ============================================================================
// Typed API surface. Each method mirrors one OpenCode REST endpoint and decodes
// the upstream JSON body into json.RawMessage so callers keep full fidelity
// (the /api/opencode mirror proxy relays requests verbatim via Do instead).
// ============================================================================

// dirQueryParam blocks used across directory-scoped endpoints.
func dirQuery(directory, workspace string) url.Values {
	q := url.Values{}
	if directory != "" {
		q.Set("directory", directory)
	}
	if workspace != "" {
		q.Set("workspace", workspace)
	}
	return q
}

// CreateSessionRequest is the body of POST /session.
type CreateSessionRequest struct {
	Title    string `json:"title,omitempty"`
	ParentID string `json:"parentID,omitempty"`
}

// UpdateSessionRequest is the body of PATCH /session/{id}; Time carries nested
// keys such as {"archived": <ms|null>}.
type UpdateSessionRequest struct {
	Title string         `json:"title,omitempty"`
	Time  map[string]any `json:"time,omitempty"`
}

// CreateSession creates a session (POST /session). directory is passed as the
// query parameter opencode keys sessions on; it also guards multi-project scopes.
func (c *Client) CreateSession(ctx context.Context, title, parentID, directory string) (json.RawMessage, error) {
	body := CreateSessionRequest{Title: title, ParentID: parentID}
	return c.postRaw(ctx, "/session", dirQuery(directory, ""), body)
}

// ListAllSessions lists root sessions across every project (GET
// /experimental/session?roots=true). The plain /session endpoint only returns
// the current directory's sessions, so this is the mirror of the APP's
// session list. directory scopes the result when non-empty.
func (c *Client) ListAllSessions(ctx context.Context, directory string) (json.RawMessage, error) {
	q := url.Values{"roots": {"true"}}
	if directory != "" {
		q.Set("directory", directory)
	}
	return c.getRaw(ctx, "/experimental/session", q)
}

// ListAllSessionsDetailed lists every session across all projects WITHOUT the
// roots filter (GET /experimental/session, no params). Unlike /session — which
// normalizes every session to projectID 'global' and a flat root directory —
// this endpoint returns each session's real directory and projectID, so it is
// the reliable source for grouping the dashboard by working directory. When
// directory is non-empty it scopes the result to that directory.
func (c *Client) ListAllSessionsDetailed(ctx context.Context, directory string) (json.RawMessage, error) {
	var q url.Values
	if directory != "" {
		q = url.Values{"directory": {directory}}
	}
	return c.getRaw(ctx, "/experimental/session", q)
}

// GetSession fetches a single session (GET /session/{id}).
func (c *Client) GetSession(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/session/"+sessionID, nil)
}

// GetSessionChildren lists a session's child sessions (GET /session/{id}/children).
func (c *Client) GetSessionChildren(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/session/"+sessionID+"/children", nil)
}

// ListSessionStatuses returns the busy/idle map of all sessions
// (GET /session/status).
func (c *Client) ListSessionStatuses(ctx context.Context) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/session/status", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateSession patches a session (PATCH /session/{id}).
func (c *Client) UpdateSession(ctx context.Context, sessionID string, req UpdateSessionRequest) (json.RawMessage, error) {
	return c.patchRaw(ctx, "/session/"+sessionID, nil, req)
}

// DeleteSession deletes a session (DELETE /session/{id}).
func (c *Client) DeleteSession(ctx context.Context, sessionID string) error {
	return c.delRaw(ctx, "/session/"+sessionID)
}

// AbortSession aborts the in-flight run of a session (POST /session/{id}/abort).
func (c *Client) AbortSession(ctx context.Context, sessionID string) error {
	return c.request(ctx, http.MethodPost, "/session/"+sessionID+"/abort", nil, nil, nil)
}

// GetSessionDiff returns the file diff of a session (GET /session/{id}/diff).
func (c *Client) GetSessionDiff(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/session/"+sessionID+"/diff", nil)
}

// ShareSession creates a shareable link for a session (POST /session/{id}/share).
func (c *Client) ShareSession(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/session/"+sessionID+"/share", nil, nil)
}

// UnshareSession removes the shareable link of a session (DELETE /session/{id}/share).
func (c *Client) UnshareSession(ctx context.Context, sessionID string) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodDelete, "/session/"+sessionID+"/share", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SummarizeSession compacts a session into a summary using the given model
// (POST /session/{id}/summarize).
func (c *Client) SummarizeSession(ctx context.Context, sessionID, providerID, modelID string) error {
	body := map[string]any{"providerID": providerID, "modelID": modelID}
	return c.request(ctx, http.MethodPost, "/session/"+sessionID+"/summarize", nil, body, nil)
}

// RevertSession undoes messages from messageID onwards (POST /session/{id}/revert).
func (c *Client) RevertSession(ctx context.Context, sessionID, messageID string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/session/"+sessionID+"/revert", nil, map[string]string{"messageID": messageID})
}

// UnrevertSession redoes the last reverted message (POST /session/{id}/unrevert).
func (c *Client) UnrevertSession(ctx context.Context, sessionID string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/session/"+sessionID+"/unrevert", nil, nil)
}

// ForkSession creates a new session from a message point (POST /session/{id}/fork).
func (c *Client) ForkSession(ctx context.Context, sessionID, messageID string) (json.RawMessage, error) {
	body := map[string]string{"messageID": messageID}
	return c.postRaw(ctx, "/session/"+sessionID+"/fork", nil, body)
}

// ExecuteCommand runs a registered slash command in a session
// (POST /session/{id}/command, body {"command","arguments"}).
func (c *Client) ExecuteCommand(ctx context.Context, sessionID, command, arguments string) error {
	body := map[string]string{"command": command, "arguments": arguments}
	return c.request(ctx, http.MethodPost, "/session/"+sessionID+"/command", nil, body, nil)
}

// RunShellCommand runs a shell command in a session
// (POST /session/{id}/shell, body ShellRequest).
func (c *Client) RunShellCommand(ctx context.Context, sessionID, agent, command string, model map[string]any) error {
	body := map[string]any{"agent": agent, "command": command, "model": model}
	return c.request(ctx, http.MethodPost, "/session/"+sessionID+"/shell", nil, body, nil)
}

// PromptPart is one attachment of a prompt (text / file / url).
type PromptPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`
	Mime     string `json:"mime,omitempty"`
	URL      string `json:"url,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// PromptAsyncRequest is the body of POST /session/{id}/prompt_async.
type PromptAsyncRequest struct {
	MessageID string          `json:"messageID"`
	Parts     []PromptPart    `json:"parts"`
	Model     map[string]any  `json:"model,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	Variant   string          `json:"variant,omitempty"`
	System    string          `json:"system,omitempty"`
	Tools     map[string]bool `json:"tools,omitempty"`
}

// PromptAsync sends a fire-and-forget prompt (POST /session/{id}/prompt_async).
// Any 2xx response (usually 204) is success.
func (c *Client) PromptAsync(ctx context.Context, sessionID string, req PromptAsyncRequest) error {
	return c.request(ctx, http.MethodPost, "/session/"+sessionID+"/prompt_async", nil, req, nil)
}

// V2PromptRequest is the body of POST /api/session/{id}/prompt.
type V2PromptRequest struct {
	ID       string   `json:"id"`
	Prompt   V2Prompt `json:"prompt"`
	Delivery string   `json:"delivery,omitempty"`
	Resume   bool     `json:"resume"`
}

// V2Prompt is the prompt payload of the V2 admission endpoint.
type V2Prompt struct {
	Text   string              `json:"text"`
	Files  []V2FileAttachment  `json:"files,omitempty"`
	Agents []V2AgentAttachment `json:"agents,omitempty"`
}

// V2FileAttachment describes a file reference in a V2 prompt.
type V2FileAttachment struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// V2AgentAttachment is a subagent reference in a V2 prompt.
type V2AgentAttachment struct {
	Name string `json:"name"`
}

// PromptV2 submits a prompt through the V2 admission pipeline
// (POST /api/session/{id}/prompt). directory/workspace are query parameters.
func (c *Client) PromptV2(ctx context.Context, sessionID string, req V2PromptRequest, directory, workspaceID string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/api/session/"+sessionID+"/prompt", dirQuery(directory, workspaceID), req)
}

// SwitchSessionAgent switches the active agent of a session
// (POST /api/session/{id}/agent, body {"agent"}).
func (c *Client) SwitchSessionAgent(ctx context.Context, sessionID, agent, directory, workspaceID string) error {
	body := map[string]string{"agent": agent}
	return c.request(ctx, http.MethodPost, "/api/session/"+sessionID+"/agent", dirQuery(directory, workspaceID), body, nil)
}

// SwitchSessionModel switches the model of a session
// (POST /api/session/{id}/model, body {"providerID","modelID","variant"}).
func (c *Client) SwitchSessionModel(ctx context.Context, sessionID, providerID, modelID, variant, directory, workspaceID string) error {
	body := map[string]any{"providerID": providerID, "modelID": modelID, "variant": variant}
	return c.request(ctx, http.MethodPost, "/api/session/"+sessionID+"/model", dirQuery(directory, workspaceID), body, nil)
}

// ListMessages returns the raw message list of a session
// (GET /session/{id}/message?limit=&before=). before paginates via the
// X-Next-Cursor header, which is not surfaced through this raw path.
func (c *Client) ListMessages(ctx context.Context, sessionID string, limit int, before string) (json.RawMessage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if before != "" {
		q.Set("before", before)
	}
	return c.getRaw(ctx, "/session/"+sessionID+"/message", q)
}

// GetMessage returns a single message with its parts
// (GET /session/{id}/message/{messageID}).
func (c *Client) GetMessage(ctx context.Context, sessionID, messageID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/session/"+sessionID+"/message/"+messageID, nil)
}

// SearchText runs a text search across the worktree (GET /find?pattern=).
func (c *Client) SearchText(ctx context.Context, pattern string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/find", url.Values{"pattern": {pattern}})
}

// FindFiles locates files by query (GET /find/file). type, limit and dirs are
// optional filters; directory scopes the search.
func (c *Client) FindFiles(ctx context.Context, query, ftype, directory string, limit int, dirs string) (json.RawMessage, error) {
	q := url.Values{"query": {query}}
	if ftype != "" {
		q.Set("type", ftype)
	}
	if directory != "" {
		q.Set("directory", directory)
	}
	if dirs != "" {
		q.Set("dirs", dirs)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	return c.getRaw(ctx, "/find/file", q)
}

// ReadFile returns the content of a file (GET /file/content?path=&directory=).
func (c *Client) ReadFile(ctx context.Context, path, directory string) (json.RawMessage, error) {
	q := url.Values{"path": {path}}
	if directory != "" {
		q.Set("directory", directory)
	}
	return c.getRaw(ctx, "/file/content", q)
}

// ListDirectory lists a directory's entries (GET /file?path=&directory=).
func (c *Client) ListDirectory(ctx context.Context, path, directory string) (json.RawMessage, error) {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	if directory != "" {
		q.Set("directory", directory)
	}
	return c.getRaw(ctx, "/file", q)
}

// ListProjects lists the projects known to the server (GET /project).
func (c *Client) ListProjects(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/project", nil)
}

// GetProjectDirectories lists the directories of a project
// (GET /project/{id}/directories).
func (c *Client) GetProjectDirectories(ctx context.Context, projectID, directory, workspaceID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/project/"+projectID+"/directories", dirQuery(directory, workspaceID))
}

// GetCurrentProject returns the project of the current worktree (GET /project/current).
func (c *Client) GetCurrentProject(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/project/current", nil)
}

// ListWorkspaces lists workspaces (GET /experimental/workspace?directory=).
func (c *Client) ListWorkspaces(ctx context.Context, directory, workspaceID string) (json.RawMessage, error) {
	return c.getRaw(ctx, "/experimental/workspace", dirQuery(directory, workspaceID))
}

// GetServerPaths returns the server's directories (home, state, worktree, …)
// from GET /path.
func (c *Client) GetServerPaths(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/path", nil)
}

// ListAgents lists the available agents (GET /agent).
func (c *Client) ListAgents(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/agent", nil)
}

// ListCommands lists available slash commands (GET /command).
func (c *Client) ListCommands(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/command", nil)
}

// ListSkills lists the server's skills incl. SKILL.md content (GET /skill).
func (c *Client) ListSkills(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/skill", nil)
}

// ListPendingPermissions lists pending permission requests (GET /permission).
func (c *Client) ListPendingPermissions(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/permission", nil)
}

// ReplyToPermission answers a permission request (POST /permission/{id}/reply;
// body {"reply": "once"|"always"|"reject", "message"?}).
func (c *Client) ReplyToPermission(ctx context.Context, requestID, reply, message string) error {
	body := map[string]string{"reply": reply, "message": message}
	return c.request(ctx, http.MethodPost, "/permission/"+requestID+"/reply", nil, body, nil)
}

// ListPendingQuestions lists pending question requests (GET /question).
func (c *Client) ListPendingQuestions(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/question", nil)
}

// ReplyToQuestion answers a question request (POST /question/{id}/reply; body
// {"answers": string[][]}).
func (c *Client) ReplyToQuestion(ctx context.Context, requestID string, answers [][]string) error {
	return c.request(ctx, http.MethodPost, "/question/"+requestID+"/reply", nil, map[string]any{"answers": answers}, nil)
}

// RejectQuestion rejects a question request (POST /question/{id}/reject).
func (c *Client) RejectQuestion(ctx context.Context, requestID string) error {
	return c.request(ctx, http.MethodPost, "/question/"+requestID+"/reject", nil, nil, nil)
}

// GetMcpStatus returns MCP server connection statuses (GET /mcp).
func (c *Client) GetMcpStatus(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/mcp", nil)
}

// connectMcp connects or disconnects an MCP server (POST /mcp/{name}/{action}).
func (c *Client) connectMcp(ctx context.Context, name, action string) error {
	return c.request(ctx, http.MethodPost, "/mcp/"+url.PathEscape(name)+"/"+action, nil, nil, nil)
}

// ConnectMcp connects an MCP server (POST /mcp/{name}/connect).
func (c *Client) ConnectMcp(ctx context.Context, name string) error {
	return c.connectMcp(ctx, name, "connect")
}

// DisconnectMcp disconnects an MCP server (POST /mcp/{name}/disconnect).
func (c *Client) DisconnectMcp(ctx context.Context, name string) error {
	return c.connectMcp(ctx, name, "disconnect")
}

// StartMcpAuth kicks off OAuth for an MCP server (POST /mcp/{name}/auth).
func (c *Client) StartMcpAuth(ctx context.Context, name string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/mcp/"+url.PathEscape(name)+"/auth", nil, nil)
}

// GetProviders returns available providers and models (GET /config/providers).
func (c *Client) GetProviders(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/config/providers", nil)
}

// ListProviderCatalog returns the provider catalog incl. connection status
// (GET /provider).
func (c *Client) ListProviderCatalog(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/provider", nil)
}

// GetProviderAuthMethods returns the auth methods each provider supports
// (GET /provider/auth).
func (c *Client) GetProviderAuthMethods(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/provider/auth", nil)
}

// AuthorizeProviderOAuth starts an OAuth flow for a provider
// (POST /provider/{id}/oauth/authorize, body {"method": index}).
func (c *Client) AuthorizeProviderOAuth(ctx context.Context, providerID string, method int) (json.RawMessage, error) {
	return c.postRaw(ctx, "/provider/"+url.PathEscape(providerID)+"/oauth/authorize", nil, map[string]int{"method": method})
}

// CompleteProviderOAuth finishes an OAuth flow for a provider
// (POST /provider/{id}/oauth/callback, body {"method","code"?}).
func (c *Client) CompleteProviderOAuth(ctx context.Context, providerID string, method int, code string) error {
	body := map[string]any{"method": method}
	if code != "" {
		body["code"] = code
	}
	return c.request(ctx, http.MethodPost, "/provider/"+url.PathEscape(providerID)+"/oauth/callback", nil, body, nil)
}

// SetProviderApiKey stores an API key for a provider (PUT /auth/{id}, body
// {"type":"api","key"}).
func (c *Client) SetProviderApiKey(ctx context.Context, providerID, apiKey string) error {
	body := map[string]string{"type": "api", "key": apiKey}
	return c.request(ctx, http.MethodPut, "/auth/"+url.PathEscape(providerID), nil, body, nil)
}

// RemoveProviderAuth deletes the stored auth for a provider (DELETE /auth/{id}).
func (c *Client) RemoveProviderAuth(ctx context.Context, providerID string) error {
	return c.delRaw(ctx, "/auth/"+url.PathEscape(providerID))
}

// GetConfig returns the server config (GET /config).
func (c *Client) GetConfig(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/config", nil)
}

// GetGlobalConfig returns the global server config (GET /global/config).
func (c *Client) GetGlobalConfig(ctx context.Context) (json.RawMessage, error) {
	return c.getRaw(ctx, "/global/config", nil)
}

// UpdateConfig patches the server config (PATCH /config).
func (c *Client) UpdateConfig(ctx context.Context, patch json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodPatch, "/config", nil, patch, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateGlobalConfig patches the global server config (PATCH /global/config).
func (c *Client) UpdateGlobalConfig(ctx context.Context, patch json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.request(ctx, http.MethodPatch, "/global/config", nil, patch, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DisposeGlobal disposes all global instances and refreshes state
// (POST /global/dispose).
func (c *Client) DisposeGlobal(ctx context.Context) error {
	return c.request(ctx, http.MethodPost, "/global/dispose", nil, nil, nil)
}

// PtyCreateRequest is the body of POST /pty.
type PtyCreateRequest struct {
	Title string `json:"title,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
}

// PtySize is the terminal dimensions used by PUT /pty/{id}.
type PtySize struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// PtyUpdateRequest is the body of PUT /pty/{id}.
type PtyUpdateRequest struct {
	Title string   `json:"title,omitempty"`
	Size  *PtySize `json:"size,omitempty"`
}

// CreatePty starts a PTY (POST /pty).
func (c *Client) CreatePty(ctx context.Context, title, cwd string) (json.RawMessage, error) {
	return c.postRaw(ctx, "/pty", nil, PtyCreateRequest{Title: title, Cwd: cwd})
}

// UpdatePtySize updates a PTY's terminal size (PUT /pty/{id}).
func (c *Client) UpdatePtySize(ctx context.Context, ptyID string, rows, cols int) error {
	body := PtyUpdateRequest{Size: &PtySize{Rows: rows, Cols: cols}}
	return c.request(ctx, http.MethodPut, "/pty/"+url.PathEscape(ptyID), nil, body, nil)
}

// DeletePty stops and removes a PTY (DELETE /pty/{id}).
func (c *Client) DeletePty(ctx context.Context, ptyID string) error {
	return c.delRaw(ctx, "/pty/"+url.PathEscape(ptyID))
}
