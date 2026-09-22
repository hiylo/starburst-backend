package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/doc"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// docEventPayload builds the `doc.event` push payload for a generation outcome
// (docs/DOCUMENTS.md §8). Pure so it can be unit-tested without a WebSocket.
func docEventPayload(status string, doc *store.DocDocument) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":      doc.ID,
		"name":    doc.Name,
		"docType": doc.DocType,
		"status":  status,
		"size":    doc.SizeBytes,
	})
	return b
}

// broadcastDocEvent fans a generation outcome out to connected clients so other
// devices / the Web workbench see "文档已生成/失败" without polling.
func (s *Server) broadcastDocEvent(status string, doc *store.DocDocument) {
	if s.hub == nil {
		return
	}
	s.hub.Broadcast(push.Message{Type: "doc.event", Payload: docEventPayload(status, doc)})
}

// Generated-document API (docs/DOCUMENTS.md §4):
//
//	POST   /api/documents/generate      {type, prompt} → {id, name, docType, downloadUrl}
//	GET    /api/documents/?limit=       list
//	GET    /api/documents/{id}          detail
//	DELETE /api/documents/{id}          delete record + file
//	GET    /api/documents/{id}/download download file
//
// Generation is deterministic-render after an LLM skeleton draft: the model only
// produces structure/text, numbers come from the caller (or KB retrieval later).

// docGenerateSystem is the skeleton-drafting instruction the LLM is given.
// The schema mirrors doc.Skeleton; renderers never touch the LLM output besides
// this JSON (docs/DOCUMENTS.md §4.2).
const docGenerateSystem = `你是文档生成助手。根据用户需求输出一个 JSON 文档骨架，只输出合法 JSON，不要输出任何其它内容或 Markdown 围栏。

骨架字段：
{
  "title": "文档标题",
  // 仅当类型为 xlsx 时：
  "sheets": [{
    "name": "表名",
    "rows": [["列1","列2"],["值1","值2"]],
    "charts": [{"type":"bar|line|pie|area|doughnut|radar","title":"图标题","startRow":1,"startCol":1,"endRow":4,"endCol":2}]
  }],
  // 仅当类型为 docx 时：
  "paragraphs": ["段落1", "标题段落可用 \"# 标题\" 前缀"],
  "tables": [{"headers":["列1","列2"],"rows":[["值1","值2"]]}],
  // 仅当类型为 pptx 时：
  "slides": [
    {"title":"封面","layout":"cover"},
    {"title":"要点","layout":"bullets","bullets":["要点1","要点2"]},
    {"title":"对比","layout":"table","table":{"headers":["A","B"],"rows":[["1","2"]]}},
    {"title":"优劣势","layout":"two-col","left":["优点1"],"right":["缺点1"]}
  ]
}

说明：charts 的 startRow/startCol/endRow/endCol 是 1 基单元格范围（首列为分类、末列为数值）；表格数字必须来自用户输入或检索资料，不得编造。`

// skeletonFieldFor returns the JSON field an LLM skeleton must populate for a
// document type (empty for an unknown type).
func skeletonFieldFor(docType string) string {
	switch docType {
	case "pptx":
		return "slides"
	case "docx":
		return "paragraphs"
	case "xlsx":
		return "sheets"
	}
	return ""
}

// validSkeletonForType reports whether sk carries the structure its type needs.
func validSkeletonForType(docType string, sk *doc.Skeleton) bool {
	switch docType {
	case "pptx":
		return len(sk.Slides) > 0
	case "docx":
		return len(sk.Paragraphs) > 0 || len(sk.Tables) > 0
	case "xlsx":
		return len(sk.Sheets) > 0
	}
	return true
}

