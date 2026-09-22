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
  "sheets": [{"name": "表名", "rows": [["列1","列2"],["值1","值2"]]}],
  // 仅当类型为 docx 时：
  "paragraphs": ["段落1", "标题段落可用 \"# 标题\" 前缀"],
  // 仅当类型为 pptx 时：
  "slides": [{"title": "页标题", "bullets": ["要点1","要点2"]}]
}`

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
		Type   string `json:"type"`
		Prompt string `json:"prompt"`
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

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	var sk doc.Skeleton
	if err := s.llm.CompleteJSON(ctx, docGenerateSystem, req.Prompt, &sk); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "LLM skeleton failed: "+err.Error())
		return
	}
	// 文档类型由请求方决定，LLM 只负责产出对应 body。
	sk.Type = req.Type

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

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          docRow.ID,
		"name":        docRow.Name,
		"docType":     docRow.DocType,
		"chunkCount":  docRow.SizeBytes,
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
		DocID       int64  `json:"docId"`
		Instruction string `json:"instruction"`
		SessionID   string `json:"sessionId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.DocID <= 0 {
		writeErr(w, http.StatusBadRequest, "docId is required")
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

	var sk doc.Skeleton
	if err := s.llm.CompleteJSON(ctx, docGenerateSystem, user.String(), &sk); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "LLM skeleton failed: "+err.Error())
		return
	}
	sk.Type = docRow.DocType
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

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          docRow.ID,
		"name":        docRow.Name,
		"docType":     docRow.DocType,
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
	id, isDownload := docIDFromPath(r)
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid document id")
		return
	}
	if isDownload {
		s.handleDocDownload(w, r, id)
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
// /api/documents/{id}/download, returning whether the /download suffix is used.
func docIDFromPath(r *http.Request) (int64, bool) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/documents/")
	isDownload := false
	if strings.HasSuffix(rest, "/download") {
		isDownload = true
		rest = strings.TrimSuffix(rest, "/download")
	}
	id, err := strconv.ParseInt(strings.Trim(rest, "/"), 10, 64)
	if err != nil || id <= 0 {
		return 0, isDownload
	}
	return id, isDownload
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
	downloadName := d.Name + doc.Extension(d.DocType)
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