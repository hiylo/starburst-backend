package server

import (
	"context"
	"net/http"
	"time"
)

// handleEmbedConfig reads (GET) or updates (POST) the runtime embeddings
// configuration, kept fully independent from the orchestration LLM settings.
// Requires a web session (admin). The API key is never returned; GET only
// reports whether one is set.
func (s *Server) handleEmbedConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getEmbedConfig(w, r)
	case http.MethodPost:
		s.updateEmbedConfig(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// getEmbedConfig returns the current embeddings endpoint config, key masked.
func (s *Server) getEmbedConfig(w http.ResponseWriter, r *http.Request) {
	url, key, model := "", "", ""
	if s.embedding != nil {
		url, key, model = s.embedding.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":     url,
		"model":   model,
		"keySet":  key != "",
		"enabled": s.embedding != nil && s.embedding.Enabled(),
	})
}

// updateEmbedConfig applies and persists a new embeddings configuration. url
// and model are set verbatim (empty = clear); an empty key keeps the current.
func (s *Server) updateEmbedConfig(w http.ResponseWriter, r *http.Request) {
	if s.embedding == nil {
		writeErr(w, http.StatusServiceUnavailable, "embeddings not configured")
		return
	}
	var req struct {
		URL   string `json:"url"`
		Key   string `json:"key"`
		Model string `json:"model"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Preserve the existing key when the client does not re-submit it.
	_, curKey, _ := s.embedding.Snapshot()
	if req.Key == "" {
		req.Key = curKey
	}

	// Persist for next restart, then apply live.
	_ = s.store.SetSetting(ctx, "embed.url", req.URL)
	_ = s.store.SetSetting(ctx, "embed.key", req.Key)
	_ = s.store.SetSetting(ctx, "embed.model", req.Model)
	s.embedding.SetConfig(req.URL, req.Key, req.Model)

	s.getEmbedConfig(w, r)
}