// draftSkeleton asks the orchestration LLM for a type-shaped skeleton and, when
// the model returns the wrong shape (e.g. docx paragraphs for a pptx request),
// retries once with an explicit correction before giving up. When refs is
// non-empty it is injected as "参考资料" so generated content sticks to the
// knowledge base instead of hallucinating figures.
func (s *Server) draftSkeleton(ctx context.Context, docType, prompt string, refs []kbHit) (doc.Skeleton, int, error) {
	field := skeletonFieldFor(docType)
	system := docGenerateSystem
	if len(refs) > 0 {
		system += "\n\n参考资料（优先采用其中的数据与表述，不要编造数字）:\n" + buildKBRefBlock(refs)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			system += "\n（重要：请求类型为 " + docType + "，骨架必须包含非空的 " + field +
				" 字段并填充真实内容；上一次返回缺少该字段。）"
		}
		var sk doc.Skeleton
		if err := s.llm.CompleteJSON(ctx, system, prompt, &sk); err != nil {
			return sk, http.StatusServiceUnavailable, fmt.Errorf("LLM skeleton failed: %w", err)
		}
		sk.Type = docType
		if validSkeletonForType(docType, &sk) {
			return sk, http.StatusOK, nil
		}
		log.Printf("doc generate: type %s skeleton missing %s, retrying", docType, field)
	}
	return doc.Skeleton{}, http.StatusUnprocessableEntity,
		fmt.Errorf("doc: %s skeleton missing %s after retry", docType, field)
}

// buildKBRefBlock renders KB hits into the reference block the skeleton LLM
// consumes (mirrors the splice format so provenance is uniform).
func buildKBRefBlock(refs []kbHit) string {
	var b strings.Builder
	for i, r := range refs {
		fmt.Fprintf(&b, "[来源%d] %s", i+1, r.source)
		if r.section != "" {
			fmt.Fprintf(&b, " · %s", r.section)
		}
		b.WriteString(":\n")
		b.WriteString(strings.TrimSpace(clipTextRef(r.content, 900)))
		b.WriteString("\n")
	}
	return b.String()
}

// clipTextRef truncates reference text to the first n runes, preserving the
// natural cut so the LLM never sees a half-word-pile.
func clipTextRef(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

// docGenerationRefs retrieves knowledge-base context for a generation request.
// It honours the explicit kbCollectionIds (or the single kbCollectionId
// legacy field); when none is given it falls back to the global RAG collection
// scope so a generation in a scoped deployment automatically stays on-brand.
// Retrieval is best-effort: a missing embedding/pgvector/timeout yields no refs
// without failing the request.
func (s *Server) docGenerationRefs(ctx context.Context, req struct {
	Type            string  `json:"type"`
	Prompt          string  `json:"prompt"`
	KbCollectionID  int64   `json:"kbCollectionId"`
	KbCollectionIDs []int64 `json:"kbCollectionIds"`
	Async           bool    `json:"async"`
}) []kbHit {
	return s.docRefsForQuery(ctx, appendCompare(req.KbCollectionIDs, req.KbCollectionID), req.Prompt)
}

// appendCompare merges a []int64 with a single int64 fallback (no duplicates,
// zero values dropped), for callers that accept both the singular and plural
// collection fields.
func appendCompare(ids []int64, single int64) []int64 {
	if single > 0 {
		for _, id := range ids {
			if id == single {
				return ids
			}
		}
		return append(append([]int64{}, ids...), single)
	}
	return ids
}

// docRefsForQuery retrieves up to 6 KB hits for a query against the given
// collection scope (empty scope = global ragCollectionScope; still empty = no
// refs). Best-effort: embedding/pgvector/timeout failures yield nil, never an
// error, so document generation stays available without a knowledge base.
func (s *Server) docRefsForQuery(ctx context.Context, collectionIDs []int64, query string) []kbHit {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	if len(collectionIDs) == 0 {
		collectionIDs = s.ragCollectionScope()
	}
	if len(collectionIDs) == 0 {
		return nil
	}
	searchCtx, cancel := context.WithTimeout(ctx, s.ragTimeout())
	defer cancel()
	hits, err := s.searchKB(searchCtx, query, collectionIDs, 6, 0.5)
	if err != nil || len(hits) == 0 {
		return nil
	}
	return hits
}

// docFilePath resolves where a generated product file lives on disk for a row.
func (s *Server) docFilePath(d *store.DocDocument) string {
	if s.cfg == nil || s.cfg.DocsDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.DocsDir, strconv.FormatInt(d.ID, 10)+doc.Extension(d.DocType))
}

