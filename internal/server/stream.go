package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/hiylo/starburst-backend/internal/opencode"
)

// handleStream relays the upstream OpenCode global SSE event stream to the
// client verbatim. It exists so the APP talks to one stable connection (this
// backend) instead of reaching across networks to OpenCode directly — the
// backend keeps the long-lived upstream link, auto-reconnects on failure, and
// forwards every event unchanged. Heartbeats from upstream are passed through.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// EventSource cannot set custom headers, so the web UI passes its admin
	// session via ?session= when no APP token is configured. Accept either a
	// Bearer/query token or a web session, mirroring the other web endpoints.
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

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	// Keep a sub-context so we can cancel the upstream on client disconnect.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Signal client the stream is ready.
	if _, err := fmt.Fprint(w, "event: connected\ndata: {}\n\n"); err != nil {
		return
	}
	fl.Flush()

	// Retry loop: re-open upstream SSE with backoff if it drops.
	backoff := time.Second
	for {
		err := s.openCode.StreamEvents(ctx, func(ev opencode.SSEEvent) error {
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", ev.Data); werr != nil {
				return werr // client gone; stop streaming
			}
			fl.Flush()
			return nil
		})
		if err != nil {
			// Distinguish client-side vs upstream-side failure.
			if ctx.Err() != nil {
				return // client disconnected
			}
			log.Printf("stream: upstream event stream dropped: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// Escalate backoff but keep it bounded.
		if backoff < 30*time.Second {
			backoff *= 2
		} else {
			backoff = time.Second // reset so we probe frequently after long gaps
		}
	}
}
