package server

import "net/http"

// handleIndex serves the embedded config page and its static assets
// (/, /assets/*, etc.) when web assets are built, otherwise a plain text hint
// that the backend is running. Authentication is done by the web UI itself via
// /api/web/session; the page itself is public so the login screen can be reached.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.hasWebUI {
		s.serveWebUI(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("starburst-backend is running (headless).\nConfig UI is not compiled in; authenticate via POST /api/web/session.\n"))
}
