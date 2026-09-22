package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
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

// handleKbCollectionByID handles PATCH (rename/re-describe) and DELETE for
// /api/kb/collections/{id}.
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
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" && strings.TrimSpace(req.Description) == "" {
			writeErr(w, http.StatusBadRequest, "name or description is required")
			return
		}
		col, err := s.store.UpdateKBCollection(r.Context(), id, req.Name, req.Description)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				writeErr(w, http.StatusConflict, "collection name already exists")
				return
			}
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "collection not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "update collection failed: "+err.Error())
			return
		}
		// 集合名参与检索来源展示（source），改名后旧缓存里的 source 已过期。
		kbCache.invalidateAll()
		writeJSON(w, http.StatusOK, col)
	case http.MethodDelete:
		if err := s.store.DeleteKBCollection(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete collection failed: "+err.Error())
			return
		}
		kbCache.invalidateAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleKbDocuments lists a collection's documents (GET ?collectionId=&limit=&offset=).
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
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	docs, err := s.store.ListKBDocuments(r.Context(), collectionID, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list documents failed: "+err.Error())
		return
	}
	total, err := s.store.CountKBDocuments(r.Context(), collectionID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "count documents failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs, "total": total})
}

// handleKbDocumentByID handles GET/DELETE /api/kb/documents/{id}.
func (s *Server) handleKbDocumentByID(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/chunks") {
		s.handleKbDocumentChunks(w, r)
		return
	}
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
		kbCache.invalidateAll()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleKbDocumentChunks returns a document's ingested chunks (title + content,
