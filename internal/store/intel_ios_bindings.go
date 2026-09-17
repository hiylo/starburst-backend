package store

import (
	"context"
	"time"
)

// IntelIosBinding is one "page -> data field" binding extracted from a SwiftUI
// view: the iOS client's must-display field list (mirrors the Android and Web
// client bindings).
type IntelIosBinding struct {
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

// ReplaceIntelIosBindings deletes the project's iOS bindings and re-inserts the
// given set, so a rescan reflects the current views.
func (s *sqlStore) ReplaceIntelIosBindings(ctx context.Context, projectID int64, bindings []*IntelIosBinding) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_ios_bindings WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, b := range bindings {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_ios_bindings (project_id, module_id, page, field_path,
				slot, source_file, source_line, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, b.ModuleID, b.Page, b.FieldPath, b.Slot, b.SourceFile, b.SourceLine); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelIosBindings returns the project's iOS bindings ordered by page then
// field path.
func (s *sqlStore) ListIntelIosBindings(ctx context.Context, projectID int64) ([]*IntelIosBinding, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, module_id, page, field_path, slot, source_file, source_line, created_at
		FROM intel_ios_bindings WHERE project_id = ? ORDER BY page ASC, field_path ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelIosBinding, 0)
	for rows.Next() {
		b := &IntelIosBinding{}
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ModuleID, &b.Page, &b.FieldPath,
			&b.Slot, &b.SourceFile, &b.SourceLine, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
