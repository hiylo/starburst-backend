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
	TaskPending   = "pending"   // 等待前置依赖完成
	TaskBlocked   = "blocked"   // 前置依赖最终失败被阻塞
	TaskScheduled = "scheduled" // 已排期：等待 scheduled_at 到期，或周期模板（cron 非空）
)

// taskColumns is the canonical SELECT/RETURNING column list, kept in a single
// place so scanTask and every query stay in lockstep.
const taskColumns = "id, kind, session_id, directory, name, prompt, depends_on, status, error, result, progress, ai_summary, attempts, priority, timeout_seconds, workflow_id, created_at, updated_at, started_at, finished_at, available_at, scheduled_at, cron, last_fired_at"

// Task is an asynchronous orchestration job submitted by a client.
type Task struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`      // 任务类型："" = prompt（agent 编排任务，走 executor）；非空 = 内置追踪任务（如 test-run），不进 executor
	SessionID   string     `json:"sessionId"` // target OpenCode session id ("" = new session)
	Directory   string     `json:"directory"` // working directory hint for new sessions
	Name        string     `json:"name"`      // user-facing task name ("" = derive from prompt)
	Prompt      string     `json:"prompt"`
	DependsOn   string     `json:"dependsOn"` // 前置依赖任务 id（空表示无依赖）
	Status      string     `json:"status"`
	Error       string     `json:"error"`
	Result      string     `json:"result"`
	Progress    string     `json:"progress"`
	AISummary   string     `json:"aiSummary"` // LLM result summary (success) or root-cause analysis (failure)
	Attempts    int        `json:"attempts"`
	Priority    int        `json:"priority"`       // 0-100，越高越先执行（默认 50）
	TimeoutSec  int        `json:"timeoutSeconds"` // 单次执行超时（秒），0=不限制
	WorkflowID  string     `json:"workflowId"`     // 多步编排分组 id（空=独立任务）
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	StartedAt   *time.Time `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
	AvailableAt time.Time  `json:"availableAt"` // earliest time this task may be claimed (retry backoff)
	ScheduledAt *time.Time `json:"scheduledAt"` // 一次性排期：到点后转 queued（为空=立即）
	Cron        string     `json:"cron"`        // 周期模板：6 字段秒级 cron（非空时本任务不直接执行，到点克隆实体任务）
	LastFiredAt *time.Time `json:"lastFiredAt"` // 周期任务上次触发时间（用于计算下一次）
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
		INSERT INTO tasks (id, kind, session_id, directory, name, prompt, depends_on, status, error, result, progress, attempts, priority, timeout_seconds, workflow_id, created_at, updated_at, available_at, scheduled_at, cron)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '', '', 0, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, ?)`),
		t.ID, t.Kind, t.SessionID, t.Directory, t.Name, t.Prompt, t.DependsOn, status,
		t.Priority, t.TimeoutSec, t.WorkflowID, t.ScheduledAt, t.Cron,
	)
	return err
}

// ListTasks returns tasks ordered newest first, with an optional status filter
// and limit/offset pagination.
func (s *sqlStore) ListTasks(ctx context.Context, status string, limit, offset int) ([]*Task, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks`
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
	if offset > 0 {
		query += ` OFFSET ?`
		args = append(args, offset)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// CountTasks returns how many tasks match the optional status filter. Report it
// next to a limited ListTasks page so a client can tell whether more exist.
func (s *sqlStore) CountTasks(ctx context.Context, status string) (int, error) {
	query := `SELECT COUNT(*) FROM tasks`
	args := []any{}
	if status != "" {
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, s.q(query), args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// GetTask loads a single task.
func (s *sqlStore) GetTask(ctx context.Context, id string) (*Task, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT `+taskColumns+`
		FROM tasks WHERE id = ?`), id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// ClaimNextTask atomically picks the oldest queued and available prompt task
// (an empty kind) and marks it running. Each claim increments attempts, so
// retried tasks report their true run count. It is the prompt-worker variant of
// ClaimNextTaskOfKind and keeps the historical call contract for the executor's
// prompt workers.
func (s *sqlStore) ClaimNextTask(ctx context.Context) (*Task, error) {
	return s.ClaimNextTaskOfKind(ctx, "")
}

// ClaimNextTaskOfKind atomically picks the oldest queued and available task of
// the given kind and marks it running. Each claim increments attempts, so a
// retried task reports its true run count. An empty kind selects ordinary
// prompt tasks; non-empty kinds (e.g. "test-run") select built-in tracking
// tasks so a dedicated worker can drive them without stealing slots from
// prompt workers.
func (s *sqlStore) ClaimNextTaskOfKind(ctx context.Context, kind string) (*Task, error) {
	// Single statement keeps the claim atomic. The inner select differs by
	// dialect: SQLite serializes writers so a plain select is enough, while
	// PostgreSQL would let two concurrent claims read the same row before either
	// lock lands, so it uses FOR UPDATE SKIP LOCKED to let the second worker
	// take the next task instead.
	var query string
	if isPostgres(s.driver) {
		query = `
		UPDATE tasks SET status = ?, attempts = attempts + 1, started_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM tasks WHERE status = ? AND kind = ? AND available_at <= CURRENT_TIMESTAMP ORDER BY priority DESC, created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING ` + taskColumns
	} else {
		query = `
		UPDATE tasks SET status = ?, attempts = attempts + 1, started_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM tasks WHERE status = ? AND kind = ? AND available_at <= CURRENT_TIMESTAMP ORDER BY priority DESC, created_at ASC LIMIT 1
		)
		RETURNING ` + taskColumns
	}
	row := s.db.QueryRowContext(ctx, s.q(query), TaskRunning, TaskQueued, kind)
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

// UpdateTaskStatusByResult transitions a kind=test-run tracking task by its
// result column (which stores an associated intel run id). Deprecated for
// executor-driven tracking tasks: since run-all was wired into the shared task
// executor, the executor's CompleteTask/FailTask are the single writers of the
// task status, so this should no longer be called from the intel runner. Kept
// for callers that predate that migration.
func (s *sqlStore) UpdateTaskStatusByResult(ctx context.Context, result, status string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE result = ? AND kind = 'test-run'`), status, result)
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

// RetryTask re-queues a task for another attempt after backoffSecs, scheduling
// it at now+backoffSecs so the worker does not claim it again immediately.
// The status precondition accepts both "failed" (failed by FailTask, then re-run)
// and "running" (a worker executing this attempt decided to retry), since
// ClaimNextTask moves the row to running before execution starts. The next claim
// counts the new attempt; the attempts field is bumped by ClaimNextTask only, so
// it reflects the true number of executions.
func (s *sqlStore) RetryTask(ctx context.Context, id string, backoffSecs int) error {
	var query string
	if s.driver == "pgx" {
		query = `
			UPDATE tasks SET status = ?, error = '', finished_at = NULL,
				available_at = CURRENT_TIMESTAMP + (? || ' seconds')::interval, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status IN (?, ?)`
	} else {
		query = `
			UPDATE tasks SET status = ?, error = '', finished_at = NULL,
				available_at = datetime('now', '+' || ? || ' seconds'), updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND status IN (?, ?)`
	}
	res, err := s.db.ExecContext(ctx, s.q(query), TaskQueued, itoa(backoffSecs), id, TaskFailed, TaskRunning)
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
//
// The promotion is a single atomic UPDATE ... RETURNING: the SELECT-then-UPDATE
// split allowed a task inserted concurrently (after the read, before the write)
// to be permanently skipped. Both SQLite (>=3.35) and PostgreSQL support
// RETURNING, so one statement both claims and reports the promoted rows.
func (s *sqlStore) PromotePendingDependents(ctx context.Context, upstreamID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = '', available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE depends_on = ? AND status IN (?, ?)
		RETURNING id`),
		TaskQueued, upstreamID, TaskPending, TaskBlocked)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
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

// RequeueRunning marks a single running task back to queued (used when a worker
// is shutting down before it started executing a claimed task, so the task is
// not left stranded in running until the next restart).
func (s *sqlStore) RequeueRunning(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`),
		TaskQueued, id, TaskRunning)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListStuckRunning returns running tasks whose progress/updated_at has not
// moved since noProgressSince — i.e. the executor's busy-poll heartbeat is
// silent, which means the run is stuck (or the worker died mid-run).
func (s *sqlStore) ListStuckRunning(ctx context.Context, noProgressSince time.Time) ([]*Task, error) {
	// updated_at is stored as CURRENT_TIMESTAMP text (UTC without zone); pass
	// the cutoff in the same format so the comparison is not thrown off by the
	// driver's timezone formatting.
	cutoff := noProgressSince.UTC().Format("2006-01-02 15:04:05")
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT `+taskColumns+` FROM tasks
		WHERE status = ? AND updated_at < ? ORDER BY updated_at ASC`), TaskRunning, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// RequeueTask manually re-queues a terminal task (failed/canceled) for another
// run immediately (available_at = now), clearing the error. Returns false when
// the task is not in a terminal state.
func (s *sqlStore) RequeueTask(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = '', finished_at = NULL,
			available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?)`),
		TaskQueued, id, TaskFailed, TaskCanceled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CancelTasks batch-cancels non-terminal tasks (queued/pending/running/
// retrying). Returns how many were actually transitioned.
func (s *sqlStore) CancelTasks(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+5)
	args = append(args, TaskCanceled)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, TaskQueued, TaskPending, TaskRunning, "retrying")
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id IN (`+placeholders+`) AND status IN (?, ?, ?, ?)`), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RequeueTasks batch-requeues terminal tasks (failed/canceled) immediately.
// Returns how many were actually transitioned.
func (s *sqlStore) RequeueTasks(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, 3+len(ids))
	args = append(args, TaskQueued)
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, TaskFailed, TaskCanceled)
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, error = '', finished_at = NULL,
			available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id IN (`+placeholders+`) AND status IN (?, ?)`), args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CancelWorkflow cancels every non-terminal task of an orchestration.
