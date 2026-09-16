// Package rootcause implements the deterministic failure-attribution heuristic of
// the Test Intelligence subsystem. It classifies a single test failure (exception
// text, assertion message or command output) into one of the categories
// backend / database / client / env / contract by keyword and pattern matching.
// The LLM only performs semantic completion on top of this result; structural
// attribution never depends on it.
package rootcause

import (
	"regexp"
	"strings"
)

// Category is a failure-attribution category.
type Category string

const (
	// CategoryBackend marks a backend logic failure (null pointer, illegal argument, out-of-bounds).
	CategoryBackend Category = "backend"
	// CategoryDatabase marks a database failure (connection, SQL or constraint).
	CategoryDatabase Category = "database"
	// CategoryClient marks a client binding / UI failure.
	CategoryClient Category = "client"
	// CategoryEnv marks a missing environment or configuration failure.
	CategoryEnv Category = "env"
	// CategoryContract marks a contract mismatch (missing field or type mismatch).
	CategoryContract Category = "contract"
	// CategoryUnknown marks a failure that no heuristic could attribute.
	CategoryUnknown Category = "unknown"
)

// Hypothesis is a single attribution hypothesis.
type Hypothesis struct {
	Category   Category `json:"category"`
	Confidence string   `json:"confidence"` // high | medium | low
	Reason     string   `json:"reason"`     // the matched keyword or pattern
}

// direct keywords produce high confidence, patterns and conjunctions medium,
// and the fallback low.
const (
	confidenceHigh   = "high"
	confidenceMedium = "medium"
	confidenceLow    = "low"
)

// reDBRefused matches a connection refusal to a database port.
var reDBRefused = regexp.MustCompile(`(?i)connection refused.*(mysql|postgres|redis)`)

// reExpectedButWas matches an assertion that states expected and actual values.
var reExpectedButWas = regexp.MustCompile(`(?i)expected.*but was`)

// reAssertFailed matches a failed assertion message.
var reAssertFailed = regexp.MustCompile(`(?i)assert.*failed`)

// reExpectedLess matches an assertion that compares an expected value with "<".
var reExpectedLess = regexp.MustCompile(`(?i)expected.*<`)

// reNullWord matches the standalone literal null (JSON null), never
// "NullPointerException", because \b does not split inside a word.
var reNullWord = regexp.MustCompile(`(?i)\bnull\b`)

// contains reports whether text contains substr, case-insensitively.
func contains(text, substr string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(substr))
}

// containsAny returns the first keyword in order that text contains,
// case-insensitively, or the empty string when none matches.
func containsAny(text string, keywords ...string) string {
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			return kw
		}
	}
	return ""
}

// analyzeDatabase attributes database failures. Direct keywords yield high
// confidence; the database-port refusal pattern yields medium.
func analyzeDatabase(failure string) (Hypothesis, bool) {
	if kw := containsAny(failure, "SQLException", "SQLSyntaxError", "DuplicateKey",
		"constraint", "deadlock", "Communications link failure",
		"CannotGetJdbcConnection", "connect timed out"); kw != "" {
		return Hypothesis{Category: CategoryDatabase, Confidence: confidenceHigh, Reason: kw}, true
	}
	if reDBRefused.MatchString(failure) {
		return Hypothesis{Category: CategoryDatabase, Confidence: confidenceMedium,
			Reason: "connection refused to a database port"}, true
	}
	return Hypothesis{}, false
}

// analyzeEnv attributes environment failures: missing files, missing commands,
// unsupported platforms and non-database connection refusals.
func analyzeEnv(failure string) (Hypothesis, bool) {
	if kw := containsAny(failure, "No such file", "command not found", "Docker",
		"port already in use", "unsupported platform", "SDK", "toolchain"); kw != "" {
		return Hypothesis{Category: CategoryEnv, Confidence: confidenceHigh, Reason: kw}, true
	}
	if contains(failure, "connection refused") {
		return Hypothesis{Category: CategoryEnv, Confidence: confidenceHigh,
			Reason: "connection refused"}, true
	}
	return Hypothesis{}, false
}

