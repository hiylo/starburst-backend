package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/doc"
	"github.com/hiylo/starburst-backend/internal/store"
)

// docGenerateTaskInstr is the payload packed into a kind=doc-generate task's
// Prompt field. It mirrors the synchronous /api/documents/generate request so
// the async path renders the same document.
type docGenerateTaskInstr struct {
	DocID           int64   `json:"docId,omitempty"`     // 0 = fresh generate
	Type            string  `json:"type"`
	Prompt          string  `json:"prompt"`
	Instruction     string  `json:"instruction,omitempty"` // regenerate-only revision
	SessionID       string  `json:"sessionId,omitempty"`
	KbCollectionID  int64   `json:"kbCollectionId,omitempty"`
	KbCollectionIDs []int64 `json:"kbCollectionIds,omitempty"`
	KbIngest        bool    `json:"kbIngest,omitempty"` // reverse-ingest into KbCollectionID
}

// RunDocGenerateTask is the executor callback for kind=doc-generate tasks: it
// decodes the instruction and drives the same skeleton → render → write →
// reverse-ingest pipeline as the synchronous endpoints, but in the background
// worker so a long render never blocks the HTTP request. Progress is reported
// through the task's progress field and the shared doc.event WebSocket push.
func (s *Server) RunDocGenerateTask(ctx context.Context, t *store.Task) (string, error) {
	var instr docGenerateTaskInstr
	if err := json.Unmarshal([]byte(t.Prompt), &instr); err != nil {
		return "", fmt.Errorf("parse doc generate instruction: %w", err)
	}
	if s.llm == nil || !s.llm.Enabled() {
		return "", fmt.Errorf("orchestration LLM not configured")
	}
	if s.cfg == nil || s.cfg.DocsDir == "" {
		return "", fmt.Errorf("docs-dir not configured")
	}
	if !doc.ValidType(instr.Type) {
		return "", fmt.Errorf("type must be one of xlsx, docx, pptx")
	}
	if strings.TrimSpace(instr.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}

	if instr.DocID > 0 {
		return s.runDocRegenerateTask(ctx, t, &instr)
	}
	return s.runDocGenerateTask(ctx, t, &instr)
}

// runDocGenerateTask drafts a fresh skeleton, creates the document row, renders
// the product file and reverse-ingests when requested. Mirrors
// handleDocGenerate, sharing doc.event pushes.
func (s *Server) runDocGenerateTask(ctx context.Context, t *store.Task, instr *docGenerateTaskInstr) (string, error) {
	_ = s.store.UpdateTaskProgress(ctx, t.ID, "drafting skeleton")
	refs := s.docRefsForQuery(ctx, appendCompare(instr.KbCollectionIDs, instr.KbCollectionID), instr.Prompt)
	sk, _, err := s.draftSkeleton(ctx, instr.Type, instr.Prompt, refs)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(s.cfg.DocsDir, 0o755); err != nil {
		return "", fmt.Errorf("create docs dir: %w", err)
	}
	name := strings.TrimSpace(sk.Title)
	if name == "" {
		name = "document"
	}
	skeletonJSON, _ := json.Marshal(&sk)
	docRow := &store.DocDocument{
		Name:     name,
		DocType:  instr.Type,
		Prompt:   strings.TrimSpace(instr.Prompt),
		Skeleton: string(skeletonJSON),
		Status:   "created",
	}
	if err := s.store.CreateDocDocument(ctx, docRow); err != nil {
		return "", fmt.Errorf("create document: %w", err)
	}

	_ = s.store.UpdateTaskProgress(ctx, t.ID, "rendering "+instr.Type)
	renderedType, data, err := doc.RenderFromSkeletonJSON(skeletonJSON)
	if err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		return "", fmt.Errorf("render: %w", err)
	}
	path := filepath.Join(s.cfg.DocsDir, strconv.FormatInt(docRow.ID, 10)+doc.Extension(renderedType))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		return "", fmt.Errorf("write file: %w", err)
	}

	docRow.DocType = renderedType
	docRow.SizeBytes = int64(len(data))
	docRow.Skeleton = string(skeletonJSON)
	docRow.Status = "ready"
	if err := s.store.UpdateDocDocumentResult(ctx, docRow); err != nil {
		log.Printf("doc task %s: update result: %v", t.ID, err)
	}
	s.broadcastDocEvent("ready", docRow)

	kbIngested := false
	if instr.KbIngest {
		kbIngested = s.reverseIngestGenerated(ctx, instr.KbCollectionID, docRow, data)
	}
	return fmt.Sprintf("文档已生成 #%d（%s，%.1f KB%s）", docRow.ID, docRow.Name,
		float64(docRow.SizeBytes)/1024, ingestNote(kbIngested, instr.KbIngest)), nil
}

