package push

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestConn dials a loopback WS server backed by the hub.
func newTestConn(t *testing.T, hub *Hub) *websocket.Conn {
	t.Helper()
	server := newHubServer(t, hub)

	dialer := websocket.Dialer{}
	conn, _, err := dialer.Dial("ws://"+server+"/ws", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestHubBroadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	conn := newTestConn(t, hub)

	// Consume confirmation: upon registration the server sends "subscribed".
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read subscribed: %v", err)
	}
	var first Message
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if first.Type != "subscribed" {
		t.Fatalf("expected subscribed, got %s", first.Type)
	}

	hub.Broadcast(Message{Type: "test.event"})
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, data, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read broadcast: %v", err)
	}
	var got Message
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "test.event" {
		t.Fatalf("got %s want test.event", got.Type)
	}
}

func TestHubBroadcastNoClients(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	// Broadcasting with zero clients must not panic.
	hub.Broadcast(Message{Type: "noone"})
}

func TestHubDropsBrokenClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	conn := newTestConn(t, hub)

	// Eat the "subscribed" confirmation.
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read subscribed: %v", err)
	}
	if hub.Count() != 1 {
		t.Fatalf("count = %d, want 1", hub.Count())
	}

	// A dead peer fails the write, so Broadcast must drop it instead of
	// keeping it in the fan-out set.
	conn.Close()
	hub.Broadcast(Message{Type: "after-close"})
	if hub.Count() != 0 {
		t.Fatalf("count = %d after dead peer, want 0", hub.Count())
	}
}

func TestHubConcurrentBroadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	conn := newTestConn(t, hub)
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read subscribed: %v", err)
	}

	// Fan out from many goroutines at once; must not race or deadlock.
	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			hub.Broadcast(Message{Type: "conc"})
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if hub.Count() != 1 {
		t.Fatalf("count = %d, want 1", hub.Count())
	}
}

func TestHubStopClosesConnections(t *testing.T) {
	hub := NewHub()
	done := make(chan struct{})
	go func() {
		hub.Run()
		close(done)
	}()
	conn := newTestConn(t, hub)
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read subscribed: %v", err)
	}

	hub.Stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Stop")
	}
	if hub.Count() != 0 {
		t.Fatalf("count = %d after Stop, want 0", hub.Count())
	}
	// Stop is idempotent.
	hub.Stop()
}
