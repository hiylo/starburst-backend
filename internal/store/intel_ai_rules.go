package store

import (
	"context"
	"time"
)

// IntelAIRule is one configurable AI suggestion rule (prompt rule). Rules are
// global, enable/disable-able and ordered; the scan pass runs enabled rules
// over the project code to surface risk/performance/compliance suggestions.
type IntelAIRule struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Prompt       string    `json:"prompt"`
	Scope        string    `json:"scope"`    // all | affected
	Target       string    `json:"target"`   // risk | performance | compliance
	Severity     string    `json:"severity"` // high | medium | low
	Enabled      bool      `json:"enabled"`
	SortOrder    int       `json:"sortOrder"`
	PolishedFrom string    `json:"polishedFrom"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// ListIntelAIRules returns all AI rules ordered by enabled/sort_order.
func (s *sqlStore) ListIntelAIRules(ctx context.Context, onlyEnabled bool) ([]*IntelAIRule, error) {
	query := `SELECT id, name, prompt, scope, target, severity, enabled, sort_order,
		polished_from, created_at, updated_at FROM intel_ai_rules`
	if onlyEnabled {
		query += ` WHERE enabled = TRUE`
	}
	query += ` ORDER BY enabled DESC, sort_order ASC, id ASC`
	rows, err := s.db.QueryContext(ctx, s.q(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelAIRule, 0)
	for rows.Next() {
		r := &IntelAIRule{}
		if err := rows.Scan(&r.ID, &r.Name, &r.Prompt, &r.Scope, &r.Target, &r.Severity,
			&r.Enabled, &r.SortOrder, &r.PolishedFrom, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetIntelAIRule loads one rule by id.
func (s *sqlStore) GetIntelAIRule(ctx context.Context, id int64) (*IntelAIRule, error) {
	row := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, prompt, scope, target, severity, enabled, sort_order,
			polished_from, created_at, updated_at FROM intel_ai_rules WHERE id = ?`), id)
	r := &IntelAIRule{}
	if err := row.Scan(&r.ID, &r.Name, &r.Prompt, &r.Scope, &r.Target, &r.Severity,
		&r.Enabled, &r.SortOrder, &r.PolishedFrom, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	return r, nil
}

// CreateIntelAIRule inserts a rule and fills its id.
func (s *sqlStore) CreateIntelAIRule(ctx context.Context, r *IntelAIRule) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_ai_rules (name, prompt, scope, target, severity, enabled, sort_order,
				polished_from, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			RETURNING id`),
			r.Name, r.Prompt, r.Scope, r.Target, r.Severity, r.Enabled, r.SortOrder, r.PolishedFrom,
		).Scan(&r.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_ai_rules (name, prompt, scope, target, severity, enabled, sort_order,
			polished_from, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`),
		r.Name, r.Prompt, r.Scope, r.Target, r.Severity, r.Enabled, r.SortOrder, r.PolishedFrom)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	r.ID = id
	return nil
}

// UpdateIntelAIRule persists a rule's mutable fields.
func (s *sqlStore) UpdateIntelAIRule(ctx context.Context, r *IntelAIRule) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_ai_rules SET name = ?, prompt = ?, scope = ?, target = ?, severity = ?,
			enabled = ?, sort_order = ?, polished_from = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`),
		r.Name, r.Prompt, r.Scope, r.Target, r.Severity, r.Enabled, r.SortOrder,
		r.PolishedFrom, r.ID)
	return err
}

// DeleteIntelAIRule removes a rule by id.
func (s *sqlStore) DeleteIntelAIRule(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_ai_rules WHERE id = ?`), id)
	return err
}