func (s *sqlStore) CancelWorkflow(ctx context.Context, workflowID string) (int, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE workflow_id = ? AND status IN (?, ?, ?, ?)`),
		TaskCanceled, workflowID, TaskQueued, TaskPending, TaskRunning, "retrying")
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RerunWorkflowFromFailed restarts an orchestration from its first finished
// step that did not succeed (failed/canceled/blocked): that step is re-queued
// (full retry budget) and every later finished step returns to pending, waiting
// on the chain. Already-succeeded steps are kept. Steps still in progress
// (queued/running/scheduled/retrying/pending) are left untouched so a mid-run
// rerun never double-executes a live step. Returns how many steps were reset.
func (s *sqlStore) RerunWorkflowFromFailed(ctx context.Context, workflowID string) (int, error) {
	tasks, err := s.ListTasksByWorkflow(ctx, workflowID)
	if err != nil {
		return 0, err
	}
	n := 0
	prevSucceeded := true
	for _, t := range tasks {
		switch t.Status {
		case TaskSucceeded:
			prevSucceeded = true
		case TaskRunning, TaskQueued, TaskScheduled, "retrying", TaskPending:
			// 进行中/未被上游阻塞最终化：不动它；其下游仍需等待。
			prevSucceeded = false
		default: // failed / canceled / blocked → 从这里恢复
			status := TaskQueued
			if !prevSucceeded {
				status = TaskPending
			}
			if _, err := s.db.ExecContext(ctx, s.q(`
				UPDATE tasks SET status = ?, error = '', result = '', progress = '',
					attempts = 0, started_at = NULL, finished_at = NULL,
					available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`), status, t.ID); err != nil {
				return n, err
			}
			n++
			prevSucceeded = false
		}
	}
	return n, nil
}

// ListDependentsOf returns the tasks waiting on upstreamID (for the dependency
// view in a task's detail).
func (s *sqlStore) ListDependentsOf(ctx context.Context, upstreamID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT `+taskColumns+` FROM tasks WHERE depends_on = ? ORDER BY created_at ASC`), upstreamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ListTasksByWorkflow returns all tasks sharing a workflow id (oldest first),
// used by the orchestration page to render the chained steps with live status.
func (s *sqlStore) ListTasksByWorkflow(ctx context.Context, workflowID string) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT `+taskColumns+` FROM tasks WHERE workflow_id = ? ORDER BY created_at ASC`), workflowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// WorkflowSummary is one orchestration's rollup shown in the orchestration page.
type WorkflowSummary struct {
	WorkflowID string    `json:"workflowId"`
	Name       string    `json:"name"`
	Steps      int       `json:"steps"`
	Succeeded  int       `json:"succeeded"`
	Failed     int       `json:"failed"`
	Running    int       `json:"running"` // queued/running/pending/retrying
	CreatedAt  time.Time `json:"createdAt"`
}

// ListWorkflowSummaries returns recent multi-step orchestrations (newest
// first) with a step status rollup for the orchestration page.
func (s *sqlStore) ListWorkflowSummaries(ctx context.Context, limit int) ([]*WorkflowSummary, error) {
	if limit <= 0 {
		limit = 50
	}
	sum := func(status string) string {
		return `SUM(CASE WHEN status = '` + status + `' THEN 1 ELSE 0 END)`
	}
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT workflow_id,
			(SELECT name FROM tasks t2 WHERE t2.workflow_id = t.workflow_id ORDER BY created_at ASC LIMIT 1) AS name,
			COUNT(*) AS steps,
			`+sum(TaskSucceeded)+` AS succeeded,
			`+sum(TaskFailed)+` AS failed,
			`+sum(TaskRunning)+`+`+sum(TaskQueued)+`+`+sum(TaskPending)+`+`+sum("retrying")+` AS running,
			MIN(created_at) AS created_at
		FROM tasks t WHERE workflow_id <> '' GROUP BY workflow_id
		ORDER BY MIN(created_at) DESC LIMIT `+itoa(limit)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*WorkflowSummary, 0)
	for rows.Next() {
		var ws WorkflowSummary
		if err := rows.Scan(&ws.WorkflowID, &ws.Name, &ws.Steps, &ws.Succeeded, &ws.Failed, &ws.Running, &ws.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &ws)
	}
	return out, rows.Err()
}