// handleDocGenerate drafts a document skeleton via the orchestration LLM and
// renders the matching file synchronously. Requires a configured LLM and an
// on-disk docs dir.
func (s *Server) handleDocGenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "orchestration LLM not configured (set LLM 接口配置 in settings)")
		return
	}
	if s.cfg == nil || s.cfg.DocsDir == "" {
		writeErr(w, http.StatusServiceUnavailable, "docs-dir not configured")
		return
	}
	var req struct {
		Type            string  `json:"type"`
		Prompt          string  `json:"prompt"`
		KbCollectionID  int64   `json:"kbCollectionId"`
		KbCollectionIDs []int64 `json:"kbCollectionIds"`
		Async           bool    `json:"async"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if !doc.ValidType(req.Type) {
		writeErr(w, http.StatusBadRequest, "type must be one of xlsx, docx, pptx")
		return
	}

	// 异步模式：入队 kind=doc-generate 任务立即返回 taskId，由 executor 的 doc
	// worker 后台渲染，进度经任务列表 + doc.event 推送（不阻塞 HTTP）。
	if req.Async {
		instr := docGenerateTaskInstr{
			Type:            req.Type,
			Prompt:          req.Prompt,
			KbCollectionID:  req.KbCollectionID,
			KbCollectionIDs: req.KbCollectionIDs,
			KbIngest:        req.KbCollectionID > 0,
		}
		taskID, err := s.enqueueDocGeneration(r.Context(), instr, "文档生成 · "+docTypeLabel(req.Type), 150)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "enqueue document task failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"taskId": taskID, "async": true})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	refs := s.docGenerationRefs(ctx, req)
	sk, status, err := s.draftSkeleton(ctx, req.Type, req.Prompt, refs)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}

	if err := os.MkdirAll(s.cfg.DocsDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "create docs dir failed: "+err.Error())
		return
	}

	// 先落库拿到 docId（文件以 id 命名），再渲染、写入磁盘、回填结果。
	name := strings.TrimSpace(sk.Title)
	if name == "" {
		name = "document"
	}
	skeletonJSON, _ := json.Marshal(&sk)
	docRow := &store.DocDocument{
		Name:     name,
		DocType:  req.Type,
		Prompt:   strings.TrimSpace(req.Prompt),
		Skeleton: string(skeletonJSON),
		Status:   "created",
	}
	if err := s.store.CreateDocDocument(ctx, docRow); err != nil {
		writeErr(w, http.StatusInternalServerError, "create document failed: "+err.Error())
		return
	}

	renderedType, data, err := doc.RenderFromSkeletonJSON(skeletonJSON)
	if err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		writeErr(w, http.StatusInternalServerError, "render failed: "+err.Error())
		return
	}
	path := filepath.Join(s.cfg.DocsDir, strconv.FormatInt(docRow.ID, 10)+doc.Extension(renderedType))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		writeErr(w, http.StatusInternalServerError, "write file failed: "+err.Error())
		return
	}

	docRow.DocType = renderedType
	docRow.SizeBytes = int64(len(data))
	docRow.Skeleton = string(skeletonJSON)
	docRow.Status = "ready"
	if err := s.store.UpdateDocDocumentResult(ctx, docRow); err != nil {
		log.Printf("doc generate: update result: %v", err)
	}
	s.broadcastDocEvent("ready", docRow)
	kbIngested := s.reverseIngestGenerated(ctx, req.KbCollectionID, docRow, data)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          docRow.ID,
		"name":        docRow.Name,
		"docType":     docRow.DocType,
		"sizeBytes":   docRow.SizeBytes,
		"kbIngested":  kbIngested,
		"downloadUrl": fmt.Sprintf("/api/documents/%d/download", docRow.ID),
	})
}

// handleDocRegenerate re-runs a generation against the original skeleton plus
// an optional revision instruction and recent session context
// (docs/DOCUMENTS.md §6). The regenerate is exactly the same pipeline as
// generate but the prompt is seeded with the stored draft, so the LLM revises
// rather than drafts from scratch.
func (s *Server) handleDocRegenerate(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusServiceUnavailable, "orchestration LLM not configured (set LLM 接口配置 in settings)")
		return
	}
	if s.cfg == nil || s.cfg.DocsDir == "" {
		writeErr(w, http.StatusServiceUnavailable, "docs-dir not configured")
		return
	}
	var req struct {
		DocID           int64   `json:"docId"`
		Instruction     string  `json:"instruction"`
		SessionID       string  `json:"sessionId"`
		KbCollectionID  int64   `json:"kbCollectionId"`
		KbCollectionIDs []int64 `json:"kbCollectionIds"`
		Async           bool    `json:"async"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DocID <= 0 {
		writeErr(w, http.StatusBadRequest, "docId is required")
		return
	}
	if req.Async {
		instr := docGenerateTaskInstr{
			DocID:           req.DocID,
			Instruction:     req.Instruction,
			SessionID:       req.SessionID,
			KbCollectionID:  req.KbCollectionID,
			KbCollectionIDs: req.KbCollectionIDs,
			KbIngest:        req.KbCollectionID > 0,
		}
		taskID, err := s.enqueueDocGeneration(r.Context(), instr, "文档重新生成", 150)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "enqueue document task failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"taskId": taskID, "async": true})
		return
	}
	docRow, err := s.store.GetDocDocument(r.Context(), req.DocID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
		return
	}
	if !doc.ValidType(docRow.DocType) {
		writeErr(w, http.StatusBadRequest, "stored document type is not regenerable: "+docRow.DocType)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var user strings.Builder
	user.WriteString("这是要重新生成的文档骨架：\n")
	user.WriteString(docRow.Skeleton)
	user.WriteString("\n\n")
	if strings.TrimSpace(req.Instruction) != "" {
		user.WriteString("用户修改意见：\n")
		user.WriteString(strings.TrimSpace(req.Instruction))
		user.WriteString("\n\n")
	} else {
		user.WriteString("没有新的修改意见：请基于原骨架重新生成一版，保持结构与内容质量。\n\n")
	}
	if context := s.sessionContextExcerpt(ctx, req.SessionID); context != "" {
		user.WriteString("最近会话上下文（供参考，引用其中讨论时注意）：\n")
		user.WriteString(context)
		user.WriteString("\n\n")
	}

	// 重新生成时按修改意见（无则按原需求）检索知识库，引用统一走 draftSkeleton 的 refs。
	refQuery := strings.TrimSpace(req.Instruction)
	if refQuery == "" {
		refQuery = docRow.Prompt
	}
	refs := s.docRefsForQuery(ctx, appendCompare(req.KbCollectionIDs, req.KbCollectionID), refQuery)
	sk, status, err := s.draftSkeleton(ctx, docRow.DocType, user.String(), refs)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	skeletonJSON, _ := json.Marshal(&sk)

	renderedType, data, err := doc.RenderFromSkeletonJSON(skeletonJSON)
	if err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		writeErr(w, http.StatusInternalServerError, "render failed: "+err.Error())
		return
	}
	path := s.docFilePath(docRow)
	if path == "" {
		writeErr(w, http.StatusInternalServerError, "docs-dir not configured")
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		writeErr(w, http.StatusInternalServerError, "write file failed: "+err.Error())
		return
	}

	if title := strings.TrimSpace(sk.Title); title != "" {
		docRow.Name = title
	}
	docRow.DocType = renderedType
	docRow.Skeleton = string(skeletonJSON)
	docRow.SizeBytes = int64(len(data))
	docRow.Status = "ready"
	if err := s.store.UpdateDocDocumentResult(ctx, docRow); err != nil {
		log.Printf("doc regenerate: update result: %v", err)
	}
	s.broadcastDocEvent("ready", docRow)
	kbIngested := s.reverseIngestGenerated(ctx, req.KbCollectionID, docRow, data)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          docRow.ID,
		"name":        docRow.Name,
		"docType":     docRow.DocType,
		"sizeBytes":   docRow.SizeBytes,
		"kbIngested":  kbIngested,
		"downloadUrl": fmt.Sprintf("/api/documents/%d/download", docRow.ID),
	})
}

