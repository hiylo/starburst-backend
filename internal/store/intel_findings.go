package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IntelFinding is one security/compliance audit finding (dependency vuln,
// code/lint rule, compliance rule or LLM review). It moves through the closed
// loop and can be waived or marked false-positive with a reason.
type IntelFinding struct {
	ID           int64      `json:"id"`
	ProjectID    int64      `json:"projectId"`
	ModuleID     int64      `json:"moduleId"`
	Detector     string     `json:"detector"`
	Severity     string     `json:"severity"`
	Category     string     `json:"category"`
	CveOrRuleID  string     `json:"cveOrRuleId"`
	Location     string     `json:"location"`
	Summary      string     `json:"summary"`
	Status       string     `json:"status"`
	RemovedAt    *time.Time `json:"removedAt"`
	WaivedReason string     `json:"waivedReason"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// IntelFix is a fix-suggestion patch draft, applied only after human review
// (the single write path). applied_backup keeps the original text for rollback.
type IntelFix struct {
	ID            int64      `json:"id"`
	ProjectID     int64      `json:"projectId"`
	IssueID       int64      `json:"issueId"`
	FindingID     int64      `json:"findingId"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	DiffJSON      string     `json:"diffJson"`
	Status        string     `json:"status"`
	AppliedBackup string     `json:"appliedBackup"`
	WriteMode     string     `json:"writeMode"`
	AppliedAt     *time.Time `json:"appliedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// CreateIntelFinding persists a new finding and populates its id.
func (s *sqlStore) CreateIntelFinding(ctx context.Context, f *IntelFinding) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_findings (project_id, module_id, detector, severity, category,
				cve_or_rule_id, location, summary, status, removed_at, waived_reason, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			f.ProjectID, f.ModuleID, f.Detector, f.Severity, f.Category,
			f.CveOrRuleID, f.Location, f.Summary, f.Status, f.RemovedAt, f.WaivedReason,
		).Scan(&f.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_findings (project_id, module_id, detector, severity, category,
			cve_or_rule_id, location, summary, status, removed_at, waived_reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		f.ProjectID, f.ModuleID, f.Detector, f.Severity, f.Category,
		f.CveOrRuleID, f.Location, f.Summary, f.Status, f.RemovedAt, f.WaivedReason,
	)
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

// CreateIntelFindingIfAbsent inserts a finding only when no finding with the
// same detector + rule + location already exists for the project, so a rescan
// preserves user review state (waive/resolve) instead of duplicating rows.
// It reports whether a new row was inserted.
func (s *sqlStore) CreateIntelFindingIfAbsent(ctx context.Context, f *IntelFinding) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, s.q(`
		SELECT COUNT(*) FROM intel_findings
		WHERE project_id = ? AND detector = ? AND cve_or_rule_id = ? AND location = ?`),
		f.ProjectID, f.Detector, f.CveOrRuleID, f.Location).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if err := s.CreateIntelFinding(ctx, f); err != nil {
		return false, err
	}
	return true, nil
}

// CloseStaleIntelFindings marks every open finding of a project+detector that
// is NOT in keepKeys (cve_or_rule_id+location) as fixed, closing the loop after
// a fresh scan: a rule that no longer fires (code fixed) must not linger as
// "open" in the audit view. Findings the user explicitly waived/resolved are
// left untouched. Returns how many findings were closed.
func (s *sqlStore) CloseStaleIntelFindings(ctx context.Context, projectID int64, detector string, keepKeys map[string]bool) (int, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, cve_or_rule_id, location FROM intel_findings
		WHERE project_id = ? AND detector = ? AND status = ?`), projectID, detector, "open")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		var rule, loc string
		if err := rows.Scan(&id, &rule, &loc); err != nil {
			return 0, err
		}
		if !keepKeys[rule+"\x00"+loc] {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range stale {
		if _, err := s.db.ExecContext(ctx, s.q(`
			UPDATE intel_findings SET status = ? WHERE id = ?`), "fixed", id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// ListIntelFindings returns findings for a project, optionally filtered by
// status and detector ("" = all).
func (s *sqlStore) ListIntelFindings(ctx context.Context, projectID int64, status, detector string) ([]*IntelFinding, error) {
	query := `SELECT id, project_id, module_id, detector, severity, category,
		cve_or_rule_id, location, summary, status, removed_at, waived_reason, created_at
		FROM intel_findings WHERE project_id = ?`
	args := []any{projectID}
	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if detector != "" {
		query += ` AND detector = ?`
		args = append(args, detector)
	}
	query += ` ORDER BY severity DESC, created_at DESC`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelFinding, 0)
	for rows.Next() {
		f, err := scanIntelFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateIntelFinding persists mutable finding fields (status/waive/resolution).
func (s *sqlStore) UpdateIntelFinding(ctx context.Context, f *IntelFinding) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_findings SET status = ?, removed_at = ?, waived_reason = ? WHERE id = ?`),
		f.Status, f.RemovedAt, f.WaivedReason, f.ID)
	return err
}

// GetIntelFinding loads a single finding by id, or ErrNotFound when absent.
func (s *sqlStore) GetIntelFinding(ctx context.Context, id int64) (*IntelFinding, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, module_id, detector, severity, category,
			cve_or_rule_id, location, summary, status, removed_at, waived_reason, created_at
		FROM intel_findings WHERE id = ?`), id)
	f, err := scanIntelFinding(row)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// CreateIntelFix persists a new fix suggestion and populates its id.
func (s *sqlStore) CreateIntelFix(ctx context.Context, f *IntelFix) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_fixes (project_id, issue_id, finding_id, kind, title, diff_json,
				status, applied_backup, write_mode, applied_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			f.ProjectID, f.IssueID, f.FindingID, f.Kind, f.Title, f.DiffJSON,
			f.Status, f.AppliedBackup, f.WriteMode, f.AppliedAt,
		).Scan(&f.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_fixes (project_id, issue_id, finding_id, kind, title, diff_json,
			status, applied_backup, write_mode, applied_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		f.ProjectID, f.IssueID, f.FindingID, f.Kind, f.Title, f.DiffJSON,
		f.Status, f.AppliedBackup, f.WriteMode, f.AppliedAt,
	)
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

// GetIntelFix loads a single fix suggestion.
func (s *sqlStore) GetIntelFix(ctx context.Context, id int64) (*IntelFix, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, issue_id, finding_id, kind, title, diff_json, status,
			applied_backup, write_mode, applied_at, created_at
		FROM intel_fixes WHERE id = ?`), id)
	f, err := scanIntelFix(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// ListIntelFixes returns fix suggestions for a project, optionally filtered by
// status ("" = all).
func (s *sqlStore) ListIntelFixes(ctx context.Context, projectID int64, status string) ([]*IntelFix, error) {
	query := `SELECT id, project_id, issue_id, finding_id, kind, title, diff_json, status,
		applied_backup, write_mode, applied_at, created_at
		FROM intel_fixes WHERE project_id = ?`
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
	out := make([]*IntelFix, 0)
	for rows.Next() {
		f, err := scanIntelFix(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateIntelFix persists mutable fix fields (status/write-mode/apply).
func (s *sqlStore) UpdateIntelFix(ctx context.Context, f *IntelFix) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_fixes SET status = ?, applied_backup = ?, write_mode = ?, applied_at = ? WHERE id = ?`),
		f.Status, f.AppliedBackup, f.WriteMode, f.AppliedAt, f.ID)
	return err
}

func scanIntelFinding(row rowScanner) (*IntelFinding, error) {
	f := &IntelFinding{}
	var removed *time.Time
	err := row.Scan(&f.ID, &f.ProjectID, &f.ModuleID, &f.Detector, &f.Severity, &f.Category,
		&f.CveOrRuleID, &f.Location, &f.Summary, &f.Status, &removed, &f.WaivedReason, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	f.RemovedAt = removed
	return f, nil
}

func scanIntelFix(row rowScanner) (*IntelFix, error) {
	f := &IntelFix{}
	var applied *time.Time
	err := row.Scan(&f.ID, &f.ProjectID, &f.IssueID, &f.FindingID, &f.Kind, &f.Title, &f.DiffJSON,
		&f.Status, &f.AppliedBackup, &f.WriteMode, &applied, &f.CreatedAt)
	if err != nil {
		return nil, err
	}
	f.AppliedAt = applied
	return f, nil
}
