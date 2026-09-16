package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/llm"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelIndex builds (or rebuilds) a project's knowledge-base vector
// index from its scanned contracts (entities + endpoints), embedding each
// fragment into intel_chunks. Synchronous like /api/intel/analyze; requires a
// web session or APP token.
func (s *Server) handleIntelIndex(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64 `json:"projectId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()

	n, err := s.indexIntelProject(ctx, req.ProjectID)
	if err != nil {
		log.Printf("intel index project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "index failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chunks": n})
}

// indexIntelProject builds chunks from the project's entities/endpoints,
// embeds them and replaces the project's chunk set. Returns the chunk count.
func (s *Server) indexIntelProject(ctx context.Context, projectID int64) (int, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return 0, errEmbeddingDisabled
	}
	chunks, err := s.buildIntelChunks(ctx, projectID)
	if err != nil {
		return 0, err
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	if err := s.embedChunks(ctx, chunks); err != nil {
		return 0, err
	}
	if err := s.store.ReplaceProjectChunks(ctx, projectID, chunks); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// buildIntelChunks renders the scanned entities and endpoints into natural-
// language fragments carrying provenance (source file:line).
func (s *Server) buildIntelChunks(ctx context.Context, projectID int64) ([]*store.RagChunk, error) {
	entities, err := s.store.ListIntelEntities(ctx, projectID, 0)
	if err != nil {
		return nil, err
	}
	endpoints, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil {
		return nil, err
	}
	chunks := make([]*store.RagChunk, 0, len(entities)+len(endpoints))
	// The monorepo scanner can assign the same source file to multiple detected
	// modules, producing identical entity/endpoint rows. Dedup by content so the
	// knowledge base keeps a single copy of each distinct fragment.
	seen := map[string]bool{}
	add := func(c *store.RagChunk) {
		if seen[c.Content] {
			return
		}
		seen[c.Content] = true
		chunks = append(chunks, c)
	}

	// Group entity columns by (module, table) into one chunk per table.
	type key struct {
		module int64
		table  string
	}
	groups := map[key][]*store.IntelEntity{}
	var order []key
	for _, e := range entities {
		k := key{module: e.ModuleID, table: e.TableName}
		if e.TableName == "" {
			k.table = e.Entity
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], e)
	}
	for _, k := range order {
		cols := groups[k]
		var sb strings.Builder
		sb.WriteString("数据表 ")
		sb.WriteString(k.table)
		sb.WriteString("（实体 ")
		sb.WriteString(cols[0].Entity)
		sb.WriteString("），来源 ")
		sb.WriteString(cols[0].SourceFile)
		sb.WriteString("。字段：")
		for _, c := range cols {
			sb.WriteString("\n- ")
			sb.WriteString(c.ColumnName)
			if c.FieldType != "" {
				sb.WriteString("：")
				sb.WriteString(c.FieldType)
			}
			if c.IsPrimary {
				sb.WriteString("，主键")
			}
			if c.Nullable {
				sb.WriteString("，可空")
			} else {
				sb.WriteString("，非空")
			}
		}
		add(&store.RagChunk{
			ModuleID:   k.module,
			Kind:       "entity",
			Title:      k.table + " 表",
			Content:    sb.String(),
			SourceFile: cols[0].SourceFile,
			SourceLine: cols[0].SourceLine,
		})
	}
	for _, ep := range endpoints {
		var sb strings.Builder
		sb.WriteString("接口 ")
		sb.WriteString(ep.Method)
		sb.WriteString(" ")
		sb.WriteString(ep.Path)
		if ep.ResponseType != "" {
			sb.WriteString("，返回类型 ")
			sb.WriteString(ep.ResponseType)
		}
		sb.WriteString("，来源 ")
		sb.WriteString(ep.SourceFile)
		sb.WriteString("。")
		if ep.RequestJSON != "" {
			sb.WriteString("\n请求参数：")
			sb.WriteString(ep.RequestJSON)
		}
		if ep.FieldsJSON != "" {
			sb.WriteString("\n返回字段：")
			sb.WriteString(ep.FieldsJSON)
		}
		add(&store.RagChunk{
			ModuleID:   ep.ModuleID,
			Kind:       "endpoint",
			RefID:      ep.ID,
			Title:      ep.Method + " " + ep.Path,
			Content:    sb.String(),
			SourceFile: ep.SourceFile,
			SourceLine: ep.SourceLine,
		})
	}
	return chunks, nil
}

// embedChunks fills each chunk's Embedding by calling the embeddings endpoint
// in batches (a single request per batch to bound payload size).
func (s *Server) embedChunks(ctx context.Context, chunks []*store.RagChunk) error {
	const batchSize = 64
	for i := 0; i < len(chunks); i += batchSize {
		end := i + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[i:end]
		inputs := make([]string, len(batch))
		for j, c := range batch {
			inputs[j] = c.Content
		}
		vecs, err := s.embedding.EmbedBatch(ctx, inputs)
		if err != nil {
			return fmt.Errorf("embed batch %d: %w", i/batchSize, err)
		}
		if len(vecs) != len(batch) {
			return fmt.Errorf("embed batch %d: got %d vectors for %d inputs", i/batchSize, len(vecs), len(batch))
		}
		for j, v := range vecs {
			if len(v) != store.EmbedDim {
				return fmt.Errorf("embedding dimension %d does not match column dimension %d (model mismatch?)", len(v), store.EmbedDim)
			}
			batch[j].Embedding = v
		}
	}
	return nil
}

// handleIntelAsk answers a natural-language question about a project by
// embedding the question, retrieving the most similar knowledge fragments, and
// (when the orchestration LLM is configured) generating an answer grounded in
// those fragments. It is multi-turn: pass a chatId to continue a conversation,
// or omit it to start a new one. Requires a web session or APP token.
func (s *Server) handleIntelAsk(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Question  string `json:"question"`
		ChatID    int64  `json:"chatId"`
		Limit     int    `json:"limit"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "projectId and question are required")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	resp, err := s.askIntelProject(ctx, req.ProjectID, req.Question, req.ChatID, req.Limit)
	if err != nil {
		log.Printf("intel ask project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "ask failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) askIntelProject(ctx context.Context, projectID int64, question string, chatID int64, limit int) (map[string]any, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return nil, errEmbeddingDisabled
	}

	// Resolve the conversation: continue an existing thread or start a new one.
	chat, history, err := s.resolveChat(ctx, projectID, chatID, question)
	if err != nil {
		return nil, err
	}

	qvec, err := s.embedding.Embed(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	if len(qvec) != store.EmbedDim {
		return nil, fmt.Errorf("embedding dimension %d does not match column dimension %d (model mismatch?)", len(qvec), store.EmbedDim)
	}
	chunks, err := s.store.SearchRagChunks(ctx, projectID, 0, qvec, limit)
	if err != nil {
		return nil, err
	}

	sources := make([]map[string]any, 0, len(chunks))
	var contextBuf strings.Builder
	for _, c := range chunks {
		sources = append(sources, map[string]any{
			"title":      c.Title,
			"kind":       c.Kind,
			"content":    c.Content,
			"sourceFile": c.SourceFile,
			"sourceLine": c.SourceLine,
			"similarity": c.Similarity,
		})
		contextBuf.WriteString("【")
		contextBuf.WriteString(c.Title)
		contextBuf.WriteString("】(来源 ")
		contextBuf.WriteString(c.SourceFile)
		fmt.Fprintf(&contextBuf, ":%d", c.SourceLine)
		contextBuf.WriteString(")\n")
		contextBuf.WriteString(c.Content)
		contextBuf.WriteString("\n\n")
	}

	answer := ""
	if s.llm != nil && s.llm.Enabled() && len(chunks) > 0 {
		answer = s.generateChatAnswer(ctx, history, contextBuf.String(), question)
	}

	// Persist the turn so subsequent questions carry full context.
	sourcesJSON, _ := json.Marshal(sources)
	_ = s.store.AddIntelChatMessage(ctx, &store.IntelChatMessage{ChatID: chat.ID, Role: "user", Content: question})
	if answer != "" {
		_ = s.store.AddIntelChatMessage(ctx, &store.IntelChatMessage{ChatID: chat.ID, Role: "assistant", Content: answer, SourcesJSON: string(sourcesJSON)})
	}

	return map[string]any{
		"chatId":   chat.ID,
		"question": question,
		"answer":   answer,
		"sources":  sources,
		"count":    len(sources),
	}, nil
}

// resolveChat loads the conversation (and its recent history) to continue, or
// creates a fresh one titled from the opening question.
func (s *Server) resolveChat(ctx context.Context, projectID, chatID int64, question string) (*store.IntelChat, []*store.IntelChatMessage, error) {
	if chatID > 0 {
		chat, err := s.store.GetIntelChat(ctx, chatID)
		if err != nil {
			return nil, nil, fmt.Errorf("conversation %d: %w", chatID, err)
		}
		if chat.ProjectID != projectID {
			return nil, nil, fmt.Errorf("conversation %d does not belong to project %d", chatID, projectID)
		}
		history, err := s.store.ListIntelChatMessages(ctx, chatID, 20)
		if err != nil {
			return nil, nil, err
		}
		return chat, history, nil
	}
	chat := &store.IntelChat{ProjectID: projectID, Title: truncateRunes(strings.TrimSpace(question), 40)}
	if err := s.store.CreateIntelChat(ctx, chat); err != nil {
		return nil, nil, err
	}
	return chat, nil, nil
}

// generateChatAnswer builds a multi-turn prompt: the fixed system role, then
// recent history (user/assistant turns), then the current question grounded in
// the retrieved fragments.
func (s *Server) generateChatAnswer(ctx context.Context, history []*store.IntelChatMessage, contextText, question string) string {
	system := "你是项目代码知识库助手。只依据给定的项目上下文和历史对话回答问题，引用上下文中的事实；如果上下文不足以回答，请明确说明，不要编造。"
	messages := []llm.ChatMessage{{Role: "system", Content: system}}
	for _, m := range history {
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		messages = append(messages, llm.ChatMessage{Role: role, Content: m.Content})
	}
	messages = append(messages, llm.ChatMessage{
		Role:    "user",
		Content: "项目上下文：\n" + contextText + "\n问题：" + question,
	})
	out, err := s.llm.Chat(ctx, messages)
	if err != nil {
		log.Printf("intel ask llm: %v", err)
		return ""
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

var errEmbeddingDisabled = &pathErr{msg: "embeddings not configured (set 嵌入模型配置 in settings)"}
