package server

import "strings"

// llmTextMax caps any single LLM-produced text field so a runaway answer cannot
// bloat persisted rows. Longer values are truncated (deterministic).
const llmTextMax = 4000

// cleanLLMText trims whitespace and enforces the caps on an LLM-produced text.
func cleanLLMText(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

// validLLMBool tolerates the JSON boolean plus common string forms the model
// may emit ("true"/"false", "1"/"0", "yes"/"no") and defaults to false.
func validLLMBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1", "yes", "on":
			return true
		default:
			return false
		}
	case float64:
		return t != 0
	default:
		return false
	}
}

// capLLMList caps a slice to n entries deterministically (keeps the head).
func capLLMList[T any](items []T, n int) []T {
	if len(items) > n {
		return items[:n]
	}
	return items
}
