package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/startburst-backend/internal/store"
)

// secureCompare compares two strings in constant time.
func secureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// handleRules implements rules CRUD. GET/POST require a web session (admin)
// or an APP token.
func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		s.listRules(w, r)
	case http.MethodPost:
		s.createRule(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rules, err := s.store.ListRules(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list rules failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Schedule  string `json:"schedule"`
		Directory string `json:"directory"`
		Prompt    string `json:"prompt"`
		Enabled   bool   `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Kind == "" || req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "kind and prompt are required")
		return
	}
	switch req.Kind {
	case store.TriggerCron, store.TriggerGit, store.TriggerHTTP:
	default:
		writeErr(w, http.StatusBadRequest, "invalid kind")
		return
	}
	rule := &store.Rule{
		ID:        newRuleID(),
		Name:      req.Name,
		Kind:      req.Kind,
		Schedule:  req.Schedule,
		Directory: req.Directory,
		Prompt:    req.Prompt,
		Enabled:   req.Enabled,
	}
	if err := s.store.CreateRule(r.Context(), rule); err != nil {
		writeErr(w, http.StatusInternalServerError, "create rule failed")
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

// handleRuleByID deletes (DELETE) a rule by id, or lists its executions
// (GET /api/rules/{id}/executions).
func (s *Server) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	rest := r.URL.Path[len("/api/rules/"):]
	if rest == "" {
		writeErr(w, http.StatusBadRequest, "missing rule id")
		return
	}
	// Split "id" vs "id/executions".
	if strings.HasSuffix(rest, "/executions") {
		id := strings.TrimSuffix(rest, "/executions")
		s.handleRuleExecutions(w, r, id)
		return
	}
	id := rest
	switch r.Method {
	case http.MethodDelete:
		if err := s.store.DeleteRule(r.Context(), id); err != nil {
			writeErr(w, http.StatusNotFound, "rule not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleRuleExecutions returns the recent execution history of a rule.
func (s *Server) handleRuleExecutions(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	execs, err := s.store.ListRuleExecutions(ctx, id, 50)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list executions failed")
		return
	}
	count, err := s.store.CountRuleExecutions(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "count executions failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"executions": execs,
		"total":      count,
	})
}

// handleRuleWebhook fires a matching http-kind rule. It accepts an optional
// ?target= path filter. If a webhook secret is configured, the request must
// carry it in X-Webhook-Secret (or ?secret=).
func (s *Server) handleRuleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg.WebhookSecret != "" {
		provided := r.Header.Get("X-Webhook-Secret")
		if provided == "" {
			provided = r.URL.Query().Get("secret")
		}
		if !secureCompare(provided, s.cfg.WebhookSecret) {
			writeErr(w, http.StatusUnauthorized, "invalid webhook secret")
			return
		}
	}
	target := r.URL.Query().Get("target")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	fired, err := s.automation.FireKind(ctx, store.TriggerHTTP, target)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "fire rule failed")
		return
	}
	if !fired {
		writeErr(w, http.StatusNotFound, "no matching rule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"fired": true})
}

func newRuleID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return "rule_" + hex.EncodeToString(buf)
}