// TaskStatsWindow aggregates task outcomes over the trailing windowDays days:
// per-status counts, success rate and average run duration, plus a per-day
// trend (created vs succeeded/failed) for the overview chart.
type TaskStatsWindow struct {
	StatusCounts   map[string]int `json:"statusCounts"`
	Total          int            `json:"total"`
	Completed      int            `json:"completed"`
	Succeeded      int            `json:"succeeded"`
	Failed         int            `json:"failed"`
	SuccessRate    float64        `json:"successRate"` // 0..1
	AvgDurationSec float64        `json:"avgDurationSec"`
	Trend          []DayTrend     `json:"trend"`
}

// DayTrend is one day bucket of the task volume/success trend.
type DayTrend struct {
	Day       string `json:"day"` // YYYY-MM-DD
	Created   int    `json:"created"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}

// TaskStatsDetailed computes the task statistics window for the stats page.
func (s *sqlStore) TaskStatsDetailed(ctx context.Context, windowDays int) (*TaskStatsWindow, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	out := &TaskStatsWindow{StatusCounts: map[string]int{}}
	var cutoff string
	if isPostgres(s.driver) {
		cutoff = "created_at >= CURRENT_TIMESTAMP - (? || ' days')::interval"
	} else {
		cutoff = "created_at >= datetime('now', '-' || ? || ' days')"
	}
	// Per-status counts.
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT status, COUNT(*) FROM tasks WHERE `+cutoff+` GROUP BY status`), itoa(windowDays))
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			rows.Close()
			return nil, err
		}
		out.StatusCounts[st] = n
		out.Total += n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Success/failure + avg duration over finished tasks.
	if err := s.db.QueryRowContext(ctx, s.q(`
		SELECT
			COUNT(*) FILTER (WHERE status = ?),
			COUNT(*) FILTER (WHERE status = ?),
			AVG(EXTRACT(EPOCH FROM (finished_at - started_at)))
		FROM tasks
		WHERE `+cutoff+` AND status IN (?, ?) AND started_at IS NOT NULL AND finished_at IS NOT NULL`),
		TaskSucceeded, TaskFailed, itoa(windowDays), TaskSucceeded, TaskFailed).Scan(
		&out.Succeeded, &out.Failed, &out.AvgDurationSec); err != nil {
		return nil, err
	}
	out.Completed = out.Succeeded + out.Failed
	if out.Completed > 0 {
		out.SuccessRate = float64(out.Succeeded) / float64(out.Completed)
	}
	// Per-day trend for the trailing window.
	var dayExpr string
	if isPostgres(s.driver) {
		dayExpr = "to_char(created_at, 'YYYY-MM-DD')"
	} else {
		dayExpr = "strftime('%Y-%m-%d', created_at)"
	}
	rows2, err := s.db.QueryContext(ctx, s.q(`
		SELECT `+dayExpr+`,
			COUNT(*),
			COUNT(*) FILTER (WHERE status = ?),
			COUNT(*) FILTER (WHERE status = ?)
		FROM tasks WHERE `+cutoff+` GROUP BY `+dayExpr+` ORDER BY `+dayExpr+` ASC`),
		TaskSucceeded, TaskFailed, itoa(windowDays))
	if err != nil {
		return nil, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var d DayTrend
		if err := rows2.Scan(&d.Day, &d.Created, &d.Succeeded, &d.Failed); err != nil {
			return nil, err
		}
		out.Trend = append(out.Trend, d)
	}
	return out, rows2.Err()
}

