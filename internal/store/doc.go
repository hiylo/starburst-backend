package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DocDocument is a generated document record. The rendered file itself lives on
// disk under the configured docs-dir (keyed by id + type extension); this row
// carries the metadata plus the LLM skeleton needed for regeneration.
type DocDocument struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	DocType   string    `json:"docType"` // xlsx | docx | pptx
	Prompt    string    `json:"prompt"`
	Skeleton  string    `json:"skeleton,omitempty"`
	SizeBytes int64     `json:"sizeBytes"`
	Status    string    `json:"status"` // created | ready | failed
	Error     string    `json:"error,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CreateDocDocument persists a generated-document record and returns it with id.
func (s *sqlStore) CreateDocDocument(ctx context.Context, d *DocDocument) error {
	id, err := s.insertReturningID(ctx, `
		INSERT INTO doc_documents (name, doc_type, prompt, skeleton, status)
		VALUES (?, ?, ?, ?, ?)`,
		d.Name, d.DocType, d.Prompt, d.Skeleton, d.Status)
	if err != nil {
		return err
	}
	d.ID = id
	return nil
}

// GetDocDocument loads a generated document by id; ErrNotFound when missing.
func (s *sqlStore) GetDocDocument(ctx context.Context, id int64) (*DocDocument, error) {
	d := &DocDocument{}
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, doc_type, prompt, skeleton, size_bytes, status, error, created_at, updated_at
		FROM doc_documents WHERE id = ?`), id).
		Scan(&d.ID, &d.Name, &d.DocType, &d.Prompt, &d.Skeleton, &d.SizeBytes,
			&d.Status, &d.Error, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// ListDocDocuments returns generated documents, newest first (default 50, max 200).
func (s *sqlStore) ListDocDocuments(ctx context.Context, limit int) ([]*DocDocument, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, doc_type, prompt, skeleton, size_bytes, status, error, created_at, updated_at
		FROM doc_documents ORDER BY id DESC LIMIT ?`), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*DocDocument{}
	for rows.Next() {
		d := &DocDocument{}
		if err := rows.Scan(&d.ID, &d.Name, &d.DocType, &d.Prompt, &d.Skeleton, &d.SizeBytes,
			&d.Status, &d.Error, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UpdateDocDocumentResult persists the rendered outcome: name/type/size/status
// plus an optional error message.
func (s *sqlStore) UpdateDocDocumentResult(ctx context.Context, d *DocDocument) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE doc_documents SET name = ?, doc_type = ?, skeleton = ?, size_bytes = ?,
			status = ?, error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`),
		d.Name, d.DocType, d.Skeleton, d.SizeBytes, d.Status, d.Error, d.ID)
	return err
}

// DeleteDocDocument removes a generated-document record (the on-disk file is
// removed by the caller).
func (s *sqlStore) DeleteDocDocument(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM doc_documents WHERE id = ?`), id)
	return err
}
