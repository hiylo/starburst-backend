package store

import (
	"context"
	"time"
)

// IntelWebBinding is one "page -> data field" binding extracted from a Vue
// template: the Web client's must-display field list (mirrors the Android
// client binding).
type IntelWebBinding struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"projectId"`
	ModuleID   int64     `json:"moduleId"`
	Page       string    `json:"page"`
	FieldPath  string    `json:"fieldPath"`
	Slot       string    `json:"slot"`
	SourceFile string    `json:"sourceFile"`
	SourceLine int       `json:"sourceLine"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ReplaceIntelWebBindings deletes the project's web bindings and re-inserts the
// given set, so a rescan reflects the current templates.
func (s *sqlStore) ReplaceIntelWebBindings(ctx context.Context, projectID int64, bindings []*IntelWebBinding) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_web_bindings WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, b := range bindings {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_web_bindings (project_id, module_id, page, field_path,
				slot, source_file, source_line, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, b.ModuleID, b.Page, b.FieldPath, b.Slot, b.SourceFile, b.SourceLine); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelWebBindings returns the project's web bindings ordered by page then
// field path.
func (s *sqlStore) ListIntelWebBindings(ctx context.Context, projectID int64) ([]*IntelWebBinding, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, module_id, page, field_path, slot, source_file, source_line, created_at
		FROM intel_web_bindings WHERE project_id = ? ORDER BY page ASC, field_path ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelWebBinding, 0)
	for rows.Next() {
		b := &IntelWebBinding{}
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ModuleID, &b.Page, &b.FieldPath,
			&b.Slot, &b.SourceFile, &b.SourceLine, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
