package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// SessionEvent is one event captured from the upstream OpenCode global event
// stream. Payload keeps the raw event JSON so clients can render the original
// shape without the collector having to model every event type.
type SessionEvent struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"sessionId"`
	EventType string          `json:"eventType"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// InsertEvent stores one session event. The caller sets CreatedAt (the
// collector timestamps with UTC so sub-second events stay ordered and the
// since cursor never skips a row sharing the same second). On SQLite the
// timestamp is stored as plain UTC text so comparisons in ListEvents /
// DeleteEventsOlderThan stay aligned.
func (s *sqlStore) InsertEvent(ctx context.Context, e *SessionEvent) error {
	var created any = e.CreatedAt
	if !isPostgres(s.driver) {
		created = sqliteTime(e.CreatedAt)
	}
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO session_events (session_id, event_type, payload, created_at)
		VALUES (?, ?, ?, ?)`),
		e.SessionID, e.EventType, string(sanitizePayloadForJSONB(e.Payload)), created)
	return err
}

// InsertEvents stores a batch of session events with a single multi-row
// INSERT, used by the event collector's buffered flusher so high-frequency
// delta/progress bursts do not turn into one round-trip per event. An empty
// slice is a no-op. Like InsertEvent, the caller sets CreatedAt.
func (s *sqlStore) InsertEvents(ctx context.Context, events []*SessionEvent) error {
	if len(events) == 0 {
		return nil
	}
	// 单语句多值 INSERT：s.q 会按驱动 rebind 所有 ? 占位符。
	var sb strings.Builder
	sb.WriteString(`INSERT INTO session_events (session_id, event_type, payload, created_at) VALUES `)
	args := make([]any, 0, len(events)*4)
	for i, e := range events {
		if i > 0 {
			sb.WriteString(",")
		}
		var created any = e.CreatedAt
		if !isPostgres(s.driver) {
			created = sqliteTime(e.CreatedAt)
		}
		sb.WriteString("(?, ?, ?, ?)")
		args = append(args, e.SessionID, e.EventType, string(sanitizePayloadForJSONB(e.Payload)), created)
	}
	_, err := s.db.ExecContext(ctx, s.q(sb.String()), args...)
	return err
}

// sanitizePayloadForJSONB makes a raw event payload acceptable to PostgreSQL
// jsonb columns. The entire \u0000 escape (NUL) is valid JSON but PostgreSQL
// rejects it in jsonb with "unsupported Unicode escape sequence", which would
// drop the whole event; replacing it with U+FFFD keeps the rest of the payload
// intact. Safe no-op on TEXT (SQLite) since no such escape means no rewrite.
func sanitizePayloadForJSONB(b []byte) []byte {
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	return bytes.ReplaceAll(b, []byte(`\u0000`), []byte(`\ufffd`))
}

// ListEvents returns session events matching the optional session id filter and
// the optional since cutoff (exclusive), oldest first. limit caps the number of
// rows (default 200). The created_at/id ordering makes the result a stable
// forward cursor: a client passes the previous response's last createdAt as the
// next since value.
func (s *sqlStore) ListEvents(ctx context.Context, sessionID string, since time.Time, limit int) ([]*SessionEvent, error) {
	query := `SELECT id, session_id, event_type, payload, created_at FROM session_events`
	args := []any{}
	where := false
	if sessionID != "" {
		query += ` WHERE session_id = ?`
		args = append(args, sessionID)
		where = true
	}
	if !since.IsZero() {
		if !where {
			query += ` WHERE`
			where = true
		} else {
			query += ` AND`
		}
		query += ` created_at > ?`
		if isPostgres(s.driver) {
			args = append(args, since)
		} else {
			// created_at 列存 UTC 文本，游标参数须同格式，否则非 UTC 主机时区错位。
			args = append(args, sqliteTime(since))
		}
	}
	if limit <= 0 {
		limit = 200
	}

	// 无 since 时返回「最新」的 limit 条（倒序取再升序），App 首次拉取拿到最近动态；
	// 带 since 时保持 created_at ASC 的稳定正向游标。
	ascending := true
	if since.IsZero() {
		query += ` ORDER BY created_at DESC, id DESC LIMIT ` + itoa(limit)
		ascending = false
	} else {
		query += ` ORDER BY created_at ASC, id ASC LIMIT ` + itoa(limit)
	}

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*SessionEvent, 0)
	for rows.Next() {
		e := &SessionEvent{}
		var payload []byte
		if err := rows.Scan(&e.ID, &e.SessionID, &e.EventType, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	if !ascending {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, rows.Err()
}

// DeleteEventsOlderThan purges session events created strictly before cutoff,
// keeping the retention window bounded. Returns the number of rows deleted.
func (s *sqlStore) DeleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	var arg any = cutoff
	if !isPostgres(s.driver) {
		arg = sqliteTime(cutoff)
	}
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM session_events WHERE created_at < ?`), arg)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

var _ = sql.ErrNoRows
