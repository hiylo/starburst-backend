package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/auth"
	"github.com/hiylo/starburst-backend/internal/automation"
	"github.com/hiylo/starburst-backend/internal/config"
	"github.com/hiylo/starburst-backend/internal/embed"
	"github.com/hiylo/starburst-backend/internal/llm"
	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
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
	embedding  *embed.Client
	stt        *sttEngine
	httpServer *http.Server
	hasWebUI   bool
	webUIFS    webUIFSProvider
	testMux    http.Handler // set only in tests
	loginLimit *loginLimiter
	genLimit   *loginLimiter
	// touchMu 保护 touchSeen，实现 TouchToken 写库节流。
	touchMu   sync.Mutex
	touchSeen map[string]time.Time
	// sessionStatuses 是采集器从 session.status/idle 事件聚合的最新会话状态
	//（sessionId → "busy"|"idle"|"retry"|"error"），用于给 App 提供比上游
	// /session/status 快照更准确、更完整的状态视图。
	sessionStatuses sync.Map
	// sessionActivity 记录每个会话最近一次消息类事件的时间（用于把「状态已标
	// idle 但仍在流式输出」的会话正确识别为处理中，上游 status 事件本身并不可靠）。
	sessionActivity sync.Map
	// maxConcurrency 全局任务并发上限（0 = 仅受 worker 数限制），供状态页展示占用。
	maxConcurrency int
	// activeStreams counts live /api/stream SSE connections (capped to avoid a
	// pile-up of goroutines + upstream connections from wedged clients).
	activeStreams int32
	// auditCh buffers audit entries written off the request path and drained by
	// StartAuditFlusher, so polling traffic does not pay a synchronous INSERT.
	auditCh chan *store.AuditEntry
}

// SetMaxConcurrency records the global task concurrency cap for observability.
func (s *Server) SetMaxConcurrency(n int) { s.maxConcurrency = n }

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
		touchSeen:  make(map[string]time.Time),
		auditCh:    make(chan *store.AuditEntry, 512),
	}
}

// StartAuditFlusher drains the async audit queue and batches entries into
// multi-row INSERTs, keeping polling traffic off the request hot path. Call it
// once in a goroutine with the process lifecycle context.
func (s *Server) StartAuditFlusher(ctx context.Context) {
	const (
		flushTick   = 500 * time.Millisecond
		maxBatch    = 256
		flushWindow = 3 * time.Second
	)
	ticker := time.NewTicker(flushTick)
	defer ticker.Stop()
	batch := make([]*store.AuditEntry, 0, maxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), flushWindow)
		if err := s.store.RecordAudits(fctx, batch); err != nil {
			log.Printf("audit: batch insert %d rows: %v", len(batch), err)
		}
		cancel()
		batch = batch[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case e := <-s.auditCh:
			batch = append(batch, e)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// SetAutomation wires the automation engine used by rule webhooks.
func (s *Server) SetAutomation(eng *automation.Engine) { s.automation = eng }

// SetLLM wires the optional orchestration LLM client. When nil the smart
// orchestration endpoints report they are unavailable.
func (s *Server) SetLLM(c *llm.Client) { s.llm = c }

// SetEmbedding wires the optional embeddings client used by the project
// knowledge base. When nil the knowledge-base endpoints report unavailable.
func (s *Server) SetEmbedding(c *embed.Client) { s.embedding = c }

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
	mux.HandleFunc("/api/tasks/action", s.handleTaskBatchAction)
	mux.HandleFunc("/api/tasks/stats", s.handleTaskStats)
	mux.HandleFunc("/api/workflow", s.handleWorkflowCreate)
	mux.HandleFunc("/api/workflows", s.handleWorkflowList)
	mux.HandleFunc("/api/workflow/", s.handleWorkflowSteps)
	mux.HandleFunc("/api/tasks/generate", s.handleTaskGenerate)
	mux.HandleFunc("/api/tasks/", s.handleTaskByID)
	mux.HandleFunc("/api/batch", s.handleBatch)
	mux.HandleFunc("/api/rules", s.handleRules)
	mux.HandleFunc("/api/rules/", s.handleRuleByID)
	mux.HandleFunc("/api/rules/generate", s.handleRuleGenerate)
	mux.HandleFunc("/api/llm", s.handleLLMConfig)
	mux.HandleFunc("/api/llm/generate", s.handleLLMGenerate)
	mux.HandleFunc("/api/llm/complete", s.handleLLMComplete)
	mux.HandleFunc("/api/embed", s.handleEmbedConfig)
	mux.HandleFunc("/api/audit", s.handleAudit)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/archives", s.handleArchives)
	mux.HandleFunc("/api/archives/", s.handleArchiveByID)
	// OpenCode 镜像代理：/api/opencode/<path> ↔ opencode /<path>（App 后端可用时走此通道）。
	mux.HandleFunc(OpenCodeProxyPrefix+"/", s.handleOpenCodeProxy)
	// 全局事件查询：App 看板拉取最近会话动态。
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/unread", s.handleUnreadList)
	mux.HandleFunc("/api/unread/", s.handleUnreadMarkRead)
	mux.HandleFunc("/api/stt", s.handleSTTStatus)
	mux.HandleFunc("/api/stt/refine", s.handleSTTRefine)
	mux.HandleFunc("/api/stt/sessions", s.handleSTTCreate)
	mux.HandleFunc("/api/stt/sessions/", s.handleSTTSession)
	mux.HandleFunc("/api/webhook", s.handleRuleWebhook)
	// Test Intelligence (intel) subsystem routes.
	mux.HandleFunc("/api/intel/projects", s.handleIntelProjects)
	mux.HandleFunc("/api/intel/projects/", s.handleIntelProjectByID)
	mux.HandleFunc("/api/intel/analyze", s.handleIntelAnalyze)
	mux.HandleFunc("/api/intel/index", s.handleIntelIndex)
	mux.HandleFunc("/api/intel/ask", s.handleIntelAsk)
	mux.HandleFunc("/api/intel/chats", s.handleIntelChats)
	mux.HandleFunc("/api/intel/chats/", s.handleIntelChatByID)
	mux.HandleFunc("/api/intel/endpoints", s.handleIntelEndpoints)
	mux.HandleFunc("/api/intel/entities", s.handleIntelEntities)
	mux.HandleFunc("/api/intel/modules", s.handleIntelModules)
	mux.HandleFunc("/api/intel/test-cases", s.handleIntelTestCases)
	mux.HandleFunc("/api/intel/features", s.handleIntelFeatures)
	mux.HandleFunc("/api/intel/issues", s.handleIntelIssues)
	mux.HandleFunc("/api/intel/run", s.handleIntelRun)
	mux.HandleFunc("/api/intel/runs", s.handleIntelRuns)
	mux.HandleFunc("/api/intel/runs/", s.handleIntelRunByID)
	mux.HandleFunc("/api/intel/findings", s.handleIntelFindings)
	mux.HandleFunc("/api/intel/findings/", s.handleIntelFindingWaive)
	mux.HandleFunc("/api/intel/fixes", s.handleIntelFixes)
	mux.HandleFunc("/api/intel/fixes/", s.handleIntelFixAction)
	mux.HandleFunc("/", s.handleIndex)
}

