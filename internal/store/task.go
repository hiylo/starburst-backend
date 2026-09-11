package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Task status lifecycle.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskCanceled  = "canceled"
	TaskPending   = "pending" // 等待前置依赖完成
	TaskBlocked   = "blocked" // 前置依赖最终失败被阻塞
)

// Task is an asynchronous orchestration job submitted by a client.
type Task struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"sessionId"` // target OpenCode session id ("" = new session)
	Directory   string     `json:"directory"` // working directory hint for new sessions
	Prompt      string     `json:"prompt"`
	DependsOn   string     `json:"dependsOn"` // 前置依赖任务 id（空表示无依赖）
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	Result      string     `json:"result"`
	Progress    string     `json:"progress"`
	AISummary   string     `json:"aiSummary"` // LLM result summary (success) or root-cause analysis (failure)
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	StartedAt   *time.Time `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	AvailableAt time.Time  `json:"availableAt"` // earliest time this task may be claimed (retry backoff)
}

// CreateTask persists a queued task.
func (s *sqlStore) CreateTask(ctx context.Context, t *Task) error {
	return s.CreateTaskWithStatus(ctx, t, TaskQueued)
}

// CreateTaskWithStatus persists a task with an explicit initial status,
// used to enqueue a task either directly (queued) or waiting on a dependency
// (pending). The depends_on column records the upstream task id when present.
func (s *sqlStore) CreateTaskWithStatus(ctx context.Context, t *Task, status string) error {
	t.Status = status
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO tasks (id, session_id, directory, prompt, depends_on, status, error, result, progress, attempts, created_at, updated_at, available_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', '', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		t.ID, t.SessionID, t.Directory, t.Prompt, t.DependsOn, status,
	)
	return err
}

// ListTasks returns tasks ordered newest first, with an optional status filter.
func (s *sqlStore) ListTasks(ctx context.Context, status string, limit int) ([]*Task, error) {
	query := `SELECT id, session_id, directory, prompt, depends_on, status, error, result, progress, ai_summary, attempts, created_at, updated_at, started_at, finished_at, available_at FROM tasks`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// GetTask loads a single task.
func (s *sqlStore) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, session_id, directory, prompt, depends_on, status, error, result, progress, ai_summary, attempts, created_at, updated_at, started_at, finished_at, available_at
		FROM tasks WHERE id = ?`), id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ClaimNextTask atomically picks the oldest queued and available task.
// Each claim increments attempts, so retried tasks report their true run count.
func (s *sqlStore) ClaimNextTask(ctx context.Context) (*Task, error) {
	// Single statement keeps it atomic enough for a single-node backend.
	row := s.db.QueryRowContext(ctx, s.q(`
		UPDATE tasks SET status = ?, attempts = attempts + 1, started_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM tasks WHERE status = ? AND available_at <= CURRENT_TIMESTAMP ORDER BY created_at ASC LIMIT 1
		)
		RETURNING id, session_id, directory, prompt, depends_on, status, error, result, progress, ai_summary, attempts, created_at, updated_at, started_at, finished_at, available_at`),
		TaskRunning, TaskQueued)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// UpdateTaskProgress records a progress note for a running task.
func (s *sqlStore) UpdateTaskProgress(ctx context.Context, id, progress string) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tasks SET progress = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), progress, id)
	return err
}

// SetTaskSession records the resolved session id for a task.
func (s *sqlStore) SetTaskSession(ctx context.Context, id, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tasks SET session_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), sessionID, id)
	return err
}

// CompleteTask marks a task as succeeded with a result.
func (s *sqlStore) CompleteTask(ctx context.Context, id, result string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, result = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`), TaskSucceeded, result, id)
	return err
}

// SetTaskAISummary records the LLM-generated summary or failure analysis for a
// task. It is advisory metadata and never changes the task status.
func (s *sqlStore) SetTaskAISummary(ctx context.Context, id, summary string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET ai_summary = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), summary, id)
	return err
}

