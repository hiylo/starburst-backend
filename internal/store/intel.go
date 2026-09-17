package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// IntelProject is one registered test-intelligence project (a flat list; a
// single user, no product-group hierarchy). Source is either a local path or a
// git URL that is cloned into the repos_dir cache.
type IntelProject struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Source        string     `json:"source"` // local | git
	LocalPath     string     `json:"localPath"`
	GitURL        string     `json:"gitUrl"`
	GitRef        string     `json:"gitRef"`
	LastTestedSHA string     `json:"lastTestedSha"`
	SnapshotSHA   string     `json:"snapshotSha"`
	CommandsJSON  string     `json:"commandsJson"`
	EnvName       string     `json:"envName"`
	AnalyzedAt    *time.Time `json:"analyzedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// IntelModule is one sub-module of a monorepo/mixed-type repository.
// Each module gets an independent type/role and owns its own contracts, test
// assets, env requirements and command whitelist.
type IntelModule struct {
	ID            int64      `json:"id"`
	ProjectID     int64      `json:"projectId"`
	RelPath       string     `json:"relPath"`
	KindType      string     `json:"kindType"`
	KindRole      string     `json:"kindRole"`
	BuildTool     string     `json:"buildTool"`
	CommandsJSON  string     `json:"commandsJson"`
	LastTestedSHA string     `json:"lastTestedSha"`
	AnalyzedAt    *time.Time `json:"analyzedAt"`
	CreatedAt     time.Time  `json:"createdAt"`
}

// IntelEntity is a single entity↔table↔column mapping extracted deterministically
// from the source code (JPA @Entity/@Table/@Column, Flyway migrations, etc.).
type IntelEntity struct {
	ID         int64  `json:"id"`
	ProjectID  int64  `json:"projectId"`
	ModuleID   int64  `json:"moduleId"`
	Entity     string `json:"entity"`
	TableName  string `json:"table"`
	ColumnName string `json:"column"`
	FieldType  string `json:"fieldType"`
	Nullable   bool   `json:"nullable"`
	IsPrimary  bool   `json:"isPrimary"`
	SourceFile string `json:"sourceFile"`
	SourceLine int    `json:"sourceLine"`
}

// IntelEndpoint is one API endpoint contract extracted from the code
// (Controller annotations), including the request side (params/body) and the
// response field set with required/nullable markers.
type IntelEndpoint struct {
	ID           int64  `json:"id"`
	ProjectID    int64  `json:"projectId"`
	ModuleID     int64  `json:"moduleId"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	ResponseType string `json:"responseType"`
	RequestJSON  string `json:"requestJson"`
	FieldsJSON   string `json:"fieldsJson"`
	Summary      string `json:"summary"`
	SourceFile   string `json:"sourceFile"`
	SourceLine   int    `json:"sourceLine"`
	// GatewayRoutes is the computed public gateway exposure (path patterns),
	// filled on read; it is not persisted.
	GatewayRoutes []string `json:"gatewayRoutes,omitempty"`
}

// CreateIntelProject persists a new project and populates its auto-generated id.
func (s *sqlStore) CreateIntelProject(ctx context.Context, p *IntelProject) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO projects (name, source, local_path, git_url, git_ref, last_tested_sha,
				snapshot_sha, commands_json, env_name, analyzed_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id`),
			p.Name, p.Source, p.LocalPath, p.GitURL, p.GitRef, p.LastTestedSHA,
			p.SnapshotSHA, p.CommandsJSON, p.EnvName, p.AnalyzedAt,
		).Scan(&p.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO projects (name, source, local_path, git_url, git_ref, last_tested_sha,
			snapshot_sha, commands_json, env_name, analyzed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		p.Name, p.Source, p.LocalPath, p.GitURL, p.GitRef, p.LastTestedSHA,
		p.SnapshotSHA, p.CommandsJSON, p.EnvName, p.AnalyzedAt,
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	p.ID = id
	return nil
}

// ListIntelProjects returns all registered projects, newest first.
func (s *sqlStore) ListIntelProjects(ctx context.Context) ([]*IntelProject, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, source, local_path, git_url, git_ref, last_tested_sha,
			snapshot_sha, commands_json, env_name, analyzed_at, created_at, updated_at
		FROM projects ORDER BY created_at DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelProject, 0)
	for rows.Next() {
		p, err := scanIntelProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetIntelProject loads a single project.
func (s *sqlStore) GetIntelProject(ctx context.Context, id int64) (*IntelProject, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, source, local_path, git_url, git_ref, last_tested_sha,
			snapshot_sha, commands_json, env_name, analyzed_at, created_at, updated_at
		FROM projects WHERE id = ?`), id)
	p, err := scanIntelProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// UpdateIntelProject persists the mutable project fields.
func (s *sqlStore) UpdateIntelProject(ctx context.Context, p *IntelProject) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE projects SET name = ?, source = ?, local_path = ?, git_url = ?, git_ref = ?,
			last_tested_sha = ?, snapshot_sha = ?, commands_json = ?, env_name = ?,
			analyzed_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`),
		p.Name, p.Source, p.LocalPath, p.GitURL, p.GitRef, p.LastTestedSHA,
		p.SnapshotSHA, p.CommandsJSON, p.EnvName, p.AnalyzedAt, p.ID)
	return err
}

// MarkIntelProjectAnalyzed records the snapshot sha and analyzed timestamp after
// a successful scan, and clears the pending "to re-analyze" state.
func (s *sqlStore) MarkIntelProjectAnalyzed(ctx context.Context, id int64, snapshotSHA string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE projects SET snapshot_sha = ?, analyzed_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP WHERE id = ?`), snapshotSHA, id)
	return err
}