// sessionContextExcerpt pulls the recent turns of a session as a bounded text
// block for regeneration. Best-effort: an empty result (or a failure) simply
// yields no context, never failing the regenerate.
func (s *Server) sessionContextExcerpt(ctx context.Context, sessionID string) string {
	if sessionID == "" || s.openCode == nil {
		return ""
	}
	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	msgs, err := s.openCode.FetchSessionMessages(fctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	const maxTurns = 20
	const maxTotalRunes = 4000
	var sb strings.Builder
	start := 0
	if len(msgs) > maxTurns {
		start = len(msgs) - maxTurns
	}
	for i := start; i < len(msgs); i++ {
		m := msgs[i]
		role := "用户"
		switch m.Role {
		case "assistant":
			role = "助手"
		case "tool":
			role = "工具"
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		runes := []rune(content)
		if len(runes) > 500 {
			content = string(runes[:500]) + "…"
		}
		sb.WriteString(role)
		sb.WriteString("：")
		sb.WriteString(content)
		sb.WriteString("\n\n")
		if sb.Len() > maxTotalRunes {
			break
		}
	}
	return strings.TrimSpace(sb.String())
}

// reverseIngestGenerated optionally ingests a freshly rendered document back
// into the knowledge base (docs/DOCUMENTS.md §2.1): the rendered file is
// parsed to Markdown and run through the same chunk/embed pipeline. Best-effort:
// a failure only drops the KB copy and is surfaced via the kbIngested flag.
// The KB document name carries a `-@doc<id>` marker; a previous copy of the same
// generated document (e.g. from an earlier regenerate) is deleted first so the
// knowledge base keeps exactly one version per docId.
func (s *Server) reverseIngestGenerated(ctx context.Context, collectionID int64, rec *store.DocDocument, data []byte) bool {
	if collectionID <= 0 || len(data) == 0 {
		return false
	}
	md, err := doc.Parse(rec.Name+doc.Extension(rec.DocType), "", data)
	if err != nil {
		log.Printf("doc reverse-ingest: parse: %v", err)
		return false
	}
	kbName := fmt.Sprintf("%s-@doc%d", rec.Name, rec.ID)
	if _, err := s.store.DeleteKBDocumentsByNameSuffix(ctx, collectionID, fmt.Sprintf("-@doc%d", rec.ID)); err != nil {
		log.Printf("doc reverse-ingest: drop old copy: %v", err)
	}
	if _, _, err := s.ingestKBDocument(ctx, collectionID, kbName, "text/markdown", md); err != nil {
		log.Printf("doc reverse-ingest: ingest: %v", err)
		return false
	}
	return true
}

// handleDocDocuments lists generated documents (GET).
func (s *Server) handleDocDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	docs, err := s.store.ListDocDocuments(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list documents failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"documents": docs})
}

