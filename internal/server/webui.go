package server

import "net/http"

// webUIFSProvider abstracts the embedded config-page assets so the server
// package can declare the hook without depending on the webui package.
// Implementations are provided at wiring time in main.
type webUIFSProvider interface {
	// Open returns the file content for a path within the embedded assets.
	Open(name string) ([]byte, error)
	// Serve serves the file at p to w.
	Serve(w http.ResponseWriter, r *http.Request, p string)
}

// serveWebUI delegates to the configured provider.
func (s *Server) serveWebUI(w http.ResponseWriter, r *http.Request) {
	if s.webUIFS == nil {
		writeErr(w, http.StatusNotFound, "web UI not built")
		return
	}
	s.webUIFS.Serve(w, r, r.URL.Path)
}

// SetWebUI enables serving of the embedded config UI.
func (s *Server) SetWebUI(fs webUIFSProvider) {
	s.hasWebUI = true
	s.webUIFS = fs
}

var _ webUIFSProvider = (*nilWebUI)(nil)

// nilWebUI is a compile-time placeholder; real implementations live in the webui package.
type nilWebUI struct{}

func (*nilWebUI) Open(string) ([]byte, error) { return nil, http.ErrNotSupported }
func (*nilWebUI) Serve(w http.ResponseWriter, r *http.Request, p string) {
	writeErr(w, http.StatusNotFound, "web UI not built")
}
