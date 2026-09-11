package push

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Message severity levels for client notification routing.
const (
	Info     = "info"
	Warning  = "warning"
	Critical = "critical"
)

// writeTimeout bounds a single fan-out write so one wedged client cannot stall
// the whole broadcast.
const writeTimeout = 5 * time.Second

// Message is a push event sent from the server to connected clients.
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Severity routes the event to client notification channels
	// (info = silent, warning/critical = notify).
	Severity string `json:"severity,omitempty"`
}

// Hub fans out push messages to all connected WebSocket clients.
// It is safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	clients map[*hubConn]struct{}
	done    chan struct{}
}

// hubConn serializes writes to one connection. gorilla/websocket permits a
// single concurrent writer, and both the hub fan-out and the per-connection
// "subscribed" greeting write to it, so every write goes through write.
type hubConn struct {
	mu sync.Mutex
	c  *websocket.Conn
}

// Write marshals and sends one message, bounded by writeTimeout.
func (h *hubConn) Write(msg Message) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_ = h.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return h.c.WriteMessage(websocket.TextMessage, data)
}

// SeverityFor maps a task status to a push severity for notification routing.
// blocked is a warning because it always needs human attention: the task will
// never run on its own and someone has to retry the upstream or unblock it.
func SeverityFor(status string) string {
	switch status {
	case "failed":
		return Critical
	case "retrying", "blocked":
		return Warning
	default:
		return Info
	}
}

// NewHub creates an empty hub.
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*hubConn]struct{}),
		done:    make(chan struct{}),
	}
}

// Register wraps a connection so its writes are serialized, adds it to the
// fan-out set, and returns the wrapper used for all further writes.
func (h *Hub) Register(c *websocket.Conn) *hubConn {
	hc := &hubConn{c: c}
	h.mu.Lock()
	h.clients[hc] = struct{}{}
	h.mu.Unlock()
	return hc
}

// Unregister removes a connection and closes it. It is safe to call from any
// goroutine, including after Run has returned.
func (h *Hub) Unregister(hc *hubConn) {
	if hc == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.clients[hc]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.clients, hc)
	h.mu.Unlock()
	_ = hc.c.Close()
}

// Count returns how many clients are currently subscribed.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Run blocks until Stop is called, then closes every remaining connection so
// shutdown is deterministic. Call it once in a goroutine.
func (h *Hub) Run() {
	<-h.done
	h.mu.Lock()
	conns := make([]*hubConn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.clients = make(map[*hubConn]struct{})
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.c.Close()
	}
}

// Stop terminates Run and closes all connections. It is idempotent.
func (h *Hub) Stop() {
	select {
	case <-h.done:
	default:
		close(h.done)
	}
}

// Broadcast sends a message to all connected clients. The connection list is
// snapshotted before writing so slow clients cannot hold the hub lock, and
// each write is bounded by writeTimeout.
func (h *Hub) Broadcast(msg Message) {
	h.mu.Lock()
	conns := make([]*hubConn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.Unlock()

	for _, hc := range conns {
		if err := hc.Write(msg); err != nil {
			log.Printf("push: write to client: %v", err)
			h.Unregister(hc)
		}
	}
}
