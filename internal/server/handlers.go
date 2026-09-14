package server

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/startburst-backend/internal/push"
)

// handleHealth reports backend liveness and upstream OpenCode health.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ocHealthy := false
	ocErr := ""
	if err := s.openCode.Ping(ctx); err == nil {
		ocHealthy = true
	} else {
		ocErr = err.Error()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "ok",
		"upstream":      ocHealthy,
		"upstreamError": ocErr,
		"time":          time.Now().UTC(),
	})
}

// handleSystem reports version and server config summary. It leaks internal
// hostnames, so it requires either an APP token or a web session.
func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.tokenFromRequest(r); !ok && !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	version, _ := s.openCode.GetVersion(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":         "startburst-backend",
		"opencodeURL":     s.cfg.OpenCodeURL,
		"opencodeVersion": version,
		"db":              s.cfg.DBDriver,
	})
}

// handleWebSession handles web admin login (POST) and logout (DELETE).
// It is deliberately unauthenticated for POST so the wizard can set a password.
func (s *Server) handleWebSession(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Password string `json:"password"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		key := clientKey(r)
		if !s.loginLimit.allow(key) {
			writeErr(w, http.StatusTooManyRequests, "too many login attempts, try again later")
			return
		}
		ok, err := s.auth.VerifyPassword(r.Context(), req.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "auth error")
			return
		}
		if !ok {
			writeErr(w, http.StatusUnauthorized, "invalid password")
			return
		}
		s.loginLimit.clear(key)
		sid, err := s.registerWebSession(r)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "create session failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"session": sid})
	case http.MethodDelete:
		if !s.requireWeb(r) {
			writeErr(w, http.StatusUnauthorized, "not authorized")
			return
		}
		sid := r.Header.Get("X-Web-Session")
		_ = s.store.DeleteWebSession(r.Context(), sid)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleWebPassword changes the admin password (requires web session).
func (s *Server) handleWebPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "not authorized")
		return
	}
	var req struct {
		NewPassword string `json:"newPassword"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.auth.SetPassword(r.Context(), req.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleTokens lists (GET) and creates (POST) tokens. Requires a web session
// (admin) or an APP token.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.auth.ListTokens(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list tokens failed")
			return
		}
		writeJSON(w, http.StatusOK, tokens)
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeErr(w, http.StatusBadRequest, "name is required")
			return
		}
		raw, err := s.auth.CreateToken(r.Context(), req.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "create token failed")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"token": raw})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleTokenByID revokes a token (DELETE). Requires a web session (admin) or
// an APP token.
func (s *Server) handleTokenByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := r.URL.Path[len("/api/tokens/"):]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing token id")
		return
	}
	if err := s.auth.RevokeToken(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleWebSocket upgrades a token-authenticated connection and registers it
// with the push hub for real-time notifications.
// The token may be supplied via the Authorization header (Bearer) or the
// ?token= query parameter, since browsers cannot set WS headers.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.tokenFromRequest(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}
	hc := s.hub.Register(conn)
	defer s.hub.Unregister(hc)

	// Notify the client it is subscribed.
	_ = hc.Write(push.Message{Type: "subscribed"})

	// Read loop: discard inbound frames (keep-alive pings); on error exit.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
