package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/hiylo/starburst-backend/internal/config"
	"github.com/hiylo/starburst-backend/internal/push"
)

// WebSocket keep-alive timing: we ping every wsPingInterval and drop the
// connection if no pong arrives within wsPongWait. Writes must complete within
// wsWriteWait. These mirror the gorilla chat example's recommended values.
const (
	wsWriteWait    = 10 * time.Second
	wsPongWait     = 60 * time.Second
	wsPingInterval = (wsPongWait * 9) / 10
)

// readBody 读取 JSON 请求体（限 4MiB），超限返回 413，解析失败返回 400。
func readBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := readJSONLimited(w, r, v); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid request body")
		}
		return false
	}
	return true
}

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
		// 无鉴权端点：不回传 Ping 的原始错误（形如 `Get "http://127.0.0.1:4096/…"`
		// 会泄露内网上游地址），只给泛化描述。
		ocErr = "upstream check failed"
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
	// vectorCapable: 智能测试（含 RAG 向量检索）完整可用需要
	// PostgreSQL(pgvector 扩展) + embedding 均已就绪。SQLite 轻量化部署不
	// 满足即隐藏智能测试入口，而非给出残缺功能。
	pgVec, pgErr := s.store.PGVectorInstalled(ctx)
	vectorCapable := pgVec && s.embedding != nil && s.embedding.Enabled()
	if pgErr != nil {
		log.Printf("pgvector probe: %v", pgErr)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"backend":         "starburst-backend",
		"version":         config.Version,
		"opencodeURL":     s.cfg.OpenCodeURL,
		"opencodeVersion": version,
		"db":              s.cfg.DBDriver,
		"pgvector":        pgVec,
		"vectorCapable":   vectorCapable,
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
		if !readBody(w, r, &req) {
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
// Changing it also revokes every other web session: a hijacked cookie must not
// survive the credential rotation meant to lock the hijacker out. The caller's
// own session is kept so the admin isn't logged out by their own action.
func (s *Server) handleWebPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	sid := r.Header.Get("X-Web-Session")
	if sid == "" || !s.requireWeb(r) {
		writeErr(w, http.StatusUnauthorized, "not authorized")
		return
	}
	var req struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !readBody(w, r, &req) {
		return
	}
	// 仅凭会话即可换密码 = 拿到会话就能夺取账号（会话可能来自 XSS、共用电脑或
	// 日志转储），所以旧密码是必填项。
	ok, err := s.auth.VerifyPassword(r.Context(), req.CurrentPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "auth error")
		return
	}
	if !ok {
		writeErr(w, http.StatusUnauthorized, "current password is incorrect")
		return
	}
	if err := s.auth.SetPassword(r.Context(), req.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if n, err := s.store.RevokeWebSessionsExcept(r.Context(), sid); err != nil {
		// 密码已经改成功，这里不回滚也不报失败，但要留下告警：其他会话仍有效。
		log.Printf("change password: revoke other web sessions failed: %v", err)
	} else if n > 0 {
		log.Printf("change password: revoked %d other web session(s)", n)
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
		if !readBody(w, r, &req) {
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

// handleWebSocket upgrades a token- or web-session-authenticated connection and
// registers it with the push hub for real-time notifications.
// The token may be supplied via the Authorization header (Bearer) or the
// ?token= query parameter, since browsers cannot set WS headers. A web session
// is accepted via the X-Web-Session header or the ?session= query parameter
// (the browser can only pass query params to a WebSocket URL).
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.tokenFromRequest(r); !ok && !s.requireWeb(r) && r.URL.Query().Get("session") == "" {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}
	if r.URL.Query().Get("session") != "" {
		if _, err := s.store.GetWebSession(r.Context(), r.URL.Query().Get("session")); err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid session")
			return
		}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade failed: %v", err)
		return
	}
	hc := s.hub.Register(conn)
	defer s.hub.Unregister(hc)

	// 半开连接防护：客户端必须周期回 pong，否则连接会被判定失联并回收，
	// 避免只发 FIN 而不关闭 TCP 的坏客户端永久占用连接与 hub 槽位。
	conn.SetReadDeadline(time.Now().Add(wsPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsPongWait))
	})
	conn.SetReadLimit(1024) // 入站仅 keep-alive，任何大数据帧都视为异常

	// 周期性 ping 探活，同时重置写 deadline。
	pingTicker := time.NewTicker(wsPingInterval)
	defer pingTicker.Stop()
	pingDone := make(chan struct{})
	go func() {
		for {
			select {
			case <-pingDone:
				return
			case <-pingTicker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteWait)); err != nil {
					// 写失败说明连接已死；主动关闭读循环。
					_ = conn.Close()
					return
				}
			}
		}
	}()
	defer close(pingDone)

	// Notify the client it is subscribed.
	hc.Write(push.Message{Type: "subscribed"})

	// Read loop: discard inbound frames (keep-alive pings); on error exit.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}
