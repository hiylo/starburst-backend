package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Common errors returned by the store.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: conflict")
)

// Setting is a single configuration key/value persisted in the store.
type Setting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Token represents an API token issued to a client device.
type Token struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	TokenHash string     `json:"-"` // sha256 hex; never serialized to clients
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt"`
	LastUsed  *time.Time `json:"lastUsed"`
}

// Store is the persistence abstraction shared by the app.
// It is implemented by both SQLite and PostgreSQL drivers.
type Store interface {
	// Close releases the underlying database.
	Close() error
	// GetSetting reads a single setting value; returns ErrNotFound when missing.
	GetSetting(ctx context.Context, key string) (string, error)
	// SetSetting upserts a single setting value.
	SetSetting(ctx context.Context, key, value string) error
	// CreateToken persists a new token record. Returns the record with generated ID.
	CreateToken(ctx context.Context, t *Token) error
	// GetTokenByHash looks up a non-revoked token by its sha256 hash.
	GetTokenByHash(ctx context.Context, hash string) (*Token, error)
	// ListTokens returns all tokens, revoked ones last, newest first.
	ListTokens(ctx context.Context) ([]*Token, error)
	// RevokeToken marks a token revoked by id. Returns ErrNotFound if missing.
	RevokeToken(ctx context.Context, id string) error
	// TouchToken updates LastUsed for a token.
	TouchToken(ctx context.Context, id string) error
	// Ping verifies database connectivity.
	Ping(ctx context.Context) error

	// ---- Tasks (async orchestration queue) ----

	// CreateTask persists a queued task.
	CreateTask(ctx context.Context, t *Task) error
	// CreateTaskWithStatus persists a task with an explicit initial status,
	// e.g. TaskPending when it must wait for its DependsOn task to succeed.
	CreateTaskWithStatus(ctx context.Context, t *Task, status string) error
	// ListTasks returns tasks, newest first, with optional status filter and
	// limit/offset pagination.
	ListTasks(ctx context.Context, status string, limit, offset int) ([]*Task, error)
	// CountTasks returns how many tasks match the optional status filter.
	CountTasks(ctx context.Context, status string) (int, error)
	// PurgeFinishedTasks deletes terminal tasks older than olderThan, skipping
	// any that a dependent still references. Returns deleted and kept counts.
	PurgeFinishedTasks(ctx context.Context, olderThan time.Duration, limit int) (int, int, error)
	// GetTask loads a single task.
	GetTask(ctx context.Context, id string) (*Task, error)
	// ClaimNextTask picks the oldest queued task and marks it running.
	ClaimNextTask(ctx context.Context) (*Task, error)
	// PromotePendingDependents re-queues every pending or blocked task waiting
	// on upstreamID, e.g. when the upstream later succeeds after a retry.
	// Returns the ids of the re-queued tasks.
	PromotePendingDependents(ctx context.Context, upstreamID string) ([]string, error)
	// BlockDependents marks every pending task waiting on upstreamID as
	// blocked, recording reason in the error column. Returns the ids of the
	// blocked tasks.
	BlockDependents(ctx context.Context, upstreamID, reason string) ([]string, error)
	// UnblockTask manually re-queues a blocked task. Returns true if changed.
	UnblockTask(ctx context.Context, id string) (bool, error)
	// RequeueTask re-queues a terminal task (failed/canceled) immediately.
	RequeueTask(ctx context.Context, id string) (bool, error)
	// ListTasksByWorkflow returns a multi-step orchestration's tasks (oldest first).
	ListTasksByWorkflow(ctx context.Context, workflowID string) ([]*Task, error)
	// ListWorkflowSummaries returns recent orchestrations with a step status rollup.
	ListWorkflowSummaries(ctx context.Context, limit int) ([]*WorkflowSummary, error)
	// CancelWorkflow cancels every non-terminal task of an orchestration.
	CancelWorkflow(ctx context.Context, workflowID string) (int, error)
	// RerunWorkflowFromFailed restarts an orchestration from its first unfinished step.
	RerunWorkflowFromFailed(ctx context.Context, workflowID string) (int, error)
	// CancelTasks batch-cancels non-terminal tasks.
	CancelTasks(ctx context.Context, ids []string) (int, error)
	// RequeueTasks batch-requeues terminal (failed/canceled) tasks.
	RequeueTasks(ctx context.Context, ids []string) (int, error)
	// ListDependentsOf returns tasks waiting on upstreamID.
	ListDependentsOf(ctx context.Context, upstreamID string) ([]*Task, error)
	// ListStuckRunning returns running tasks with no progress since the given time.
	ListStuckRunning(ctx context.Context, noProgressSince time.Time) ([]*Task, error)
	// RequeueRunning marks a single running task back to queued.
	RequeueRunning(ctx context.Context, id string) (bool, error)
	// TaskStatsDetailed aggregates task outcomes over the trailing window days.
	TaskStatsDetailed(ctx context.Context, windowDays int) (*TaskStatsWindow, error)
	// UpdateTaskProgress records a progress note for a running task.
	UpdateTaskProgress(ctx context.Context, id, progress string) error
	// SetTaskSession records the resolved session id for a task.
	SetTaskSession(ctx context.Context, id, sessionID string) error
	// CompleteTask marks a task succeeded with a result.
	CompleteTask(ctx context.Context, id, result string) error
	// SetTaskAISummary records the LLM-generated summary or failure analysis.
	SetTaskAISummary(ctx context.Context, id, summary string) error
	// FailTask marks a task failed.
	FailTask(ctx context.Context, id, errMsg string) error
	// RetryTask re-queues a failed task for another attempt after backoffSecs,
	// incrementing attempts and clearing the error. Returns ErrNotFound if the
	// task does not exist.
	RetryTask(ctx context.Context, id string, backoffSecs int) error
	// CancelTask marks a queued/pending/running task canceled. Returns true if changed.
	CancelTask(ctx context.Context, id string) (bool, error)
	// IsTaskCanceled reports whether a task is currently in canceled state.
	IsTaskCanceled(ctx context.Context, id string) (bool, error)
	// RecoverStaleRunning resets tasks left in running state (e.g. after a
	// process restart) back to queued so the executor picks them up again.
	RecoverStaleRunning(ctx context.Context) (int, error)

	// ---- Task scheduling ----

	// ListScheduledTasks returns all tasks in the scheduled state (one-shot
	// future and recurring cron templates).
	ListScheduledTasks(ctx context.Context) ([]*Task, error)
	// PromoteScheduledTask flips a scheduled one-shot task to queued.
	// Returns true if changed.
	PromoteScheduledTask(ctx context.Context, id string) (bool, error)
	// SetTaskLastFiredAt records the last time a recurring template fired.
	SetTaskLastFiredAt(ctx context.Context, id string, at time.Time) error
	// SetTaskLastFiredAtPtr sets the cursor to at, or NULL when at is nil.
	SetTaskLastFiredAtPtr(ctx context.Context, id string, at *time.Time) error
	// ClaimRecurringFire atomically advances a recurring template's cursor only
	// if it still matches expected; returns false when another scheduler won.
	ClaimRecurringFire(ctx context.Context, id string, expected *time.Time, now time.Time) (bool, error)
	// CancelScheduledTask cancels a scheduled (one-shot or recurring) task.
	// Returns true if changed.
	CancelScheduledTask(ctx context.Context, id string) (bool, error)

	// ---- Automation rules ----

	// CreateRule persists a new rule.
	CreateRule(ctx context.Context, r *Rule) error
	// ListRules returns all rules, enabled first.
	ListRules(ctx context.Context) ([]*Rule, error)
	// GetRule loads a single rule.
	GetRule(ctx context.Context, id string) (*Rule, error)
	// DeleteRule removes a rule by id.
	DeleteRule(ctx context.Context, id string) error
	// MarkRuleFired records when a rule last created a task.
	MarkRuleFired(ctx context.Context, id string) error

	// ---- Rule executions ----

	// RecordRuleExecution logs a rule firing with the task it produced.
	RecordRuleExecution(ctx context.Context, ruleID, taskID string) error
	// ListRuleExecutions returns recent executions (per rule or all).
	ListRuleExecutions(ctx context.Context, ruleID string, limit int) ([]*RuleExecution, error)
	// CountRuleExecutions returns total executions for a rule (or all).
	CountRuleExecutions(ctx context.Context, ruleID string) (int, error)

	// ---- Audit trail ----

	// RecordAudit inserts an API access audit entry.
	RecordAudit(ctx context.Context, e *AuditEntry) error
	// RecordAudits inserts many audit rows in one multi-row INSERT.
	RecordAudits(ctx context.Context, entries []*AuditEntry) error
	// ListAudit returns recent audit entries, newest first.
	ListAudit(ctx context.Context, tokenID string, limit int) ([]*AuditEntry, error)
	// DeleteAuditOlderThan purges audit entries older than the given cutoff.
	DeleteAuditOlderThan(ctx context.Context, cutoff time.Time) (int, error)

	// ---- Session archives ----

	// CreateArchive persists an archive snapshot.
	CreateArchive(ctx context.Context, a *Archive) error
	// ListArchives returns archive metadata (without content), newest first.
	ListArchives(ctx context.Context, limit int) ([]*Archive, error)
	// GetArchive loads a full archive including content.
	GetArchive(ctx context.Context, id string) (*Archive, error)
	// DeleteArchive removes an archive by id.
	DeleteArchive(ctx context.Context, id string) error

	// ---- Usage statistics ----

	// TaskStats returns aggregate task counters by status.
	TaskStats(ctx context.Context) (*TaskStats, error)
	// TokenUsageByAudit aggregates audit calls per token, newest active first.
	TokenUsageByAudit(ctx context.Context, limit int) ([]*TokenUsage, error)
	// CountArchives returns the number of stored session archives.
	CountArchives(ctx context.Context) (int, error)

	// ---- Web sessions ----

	// CreateWebSession stores a web session with an expiry.
	CreateWebSession(ctx context.Context, id string, expiresAt time.Time) error
	// GetWebSession returns a non-expired session by id.
	GetWebSession(ctx context.Context, id string) (*WebSession, error)
	// DeleteWebSession removes a session (logout).
	DeleteWebSession(ctx context.Context, id string) error
	// DeleteExpiredWebSessions purges expired sessions.
	DeleteExpiredWebSessions(ctx context.Context) (int, error)

	// ---- Session events (global event collector) ----

	// InsertEvent stores one event captured from the global event stream.
	InsertEvent(ctx context.Context, e *SessionEvent) error
	// InsertEvents stores a batch of session events with a single multi-row INSERT.
	InsertEvents(ctx context.Context, events []*SessionEvent) error
	// ListEvents returns session events, oldest first, optionally filtered by
	// session id and a since cutoff. limit caps the number of rows.
	ListEvents(ctx context.Context, sessionID string, since time.Time, limit int) ([]*SessionEvent, error)
	// DeleteEventsOlderThan purges session events older than cutoff.
	DeleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int, error)

	// ---- Session unread (new-message indicator, shared across Web/App) ----

	// SetSessionsUnread upserts unread=1 for the given session ids.
	SetSessionsUnread(ctx context.Context, sessionIDs []string) error
	// ListUnread returns the set of session ids currently flagged unread.
	ListUnread(ctx context.Context) (map[string]bool, error)
	// MarkSessionRead clears the unread flag for a session.
	MarkSessionRead(ctx context.Context, sessionID string) error

	// ---- Test Intelligence (intel subsystem) ----

	// CreateIntelProject persists a new test-intelligence project.
	CreateIntelProject(ctx context.Context, p *IntelProject) error
	// ListIntelProjects returns all registered projects, newest first.
	ListIntelProjects(ctx context.Context) ([]*IntelProject, error)
	// GetIntelProject loads a single project by id.
	GetIntelProject(ctx context.Context, id int64) (*IntelProject, error)
	// UpdateIntelProject persists the mutable project fields.
	UpdateIntelProject(ctx context.Context, p *IntelProject) error
	// MarkIntelProjectAnalyzed records the snapshot sha and analyzed timestamp.
	MarkIntelProjectAnalyzed(ctx context.Context, id int64, snapshotSHA string) error
	// DeleteIntelProject removes a project and all its intel data.
	DeleteIntelProject(ctx context.Context, id int64) error
	// ReplaceIntelModules replaces the project's module list (full rescan).
	ReplaceIntelModules(ctx context.Context, projectID int64, mods []*IntelModule) error
	// ListIntelModules returns the project's sub-project modules.
	ListIntelModules(ctx context.Context, projectID int64) ([]*IntelModule, error)
	// ReplaceIntelEntities replaces a module's entity↔table↔column mappings.
	ReplaceIntelEntities(ctx context.Context, projectID, moduleID int64, ents []*IntelEntity) error
	// ListIntelEntities returns entity mappings for a project/module.
	ListIntelEntities(ctx context.Context, projectID, moduleID int64) ([]*IntelEntity, error)
	// ReplaceIntelEndpoints replaces a module's endpoint contracts.
	ReplaceIntelEndpoints(ctx context.Context, projectID, moduleID int64, eps []*IntelEndpoint) error
	// ListIntelEndpoints returns endpoint contracts for a project/module.
	ListIntelEndpoints(ctx context.Context, projectID, moduleID int64) ([]*IntelEndpoint, error)

	// ---- Test Intelligence: test assets & execution ----

	// ReplaceIntelTestCases replaces a module's discovered test cases.
	ReplaceIntelTestCases(ctx context.Context, projectID, moduleID int64, cases []*TestCase) error
	// ListIntelTestCases returns test cases for a project/module.
	ListIntelTestCases(ctx context.Context, projectID, moduleID int64) ([]*TestCase, error)
	// CreateIntelTestRun persists a new test run and populates its id.
	CreateIntelTestRun(ctx context.Context, run *TestRun) error
	// GetIntelTestRun loads a single test run.
	GetIntelTestRun(ctx context.Context, id int64) (*TestRun, error)
	// ListIntelTestRuns returns test runs for a project, newest first.
	ListIntelTestRuns(ctx context.Context, projectID int64) ([]*TestRun, error)
	// UpdateIntelTestRun persists mutable run fields (status/timestamps/log).
	UpdateIntelTestRun(ctx context.Context, run *TestRun) error
	// AddIntelTestResults appends per-case results to a run.
	AddIntelTestResults(ctx context.Context, results []*TestResult) error
	// ListIntelTestResults returns results for a run.
	ListIntelTestResults(ctx context.Context, runID int64) ([]*TestResult, error)

	// ---- Test Intelligence: issues & features ----

	// CreateIntelIssue persists a new issue (closed-loop tracking).
	CreateIntelIssue(ctx context.Context, issue *IntelIssue) error
	// ListIntelIssues returns issues for a project, optionally by status.
	ListIntelIssues(ctx context.Context, projectID int64, status string) ([]*IntelIssue, error)
	// UpdateIntelIssue persists mutable issue fields (status/resolution).
	UpdateIntelIssue(ctx context.Context, issue *IntelIssue) error
	// ReplaceIntelFeatures replaces a project's feature-point set (rescan).
	ReplaceIntelFeatures(ctx context.Context, projectID int64, feats []*IntelFeature) error
	// ListIntelFeatures returns feature points for a project.
	ListIntelFeatures(ctx context.Context, projectID int64) ([]*IntelFeature, error)
	// UpdateIntelFeature persists mutable feature fields.
	UpdateIntelFeature(ctx context.Context, feat *IntelFeature) error

	// ---- Test Intelligence: knowledge base (RAG) ----

	// ReplaceProjectChunks rebuilds a project's knowledge-base vector chunks.
	ReplaceProjectChunks(ctx context.Context, projectID int64, chunks []*RagChunk) error
	// SearchRagChunks returns the chunks most similar to a query embedding.
	SearchRagChunks(ctx context.Context, projectID, moduleID int64, embedding []float32, limit int) ([]*RagChunk, error)
	// CountProjectChunks returns the number of indexed chunks for a project.
	CountProjectChunks(ctx context.Context, projectID int64) (int64, error)
	// DeleteProjectChunks removes a project's knowledge-base chunks.
	DeleteProjectChunks(ctx context.Context, projectID int64) error

	// ---- Test Intelligence: knowledge base Q&A (project-level chat) ----

	// CreateIntelChat persists a new conversation and populates its id.
	CreateIntelChat(ctx context.Context, c *IntelChat) error
	// ListIntelChats returns a project's conversations, newest first.
	ListIntelChats(ctx context.Context, projectID int64) ([]*IntelChat, error)
	// GetIntelChat loads a single conversation.
	GetIntelChat(ctx context.Context, id int64) (*IntelChat, error)
	// DeleteIntelChat removes a conversation and its messages.
	DeleteIntelChat(ctx context.Context, id int64) error
	// AddIntelChatMessage appends a message to a conversation.
	AddIntelChatMessage(ctx context.Context, m *IntelChatMessage) error
	// ListIntelChatMessages returns a conversation's messages, chronologically.
	ListIntelChatMessages(ctx context.Context, chatID int64, limit int) ([]*IntelChatMessage, error)
}

