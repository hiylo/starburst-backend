package server

import (
	"context"
	"net/http"
	"time"
)

// handleLLMComplete runs a free-text system+user prompt through the configured
// orchestration LLM and returns the raw assistant text. Unlike /api/llm/generate
// (which forces a JSON array of suggestions), this returns arbitrary prose, e.g.
// an AGENTS.md draft. The result is NOT persisted. Requires an APP token.
func (s *Server) handleLLMComplete(w http.ResponseWriter, r *http.Request) {
	tok, ok := s.requireToken(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "orchestration LLM is not configured")
		return
	}
	if !s.genLimit.allow(tok.ID) {
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	var req struct {
		System string `json:"system"`
		User   string `json:"user"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.User == "" {
		writeErr(w, http.StatusBadRequest, "empty user context")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	text, err := s.llm.Complete(ctx, req.System, req.User)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "complete failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}
