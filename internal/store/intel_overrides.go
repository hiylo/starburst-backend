package store

import (
	"context"
	"time"
)

// IntelOverride is one human correction overriding an auto-derived value
// (module role, feature name, env version, ...). Pending rows form the
// 待确认队列; confirm/apply finalizes them; superseded/rejected retire them.
type IntelOverride struct {
	ID            int64     `json:"id"`
	ProjectID     int64     `json:"projectId"`
	ModuleID      int64     `json:"moduleId"`
	Target        string    `json:"target"` // e.g. module | feature | env_requirement
	RowKey        string    `json:"rowKey"` // the auto row's natural key
	Field         string    `json:"field"`  // e.g. role | name | version
	AutoValueJSON string    `json:"autoValueJson"`
	ManualValue   string    `json:"manualValue"`
	Confidence    string    `json:"confidence"` // high | medium | low
	Status        string    `json:"status"`     // pending | applied | superseded | rejected
	Source        string    `json:"source"`     // table | manual | llm-suggest
	Anchor        string    `json:"anchor"`     // file:line / commit provenance
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// CreateIntelOverride inserts one override (usually pending) and fills its id.
func (s *sqlStore) CreateIntelOverride(ctx context.Context, o *IntelOverride) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_overrides (project_id, module_id, target, row_key, field,
				auto_value_json, manual_value, confidence, status, source, anchor,
				created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id`),
			o.ProjectID, o.ModuleID, o.Target, o.RowKey, o.Field, o.AutoValueJSON,
			o.ManualValue, o.Confidence, o.Status, o.Source, o.Anchor,
		).Scan(&o.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_overrides (project_id, module_id, target, row_key, field,
			auto_value_json, manual_value, confidence, status, source, anchor,
			created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		o.ProjectID, o.ModuleID, o.Target, o.RowKey, o.Field, o.AutoValueJSON,
		o.ManualValue, o.Confidence, o.Status, o.Source, o.Anchor)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	o.ID = id
	return nil
}

// UpsertIntelOverride replaces the existing override for the same
// (project_id, target, row_key, field) natural key (status applied), so a
// human edit is idempotent and no duplicates accumulate across re-analysis.
func (s *sqlStore) UpsertIntelOverride(ctx context.Context, o *IntelOverride) error {
	if _, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM intel_overrides WHERE project_id = ? AND target = ? AND row_key = ? AND field = ?`),
		o.ProjectID, o.Target, o.RowKey, o.Field); err != nil {
		return err
	}
	return s.CreateIntelOverride(ctx, o)
}

// ListIntelOverrides returns overrides for a project, optionally only pending.
func (s *sqlStore) ListIntelOverrides(ctx context.Context, projectID int64, onlyPending bool) ([]*IntelOverride, error) {
	query := `SELECT id, project_id, module_id, target, row_key, field, auto_value_json,
		manual_value, confidence, status, source, anchor, created_at, updated_at
		FROM intel_overrides WHERE project_id = ?`
	args := []any{projectID}
	if onlyPending {
		query += ` AND status = 'pending'`
	}
	query += ` ORDER BY id DESC`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelOverride, 0)
	for rows.Next() {
		o := &IntelOverride{}
		if err := rows.Scan(&o.ID, &o.ProjectID, &o.ModuleID, &o.Target, &o.RowKey,
			&o.Field, &o.AutoValueJSON, &o.ManualValue, &o.Confidence, &o.Status,
			&o.Source, &o.Anchor, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetIntelOverride loads one override by id.
func (s *sqlStore) GetIntelOverride(ctx context.Context, id int64) (*IntelOverride, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, module_id, target, row_key, field, auto_value_json,
			manual_value, confidence, status, source, anchor, created_at, updated_at
		FROM intel_overrides WHERE id = ?`), id)
	o := &IntelOverride{}
	if err := row.Scan(&o.ID, &o.ProjectID, &o.ModuleID, &o.Target, &o.RowKey,
		&o.Field, &o.AutoValueJSON, &o.ManualValue, &o.Confidence, &o.Status,
		&o.Source, &o.Anchor, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	return o, nil
}

// UpdateIntelOverride persists a pending override's manual value and status
// (confirm/apply/reject path, 人工最后拍板).
func (s *sqlStore) UpdateIntelOverride(ctx context.Context, o *IntelOverride) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_overrides SET manual_value = ?, status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`), o.ManualValue, o.Status, o.ID)
	return err
}
