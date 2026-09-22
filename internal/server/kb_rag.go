package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// RAG-in-Prompt（Go 端拼装，docs/RAG_PROMPT.md）：拦截 POST /api/opencode/session/{id}
// /prompt_async，检索知识库 Top-K，命中则在 parts 最前追加上下文 text part 再转发，
// 未命中 / 能力缺失 / 超时一律原样转发。RAG 是锦上添花，绝不让消息发送失败或变慢。

// ragQueryTimeoutDefault 是代知识库检索的默认上限（可被 --rag-timeout 覆盖）：
// 实测 embedding 往返可达 ~8s，原 800ms 会让绝大多数命中被超时丢弃；命中时
// 消息多等几秒换取可靠上下文（prompt_async 是 fire-and-forget，客户端无感）。
const ragQueryTimeoutDefault = 8 * time.Second

// ragTimeout returns the configured retrieval timeout, defaulting to
// ragQueryTimeoutDefault when unset.
func (s *Server) ragTimeout() time.Duration {
	if s.cfg != nil && s.cfg.RagTimeout > 0 {
		return s.cfg.RagTimeout
	}
	return ragQueryTimeoutDefault
}

// ragStats 统计 RAG-in-Prompt 的结局分布（docs/RAG_PROMPT.md §9）：spliced 之外
// 的 skip* 归因让「知识库有没有被用上」可观测。
type ragStats struct {
	spliced            atomic.Int64 // 命中并拼装
	skipNoEmbedding    atomic.Int64 // embedding 未配置
	skipNoVector       atomic.Int64 // 无 pgvector（SQLite）
	skipTimeout        atomic.Int64 // 检索超时
	skipNoResult       atomic.Int64 // 检索无命中 / 检索失败
	skipBelowThreshold atomic.Int64 // 有命中但低于相关度阈值 / 预算放不下
}

func newRagStats() *ragStats { return &ragStats{} }

func (rs *ragStats) snapshot() map[string]any {
	return map[string]any{
		"spliced":            rs.spliced.Load(),
		"skipNoEmbedding":    rs.skipNoEmbedding.Load(),
		"skipNoVector":       rs.skipNoVector.Load(),
		"skipTimeout":        rs.skipTimeout.Load(),
		"skipNoResult":       rs.skipNoResult.Load(),
		"skipBelowThreshold": rs.skipBelowThreshold.Load(),
	}
}

// ragMinScore 是 Top-K 检索的相关度阈值，低于即弃（低质量噪音不如不拼）。
const ragMinScore = 0.5

// ragTopK 是单次检索的片段数上限。
const ragTopK = 5

// ragBudgetTokens 是拼装块的 token 预算上限（沿用文档 §7）。
const ragBudgetTokens = 1500

// ragHeader / ragFooter 界定「资料区」与「提问区」，让模型不把资料当指令执行。
const (
	ragHeader = "[RAG_CONTEXT_START]\n" +
		"以下是知识库检索到的参考资料，供回答本次问题使用。\n" +
		"请优先依据资料作答；资料未覆盖的部分请如实说明，不要编造。\n" +
		"引用结论时可标注对应来源，例如 [来源1]。\n\n"
	ragFooter = "\n[RAG_CONTEXT_END]"
)

// promptAsyncPath tells whether an upstream path is the fire-and-forget prompt
// endpoint (POST /session/{id}/prompt_async).
func promptAsyncPath(method, path string) bool {
	return method == http.MethodPost && strings.HasSuffix(path, "/prompt_async")
}

