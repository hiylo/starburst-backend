package store

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// EmbedDim is the fixed pgvector column dimension. It matches the default
// embedding model bge-m3 (1024 dims). Switching to a model with a different
// dimension requires re-indexing (see docs/TEST_INTELLIGENCE.md).
const EmbedDim = 1024

// RagChunk is one vectorized knowledge fragment of a project (an entity/table
// summary, an endpoint contract, a source file excerpt, etc.). Its content is
// embedded into the vector column and retrieved by cosine similarity.
type RagChunk struct {
	ID         int64     `json:"id"`
	ProjectID  int64     `json:"projectId"`
	ModuleID   int64     `json:"moduleId"`
	Kind       string    `json:"kind"`
	RefID      int64     `json:"refId"`
	Title      string    `json:"title"`
	Content    string    `json:"content"`
	SourceFile string    `json:"sourceFile"`
	SourceLine int       `json:"sourceLine"`
	Embedding  []float32 `json:"-"`
	Similarity float64   `json:"similarity,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

// VectorString renders a float32 vector as pgvector text form "[1,2,3]".
func VectorString(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}

// ReplaceProjectChunks rebuilds a project's knowledge base: it deletes the
// existing chunks and inserts the given set (with embeddings) in one pass.
func (s *sqlStore) ReplaceProjectChunks(ctx context.Context, projectID int64, chunks []*RagChunk) error {
	// 空列表保护：同 ReplaceIntelEntities——无结果保留旧快照，避免误清空。
	if len(chunks) == 0 {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_chunks WHERE project_id = ?`), projectID); err != nil {
		return err
	}
	for _, c := range chunks {
		c.ProjectID = projectID
		if err := s.insertRagChunk(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlStore) insertRagChunk(ctx context.Context, c *RagChunk) error {
	vec := VectorString(c.Embedding)
	if isPostgres(s.driver) {
		return s.db.QueryRowContext(ctx, s.q(`
			INSERT INTO intel_chunks (project_id, module_id, kind, ref_id, title, content,
				source_file, source_line, embedding, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?::vector, CURRENT_TIMESTAMP)
			RETURNING id`),
			c.ProjectID, c.ModuleID, c.Kind, c.RefID, c.Title, c.Content,
			c.SourceFile, c.SourceLine, vec,
		).Scan(&c.ID)
	}
	res, err := s.db.ExecContext(ctx, s.q(`
		INSERT INTO intel_chunks (project_id, module_id, kind, ref_id, title, content,
			source_file, source_line, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`),
		c.ProjectID, c.ModuleID, c.Kind, c.RefID, c.Title, c.Content,
		c.SourceFile, c.SourceLine, vec)
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

// SearchRagChunks returns the chunks most similar to the query embedding for a
// project (optionally narrowed to a module), ordered by cosine similarity
// descending. Vector retrieval is PostgreSQL/pgvector only: SQLite deployments
// are lightweight by design and the intel features that need retrieval are
// hidden there (see PGVectorInstalled), so this returns ErrRagUnsupported.
func (s *sqlStore) SearchRagChunks(ctx context.Context, projectID, moduleID int64, embedding []float32, limit int) ([]*RagChunk, error) {
	if limit <= 0 {
		limit = 5
	}
	if !isPostgres(s.driver) {
		return nil, ErrRagUnsupported
	}
	return s.searchRagChunksPG(ctx, projectID, moduleID, embedding, limit)
}

func (s *sqlStore) searchRagChunksPG(ctx context.Context, projectID, moduleID int64, embedding []float32, limit int) ([]*RagChunk, error) {
	query := `SELECT id, project_id, module_id, kind, ref_id, title, content, source_file,
		source_line, 1 - (embedding <=> ?::vector) AS similarity
		FROM intel_chunks WHERE project_id = ?`
	args := []any{VectorString(embedding), projectID}
	if moduleID > 0 {
		query += ` AND module_id = ?`
		args = append(args, moduleID)
	}
	query += ` ORDER BY embedding <=> ?::vector LIMIT ?`
	args = append(args, VectorString(embedding), limit)

	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*RagChunk, 0, limit)
	for rows.Next() {
		c := &RagChunk{}
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ModuleID, &c.Kind, &c.RefID,
			&c.Title, &c.Content, &c.SourceFile, &c.SourceLine, &c.Similarity); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PGVectorInstalled reports whether the connected backend provides pgvector,
// which the intel RAG/向量检索 requires. Only PostgreSQL with the `vector`
// extension installed counts; SQLite (轻量化部署) does not provide pgvector and
// therefore reports false so the UI hides vector-dependent intel features.
func (s *sqlStore) PGVectorInstalled(ctx context.Context) (bool, error) {
	if !isPostgres(s.driver) {
		return false, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_extension WHERE extname = 'vector'`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CountProjectChunks returns the number of indexed chunks for a project.
func (s *sqlStore) CountProjectChunks(ctx context.Context, projectID int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, s.q(
		`SELECT COUNT(*) FROM intel_chunks WHERE project_id = ?`), projectID).Scan(&n)
	return n, err
}

// DeleteProjectChunks removes all chunks for a project (used by project delete).
func (s *sqlStore) DeleteProjectChunks(ctx context.Context, projectID int64) error {
	_, err := s.db.ExecContext(ctx, s.q(`DELETE FROM intel_chunks WHERE project_id = ?`), projectID)
	return err
}
