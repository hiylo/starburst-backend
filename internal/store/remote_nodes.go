package store

import (
	"context"
	"time"
)

// RemoteNode is one SSH reachable test-execution node with capability labels
// (ios-xcode / android-sdk / linux-docker) used to route test runs off the
// local machine when the environment gate cannot be satisfied locally.
type RemoteNode struct {
	ID           int64      `json:"id"`
	Name         string     `json:"name"`
	Host         string     `json:"host"`
	Port         int        `json:"port"`
	User         string     `json:"user"`
	Auth         string     `json:"-"`
	HostKeyFP    string     `json:"hostKeyFp"`
	Capabilities string     `json:"capabilities"`
	Reachable    bool       `json:"reachable"`
	LastCheckAt  *time.Time `json:"lastCheckAt"`
	WorkDir      string     `json:"workDir"`
	Note         string     `json:"note"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// ListRemoteNodes returns all remote nodes, newest first.
func (s *sqlStore) ListRemoteNodes(ctx context.Context) ([]*RemoteNode, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, host, port, user, auth, host_key_fp, capabilities,
			reachable, last_check_at, work_dir, note, created_at FROM remote_nodes
		ORDER BY id DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RemoteNode, 0)
	for rows.Next() {
		n := &RemoteNode{}
		if err := rows.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.User, &n.Auth,
			&n.HostKeyFP, &n.Capabilities, &n.Reachable, &n.LastCheckAt, &n.WorkDir, &n.Note, &n.CreatedAt); err != nil {
			return nil, err
		}
		if dec, derr := s.decryptSecret(ctx, n.Auth); derr == nil {
			n.Auth = dec
		} else {
			return nil, derr
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetRemoteNode loads one node by id.
func (s *sqlStore) GetRemoteNode(ctx context.Context, id int64) (*RemoteNode, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, host, port, user, auth, host_key_fp, capabilities,
			reachable, last_check_at, work_dir, note, created_at FROM remote_nodes WHERE id = ?`), id)
	n := &RemoteNode{}
	if err := row.Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.User, &n.Auth,
		&n.HostKeyFP, &n.Capabilities, &n.Reachable, &n.LastCheckAt, &n.WorkDir, &n.Note, &n.CreatedAt); err != nil {
		return nil, err
	}
	if dec, derr := s.decryptSecret(ctx, n.Auth); derr == nil {
		n.Auth = dec
	} else {
		return nil, derr
	}
	return n, nil
}

// CreateRemoteNode inserts a node and fills its id.
func (s *sqlStore) CreateRemoteNode(ctx context.Context, n *RemoteNode) error {
	encAuth, err := s.encryptSecret(ctx, n.Auth)
	if err != nil {
		return err
	}
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO remote_nodes (name, host, port, user, auth, host_key_fp, capabilities,
				reachable, last_check_at, work_dir, note, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			n.Name, n.Host, n.Port, n.User, encAuth, n.HostKeyFP, n.Capabilities,
			n.Reachable, n.LastCheckAt, n.WorkDir, n.Note,
		).Scan(&n.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO remote_nodes (name, host, port, user, auth, host_key_fp, capabilities,
			reachable, last_check_at, work_dir, note, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		n.Name, n.Host, n.Port, n.User, encAuth, n.HostKeyFP, n.Capabilities,
		n.Reachable, n.LastCheckAt, n.WorkDir, n.Note)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	n.ID = id
	return nil
}

// UpdateRemoteNode persists a node's mutable fields (reachability, note).
func (s *sqlStore) UpdateRemoteNode(ctx context.Context, n *RemoteNode) error {
	encAuth, err := s.encryptSecret(ctx, n.Auth)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, s.q(`
		UPDATE remote_nodes SET name = ?, host = ?, port = ?, user = ?, auth = ?,
			host_key_fp = ?, capabilities = ?, reachable = ?, last_check_at = ?, work_dir = ?, note = ?
		WHERE id = ?`),
		n.Name, n.Host, n.Port, n.User, encAuth, n.HostKeyFP, n.Capabilities,
		n.Reachable, n.LastCheckAt, n.WorkDir, n.Note, n.ID)
	return err
}

// DeleteRemoteNode removes a node by id.
func (s *sqlStore) DeleteRemoteNode(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM remote_nodes WHERE id = ?`), id)
	return err
}
