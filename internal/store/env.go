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
	// 空列表保护：同 ReplaceIntelEntities——无结果保留旧快照，避免误清空。
	if len(reqs) == 0 {
		return nil
	}
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
	// 空列表保护：同 ReplaceIntelEntities——无结果保留旧快照，避免误清空。
	if len(services) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM env_services WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	return s.upsertIntelEnvServices(ctx, projectID, services)
}

// UpsertIntelEnvServices upserts per-service status rows without deleting
// unrelated services, preserving manually-entered external config. The natural
// key (project_id, service) is UNIQUE, so INSERT ... ON CONFLICT keeps it
// race-free where the old DELETE+INSERT could double-insert under concurrency.
func (s *sqlStore) UpsertIntelEnvServices(ctx context.Context, projectID int64, services []*IntelEnvService) error {
	return s.upsertIntelEnvServices(ctx, projectID, services)
}

func (s *sqlStore) upsertIntelEnvServices(ctx context.Context, projectID int64, services []*IntelEnvService) error {
	for _, svc := range services {
		encPassword, err := s.encryptSecret(ctx, svc.Password)
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO env_services (project_id, service, category, version, provider, status,
				host, port, endpoint, healthy, container_name, container_id, username, password,
				health_check_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT (project_id, service) DO UPDATE SET
				category = excluded.category, version = excluded.version, provider = excluded.provider,
				status = excluded.status, host = excluded.host, port = excluded.port,
				endpoint = excluded.endpoint, healthy = excluded.healthy,
				container_name = excluded.container_name, container_id = excluded.container_id,
				username = excluded.username, password = excluded.password,
				health_check_at = excluded.health_check_at, updated_at = CURRENT_TIMESTAMP`),
			projectID, svc.Service, svc.Category, svc.Version, svc.Provider, svc.Status,
			svc.Host, svc.Port, svc.Endpoint, svc.Healthy, svc.ContainerName, svc.ContainerID,
			svc.Username, encPassword, svc.HealthCheckAt); err != nil {
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
		if dec, derr := s.decryptSecret(ctx, svc.Password); derr == nil {
			svc.Password = dec
		} else {
			return nil, derr
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}