// ordered by seq) so clients can preview what was actually indexed. The id is
// parsed from the `/api/kb/documents/{id}/chunks` path because pathID only
// reads the trailing segment.
func (s *Server) handleKbDocumentChunks(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	trimmed := strings.TrimSuffix(r.URL.Path, "/chunks")
	i := strings.LastIndex(trimmed, "/")
	if i < 0 || i == len(trimmed)-1 {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	id, err := strconv.ParseInt(trimmed[i+1:], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chunks, err := s.store.ListKBDocumentChunks(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list chunks failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documentId": id, "chunks": chunks})
}

// ingestInput is the parsed payload of POST /api/kb/ingest, from either a JSON
// body or a multipart form.
type ingestInput struct {
	collectionID int64
	name         string
	mime         string
	content      string
	// replace deletes an existing document with the same name in the collection
	// before ingesting, so re-uploading a revised file updates in place instead
	// of leaving a duplicate behind.
	replace bool
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
	// 兼容两种载体：JSON（content / contentBase64）或 multipart（大文件）。
	in, code, errMsg := s.parseIngestInput(w, r)
	if code != 0 {
		writeErr(w, code, errMsg)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	// 同名替换：先删旧文档（级联清 chunk），再入库新版本，避免重复文档。
	replaced := false
	if in.replace {
		if old, err := s.store.FindKBDocumentByName(ctx, in.collectionID, in.name); err == nil && old != nil {
			if err := s.store.DeleteKBDocument(ctx, old.ID); err != nil {
				writeErr(w, http.StatusInternalServerError, "replace old document failed: "+err.Error())
				return
			}
			replaced = true
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusInternalServerError, "lookup existing document failed: "+err.Error())
			return
		}
	}

	doc, chunkCount, err := s.ingestKBDocument(ctx, in.collectionID, in.name, in.mime, in.content)
	if err != nil {
		if errors.Is(err, store.ErrRagUnsupported) {
			writeErr(w, http.StatusServiceUnavailable,
				"the knowledge-base index needs PostgreSQL with pgvector; SQLite deployments do not include the knowledge base")
			return
		}
		if errors.Is(err, errEmbeddingDisabled) {
			writeErr(w, http.StatusServiceUnavailable, "embeddings not configured (set 嵌入模型配置 in settings)")
			return
		}
		writeErr(w, http.StatusInternalServerError, "ingest failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"document": doc, "chunks": chunkCount, "replaced": replaced})
}

// parseIngestInput reads collectionId/name/mime/content from either a JSON body
// or a multipart form (大文件走 multipart，绕开 4MiB JSON 上限). On error it
// returns a non-zero HTTP status plus message for the caller to write.
func (s *Server) parseIngestInput(w http.ResponseWriter, r *http.Request) (in ingestInput, status int, errMsg string) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadRequestBytes)
		if err := r.ParseMultipartForm(maxUploadRequestBytes); err != nil {
			return ingestInput{}, http.StatusBadRequest, "invalid multipart form"
		}
		in.collectionID, _ = strconv.ParseInt(strings.TrimSpace(r.FormValue("collectionId")), 10, 64)
		in.name = strings.TrimSpace(r.FormValue("name"))
		if in.name == "" {
			in.name = sanitizeUploadName(r.FormValue("fileName"))
		}
		in.mime = r.FormValue("mime")
		in.replace = parseBoolForm(r.FormValue("replace"))
		file, header, err := r.FormFile("file")
		if err != nil {
			return ingestInput{}, http.StatusBadRequest, "file is required"
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, maxUploadFileBytes+1))
		if err != nil {
			return ingestInput{}, http.StatusBadRequest, "read upload failed: " + err.Error()
		}
		if len(raw) > maxUploadFileBytes {
			return ingestInput{}, http.StatusRequestEntityTooLarge, "file exceeds 10 MiB"
		}
		if in.name == "" {
			in.name = sanitizeUploadName(header.Filename)
		}
		if in.collectionID <= 0 {
			return ingestInput{}, http.StatusBadRequest, "collectionId is required"
		}
		if in.name == "" {
			return ingestInput{}, http.StatusBadRequest, "name is required"
		}
		md, err := s.parseUploadedDocument(in.name, in.mime, raw)
		if err != nil {
			return ingestInput{}, http.StatusBadRequest, err.Error()
		}
		in.content = md
		return in, 0, ""
	}

	var req struct {
		CollectionID  int64  `json:"collectionId"`
		Name          string `json:"name"`
		MIME          string `json:"mime"`
		Content       string `json:"content"`
		ContentBase64 string `json:"contentBase64"`
		Replace       bool   `json:"replace"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		return ingestInput{}, http.StatusBadRequest, "invalid request body"
	}
	if req.CollectionID <= 0 {
		return ingestInput{}, http.StatusBadRequest, "collectionId is required"
	}
	if strings.TrimSpace(req.Name) == "" {
		return ingestInput{}, http.StatusBadRequest, "name is required"
	}
	in.collectionID = req.CollectionID
	in.name = strings.TrimSpace(req.Name)
	in.mime = req.MIME
	in.replace = req.Replace
	in.content = req.Content
	if req.ContentBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.ContentBase64)
		if err != nil {
			return ingestInput{}, http.StatusBadRequest, "contentBase64 is not valid base64"
		}
		md, err := s.parseUploadedDocument(req.Name, req.MIME, raw)
		if err != nil {
			return ingestInput{}, http.StatusBadRequest, err.Error()
		}
		in.content = md
	}
	if strings.TrimSpace(in.content) == "" {
		return ingestInput{}, http.StatusBadRequest, "content or contentBase64 is required"
	}
	return in, 0, ""
}

// parseBoolForm parses the common truthy form encodings ("1", "true", "on",
// "yes"); anything else is false.
func parseBoolForm(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// parseUploadedDocument renders raw bytes to Markdown via internal/doc, with a
// uniform error message for unsupported/parse failures. Plain-text formats
// (Markdown/TXT/JSON/… or any text/* mime) are used as-is — doc.Parse only
// handles the binary/structured office formats.
func (s *Server) parseUploadedDocument(name, mime string, raw []byte) (string, error) {
	if isPlainTextDoc(name, mime) {
		return string(raw), nil
	}
	md, err := doc.Parse(name, mime, raw)
	if err != nil {
		if errors.Is(err, doc.ErrUnsupported) {
			return "", fmt.Errorf("unsupported document type: %s", name)
		}
		return "", fmt.Errorf("parse document failed: %v", err)
	}
	return md, nil
}

// isPlainTextDoc reports whether a document should be ingested as raw text
// rather than parsed.
func isPlainTextDoc(name, mime string) bool {
	if strings.HasPrefix(strings.ToLower(mime), "text/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".txt", ".text", ".json", ".yaml", ".yml",
		".log", ".xml", ".toml", ".ini", ".conf":
		return true
	}
	return false
}

// ingestKBDocument is the shared ingest pipeline (create doc → chunk → embed →
// store), reused by /api/kb/ingest and the generate→KB reverse-ingest path.
func (s *Server) ingestKBDocument(ctx context.Context, collectionID int64, name, mime, content string) (*store.KBDocument, int, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return nil, 0, errEmbeddingDisabled
	}
	if ok, err := s.store.PGVectorInstalled(ctx); err != nil || !ok {
		return nil, 0, store.ErrRagUnsupported
	}
	doc := &store.KBDocument{
		CollectionID: collectionID,
		Name:         name,
		MIME:         mime,
		SizeBytes:    int64(len(content)),
		Status:       "pending",
	}
	if err := s.store.CreateKBDocument(ctx, doc); err != nil {
		return nil, 0, fmt.Errorf("create document: %w", err)
	}
	chunks, err := s.embedKnowledgeChunks(ctx, doc, content)
	if err != nil {
		_ = s.store.UpdateKBDocumentResult(ctx, doc.ID, "failed", 0, err.Error())
		return nil, 0, err
	}
	if err := s.store.ReplaceKBDocumentChunks(ctx, doc, chunks); err != nil {
		_ = s.store.UpdateKBDocumentResult(ctx, doc.ID, "failed", 0, err.Error())
		return nil, 0, fmt.Errorf("store chunks: %w", err)
	}
	if err := s.store.UpdateKBDocumentResult(ctx, doc.ID, "indexed", int64(len(chunks)), ""); err != nil {
		log.Printf("kb ingest: update result: %v", err)
	}
	// 检索缓存失效：新文档入库后，旧的检索结果可能引用了被替换的片段。
	kbCache.invalidateAll()
	return doc, len(chunks), nil
}

// kbHit is a single retrieved KB fragment in the shape RAG-in-Prompt expects:
// source (document name) + section (chunk title) + content + score, plus the
// ids needed for引用溯源 (tap-through to the source document).
type kbHit struct {
	source       string
	section      string
	content      string
	score        float64
	documentID   int64
	collectionID int64
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
	if hits, ok := kbCache.get(kbSearchCacheKey(query, collectionIDs, topK, minScore)); ok {
		return hits, nil
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
		source := c.Source
		if source == "" {
			// 兜底：JOIN 未命中时（老数据 / 文档已删）退回单查。
			source = kbSourceName(ctx, s.store, c.DocumentID)
		}
		hits = append(hits, kbHit{
			source:       source,
			section:      c.Title,
			content:      c.Content,
			score:        c.Similarity,
			documentID:   c.DocumentID,
			collectionID: c.CollectionID,
		})
	}
	diversified := diversifyHits(hits, kbMaxChunksPerDocument)
	kbCache.put(kbSearchCacheKey(query, collectionIDs, topK, minScore), diversified)
	return diversified, nil
}

// kbMaxChunksPerDocument caps how many fragments one document may contribute to
// a single retrieval, so a long document does not crowd out other sources in
// the injected context.
const kbMaxChunksPerDocument = 3

// diversifyHits keeps at most maxPerDoc hits per document, preserving the score
// order. Pure so it can be unit-tested without a vector backend.
func diversifyHits(hits []kbHit, maxPerDoc int) []kbHit {
	if maxPerDoc <= 0 || len(hits) == 0 {
		return hits
	}
	counts := make(map[int64]int, len(hits))
	out := make([]kbHit, 0, len(hits))
	for _, h := range hits {
		if counts[h.documentID] >= maxPerDoc {
			continue
		}
		counts[h.documentID]++
		out = append(out, h)
	}
	return out
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
			"source":       h.source,
			"section":      h.section,
			"content":      h.content,
			"score":        h.score,
			"documentId":   h.documentID,
			"collectionId": h.collectionID,
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
