package store

import (
	"context"
	"time"
)

// IntelImpact is the latest incremental-impact snapshot: what changed between
// the previous analysis and HEAD, and which modules/tests/sources are affected.
type IntelImpact struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"projectId"`
	BaseSHA    string    `json:"baseSha"`
	HeadSHA    string    `json:"headSha"`
	ImpactJSON string    `json:"impactJson"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ReplaceIntelImpact deletes the project's impact snapshots and inserts the
// latest one, keeping only the most recent delta.
func (s *sqlStore) ReplaceIntelImpact(ctx context.Context, projectID int64, imp *IntelImpact) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_impacts WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	if imp == nil {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_impacts (project_id, base_sha, head_sha, impact_json, created_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		projectID, imp.BaseSHA, imp.HeadSHA, imp.ImpactJSON); err != nil {
		return err
	}
	return nil
}

// GetIntelImpact returns the project's latest impact snapshot, or ErrNotFound
// when none has been recorded.
func (s *sqlStore) GetIntelImpact(ctx context.Context, projectID int64) (*IntelImpact, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, base_sha, head_sha, impact_json, created_at
		FROM intel_impacts WHERE project_id = ? ORDER BY id DESC LIMIT 1`), projectID)
	imp := &IntelImpact{}
	err := row.Scan(&imp.ID, &imp.ProjectID, &imp.BaseSHA, &imp.HeadSHA, &imp.ImpactJSON, &imp.CreatedAt)
	if err != nil {
		return nil, err
	}
	return imp, nil
}
