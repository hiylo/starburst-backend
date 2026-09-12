package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// wordTokens splits a transcript into words, whitespace runs and everything else,
// so dedupWords can rewrite it without losing punctuation.
var wordTokens = regexp.MustCompile(`[A-Za-z]+|\s+|[^\sA-Za-z]+`)

// dedupWords collapses consecutive identical ASCII words ("MONDAY MONDAY" ->
// "MONDAY"). Go's RE2 has no backreferences, so this is done by hand; a
// punctuation mark between the two words counts as a new clause and is kept.
func dedupWords(s string) string {
	fields := wordTokens.FindAllString(s, -1)
	if len(fields) < 3 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	lastWord := ""
	lastWordAtEnd := false
	for _, f := range fields {
		if isASCIILetterRun(f) {
			lower := strings.ToLower(f)
			if lastWordAtEnd && lastWord == lower {
				trimmed := strings.TrimRight(b.String(), " \t")
				b.Reset()
				b.WriteString(trimmed)
				continue
			}
			lastWord, lastWordAtEnd = lower, true
			b.WriteString(f)
			continue
		}
		if !isSpaceRun(f) {
			lastWordAtEnd = false
		}
		b.WriteString(f)
	}
	return b.String()
}

func isASCIILetterRun(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

func isSpaceRun(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' {
			return false
		}
	}
	return true
}

func isPunctRun(s string) bool {
	for _, r := range s {
		if !isPunctRune(r) {
			return false
		}
	}
	return s != ""
}

func isPunctRune(r rune) bool {
	switch r {
	case '，', '。', '！', '？', '、', '；', '：':
		return true
	default:
		return false
	}
}

// punctRun matches two or more punctuation marks in a row; the replacement keeps
// only the first one. RE2 has no backreferences, so a run of *any* marks is
// matched and collapsed instead of a run of the *same* mark.
var punctRun = regexp.MustCompile(`[，。！？、；：]{2,}`)

// refineMaxInputBytes caps the transcript size sent to the model. A full day of
// dictation would still fit; a 30s recording is a few hundred bytes.
const refineMaxInputBytes = 16 * 1024

// refineTimeout bounds one correction round trip. Measured latencies range from
// about 3s for a short Chinese sentence to 12s+ for mixed Chinese/English with
// several repeated words, so this has to be generous; the client calls it in the
// background and never blocks the release gesture on it.
const refineTimeout = 25 * time.Second

// refineSystem tells the model to repair a streaming-ASR transcript in place
// and to output nothing but the repaired text.
const refineSystem = `你是语音识别后处理校对器。下面是一段流式语音识别的原始转写文本，请就地修复其中的识别错误，只输出修复后的文本本身。

必须修复：
- 重复的字或词（"昨天是是周一"→"昨天是周一"，"MONDAY MONDAY"→"MONDAY"，"的的"→"的"）
- 同音或近音字识别错（结合上下文判断，如"周未"→"周末"、"以经"→"已经"）
- 英文单词或字母被识别成中文同音字时，按语义还原为拉丁字母（如"选择走唉"→"选择走A"、"帮我查AMC"→"帮我查A M C"；识别为"公司/爱/哎/埃/艾/啊/哦"等疑似英文发音时优先还原）
- 中英混排的空格与大小写（英文单词之间补空格，专有名词用惯用写法）
- 标点缺失或多余（按语义补逗号句号，删除重复或无意义的标点）
- 语气词和口头禅的多余重复

严格禁止：
- 不改变语义，不增删信息，不翻译，不总结，不回答文本里提出的问题
- 不添加任何解释、前后缀、引号、项目符号或代码块标记
- 人名地名产品名等专有名词按原文保留，不确定时保留原文
- 数字、金额、日期原样保留
- 语义不通顺但无法从上下文判断出原文的，保留原文，不要臆造英文

文本本身没有问题就原样输出。只输出文本。`

// sttRefineResponse carries the corrected transcript back to the client.
// Changed is false when the model produced the same text or was not consulted;
// Text then equals the input and Reason says why nothing was corrected.
type sttRefineResponse struct {
	Text    string `json:"text"`
	Changed bool   `json:"changed"`
	Reason  string `json:"reason,omitempty"`
}

