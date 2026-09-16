package store

import (
	"context"
	"time"
)

// IntelGatewayRoute is one public gateway exposure of a backend service:
// a service id plus the public path pattern(s) under which it is reachable.
type IntelGatewayRoute struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"projectId"`
	Service    string    `json:"service"`
	PathsJSON  string    `json:"pathsJson"`
	URI        string    `json:"uri"`
	Source     string    `json:"source"`
	SourceLine int       `json:"sourceLine"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ReplaceIntelGatewayRoutes deletes the project's gateway routes and re-inserts
// the given set, so a rescan reflects the current configuration.
func (s *sqlStore) ReplaceIntelGatewayRoutes(ctx context.Context, projectID int64, routes []*IntelGatewayRoute) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_gateway_routes WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	return s.insertIntelGatewayRoutes(ctx, projectID, routes)
}

// AddIntelGatewayRoutes appends gateway routes without deleting existing ones.
// Used for LLM-suggested routes so config-derived routes are preserved.
func (s *sqlStore) AddIntelGatewayRoutes(ctx context.Context, projectID int64, routes []*IntelGatewayRoute) error {
	return s.insertIntelGatewayRoutes(ctx, projectID, routes)
}

func (s *sqlStore) insertIntelGatewayRoutes(ctx context.Context, projectID int64, routes []*IntelGatewayRoute) error {
	for _, r := range routes {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_gateway_routes (project_id, service, paths_json, uri, source, source_line, created_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, r.Service, r.PathsJSON, r.URI, r.Source, r.SourceLine); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelGatewayRoutes returns the project's gateway routes ordered by
// service.
func (s *sqlStore) ListIntelGatewayRoutes(ctx context.Context, projectID int64) ([]*IntelGatewayRoute, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, service, paths_json, uri, source, source_line, created_at
		FROM intel_gateway_routes WHERE project_id = ? ORDER BY service ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelGatewayRoute, 0)
	for rows.Next() {
		r := &IntelGatewayRoute{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Service, &r.PathsJSON, &r.URI,
			&r.Source, &r.SourceLine, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