// ListScheduledTasks returns all tasks currently in the scheduled state
// (both one-shot future tasks and recurring cron templates).
func (s *sqlStore) ListScheduledTasks(ctx context.Context) ([]*Task, error) {
	rows, err := s.db.QueryContext(ctx,
		s.q(`SELECT `+taskColumns+` FROM tasks WHERE status = ? ORDER BY created_at ASC`), TaskScheduled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// PromoteScheduledTask flips a scheduled one-shot task to queued so the worker
// can claim it. Returns false when the task is not in scheduled state.
func (s *sqlStore) PromoteScheduledTask(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, available_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`),
		TaskQueued, id, TaskScheduled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetTaskLastFiredAt records the last time a recurring cron template fired.
func (s *sqlStore) SetTaskLastFiredAt(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tasks SET last_fired_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), at, id)
	return err
}

// SetTaskLastFiredAtPtr sets the cursor to at, or to NULL when at is nil (used
// to roll back a claimed fire whose clone failed, restoring the pre-claim state
// so the next scheduler tick retries the occurrence).
func (s *sqlStore) SetTaskLastFiredAtPtr(ctx context.Context, id string, at *time.Time) error {
	_, err := s.db.ExecContext(ctx,
		s.q(`UPDATE tasks SET last_fired_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`), at, id)
	return err
}

// ClaimRecurringFire atomically advances the last_fired_at cursor of a recurring
// template to `now`, but only if it still equals `expected` (nil means "never
// fired"). Returns true only when this caller won the race; a concurrent
// scheduler that already advanced the cursor gets false and must skip its tick,
// otherwise two schedulers would each clone the task for the same due time.
func (s *sqlStore) ClaimRecurringFire(ctx context.Context, id string, expected *time.Time, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET last_fired_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ? AND
		      ((last_fired_at IS NULL AND ? IS NULL) OR last_fired_at = ?)`),
		now, id, TaskScheduled, expected, expected)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CancelScheduledTask marks a scheduled (one-shot or recurring) task canceled.
func (s *sqlStore) CancelScheduledTask(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE tasks SET status = ?, finished_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status = ?`),
		TaskCanceled, id, TaskScheduled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row rowScanner) (*Task, error) {
	t := &Task{}
	var started, finished, scheduled, lastFired *time.Time
	err := row.Scan(&t.ID, &t.Kind, &t.SessionID, &t.Directory, &t.Name, &t.Prompt, &t.DependsOn, &t.Status,
		&t.Error, &t.Result, &t.Progress, &t.AISummary, &t.Attempts, &t.Priority, &t.TimeoutSec, &t.WorkflowID,
		&t.CreatedAt, &t.UpdatedAt, &started, &finished, &t.AvailableAt, &scheduled, &t.Cron, &lastFired)
	if err != nil {
		return nil, err
	}
	t.StartedAt = started
	t.FinishedAt = finished
	t.ScheduledAt = scheduled
	t.LastFiredAt = lastFired
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

// PurgeFinishedTasks deletes succeeded/failed/canceled tasks whose updated_at is
// older than olderThan, bounded by limit rows per pass. A finished task that is
// still referenced by an ACTIVE (non-terminal) dependent is kept so no live
// dependent points at a missing upstream; a task referenced only by other
// terminal tasks is deleted alongside them. Non-terminal tasks are never
// touched. The second return value reports how many terminal tasks were kept
// because an active dependent still references them.
func (s *sqlStore) PurgeFinishedTasks(ctx context.Context, olderThan time.Duration, limit int) (int, int, error) {
	if limit <= 0 {
		limit = 500
	}
	secs := int(olderThan.Seconds())
	// The timestamp column holds CURRENT_TIMESTAMP output, so the cutoff must be
	// computed in SQL with the same format rather than passed as a time.Time.
	var cutoff, recent string
	if isPostgres(s.driver) {
		cutoff = "updated_at < CURRENT_TIMESTAMP - (? || ' seconds')::interval"
		recent = "CURRENT_TIMESTAMP - (? || ' seconds')::interval"
	} else {
		cutoff = "updated_at < datetime('now', '-' || ? || ' seconds')"
		recent = "datetime('now', '-' || ? || ' seconds')"
	}
	// activeStatuses lists statuses that still need their upstream to exist.
	activeStatuses := []any{TaskQueued, TaskRunning, TaskPending, TaskBlocked, TaskScheduled}
	statuses := []any{TaskSucceeded, TaskFailed, TaskCanceled}
	activeSet := "?, ?, ?, ?, ?"
	args := make([]any, 0, 10)
	args = append(args, statuses[0], statuses[1], statuses[2], itoa(secs))
	args = append(args, activeStatuses...)
	args = append(args, itoa(secs))
	// A terminal step of a still-recent orchestration is kept (NOT deleted) so
	// the workflow page keeps showing it until the whole workflow ages out; the
	// per-group subquery uses the same retention window.
	workflowKeep := `AND NOT (workflow_id <> '' AND EXISTS (
		SELECT 1 FROM tasks wg WHERE wg.workflow_id = tasks.workflow_id AND wg.updated_at >= ` + recent + `))`
	// The limit lives in the inner select: SQLite refuses DELETE ... LIMIT. The
	// seconds are bound as text (pgx cannot encode an int where || expects
	// text) and the limit is inlined like the other paged queries.
	res, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM tasks
		WHERE id IN (
			SELECT id FROM tasks
			WHERE status IN (?, ?, ?) AND `+cutoff+`
			  AND id NOT IN (SELECT depends_on FROM tasks WHERE depends_on <> '' AND status IN (`+activeSet+`))
			  `+workflowKeep+`
			ORDER BY updated_at ASC
			LIMIT `+itoa(limit)+`
		)`), args...)
	if err != nil {
		return 0, 0, err
	}
	deleted, _ := res.RowsAffected()

	var kept int
	keptArgs := append([]any{}, statuses[0], statuses[1], statuses[2], itoa(secs))
	keptArgs = append(keptArgs, activeStatuses...)
	if err := s.db.QueryRowContext(ctx, s.q(`
		SELECT COUNT(*) FROM tasks
		WHERE status IN (?, ?, ?) AND `+cutoff+`
		  AND id IN (SELECT depends_on FROM tasks WHERE depends_on <> '' AND status IN (`+activeSet+`))`),
		keptArgs...).Scan(&kept); err != nil {
		return 0, 0, err
	}
	return int(deleted), kept, nil
}
