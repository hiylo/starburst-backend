package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hiylo/opencode-backend/internal/llm"
)

// fakeLLM serves an OpenAI-compatible /chat/completions endpoint. It records
// the last user message so tests can assert what was sent to the model.
func fakeLLM(t *testing.T, reply string) (*httptest.Server, *string) {
	t.Helper()
	var lastUser string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if m.Role == "user" {
				lastUser = m.Content
			}
		}
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &lastUser
}

// failingLLM always answers 500, to exercise the graceful-degradation path.
func failingLLM(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSTTRefineFixesDuplicates(t *testing.T) {
	raw := "昨天是 MONDAY MONDAY TODAY 的的 周末"
	fixed := "昨天是 MONDAY TODAY 的 周末"
	srv, lastUser := fakeLLM(t, fixed)
	s := newTestServer(t)
	s.SetLLM(llm.New(srv.URL, "key", "test-model"))
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodPost, "/api/stt/refine",
		`{"text":`+mustQuote(raw)+`}`, th)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out sttRefineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Text != fixed || !out.Changed {
		t.Fatalf("out %+v want text=%q changed=true", out, fixed)
	}
	if *lastUser != raw {
		t.Fatalf("user prompt %q want %q", *lastUser, raw)
	}
}

// TestSTTRefineReturnsUnchangedWhenModelAgrees covers the "text is already
// fine" path: the model echoes the input and the response must report that no
// change was made.
func TestSTTRefineReturnsUnchangedWhenModelAgrees(t *testing.T) {
	raw := "昨天是周一"
	srv, _ := fakeLLM(t, raw)
	s := newTestServer(t)
	s.SetLLM(llm.New(srv.URL, "key", "test-model"))
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodPost, "/api/stt/refine", `{"text":"`+raw+`"}`, th)
	var out sttRefineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Text != raw || out.Changed {
		t.Fatalf("out %+v want text=%q changed=false", out, raw)
	}
}

func TestSTTRefineStripsCodeFences(t *testing.T) {
	srv, _ := fakeLLM(t, "```text\n昨天是周一\n```")
	s := newTestServer(t)
	s.SetLLM(llm.New(srv.URL, "key", "test-model"))
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodPost, "/api/stt/refine", `{"text":"昨天是是周一"}`, th)
	var out sttRefineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Text != "昨天是周一" {
		t.Fatalf("text %q want 昨天是周一", out.Text)
	}
}

// TestSTTRefineRejectsUnrelatedOutput guards the case where the model answers
// or rewrites the sentence instead of repairing it.
func TestSTTRefineRejectsUnrelatedOutput(t *testing.T) {
	srv, _ := fakeLLM(t, "这是关于星期几的问答。")
	s := newTestServer(t)
	s.SetLLM(llm.New(srv.URL, "key", "test-model"))
	th := sttTestToken(t, s)

	rec := s.do(t, http.MethodPost, "/api/stt/refine", `{"text":"昨天是 MONDAY TODAY IS THE DAY"}`, th)
	var out sttRefineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Changed || out.Text != "昨天是 MONDAY TODAY IS THE DAY" {
		t.Fatalf("out %+v must fall back to the original text", out)
	}
	if out.Reason == "" {
		t.Fatal("expected a reason when the output is rejected")
	}
}

func TestSTTRefineDegradations(t *testing.T) {
	// One server throughout: tokens are verified against that server's auth
	// manager, so a token minted on a different instance is rejected.
	s := newTestServer(t)
	th := sttTestToken(t, s)
	payload := `{"text":"昨天的话"}`

	// No LLM configured at all.
	rec := s.do(t, http.MethodPost, "/api/stt/refine", payload, th)
	var out sttRefineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Code != http.StatusOK || out.Changed || out.Text != "昨天的话" {
		t.Fatalf("no-llm: status %d out %+v", rec.Code, out)
	}
	if out.Reason != "llm not configured" {
		t.Fatalf("reason %q", out.Reason)
	}

	// LLM configured but unreachable.
	s.SetLLM(llm.New("http://127.0.0.1:1", "key", "test-model"))
	rec = s.do(t, http.MethodPost, "/api/stt/refine", payload, th)
	out = sttRefineResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Code != http.StatusOK || out.Changed || out.Text != "昨天的话" {
		t.Fatalf("unreachable-llm: status %d out %+v", rec.Code, out)
	}

	// LLM answers 500.
	s.SetLLM(llm.New(failingLLM(t).URL, "key", "test-model"))
	rec = s.do(t, http.MethodPost, "/api/stt/refine", payload, th)
	out = sttRefineResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Code != http.StatusOK || out.Changed || out.Reason != "llm failed" {
		t.Fatalf("llm-500: status %d out %+v", rec.Code, out)
	}
}

