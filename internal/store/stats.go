package store

import (
	"context"
)

// TaskStats summarizes task counts by status.
type TaskStats struct {
	Queued    int `json:"queued"`
	Running   int `json:"running"`
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
	Canceled  int `json:"canceled"`
	Pending   int `json:"pending"` // waiting for its DependsOn task to succeed
	Blocked   int `json:"blocked"` // upstream failed, can never run
	Retried   int `json:"retried"` // tasks with attempts > 1 among all tasks
	Total     int `json:"total"`
}

// TokenUsage groups task counts by the owning token (via audit joins) and is
// intentionally kept simple: tasks are attributed to the token that created
// them is not tracked per-task, so this reports audit volume per token instead.
type TokenUsage struct {
	TokenID   string `json:"tokenId"`
	TokenName string `json:"tokenName"`
	Calls     int    `json:"calls"`
}

// TaskStats returns aggregate counters over the tasks table.
func (s *sqlStore) TaskStats(ctx context.Context) (*TaskStats, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT status, COUNT(*) FROM tasks GROUP BY status`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	st := &TaskStats{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		st.Total += n
		switch status {
		case TaskQueued:
			st.Queued = n
		case TaskRunning:
			st.Running = n
		case TaskSucceeded:
			st.Succeeded = n
		case TaskFailed:
			st.Failed = n
		case TaskCanceled:
			st.Canceled = n
		case TaskPending:
			st.Pending = n
		case TaskBlocked:
			st.Blocked = n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Count tasks that were retried (attempts > 1).
	if err := s.db.QueryRowContext(ctx, s.q(`
		SELECT COUNT(*) FROM tasks WHERE attempts > 1`)).Scan(&st.Retried); err != nil {
		return nil, err
	}
	return st, nil
}

// TokenUsageByAudit aggregates audit entries per token, newest active first.
func (s *sqlStore) TokenUsageByAudit(ctx context.Context, limit int) ([]*TokenUsage, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT token_id, token_name, COUNT(*) AS calls
		FROM audit_log GROUP BY token_id, token_name ORDER BY calls DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*TokenUsage, 0)
	for rows.Next() {
		u := &TokenUsage{}
		if err := rows.Scan(&u.TokenID, &u.TokenName, &u.Calls); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountArchives returns the number of stored session archives.
func (s *sqlStore) CountArchives(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM archives`)).Scan(&n)
	return n, err
}
