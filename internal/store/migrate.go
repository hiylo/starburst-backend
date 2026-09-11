package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrate applies schema migrations to the database.
// Migration version is tracked in a meta table, so both SQLite and
// PostgreSQL start from the same version sequence.
func migrate(ctx context.Context, driver string, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	current, err := currentVersion(ctx, db)
	if err != nil {
		return err
	}

	for current < len(migrations) {
		next := current + 1
		m := migrations[current]
		if err := m.apply(ctx, driver, db); err != nil {
			return fmt.Errorf("migration %d (%s): %w", next, m.name, err)
		}
		if _, err := db.ExecContext(ctx, rebind(driver,
			`INSERT INTO schema_migrations (version) VALUES (?)`), next); err != nil {
			return fmt.Errorf("record migration %d: %w", next, err)
		}
		current = next
	}
	return nil
}

func currentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read current version: %w", err)
	}
	return v, nil
}

// migration is a single versioned schema step with a portable name.
// apply receives the logical driver ("sqlite" or "postgres") so DDL can be
// emitted per dialect where the two backends diverge.
type migration struct {
	name  string
	apply func(ctx context.Context, driver string, db *sql.DB) error
}

// isPostgres reports whether driver refers to the PostgreSQL backend. It may
// arrive as the logical name "postgres" (store.Open called directly) or as the
// registered database/sql name "pgx" (the path actually used via
// OpenFromConfig), so both spellings are accepted.
func isPostgres(driver string) bool {
	return driver == "postgres" || driver == "pgx"
}

// idColumn returns a portable auto-increment primary key definition.
// PostgreSQL has no AUTOINCREMENT keyword and the SQLite bundled with
// modernc.org/sqlite predates GENERATED AS IDENTITY, so the two backends need
// different DDL.
func idColumn(driver string) string {
	if isPostgres(driver) {
		return "id BIGSERIAL PRIMARY KEY"
	}
	return "id INTEGER PRIMARY KEY AUTOINCREMENT"
}

// migrations is the ordered list of schema steps.
// IMPORTANT: never reorder or edit existing entries; append new ones only.
var migrations = []migration{
	{name: "initial", apply: migrationInitial},
	{name: "tasks", apply: migrationTasks},
	{name: "tasks_available_at", apply: migrationTasksAvailableAt},
	{name: "rules", apply: migrationRules},
	{name: "audit_log", apply: migrationAuditLog},
	{name: "archives", apply: migrationArchives},
	{name: "web_sessions", apply: migrationWebSessions},
	{name: "rule_executions", apply: migrationRuleExecutions},
	{name: "tasks_ai_summary", apply: migrationTasksAISummary},
	{name: "tasks_depends_on", apply: migrationTasksDependsOn},
}

// migrationTasksDependsOn adds the depends_on column holding the id of the
// single upstream task this task waits for before it may be executed, plus an
// index for the dependents lookup used when the upstream finishes.
func migrationTasksDependsOn(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE tasks ADD COLUMN depends_on TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_depends_on ON tasks(depends_on, status)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationTasksAISummary adds the ai_summary column holding the LLM-generated
// result summary (success) or root-cause analysis (failure) for a task.
func migrationTasksAISummary(ctx context.Context, driver string, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE tasks ADD COLUMN ai_summary TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return nil
}

// migrationRuleExecutions adds the rule execution history table.
func migrationRuleExecutions(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS rule_executions (
			%s,
			rule_id TEXT NOT NULL,
			task_id TEXT NOT NULL DEFAULT '',
			triggered_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_rule_exec_rule_id ON rule_executions(rule_id, id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationWebSessions adds the persisted web admin session table.
func migrationWebSessions(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS web_sessions (
			id TEXT PRIMARY KEY,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions(expires_at)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationArchives adds the session archive table.
func migrationArchives(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS archives (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			format TEXT NOT NULL DEFAULT 'markdown',
			content TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationAuditLog adds the API audit trail table.
func migrationAuditLog(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS audit_log (
			%s,
			token_id TEXT NOT NULL DEFAULT '',
			token_name TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			status INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_audit_token_created ON audit_log(token_id, id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationRules adds the automation rules table.
func migrationRules(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rules (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'cron',
			schedule TEXT NOT NULL DEFAULT '',
			directory TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_fired_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationTasksAvailableAt adds the available_at gate used for retry backoff.
// It must tolerate both freshly-migrated and pre-existing databases.
func migrationTasksAvailableAt(ctx context.Context, driver string, db *sql.DB) error {
	// SQLite supports ADD COLUMN with a constant default; PG too.
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE tasks ADD COLUMN available_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_tasks_available ON tasks(status, available_at)`); err != nil {
		return err
	}
	return nil
}

// migrationInitial creates the base tables shared by all drivers.
// DDL uses only portable constructs that both SQLite and PostgreSQL accept.
func migrationInitial(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS tokens (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL UNIQUE,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			revoked_at TIMESTAMP NULL,
			last_used TIMESTAMP NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationTasks adds the async orchestration task table.
func migrationTasks(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			directory TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			error TEXT NOT NULL DEFAULT '',
			result TEXT NOT NULL DEFAULT '',
			progress TEXT NOT NULL DEFAULT '',
			attempts INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			started_at TIMESTAMP NULL,
			finished_at TIMESTAMP NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_status_created ON tasks(status, created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
