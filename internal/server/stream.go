package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hiylo/starburst-backend/internal/opencode"
)

// sseMaxStreams caps concurrent /api/stream connections so a pile-up of wedged
// clients cannot exhaust goroutines or upstream SSE links.
const sseMaxStreams = 64

// sseIdleTimeout bounds one SSE write; a client that stops reading will time
// out the write and free the goroutine + upstream connection.
const sseIdleTimeout = 30 * time.Second

// sseMaxWriteFailures bounds consecutive client-write failures before the
// handler gives up. A wedged client (still TCP-connected but never draining)
// does not cancel ctx, so without this the retry loop would keep re-opening
// upstream SSE links forever, churning connections for a client that can no
// longer consume events.
const sseMaxWriteFailures = 3

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
	// 并发上限：超限直接 429，避免慢客户端堆积。
	if atomic.AddInt32(&s.activeStreams, 1) > sseMaxStreams {
		atomic.AddInt32(&s.activeStreams, -1)
		writeErr(w, http.StatusTooManyRequests, "too many active event streams")
		return
	}
	defer atomic.AddInt32(&s.activeStreams, -1)
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

	// 每次写前设置写超时，慢客户端（不读但 TCP 未断）会在超时后被回收。
	rc := http.NewResponseController(w)

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering

	// Keep a sub-context so we can cancel the upstream on client disconnect.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Signal client the stream is ready.
	_ = rc.SetWriteDeadline(time.Now().Add(sseIdleTimeout))
	if _, err := fmt.Fprint(w, "event: connected\ndata: {}\n\n"); err != nil {
		return
	}
	fl.Flush()

	// Retry loop: re-open upstream SSE with backoff if it drops.
	//
	// lastWriteErr distinguishes a client-side write failure (wedged client, not
	// draining) from an upstream drop: onEvent returning the write error is the
	// only way it gets set. Such failures accumulate across reconnects — a
	// client that never acknowledges a write is dead weight, so past
	// sseMaxWriteFailures we break out instead of reconnecting upstream
	// forever. A successful write resets the counter so transient timeouts
	// recover on their own.
	var (
		lastWriteErr  error
		writeFailures int
	)
	backoff := time.Second
	for {
		err := s.openCode.StreamEvents(ctx, func(ev opencode.SSEEvent) error {
			_ = rc.SetWriteDeadline(time.Now().Add(sseIdleTimeout))
			// SSE 规范：数据内的换行必须拆成多个 data: 行，否则客户端会把换行后的
			// 内容当成新事件字段、遇空行提前终止事件。上游事件偶发携带 CRLF，逐行剥 \r。
			for _, line := range strings.Split(string(ev.Data), "\n") {
				if _, werr := fmt.Fprintf(w, "data: %s\n", strings.TrimSuffix(line, "\r")); werr != nil {
					lastWriteErr = werr
					return werr // client write failed; stop forwarding
				}
			}
			if _, werr := fmt.Fprint(w, "\n"); werr != nil {
				lastWriteErr = werr
				return werr
			}
			writeFailures = 0
			fl.Flush()
			return nil
		})
		if ctx.Err() != nil {
			return // client disconnected
		}
		if err != nil {
			if lastWriteErr != nil {
				lastWriteErr = nil
				writeFailures++
				if writeFailures >= sseMaxWriteFailures {
					log.Printf("stream: client not draining writes, giving up after %d consecutive failures", writeFailures)
					return
				}
				log.Printf("stream: client write failed (%d/%d): %v", writeFailures, sseMaxWriteFailures, err)
			} else {
				log.Printf("stream: upstream event stream dropped: %v", err)
			}
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