// Start begins serving on the configured address.
func (s *Server) Start(ctx context.Context) error {
	s.httpServer = &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s.routesMux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("starburst-backend listening on %s", s.cfg.ListenAddr)
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
			// 异步审计：仅入队，由 StartAuditFlusher 批量落库，避免每个
			// token 请求（含 App/Web 的轮询）都同步一次 INSERT。
			select {
			case s.auditCh <- &store.AuditEntry{
				TokenID:   rec.ID,
				TokenName: rec.Name,
				Method:    r.Method,
				Path:      r.URL.Path,
				Status:    ww.status,
			}:
			default:
				// 队列满则丢弃（审计是尽力而为的记账），不阻塞请求。
			}
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
		s.touchTokenThrottled(r.Context(), rec.ID)
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

// tokenCtxKey 是 request context 中缓存已验证 token 记录的键，避免同一
// 请求内（日志中间件 + handler）重复查库。
type tokenCtxKey struct{}

// touchInterval 是 TouchToken 写库的最小间隔：同一 token 距上次更新小于
// 该值时跳过 UPDATE，避免高并发请求下的写放大。
const touchInterval = 60 * time.Second

// requireToken validates the Authorization Bearer token against the store.
// 已通过校验的 token 记录会缓存在 request context 中，重复调用直接命中缓存。
func (s *Server) requireToken(r *http.Request) (*store.Token, bool) {
	if rec, ok := r.Context().Value(tokenCtxKey{}).(*store.Token); ok && rec != nil {
		return rec, true
	}
	authz := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(authz, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return nil, false
	}
	rec, err := s.auth.VerifyToken(r.Context(), token)
	if err != nil || rec == nil {
		return nil, false
	}
	s.touchTokenThrottled(r.Context(), rec.ID)
	*r = *r.WithContext(context.WithValue(r.Context(), tokenCtxKey{}, rec))
	return rec, true
}

// touchTokenThrottled 按 tokenID 节流调用 TouchToken，距上次写库小于
// touchInterval 时跳过。并发安全。
func (s *Server) touchTokenThrottled(ctx context.Context, id string) {
	s.touchMu.Lock()
	last, seen := s.touchSeen[id]
	if seen && time.Since(last) < touchInterval {
		s.touchMu.Unlock()
		return
	}
	s.touchSeen[id] = time.Now()
	s.touchMu.Unlock()
	_ = s.auth.TouchToken(ctx, id)
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

// maxJSONBody 是 JSON 请求体的最大字节数（4MiB）。
const maxJSONBody = 4 << 20

// errBodyTooLarge 表示请求体超过 maxJSONBody，调用方应返回 413。
var errBodyTooLarge = errors.New("request body too large")

// readJSONLimited 与 readJSON 类似，但用 http.MaxBytesReader 限制请求体
// 大小，超限时返回 errBodyTooLarge（对应 HTTP 413）。
func readJSONLimited(w http.ResponseWriter, r *http.Request, v any) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errBodyTooLarge
		}
		return err
	}
	return nil
}
