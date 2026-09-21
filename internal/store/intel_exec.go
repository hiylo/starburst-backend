package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// TestCase is a discovered test asset (unit/integration/e2e/…), classified by
// the scanner into kind/framework/class/method and kept for execution and
// flaky tracking. Tags is a JSON array of string tags.
type TestCase struct {
	ID             int64      `json:"id"`
	ProjectID      int64      `json:"projectId"`
	ModuleID       int64      `json:"moduleId"`
	Module         string     `json:"module"`
	Kind           string     `json:"kind"`
	Framework      string     `json:"framework"`
	Class          string     `json:"class"`
	Method         string     `json:"method"`
	Path           string     `json:"path"`
	Tags           string     `json:"tags"`
	LastStatus     string     `json:"lastStatus"`
	LastDurationMs int64      `json:"lastDurationMs"`
	FlakyCount     int        `json:"flakyCount"`
	Quarantined    int        `json:"quarantined"`
	LastRunAt      *time.Time `json:"lastRunAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// flakyQuarantineThreshold is the number of times a case must first fail then
// pass (flaky) before it is quarantined and excluded from flaky retries and
// issue creation.
const flakyQuarantineThreshold = 3

// TestRun is one test execution (scope/kind/command), owned by a project/module.
type TestRun struct {
	ID         int64      `json:"id"`
	ProjectID  int64      `json:"projectId"`
	ModuleID   int64      `json:"moduleId"`
	Scope      string     `json:"scope"`
	Kind       string     `json:"kind"`
	Command    string     `json:"command"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	LogPath    string     `json:"logPath"`
	Progress   string     `json:"progress,omitempty"`
	Output     string     `json:"output,omitempty"`
	Priority   int        `json:"priority"` // 0-100 调度权重，越高越先执行（默认 0）
	CreatedAt  time.Time  `json:"createdAt"`
}

// TestResult is one per-case outcome of a test run.
type TestResult struct {
	ID            int64     `json:"id"`
	RunID         int64     `json:"runId"`
	ProjectID     int64     `json:"projectId"`
	ModuleID      int64     `json:"moduleId"`
	CaseID        int64     `json:"caseId"`
	Kind          string    `json:"kind"`
	Endpoint      string    `json:"endpoint"`
	Passed        bool      `json:"passed"`
	FailuresJSON  string    `json:"failuresJson"`
	RootcauseJSON string    `json:"rootcauseJson"`
	CreatedAt     time.Time `json:"createdAt"`
}

// IntelIssue is one problem tracked through the closed-loop: found at a commit,
// re-checked on later commits, resolved when its targeted case goes green.
type IntelIssue struct {
	ID          int64      `json:"id"`
	ProjectID   int64      `json:"projectId"`
	ModuleID    int64      `json:"moduleId"`
	FeatureID   int64      `json:"featureId"`
	Key         string     `json:"key"`
	Kind        string     `json:"kind"`
	Severity    string     `json:"severity"`
	Location    string     `json:"location"`
	CommitSeen  string     `json:"commitSeen"`
	CommitFixed string     `json:"commitFixed"`
	Status      string     `json:"status"`
	ResolvedAt  *time.Time `json:"resolvedAt"`
	LastCheckAt *time.Time `json:"lastCheckAt"`
	DetailJSON  string     `json:"detailJson"`
	CreatedAt   time.Time  `json:"createdAt"`
}

// IntelFeature is a business feature unit (clustered endpoints) that hosts
// feature-level tests, attaches integration bugs and carries AI-chat context.
type IntelFeature struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"projectId"`
	Name      string    `json:"name"`
	Summary   string    `json:"summary"`
	EndsJSON  string    `json:"endsJson"`
	SortOrder int       `json:"sortOrder"`
	Source    string    `json:"source"`
	Anchor    string    `json:"anchor"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ReplaceIntelTestCases deletes a module's test cases and re-inserts the given
