package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
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
	{name: "tasks_schedule", apply: migrationTasksSchedule},
	{name: "session_events", apply: migrationSessionEvents},
	{name: "archives_raw_messages", apply: migrationArchivesRawMessages},
	{name: "session_unread", apply: migrationSessionUnread},
	{name: "tasks_priority_timeout_workflow", apply: migrationTasksPriorityTimeoutWorkflow},
	{name: "rules_session_id", apply: migrationRulesSessionID},
	{name: "tasks_workflow_index", apply: migrationTasksWorkflowIndex},
	{name: "intel", apply: migrationIntel},
	{name: "intel_field_meta", apply: migrationIntelFieldMeta},
	{name: "intel_test_assets", apply: migrationIntelTestAssets},
	{name: "intel_features", apply: migrationIntelFeatures},
	{name: "intel_rag", apply: migrationIntelRag},
	{name: "intel_chat", apply: migrationIntelChat},
	{name: "intel_findings", apply: migrationIntelFindings},
	{name: "intel_fixes", apply: migrationIntelFixes},
	{name: "intel_gateway_routes", apply: migrationIntelGatewayRoutes},
	{name: "intel_endpoint_summary", apply: migrationIntelEndpointSummary},
	{name: "intel_impacts", apply: migrationIntelImpacts},
	{name: "intel_overviews", apply: migrationIntelOverviews},
	{name: "intel_fix_finding", apply: migrationIntelFixFinding},
	{name: "intel_dedup", apply: migrationIntelDedup},
	{name: "intel_android_bindings", apply: migrationIntelAndroidBindings},
	{name: "intel_web_bindings", apply: migrationIntelWebBindings},
	{name: "sync_bundle", apply: migrationSyncBundle},
	{name: "intel_ios_bindings", apply: migrationIntelIOSBindings},
	{name: "env", apply: migrationEnv},
}

