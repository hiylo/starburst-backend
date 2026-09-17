package store

import (
	"context"
	"time"
)

// IntelFeatureChat is one per-feature AI Q&A record. The automatically
// assembled context (endpoint contracts, latest test results, linked issues) is
// kept next to the question and answer so conclusions stay reproducible.
type IntelFeatureChat struct {
	ID          int64     `json:"id"`
	FeatureID   int64     `json:"featureId"`
	ProjectID   int64     `json:"projectId"`
	Question    string    `json:"question"`
	ContextJSON string    `json:"contextJson"`
	Answer      string    `json:"answer"`
	CreatedAt   time.Time `json:"createdAt"`
}

// CreateIntelFeatureChat persists a feature Q&A and fills its id.
func (s *sqlStore) CreateIntelFeatureChat(ctx context.Context, c *IntelFeatureChat) error {
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_feature_chats (feature_id, project_id, question, context_json, answer, created_at)
			VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			RETURNING id`),
			c.FeatureID, c.ProjectID, c.Question, c.ContextJSON, c.Answer,
		).Scan(&c.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_feature_chats (feature_id, project_id, question, context_json, answer, created_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		c.FeatureID, c.ProjectID, c.Question, c.ContextJSON, c.Answer)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	c.ID = id
	return nil
}

// ListIntelFeatureChats returns a feature's Q&A history, newest first.
func (s *sqlStore) ListIntelFeatureChats(ctx context.Context, projectID, featureID int64) ([]*IntelFeatureChat, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, feature_id, project_id, question, context_json, answer, created_at
		FROM intel_feature_chats WHERE project_id = ? AND feature_id = ?
		ORDER BY id DESC`), projectID, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*IntelFeatureChat, 0)
	for rows.Next() {
		c := &IntelFeatureChat{}
		if err := rows.Scan(&c.ID, &c.FeatureID, &c.ProjectID, &c.Question,
			&c.ContextJSON, &c.Answer, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
