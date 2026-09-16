package store

import (
	"context"
	"time"
)

// IntelOverview is the aggregated dependency/environment/SBOM snapshot of a
// project, refreshed on each analyze.
type IntelOverview struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"projectId"`
	DepsJSON  string    `json:"depsJson"`
	EnvJSON   string    `json:"envJson"`
	SbomJSON  string    `json:"sbomJson"`
	CreatedAt time.Time `json:"createdAt"`
}

// ReplaceIntelOverview deletes the project's overview rows and inserts the
// latest snapshot.
func (s *sqlStore) ReplaceIntelOverview(ctx context.Context, projectID int64, ov *IntelOverview) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_overviews WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	if ov == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_overviews (project_id, deps_json, env_json, sbom_json, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		projectID, ov.DepsJSON, ov.EnvJSON, ov.SbomJSON); err != nil {
		return err
	}
	return nil
}

// GetIntelOverview returns the project's latest overview snapshot, or
// ErrNotFound when none has been recorded.
func (s *sqlStore) GetIntelOverview(ctx context.Context, projectID int64) (*IntelOverview, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, deps_json, env_json, sbom_json, created_at
		FROM intel_overviews WHERE project_id = ? ORDER BY id DESC LIMIT 1`), projectID)
	ov := &IntelOverview{}
	if err := row.Scan(&ov.ID, &ov.ProjectID, &ov.DepsJSON, &ov.EnvJSON, &ov.SbomJSON, &ov.CreatedAt); err != nil {
		return nil, err
	}
	return ov, nil
}
