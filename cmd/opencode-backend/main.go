package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hiylo/opencode-backend/internal/auth"
	"github.com/hiylo/opencode-backend/internal/automation"
	"github.com/hiylo/opencode-backend/internal/config"
	"github.com/hiylo/opencode-backend/internal/llm"
	"github.com/hiylo/opencode-backend/internal/opencode"
	"github.com/hiylo/opencode-backend/internal/push"
	"github.com/hiylo/opencode-backend/internal/server"
	"github.com/hiylo/opencode-backend/internal/store"
	"github.com/hiylo/opencode-backend/internal/tasks"
	"github.com/hiylo/opencode-backend/internal/webui"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.ShowVersion {
		fmt.Printf("opencode-backend %s\n", config.Version)
		return
	}

	if cfg.HealthCheck {
		os.Exit(runHealthCheck(cfg))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Build the DSN and open the store.
	var dsn string
	if cfg.DBDriver == "sqlite" {
		dsn = store.SQLiteDSN(cfg.SQLitePath)
	} else {
		dsn = cfg.PostgresDSN
	}
	st, err := store.OpenFromConfig(ctx, cfg.DBDriver, dsn)
	if err != nil {
		log.Fatalf("open %s store: %v", cfg.DBDriver, err)
	}
	defer st.Close()
	log.Printf("storage: %s", cfg.DBDriver)

	// Initialize admin password (first run only) and the auth manager.
	am := auth.NewManager(st)
	created, err := am.Initialize(ctx, cfg.DefaultAdminPassword)
	if err != nil {
		log.Fatalf("initialize auth: %v", err)
	}
	if created {
		log.Printf("initialized admin password (change it via the config UI)")
	}

	oc := opencode.New(cfg.OpenCodeURL)
	hub := push.NewHub()
	go hub.Run()
	// Deferred so Stop runs after the HTTP server has shut down and no handler
	// can register a new connection.
	defer hub.Stop()

	srv := server.New(cfg, st, am, oc, hub)
	srv.SetWebUI(webui.New())

	// Optional orchestration LLM: powers natural-language rule generation and
	// (later) result summaries and failure self-healing. Configuration is
	// loaded from persisted settings (web-configurable) with flag/env fallback.
	llmURL, llmKey, llmModel := loadLLMConfig(ctx, st, cfg)
	llmClient := llm.New(llmURL, llmKey, llmModel)
	if llmClient.Enabled() {
		log.Printf("orchestration LLM enabled: %s (%s)", llmURL, llmModel)
	}
	srv.SetLLM(llmClient)

	// Async orchestration worker: claims queued tasks and drives the
	// upstream OpenCode server. Runs for the lifetime of the process.
	exec := tasks.NewExecutor(st, hub, cfg.OpenCodeURL)
	exec.WithLLM(llmClient)
	exec.WithWorkers(cfg.Workers)
	log.Printf("orchestration workers: %d", cfg.Workers)
	go exec.Run(ctx)

	// Automation engine: evaluates cron rules and handles webhook triggers.
	// Fired rules enqueue tasks which the executor above picks up.
	eng := automation.NewEngine(st, 15*time.Second)
	srv.SetAutomation(eng)
	go eng.Run(ctx)

	// Housekeeping: purge audit logs and expired web sessions periodically.
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := st.DeleteAuditOlderThan(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
					log.Printf("housekeeping: audit cleanup: %v", err)
				} else if n > 0 {
					log.Printf("housekeeping: purged %d old audit entries", n)
				}
				if n, err := st.DeleteExpiredWebSessions(ctx); err != nil {
					log.Printf("housekeeping: session cleanup: %v", err)
				} else if n > 0 {
					log.Printf("housekeeping: purged %d expired sessions", n)
				}
			}
		}
	}()

	// Orchestration: periodically report upstream health so subscribers get
	// live status without polling from the app.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				healthy := oc.Ping(ctx) == nil
				msg := push.Message{
					Type: "upstream.health",
					Payload: mustJSON(map[string]any{
						"healthy": healthy,
						"time":    time.Now().UTC(),
					}),
				}
				hub.Broadcast(msg)
				if healthy {
					log.Printf("upstream opencode reachable at %s", cfg.OpenCodeURL)
				} else {
					log.Printf("upstream opencode UNREACHABLE at %s", cfg.OpenCodeURL)
				}
			}
		}
	}()

	if err := srv.Start(ctx); err != nil {
		log.Fatalf("server: %v", err)
	}
	log.Println("opencode-backend stopped")
}

// loadLLMConfig resolves the orchestration LLM settings. Persisted settings
// (set via the web UI) take precedence; on first run the flag/env values are
// persisted as the initial settings.
func loadLLMConfig(ctx context.Context, st store.Store, cfg *config.Config) (string, string, string) {
	url, err := st.GetSetting(ctx, "llm.url")
	if err != nil || url == "" {
		url = cfg.LLMURL
		if url != "" {
			_ = st.SetSetting(ctx, "llm.url", url)
		}
	}
	key, err := st.GetSetting(ctx, "llm.key")
	if err != nil || key == "" {
		key = cfg.LLMKey
		if key != "" {
			_ = st.SetSetting(ctx, "llm.key", key)
		}
	}
	model, err := st.GetSetting(ctx, "llm.model")
	if err != nil || model == "" {
		model = cfg.LLMModel
		if model != "" {
			_ = st.SetSetting(ctx, "llm.model", model)
		}
	}
	return url, key, model
}

// runHealthCheck verifies database and upstream connectivity without starting
// the HTTP server. It returns a process exit code (0 = all healthy).
func runHealthCheck(cfg *config.Config) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var dsn string
	if cfg.DBDriver == "sqlite" {
		dsn = store.SQLiteDSN(cfg.SQLitePath)
	} else {
		dsn = cfg.PostgresDSN
	}
	st, err := store.OpenFromConfig(ctx, cfg.DBDriver, dsn)
	if err != nil {
		fmt.Printf("FAIL db[%s]: %v\n", cfg.DBDriver, err)
		return 1
	}
	defer st.Close()
	fmt.Printf("OK   db[%s]\n", cfg.DBDriver)

	oc := opencode.New(cfg.OpenCodeURL)
	if err := oc.Ping(ctx); err != nil {
		fmt.Printf("FAIL upstream %s: %v\n", cfg.OpenCodeURL, err)
		return 1
	}
	version, _ := oc.GetVersion(ctx)
	fmt.Printf("OK   upstream %s (opencode %s)\n", cfg.OpenCodeURL, version)

	if err := st.Ping(ctx); err != nil {
		fmt.Printf("FAIL store ping: %v\n", err)
		return 1
	}
	fmt.Println("OK   store ping")
	return 0
}
