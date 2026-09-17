package store

import (
	"context"
	"time"
)

// IntelEnvRequirement is one declared environment dependency of a project,
// derived from its build/config files (envdetect) or a manual intel-env.yaml.
type IntelEnvRequirement struct {
	ID        int64     `json:"id"`
	ProjectID int64     `json:"projectId"`
	ModuleID  int64     `json:"moduleId"`
	Service   string    `json:"service"`
	Category  string    `json:"category"` // middleware | toolchain
	Version   string    `json:"version"`
	Source    string    `json:"source"` // auto | manual
	CreatedAt time.Time `json:"createdAt"`
}

// IntelEnvService is the live per-item status of one middleware/toolchain for a
// project: ready (running container or installed toolchain), missing (can be
// provisioned) or unsupported. provider distinguishes container/external/
// installed.
type IntelEnvService struct {
	ID            int64      `json:"id"`
	ProjectID     int64      `json:"projectId"`
	Service       string     `json:"service"`
	Category      string     `json:"category"`
	Version       string     `json:"version"`
	Provider      string     `json:"provider"`
	Status        string     `json:"status"`
	Host          string     `json:"host"`
	Port          int        `json:"port"`
	Endpoint      string     `json:"endpoint"`
	Healthy       bool       `json:"healthy"`
	ContainerName string     `json:"containerName"`
	ContainerID   string     `json:"containerId"`
	Username      string     `json:"username"`
	Password      string     `json:"-"`
	HealthCheckAt *time.Time `json:"healthCheckAt"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// ReplaceIntelEnvRequirements deletes the project's declared environment
// requirements and re-inserts the given set (a full rescan replaces the
// snapshot). Requirements are the input for the environment gate.
func (s *sqlStore) ReplaceIntelEnvRequirements(ctx context.Context, projectID int64, reqs []*IntelEnvRequirement) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM env_requirements WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, r := range reqs {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO env_requirements (project_id, module_id, service, category, version, source, created_at)
			VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
			projectID, r.ModuleID, r.Service, r.Category, r.Version, r.Source); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelEnvRequirements returns the project's declared environment
// requirements ordered by service.
func (s *sqlStore) ListIntelEnvRequirements(ctx context.Context, projectID int64) ([]*IntelEnvRequirement, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, module_id, service, category, version, source, created_at
		FROM env_requirements WHERE project_id = ? ORDER BY service ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelEnvRequirement, 0)
	for rows.Next() {
		r := &IntelEnvRequirement{}
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ModuleID, &r.Service, &r.Category,
			&r.Version, &r.Source, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceIntelEnvServices deletes the project's environment status rows and
// re-inserts the given set, so a rescan reflects the current live status.
func (s *sqlStore) ReplaceIntelEnvServices(ctx context.Context, projectID int64, services []*IntelEnvService) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM env_services WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	return s.upsertIntelEnvServices(ctx, projectID, services)
}

// UpsertIntelEnvServices upserts per-service status rows without deleting
// unrelated services, preserving manually-entered external config.
func (s *sqlStore) UpsertIntelEnvServices(ctx context.Context, projectID int64, services []*IntelEnvService) error {
	for _, svc := range services {
		if _, err := s.db.ExecContext(ctx, s.q(`
			DELETE FROM env_services WHERE project_id = ? AND service = ?`), projectID, svc.Service); err != nil {
			return err
		}
	}
	return s.upsertIntelEnvServices(ctx, projectID, services)
}

func (s *sqlStore) upsertIntelEnvServices(ctx context.Context, projectID int64, services []*IntelEnvService) error {
	for _, svc := range services {
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO env_services (project_id, service, category, version, provider, status,
				host, port, endpoint, healthy, container_name, container_id, username, password,
				health_check_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
			projectID, svc.Service, svc.Category, svc.Version, svc.Provider, svc.Status,
			svc.Host, svc.Port, svc.Endpoint, svc.Healthy, svc.ContainerName, svc.ContainerID,
			svc.Username, svc.Password, svc.HealthCheckAt); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelEnvServices returns the project's environment status rows ordered by
// service.
func (s *sqlStore) ListIntelEnvServices(ctx context.Context, projectID int64) ([]*IntelEnvService, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, project_id, service, category, version, provider, status, host, port,
			endpoint, healthy, container_name, container_id, username, password,
			health_check_at, created_at, updated_at
		FROM env_services WHERE project_id = ? ORDER BY service ASC`), projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelEnvService, 0)
	for rows.Next() {
		svc := &IntelEnvService{}
		if err := rows.Scan(&svc.ID, &svc.ProjectID, &svc.Service, &svc.Category, &svc.Version,
			&svc.Provider, &svc.Status, &svc.Host, &svc.Port, &svc.Endpoint, &svc.Healthy,
			&svc.ContainerName, &svc.ContainerID, &svc.Username, &svc.Password,
			&svc.HealthCheckAt, &svc.CreatedAt, &svc.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}
