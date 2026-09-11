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
	"time"
)

// Client wraps HTTP access to a local OpenCode server.
// It does not proxy traffic; it provides orchestration-style queries
// (list/aggregate sessions, status, config) that power the backend features.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a Client talking to the given OpenCode base URL.
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
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
	InputTokens    int `json:"inputTokens"`
	OutputTokens   int `json:"outputTokens"`
	ReasoningTokens int `json:"reasoningTokens"`
	Cost           int `json:"cost"`
	CreatedMs      int64 `json:"createdMs"`
	UpdatedMs      int64 `json:"updatedMs"`
}

// ListSessions returns the OpenCode sessions with metadata from GET /session,
// merged with live busy state from GET /session/status.
func (c *Client) ListSessions(ctx context.Context) ([]SessionInfo, error) {
	// Fetch session metadata list.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/session", nil)
	if err != nil {
		return nil, err
	}
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
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch messages: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch messages: %s", resp.Status)
	}

	var doc []struct {
		Info struct {
			Role string `json:"role"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}

	var out []Message
	for _, m := range doc {
		var sb strings.Builder
		for _, c := range m.Parts {
			if c.Type == "text" && c.Text != "" {
				sb.WriteString(c.Text)
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

// ExportMarkdown renders a session message list as a Markdown transcript.
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

	// Long-lived connection: no overall timeout, rely on ctx + heartbeats.
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("open event stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return fmt.Errorf("open event stream: %s: %s", resp.Status, string(raw))
	}

	// SSE framing: read lines, accumulate "data:" payloads, emit on blank line.
	var buf []byte
	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimCRLF(line)
			if len(trimmed) > 0 && trimmed[0] == 'd' && bytes.HasPrefix(trimmed, []byte("data:")) {
				payload := bytes.TrimSpace(trimmed[len("data:"):])
				buf = append(buf, payload...)
				continue
			}
			// blank line = event boundary
			if len(trimmed) == 0 && len(buf) > 0 {
				if err := onEvent(SSEEvent{Data: append([]byte(nil), buf...)}); err != nil {
					return err
				}
				buf = buf[:0]
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				// flush trailing event if any
				if len(buf) > 0 {
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
