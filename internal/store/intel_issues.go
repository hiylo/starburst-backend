package store

import "context"

// MarkIntelIssueResolved sets an issue's status and resolved_at timestamp
// (the closed-loop acknowledgement: a human confirms the problem is handled).
// It returns ErrNotFound when no issue row matched the id.
func (s *sqlStore) MarkIntelIssueResolved(ctx context.Context, id int64, status string) error {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_issues SET status = ?, resolved_at = CURRENT_TIMESTAMP WHERE id = ?`), status, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// LinkIntelIssueFeature assigns a feature point to an issue (integration bugs
// must be anchored to a feature). It returns ErrNotFound when the issue does
// not exist.
func (s *sqlStore) LinkIntelIssueFeature(ctx context.Context, issueID, featureID int64) error {
	res, err := s.db.ExecContext(ctx, s.q(`
		UPDATE intel_issues SET feature_id = ? WHERE id = ?`), featureID, issueID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
