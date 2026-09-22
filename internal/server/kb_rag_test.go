package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/embed"
	"github.com/hiylo/starburst-backend/internal/store"
)

// TestPromptAsyncPath pins the interception target.
func TestPromptAsyncPath(t *testing.T) {
	if !promptAsyncPath(http.MethodPost, "/session/s1/prompt_async") {
		t.Fatal("POST /session/{id}/prompt_async should match")
	}
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/session/s1/prompt_async"},
		{http.MethodPost, "/session/s1/message"},
		{http.MethodPost, "/session/s1/shell"},
	} {
		if promptAsyncPath(tc.method, tc.path) {
			t.Fatalf("%s %s should not match", tc.method, tc.path)
		}
	}
}

// TestLastUserText covers which part seeds the KB query.
func TestLastUserText(t *testing.T) {
	parts := []promptPartBody{
		{Type: "text", Text: "第一句"},
		{Type: "file", Path: "/tmp/x.png"},
		{Type: "text", Text: "  第二句  "},
	}
	if got := lastUserText(parts); got != "第二句" {
		t.Fatalf("lastUserText = %q, want 第二句", got)
	}
	if got := lastUserText([]promptPartBody{{Type: "file", Path: "/x"}}); got != "" {
		t.Fatalf("no text part should yield empty, got %q", got)
	}
}

func TestEstimateGoTokens(t *testing.T) {
	if estimateGoTokens("") != 0 {
		t.Fatal("empty string should be 0 tokens")
	}
	// 等长字符数下，CJK 密度高于 ASCII（约 4 字符/token vs 1.5 字符/token），
	// 所以同长度中文估算的 token 数应明显更多。
	ascii := estimateGoTokens(strings.Repeat("a", 120))
	cjk := estimateGoTokens(strings.Repeat("测", 120))
	if cjk <= ascii {
		t.Fatalf("cjk=%d should be > ascii=%d", cjk, ascii)
	}
	if cjk > ascii*3 {
		t.Fatalf("cjk=%d vs ascii=%d 差异超出密度比", cjk, ascii)
	}
}

// TestRagPromptBlock covers the injected format: delimiters, per-hit source
// labels, score ordering and the budget guard.
func TestRagPromptBlock(t *testing.T) {
	hits := []kbHit{
		{source: "《仓储管理制度.docx》", section: "第 4 章", content: "安全库存 = 日均出库量 × 1.2。", score: 0.93},
		{source: "《12月库存报表.xlsx》", section: "Sheet2", content: "甲类 SKU 处于短缺预警状态。", score: 0.84},
	}
	block := ragPromptBlock(hits)
	if !strings.HasPrefix(block, "[RAG_CONTEXT_START]") {
		t.Fatalf("missing start marker: %q", block)
	}
	if !strings.HasSuffix(block, "[RAG_CONTEXT_END]") {
		t.Fatalf("missing end marker: %q", block)
	}
	if !strings.Contains(block, "[来源1]") || !strings.Contains(block, "[来源2]") {
		t.Fatalf("missing source labels: %q", block)
	}
	if strings.Index(block, "《仓储管理制度.docx》") > strings.Index(block, "《12月库存报表.xlsx》") {
		t.Fatalf("hits not score-descending: %q", block)
	}

	if ragPromptBlock(nil) != "" {
		t.Fatal("empty hits should produce no block")
	}
	if ragPromptBlock([]kbHit{{source: "a", section: "s", content: "x", score: 0.99}}) == "" {
		t.Fatal("single hit should produce a block")
	}
}

// TestRagPromptBlockBudgetTruncation ensures a too-long chunk is cut to the
// budget rather than silently dropped or overflowing.
func TestRagPromptBlockBudgetTruncation(t *testing.T) {
	hits := []kbHit{{source: "长文.pdf", section: "第 3 章",
		content: strings.Repeat("这一段内容非常长，用来验证预算截断逻辑。", 400), score: 0.99}}
	block := ragPromptBlock(hits)
	if block == "" {
		t.Fatal("should still inject (truncated)")
	}
	if !strings.Contains(block, "已截断") {
		t.Fatalf("missing truncation marker: %q", block)
	}
	if estimateGoTokens(block) > ragBudgetTokens {
		t.Fatalf("block over budget: %d > %d", estimateGoTokens(block), ragBudgetTokens)
	}
}

// TestRagSpliceJSONDegradedOnNoKB pins the no-block/no-fail contract: on a
// SQLite deployment (no pgvector) the prompt body must pass through unchanged.
func TestRagSpliceJSONDegradedOnNoKB(t *testing.T) {
	s := newTestServer(t)
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vec := make([]float32, store.EmbedDim)
		for i := range vec {
			vec[i] = 0.01
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": vec}},
		})
	}))
	t.Cleanup(embSrv.Close)
	s.SetEmbedding(embed.New(embSrv.URL, "", "test-model"))

	body := []byte(`{"messageID":"m1","parts":[{"type":"text","text":"安全库存规则是什么？"}],"agent":"build"}`)
	out, spliced := s.ragSpliceJSON(t.Context(), body)
	if spliced {
		t.Fatal("SQLite (no pgvector) must not splice")
	}
	if string(out) != string(body) {
		t.Fatalf("body changed on degrade: %s", out)
	}
}

// TestProxyPromptAsyncRagHeader verifies the X-Rag-Spliced header wiring and
// that an unconfigured KB short-circuits to the original body (embedding is
// nil on newTestServer).
func TestProxyPromptAsyncRagHeader(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/opencode/session/s1/prompt_async",
		`{"messageID":"m1","parts":[{"type":"text","text":"你好"}]}`, wh)
	if rec.Header().Get("X-Rag-Spliced") != "0" {
		t.Fatalf("X-Rag-Spliced = %q, want 0 (KB not configured)", rec.Header().Get("X-Rag-Spliced"))
	}
}

// TestRagTimeoutConfig pins the configurable retrieval timeout: default 8s
// (embedding round trips are ~8s, 800ms dropped most splices), overridable via
// cfg.RagTimeout.
func TestRagTimeoutConfig(t *testing.T) {
	s := newTestServer(t)
	if got := s.ragTimeout(); got != 8*time.Second {
		t.Fatalf("default ragTimeout = %v, want 8s", got)
	}
	s.cfg.RagTimeout = 1500 * time.Millisecond
	if got := s.ragTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("overridden ragTimeout = %v, want 1.5s", got)
	}
	s.cfg.RagTimeout = 0
	if got := s.ragTimeout(); got != 8*time.Second {
		t.Fatalf("zero-override ragTimeout = %v, want default 8s", got)
	}
}
