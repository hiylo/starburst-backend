package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Trigger kinds supported by the automation engine.
const (
	TriggerCron = "cron" // scheduled interval: "*/30 * * * * *"
	TriggerGit  = "git"  // watched repository event (push happens, etc.)
	TriggerHTTP = "http" // inbound webhook
)

// Rule is a user-defined automation rule: when a trigger fires, run a prompt
// (default) or trigger a test-intelligence regression run when IntelProjectID
// is non-zero.
type Rule struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	Kind           string     `json:"kind"`           // TriggerCron | TriggerGit | TriggerHTTP
	Schedule       string     `json:"schedule"`       // cron expression (kind=cron) or repo path (kind=git) or path (kind=http)
	Directory      string     `json:"directory"`      // working directory for the generated task
	SessionID      string     `json:"sessionId"`      // 固定会话：触发时写入该会话（空=每次新建会话）
	Prompt         string     `json:"prompt"`         // prompt template sent to the agent
	IntelProjectID int64      `json:"intelProjectId"` // 非 0：触发 intel run-all 回归（替代 prompt 任务）
	Enabled        bool       `json:"enabled"`
	LastFiredAt    *time.Time `json:"lastFiredAt"`
	CreatedAt      time.Time  `json:"createdAt"`
}

// CreateRule persists a new rule.
func (s *sqlStore) CreateRule(ctx context.Context, r *Rule) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO rules (id, name, kind, schedule, directory, session_id, prompt, intel_project_id, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		r.ID, r.Name, r.Kind, r.Schedule, r.Directory, r.SessionID, r.Prompt, r.IntelProjectID, r.Enabled)
	return err
}

// ListRules returns all rules, enabled first then by creation time.
func (s *sqlStore) ListRules(ctx context.Context) ([]*Rule, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, kind, schedule, directory, session_id, prompt, intel_project_id, enabled, last_fired_at, created_at
		FROM rules ORDER BY enabled DESC, created_at ASC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Rule, 0)
	for rows.Next() {
		r := &Rule{}
		var last *time.Time
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.Schedule, &r.Directory,
			&r.SessionID, &r.Prompt, &r.IntelProjectID, &r.Enabled, &last, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.LastFiredAt = last
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRule loads a single rule.
func (s *sqlStore) GetRule(ctx context.Context, id string) (*Rule, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, kind, schedule, directory, session_id, prompt, intel_project_id, enabled, last_fired_at, created_at
		FROM rules WHERE id = ?`), id)
	r := &Rule{}
	var last *time.Time
	err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Schedule, &r.Directory,
		&r.SessionID, &r.Prompt, &r.IntelProjectID, &r.Enabled, &last, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	r.LastFiredAt = last
	return r, err
}

// DeleteRule removes a rule by id. Returns ErrNotFound if missing.
func (s *sqlStore) DeleteRule(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM rules WHERE id = ?`), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkRuleFired records when a rule last created a task.
func (s *sqlStore) MarkRuleFired(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE rules SET last_fired_at = CURRENT_TIMESTAMP WHERE id = ?`), id)
	return err
}
