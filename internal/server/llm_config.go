package server

import (
	"context"
	"net/http"
	"time"
)

// handleLLMConfig reads (GET) or updates (POST) the runtime LLM configuration.
// Requires a web session (admin). The API key is never returned; GET only
// reports whether one is set.
func (s *Server) handleLLMConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "web session required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getLLMConfig(w, r)
	case http.MethodPost:
		s.updateLLMConfig(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// getLLMConfig returns the current LLM endpoint config, with the key masked.
func (s *Server) getLLMConfig(w http.ResponseWriter, r *http.Request) {
	url, key, model := "", "", ""
	if s.llm != nil {
		url, key, model = s.llm.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"url":     url,
		"model":   model,
		"keySet":  key != "",
		"enabled": s.llm != nil && s.llm.Enabled(),
	})
}

// updateLLMConfig applies and persists a new LLM configuration. url and model
// are set verbatim (empty = clear); an empty key keeps the current key.
func (s *Server) updateLLMConfig(w http.ResponseWriter, r *http.Request) {
	if s.llm == nil {
		writeErr(w, http.StatusServiceUnavailable, "llm not configured")
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
	_, curKey, _ := s.llm.Snapshot()
	if req.Key == "" {
		req.Key = curKey
	}

	// Persist for next restart, then apply live.
	_ = s.store.SetSetting(ctx, "llm.url", req.URL)
	_ = s.store.SetSetting(ctx, "llm.key", req.Key)
	_ = s.store.SetSetting(ctx, "llm.model", req.Model)
	s.llm.SetConfig(req.URL, req.Key, req.Model)

	s.getLLMConfig(w, r)
}
