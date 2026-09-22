package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/doc"
	"github.com/hiylo/starburst-backend/internal/store"
)

// Knowledge base API surface (generic, client-managed):
//
//	GET  /api/kb/collections            list collections
//	POST /api/kb/collections            create {name, description}
//	DELETE /api/kb/collections/{id}     delete collection + documents + chunks
//	GET  /api/kb/documents?collectionId=&limit=   list documents
//	GET  /api/kb/documents/{id}         document detail
//	DELETE /api/kb/documents/{id}       delete document + chunks
//	POST /api/kb/ingest                 {collectionId, name, mime?, content}
//	POST /api/kb/search                 {query, collectionIds?, topK?, minScore?}
//
// All endpoints accept a web session or an APP token (dual-channel).

// requireDualAuth is the shared dual-channel auth check: web session or APP token.
func (s *Server) requireDualAuth(r *http.Request) bool {
	if s.requireWeb(r) {
		return true
	}
	if _, ok := s.requireToken(r); ok {
		return true
	}
	return false
}

// handleKbCollections lists collections (GET) or creates one (POST).
func (s *Server) handleKbCollections(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cols, err := s.store.ListKBCollections(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "list collections failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"collections": cols})
	case http.MethodPost:
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeErr(w, http.StatusBadRequest, "name is required")
			return
		}
		col, err := s.store.CreateKBCollection(r.Context(), strings.TrimSpace(req.Name), req.Description)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeErr(w, http.StatusConflict, "collection name already exists")
				return
			}
			writeErr(w, http.StatusInternalServerError, "create collection failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, col)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleKbCollectionByID handles DELETE /api/kb/collections/{id}.
func (s *Server) handleKbCollectionByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid collection id")
		return
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := s.store.DeleteKBCollection(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete collection failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleKbDocuments lists a collection's documents (GET ?collectionId=&limit=).
func (s *Server) handleKbDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	collectionID, _ := strconv.ParseInt(r.URL.Query().Get("collectionId"), 10, 64)
	if collectionID <= 0 {
		writeErr(w, http.StatusBadRequest, "collectionId is required")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	docs, err := s.store.ListKBDocuments(r.Context(), collectionID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list documents failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// handleKbDocumentByID handles GET/DELETE /api/kb/documents/{id}.
func (s *Server) handleKbDocumentByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	switch r.Method {
	case http.MethodGet:
		doc, err := s.store.GetKBDocument(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "document not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, doc)
	case http.MethodDelete:
		if err := s.store.DeleteKBDocument(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete document failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleKbIngest accepts raw text/markdown content, chunks it, embeds every
// chunk and persists document + chunks in one synchronous pass. Errors mark the
// document failed so the UI can show why.
func (s *Server) handleKbIngest(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		CollectionID  int64  `json:"collectionId"`
		Name          string `json:"name"`
		MIME          string `json:"mime"`
		Content       string `json:"content"`
		ContentBase64 string `json:"contentBase64"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CollectionID <= 0 {
		writeErr(w, http.StatusBadRequest, "collectionId is required")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}

	// 二进制文件走 base64：交给 internal/doc 解析成 Markdown，再走统一的分块链路。
	content := req.Content
	if req.ContentBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "contentBase64 is not valid base64")
			return
		}
		markdown, err := doc.Parse(req.Name, req.MIME, raw)
		if err != nil {
			if errors.Is(err, doc.ErrUnsupported) {
				writeErr(w, http.StatusBadRequest, "unsupported document type: "+req.Name)
				return
			}
			writeErr(w, http.StatusBadRequest, "parse document failed: "+err.Error())
			return
		}
		content = markdown
	}
	if strings.TrimSpace(content) == "" {
		writeErr(w, http.StatusBadRequest, "content or contentBase64 is required")
		return
	}
	if s.embedding == nil || !s.embedding.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "embeddings not configured (set 嵌入模型配置 in settings)")
		return
	}
	if ok, err := s.store.PGVectorInstalled(r.Context()); err != nil || !ok {
		writeErr(w, http.StatusServiceUnavailable,
			"the knowledge-base index needs PostgreSQL with pgvector; SQLite deployments do not include the knowledge base")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	doc := &store.KBDocument{
		CollectionID: req.CollectionID,
		Name:         strings.TrimSpace(req.Name),
		MIME:         req.MIME,
		SizeBytes:    int64(len(content)),
		Status:       "pending",
	}
	if err := s.store.CreateKBDocument(ctx, doc); err != nil {
		writeErr(w, http.StatusInternalServerError, "create document failed: "+err.Error())
		return
	}

	chunks, err := s.embedKnowledgeChunks(ctx, doc, req.Content)
	if err != nil {
		_ = s.store.UpdateKBDocumentResult(ctx, doc.ID, "failed", 0, err.Error())
		if errors.Is(err, store.ErrRagUnsupported) {
			writeErr(w, http.StatusServiceUnavailable,
				"the knowledge-base index needs PostgreSQL with pgvector; SQLite deployments do not include the knowledge base")
			return
		}
		writeErr(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
		return
	}
	if err := s.store.ReplaceKBDocumentChunks(ctx, doc, chunks); err != nil {
		_ = s.store.UpdateKBDocumentResult(ctx, doc.ID, "failed", 0, err.Error())
		writeErr(w, http.StatusInternalServerError, "store chunks failed: "+err.Error())
		return
	}
	if err := s.store.UpdateKBDocumentResult(ctx, doc.ID, "indexed", int64(len(chunks)), ""); err != nil {
		log.Printf("kb ingest: update result: %v", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": doc, "chunks": len(chunks)})
}

// kbHit is a single retrieved KB fragment in the shape RAG-in-Prompt expects:
// source (document name) + section (chunk title) + content + score.
type kbHit struct {
	source  string
	section string
	content string
	score   float64
}

// searchKB embeds the query and runs the vector search over the KB, optionally
// narrowed to a set of collections (empty = all) and filtered by minScore.
// Returns hits sorted by score descending. errEmbeddingDisabled (503) when no
// embedding model is configured; store.ErrRagUnsupported (503) on SQLite.
func (s *Server) searchKB(ctx context.Context, query string, collectionIDs []int64, topK int, minScore float64) ([]kbHit, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return nil, errEmbeddingDisabled
	}
	if topK <= 0 {
		topK = 5
	}
	if topK > 20 {
		topK = 20
	}
	if minScore <= 0 {
		minScore = 0.5
	}
	qvec, err := s.embedding.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	if len(qvec) != store.EmbedDim {
		return nil, fmt.Errorf("embedding dimension %d does not match column dimension %d (model mismatch?)",
			len(qvec), store.EmbedDim)
	}
	chunks, err := s.store.SearchKBChunks(ctx, collectionIDs, qvec, topK)
	if err != nil {
		return nil, err
	}
	hits := make([]kbHit, 0, len(chunks))
	for _, c := range chunks {
		if c.Similarity < minScore {
			continue
		}
		hits = append(hits, kbHit{
			source:  kbSourceName(ctx, s.store, c.DocumentID),
			section: c.Title,
			content: c.Content,
			score:   c.Similarity,
		})
	}
	return hits, nil
}

// kbSourceName resolves a chunk's source label from its document name. A
// missing document falls back to a numeric reference rather than failing the
// whole retrieval.
func kbSourceName(ctx context.Context, st store.Store, documentID int64) string {
	if documentID <= 0 {
		return "知识库文档"
	}
	doc, err := st.GetKBDocument(ctx, documentID)
	if err != nil || doc == nil {
		return fmt.Sprintf("文档#%d", documentID)
	}
	return doc.Name
}

// handleKbSearch embeds the query, retrieves the most similar KB chunks and
// returns them (optionally filtered by minScore). This is the endpoint the
// RAG-in-Prompt proxy layer calls before splicing context into a message.
func (s *Server) handleKbSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Query         string  `json:"query"`
		CollectionIDs []int64 `json:"collectionIds"`
		TopK          int     `json:"topK"`
		MinScore      float64 `json:"minScore"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeErr(w, http.StatusBadRequest, "query is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	hits, err := s.searchKB(ctx, req.Query, req.CollectionIDs, req.TopK, req.MinScore)
	if err != nil {
		if errors.Is(err, store.ErrRagUnsupported) {
			writeErr(w, http.StatusServiceUnavailable,
				"vector retrieval needs PostgreSQL with pgvector; SQLite deployments do not include the knowledge base")
			return
		}
		if errors.Is(err, errEmbeddingDisabled) {
			writeErr(w, http.StatusServiceUnavailable, "embeddings not configured (set 嵌入模型配置 in settings)")
			return
		}
		writeErr(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		results = append(results, map[string]any{
			"source":  h.source,
			"section": h.section,
			"content": h.content,
			"score":   h.score,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// pathID extracts the trailing integer id from a /api/.../{id} URL.
func pathID(r *http.Request) (int64, bool) {
	raw := r.URL.Path
	i := strings.LastIndex(raw, "/")
	if i < 0 || i == len(raw)-1 {
		return 0, false
	}
	id, err := strconv.ParseInt(raw[i+1:], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
