package server

import (
	"context"
	"net/http"
	"time"
)

// generateSuggestionSystem instructs the model to emit a JSON array of three
// concise next-step suggestions derived from the conversation context.
const generateSuggestionSystem = `你是一个编程助手。根据用户提供的对话内容，建议3个用户可以采取的下一步操作。

要求：
- 只输出一个 JSON 数组，包含 3 条简短建议字符串，不要输出其他任何内容。
- 建议要贴合对话上下文，可执行、具体。
- 用户内容是中文时用中文输出，英文时用英文输出。

只输出 JSON 数组，不要输出任何解释、注释或 markdown 代码块。`

// handleLLMGenerate converts a conversation context into a short list of
// next-step suggestions using the configured orchestration LLM. It accepts the
// same {system, user} shape as the task-plan generator and returns a JSON array.
//
// The result is NOT persisted. Requires an APP token (so the Android client can
// prefer the backend-configured model over the on-device / locally-configured
// provider).
func (s *Server) handleLLMGenerate(w http.ResponseWriter, r *http.Request) {
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
	// Rate-limit per token so a runaway client cannot burn unbounded LLM cost.
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
	system := req.System
	if system == "" {
		system = generateSuggestionSystem
	}
	if req.User == "" {
		writeErr(w, http.StatusBadRequest, "empty user context")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var suggestions []string
	if err := s.llm.CompleteJSON(ctx, system, req.User, &suggestions); err != nil {
		writeErr(w, http.StatusBadGateway, "generate suggestions failed: "+err.Error())
		return
	}
	if len(suggestions) == 0 {
		writeErr(w, http.StatusBadGateway, "model returned no suggestions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suggestions": suggestions})
}