// DeleteIntelProject removes a project and all its intel data.
func (s *sqlStore) DeleteIntelProject(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM project_modules WHERE project_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_entities WHERE project_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_endpoints WHERE project_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_chunks WHERE project_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM projects WHERE id = ?`), id); err != nil {
		return err
	}
	return nil
}

// ReplaceIntelModules deletes the project's modules and re-inserts the given
// set, so a scan always reflects the current repository layout.
func (s *sqlStore) ReplaceIntelModules(ctx context.Context, projectID int64, mods []*IntelModule) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM project_modules WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, m := range mods {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO project_modules (project_id, rel_path, kind_type, kind_role, build_tool,
				commands_json, last_tested_sha, analyzed_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, m.RelPath, m.KindType, m.KindRole, m.BuildTool,
			m.CommandsJSON, m.LastTestedSHA, m.AnalyzedAt); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelModules returns the project's sub-modules ordered by path.
func (s *sqlStore) ListIntelModules(ctx context.Context, projectID int64) ([]*IntelModule, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, rel_path, kind_type, kind_role, build_tool,
			commands_json, last_tested_sha, analyzed_at, created_at
		FROM project_modules WHERE project_id = ? ORDER BY rel_path ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelModule, 0)
	for rows.Next() {
		m := &IntelModule{}
		var analyzed *time.Time
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.RelPath, &m.KindType, &m.KindRole,
			&m.BuildTool, &m.CommandsJSON, &m.LastTestedSHA, &analyzed, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.AnalyzedAt = analyzed
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceIntelEntities deletes the project's entity mappings and re-inserts the
// given set (a full rescan replaces the prior snapshot). Each entity carries
// its own ModuleID.
func (s *sqlStore) ReplaceIntelEntities(ctx context.Context, projectID int64, ents []*IntelEntity) error {
	if _, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM intel_entities WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, e := range ents {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_entities (project_id, module_id, entity, table_name, column_name,
				field_type, nullable, is_primary, source_file, source_line)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			projectID, e.ModuleID, e.Entity, e.TableName, e.ColumnName,
			e.FieldType, e.Nullable, e.IsPrimary, e.SourceFile, e.SourceLine); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelEntities returns entity mappings for a project (optionally narrowed
// to a single module).
func (s *sqlStore) ListIntelEntities(ctx context.Context, projectID, moduleID int64) ([]*IntelEntity, error) {
	query := `SELECT id, project_id, module_id, entity, table_name, column_name,
		field_type, nullable, is_primary, source_file, source_line FROM intel_entities WHERE project_id = ?`
	args := []any{projectID}
	if moduleID > 0 {
		query += ` AND module_id = ?`
		args = append(args, moduleID)
	}
	query += ` ORDER BY table_name, column_name`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelEntity, 0)
	for rows.Next() {
		e := &IntelEntity{}
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.ModuleID, &e.Entity, &e.TableName,
			&e.ColumnName, &e.FieldType, &e.Nullable, &e.IsPrimary, &e.SourceFile, &e.SourceLine); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReplaceIntelEndpoints deletes the project's endpoint contracts and re-inserts
// the given set (a full rescan replaces the prior snapshot).
func (s *sqlStore) ReplaceIntelEndpoints(ctx context.Context, projectID int64, eps []*IntelEndpoint) error {
	if _, err := s.db.ExecContext(ctx, s.q(`
		DELETE FROM intel_endpoints WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, ep := range eps {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO intel_endpoints (project_id, module_id, method, path, response_type,
				request_json, fields_json, source_file, source_line, summary)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			projectID, ep.ModuleID, ep.Method, ep.Path, ep.ResponseType,
			ep.RequestJSON, ep.FieldsJSON, ep.SourceFile, ep.SourceLine, ep.Summary); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelEndpoints returns endpoint contracts for a project (optionally
// narrowed to a single module).
func (s *sqlStore) ListIntelEndpoints(ctx context.Context, projectID, moduleID int64) ([]*IntelEndpoint, error) {
	query := `SELECT id, project_id, module_id, method, path, response_type,
		request_json, fields_json, source_file, source_line, summary FROM intel_endpoints WHERE project_id = ?`
	args := []any{projectID}
	if moduleID > 0 {
		query += ` AND module_id = ?`
		args = append(args, moduleID)
	}
	query += ` ORDER BY path, method`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelEndpoint, 0)
	for rows.Next() {
		ep := &IntelEndpoint{}
		if err := rows.Scan(&ep.ID, &ep.ProjectID, &ep.ModuleID, &ep.Method, &ep.Path,
			&ep.ResponseType, &ep.RequestJSON, &ep.FieldsJSON, &ep.SourceFile, &ep.SourceLine, &ep.Summary); err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// UpdateIntelEndpointSummary persists the LLM-derived business summary for a
// single endpoint, matched by project + method + path.
func (s *sqlStore) UpdateIntelEndpointSummary(ctx context.Context, projectID int64, method, path, summary string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_endpoints SET summary = ? WHERE project_id = ? AND method = ? AND path = ?`),
		summary, projectID, method, path)
	return err
}

func scanIntelProject(row rowScanner) (*IntelProject, error) {
	p := &IntelProject{}
	var analyzed *time.Time
	err := row.Scan(&p.ID, &p.Name, &p.Source, &p.LocalPath, &p.GitURL, &p.GitRef,
		&p.LastTestedSHA, &p.SnapshotSHA, &p.CommandsJSON, &p.EnvName,
		&analyzed, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.AnalyzedAt = analyzed
	return p, nil
}
