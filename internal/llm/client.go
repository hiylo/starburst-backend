// Package llm provides a minimal, dependency-free client for OpenAI-compatible
// chat completions (e.g. a local LiteLLM gateway). It powers the "smart
// orchestration" features: natural-language rule generation, result summaries
// and failure self-healing decisions.
//
// The package is intentionally optional: if no URL is configured the client
// is disabled and every call short-circuits, keeping the backend a pure
// deterministic engine when no model is available.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ChatMessage is a single turn in a chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client talks to an OpenAI-compatible chat completions endpoint.
// Its configuration is mutable and safe for concurrent use, so the web UI can
// update url/key/model at runtime without a restart.
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// New builds a Client. If baseURL or apiKey is empty the client is disabled.
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// SetConfig atomically replaces the client's endpoint configuration.
// Empty strings clear the corresponding field.
func (c *Client) SetConfig(baseURL, apiKey, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = strings.TrimRight(baseURL, "/")
	c.apiKey = apiKey
	c.model = model
}

// Snapshot returns a copy of the current configuration. The API key is
// masked unless includeKey is true (used internally / when persisting).
func (c *Client) Snapshot() (baseURL, apiKey, model string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL, c.apiKey, c.model
}

// Enabled reports whether the client can make requests.
func (c *Client) Enabled() bool {
	if c == nil {
		return false
	}
	u, k, m := c.Snapshot()
	return u != "" && k != "" && m != ""
}

// errDisabled is returned when the client is not configured.
var errDisabled = errors.New("llm: not configured")

// defaultMaxTokens caps the completion output length. Reasoning models burn
// their whole budget thinking when unconstrained and return empty content;
// a generous cap keeps them productive while bounding latency and cost.
const defaultMaxTokens = 8000

// Complete sends a system+user prompt and returns the assistant text.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	if !c.Enabled() {
		return "", errDisabled
	}
	messages := []ChatMessage{}
	if system != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: system})
	}
	messages = append(messages, ChatMessage{Role: "user", Content: user})
	return c.chat(ctx, messages, 0.2)
}

// CompleteJSON asks the model for a JSON object and decodes it into v.
// It tolerates ```json fenced output. The system prompt should instruct the
// model to emit only JSON.
func (c *Client) CompleteJSON(ctx context.Context, system, user string, v any) error {
	if !c.Enabled() {
		return errDisabled
	}
	messages := []ChatMessage{}
	if system != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: system})
	}
	messages = append(messages, ChatMessage{Role: "user", Content: user})
	text, err := c.chat(ctx, messages, 0.1)
	if err != nil {
		return err
	}
	raw := extractJSON(text)
	if raw == "" {
		return fmt.Errorf("llm: empty JSON output")
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("llm: invalid JSON output: %w", err)
	}
	return nil
}

// chat performs the low-level completion request and returns content text.
func (c *Client) chat(ctx context.Context, messages []ChatMessage, temperature float64) (string, error) {
	baseURL, apiKey, model := c.Snapshot()
	payload := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": temperature,
		"max_tokens":  defaultMaxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 500))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm: parse response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm: empty response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// extractJSON pulls a JSON object/array out of a model response, tolerating
// markdown code fences and surrounding prose.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	// Strip a single ```json ... ``` fence if present.
	if strings.HasPrefix(s, "```") {
		if end := strings.Index(s, "\n"); end >= 0 {
			s = s[end+1:]
		}
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	// If the whole string parses as JSON, return it directly.
	var probe any
	if json.Unmarshal([]byte(s), &probe) == nil {
		return s
	}
	// Otherwise find the first '{' or '[' and last matching '}' or ']'.
	start := strings.IndexAny(s, "{")
	if start < 0 {
		start = strings.IndexAny(s, "[")
	}
	if start < 0 {
		return ""
	}
	open := s[start]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	end := strings.LastIndexByte(s, closeCh)
	if end <= start {
		return ""
	}
	return s[start : end+1]
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
