package store

import (
	"context"
	"time"
)

// IntelAndroidBinding is one "page -> data field" binding extracted from an
// Android DataBinding layout expression: the client's must-display field list.
// When the backend returns null for one of these bound fields the client
// necessarily breaks, which the test layer reports as CLIENT_MISSING_FIELD.
type IntelAndroidBinding struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"projectId"`
	ModuleID   int64     `json:"moduleId"`
	Page       string    `json:"page"`
	FieldPath  string    `json:"fieldPath"`
	Widget     string    `json:"widget"`
	SourceFile string    `json:"sourceFile"`
	SourceLine int       `json:"sourceLine"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ReplaceIntelAndroidBindings deletes the project's client bindings and
// re-inserts the given set, so a rescan reflects the current layouts.
func (s *sqlStore) ReplaceIntelAndroidBindings(ctx context.Context, projectID int64, bindings []*IntelAndroidBinding) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_android_bindings WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, b := range bindings {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_android_bindings (project_id, module_id, page, field_path,
				widget, source_file, source_line, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, b.ModuleID, b.Page, b.FieldPath, b.Widget, b.SourceFile, b.SourceLine); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelAndroidBindings returns the project's client bindings ordered by
// page then field path.
func (s *sqlStore) ListIntelAndroidBindings(ctx context.Context, projectID int64) ([]*IntelAndroidBinding, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, module_id, page, field_path, widget, source_file, source_line, created_at
		FROM intel_android_bindings WHERE project_id = ? ORDER BY page ASC, field_path ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelAndroidBinding, 0)
	for rows.Next() {
		b := &IntelAndroidBinding{}
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.ModuleID, &b.Page, &b.FieldPath,
			&b.Widget, &b.SourceFile, &b.SourceLine, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
