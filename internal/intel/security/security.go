// Package security detects sensitive fields and escalates ordinary issues
// into security warnings. The rules are deterministic: a field name or type
// matching a known sensitive pattern yields a security warning with a fixed
// severity, leaving semantic completion to the LLM layer.
package security

import "strings"

// Severity is the classification level of a security warning.
type Severity string

const (
	// SeverityCritical marks credentials or secrets that must never be exposed.
	SeverityCritical Severity = "critical"
	// SeverityHigh marks personal identity or payment data with a high leak risk.
	SeverityHigh Severity = "high"
	// SeverityMedium marks personal contact or monetary data that should be masked.
	SeverityMedium Severity = "medium"
	// SeverityLow marks contact data with a limited privacy impact.
	SeverityLow Severity = "low"
	// SeverityInfo marks findings without direct exposure risk.
	SeverityInfo Severity = "info"
)

// Field describes a single field of a response or entity.
type Field struct {
	Name string `json:"name"` // Field name, camelCase or snake_case both accepted.
	Type string `json:"type"` // Field type such as String/Long/BigDecimal; may be empty.
}

// Warning is a single security warning produced for a sensitive field.
type Warning struct {
	Field    string   `json:"field"`
	Kind     string   `json:"kind"` // password | secret | idcard | bankcard | mobile | amount | email
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// rule is a deterministic sensitive-field pattern. When types is non-empty the
// field type must match one of them, otherwise the rule matches any type.
type rule struct {
	kind     string
	severity Severity
	keywords []string
	types    []string
}

// rules lists every deterministic sensitive pattern in priority order. The
// first rule whose name (and type, when required) matches wins.
var rules = []rule{
	{
		kind:     "password",
		severity: SeverityCritical,
		keywords: []string{"password", "passwd", "pwd"},
	},
	{
		kind:     "secret",
		severity: SeverityCritical,
		keywords: []string{"secret", "token", "apiKey", "accessKey", "privateKey"},
	},
	{
		kind:     "idcard",
		severity: SeverityHigh,
		keywords: []string{"idCard", "idcard", "identityCard", "身份证"},
	},
	{
		kind:     "bankcard",
		severity: SeverityHigh,
		keywords: []string{"bankCard", "bankcard", "bankAccount", "银行卡"},
	},
	{
		kind:     "mobile",
		severity: SeverityMedium,
		keywords: []string{"mobile", "phone", "phoneNumber", "手机号"},
	},
	{
		kind:     "email",
		severity: SeverityLow,
		keywords: []string{"email", "mail"},
	},
	{
		kind:     "amount",
		severity: SeverityMedium,
		keywords: []string{"amount", "balance", "money", "price", "金额"},
		types:    []string{"BigDecimal", "Float", "Double"},
	},
}

// messages maps a warning kind to its human-readable explanation.
var messages = map[string]string{
	"password": "field appears to be a password and may be returned in plain text",
	"secret":   "field appears to hold a secret, token, or key and may leak credentials",
	"idcard":   "field appears to contain an identity card number",
	"bankcard": "field appears to contain a bank card or account number",
	"mobile":   "field appears to contain a mobile phone number and may not be masked",
	"amount":   "monetary field may expose precision, overflow, or unmasked values",
	"email":    "field appears to contain an email address",
}

// DetectField checks a single field against the sensitive patterns and returns
// the matching Warning, or nil when the field is not sensitive.
func DetectField(f Field) *Warning {
	for _, r := range rules {
		if !nameMatches(f.Name, r) {
			continue
		}
		if !typeMatches(f.Type, r.types) {
			continue
		}
		return &Warning{
			Field:    f.Name,
			Kind:     r.kind,
			Severity: r.severity,
			Message:  messages[r.kind],
		}
	}
	return nil
}

// DetectFields checks a group of fields in order and returns the security
// warnings for every sensitive field. The result may be empty.
func DetectFields(fields []Field) []Warning {
	out := make([]Warning, 0)
	for _, f := range fields {
		if w := DetectField(f); w != nil {
			out = append(out, *w)
		}
	}
	return out
}

// ClassifyIssue escalates an ordinary issue into a security warning when its
// summary or any of its associated field names hit a sensitive pattern. On a
// hit it returns kind "security_warning" and a severity at least as high as
// both the original severity and the matched pattern; otherwise it returns the
// original kind and severity unchanged.
func ClassifyIssue(kind, severity, summary string, fieldNames []string) (string, string) {
	matched := false
	best := Severity(severity)
	for _, name := range fieldNames {
		if r := matchName(name); r != nil {
			matched = true
			if severityRank(r.severity) > severityRank(best) {
				best = r.severity
			}
		}
	}
	if r := matchName(summary); r != nil {
		matched = true
		if severityRank(r.severity) > severityRank(best) {
			best = r.severity
		}
	}
	if !matched {
		return kind, severity
	}
	return "security_warning", string(best)
}

// matchName returns the first rule whose keywords appear in name, regardless
// of type. It backs both field-name matching and summary-text matching.
func matchName(name string) *rule {
	for i := range rules {
		if nameMatches(name, rules[i]) {
			return &rules[i]
		}
	}
	return nil
}

// nameMatches reports whether name contains any keyword of r after both are
// normalized for case-insensitive camelCase and snake_case comparison.
func nameMatches(name string, r rule) bool {
	n := normalize(name)
	for _, kw := range r.keywords {
		if strings.Contains(n, normalize(kw)) {
			return true
		}
	}
	return false
}

// typeMatches reports whether fieldType matches the rule's type list. An empty
// list means any type matches.
func typeMatches(fieldType string, types []string) bool {
	if len(types) == 0 {
		return true
	}
	t := normalize(fieldType)
	for _, want := range types {
		if strings.Contains(t, normalize(want)) {
			return true
		}
	}
	return false
}

// normalize lowercases a string and strips underscores so that idCard, id_card
// and IDCARD all compare equal for matching purposes.
func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "")
	return s
}

// severityRank orders severities so a higher value means more severe. Unknown
// severities rank below SeverityInfo.
func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	case SeverityInfo:
		return 0
	default:
		return -1
	}
}
