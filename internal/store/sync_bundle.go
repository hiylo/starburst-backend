package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SyncBundle is the latest per-key JSON snapshot shared across devices through
// the backend. Each key (a server id or "global") keeps exactly one row; a push
// overwrites it and bumps the revision so the app can detect drift from its own
// base revision and re-pull when another device wrote after it.
type SyncBundle struct {
	Key       string    `json:"key"`
	Payload   string    `json:"payload"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// GetSyncBundle returns the latest bundle for key, or ErrNotFound when the key
// has never been pushed.
func (s *sqlStore) GetSyncBundle(ctx context.Context, key string) (*SyncBundle, error) {
	b := &SyncBundle{}
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT key, payload, revision, updated_at FROM sync_bundle WHERE key = ?`),
		key,
	).Scan(&b.Key, &b.Payload, &b.Revision, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// PutSyncBundle upserts a bundle under key (last-write-wins) and returns the new
// revision. A fresh key starts at revision 1; every subsequent write increments
// it so callers get a strictly increasing version number.
func (s *sqlStore) PutSyncBundle(ctx context.Context, key, payload string) (int64, error) {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO sync_bundle (key, payload, revision, updated_at)
		VALUES (?, ?, 1, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET
			payload = excluded.payload,
			revision = sync_bundle.revision + 1,
			updated_at = CURRENT_TIMESTAMP`),
		key, payload,
	)
	if err != nil {
		return 0, err
	}
	var rev int64
	if err := s.db.QueryRowContext(ctx, s.q(
		`SELECT revision FROM sync_bundle WHERE key = ?`), key).Scan(&rev); err != nil {
		return 0, err
	}
	return rev, nil
}