func TestSTTRefineValidation(t *testing.T) {
	srv, _ := fakeLLM(t, "ok")
	s := newTestServer(t)
	s.SetLLM(llm.New(srv.URL, "key", "test-model"))
	th := sttTestToken(t, s)

	if rec := s.do(t, http.MethodPost, "/api/stt/refine", `{}`, th); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty text status %d", rec.Code)
	}
	if rec := s.do(t, http.MethodPost, "/api/stt/refine", `{"text":"  "}`, th); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank text status %d", rec.Code)
	}
	if rec := s.do(t, http.MethodPost, "/api/stt/refine", `{not json`, th); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json status %d", rec.Code)
	}
	if rec := s.do(t, http.MethodPost, "/api/stt/refine", `{"text":"x"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token status %d", rec.Code)
	}
	if rec := s.do(t, http.MethodGet, "/api/stt/refine", "", th); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get status %d", rec.Code)
	}
}

func TestRefinePlausible(t *testing.T) {
	cases := []struct {
		name     string
		original string
		cleaned  string
		want     bool
	}{
		{"fixes duplicates", "昨天是是周一", "昨天是周一", true},
		{"collapses repetition", "MONDAY MONDAY TODAY", "MONDAY TODAY", true},
		{"trivially changed", "abc", "abcd", true},
		{"empty output", "昨天是周一", "", false},
		{"rewritten sentence", "昨天是周一", "关于时间的问题", false},
		{"too short", "这是一个比较长的原始识别文本内容", "周一", false},
		{"too long", "周一", "这是一个被模型扩写得非常冗长的回复内容哦", false},
	}
	for _, tc := range cases {
		if got := refinePlausible(tc.original, tc.cleaned); got != tc.want {
			t.Errorf("%s: refinePlausible(%q,%q)=%v want %v",
				tc.name, tc.original, tc.cleaned, got, tc.want)
		}
	}
}

func TestStripFences(t *testing.T) {
	cases := map[string]string{
		"```text\n昨天是周一\n```": "昨天是周一",
		"```\n昨天是周一\n```":     "昨天是周一",
		"昨天是周一":               "昨天是周一",
	}
	for in, want := range cases {
		if got := stripFences(in); got != want {
			t.Errorf("stripFences(%q)=%q want %q", in, got, want)
		}
	}
}

// mustQuote renders s as a JSON string literal for the request bodies below.
func mustQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestDedupTranscript(t *testing.T) {
	cases := map[string]string{
		"昨天是是周一":                                "昨天是周一",
		"我们们一起起吃饭饭":                             "我们一起吃饭",
		"饭饭饭好吃":                                 "饭好吃",
		"昨天是 MONDAY MONDAY，TODAY TODAY IS LIBR": "昨天是 MONDAY，TODAY IS LIBR",
		"昨天是 MONDAY MONDAY MONDAY":              "昨天是 MONDAY",
		"是是是":                                   "是",
		"昨天的话":                                  "昨天的话",
		"看看说说一一":                                "看看说说一一",
		"慢慢渐渐往往":                                "慢慢渐渐往往",
		"妈妈爸爸好":                                 "妈妈爸爸好",
		"杀杀敌发发果断如如今":                            "杀敌发果断如今",
		"我我们们吃饭饭饭":                              "我们吃饭",
		"娃娃宝宝哈哈":                                "娃娃宝宝哈哈",
		"MONDAY, MONDAY":                        "MONDAY, MONDAY",
		"，，，好。。好":                               "，好。好",
		// 整段前缀被引擎重新解码后全文重现（中间重复，非尾部），
		// 老逻辑 trimTailRepeat 管不到，dedupAdjacentRepeat 处理。
		"今天天气真好今天天气真好我们":                       "今天天气真好我们",
		// 相邻重复块之间允许一个空格分隔。
		"想将军去杀敌 想将军去杀敌回来":                     "想将军去杀敌回来",
	}
	for in, want := range cases {
		if got := dedupTranscript(in); got != want {
			t.Errorf("dedupTranscript(%q)=%q want %q", in, got, want)
		}
	}
}

// TestDedupTranscriptRealJiangjun regresses the actual transcript the user
// pasted after dictating a long drama line: the engine re-emitted the whole
// accumulated prefix once and double-wrote nearly every content word. Every
// reproduction of the original must be reachable from the source, and the
// output must no longer contain the double-writes or the duplicated prefix.
func TestDedupTranscriptRealJiangjun(t *testing.T) {
	in := "想将军在片宾面馆关杀杀敌杀伐发发果果断如如今为女儿恨恨恨道到极致明连陛下牵连林都丝毫好不回 想将军在片宾面馆关杀杀敌杀伐发发果果断如如今为女儿恨恨恨道到极致明连陛下牵连林都丝毫好不回收敛怒怒火火呵赫赫赫连莲立刻停停停手御玉浅御御前动送动动私刑有势有失失朝朝日朝朝臣提面提面面当你当初他他亲手天挑断你今京今朝昭琉兽首守金把我女儿丢丢丢丢进入烟烟雨楼楼任任践践踏她TERN时他可讲过柔柔弱弱博吒博同同同情景请景狗勾结勾结姐她太太太子算算算算计计我正正镇国果兵权请景景今日日这边的鞭哨伤的是是他欠欠我女儿的长偿偿长还当当着这天天子子的命令"
	got := dedupTranscript(in)
	if strings.Contains(got, "杀杀") {
		t.Errorf("still contains 杀杀: %q", got)
	}
	if strings.Contains(got, "想将军在片宾面馆关杀杀敌杀伐") && strings.Count(got, "想将军") > 1 {
		t.Errorf("duplicated prefix survives: %q", got)
	}
	if strings.Count(got, "想将军") != 1 {
		t.Errorf("想将军 must appear exactly once, got %d: %q", strings.Count(got, "想将军"), got)
	}
	if strings.Contains(got, "哎") {
		t.Errorf("unexpected 哎: %q", got)
	}
}