// handleSTTRefine repairs a raw streaming-ASR transcript using the LLM, so the
// client can hand the user a cleaned-up sentence instead of the engine's
// duplicated characters. It never fails the request: when the LLM is missing or
// rejects the output the original text comes back with Refined=false.
func (s *Server) handleSTTRefine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.requireToken(r); !ok {
		writeErr(w, http.StatusUnauthorized, "invalid token")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, refineMaxInputBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) > refineMaxInputBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "text too long")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	original := strings.TrimSpace(req.Text)
	if original == "" {
		writeErr(w, http.StatusBadRequest, "text is empty")
		return
	}

	unrefined := func(reason string) {
		writeJSON(w, http.StatusOK, sttRefineResponse{Text: original, Reason: reason})
	}

	if s.llm == nil || !s.llm.Enabled() {
		unrefined("llm not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), refineTimeout)
	defer cancel()
	out, err := s.llm.Complete(ctx, refineSystem, original)
	if err != nil {
		unrefined("llm failed")
		return
	}

	cleaned := stripFences(out)
	if !refinePlausible(original, cleaned) {
		unrefined("rejected")
		return
	}
	if cleaned == original {
		// 模型没发现问题，退而用本地规则去重，别让用户白等这一轮往返。
		cleaned = dedupTranscript(original)
	}
	changed := cleaned != original
	writeJSON(w, http.StatusOK, sttRefineResponse{Text: cleaned, Changed: changed})
}

// repeatedCN is a Chinese character the engine routinely duplicates: particles,
// function words and pronouns. Content words are deliberately excluded because
// Chinese legitimately reduplicates them (看看、说说、往往、一一), so for those a
// run of three or more is required before deduplicating.
const repeatedCN = "的了是在不我你他她它们也都就还再又会能要和与及把被给让使到从对为以这那很最太更起"

// dedupPunct keeps the first punctuation mark in a run and drops the rest.
func dedupPunct(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}

	out := make([]rune, 0, len(runes))
	lastWasPunct := false
	for _, r := range runes {
		if isPunctRune(r) {
			if lastWasPunct {
				continue
			}
			out = append(out, r)
			lastWasPunct = true
			continue
		}
		out = append(out, r)
		lastWasPunct = false
	}

	return string(out)
}

// dedupRuns collapses runs of identical Chinese characters: runs of three or
// more reduce to one, and pairs of routine particles also reduce to one, while
// legitimate two-character reduplications are preserved.
func dedupRuns(s string) string {
	runes := []rune(s)
	if len(runes) == 0 {
		return s
	}

	out := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		keep := 1
		if j-i == 2 && !strings.ContainsRune(repeatedCN, runes[i]) {
			keep = 2
		}
		for k := 0; k < keep; k++ {
			out = append(out, runes[i])
		}
		i = j
	}
	return string(out)
}

// dedupTranscript collapses repeated ASCII words, repeated punctuation, and
// repeated runs of Chinese characters.
func dedupTranscript(s string) string {
	return dedupRuns(dedupPunct(dedupWords(s)))
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.Index(s, "\n"); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return strings.TrimSpace(s)
}

// refinePlausible reports whether cleaned is a believable repair of original.
// It guards against a model that rewrites, summarises or answers the sentence
// instead of correcting it: the length must stay comparable and most of the
// repaired runes must already appear in the input.
func refinePlausible(original, cleaned string) bool {
	if strings.TrimSpace(cleaned) == "" {
		return false
	}
	o, c := []rune(original), []rune(cleaned)
	if len(o) == 0 || len(c) == 0 {
		return false
	}
	ratio := float64(len(c)) / float64(len(o))
	if ratio < 0.3 || ratio > 3.0 {
		return false
	}
	counts := make(map[rune]int, len(o))
	for _, r := range o {
		counts[r]++
	}
	shared := 0
	for _, r := range c {
		if counts[r] > 0 {
			counts[r]--
			shared++
		}
	}
	return float64(shared)/float64(len(c)) >= 0.6
}