// migrationIntel creates the Test Intelligence subsystem tables: flat project
// registry, monorepo sub-module detection results, entity/table/column
// mappings and API endpoint contracts. All intel_* rows carry provenance
// (source_file + source_line) per the deterministic-first design.
func migrationIntel(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS projects (
			%s,
			name TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'local',
			local_path TEXT NOT NULL DEFAULT '',
			git_url TEXT NOT NULL DEFAULT '',
			git_ref TEXT NOT NULL DEFAULT '',
			last_tested_sha TEXT NOT NULL DEFAULT '',
			snapshot_sha TEXT NOT NULL DEFAULT '',
			commands_json TEXT NOT NULL DEFAULT '',
			env_name TEXT NOT NULL DEFAULT '',
			analyzed_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS project_modules (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			rel_path TEXT NOT NULL DEFAULT '',
			kind_type TEXT NOT NULL DEFAULT '',
			kind_role TEXT NOT NULL DEFAULT '',
			build_tool TEXT NOT NULL DEFAULT '',
			commands_json TEXT NOT NULL DEFAULT '',
			last_tested_sha TEXT NOT NULL DEFAULT '',
			analyzed_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_entities (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			entity TEXT NOT NULL DEFAULT '',
			table_name TEXT NOT NULL DEFAULT '',
			column_name TEXT NOT NULL DEFAULT '',
			nullable BOOLEAN NOT NULL DEFAULT TRUE,
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_endpoints (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			response_type TEXT NOT NULL DEFAULT '',
			request_json TEXT NOT NULL DEFAULT '',
			fields_json TEXT NOT NULL DEFAULT '',
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_project_modules_project ON project_modules(project_id)`,
		`CREATE INDEX IF NOT EXISTS idx_intel_entities_project ON intel_entities(project_id, module_id)`,
		`CREATE INDEX IF NOT EXISTS idx_intel_endpoints_project ON intel_endpoints(project_id, module_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelFieldMeta adds the field type and primary-key marker columns to
// intel_entities so the project detail view can render a complete column
// contract (name/type/nullable/primary-key) instead of just column names.
func migrationIntelFieldMeta(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE intel_entities ADD COLUMN field_type TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE intel_entities ADD COLUMN is_primary BOOLEAN NOT NULL DEFAULT FALSE`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationArchivesRawMessages adds the raw_messages column storing an
// archive's complete structured message JSON (with parts) so archived sessions
// can be restored byte-for-byte into a new OpenCode session. Older archives
// have the column empty (”). Kept as its own migration because archives was
// already applied on existing databases.
func migrationArchivesRawMessages(ctx context.Context, driver string, db *sql.DB) error {
	if isPostgres(driver) {
		_, err := db.ExecContext(ctx, `ALTER TABLE archives ADD COLUMN IF NOT EXISTS raw_messages TEXT NOT NULL DEFAULT ''`)
		return err
	}
	// sqlite：无 ADD COLUMN IF NOT EXISTS，先查列存在性。
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('archives') WHERE name='raw_messages'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx, `ALTER TABLE archives ADD COLUMN raw_messages TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
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

// migrationTasksSchedule adds the name and scheduling columns that turn a task
// into a named, optionally delayed (scheduled_at) or recurring (cron) job.
func migrationTasksSchedule(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE tasks ADD COLUMN name TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN scheduled_at TIMESTAMP NULL`,
		`ALTER TABLE tasks ADD COLUMN cron TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tasks ADD COLUMN last_fired_at TIMESTAMP NULL`,
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

// migrationSessionEvents adds the table holding every event captured from the
// upstream OpenCode global event stream. The payload column stores the raw
// event JSON: jsonb on PostgreSQL, plain text on SQLite (the two backends
// diverge here because SQLite has no native binary JSON type). Both indexes
// serve the "recent activity per session" dashboard query and the retention
// janitor.
func migrationSessionEvents(ctx context.Context, driver string, db *sql.DB) error {
	payloadType := "TEXT"
	if isPostgres(driver) {
		payloadType = "jsonb"
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS session_events (
			%s,
			session_id TEXT NOT NULL DEFAULT '',
			event_type TEXT NOT NULL DEFAULT '',
			payload %s,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver), payloadType),
		`CREATE INDEX IF NOT EXISTS idx_session_events_session_created ON session_events(session_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_created ON session_events(created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationTasksPriorityTimeoutWorkflow adds orchestration columns:
// priority (0-100, higher runs first), timeout_seconds (0 = no timeout), and
// workflow_id (groups chained steps of a multi-step orchestration).
func migrationTasksPriorityTimeoutWorkflow(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE tasks ADD COLUMN priority INTEGER NOT NULL DEFAULT 50`,
		`ALTER TABLE tasks ADD COLUMN timeout_seconds INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE tasks ADD COLUMN workflow_id TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_claim ON tasks(status, available_at, priority, created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationTasksWorkflowIndex adds indexes for the orchestration queries that
// group/filter by workflow_id and for the newest-first task listing, avoiding
// full-table scans once the tasks table grows.
func migrationTasksWorkflowIndex(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_tasks_workflow ON tasks(workflow_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_created_at ON tasks(created_at)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationRulesSessionID lets a rule fire into a fixed, pre-bound session
// instead of creating a fresh session per execution. Old rows keep ” (new
// session per run, previous behavior).
func migrationRulesSessionID(ctx context.Context, driver string, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `ALTER TABLE rules ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`)
	return err
}

// migrationSessionUnread adds the session unread table used by the shared
// Web/App "has new messages" indicator. Row exists = unread; MarkSessionRead
// deletes it. Portable across SQLite and PostgreSQL.
func migrationSessionUnread(ctx context.Context, driver string, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS session_unread (
		session_id TEXT PRIMARY KEY,
		unread BOOLEAN NOT NULL DEFAULT TRUE,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
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

// migrationIntelTestAssets creates the test-execution half of the Test
// Intelligence subsystem: discovered test cases, test runs and per-case results
// plus the issue closed-loop table (intel_issues). test_cases are the static
// assets discovered by the scanner; test_runs/test_results are one execution's
// dynamic outcome; intel_issues track problems across commits until resolved.
func migrationIntelTestAssets(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS test_cases (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			module TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			framework TEXT NOT NULL DEFAULT '',
			class TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '',
			last_status TEXT NOT NULL DEFAULT '',
			last_duration_ms INTEGER NOT NULL DEFAULT 0,
			flaky_count INTEGER NOT NULL DEFAULT 0,
			last_run_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS test_runs (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			scope TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			command TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			started_at TIMESTAMP NULL,
			finished_at TIMESTAMP NULL,
			log_path TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS test_results (
			%s,
			run_id INTEGER NOT NULL DEFAULT 0,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			case_id INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT '',
			endpoint TEXT NOT NULL DEFAULT '',
			passed BOOLEAN NOT NULL DEFAULT FALSE,
			failures_json TEXT NOT NULL DEFAULT '',
			rootcause_json TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_issues (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			feature_id INTEGER NOT NULL DEFAULT 0,
			key TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'bug',
			severity TEXT NOT NULL DEFAULT 'medium',
			location TEXT NOT NULL DEFAULT '',
			commit_seen TEXT NOT NULL DEFAULT '',
			commit_fixed TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			resolved_at TIMESTAMP NULL,
			last_check_at TIMESTAMP NULL,
			detail_json TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_test_cases_project ON test_cases(project_id, module_id)`,
		`CREATE INDEX IF NOT EXISTS idx_test_runs_project ON test_runs(project_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_test_results_run ON test_results(run_id)`,
		`CREATE INDEX IF NOT EXISTS idx_intel_issues_project ON intel_issues(project_id, status)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelFeatures creates the feature-point table: business feature units
// that cluster endpoints, host feature-level tests, attach integration bugs and
// carry the AI-chat context. sort_order is human-priority (drag reorder), never
// overwritten by re-scans.
func migrationIntelFeatures(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_features (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			ends_json TEXT NOT NULL DEFAULT '',
			sort_order INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT 'auto',
			anchor TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_features_project ON intel_features(project_id, sort_order)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelRag creates the project knowledge-base vector store: one
// intel_chunks table holding embedded fragments of a project's code/contracts,
// indexed by pgvector for cosine retrieval. On PostgreSQL it installs the
// vector extension and an HNSW index; on SQLite the embedding column degrades
// to a JSON-text blob and retrieval falls back to in-process cosine.
func migrationIntelRag(ctx context.Context, driver string, db *sql.DB) error {
	embedCol := `embedding TEXT NOT NULL DEFAULT ''`
	if isPostgres(driver) {
		if _, err := db.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			return err
		}
		embedCol = `embedding vector(` + strconv.Itoa(EmbedDim) + `) NOT NULL`
	}
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_chunks (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT '',
			ref_id INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0,
			%s,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver), embedCol),
		`CREATE INDEX IF NOT EXISTS idx_intel_chunks_project ON intel_chunks(project_id, module_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	if isPostgres(driver) {
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS idx_intel_chunks_embedding
			 ON intel_chunks USING hnsw (embedding vector_cosine_ops)`)); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelChat creates the project-level knowledge-base conversation
// tables: intel_chats groups a dialog thread, intel_chat_messages stores each
// user/assistant turn so multi-turn context can be reconstructed and reviewed.
func migrationIntelChat(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_chats (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			title TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_chat_messages (
			%s,
			chat_id INTEGER NOT NULL DEFAULT 0,
			role TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL DEFAULT '',
			sources_json TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_chats_project ON intel_chats(project_id, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_intel_chat_messages_chat ON intel_chat_messages(chat_id, id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelFindings creates the security/compliance audit findings table.
// Findings come from dependency-vuln scanners, code/lint rules, compliance
// rules and LLM review; each carries a detector + severity + location and moves
// through the closed loop (open/resolved/removed/false_positive/waived).
func migrationIntelFindings(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_findings (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			detector TEXT NOT NULL DEFAULT '',
			severity TEXT NOT NULL DEFAULT 'medium',
			category TEXT NOT NULL DEFAULT '',
			cve_or_rule_id TEXT NOT NULL DEFAULT '',
			location TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			removed_at TIMESTAMP NULL,
			waived_reason TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_findings_project ON intel_findings(project_id, status)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelGatewayRoutes creates the gateway-route table: the public
// gateway exposure (path patterns) of each backend service, discovered from
// Spring Cloud Gateway config and Nacos gateway.paths metadata.
func migrationIntelGatewayRoutes(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_gateway_routes (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			service TEXT NOT NULL DEFAULT '',
			paths_json TEXT NOT NULL DEFAULT '',
			uri TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_gateway_routes_project ON intel_gateway_routes(project_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelEndpointSummary adds the business summary column to
// intel_endpoints, filled by the LLM document-analysis pass (empty until the
// orchestration LLM is configured).
func migrationIntelEndpointSummary(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE intel_endpoints ADD COLUMN summary TEXT NOT NULL DEFAULT ''`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelImpacts creates the incremental-impact table: the latest
// Git-delta impact snapshot (changed files → affected modules/tests/sources)
// computed during analyze for git-backed projects.
func migrationIntelImpacts(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_impacts (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			base_sha TEXT NOT NULL DEFAULT '',
			head_sha TEXT NOT NULL DEFAULT '',
			impact_json TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_impacts_project ON intel_impacts(project_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelOverviews creates the dependency/environment overview table:
// one row per project holding the aggregated dependencies, env requirements and
// CycloneDX SBOM, refreshed on each analyze.
func migrationIntelOverviews(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_overviews (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			deps_json TEXT NOT NULL DEFAULT '',
			env_json TEXT NOT NULL DEFAULT '',
			sbom_json TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_overviews_project ON intel_overviews(project_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelFixFinding adds the finding_id column to intel_fixes so a fix
// suggestion can be generated from a compliance/security finding (which carries
// a source file:line location) in addition to a test-failure issue.
func migrationIntelFixFinding(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE intel_fixes ADD COLUMN finding_id INTEGER NOT NULL DEFAULT 0`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelAndroidBindings creates the client field-binding table:
// Android DataBinding "page -> field path" extractions (the must-display field
// list). Each row carries source file:line provenance.
func migrationIntelAndroidBindings(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_android_bindings (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			page TEXT NOT NULL DEFAULT '',
			field_path TEXT NOT NULL DEFAULT '',
			widget TEXT NOT NULL DEFAULT '',
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_android_bindings_project ON intel_android_bindings(project_id, module_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelWebBindings creates the Web client field-binding table: Vue
// template "page -> field path" extractions (the must-display field list).
func migrationIntelWebBindings(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_web_bindings (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			page TEXT NOT NULL DEFAULT '',
			field_path TEXT NOT NULL DEFAULT '',
			slot TEXT NOT NULL DEFAULT '',
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_web_bindings_project ON intel_web_bindings(project_id, module_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelDedup removes duplicate rows left by earlier scans that keyed
// the child tables by an unstable module id. Each table is collapsed to the
// lowest id per natural key; a subsequent analyze re-inserts a clean snapshot.
func migrationIntelDedup(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`DELETE FROM intel_entities WHERE id NOT IN (
			SELECT MIN(id) FROM intel_entities
			GROUP BY project_id, entity, table_name, column_name, source_file, source_line)`,
		`DELETE FROM intel_endpoints WHERE id NOT IN (
			SELECT MIN(id) FROM intel_endpoints
			GROUP BY project_id, method, path, source_file, source_line)`,
		`DELETE FROM test_cases WHERE id NOT IN (
			SELECT MIN(id) FROM test_cases
			GROUP BY project_id, module, class, method, path)`,
		`DELETE FROM intel_findings WHERE id NOT IN (
			SELECT MIN(id) FROM intel_findings
			GROUP BY project_id, detector, cve_or_rule_id, location)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelIOSBindings creates the iOS client field-binding table: SwiftUI
// view "page -> field path" extractions (the must-display field list).
func migrationIntelIOSBindings(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_ios_bindings (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			page TEXT NOT NULL DEFAULT '',
			field_path TEXT NOT NULL DEFAULT '',
			slot TEXT NOT NULL DEFAULT '',
			source_file TEXT NOT NULL DEFAULT '',
			source_line INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_ios_bindings_project ON intel_ios_bindings(project_id, module_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationSyncBundle creates the device/team config sync table: one row per
// sync key (a server id or "global") holding the latest JSON snapshot pushed by
// any device. Last-write-wins via a monotonically increasing revision so the
// app can pull the newest snapshot and detect a drift from its base revision.
func migrationSyncBundle(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_bundle (
			key TEXT PRIMARY KEY,
			payload TEXT NOT NULL DEFAULT '',
			revision BIGINT NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationIntelFixes creates the fix-suggestion table: patch drafts produced
// from failures/audit findings, applied only after human review on the page
// (the single write path). Backups are kept for one-click rollback.
func migrationIntelFixes(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS intel_fixes (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			issue_id INTEGER NOT NULL DEFAULT 0,
			kind TEXT NOT NULL DEFAULT 'ai-suggest',
			title TEXT NOT NULL DEFAULT '',
			diff_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'proposed',
			applied_backup TEXT NOT NULL DEFAULT '',
			write_mode TEXT NOT NULL DEFAULT 'direct',
			applied_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_intel_fixes_project ON intel_fixes(project_id, status)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// migrationEnv creates the environment-management tables: env_requirements
// (project dependency declarations from static detection) and env_services
// (the current per-item status of each middleware/toolchain after probing and
// provisioning). status: ready|missing|unsupported; provider: container|
// external|installed.
func migrationEnv(ctx context.Context, driver string, db *sql.DB) error {
	stmts := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS env_requirements (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			module_id INTEGER NOT NULL DEFAULT 0,
			service TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'auto',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_env_requirements_project ON env_requirements(project_id)`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS env_services (
			%s,
			project_id INTEGER NOT NULL DEFAULT 0,
			service TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'missing',
			host TEXT NOT NULL DEFAULT '',
			port INTEGER NOT NULL DEFAULT 0,
			endpoint TEXT NOT NULL DEFAULT '',
			healthy BOOLEAN NOT NULL DEFAULT FALSE,
			container_name TEXT NOT NULL DEFAULT '',
			container_id TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '',
			health_check_at TIMESTAMP NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`, idColumn(driver)),
		`CREATE INDEX IF NOT EXISTS idx_env_services_project ON env_services(project_id)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}
