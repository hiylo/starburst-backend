package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Archive is a stored snapshot of a remote OpenCode session.
type Archive struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Format    string    `json:"format"` // "markdown" | "json"
	Content   string    `json:"content"`
	// RawMessages 保存会话的完整结构化消息 JSON（含 info/parts），用于恢复。
	// 旧归档无该数据时为空字符串。
	RawMessages string    `json:"rawMessages,omitempty"`
	Size        int       `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
}

// CreateArchive persists an archive snapshot.
func (s *sqlStore) CreateArchive(ctx context.Context, a *Archive) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO archives (id, session_id, title, format, content, raw_messages, size, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		a.ID, a.SessionID, a.Title, a.Format, a.Content, a.RawMessages, a.Size)
	return err
}

// ListArchives returns archive metadata (without content), newest first.
func (s *sqlStore) ListArchives(ctx context.Context, limit int) ([]*Archive, error) {
	query := `SELECT id, session_id, title, format, '', '', size, created_at FROM archives`
	if limit <= 0 {
		limit = 50
	}
	query += ` ORDER BY created_at DESC LIMIT ` + itoa(limit)
	rows, err := s.db.QueryContext(ctx, s.q(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Archive, 0)
	for rows.Next() {
		a := &Archive{}
		if err := rows.Scan(&a.ID, &a.SessionID, &a.Title, &a.Format, &a.Content, &a.RawMessages, &a.Size, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetArchive loads a full archive including content.
func (s *sqlStore) GetArchive(ctx context.Context, id string) (*Archive, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, session_id, title, format, content, raw_messages, size, created_at
		FROM archives WHERE id = ?`), id)
	a := &Archive{}
	err := row.Scan(&a.ID, &a.SessionID, &a.Title, &a.Format, &a.Content, &a.RawMessages, &a.Size, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// DeleteArchive removes an archive by id. Returns ErrNotFound if missing.
func (s *sqlStore) DeleteArchive(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM archives WHERE id = ?`), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