// runDocRegenerateTask revises an existing document via its stored skeleton,
// overwriting the product file. Mirrors handleDocRegenerate.
func (s *Server) runDocRegenerateTask(ctx context.Context, t *store.Task, instr *docGenerateTaskInstr) (string, error) {
	docRow, err := s.store.GetDocDocument(ctx, instr.DocID)
	if err != nil {
		return "", fmt.Errorf("load document %d: %w", instr.DocID, err)
	}
	if !doc.ValidType(docRow.DocType) {
		return "", fmt.Errorf("stored document type is not regenerable: %s", docRow.DocType)
	}

	var user strings.Builder
	user.WriteString("这是要重新生成的文档骨架：\n")
	user.WriteString(docRow.Skeleton)
	user.WriteString("\n\n")
	if strings.TrimSpace(instr.Instruction) != "" {
		user.WriteString("用户修改意见：\n")
		user.WriteString(strings.TrimSpace(instr.Instruction))
		user.WriteString("\n\n")
	} else {
		user.WriteString("没有新的修改意见：请基于原骨架重新生成一版，保持结构与内容质量。\n\n")
	}
	if context := s.sessionContextExcerpt(ctx, instr.SessionID); context != "" {
		user.WriteString("最近会话上下文（供参考，引用其中讨论时注意）：\n")
		user.WriteString(context)
		user.WriteString("\n\n")
	}

	refQuery := strings.TrimSpace(instr.Instruction)
	if refQuery == "" {
		refQuery = docRow.Prompt
	}
	_ = s.store.UpdateTaskProgress(ctx, t.ID, "revising skeleton")
	refs := s.docRefsForQuery(ctx, appendCompare(instr.KbCollectionIDs, instr.KbCollectionID), refQuery)
	sk, _, err := s.draftSkeleton(ctx, docRow.DocType, user.String(), refs)
	if err != nil {
		return "", err
	}
	skeletonJSON, _ := json.Marshal(&sk)

	_ = s.store.UpdateTaskProgress(ctx, t.ID, "rendering "+docRow.DocType)
	renderedType, data, err := doc.RenderFromSkeletonJSON(skeletonJSON)
	if err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		return "", fmt.Errorf("render: %w", err)
	}
	path := filepath.Join(s.cfg.DocsDir, strconv.FormatInt(docRow.ID, 10)+doc.Extension(renderedType))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		docRow.Status = "failed"
		docRow.Error = err.Error()
		_ = s.store.UpdateDocDocumentResult(ctx, docRow)
		s.broadcastDocEvent("failed", docRow)
		return "", fmt.Errorf("write file: %w", err)
	}

	docRow.DocType = renderedType
	docRow.SizeBytes = int64(len(data))
	docRow.Skeleton = string(skeletonJSON)
	docRow.Status = "ready"
	if err := s.store.UpdateDocDocumentResult(ctx, docRow); err != nil {
		log.Printf("doc task %s: update result: %v", t.ID, err)
	}
	s.broadcastDocEvent("ready", docRow)

	kbIngested := false
	if instr.KbIngest {
		kbIngested = s.reverseIngestGenerated(ctx, instr.KbCollectionID, docRow, data)
	}
	return fmt.Sprintf("文档已重新生成 #%d（%s，%.1f KB%s）", docRow.ID, docRow.Name,
		float64(docRow.SizeBytes)/1024, ingestNote(kbIngested, instr.KbIngest)), nil
}

// docTypeLabel maps a document type to a short Chinese label for task names.
func docTypeLabel(docType string) string {
	switch docType {
	case "pptx":
		return "PPT"
	case "docx":
		return "Word"
	case "xlsx":
		return "Excel"
	}
	return docType
}

// ingestNote renders the reverse-ingest outcome suffix for a task summary.
func ingestNote(kbIngested, wanted bool) string {
	if !wanted {
		return ""
	}
	if kbIngested {
		return "，已同步进知识库"
	}
	return "，知识库同步失败"
}

// enqueueDocGeneration queues a kind=doc-generate task and returns its id. The
// executor's doc worker runs the render pipeline asynchronously, so callers
// get a task id immediately and track progress via the task list / doc.event.
func (s *Server) enqueueDocGeneration(ctx context.Context, instr docGenerateTaskInstr, name string, timeoutSec int) (string, error) {
	b, err := json.Marshal(&instr)
	if err != nil {
		return "", err
	}
	t := &store.Task{
		ID:           newTaskID(),
		Kind:         "doc-generate",
		Name:         name,
		Prompt:       string(b),
		TimeoutSec:   timeoutSec,
		AvailableAt:  time.Now(),
	}
	if err := s.store.CreateTask(ctx, t); err != nil {
		return "", err
	}
	return t.ID, nil
}