// handleDocDocumentByID handles GET/DELETE /api/documents/{id} and
// GET /api/documents/{id}/download.
func (s *Server) handleDocDocumentByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	id, action := docIDFromPath(r)
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	switch action {
	case "download":
		s.handleDocDownload(w, r, id)
		return
	case "attach":
		s.handleDocAttach(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		d, err := s.store.GetDocDocument(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "document not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, d)
	case http.MethodDelete:
		d, err := s.store.GetDocDocument(r.Context(), id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "document not found")
				return
			}
			writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
			return
		}
		if err := s.store.DeleteDocDocument(r.Context(), id); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete document failed: "+err.Error())
			return
		}
		if path := s.docFilePath(d); path != "" {
			_ = os.Remove(path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// docIDFromPath extracts the document id from /api/documents/{id} or
// /api/documents/{id}/download, and dispatches the sub-action suffix via the
// caller. It returns the id plus the raw remaining action (""、"download"或
// "attach") so the caller can route without re-parsing the path.
func docIDFromPath(r *http.Request) (int64, string) {
	rest := strings.TrimSuffix(r.URL.Path, "/")
	rest = strings.TrimPrefix(rest, "/api/documents")
	rest = strings.TrimPrefix(rest, "/")
	action := ""
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		action = rest[i+1:]
		rest = rest[:i]
	}
	id, err := strconv.ParseInt(strings.Trim(rest, "/"), 10, 64)
	if err != nil || id <= 0 {
		return 0, action
	}
	return id, action
}

// handleDocDownload serves the generated product file for a known id.
func (s *Server) handleDocDownload(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	d, err := s.store.GetDocDocument(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
		return
	}
	path := s.docFilePath(d)
	if path == "" {
		writeErr(w, http.StatusInternalServerError, "docs-dir not configured")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing on disk")
		return
	}
	defer f.Close()
	downloadName := fmt.Sprintf("%s-@doc%d%s", d.Name, d.ID, doc.Extension(d.DocType))
	w.Header().Set("Content-Type", docMime(d.DocType))
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeFilename(downloadName)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func docMime(docType string) string {
	switch docType {
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	}
	return "application/octet-stream"
}

func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		if r >= 0 && r < 128 {
			return r
		}
		return '_'
	}, name)
	name = strings.ReplaceAll(name, "\"", "_")
	name = strings.ReplaceAll(name, "\n", "_")
	name = strings.ReplaceAll(name, "\r", "_")
	if name == "" {
		return "document"
	}
	return name
}

