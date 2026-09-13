package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
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

// refineTimeout bounds one correction round trip. The model is a deep-reasoning
// engine that spends the first several thousand tokens thinking, so it needs
// both a generous token budget (defaultMaxTokens) and enough wall-clock time;
// 90s still never blocks the release gesture because the app runs refine in
// the background (its own client timeout is ~95s and both bounds share the
// same intent: let a long dictation get corrected instead of silently failing).
const refineTimeout = 90 * time.Second

// refineSystem tells the model to repair a streaming-ASR transcript in place,
// fill in missing punctuation, and output nothing but the repaired text.
const refineSystem = `你是语音识别后处理校对器。下面是一段流式语音识别的原始转写文本，请就地修复并补全标点，只输出修复后的文本本身。

必须修复：
- 标点必须补全：按语义在停顿处添加逗号、句号、问号、冒号、顿号等。只要朗读了多个短句或分句，就不允许整段没有任何标点。
- 重复的字或词（"昨天是是周一"→"昨天是周一"，"MONDAY MONDAY"→"MONDAY"，"的的"→"的"）
- 同音或近音字识别错：结合上下文判断哪个字/词才通顺，宁可多改一个可疑字也不要放过明显的错字。常见流式识别错例如："以经"→"已经"、"周未"→"周末"、"在再"混用、"的地得"混用、"那他"→"那他"上下文确认、"什么"→"神马"等网络音译、"那那那你"这类叠音。判断规则：如果换成一个同音字后句子语义立刻通顺，就改。
- 英文单词或字母被识别成中文同音字时，按语义还原为拉丁字母（如"选择走唉"→"选择走A"、"帮我查AMC"→"帮我查A M C"；识别为"公司/爱/哎/埃/艾/啊/哦"等疑似英文发音时优先还原）
- 中英混排的空格与大小写（英文单词之间补空格，专有名词用惯用写法）
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

	// fallbackDedup returns the transcript after local rule dedup when the LLM
	// is unavailable, fails or is not trusted. The engine is prone to trailing
	// repeats (the last word being decoded again on flush), so even without a
	// model we must still strip duplicates — otherwise the user sees raw text.
	fallbackDedup := func(reason string) {
		cleaned := dedupTranscript(original)
		writeJSON(w, http.StatusOK, sttRefineResponse{
			Text:    cleaned,
			Changed: cleaned != original,
			Reason:  reason,
		})
	}

	if s.llm == nil || !s.llm.Enabled() {
		fallbackDedup("llm not configured")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), refineTimeout)
	defer cancel()
	// The deep-reasoning model occasionally returns 5xx/connection issues on the
	// first try; a single retry drops the effective failure rate without
	// noticeably delaying the release gesture (the app runs this in the
	// background). Timeout budget is shared across both attempts via ctx.
	refineStart := time.Now()
	refineOutcome := "llm ok"
	var cleaned string
	if out, err := s.llm.Complete(ctx, refineSystem, original); err == nil {
		cleaned = stripFences(out)
	} else {
		select {
		case <-ctx.Done():
			refineOutcome = "llm failed (timeout)"
			fallbackDedup("llm failed")
			log.Printf("refine: %s after %.0fs", refineOutcome, time.Since(refineStart).Seconds())
			return
		case <-time.After(800 * time.Millisecond):
		}
		if out, err := s.llm.Complete(ctx, refineSystem, original); err != nil {
			refineOutcome = "llm failed"
			fallbackDedup("llm failed")
			log.Printf("refine: %s after %.0fs", refineOutcome, time.Since(refineStart).Seconds())
			return
		} else {
			cleaned = stripFences(out)
		}
	}

	if !refinePlausible(original, cleaned) {
		log.Printf("refine: rejected after %.0fs original=%q cleaned=%q", time.Since(refineStart).Seconds(), original, cleaned)
		fallbackDedup("rejected")
		return
	}
	if cleaned == original {
		// 模型没发现问题，退而用本地规则去重，别让用户白等这一轮往返。
		refineOutcome = "llm unchanged"
		cleaned = dedupTranscript(original)
	}
	log.Printf("refine: %s after %.0fs in=%d out=%d", refineOutcome, time.Since(refineStart).Seconds(), len([]rune(original)), len([]rune(cleaned)))
	changed := cleaned != original
	writeJSON(w, http.StatusOK, sttRefineResponse{Text: cleaned, Changed: changed})
}

// repeatedCN is a Chinese character the engine routinely duplicates: particles,
// function words and pronouns. Kept for documentation; the actual decision in
// dedupRuns is now the legalRedup allow-list below.
const repeatedCN = "的了是在不我你他她它们也都就还再又会能要和与及把被给让使到从对为以这那很最太更起"

// legalRedup lists Chinese characters that may legitimately appear as a pair
// (叠词): 看看、慢慢、渐渐、常常、刚刚、妈妈、哈哈 ... Everything else that
// comes out duplicated is assumed to be an engine stutter and collapsed to one,
// because streaming ASR routinely double-writes content words (杀杀敌、发发果、
// 失失朝朝), which far outnumbers real reduplication in dictation.
const legalRedup = "看看吧说说见面想想问问听听读写练试试找走走玩玩尝尝拍拍坐坐聊聊慢慢渐渐往往常常刚刚天天日日年年月月夜夜" +
	"时时刻刻每每隐隐微微轻轻紧紧偷偷悄悄默默匆匆忙忙家家户户人人条条件件种种样样星星点点滴滴谢谢见见太太娃娃宝宝一一" +
	"爸爸妈妈哥哥姐姐弟弟妹妹爷爷奶奶叔叔姑姑舅舅姥姥" +
	"哈哈呵呵嘿嘿嘻嘻哼哼汪汪喵喵呱呱"

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

// dedupRuns collapses runs of identical Chinese characters to one, except when
// the character legitimately reduplicates (看看、慢慢、妈妈) and appears exactly
// twice. Streaming ASR routinely double-writes ordinary content words (杀杀敌、
// 发发果、失失朝朝), so a pair is only kept when the character is allow-listed.
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
		if j-i == 2 && strings.ContainsRune(legalRedup, runes[i]) {
			keep = 2
		}
		for k := 0; k < keep; k++ {
			out = append(out, runes[i])
		}
		i = j
	}
	return string(out)
}

// dedupAdjacentRepeat removes the first copy of the longest block that appears
// twice in a row anywhere in the text, e.g. "AB AB C" -> "AB C" and
// "我们一起去吃饭我们一起去吃饭" -> "我们一起去吃饭". The engine re-decodes the
// accumulated transcript on flush, which duplicates a long prefix in the middle
// of the text (not just at the tail), and trimTailRepeat alone cannot reach
// that. A single optional space between the copies is ignored. Runs of a single
// character are owned by dedupRuns, so the minimum block length is two.
func dedupAdjacentRepeat(s string) string {
	trimmed := strings.TrimSpace(s)
	r := []rune(trimmed)
	n := len(r)
	if n < 2*2 {
		return trimmed
	}
	for {
		bestL, bestI := 0, 0
		for l := 2; l <= n/2; l++ {
			for i := 0; i+2*l <= n; i++ {
				if equalRunes(r[i:i+l], r[i+l:i+2*l]) {
					if l > bestL {
						bestL, bestI = l, i
					}
				} else if r[i+l] == ' ' && i+2*l+1 <= n &&
					equalRunes(r[i:i+l], r[i+l+1:i+2*l+1]) {
					if l > bestL {
						bestL, bestI = l, i
					}
				}
			}
		}
		if bestL == 0 {
			return trimmed
		}
		secondStart := bestI + bestL
		if secondStart < n && r[secondStart] == ' ' {
			secondStart++
		}
		out := make([]rune, 0, n-bestL)
		out = append(out, r[:bestI]...)
		out = append(out, r[secondStart:]...)
		trimmed = strings.TrimSpace(string(out))
		r = []rune(trimmed)
		n = len(r)
	}
}

// dedupTranscript collapses repeated ASCII words, repeated punctuation, and
// repeated runs of Chinese characters, drops an adjacent duplicate block
// anywhere in the text, then trims a duplicated trailing phrase that streaming
// ASR engines tend to emit on finish.
func dedupTranscript(s string) string {
	return trimTailRepeat(dedupAdjacentRepeat(dedupRuns(dedupPunct(dedupWords(s)))))
}

// trimTailRepeat removes a phrase duplicated verbatim at the end of the text,
// e.g. "今天天气怎么样 怎么样" -> "今天天气怎么样" and
// "我们一起去吃饭我们一起去吃饭" -> "我们一起去吃饭". Streaming engines often
// re-decode the last word/phrase once more when flushing the trailing silence,
// so the raw transcript ends with an exact copy of its tail.
func trimTailRepeat(s string) string {
	trimmed := strings.TrimSpace(s)
	if len([]rune(trimmed)) < 2 {
		return trimmed
	}

	// Space-separated tail: compare the last two tokens directly.
	words := strings.Fields(trimmed)
	if len(words) >= 2 && words[len(words)-1] == words[len(words)-2] {
		return strings.TrimSpace(strings.TrimSuffix(trimmed, words[len(words)-1]))
	}

	// Chinese tail without separators: try every phrase length of 2+ runes up
	// to half, preferring shorter phrases ("ABAB" -> "AB"). Single-rune tails
	// are excluded because dedupRuns already owns those (叠词 like 看看/一一
	// must survive, and it collapses real 3+ repeats).
	r := []rune(trimmed)
	n := len(r)
	for k := 2; k <= n/2; k++ {
		if equalRunes(r[n-k:], r[n-2*k:n-k]) {
			return strings.TrimSpace(string(r[:n-k]))
		}
	}
	return trimmed
}

// equalRunes reports whether two rune slices are identical.
func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