// set, so a scan reflects the current repository layout.
func (s *sqlStore) ReplaceIntelTestCases(ctx context.Context, projectID int64, cases []*TestCase) error {
	// 空列表保护：同 ReplaceIntelEntities——无结果保留旧快照，避免误清空。
	if len(cases) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM test_cases WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, c := range cases {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO test_cases (project_id, module_id, module, kind, framework, class, method,
				path, tags, last_status, last_duration_ms, flaky_count, last_run_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, c.ModuleID, c.Module, c.Kind, c.Framework, c.Class, c.Method,
			c.Path, c.Tags, c.LastStatus, c.LastDurationMs, c.FlakyCount, c.LastRunAt); err != nil {
			return err
		}
	}
	return nil
}

// DeleteIntelModuleTestCases removes one module's test cases.
func (s *sqlStore) DeleteIntelModuleTestCases(ctx context.Context, projectID, moduleID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM test_cases WHERE project_id = ? AND module_id = ?`), projectID, moduleID)
	return err
}

// AppendIntelTestCases inserts test cases without wiping the rest.
func (s *sqlStore) AppendIntelTestCases(ctx context.Context, projectID int64, cases []*TestCase) error {
	for _, c := range cases {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO test_cases (project_id, module_id, module, kind, framework, class, method,
				path, tags, last_status, last_duration_ms, flaky_count, last_run_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, c.ModuleID, c.Module, c.Kind, c.Framework, c.Class, c.Method,
			c.Path, c.Tags, c.LastStatus, c.LastDurationMs, c.FlakyCount, c.LastRunAt); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelTestCases returns test cases for a project (optionally narrowed to a
// module).
func (s *sqlStore) ListIntelTestCases(ctx context.Context, projectID, moduleID int64) ([]*TestCase, error) {
	query := `SELECT id, project_id, module_id, module, kind, framework, class, method,
		path, tags, last_status, last_duration_ms, flaky_count, quarantined, last_run_at, created_at
		FROM test_cases WHERE project_id = ?`
	args := []any{projectID}
	if moduleID > 0 {
		query += ` AND module_id = ?`
		args = append(args, moduleID)
	}
	query += ` ORDER BY path, class, method`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*TestCase, 0)
	for rows.Next() {
		c := &TestCase{}
		var lastRun *time.Time
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ModuleID, &c.Module, &c.Kind, &c.Framework,
			&c.Class, &c.Method, &c.Path, &c.Tags, &c.LastStatus, &c.LastDurationMs,
			&c.FlakyCount, &c.Quarantined, &lastRun, &c.CreatedAt); err != nil {
			return nil, err
		}
		c.LastRunAt = lastRun
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateIntelTestCaseOutcome records a run outcome on the matching test case
// (matched by module+method, plus class when non-empty) and bumps flaky_count
// when the outcome is marked flaky. It updates at most the first matching row so
// the asset list stays deterministic. Matching by module_id prevents a
// same-named class+method in another module from swallowing the outcome.
func (s *sqlStore) UpdateIntelTestCaseOutcome(ctx context.Context, projectID, moduleID int64, class, method string, passed bool, durationMs int64, flaky bool) error {
	if method == "" {
		return nil
	}
	status := "failed"
	if passed {
		status = "passed"
	}
	flakyInt := 0
	if flaky {
		flakyInt = 1
	}
	// quarantined 在 flaky_count 累计达到阈值时置 1（用更新后的值判断），
	// 隔离的用例不再参与 flaky 重跑与 issue 生成。
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE test_cases SET last_status = ?, last_duration_ms = ?,
			flaky_count = flaky_count + ?,
			quarantined = CASE WHEN flaky_count + ? >= ? THEN 1 ELSE quarantined END,
			last_run_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM test_cases WHERE project_id = ? AND module_id = ? AND method = ? AND (? = '' OR class = ?)
			ORDER BY id LIMIT 1
		)`), status, durationMs, flakyInt, flakyInt, flakyQuarantineThreshold, projectID, moduleID, method, class, class)
	return err
}

