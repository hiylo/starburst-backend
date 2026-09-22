package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// AuditEntry records a single authenticated API action for accountability.
type AuditEntry struct {
	ID        int64     `json:"id"`
	TokenID   string    `json:"tokenId"`
	TokenName string    `json:"tokenName"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Status    int       `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// RecordAudit inserts an audit entry.
func (s *sqlStore) RecordAudit(ctx context.Context, e *AuditEntry) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO audit_log (token_id, token_name, method, path, status, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		e.TokenID, e.TokenName, e.Method, e.Path, e.Status)
	return err
}

// RecordAudits inserts many audit rows in one multi-row INSERT, used by the
// async audit flusher to keep write amplification low under polling.
func (s *sqlStore) RecordAudits(ctx context.Context, entries []*AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString(`INSERT INTO audit_log (token_id, token_name, method, path, status, created_at) VALUES `)
	args := make([]any, 0, len(entries)*5)
	for i, e := range entries {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("(?, ?, ?, ?, ?, CURRENT_TIMESTAMP)")
		args = append(args, e.TokenID, e.TokenName, e.Method, e.Path, e.Status)
	}
	_, err := s.db.ExecContext(ctx, s.q(sb.String()), args...)
	return err
}

// ListAudit returns the most recent audit entries (newest first), optionally
// filtered by token id. limit caps the number of rows.
func (s *sqlStore) ListAudit(ctx context.Context, tokenID string, limit int) ([]*AuditEntry, error) {
	query := `SELECT id, token_id, token_name, method, path, status, created_at FROM audit_log`
	args := []any{}
	if tokenID != "" {
		query += ` WHERE token_id = ?`
		args = append(args, tokenID)
	}
	query += ` ORDER BY id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*AuditEntry, 0)
	for rows.Next() {
		e := &AuditEntry{}
		if err := rows.Scan(&e.ID, &e.TokenID, &e.TokenName, &e.Method, &e.Path, &e.Status, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecordAuditFn is a lightweight hook signature for middleware.
type RecordAuditFn func(ctx context.Context, tokenID, tokenName, method, path string, status int)

// DeleteAuditOlderThan purges audit entries older than cutoff.
func (s *sqlStore) DeleteAuditOlderThan(ctx context.Context, cutoff time.Time) (int, error) {
	var arg any = cutoff
	if !isPostgres(s.driver) {
		// audit_log.created_at 是 CURRENT_TIMESTAMP 的 UTC 文本，比较参数须同格式。
		arg = sqliteTime(cutoff)
	}
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM audit_log WHERE created_at <= ?`), arg)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

var _ = sql.ErrNoRows // keep import if refactored
