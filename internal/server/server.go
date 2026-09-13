package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/opencode-backend/internal/auth"
	"github.com/hiylo/opencode-backend/internal/automation"
	"github.com/hiylo/opencode-backend/internal/config"
	"github.com/hiylo/opencode-backend/internal/llm"
	"github.com/hiylo/opencode-backend/internal/opencode"
	"github.com/hiylo/opencode-backend/internal/push"
	"github.com/hiylo/opencode-backend/internal/store"
)

// Server wires all backend components behind an HTTP/WS listener.
type Server struct {
	cfg        *config.Config
	store      store.Store
	auth       *auth.Manager
	openCode   *opencode.Client
	hub        *push.Hub
	automation *automation.Engine
	llm        *llm.Client
	stt        *sttEngine
	httpServer *http.Server
	hasWebUI   bool
	webUIFS    webUIFSProvider
	testMux    http.Handler // set only in tests
	loginLimit *loginLimiter
	genLimit   *loginLimiter
}

// New assembles the server with its dependencies.
func New(cfg *config.Config, st store.Store, am *auth.Manager, oc *opencode.Client, hub *push.Hub) *Server {
	return &Server{
		cfg:        cfg,
		store:      st,
		auth:       am,
		openCode:   oc,
		hub:        hub,
		loginLimit: newLoginLimiter(5, 5*time.Minute),
		genLimit:   newLoginLimiter(20, time.Minute),
	}
}

// SetAutomation wires the automation engine used by rule webhooks.
func (s *Server) SetAutomation(eng *automation.Engine) { s.automation = eng }

// SetLLM wires the optional orchestration LLM client. When nil the smart
// orchestration endpoints report they are unavailable.
func (s *Server) SetLLM(c *llm.Client) { s.llm = c }

// SetSTT wires the streaming recognition engine proxy. An empty baseURL
// disables /api/stt so clients can fall back to on-device recognition.
func (s *Server) SetSTT(baseURL string, timeout time.Duration) {
	if strings.TrimSpace(baseURL) == "" {
		s.stt = nil
		return
	}
	s.stt = newSTTEngine(baseURL, timeout)
}

// Routes registers all handlers on mux and starts background goroutines.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/system", s.handleSystem)
	mux.HandleFunc("/api/web/session", s.handleWebSession) // POST login / DELETE logout
	mux.HandleFunc("/api/web/password", s.handleWebPassword)
	mux.HandleFunc("/api/tokens", s.handleTokens)
	mux.HandleFunc("/api/tokens/", s.handleTokenByID)
	mux.HandleFunc("/api/ws", s.handleWebSocket)
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/projects/", s.handleProjectSessions)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/generate", s.handleTaskGenerate)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	mux.HandleFunc("/api/batch", s.handleBatch)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/", s.handleRuleByID)
	mux.HandleFunc("/api/rules/generate", s.handleRuleGenerate)
	mux.HandleFunc("/api/llm", s.handleLLMConfig)
	mux.HandleFunc("/api/llm/generate", s.handleLLMGenerate)
	mux.HandleFunc("/api/llm/complete", s.handleLLMComplete)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/archives", s.handleArchives)
	mux.HandleFunc("/api/archives/", s.handleArchiveByID)
	mux.HandleFunc("/api/stt", s.handleSTTStatus)
	mux.HandleFunc("/api/stt/refine", s.handleSTTRefine)
	mux.HandleFunc("/api/stt/sessions", s.handleSTTCreate)
	mux.HandleFunc("/api/stt/sessions/", s.handleSTTSession)
	mux.HandleFunc("/api/webhook", s.handleRuleWebhook)
	mux.HandleFunc("/", s.handleIndex)
}

// Start begins serving on the configured address.
func (s *Server) Start(ctx context.Context) error {
	s.httpServer = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.routesMux(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("opencode-backend listening on %s", s.cfg.ListenAddr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	}
}

func (s *Server) routesMux() http.Handler {
	mux := http.NewServeMux()
	s.Routes(mux)
	return s.logMiddleware(mux)
}

// ---- middlewares ----

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(ww, r)
		log.Printf("http %d %s %s (%s)", ww.status, r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))

		// Audit: only API calls authenticated by an APP token are recorded.
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			return
		}
		// STT traffic would flood the audit table: a 10s recording at 200ms
		// per chunk is 50 rows, and the app re-probes /api/stt on every chat
		// screen open, so the whole subtree is skipped entirely.
		if strings.HasPrefix(r.URL.Path, "/api/stt") {
			return
		}
		if rec, ok := s.tokenFromRequest(r); ok {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			_ = s.store.RecordAudit(ctx, &store.AuditEntry{
				TokenID:   rec.ID,
				TokenName: rec.Name,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    ww.status,
			})
		}
	})
}

// tokenFromRequest resolves the Bearer token (or ?token=) to its record.
func (s *Server) tokenFromRequest(r *http.Request) (*store.Token, bool) {
	if rec, ok := s.requireToken(r); ok {
		return rec, true
	}
	if q := r.URL.Query().Get("token"); q != "" {
		rec, err := s.auth.VerifyToken(r.Context(), q)
		if err != nil || rec == nil {
			return nil, false
		}
		_ = s.auth.TouchToken(r.Context(), rec.ID)
		return rec, true
	}
	return nil, false
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Hijack lets the WebSocket upgrader work through the logging wrapper.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
	}
	return h.Hijack()
}

// Flush lets SSE streaming work through the logging wrapper.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// registerWebSession issues a signed session id for the web UI after password
// login and persists it to the store so sessions survive restarts.
func (s *Server) registerWebSession(r *http.Request) (string, error) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	sid := hex.EncodeToString(buf)
	expires := time.Now().Add(24 * time.Hour)
	if err := s.store.CreateWebSession(r.Context(), sid, expires); err != nil {
		return "", err
	}
	return sid, nil
}

// requireWeb validates that sid is an active web session.
func (s *Server) requireWeb(r *http.Request) bool {
	sid := r.Header.Get("X-Web-Session")
	if sid == "" {
		return false
	}
	if _, err := s.store.GetWebSession(r.Context(), sid); err != nil {
		return false
	}
	return true
}

// requireToken validates the Authorization Bearer token against the store.
func (s *Server) requireToken(r *http.Request) (*store.Token, bool) {
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return nil, false
	}
	rec, err := s.auth.VerifyToken(r.Context(), token)
	if err != nil || rec == nil {
		return nil, false
	}
	_ = s.auth.TouchToken(r.Context(), rec.ID)
	return rec, true
}

// ---- response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