// ListQuarantinedIntelTestCases returns the endpoints (class.method) of quarantined
// cases for a module, keyed by the endpoint string used in TestResult.Endpoint.
func (s *sqlStore) ListQuarantinedIntelTestCases(ctx context.Context, projectID, moduleID int64) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT class, method FROM test_cases WHERE project_id = ? AND module_id = ? AND quarantined = 1`),
		projectID, moduleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var class, method string
		if err := rows.Scan(&class, &method); err != nil {
			return nil, err
		}
		out[class+"."+method] = true
	}
	return out, rows.Err()
}

// UnquarantineIntelTestCase clears the quarantine flag and flaky counter for a
// case, restoring it to normal participation in runs and issue tracking.
func (s *sqlStore) UnquarantineIntelTestCase(ctx context.Context, projectID, moduleID int64, class, method string) error {
	if method == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE test_cases SET quarantined = 0, flaky_count = 0
		WHERE project_id = ? AND module_id = ? AND method = ? AND (? = '' OR class = ?)`),
		projectID, moduleID, method, class, class)
	return err
}

// CreateIntelTestRun persists a new run and populates its auto-generated id.
func (s *sqlStore) CreateIntelTestRun(ctx context.Context, run *TestRun) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO test_runs (project_id, module_id, scope, kind, command, status,
				attempts, started_at, finished_at, log_path, progress, output, priority, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			run.ProjectID, run.ModuleID, run.Scope, run.Kind, run.Command, run.Status,
			run.Attempts, run.StartedAt, run.FinishedAt, run.LogPath, run.Progress, run.Output, run.Priority,
		).Scan(&run.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO test_runs (project_id, module_id, scope, kind, command, status,
			attempts, started_at, finished_at, log_path, progress, output, priority, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		run.ProjectID, run.ModuleID, run.Scope, run.Kind, run.Command, run.Status,
		run.Attempts, run.StartedAt, run.FinishedAt, run.LogPath, run.Progress, run.Output, run.Priority,
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	run.ID = id
	return nil
}

// GetIntelTestRun loads a single test run.
func (s *sqlStore) GetIntelTestRun(ctx context.Context, id int64) (*TestRun, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, module_id, scope, kind, command, status, attempts,
			started_at, finished_at, log_path, progress, output, priority, created_at FROM test_runs WHERE id = ?`), id)
	run, err := scanIntelTestRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return run, err
}

// ListIntelTestRuns returns test runs for a project, newest first. The output
// column is omitted (empty) to keep list payloads small; fetch a single run for
// the full log.
func (s *sqlStore) ListIntelTestRuns(ctx context.Context, projectID int64) ([]*TestRun, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, module_id, scope, kind, command, status, attempts,
			started_at, finished_at, log_path, progress, '', priority, created_at
		FROM test_runs WHERE project_id = ? ORDER BY created_at DESC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*TestRun, 0)
	for rows.Next() {
		run, err := scanIntelTestRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

// UpdateIntelTestRun persists mutable run fields. An empty progress/output keeps
// the existing stored values; pass values to overwrite them.
func (s *sqlStore) UpdateIntelTestRun(ctx context.Context, run *TestRun) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE test_runs SET status = ?, started_at = ?, finished_at = ?, log_path = ?,
			command = COALESCE(?, command), progress = COALESCE(?, progress), output = COALESCE(?, output) WHERE id = ?`),
		run.Status, run.StartedAt, run.FinishedAt, run.LogPath, run.Command, run.Progress, run.Output, run.ID)
	return err
}