// analyzeContract attributes contract mismatches. It matches direct keywords,
// the expected/actual pattern and contract hints whose field name shows up in
// the failure together with a null or missing marker.
func analyzeContract(failure string, hints []string) (Hypothesis, bool) {
	if kw := containsAny(failure, "missing field", "type mismatch", "ClassCastException",
		"Deserialization", "JSON parse"); kw != "" {
		return Hypothesis{Category: CategoryContract, Confidence: confidenceHigh, Reason: kw}, true
	}
	if reExpectedButWas.MatchString(failure) {
		return Hypothesis{Category: CategoryContract, Confidence: confidenceMedium,
			Reason: "expected...but was mismatch"}, true
	}
	if reNullWord.MatchString(failure) {
		return Hypothesis{Category: CategoryContract, Confidence: confidenceHigh, Reason: "null"}, true
	}
	for _, h := range hints {
		if h == "" || !contains(failure, h) {
			continue
		}
		if contains(failure, "null") || contains(failure, "missing") {
			return Hypothesis{Category: CategoryContract, Confidence: confidenceMedium,
				Reason: "contract field " + h + " null or missing"}, true
		}
	}
	return Hypothesis{}, false
}

// hasUIHint reports whether any contract hint names a UI field (android,
// layout or a "ui"-prefixed field).
func hasUIHint(hints []string) bool {
	for _, h := range hints {
		lh := strings.ToLower(h)
		if strings.Contains(lh, "android") || strings.Contains(lh, "layout") ||
			strings.HasPrefix(lh, "ui") {
			return true
		}
	}
	return false
}

// analyzeClient attributes client binding failures. It is a conjunction: the
// failure must carry a client marker and the contract hints must reference UI.
func analyzeClient(failure string, hints []string) (Hypothesis, bool) {
	marker := containsAny(failure, "CLIENT_MISSING_FIELD", "binding", "NullPointerException")
	if marker == "" || !hasUIHint(hints) {
		return Hypothesis{}, false
	}
	return Hypothesis{Category: CategoryClient, Confidence: confidenceMedium,
		Reason: marker + " with UI contract hint"}, true
}

// analyzeBackend attributes backend logic failures: exceptions and assertion
// failures. Direct exception names yield high confidence; assertion patterns
// yield medium.
func analyzeBackend(failure string) (Hypothesis, bool) {
	if kw := containsAny(failure, "NullPointerException", "IllegalArgumentException",
		"ArrayIndexOutOfBounds"); kw != "" {
		return Hypothesis{Category: CategoryBackend, Confidence: confidenceHigh, Reason: kw}, true
	}
	if reAssertFailed.MatchString(failure) {
		return Hypothesis{Category: CategoryBackend, Confidence: confidenceMedium,
			Reason: "assertion failed"}, true
	}
	if reExpectedLess.MatchString(failure) {
		return Hypothesis{Category: CategoryBackend, Confidence: confidenceMedium,
			Reason: "expected value less-than assertion"}, true
	}
	return Hypothesis{}, false
}

// Analyze attributes a single failure to a category using the contract hints.
// The categories are checked in priority order (database, env, contract,
// client, backend); the first match wins. An unattributable failure returns
// CategoryUnknown with low confidence.
func Analyze(failure string, contractHints []string) Hypothesis {
	if h, ok := analyzeDatabase(failure); ok {
		return h
	}
	if h, ok := analyzeEnv(failure); ok {
		return h
	}
	if h, ok := analyzeContract(failure, contractHints); ok {
		return h
	}
	if h, ok := analyzeClient(failure, contractHints); ok {
		return h
	}
	if h, ok := analyzeBackend(failure); ok {
		return h
	}
	return Hypothesis{Category: CategoryUnknown, Confidence: confidenceLow,
		Reason: "no matching heuristic"}
}

// AnalyzeMany attributes a batch of failures against the shared contract hints
// and returns one hypothesis per failure, in input order.
func AnalyzeMany(failures []string, hints []string) []Hypothesis {
	out := make([]Hypothesis, len(failures))
	for i, f := range failures {
		out[i] = Analyze(f, hints)
	}
	return out
}
