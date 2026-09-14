package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/startburst-backend/internal/opencode"
	"github.com/hiylo/startburst-backend/internal/store"
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
		writeErr(w, http.StatusBadGateway, "opencode: "+err.Error())
		return
	}

	var content string
	if req.Format == "json" {
		content, _ = marshalMessagesJSON(msgs)
	} else {
		content = opencode.ExportMarkdown(req.SessionID, msgs)
	}

	// 同时保存完整结构化消息（含 parts），用于把归档原样恢复成新会话。
	// 拉取失败不影响归档完成（只是该归档暂不支持恢复）。
	var raw string
	if rawJSON, err := s.openCode.FetchSessionMessagesRaw(ctx, req.SessionID); err == nil && len(rawJSON) > 0 {
		raw = string(rawJSON)
	}

	arch := &store.Archive{
		ID:          newArchiveID(),
		SessionID:   req.SessionID,
		Title:       "Session " + req.SessionID,
		Format:      req.Format,
		Content:     content,
		RawMessages: raw,
		Size:        len(content),
	}
	if err := s.store.CreateArchive(ctx, arch); err != nil {
		writeErr(w, http.StatusInternalServerError, "create archive failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": arch.ID, "size": arch.Size, "format": arch.Format,
	})
}

// handleArchiveByID gets (GET) or deletes (DELETE) a single archive, and
// restores it to a new OpenCode session via POST /api/archives/{id}/restore.
func (s *Server) handleArchiveByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	rest := r.URL.Path[len("/api/archives/"):]
	restore := false
	id := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		id = rest[:i]
		restore = rest[i+1:] == "restore"
	}
	if id == "" {
		writeErr(w, http.StatusBadRequest, "missing archive id")
		return
	}
	switch {
	case restore:
		s.restoreArchive(w, r, id)
	case r.Method == http.MethodGet:
		a, err := s.store.GetArchive(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "archive not found")
			return
		}
		writeJSON(w, http.StatusOK, a)
	case r.Method == http.MethodDelete:
		if err := s.store.DeleteArchive(r.Context(), id); err != nil {
			writeErr(w, http.StatusNotFound, "archive not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// restoreArchive replays an archived session's raw structured messages into a
// freshly created OpenCode session, reproducing the original conversation
// history so work can continue. Archives created before raw message capture
// (raw_messages empty) cannot be restored and get a 422. Returns the new
// session id.
func (s *Server) restoreArchive(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a, err := s.store.GetArchive(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "archive not found")
		return
	}
	if strings.TrimSpace(a.RawMessages) == "" {
		writeErr(w, http.StatusUnprocessableEntity, "this archive has no raw messages and cannot be restored")
		return
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal([]byte(a.RawMessages), &msgs); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "archive raw messages are corrupted")
		return
	}
	if len(msgs) == 0 {
		writeErr(w, http.StatusUnprocessableEntity, "archive has no messages to restore")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// 新建会话，目录沿用归档来源目录（取不到则用工作区根）。
	dir := "/workspaces"
	created, err := s.openCode.CreateSession(ctx, a.Title, "", dir)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "create session failed: "+err.Error())
		return
	}
	var createdInfo struct {
		ID string `json:"id"`
	}
	if b, err := json.Marshal(created); err == nil {
		_ = json.Unmarshal(b, &createdInfo)
	}
	newID := createdInfo.ID
	if newID == "" {
		writeErr(w, http.StatusBadGateway, "create session returned no id")
		return
	}

	// 逐条注入历史消息。
	for i, rawMsg := range msgs {
		if err := s.openCode.InjectMessage(ctx, newID, rawMsg); err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Sprintf("restore message %d/%d failed: %v", i+1, len(msgs), err))
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id": newID, "title": a.Title, "restored": len(msgs),
	})
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
