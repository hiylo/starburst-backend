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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
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
	maxRetries int // additional attempts after the first failure; 0 = no retry
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
		maxRetries: 2,
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

// backoffBase and backoffMax bound the exponential backoff for retryable
// errors. Each retry doubles the delay up to the cap, with jitter to prevent
// thundering-herd on shared deployments.
const (
	backoffBase = 1 * time.Second
	backoffMax  = 30 * time.Second
)

// isRetryable reports whether an HTTP status code or transport error warrants
// a retry. 429 (rate limit), 5xx (server error) and transient network
// failures are retryable; 4xx (client error) is not.
func isRetryable(statusCode int, err error) bool {
	if err != nil {
		// Network/timeout errors are transient and worth retrying.
		return true
	}
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	return statusCode >= 500 && statusCode < 600
}

// backoffDelay computes the delay before retry attempt n (1-indexed).
// Respects a Retry-After header when present (seconds or HTTP date).
func backoffDelay(attempt int, resp *http.Response) time.Duration {
	if resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := time.ParseDuration(ra + "s"); err == nil {
				return secs
			}
			if t, err := http.ParseTime(ra); err == nil {
				if d := time.Until(t); d > 0 {
					return d
				}
			}
		}
	}
	d := backoffBase * time.Duration(1<<uint(attempt-1))
	if d > backoffMax {
		d = backoffMax
	}
	// Add up to 25% jitter to spread retries.
	jitter := time.Duration(int64(d) * int64(mrand.IntN(100)+75) / 100)
	return d + jitter
}

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

// Chat sends a full multi-turn message list and returns the assistant text.
// The caller is responsible for ordering messages (system first, then
// alternating user/assistant turns).
func (c *Client) Chat(ctx context.Context, messages []ChatMessage) (string, error) {
	if !c.Enabled() {
		return "", errDisabled
	}
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
	return DecodeJSON(text, v)
}

// DecodeJSON extracts a JSON object from model text and decodes it into v.
func DecodeJSON(text string, v any) error {
	raw := extractJSON(text)
	if raw == "" {
		return fmt.Errorf("llm: empty JSON output")
	}
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		return fmt.Errorf("llm: invalid JSON output: %w", err)
	}
	return nil
}

// CompleteJSONStream streams the model's raw output through onDelta as it is
// generated, then returns the accumulated full text. The caller is responsible
// for extracting/decoding JSON from the returned text. It uses the
// OpenAI-compatible streaming protocol (stream=true + SSE data chunks).
func (c *Client) CompleteJSONStream(ctx context.Context, system, user string, onDelta func(string) error) (string, error) {
	if !c.Enabled() {
		return "", errDisabled
	}
	messages := []ChatMessage{}
	if system != "" {
		messages = append(messages, ChatMessage{Role: "system", Content: system})
	}
	messages = append(messages, ChatMessage{Role: "user", Content: user})
	return c.chatStream(ctx, messages, 0.1, onDelta)
}

// chatStream performs a streaming completion request with exponential backoff
// retry on transient failures. It delegates to chatStreamOnce for the actual
// HTTP call.
func (c *Client) chatStream(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error) (string, error) {
	var lastResp *http.Response
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(attempt, lastResp)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		result, resp, err := c.chatStreamOnce(ctx, messages, temperature, onDelta)
		lastResp = resp
		if err == nil {
			return result, nil
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if !isRetryable(status, err) || attempt == c.maxRetries {
			return "", err
		}
	}
	return "", nil
}

// chatStreamOnce performs a single streaming completion request.
func (c *Client) chatStreamOnce(ctx context.Context, messages []ChatMessage, temperature float64, onDelta func(string) error) (string, *http.Response, error) {
	baseURL, apiKey, model := c.Snapshot()
	payload := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": temperature,
		"max_tokens":  defaultMaxTokens,
		"stream":      true,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return "", resp, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 500))
	}

	// Parse the OpenAI-compatible SSE stream. Each "data:" line carries a JSON
	// chunk; the stream ends with "data: [DONE]".
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var full strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		full.WriteString(delta)
		if onDelta != nil {
			if err := onDelta(delta); err != nil {
				return "", nil, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, fmt.Errorf("llm: read stream: %w", err)
	}
	return strings.TrimSpace(full.String()), nil, nil
}

// chat performs a completion request with exponential backoff retry on
// transient failures (429, 5xx, network errors). It delegates to chatOnce
// for the actual HTTP call.
func (c *Client) chat(ctx context.Context, messages []ChatMessage, temperature float64) (string, error) {
	var lastResp *http.Response
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoffDelay(attempt, lastResp)
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}
		}
		result, resp, err := c.chatOnce(ctx, messages, temperature)
		lastResp = resp
		if err == nil {
			return result, nil
		}
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if !isRetryable(status, err) || attempt == c.maxRetries {
			return "", err
		}
	}
	return "", nil
}

// chatOnce performs a single completion request and returns the content text,
// the HTTP response (for Retry-After), and any error.
func (c *Client) chatOnce(ctx context.Context, messages []ChatMessage, temperature float64) (string, *http.Response, error) {
	baseURL, apiKey, model := c.Snapshot()
	payload := map[string]any{
		"model":       model,
		"messages":    messages,
		"temperature": temperature,
		"max_tokens":  defaultMaxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("llm: request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return "", resp, fmt.Errorf("llm: %s: %s", resp.Status, truncate(string(raw), 500))
	}

	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("llm: parse response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("llm: empty response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil, nil
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
