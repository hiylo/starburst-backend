package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// KBCollection is a client-managed knowledge base collection: a named grouping
// of documents whose chunks are embedded and searched together.
type KBCollection struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	DocumentCount int64     `json:"documentCount"`
	ChunkCount    int64     `json:"chunkCount"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// KBDocument is a single ingested source (text/markdown for now; PDF/Word/Excel
// parsing lands with internal/doc). Its chunks live in kb_chunks.
type KBDocument struct {
	ID           int64     `json:"id"`
	CollectionID int64     `json:"collectionId"`
	Name         string    `json:"name"`
	MIME         string    `json:"mime"`
	SizeBytes    int64     `json:"sizeBytes"`
	Status       string    `json:"status"` // pending | indexed | failed
	ChunkCount   int64     `json:"chunkCount"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// KBChunk is one embedded fragment of a KB document. Content is embedded into
// the vector column and retrieved by cosine similarity, exactly like the intel
// RagChunk flow.
type KBChunk struct {
	ID           int64     `json:"id"`
	CollectionID int64     `json:"collectionId"`
	DocumentID   int64     `json:"documentId"`
	Seq          int       `json:"seq"`
	Title        string    `json:"title"`
	Content      string    `json:"content"`
	Embedding    []float32 `json:"-"`
	Similarity   float64   `json:"similarity,omitempty"`
	Source       string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// insertReturningID runs an INSERT and returns the new row id: PostgreSQL uses
// `RETURNING id` (pgx has no LastInsertId), SQLite uses LastInsertId.
func (s *sqlStore) insertReturningID(ctx context.Context, query string, args ...any) (int64, error) {
	if isPostgres(s.driver) {
		var id int64
		if err := s.db.QueryRowContext(ctx, s.q(query+" RETURNING id"), args...).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	res, err := s.db.ExecContext(ctx, s.q(query), args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CreateKBCollection persists a new knowledge base collection. The name is
// unique; a duplicate returns ErrConflict.
func (s *sqlStore) CreateKBCollection(ctx context.Context, name, description string) (*KBCollection, error) {
	id, err := s.insertReturningID(ctx,
		`INSERT INTO kb_collections (name, description) VALUES (?, ?)`, name, description)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.GetKBCollection(ctx, id)
}

// GetKBCollection loads a collection by id; returns ErrNotFound when missing.
func (s *sqlStore) GetKBCollection(ctx context.Context, id int64) (*KBCollection, error) {
	c := &KBCollection{}
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, name, description, document_count, chunk_count, created_at, updated_at
		FROM kb_collections WHERE id = ?`), id).
		Scan(&c.ID, &c.Name, &c.Description, &c.DocumentCount, &c.ChunkCount, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return c, nil
}

// UpdateKBCollection renames a collection and/or replaces its description.
// Empty name keeps the old one; a name collision with another collection
// returns ErrConflict. Returns the refreshed collection.
func (s *sqlStore) UpdateKBCollection(ctx context.Context, id int64, name, description string) (*KBCollection, error) {
	cur, err := s.GetKBCollection(ctx, id)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = cur.Name
	}
	if _, err := s.db.ExecContext(ctx, s.q(
		`UPDATE kb_collections SET name = ?, description = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`),
		name, strings.TrimSpace(description), id); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return s.GetKBCollection(ctx, id)
}

// ListKBCollections returns all collections, newest first.
func (s *sqlStore) ListKBCollections(ctx context.Context) ([]*KBCollection, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, name, description, document_count, chunk_count, created_at, updated_at
		FROM kb_collections ORDER BY id DESC`))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KBCollection{}
	for rows.Next() {
		c := &KBCollection{}
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.DocumentCount,
			&c.ChunkCount, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteKBCollection removes a collection and all of its documents and chunks.
func (s *sqlStore) DeleteKBCollection(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, s.q(
		`DELETE FROM kb_chunks WHERE collection_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(
		`DELETE FROM kb_documents WHERE collection_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM kb_collections WHERE id = ?`), id); err != nil {
		return err
	}
	return nil
}

// CreateKBDocument persists a pending ingestion record and returns it with id.
func (s *sqlStore) CreateKBDocument(ctx context.Context, doc *KBDocument) error {
	id, err := s.insertReturningID(ctx, `
		INSERT INTO kb_documents (collection_id, name, mime, size_bytes, status)
		VALUES (?, ?, ?, ?, ?)`,
		doc.CollectionID, doc.Name, doc.MIME, doc.SizeBytes, doc.Status)
	if err != nil {
		return err
	}
	doc.ID = id
	return nil
}

// GetKBDocument loads a document by id; returns ErrNotFound when missing.
func (s *sqlStore) GetKBDocument(ctx context.Context, id int64) (*KBDocument, error) {
	d := &KBDocument{}
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, collection_id, name, mime, size_bytes, status, chunk_count, error, created_at, updated_at
		FROM kb_documents WHERE id = ?`), id).
		Scan(&d.ID, &d.CollectionID, &d.Name, &d.MIME, &d.SizeBytes, &d.Status,
			&d.ChunkCount, &d.Error, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// ListKBDocuments returns a collection's documents, newest first, skipping
// `offset` rows and taking at most `limit` rows (default 50, max 200).
func (s *sqlStore) ListKBDocuments(ctx context.Context, collectionID int64, limit, offset int) ([]*KBDocument, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, s.q(`
		SELECT id, collection_id, name, mime, size_bytes, status, chunk_count, error, created_at, updated_at
		FROM kb_documents WHERE collection_id = ? ORDER BY id DESC LIMIT ? OFFSET ?`), collectionID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*KBDocument{}
	for rows.Next() {
		d := &KBDocument{}
		if err := rows.Scan(&d.ID, &d.CollectionID, &d.Name, &d.MIME, &d.SizeBytes,
			&d.Status, &d.ChunkCount, &d.Error, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CountKBDocuments returns how many documents a collection holds, used to
// render pagination alongside ListKBDocuments.
func (s *sqlStore) CountKBDocuments(ctx context.Context, collectionID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*) FROM kb_documents WHERE collection_id = ?`), collectionID).Scan(&n)
	return n, err
}

// FindKBDocumentByName resolves a document by (collection, name), returning
// ErrNotFound when absent. The name match ignores surrounding whitespace.
func (s *sqlStore) FindKBDocumentByName(ctx context.Context, collectionID int64, name string) (*KBDocument, error) {
	d := &KBDocument{}
	err := s.db.QueryRowContext(ctx, s.q(`
		SELECT id, collection_id, name, mime, size_bytes, status, chunk_count, error, created_at, updated_at
		FROM kb_documents WHERE collection_id = ? AND name = ? LIMIT 1`), collectionID, strings.TrimSpace(name)).
		Scan(&d.ID, &d.CollectionID, &d.Name, &d.MIME, &d.SizeBytes, &d.Status,
			&d.ChunkCount, &d.Error, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// UpdateKBDocumentResult persists the terminal (or in-progress) state of an
// ingestion: status plus the chunk count and an optional error message.
func (s *sqlStore) UpdateKBDocumentResult(ctx context.Context, id int64, status string, chunkCount int64, errMsg string) error {
	query := `UPDATE kb_documents SET status = ?, chunk_count = ?, error = '', updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	args := []any{status, chunkCount, id}
	if errMsg != "" {
		query = `UPDATE kb_documents SET status = ?, chunk_count = ?, error = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
		args = []any{status, chunkCount, errMsg, id}
	}
	_, err := s.db.ExecContext(ctx, s.q(query), args...)
	return err
}

// DeleteKBDocument removes a document and all of its chunks, then refreshes
// the parent collection's counters.
func (s *sqlStore) DeleteKBDocument(ctx context.Context, id int64) error {
	collectionID, err := s.kbDocumentCollectionID(ctx, id)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(
		`DELETE FROM kb_chunks WHERE document_id = ?`), id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM kb_documents WHERE id = ?`), id); err != nil {
		return err
	}
	if collectionID > 0 {
		return s.refreshKBCounters(ctx, collectionID)
	}
	return nil
}

// kbDocumentCollectionID resolves a document's collection (0 when the document
// is already gone — treat as a no-op delete).
func (s *sqlStore) kbDocumentCollectionID(ctx context.Context, id int64) (int64, error) {
	var collectionID int64
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT collection_id FROM kb_documents WHERE id = ?`), id).Scan(&collectionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return collectionID, nil
}

// ReplaceKBDocumentChunks rebuilds a document's chunks: delete the old set and
// insert the given one (with embeddings) in one pass.
func (s *sqlStore) ReplaceKBDocumentChunks(ctx context.Context, doc *KBDocument, chunks []*KBChunk) error {
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM kb_chunks WHERE document_id = ?`), doc.ID); err != nil {
		return err
	}
	for _, c := range chunks {
		c.CollectionID = doc.CollectionID
		c.DocumentID = doc.ID
		if err := s.insertKBChunk(ctx, c); err != nil {
			return err
		}
	}
	// Keep the collection counters in sync so list/delete show useful numbers
	// without scanning kb_chunks every request.
	if err := s.refreshKBCounters(ctx, doc.CollectionID); err != nil {
		return err
	}
	return nil
}

func (s *sqlStore) insertKBChunk(ctx context.Context, c *KBChunk) error {
	vec := VectorString(c.Embedding)
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO kb_chunks (collection_id, document_id, seq, title, content, embedding, created_at)
			VALUES (?, ?, ?, ?, ?, ?::vector, CURRENT_TIMESTAMP) RETURNING id`),
			c.CollectionID, c.DocumentID, c.Seq, c.Title, c.Content, vec,
		).Scan(&c.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO kb_chunks (collection_id, document_id, seq, title, content, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		c.CollectionID, c.DocumentID, c.Seq, c.Title, c.Content, vec)
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

// SearchKBChunks returns the chunks most similar to the query embedding,
// optionally narrowed to a set of collections (empty = every collection),
// ordered by cosine similarity descending. Vector retrieval is
// PostgreSQL/pgvector only, mirroring SearchRagChunks.
func (s *sqlStore) SearchKBChunks(ctx context.Context, collectionIDs []int64, embedding []float32, limit int) ([]*KBChunk, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	if !isPostgres(s.driver) {
		return nil, ErrRagUnsupported
	}
	query := `SELECT c.id, c.collection_id, c.document_id, c.seq, c.title, c.content,
		1 - (c.embedding <=> ?::vector) AS similarity,
		COALESCE(d.name, '')
		FROM kb_chunks c
		LEFT JOIN kb_documents d ON d.id = c.document_id`
	args := []any{VectorString(embedding)}
	if len(collectionIDs) > 0 {
		placeholders := ""
		for i := range collectionIDs {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, collectionIDs[i])
		}
		query += ` WHERE c.collection_id IN (` + placeholders + `)`
	}
	query += ` ORDER BY c.embedding <=> ?::vector LIMIT ?`
	args = append(args, VectorString(embedding), limit)

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*KBChunk, 0, limit)
	for rows.Next() {
		c := &KBChunk{}
		if err := rows.Scan(&c.ID, &c.CollectionID, &c.DocumentID, &c.Seq,
			&c.Title, &c.Content, &c.Similarity, &c.Source); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteKBDocumentsByNameSuffix removes every document in a collection whose
// name ends with suffix, cascading their chunks. It backs the generated-document
// reverse-ingest: a regenerated doc keeps its `-@doc<id>` marker, so re-ingesting
// replaces the previous KB copy instead of accumulating duplicates. Returns the
// number of documents removed.
func (s *sqlStore) DeleteKBDocumentsByNameSuffix(ctx context.Context, collectionID int64, suffix string) (int, error) {
	if collectionID <= 0 || suffix == "" {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, s.q(
		`SELECT id FROM kb_documents WHERE collection_id = ? AND name LIKE ? ESCAPE '\'`),
		collectionID, "%"+escapeLike(suffix))
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, id := range ids {
		if err := s.DeleteKBDocument(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// escapeLike escapes LIKE wildcards so a literal suffix never matches more than
// intended (paired with ESCAPE '\').
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// CountKBChunks returns how many chunks exist for a collection (detail view /
// cascade-delete verification).
func (s *sqlStore) CountKBChunks(ctx context.Context, collectionID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*) FROM kb_chunks WHERE collection_id = ?`), collectionID).Scan(&n)
	return n, err
}

// refreshKBCounters recomputes a collection's document/chunk counts after a
// document's chunks are replaced.
func (s *sqlStore) refreshKBCounters(ctx context.Context, collectionID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`
		UPDATE kb_collections SET document_count = (
			SELECT COUNT(*) FROM kb_documents WHERE collection_id = kb_collections.id),
			chunk_count = (
			SELECT COUNT(*) FROM kb_chunks WHERE collection_id = kb_collections.id),
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`), collectionID)
	return err
}

// isUniqueViolation reports whether err is a uniqueness-constraint violation,
// which both SQLite ("UNIQUE constraint failed") and PostgreSQL ("duplicate key
// value violates unique constraint") surface as a plain error string here.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "duplicate key value")
}
