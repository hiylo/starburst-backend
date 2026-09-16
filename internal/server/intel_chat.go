package server

import (
	"context"
	"net/http"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelChats lists (GET) a project's conversations. The projectId is
// passed as a query parameter, matching the other intel list endpoints.
// Requires a web session or APP token.
func (s *Server) handleIntelChats(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chats, err := s.store.ListIntelChats(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load chats failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

// handleIntelChatByID returns a conversation's messages (GET) or deletes the
// conversation (DELETE).
func (s *Server) handleIntelChatByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	id, ok := intelPathID(w, r, "/api/intel/chats/")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getIntelChat(w, r, id)
	case http.MethodDelete:
		s.deleteIntelChat(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) getIntelChat(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chat, err := s.store.GetIntelChat(ctx, id)
	if err != nil {
		if err == store.ErrNotFound {
			writeErr(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "load chat failed")
		return
	}
	messages, err := s.store.ListIntelChatMessages(ctx, id, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load messages failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": chat, "messages": messages})
}

func (s *Server) deleteIntelChat(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.store.DeleteIntelChat(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete chat failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