// FailTask marks a task as failed.
func (s *sqlStore) FailTask(ctx context.Context, id, errMsg string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`), TaskFailed, errMsg, id)
	return err
}

// RetryTask re-queues a failed task for another attempt, scheduling it at
// now+backoffSecs so the worker does not claim it again immediately.
// The next claim counts the new attempt; the attempts field is bumped by
// ClaimNextTask only, so it reflects the true number of executions.
func (s *sqlStore) RetryTask(ctx context.Context, id string, backoffSecs int) error {
	var query string
	if s.driver == "pgx" {
		query = `
			UPDATE tasks SET status = ?, error = '', finished_at = NULL,
				available_at = CURRENT_TIMESTAMP + (? || ' seconds')::interval, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`
	} else {
		query = `
			UPDATE tasks SET status = ?, error = '', finished_at = NULL,
				available_at = datetime('now', '+' || ? || ' seconds'), updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status = ?`
	}
	res, err := s.db.ExecContext(ctx, s.q(query), TaskQueued, backoffSecs, id, TaskFailed)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// CancelTask marks a queued/pending/running task as canceled.
func (s *sqlStore) CancelTask(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?, ?)`),
		TaskCanceled, id, TaskQueued, TaskPending, TaskRunning)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// PromotePendingDependents re-queues every task waiting on upstreamID so it can
// finally run. Both "not started yet" states are covered: pending (the upstream
// was still running when this task was created) and blocked (the upstream had
// already failed or was canceled, but was later retried and succeeded). The
// stored block reason is cleared. Returns the ids of the re-queued tasks so the
// caller can push a per-task event.
func (s *sqlStore) PromotePendingDependents(ctx context.Context, upstreamID string) ([]string, error) {
	ids, err := s.dependentIDs(ctx, upstreamID, TaskPending, TaskBlocked)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return ids, nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = '', available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE depends_on = ? AND status IN (?, ?)`),
		TaskQueued, upstreamID, TaskPending, TaskBlocked); err != nil {
		return nil, err
	}
	return ids, nil
}

// BlockDependents marks every pending task waiting on upstreamID as blocked,
// since its upstream did not succeed and it cannot run as-is. reason is recorded
// in the error column for visibility. Returns the ids of the blocked tasks.
func (s *sqlStore) BlockDependents(ctx context.Context, upstreamID, reason string) ([]string, error) {
	ids, err := s.dependentIDs(ctx, upstreamID, TaskPending)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return ids, nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE depends_on = ? AND status = ?`),
		TaskBlocked, reason, upstreamID, TaskPending); err != nil {
		return nil, err
	}
	return ids, nil
}

// UnblockTask manually re-queues a blocked task, for example after a human fixed
// the upstream problem directly without re-running the upstream itself. Returns
// false when the task is not in blocked state.
func (s *sqlStore) UnblockTask(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = '', finished_at = NULL,
			available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`),
		TaskQueued, id, TaskBlocked)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// dependentIDs lists the ids of the dependents of upstreamID that are currently
// in one of the given states. Callers use it to fan out a per-task push event.
func (s *sqlStore) dependentIDs(ctx context.Context, upstreamID string, states ...string) ([]string, error) {
	if len(states) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states)+1)
	args = append(args, upstreamID)
	for _, st := range states {
		args = append(args, st)
	}
	rows, err := s.db.QueryContext(ctx,
		s.q("SELECT id FROM tasks WHERE depends_on = ? AND status IN ("+placeholders+")"), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// IsTaskCanceled reports whether a task is currently canceled.
func (s *sqlStore) IsTaskCanceled(ctx context.Context, id string) (bool, error) {
	var status string
	err := s.db.QueryRowContext(ctx, s.q(`SELECT status FROM tasks WHERE id = ?`), id).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	return status == TaskCanceled, nil
}

// RecoverStaleRunning resets tasks stuck in running state back to queued.
// Called on startup so tasks interrupted by a crash/restart are re-run.
func (s *sqlStore) RecoverStaleRunning(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, started_at = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE status = ?`),
		TaskQueued, TaskRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	t := &Task{}
	var started, finished *time.Time
	err := row.Scan(&t.ID, &t.SessionID, &t.Directory, &t.Prompt, &t.DependsOn, &t.Status,
		&t.Error, &t.Result, &t.Progress, &t.AISummary, &t.Attempts, &t.CreatedAt, &t.UpdatedAt, &started, &finished, &t.AvailableAt)
	if err != nil {
		return nil, err
	}
	t.StartedAt = started
	t.FinishedAt = finished
	return t, err
}

func scanTasks(rows *sql.Rows) ([]*Task, error) {
	out := make([]*Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