// FailStaleIntelRuns marks runs left in running/queued from a previous process
// that exited before finishing (a restart, crash or cancellation) as failed.
// Without this, those rows hang forever as running/queued with no execution
// behind them. olderThan guards the fresh rows created by a concurrently
// starting run loop; the fixed 20-second window is long enough that no live run
// is ever older than it at startup.
func (s *sqlStore) FailStaleIntelRuns(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE test_runs SET status = 'failed',
			finished_at = COALESCE(finished_at, CURRENT_TIMESTAMP),
			progress = '进程重启，运行被中断（启动恢复标记为失败）'
		WHERE status IN ('running', 'queued') AND created_at < ?`), olderThan)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return n, err
}

// PurgeOldIntelTestRuns deletes terminal test runs whose finished_at is older
// than olderThan, bounded by limit rows per pass so a backlogged install drains
// over a few passes instead of one long transaction. finished_at < olderThan
// already excludes never-finished rows (NULL), so queued/running runs are never
// touched; the DELETE targets the oldest ids first. Identical SQL runs on both
// SQLite and PostgreSQL. Returns the number of rows deleted.
func (s *sqlStore) PurgeOldIntelTestRuns(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	// The candidate set is the same bounded subquery for both deletes, and both
	// run in one transaction so the per-case results of a purged run never linger
	// as orphans. The limit lives in the inner select: SQLite refuses DELETE ...
	// LIMIT, and PostgreSQL needs the same shape for a deterministic batch.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, s.q(`
		DELETE FROM test_results WHERE run_id IN (
			SELECT id FROM test_runs WHERE finished_at < ?
			ORDER BY id LIMIT `+itoa(limit)+`
		)`), olderThan); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, s.q(`
		DELETE FROM test_runs WHERE id IN (
			SELECT id FROM test_runs WHERE finished_at < ?
			ORDER BY id LIMIT `+itoa(limit)+`
		)`), olderThan)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AddIntelTestResults inserts per-case results for a run.
func (s *sqlStore) AddIntelTestResults(ctx context.Context, results []*TestResult) error {
	for _, r := range results {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO test_results (run_id, project_id, module_id, case_id, kind, endpoint,
				passed, failures_json, rootcause_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			r.RunID, r.ProjectID, r.ModuleID, r.CaseID, r.Kind, r.Endpoint,
			r.Passed, r.FailuresJSON, r.RootcauseJSON); err != nil {
			return err
		}
	}
	return nil
}

// GetIntelTestResult loads a single per-case result by id.
func (s *sqlStore) GetIntelTestResult(ctx context.Context, id int64) (*TestResult, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, run_id, project_id, module_id, case_id, kind, endpoint,
			passed, failures_json, rootcause_json, created_at
		FROM test_results WHERE id = ?`), id)
	r := &TestResult{}
	if err := row.Scan(&r.ID, &r.RunID, &r.ProjectID, &r.ModuleID, &r.CaseID,
		&r.Kind, &r.Endpoint, &r.Passed, &r.FailuresJSON, &r.RootcauseJSON, &r.CreatedAt); err != nil {
		return nil, err
	}
	return r, nil
}

