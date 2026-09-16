// Package embed provides a minimal, dependency-free client for OpenAI-compatible
// embeddings endpoints (e.g. a LiteLLM gateway or an Ollama OpenAI-compatible
// /v1/embeddings endpoint). It powers the project knowledge-base retrieval:
// turning source/document chunks into vectors stored in pgvector.
//
// Like the llm package, it is intentionally optional: if no URL is configured
// the client is disabled and every call short-circuits, so the backend keeps
// running without a vector backend when none is available.
package embed

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

// Client talks to an OpenAI-compatible embeddings endpoint. Its configuration
// is mutable and safe for concurrent use, so the web UI can update url/key/model
// at runtime without a restart, independently of the orchestration LLM.
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

// New builds a Client. If baseURL or model is empty the client is disabled.
func New(baseURL, apiKey, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
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

// Snapshot returns a copy of the current configuration.
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
	u, _, m := c.Snapshot()
	return u != "" && m != ""
}

// errDisabled is returned when the client is not configured.
var errDisabled = errors.New("embed: not configured")

// Embed computes the embedding vector for a single input string. The returned
// vector's dimension is model-specific (e.g. bge-m3 = 1024, nomic-embed-text = 768);
// callers storing into a fixed-dimension pgvector column must verify it matches.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}
	return vecs[0], nil
}

// EmbedBatch computes embeddings for multiple inputs in a single request.
// It returns one vector per input, in request order.
func (c *Client) EmbedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	if !c.Enabled() {
		return nil, errDisabled
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	baseURL, apiKey, model := c.Snapshot()

	var input any = inputs[0]
	if len(inputs) > 1 {
		input = inputs
	}
	payload := map[string]any{
		"model": model,
		"input": input,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("embed: %s: %s", resp.Status, truncate(string(raw), 500))
	}

	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: parse response: %w", err)
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}

	// Some providers return data out of order; honor the index field.
	vecs := make([][]float32, len(inputs))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(vecs) {
			continue
		}
		vecs[d.Index] = d.Embedding
	}
	for i, v := range vecs {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed: missing embedding for input %d", i)
		}
	}
	return vecs, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
