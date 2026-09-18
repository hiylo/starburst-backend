package store

import (
	"context"
	"errors"
	"math"
	"sort"
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

// parseVector parses "[1,2,3]" back into a float32 slice. It tolerates
// surrounding whitespace.
func parseVector(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, errors.New("vector: not a bracketed list")
	}
	body := strings.TrimSpace(s[1 : len(s)-1])
	if body == "" {
		return nil, nil
	}
	parts := strings.Split(body, ",")
	out := make([]float32, 0, len(parts))
	for _, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 32)
		if err != nil {
			return nil, err
		}
		out = append(out, float32(f))
	}
	return out, nil
}

// cosine returns the cosine similarity between two equal-length vectors. The
// inputs are normalized by the embedding model (bge-m3), so this uses plain dot
// product; the ||a||*||b|| division is retained for safety with non-normalized
// vectors.
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var dot, na, nb float64
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ReplaceProjectChunks rebuilds a project's knowledge base: it deletes the
// existing chunks and inserts the given set (with embeddings) in one pass.
func (s *sqlStore) ReplaceProjectChunks(ctx context.Context, projectID int64, chunks []*RagChunk) error {
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
// descending. On PostgreSQL this is a pgvector HNSW index scan; on SQLite it
// falls back to an in-process cosine comparison over the project's chunks.
func (s *sqlStore) SearchRagChunks(ctx context.Context, projectID, moduleID int64, embedding []float32, limit int) ([]*RagChunk, error) {
	if limit <= 0 {
		limit = 5
	}
	if isPostgres(s.driver) {
		return s.searchRagChunksPG(ctx, projectID, moduleID, embedding, limit)
	}
	return s.searchRagChunksSQLite(ctx, projectID, moduleID, embedding, limit)
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

func (s *sqlStore) searchRagChunksSQLite(ctx context.Context, projectID, moduleID int64, embedding []float32, limit int) ([]*RagChunk, error) {
	query := `SELECT id, project_id, module_id, kind, ref_id, title, content, source_file,
		source_line, embedding FROM intel_chunks WHERE project_id = ?`
	args := []any{projectID}
	if moduleID > 0 {
		query += ` AND module_id = ?`
		args = append(args, moduleID)
	}
	rows, err := s.db.QueryContext(ctx, s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		chunk RagChunk
		score float64
	}
	all := make([]scored, 0)
	for rows.Next() {
		c := RagChunk{}
		var vecText string
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.ModuleID, &c.Kind, &c.RefID,
			&c.Title, &c.Content, &c.SourceFile, &c.SourceLine, &vecText); err != nil {
			return nil, err
		}
		vec, err := parseVector(vecText)
		if err != nil {
			continue
		}
		all = append(all, scored{chunk: c, score: cosine(embedding, vec)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sort by similarity descending. sort.Slice runs O(n·log n); a full sort is
	// fine for typical chunk counts and far cheaper than the previous O(n²)
	// selection sort for large projects.
	sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]*RagChunk, 0, len(all))
	for _, sc := range all {
		sc.chunk.Similarity = sc.score
		out = append(out, &sc.chunk)
	}
	return out, nil
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