// ListIntelTestResults returns per-case results for a run.
func (s *sqlStore) ListIntelTestResults(ctx context.Context, runID int64) ([]*TestResult, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, run_id, project_id, module_id, case_id, kind, endpoint,
			passed, failures_json, rootcause_json, created_at
		FROM test_results WHERE run_id = ? ORDER BY id`), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*TestResult, 0)
	for rows.Next() {
		r := &TestResult{}
		if err := rows.Scan(&r.ID, &r.RunID, &r.ProjectID, &r.ModuleID, &r.CaseID,
			&r.Kind, &r.Endpoint, &r.Passed, &r.FailuresJSON, &r.RootcauseJSON, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CreateIntelIssue persists a new issue and populates its id.
func (s *sqlStore) CreateIntelIssue(ctx context.Context, issue *IntelIssue) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_issues (project_id, module_id, feature_id, key, kind, severity,
				location, commit_seen, commit_fixed, status, resolved_at, last_check_at,
				detail_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			issue.ProjectID, issue.ModuleID, issue.FeatureID, issue.Key, issue.Kind, issue.Severity,
			issue.Location, issue.CommitSeen, issue.CommitFixed, issue.Status,
			issue.ResolvedAt, issue.LastCheckAt, issue.DetailJSON,
		).Scan(&issue.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_issues (project_id, module_id, feature_id, key, kind, severity,
			location, commit_seen, commit_fixed, status, resolved_at, last_check_at,
			detail_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		issue.ProjectID, issue.ModuleID, issue.FeatureID, issue.Key, issue.Kind, issue.Severity,
		issue.Location, issue.CommitSeen, issue.CommitFixed, issue.Status,
		issue.ResolvedAt, issue.LastCheckAt, issue.DetailJSON,
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	issue.ID = id
	return nil
}

// ListIntelIssues returns issues for a project, optionally narrowed by status
// ("" returns all).
func (s *sqlStore) ListIntelIssues(ctx context.Context, projectID int64, status string) ([]*IntelIssue, error) {
	query := `SELECT id, project_id, module_id, feature_id, key, kind, severity,
		location, commit_seen, commit_fixed, status, resolved_at, last_check_at,
		detail_json, created_at FROM intel_issues WHERE project_id = ?`
	args := []any{projectID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelIssue, 0)
	for rows.Next() {
		issue, err := scanIntelIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, issue)
	}
	return out, rows.Err()
}

// UpdateIntelIssue persists mutable issue fields.
func (s *sqlStore) UpdateIntelIssue(ctx context.Context, issue *IntelIssue) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_issues SET feature_id = ?, kind = ?, severity = ?, status = ?,
			commit_fixed = ?, resolved_at = ?, last_check_at = ? WHERE id = ?`),
		issue.FeatureID, issue.Kind, issue.Severity, issue.Status,
		issue.CommitFixed, issue.ResolvedAt, issue.LastCheckAt, issue.ID)
	return err
}

// ReplaceIntelFeatures syncs a project's feature points by natural key
// (source+anchor+name), mirroring ReplaceIntelModules: existing rows keep their
// stable id and cached summary, newly detected rows are inserted, and rows no
// longer present are removed. Stable ids keep human overrides
// (intel_overrides.row_key = feature id), intel_issues.feature_id and
// intel_feature_chats.feature_id valid across analyses, so "人工优先、重扫不
// 覆盖" holds at the id level too. Rows passed in with a non-zero id (e.g. the
// manual rows re-sourced by persistFeatures) are updated in place by id.
func (s *sqlStore) ReplaceIntelFeatures(ctx context.Context, projectID int64, feats []*IntelFeature) error {
	// 空列表保护：同 ReplaceIntelEntities——无结果保留旧快照，避免误清空
	// 功能点数据（已有 manual 行时，调用方总会带上，但保险起见仍判空）。
	if len(feats) == 0 {
		return nil
	}
	existing, err := s.ListIntelFeatures(ctx, projectID)
	if err != nil {
		return err
	}
	byID := make(map[int64]*IntelFeature, len(existing))
	byKey := make(map[string]*IntelFeature, len(existing))
	for _, e := range existing {
		byID[e.ID] = e
		byKey[featureUpsertKey(e)] = e
	}
	matched := make(map[int64]bool, len(existing))
	for _, f := range feats {
		var prev *IntelFeature
		if f.ID > 0 {
			prev = byID[f.ID]
		}
		if prev == nil {
			prev = byKey[featureUpsertKey(f)]
		}
		if prev != nil {
			f.ID = prev.ID
			f.Summary = prev.Summary
			matched[prev.ID] = true
			if _, err := s.db.ExecContext(ctx, s.q(`
				UPDATE intel_features SET name = ?, ends_json = ?, sort_order = ?,
					source = ?, anchor = ?, status = ?, updated_at = CURRENT_TIMESTAMP
				WHERE id = ?`),
				f.Name, f.EndsJSON, f.SortOrder, f.Source, f.Anchor, f.Status, f.ID); err != nil {
				return err
			}
			continue
		}
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_features (project_id, name, summary, ends_json, sort_order,
				source, anchor, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
			projectID, f.Name, f.Summary, f.EndsJSON, f.SortOrder,
			f.Source, f.Anchor, f.Status); err != nil {
			return err
		}
	}
	for id := range byID {
		if !matched[id] {
			if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_features WHERE id = ?`), id); err != nil {
				return err
			}
		}
	}
	return nil
}

// featureUpsertKey is the natural key used to keep a feature row's id stable
// across analyses: source+anchor+name. Manual rows (source=manual) are grouped
// apart from auto rows, and rows that carry a real id are matched by id first.
func featureUpsertKey(f *IntelFeature) string {
	return f.Source + "\x00" + f.Anchor + "\x00" + f.Name
}

// CreateIntelFeature inserts a (usually human-created) feature point and fills
// its id. Manual features are stamped source=manual and are not overwritten by
// the next auto rescan (persistFeatures only replaces source=auto rows).
func (s *sqlStore) CreateIntelFeature(ctx context.Context, f *IntelFeature) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_features (project_id, name, summary, ends_json, sort_order,
				source, anchor, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id`),
			f.ProjectID, f.Name, f.Summary, f.EndsJSON, f.SortOrder,
			f.Source, f.Anchor, f.Status,
		).Scan(&f.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_features (project_id, name, summary, ends_json, sort_order,
			source, anchor, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		f.ProjectID, f.Name, f.Summary, f.EndsJSON, f.SortOrder,
		f.Source, f.Anchor, f.Status)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	f.ID = id
	return nil
}

// ListIntelFeatures returns feature points for a project ordered by sort order.
func (s *sqlStore) ListIntelFeatures(ctx context.Context, projectID int64) ([]*IntelFeature, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, name, summary, ends_json, sort_order, source, anchor,
			status, created_at, updated_at
		FROM intel_features WHERE project_id = ? ORDER BY sort_order, name`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelFeature, 0)
	for rows.Next() {
		f := &IntelFeature{}
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Summary, &f.EndsJSON,
			&f.SortOrder, &f.Source, &f.Anchor, &f.Status, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetIntelFeature loads a single feature point by id.
func (s *sqlStore) GetIntelFeature(ctx context.Context, id int64) (*IntelFeature, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, name, summary, ends_json, sort_order, source, anchor,
			status, created_at, updated_at
		FROM intel_features WHERE id = ?`), id)
	f := &IntelFeature{}
	if err := row.Scan(&f.ID, &f.ProjectID, &f.Name, &f.Summary, &f.EndsJSON,
		&f.SortOrder, &f.Source, &f.Anchor, &f.Status, &f.CreatedAt, &f.UpdatedAt); err != nil {
		return nil, err
	}
	return f, nil
}

