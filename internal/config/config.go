package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// DefaultSTTTimeout bounds one round trip to the recognition engine.
const DefaultSTTTimeout = 30 * time.Second

// Config holds all runtime configuration for starburst-backend.
type Config struct {
	// ListenAddr is the address the HTTP server binds to, e.g. ":8080".
	ListenAddr string
	// OpenCodeURL is the base URL of the local OpenCode server, e.g. "http://127.0.0.1:4096".
	OpenCodeURL string
	// DBDriver is "sqlite" or "postgres".
	DBDriver string
	// SQLitePath is the database file path when DBDriver is sqlite.
	SQLitePath string
	// PostgresDSN is the connection string when DBDriver is postgres.
	PostgresDSN string
	// DefaultAdminPassword is the initial web admin password if no stored one exists.
	DefaultAdminPassword string
	// DefaultToken is an optional pre-provisioned API token registered on first run,
	// so clients can connect without first creating a token via the web UI.
	DefaultToken string
	// WebhookSecret optionally protects /api/webhook (shared secret). Empty = off.
	WebhookSecret string
	// Workers is how many orchestration tasks may run concurrently. 1 restores
	// serial execution.
	Workers int
	// TaskRetention is how long finished tasks are kept before the janitor
	// deletes them. Zero keeps them forever.
	TaskRetention time.Duration
	// LLMURL is the OpenAI-compatible base URL (e.g. a LiteLLM gateway).
	// Empty disables all smart-orchestration features.
	LLMURL string
	// LLMKey is the API key for LLMURL.
	LLMKey string
	// LLMModel is the model name to use for orchestration decisions.
	LLMModel string
	// STTURL is the base URL of the streaming recognition engine, e.g.
	// "http://192.0.2.150:18090". Empty disables /api/stt entirely, so
	// clients fall back to on-device recognition.
	STTURL string
	// STTTimeout bounds a single round trip to the recognition engine.
	STTTimeout time.Duration
	// STTMaxChunkBytes caps one audio chunk accepted from a client.
	STTMaxChunkBytes int
	// ShowVersion prints the version and exits when true.
	ShowVersion bool
	// HealthCheck runs connectivity checks and exits when true.
	HealthCheck bool
}

// Version is the semantic version reported by --version. CI 打 tag 时用
// -ldflags "-X .../config.Version=<tag>" 注入，所以这里必须是 var（const 无法被链接器改写）。
var Version = "1.0.0"

// Parse reads configuration from command-line flags and environment variables.
// Environment variables take precedence over flag defaults where set.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("starburst-backend", flag.ContinueOnError)

	listenAddr := fs.String("listen", envOr("OCB_LISTEN", ":18880"), "HTTP listen address")
	opencodeURL := fs.String("opencode-url", envOr("OCB_OPENCODE_URL", "http://127.0.0.1:4096"), "local OpenCode server base URL")
	dbDriver := fs.String("db", envOr("OCB_DB", "sqlite"), "database driver: sqlite or postgres")
	sqlitePath := fs.String("sqlite-path", envOr("OCB_SQLITE_PATH", "starburst-backend.db"), "SQLite database file path")
	postgresDSN := fs.String("pg-dsn", os.Getenv("OCB_PG_DSN"), "PostgreSQL connection string")
	defaultAdmin := fs.String("default-admin-password", envOr("OCB_ADMIN_PASSWORD", "admin"), "default web admin password (used only on first initialization)")
	defaultToken := fs.String("default-token", os.Getenv("OCB_DEFAULT_TOKEN"), "optional pre-provisioned API token registered on first run (empty = off)")
	webhookSecret := fs.String("webhook-secret", os.Getenv("OCB_WEBHOOK_SECRET"), "optional shared secret protecting /api/webhook (empty = off)")
	workers := fs.Int("workers", envInt("OCB_WORKERS", 4), "concurrent task executions (1 = serial)")
	taskRetention := fs.Duration("task-retention", envDuration("OCB_TASK_RETENTION", 0), "finished task retention, 0 = forever")
	llmURL := fs.String("llm-url", envOr("OCB_LLM_URL", ""), "OpenAI-compatible base URL for orchestration LLM (empty = disabled)")
	llmKey := fs.String("llm-key", os.Getenv("OCB_LLM_KEY"), "API key for --llm-url")
	llmModel := fs.String("llm-model", envOr("OCB_LLM_MODEL", ""), "model name for orchestration decisions")
	sttURL := fs.String("stt-url", envOr("OCB_STT_URL", ""), "streaming recognition engine base URL (empty = disabled)")
	sttTimeout := fs.Duration("stt-timeout", envDuration("OCB_STT_TIMEOUT", DefaultSTTTimeout), "timeout for one engine round trip")
	sttMaxChunk := fs.Int("stt-max-chunk-bytes", envInt("OCB_STT_MAX_CHUNK_BYTES", 2*1024*1024), "max bytes accepted per audio chunk")
	showVersion := fs.Bool("version", false, "print version and exit")
	healthCheck := fs.Bool("health-check", false, "run connectivity checks and exit")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	driver := strings.ToLower(*dbDriver)
	if driver != "sqlite" && driver != "postgres" {
		return nil, fmt.Errorf("invalid db driver %q: must be sqlite or postgres", *dbDriver)
	}
	if driver == "postgres" && strings.TrimSpace(*postgresDSN) == "" {
		return nil, fmt.Errorf("db=postgres requires a postgres DSN (--pg-dsn or OCB_PG_DSN)")
	}

	return &Config{
		ListenAddr:           *listenAddr,
		OpenCodeURL:          strings.TrimRight(*opencodeURL, "/"),
		DBDriver:             driver,
		SQLitePath:           *sqlitePath,
		PostgresDSN:          *postgresDSN,
		DefaultAdminPassword: *defaultAdmin,
		DefaultToken:         *defaultToken,
		WebhookSecret:        *webhookSecret,
		Workers:              clampInt(*workers, 1, 64),
		TaskRetention:        *taskRetention,
		LLMURL:               strings.TrimRight(*llmURL, "/"),
		LLMKey:               *llmKey,
		LLMModel:             *llmModel,
		STTURL:               strings.TrimRight(*sttURL, "/"),
		STTTimeout:           sttTimeoutOrDefault(*sttTimeout, DefaultSTTTimeout),
		STTMaxChunkBytes:     clampInt(*sttMaxChunk, 1024, 8*1024*1024),
		ShowVersion:          *showVersion,
		HealthCheck:          *healthCheck,
	}, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// envInt reads an integer environment variable, falling back when unset or
// unparseable.
func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

// sttTimeoutOrDefault returns d when it is positive, otherwise fallback.
func sttTimeoutOrDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// envDuration reads a Go duration environment variable ("72h", "168h0m"),
// falling back when unset or unparseable.
func envDuration(key string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return d
}

// clampInt limits n to [lo, hi].
func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// Port returns the numeric port from ListenAddr (e.g. ":8080" -> 8080).
func (c *Config) Port() int {
	idx := strings.LastIndex(c.ListenAddr, ":")
	if idx < 0 || idx == len(c.ListenAddr)-1 {
		return 0
	}
	n, err := strconv.Atoi(c.ListenAddr[idx+1:])
	if err != nil {
		return 0
	}
	return n
}
