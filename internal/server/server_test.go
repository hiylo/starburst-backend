// Shared HTTP test harness for the server package.
//
// The server endpoint tests used to live in this single file; they are now split
// per subdomain across server_auth_test.go, server_tasks_test.go,
// server_events_test.go, server_rules_test.go and server_data_test.go.
// Everything still declared here (the fake-upstream server and the request
// helper) is used by two or more of those files, so it stays in one place.
package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/auth"
	"github.com/hiylo/starburst-backend/internal/automation"
	"github.com/hiylo/starburst-backend/internal/config"
	"github.com/hiylo/starburst-backend/internal/opencode"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	am := auth.NewManager(st)
	if _, err := am.Initialize(ctx, "S3cureAdmin!"); err != nil {
		t.Fatalf("init auth: %v", err)
	}

	// Fake upstream OpenCode that reports healthy + one busy session.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true,"version":"v9.9.9"}`))
		case "/session":
			_, _ = w.Write([]byte(`[{"id":"ses_a","slug":"alpha","title":"Alpha","directory":"/w","agent":"build","model":{"id":"m1"},"cost":0,"tokens":{"input":1,"output":1,"reasoning":1},"time":{"created":1000,"updated":2000}}]`))
		case "/project":
			// 无真实 project 元数据时返回空数组，handleProjects 应回退到
			// 按会话目录分组（测试断言依赖该回退路径）。
			_, _ = w.Write([]byte(`[]`))
		case "/experimental/session":
			// 带真实目录的会话列表（/session 会把目录归一成根目录，这里是
			// handleProjects/handleProjectSessions 的真实数据源）。
			dir := r.URL.Query().Get("directory")
			all := []string{
				`{"id":"ses_a","title":"Alpha","directory":"/w","path":"w","agent":"build"}`,
				`{"id":"ses_b","title":"Beta","directory":"/other","path":"other","agent":"build"}`,
			}
			if dir != "" {
				filtered := make([]string, 0)
				for _, s := range all {
					if strings.Contains(s, `"directory":"`+dir+`"`) {
						filtered = append(filtered, s)
					}
				}
				_, _ = w.Write([]byte("[" + strings.Join(filtered, ",") + "]"))
			} else {
				_, _ = w.Write([]byte("[" + strings.Join(all, ",") + "]"))
			}
		case "/session/status":
			_, _ = w.Write([]byte(`{"ses_a":{"type":"busy"}}`))
		case "/config":
			_, _ = w.Write([]byte(`{"version":"v9.9.9"}`))
		case "/session/ses_test123/message":
			_, _ = w.Write([]byte(`[{"info":{"role":"assistant"},"parts":[{"type":"text","text":"这是结果"}]}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := &config.Config{
		ListenAddr:  "127.0.0.1:0",
		OpenCodeURL: upstream.URL,
		DBDriver:    "sqlite",
	}
	oc := opencode.New(upstream.URL)
	hub := push.NewHub()
	go hub.Run()

	srv := New(cfg, st, am, oc, hub)
	srv.SetAutomation(automation.NewEngine(st, time.Hour))
	srv.testMux = srv.routesMux()
	// 生产由 main.go 启动审计批量 flusher；测试服务器也要启动，否则
	// 异步审计的行永远不会落库。
	aCtx, aCancel := context.WithCancel(context.Background())
	t.Cleanup(aCancel)
	go srv.StartAuditFlusher(aCtx)
	return srv
}

// do performs a request against the test mux.
func (s *Server) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body == "" {
		rd = bytes.NewReader(nil)
	} else {
		rd = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, rd)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.testMux.ServeHTTP(rec, req)
	return rec
}
