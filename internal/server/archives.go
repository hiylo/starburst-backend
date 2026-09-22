package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/store"
)

// sessionIDRe is the allow-list for session identifiers that are later
// interpolated into upstream URLs (/session/{id}/message). Restricting to
// alphanumerics plus dash/underscore prevents path-confusion injection.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// isValidSessionID reports whether sessionID is safe to embed in an upstream
// URL path segment.
func isValidSessionID(id string) bool {
	return sessionIDRe.MatchString(id)
}

// handleArchives lists archive metadata (GET) or archives a session (POST).
// Requires a web session or APP token.
func (s *Server) handleArchives(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
				limit = n
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		items, err := s.store.ListArchives(ctx, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list archives failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"archives": items})
	case http.MethodPost:
		s.archiveSession(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// archiveSession pulls a session's messages from OpenCode and stores them.
// Body: {"sessionId":"...", "format":"markdown"|"json"}
func (s *Server) archiveSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
		Format    string `json:"format"`
	}
	if !readBody(w, r, &req) {
		return
	}
	if req.SessionID == "" {
		writeErr(w, http.StatusBadRequest, "sessionId is required")
		return
	}
	// sessionId 会拼进上游 /session/{id}/message 路径，必须先做白名单校验，
	// 防止含 /、? 等字符的输入把请求打到 upstream 其它端点（路径混淆）。
	if !isValidSessionID(req.SessionID) {
		writeErr(w, http.StatusBadRequest, "invalid sessionId")
		return
	}
	if req.Format == "" {
		req.Format = "markdown"
	}
	if req.Format != "markdown" && req.Format != "json" {
		writeErr(w, http.StatusBadRequest, "format must be markdown or json")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	msgs, err := s.openCode.FetchSessionMessages(ctx, req.SessionID)
	if err != nil {
		// 原始错误可能含内网上游地址（Get "http://127.0.0.1:4096/..."），
		// 只进日志，回传泛化文案。
		log.Printf("archive session %s: %v", req.SessionID, err)
		writeErr(w, http.StatusBadGateway, "opencode request failed")
		return
	}

	var content string
	if req.Format == "json" {
		content, _ = marshalMessagesJSON(msgs)
	} else {
		content = opencode.ExportMarkdown(req.SessionID, msgs)
	}

	arch := &store.Archive{
		ID:        newArchiveID(),
		SessionID: req.SessionID,
		Title:     "Session " + req.SessionID,
		Format:    req.Format,
		Content:   content,
		Size:      len(content),
	}
	if err := s.store.CreateArchive(ctx, arch); err != nil {
		writeErr(w, http.StatusInternalServerError, "create archive failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": arch.ID, "size": arch.Size, "format": arch.Format,
	})
}

// handleArchiveByID gets (GET) or deletes (DELETE) a single archive.
func (s *Server) handleArchiveByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	id := r.URL.Path[len("/api/archives/"):]
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing archive id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		a, err := s.store.GetArchive(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "archive not found")
			return
		}
		writeJSON(w, http.StatusOK, a)
	case http.MethodDelete:
		if err := s.store.DeleteArchive(r.Context(), id); err != nil {
			writeErr(w, http.StatusNotFound, "archive not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
func marshalMessagesJSON(msgs []opencode.Message) (string, error) {

	type m struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	out := make([]m, 0, len(msgs))
	for _, x := range msgs {
		out = append(out, m{Role: x.Role, Content: x.Content})
	}
	b, err := json.MarshalIndent(out, "", "  ")
	return string(b), err
}

func newArchiveID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("arch_%s", hex.EncodeToString(buf))
}
