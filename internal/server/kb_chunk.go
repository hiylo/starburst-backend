package server

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hiylo/starburst-backend/internal/store"
)

// KB 分块与向量化的纯逻辑：把 ingest 的文本/ Markdown 切成有标题的片段，再批量
// 向量化。分块规则与 RAG_PROMPT 设计文档 §5 一致：按标题分组、片段目标大小见
// kbChunkTargetRunes。

// kbChunkTargetRunes 是单个片段的正文目标长度（rune 数），超出即从行边界处拆出。
const kbChunkTargetRunes = 800

// kbChunkCandidate 是向量化前的文本片段：标题 + 正文。
type kbChunkCandidate struct {
	title   string
	content string
}

// splitKnowledgeText splits text/markdown into titled chunks. Markdown headings
// (`#`..`######`) start a new chunk; body accumulates until it reaches
// kbChunkTargetRunes runes or the end of input. When the document has no
// headings the chunk title falls back to the document name.
func splitKnowledgeText(name, content string) []kbChunkCandidate {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	title := strings.TrimSpace(name)
	lines := strings.Split(content, "\n")
	out := make([]kbChunkCandidate, 0, 8)
	buf := make([]string, 0, 16)
	size := 0
	flush := func() {
		trimmed := strings.TrimSpace(strings.Join(buf, "\n"))
		if trimmed != "" {
			out = append(out, kbChunkCandidate{title: title, content: trimmed})
		}
		buf = buf[:0]
		size = 0
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isMarkdownHeading(trimmed) {
			flush()
			title = strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			continue
		}
		if trimmed == "" {
			if size >= kbChunkTargetRunes {
				flush()
			}
			continue
		}
		buf = append(buf, line)
		size += utf8.RuneCountInString(line) + 1
		if size >= kbChunkTargetRunes {
			flush()
		}
	}
	flush()
	if len(out) == 0 && strings.TrimSpace(content) != "" {
		out = append(out, kbChunkCandidate{title: title, content: strings.TrimSpace(content)})
	}
	return out
}

func isMarkdownHeading(line string) bool {
	if line == "" || line[0] != '#' {
		return false
	}
	i := 0
	for i < len(line) && line[i] == '#' && i < 6 {
		i++
	}
	return i < len(line) && (line[i] == ' ' || line[i] == '\t')
}

// embedKnowledgeChunks chunks the raw content and embeds every chunk via the
// configured embeddings endpoint, returning ready-to-insert store chunks. A
// chunk whose embedding dimension mismatches the column is a hard error (the
// whole ingest fails rather than persisting half a document).
func (s *Server) embedKnowledgeChunks(ctx context.Context, doc *store.KBDocument, raw string) ([]*store.KBChunk, error) {
	if s.embedding == nil || !s.embedding.Enabled() {
		return nil, errEmbeddingDisabled
	}
	candidates := splitKnowledgeText(doc.Name, raw)
	if len(candidates) == 0 {
		return nil, nil
	}

	// 批量向量化：每次 EmbedBatch 至多 embedBatchSize 条，避免单请求体过大。
	const embedBatchSize = 16
	chunks := make([]*store.KBChunk, 0, len(candidates))
	for start := 0; start < len(candidates); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		batch := candidates[start:end]
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.content
		}
		vecs, err := s.embedding.EmbedBatch(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("embed batch: %w", err)
		}
		for i, c := range batch {
			if len(vecs[i]) != store.EmbedDim {
				return nil, fmt.Errorf("embedding dimension %d does not match column dimension %d (model mismatch?)",
					len(vecs[i]), store.EmbedDim)
			}
			chunks = append(chunks, &store.KBChunk{
				Seq:       start + i + 1,
				Title:     c.title,
				Content:   c.content,
				Embedding: vecs[i],
			})
		}
	}
	return chunks, nil
}
