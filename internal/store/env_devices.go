package store

import (
	"context"
	"time"
)

// IntelDevice is one Android device/emulator that can be bound to a project for
// client tests. Binding is explicit and persists (手动才变).
type IntelDevice struct {
	ID         int64      `json:"id"`
	ProjectID  int64      `json:"projectId"`
	Name       string     `json:"name"`
	Method     string     `json:"method"` // usb | wireless | emulator
	ADBHost    string     `json:"adbHost"`
	ADBPort    int        `json:"adbPort"`
	Serial     string     `json:"serial"`
	AVD        string     `json:"avd"`
	Bound      bool       `json:"bound"`
	Status     string     `json:"status"` // connected | disconnected
	LastSeenAt *time.Time `json:"lastSeenAt"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// ReplaceIntelDevices deletes the project's device rows and re-inserts the
// given set (a rescan/refresh replaces the snapshot).
func (s *sqlStore) ReplaceIntelDevices(ctx context.Context, projectID int64, devs []*IntelDevice) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM env_devices WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	return s.upsertIntelDevices(ctx, devs)
}

// UpsertIntelDevices upserts devices by serial, preserving binding state. The
// serial key is UNIQUE, so INSERT ... ON CONFLICT is race-free where the old
// DELETE+INSERT could double-insert under concurrency.
func (s *sqlStore) UpsertIntelDevices(ctx context.Context, devs []*IntelDevice) error {
	return s.upsertIntelDevices(ctx, devs)
}

func (s *sqlStore) upsertIntelDevices(ctx context.Context, devs []*IntelDevice) error {
	for _, d := range devs {
		if d.Serial == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, s.q(`
			INSERT INTO env_devices (project_id, name, method, adb_host, adb_port, serial,
				avd, bound, status, last_seen_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT (serial) DO UPDATE SET
				project_id = excluded.project_id, name = excluded.name, method = excluded.method,
				adb_host = excluded.adb_host, adb_port = excluded.adb_port, avd = excluded.avd,
				bound = excluded.bound, status = excluded.status, last_seen_at = excluded.last_seen_at`),
			d.ProjectID, d.Name, d.Method, d.ADBHost, d.ADBPort, d.Serial,
			d.AVD, d.Bound, d.Status, d.LastSeenAt); err != nil {
			return err
		}
	}
	return nil
}

// ListIntelDevices returns all devices, optionally scoped to a project binding.
func (s *sqlStore) ListIntelDevices(ctx context.Context, projectID int64) ([]*IntelDevice, error) {
	query := `SELECT id, project_id, name, method, adb_host, adb_port, serial, avd,
		bound, status, last_seen_at, created_at FROM env_devices`
	args := []any{}
	if projectID > 0 {
		query += ` WHERE project_id = ?`
		args = append(args, projectID)
	}
	query += ` ORDER BY serial ASC`
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelDevice, 0)
	for rows.Next() {
		d := &IntelDevice{}
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Method, &d.ADBHost,
			&d.ADBPort, &d.Serial, &d.AVD, &d.Bound, &d.Status, &d.LastSeenAt, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetIntelDevice loads one device by id.
func (s *sqlStore) GetIntelDevice(ctx context.Context, id int64) (*IntelDevice, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, project_id, name, method, adb_host, adb_port, serial, avd,
			bound, status, last_seen_at, created_at FROM env_devices WHERE id = ?`), id)
	d := &IntelDevice{}
	if err := row.Scan(&d.ID, &d.ProjectID, &d.Name, &d.Method, &d.ADBHost,
		&d.ADBPort, &d.Serial, &d.AVD, &d.Bound, &d.Status, &d.LastSeenAt, &d.CreatedAt); err != nil {
		return nil, err
	}
	return d, nil
}

// UpdateIntelDevice persists a device's mutable fields (binding/status).
func (s *sqlStore) UpdateIntelDevice(ctx context.Context, d *IntelDevice) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE env_devices SET project_id = ?, name = ?, method = ?, adb_host = ?,
			adb_port = ?, serial = ?, avd = ?, bound = ?, status = ?, last_seen_at = ?
		WHERE id = ?`),
		d.ProjectID, d.Name, d.Method, d.ADBHost, d.ADBPort, d.Serial, d.AVD,
		d.Bound, d.Status, d.LastSeenAt, d.ID)
	return err
}

// DeleteIntelDevice removes a device by id.
func (s *sqlStore) DeleteIntelDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM env_devices WHERE id = ?`), id)
	return err
}
