package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all runtime configuration for opencode-backend.
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
	// LLMURL is the OpenAI-compatible base URL (e.g. a LiteLLM gateway).
	// Empty disables all smart-orchestration features.
	LLMURL string
	// LLMKey is the API key for LLMURL.
	LLMKey string
	// LLMModel is the model name to use for orchestration decisions.
	LLMModel string
	// ShowVersion prints the version and exits when true.
	ShowVersion bool
	// HealthCheck runs connectivity checks and exits when true.
	HealthCheck bool
}

// Version is the semantic version reported by --version. CI 打 tag 时用
// -ldflags "-X .../config.Version=<tag>" 注入，所以这里必须是 var（const 无法被链接器改写）。
var Version = "0.1.0"

// Parse reads configuration from command-line flags and environment variables.
// Environment variables take precedence over flag defaults where set.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("opencode-backend", flag.ContinueOnError)

	listenAddr := fs.String("listen", envOr("OCB_LISTEN", ":8080"), "HTTP listen address")
	opencodeURL := fs.String("opencode-url", envOr("OCB_OPENCODE_URL", "http://127.0.0.1:4096"), "local OpenCode server base URL")
	dbDriver := fs.String("db", envOr("OCB_DB", "sqlite"), "database driver: sqlite or postgres")
	sqlitePath := fs.String("sqlite-path", envOr("OCB_SQLITE_PATH", "opencode-backend.db"), "SQLite database file path")
	postgresDSN := fs.String("pg-dsn", os.Getenv("OCB_PG_DSN"), "PostgreSQL connection string")
	defaultAdmin := fs.String("default-admin-password", envOr("OCB_ADMIN_PASSWORD", "admin"), "default web admin password (used only on first initialization)")
	defaultToken := fs.String("default-token", os.Getenv("OCB_DEFAULT_TOKEN"), "optional pre-provisioned API token registered on first run (empty = off)")
	webhookSecret := fs.String("webhook-secret", os.Getenv("OCB_WEBHOOK_SECRET"), "optional shared secret protecting /api/webhook (empty = off)")
	workers := fs.Int("workers", envInt("OCB_WORKERS", 4), "concurrent task executions (1 = serial)")
	llmURL := fs.String("llm-url", envOr("OCB_LLM_URL", ""), "OpenAI-compatible base URL for orchestration LLM (empty = disabled)")
	llmKey := fs.String("llm-key", os.Getenv("OCB_LLM_KEY"), "API key for --llm-url")
	llmModel := fs.String("llm-model", envOr("OCB_LLM_MODEL", ""), "model name for orchestration decisions")
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
		LLMURL:               strings.TrimRight(*llmURL, "/"),
		LLMKey:               *llmKey,
		LLMModel:             *llmModel,
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
