package push

import (
	"encoding/json"
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

// outboxSize is the per-connection buffered send queue. A slow client that
// fills its queue gets its messages dropped (real-time status/events resync via
// the client's polling) instead of ever blocking the hub's fan-out.
const outboxSize = 64

// hubConn owns one websocket connection plus a buffered outbox drained by a
// single writer goroutine, so a wedged reader cannot stall the whole hub.
type hubConn struct {
	c         *websocket.Conn
	send      chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

// writeOne writes a single message with a bounded deadline.
func (hc *hubConn) writeOne(data []byte) error {
	_ = hc.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return hc.c.WriteMessage(websocket.TextMessage, data)
}

// writer drains the outbox until done or a write error, then self-unregisters.
// A non-blocking done check runs before every receive: once done is closed the
// writer exits without consuming yet another buffered message, so a stopped
// connection never performs an extra write after Unregister.
func (hc *hubConn) writer(h *Hub) {
	for {
		select {
		case <-hc.done:
			return
		default:
		}
		select {
		case <-hc.done:
			return
		case data := <-hc.send:
			if err := hc.writeOne(data); err != nil {
				h.Unregister(hc)
				return
			}
		}
	}
}

// Write marshals and enqueues one message to the connection's outbox. It never
// blocks on the client: a full outbox simply drops the message (real-time
// status/events resync via the client's polling).
func (hc *hubConn) Write(msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	hc.enqueue(data)
}

// enqueue sends already-marshaled bytes to the outbox, dropping on overflow.
func (hc *hubConn) enqueue(data []byte) {
	select {
	case hc.send <- data:
	default:
		// Slow consumer: drop rather than stall the fan-out.
	}
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

// Register wraps a connection, starts its writer goroutine, and adds it to the
// fan-out set. Returns the wrapper used for all further writes.
func (h *Hub) Register(c *websocket.Conn) *hubConn {
	hc := &hubConn{
		c:    c,
		send: make(chan []byte, outboxSize),
		done: make(chan struct{}),
	}
	h.mu.Lock()
	h.clients[hc] = struct{}{}
	h.mu.Unlock()
	go hc.writer(h)
	return hc
}

// Unregister removes a connection, stops its writer and closes it. It is safe
// to call from any goroutine, including after Run has returned.
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
	hc.closeOnce.Do(func() {
		close(hc.done)
		_ = hc.c.Close()
	})
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
		c.closeOnce.Do(func() {
			close(c.done)
			_ = c.c.Close()
		})
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

// Broadcast sends a message to all connected clients. It marshals once, then
// non-blockingly enqueues to every client's outbox: a slow client can at most
// drop messages, never stall the caller (e.g. the upstream event collector).
func (h *Hub) Broadcast(msg Message) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.Lock()
	conns := make([]*hubConn, 0, len(h.clients))
	for c := range h.clients {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, hc := range conns {
		hc.enqueue(data)
	}
}
