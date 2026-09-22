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

// TestRagQueryWorthy pins the retrieval-intent gate: casual/continuation chatter
// must NOT trigger a KB splice, real questions/requests must.
func TestRagQueryWorthy(t *testing.T) {
	skip := []string{
		"继续", "好的", "谢谢", "收到", "嗯", "ok",
		"继续吧", "再来",
		"可以了", "就这样",
		"什么意思？增加了一个配置项对吗？", // 纯追问/元问题，不应触发检索
		"你看看这个",
	}
	for _, s := range skip {
		if ragQueryWorthy(s) {
			t.Errorf("casual %q should NOT trigger retrieval", s)
		}
	}
	ask := []string{
		"安全库存的规则是什么？",
		"帮我找一下 agent 工具的说明",
		"为什么 web 端能用知识库而 app 不行",
		"计划模式支持哪些只读工具",
		"请分析一下这 30 个字符以上的长问题（即便没有问号也应该触发，因为够长像真需求）",
	}
	for _, s := range ask {
		if !ragQueryWorthy(s) {
			t.Errorf("query %q should trigger retrieval", s)
		}
	}
	// 边界：恰好 30 rune 的文本（无问号、无关键词）算 query（够长）。
	if !ragQueryWorthy("一二三四五六七八九十一二三四五六七八九十一二三四五六七八九十") {
		t.Error("30-rune text should be query-worthy")
	}
	// 问号单独触发。
	if !ragQueryWorthy("规则是？") {
		t.Error("question mark should trigger")
	}
}

// TestKBSearchCacheKeys pins the cache-key contract: same scope+query share a
// key, different scope/query/topK/minScore do not, and collection order in the
// scope does not matter.
func TestKBSearchCacheKeys(t *testing.T) {
	k1 := kbSearchCacheKey("安全库存规则", []int64{1, 2}, 5, 0.5)
	k2 := kbSearchCacheKey("安全库存规则", []int64{2, 1}, 5, 0.5)
	if k1 != k2 {
		t.Fatalf("collection order should not matter: %s != %s", k1, k2)
	}
	if k1 == kbSearchCacheKey("安全库存规则", []int64{1, 3}, 5, 0.5) {
		t.Fatal("different scope must not collide")
	}
	if k1 == kbSearchCacheKey("别的查询", []int64{1, 2}, 5, 0.5) {
		t.Fatal("different query must not collide")
	}
	if k1 == kbSearchCacheKey("安全库存规则", []int64{1, 2}, 10, 0.5) {
		t.Fatal("different topK must not collide")
	}
	if k1 == kbSearchCacheKey("安全库存规则", []int64{1, 2}, 5, 0.6) {
		t.Fatal("different minScore must not collide")
	}
}

// TestKBSearchCacheLifecycle covers get/put/eviction/invalidateAll on the
// bounded LRU-ish cache used by searchKB.
func TestKBSearchCacheLifecycle(t *testing.T) {
	c := newKBSearchCache(2)
	c.put("a", []kbHit{{source: "s1", score: 0.9}})
	if hits, ok := c.get("a"); !ok || len(hits) != 1 || hits[0].source != "s1" {
		t.Fatalf("get after put = %+v, %v", hits, ok)
	}
	c.put("b", []kbHit{{source: "s2"}})
	c.put("c", []kbHit{{source: "s3"}})
	if _, ok := c.get("a"); ok {
		t.Fatal("oldest entry should be evicted at cap")
	}
	// get moves entry to MRU, so "b" survives the next put while "c" (now
	// oldest) is evicted.
	if _, ok := c.get("b"); !ok {
		t.Fatal("b should be present")
	}
	c.put("d", []kbHit{{source: "s4"}})
	if _, ok := c.get("b"); !ok {
		t.Fatal("b was bumped to MRU and should survive")
	}
	if _, ok := c.get("c"); ok {
		t.Fatal("c should be evicted as the oldest after b was bumped")
	}
	if _, ok := c.get("d"); !ok {
		t.Fatal("d should be present")
	}
	c.invalidateAll()
	if _, ok := c.get("b"); ok {
		t.Fatal("invalidateAll should drop every entry")
	}
}

// TestRagCollectionScope pins the collection-scope plumbing: unset cfg → nil
// (all collections), configured ids → returned verbatim.
func TestRagCollectionScope(t *testing.T) {
	s := newTestServer(t)
	if got := s.ragCollectionScope(); got != nil {
		t.Fatalf("default scope = %v, want nil (all)", got)
	}
	s.cfg.RagCollectionIDs = []int64{3, 7}
	if got := s.ragCollectionScope(); len(got) != 2 || got[0] != 3 || got[1] != 7 {
		t.Fatalf("configured scope = %v", got)
	}
}
