package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// WebSession is a persisted web admin login session.
type WebSession struct {
	ID        string
	ExpiresAt time.Time
}

// CreateWebSession stores a web session with an expiry.
func (s *sqlStore) CreateWebSession(ctx context.Context, id string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO web_sessions (id, expires_at) VALUES (?, ?)`), id, expiresAt)
	return err
}

// GetWebSession returns a session by id, validating expiry in Go (avoids
// SQLite vs PG timestamp comparison pitfalls).
func (s *sqlStore) GetWebSession(ctx context.Context, id string) (*WebSession, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, expires_at FROM web_sessions WHERE id = ?`), id)
	ws := &WebSession{}
	err := row.Scan(&ws.ID, &ws.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if time.Now().After(ws.ExpiresAt) {
		return nil, ErrNotFound
	}
	return ws, nil
}

// DeleteWebSession removes a session (logout).
func (s *sqlStore) DeleteWebSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM web_sessions WHERE id = ?`), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteExpiredWebSessions purges sessions expired before now; returns count.
func (s *sqlStore) DeleteExpiredWebSessions(ctx context.Context) (int, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM web_sessions WHERE expires_at <= ?`), time.Now())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RevokeWebSessionsExcept drops every session but keepID; returns count.
func (s *sqlStore) RevokeWebSessionsExcept(ctx context.Context, keepID string) (int, error) {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM web_sessions WHERE id <> ?`), keepID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