// promptPartBody is the JSON shape of one prompt part, used both to read the
// original body and to re-serialize it after splicing. Unknown fields on the
// original body (beyond the standard PromptPart keys) are dropped — the parts
// the APP/Web send today only carry these keys, matching opencode.PromptPart.
type promptPartBody struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Path     string `json:"path,omitempty"`
	Mime     string `json:"mime,omitempty"`
	URL      string `json:"url,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// promptAsyncBody is the full request shape of POST /session/{id}/prompt_async.
// Fields outside parts are preserved via json.RawMessage so nothing else in the
// body is dropped when we splice.
type promptAsyncBody struct {
	MessageID string           `json:"messageID"`
	Parts     []promptPartBody `json:"parts"`
	Model     json.RawMessage  `json:"model,omitempty"`
	Agent     string           `json:"agent,omitempty"`
	Variant   string           `json:"variant,omitempty"`
	System    string           `json:"system,omitempty"`
	Tools     json.RawMessage  `json:"tools,omitempty"`
}

// ragSpliceJSON reads a prompt_async body, optionally injects the KB context as
// the first text part, and returns the (possibly rewritten) body to forward.
//
// It returns spliced=false and the original body when: there is no user text to
// seed the query, retrieval returns nothing above ragMinScore, the KB is not
// available (embedding disabled / no pgvector) or the search times out — the
// failure modes must never block or alter the user's message.
func (s *Server) ragSpliceJSON(ctx context.Context, body []byte) (out []byte, spliced bool) {
	var prompt promptAsyncBody
	if err := json.Unmarshal(body, &prompt); err != nil {
		return body, false
	}
	query := lastUserText(prompt.Parts)
	if query == "" || !ragQueryWorthy(query) {
		return body, false
	}

	searchCtx, cancel := context.WithTimeout(ctx, s.ragTimeout())
	defer cancel()
	hits, err := s.searchKB(searchCtx, query, nil, ragTopK, ragMinScore)
	if err != nil {
		// 任何检索失败都不阻塞发送：按「没找到」原样转发。
		if errors.Is(err, context.DeadlineExceeded) {
			s.ragCounters.skipTimeout.Add(1)
		} else if errors.Is(err, store.ErrRagUnsupported) {
			s.ragCounters.skipNoVector.Add(1)
			log.Printf("rag splice: search skip: %v", err)
		} else {
			s.ragCounters.skipNoResult.Add(1)
			log.Printf("rag splice: search skip: %v", err)
		}
		return body, false
	}
	if len(hits) == 0 {
		s.ragCounters.skipNoResult.Add(1)
		return body, false
	}
	contextText := ragPromptBlock(hits)
	if contextText == "" {
		s.ragCounters.skipBelowThreshold.Add(1)
		return body, false
	}

	prompt.Parts = append([]promptPartBody{{Type: "text", Text: contextText}}, prompt.Parts...)
	rewritten, err := json.Marshal(&prompt)
	if err != nil {
		s.ragCounters.skipNoResult.Add(1)
		return body, false
	}
	s.ragCounters.spliced.Add(1)
	return rewritten, true
}

func lastUserText(parts []promptPartBody) string {
	var texts []string
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			texts = append(texts, strings.TrimSpace(p.Text))
		}
	}
	if len(texts) == 0 {
		return ""
	}
	return texts[len(texts)-1]
}

// ragQueryWorthy gates whether a user message should trigger KB retrieval.
// Without this gate, short conversational turns (“继续”、“好的”、“谢谢”, or a bare
// continuation prompt) would each pull a bulky context block into the bubble —
// the "RAG 太乱" feedback. A message is query-worthy when it is long enough to be
// a real prompt, contains a question mark, or carries explicit query keywords.
func ragQueryWorthy(text string) bool {
	runes := []rune(text)
	if len(runes) >= 30 {
		return true
	}
	if strings.ContainsRune(text, '?') || strings.ContainsRune(text, '？') {
		return true
	}
	for _, kw := range []string{
		"怎么", "如何", "为什么", "是什么", "哪些", "能不能", "怎么用", "有何", "几个",
		"帮我", "找", "查", "分析", "总结", "比较", "推荐", "介绍", "解释", "区别",
		"规则", "库存", "配置", "设置", "文档", "文件", "功能", "能力", "支持", "是否",
	} {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

// ragPromptBlock renders the knowledge hits into the injected context block.
// Returns "" when nothing passes the relevance gate or the block cannot fit the
// token budget. Format follows docs/RAG_PROMPT.md §5: per-hit `[来源N]` + source
// label, ordered by score descending, truncated to the budget.
func ragPromptBlock(hits []kbHit) string {
	if len(hits) == 0 {
		return ""
	}
	budget := ragBudgetTokens
	headUsed := estimateGoTokens(ragHeader)
	footUsed := estimateGoTokens(ragFooter)
	if headUsed+footUsed >= budget {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(ragHeader)
	used := headUsed
	index := 0
	for _, h := range hits {
		index++
		label := fmt.Sprintf("[来源%d] %s · %s（相关度 %.2f）\n", index, h.source, h.section, h.score)
		entry := label + h.content + "\n\n"
		t := estimateGoTokens(entry)
		if used+footUsed+t <= budget {
			sb.WriteString(entry)
			used += t
			continue
		}
		// 放不下就截断正文到剩余预算，截断后仍在预算内则保留。
		remain := budget - used - footUsed
		if remain > 0 {
			if truncated := truncateGoText(entry, remain); truncated != "" {
				sb.WriteString(truncated)
			}
		}
		break
	}
	rendered := sb.String() + ragFooter
	if strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(rendered, ragFooter), ragHeader)) == "" {
		return ""
	}
	return rendered
}

// truncateGoText cuts text to fit budgetTokens (estimated), keeping a trailing
// truncation marker so the model knows the source was elided.
func truncateGoText(text string, budgetTokens int) string {
	if budgetTokens <= 0 {
		return ""
	}
	const suffix = "\n…（超出上下文预算，已截断）"
	if estimateGoTokens(text) <= budgetTokens {
		return text
	}
	labelEnd := strings.IndexByte(text, '\n')
	label := ""
	rest := text
	if labelEnd >= 0 {
		label = text[:labelEnd+1]
		rest = text[labelEnd+1:]
	}
	if estimateGoTokens(label+suffix) > budgetTokens {
		return ""
	}
	low, high, best := 0, len(rest), ""
	for low <= high {
		mid := (low + high) / 2
		candidate := label + rest[:mid] + suffix
		if estimateGoTokens(candidate) <= budgetTokens {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best
}

// estimateGoTokens 是 Go 侧的轻量 token 估算，与 App 侧启发式一致：ASCII 约
// 4 字符/token，CJK 约 1.5 字符/token。仅用于预算粗控。
func estimateGoTokens(text string) int {
	ascii, cjk := 0, 0
	for _, r := range text {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF ||
			r >= 0x3040 && r <= 0x30FF || r >= 0xAC00 && r <= 0xD7AF {
			cjk++
		} else {
			ascii++
		}
	}
	return int(float64(ascii)/4.0 + float64(cjk)/1.5 + 0.999)
}

// applyRagSpliceToProxy is called from handleOpenCodeProxy before forwarding a
// prompt_async request: it buffers the body, optionally splices KB context, and
// returns the body to forward plus whether a splice happened. When the KB is
// not configured it short-circuits to the original stream (no buffering cost).
func (s *Server) applyRagSpliceToProxy(r *http.Request) (io.Reader, bool) {
	if s.embedding == nil || !s.embedding.Enabled() {
		s.ragCounters.skipNoEmbedding.Add(1)
		return r.Body, false
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return bytes.NewReader(body), false
	}
	out, spliced := s.ragSpliceJSON(r.Context(), body)
	return bytes.NewReader(out), spliced
}

// handleKbStats reports RAG-in-Prompt outcome counters (docs §8).
func (s *Server) handleKbStats(w http.ResponseWriter, r *http.Request) {
	if !s.requireDualAuth(r) {
		writeErr(w, http.StatusUnauthorized, "web session or APP token required")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rag": s.ragCounters.snapshot()})
}