// Open opens a store for the given driver/dsn. It applies all migrations
// before returning, so the returned store is ready for use.
func Open(ctx context.Context, driver, dsn string) (Store, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	// SQLite has no connection pool concept; PG benefits from sensible limits.
	if driver == "postgres" {
		db.SetMaxOpenConns(10)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(time.Hour)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrate(ctx, driver, db); err != nil {
		db.Close()
		return nil, err
	}
	return &sqlStore{db: db, driver: driver}, nil
}

type sqlStore struct {
	db     *sql.DB
	driver string
}

// q rewrites a SQLite-style query into the driver's placeholder syntax.
func (s *sqlStore) q(query string) string { return rebind(s.driver, query) }

func (s *sqlStore) Close() error { return s.db.Close() }

func (s *sqlStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *sqlStore) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.db.QueryRowContext(ctx,
		s.q(`SELECT value FROM settings WHERE key = ?`),
		key,
	).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return val, err
}

func (s *sqlStore) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`),
		key, value,
	)
	return err
}

func (s *sqlStore) CreateToken(ctx context.Context, t *Token) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO tokens (id, name, token_hash, created_at, revoked_at, last_used)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP, NULL, NULL)`),
		t.ID, t.Name, t.TokenHash,
	)
	return err
}

func (s *sqlStore) GetTokenByHash(ctx context.Context, hash string) (*Token, error) {
	t := &Token{}
	var revoked *time.Time
	var lastUsed *time.Time
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, token_hash, created_at, revoked_at, last_used
		FROM tokens WHERE token_hash = ? AND revoked_at IS NULL LIMIT 1`),
		hash,
	).Scan(&t.ID, &t.Name, &t.TokenHash, &t.CreatedAt, &revoked, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	t.RevokedAt = revoked
	t.LastUsed = lastUsed
	return t, err
}

func (s *sqlStore) ListTokens(ctx context.Context) ([]*Token, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, token_hash, created_at, revoked_at, last_used
		FROM tokens ORDER BY created_at DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Token, 0)
	for rows.Next() {
		t := &Token{}
		var revoked *time.Time
		var lastUsed *time.Time
		if err := rows.Scan(&t.ID, &t.Name, &t.TokenHash, &t.CreatedAt, &revoked, &lastUsed); err != nil {
			return nil, err
		}
		t.RevokedAt = revoked
		t.LastUsed = lastUsed
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *sqlStore) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tokens SET revoked_at = CURRENT_TIMESTAMP WHERE id = ? AND revoked_at IS NULL`),
		id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) TouchToken(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tokens SET last_used = CURRENT_TIMESTAMP WHERE id = ?`),
		id,
	)
	return err
}
