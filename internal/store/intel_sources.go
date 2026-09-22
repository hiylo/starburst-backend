package store

import (
	"context"
)

// IntelProjectSource is one associated source directory/repo of an intel
// project. A project can span multiple 端 (platform ends: java/android/ios/
// web/node/bff...), and each 端 may live in its own repository; every such repo
// is registered here. The project's own source (projects.local_path / git_url)
// remains the primary root; these are the additional per-end repos.
type IntelProjectSource struct {
	ID        int64  `json:"id"`
	ProjectID int64  `json:"projectId"`
	EndName   string `json:"endName"`
	Source    string `json:"source"` // local | git
	LocalPath string `json:"localPath"`
	GitURL    string `json:"gitUrl"`
	GitRef    string `json:"gitRef"`
}

// ListIntelProjectSources returns the project's associated source repos,
// oldest first.
func (s *sqlStore) ListIntelProjectSources(ctx context.Context, projectID int64) ([]*IntelProjectSource, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, end_name, source, local_path, git_url, git_ref
		FROM project_sources WHERE project_id = ? ORDER BY id ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelProjectSource, 0)
	for rows.Next() {
		src := &IntelProjectSource{}
		if err := rows.Scan(&src.ID, &src.ProjectID, &src.EndName, &src.Source,
			&src.LocalPath, &src.GitURL, &src.GitRef); err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// ReplaceIntelProjectSources replaces the project's associated source list in
// one transaction-like pass (delete all + insert each). This is an explicit
// user-facing replace-all endpoint (PUT /api/intel/projects/{id}/sources): an
// empty list legitimately clears the project's sources, so no empty-list guard.
func (s *sqlStore) ReplaceIntelProjectSources(ctx context.Context, projectID int64, sources []*IntelProjectSource) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM project_sources WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, src := range sources {
		src.ProjectID = projectID
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO project_sources (project_id, end_name, source, local_path, git_url, git_ref, created_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, src.EndName, src.Source, src.LocalPath, src.GitURL, src.GitRef); err != nil {
			return err
		}
	}
	return nil
}
