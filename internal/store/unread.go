package store

import (
	"context"
	"strings"
)

// sessionUnreadFlag is the boolean column value used when a session has new
// activity. Only the "read by any end" transition clears it (MarkSessionRead).

// SetSessionsUnread upserts unread=1 for the given session ids in one
// multi-row INSERT ... ON CONFLICT (session_id) DO UPDATE, so repeated
// activity on the same session does not duplicate rows. An empty slice is a
// no-op. The UPSERT syntax is supported by both PostgreSQL and SQLite 3.24+
// (including the modernc.org/sqlite build used here).
func (s *sqlStore) SetSessionsUnread(ctx context.Context, sessionIDs []string) error {
	if len(sessionIDs) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO session_unread (session_id, unread, updated_at) VALUES `)
	args := make([]any, 0, len(sessionIDs))
	for i, id := range sessionIDs {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`(?, TRUE, CURRENT_TIMESTAMP)`)
		args = append(args, id)
	}
	sb.WriteString(` ON CONFLICT (session_id) DO UPDATE SET unread = TRUE, updated_at = CURRENT_TIMESTAMP`)
	_, err := s.db.ExecContext(ctx, s.q(sb.String()), args...)
	return err
}

// ListUnread returns the set of session ids currently flagged unread=1.
func (s *sqlStore) ListUnread(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT session_id FROM session_unread WHERE unread = TRUE`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkSessionRead clears the unread flag for a session (row is removed so a
// later activity can insert a fresh one).
func (s *sqlStore) MarkSessionRead(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM session_unread WHERE session_id = ?`), sessionID)
	return err
}