// handleDocAttach copies a generated document into a session's work directory
// (uploads/<name>-@doc<id><ext>), so the client can reference it as a file part
// in the conversation — the "生成文档作为会话附件 part" flow (docs/DOCUMENTS.md
// §6.1). The filename carries the `-@doc<id>` marker so the UI can resolve the
// document back to /api/documents/{id} for preview / regenerate / download.
//
//	POST /api/documents/{id}/attach   {sessionId}
//	→ {ok, name, path:"uploads/<...-@doc<id>>", absolutePath, size, docId}
func (s *Server) handleDocAttach(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, action := docIDFromPath(r)
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	if action != "attach" {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.SessionID) == "" {
		writeErr(w, http.StatusBadRequest, "sessionId is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// 取会话工作目录（会话不存在 / 上游不可达 → 明确报错，不静默写错目录）。
	dir, err := s.sessionWorkDirectory(ctx, req.SessionID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "resolve session directory: "+err.Error())
		return
	}
	if err := validateWorkDirectory(dir); err != nil {
		writeErr(w, http.StatusForbidden, "session directory out of scope: "+err.Error())
		return
	}

	d, err := s.store.GetDocDocument(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "document not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, "get document failed: "+err.Error())
		return
	}
	path := s.docFilePath(d)
	if path == "" {
		writeErr(w, http.StatusInternalServerError, "docs-dir not configured")
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file missing on disk")
		return
	}
	name := sanitizeUploadName(fmt.Sprintf("%s-@doc%d%s", d.Name, d.ID, doc.Extension(d.DocType)))
	if name == "" {
		writeErr(w, http.StatusInternalServerError, "invalid document name")
		return
	}
	absPath, relPath, err := writeWorkspaceUpload(dir, name, data)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "attach failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"name":         filepath.Base(absPath),
		"path":         relPath,
		"absolutePath": absPath,
		"size":         len(data),
		"docId":        d.ID,
	})
}

// sessionWorkDirectory resolves a session's work directory from the upstream
// OpenCode server (GET /session/{id} → directory). Returns an error when the
// session is unknown or the upstream is unreachable.
func (s *Server) sessionWorkDirectory(ctx context.Context, sessionID string) (string, error) {
	if s.openCode == nil {
		return "", fmt.Errorf("opencode upstream not configured")
	}
	raw, err := s.openCode.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	var se struct {
		Directory string `json:"directory"`
	}
	if err := json.Unmarshal(raw, &se); err != nil {
		return "", fmt.Errorf("parse session: %w", err)
	}
	if strings.TrimSpace(se.Directory) == "" {
		return "", fmt.Errorf("session has no work directory")
	}
	return se.Directory, nil
}
