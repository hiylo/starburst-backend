package push

import (
	"net"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"
)

// newHubServer runs a WS endpoint backed by a hub and returns its address.
func newHubServer(t *testing.T, hub *Hub) string {
	t.Helper()
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		hc := hub.Register(conn)
		defer hub.Unregister(hc)
		hc.Write(Message{Type: "subscribed"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })

	return ln.Addr().String()
}