// UpdateIntelFeature persists mutable feature fields.
func (s *sqlStore) UpdateIntelFeature(ctx context.Context, feat *IntelFeature) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_features SET name = ?, summary = ?, ends_json = ?, sort_order = ?,
			source = ?, status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`),
		feat.Name, feat.Summary, feat.EndsJSON, feat.SortOrder,
		feat.Source, feat.Status, feat.ID)
	return err
}

func scanIntelTestRun(row rowScanner) (*TestRun, error) {
	run := &TestRun{}
	var started, finished *time.Time
	err := row.Scan(&run.ID, &run.ProjectID, &run.ModuleID, &run.Scope, &run.Kind, &run.Command,
		&run.Status, &run.Attempts, &started, &finished, &run.LogPath, &run.Progress, &run.Output, &run.Priority, &run.CreatedAt)
	if err != nil {
		return nil, err
	}
	run.StartedAt = started
	run.FinishedAt = finished
	return run, nil
}

func scanIntelIssue(row rowScanner) (*IntelIssue, error) {
	i := &IntelIssue{}
	var resolved, lastCheck *time.Time
	err := row.Scan(&i.ID, &i.ProjectID, &i.ModuleID, &i.FeatureID, &i.Key, &i.Kind, &i.Severity,
		&i.Location, &i.CommitSeen, &i.CommitFixed, &i.Status, &resolved, &lastCheck,
		&i.DetailJSON, &i.CreatedAt)
	if err != nil {
		return nil, err
	}
	i.ResolvedAt = resolved
	i.LastCheckAt = lastCheck
	return i, nil
}
