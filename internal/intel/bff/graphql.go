package bff

import (
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// GraphQL contract extraction is deterministic and regex/text based: balanced
// brace pairing for declaration bodies, top-level declaration regexes for
// object/enum types, and a token masker so comments/strings never confuse brace
// or field splitting. A full GraphQL parser is deliberately avoided.

var (
	// reGQLDecl matches a top-level GraphQL declaration (`type|input|interface|
	// enum Name {`) at the start of a line. Leading whitespace is limited to
	// spaces/tabs so the match anchor never consumes a preceding blank line's
	// newline (which would shift the provenance line by one). An optional
	// `implements` clause between the name and the opening brace is tolerated.
	reGQLDecl = regexp.MustCompile(`(?m)^[ \t]*(type|input|interface|enum)\s+` +
		`([A-Za-z_][A-Za-z0-9_]*)(?:\s+implements\s+[A-Za-z0-9_&\s,]*)?\s*\{`)
	// reGQLField matches one object field `name(args): ReturnType`. The return
	// capture is limited to type tokens ([]!), so trailing directives after
	// whitespace do not leak in.
	reGQLField = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(([^)]*)\))?\s*:\s*([A-Za-z0-9_\[\]!]+)`)
	// reGQLTagged matches a `gql`/`graphql` identifier that opens a tagged
	// template literal (the caller verifies a backtick immediately follows).
	reGQLTagged = regexp.MustCompile(`\b(?:gql|graphql)\s*`)
	// reGQLCall matches a `.graphql('...')` string-literal call.
	reGQLCall = regexp.MustCompile(`\.graphql\s*\(\s*` +
		`((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'))`)
)

// gqlBlock is one GraphQL schema source: raw text plus the 0-based line at
// which it starts in its original file (0 for a standalone schema file).
type gqlBlock struct {
	text string
	line int
}

// gqlField is one object field declaration with its index within the
// declaration body (for line anchoring).
type gqlField struct {
	name  string
	args  string
	typ   string
	index int
}

// gqlEnumValue is one enum value with its index within the declaration body.
type gqlEnumValue struct {
	value string
	index int
}

// gqlSegment is one top-level slice of a declaration body (split at depth-0
// commas and newlines) with its index within the body.
type gqlSegment struct {
	text  string
	index int
}

// graphQLBlocksInFile extracts every inline GraphQL schema block from a TS/JS
// file in source order: `gql`...` / `graphql`...` tagged template literals
// (multi-line, interpolation-aware) and `.graphql('...')` string literals.
func graphQLBlocksInFile(path string) []gqlBlock {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := string(data)
	out := make([]gqlBlock, 0)
	for _, m := range reGQLTagged.FindAllStringIndex(text, -1) {
		if m[1] >= len(text) || text[m[1]] != '`' {
			continue
		}
		end := scanTemplateEnd(text, m[1])
		if end <= m[1]+1 {
			continue
		}
		out = append(out, gqlBlock{
			text: text[m[1]+1 : end-1],
			line: strings.Count(text[:m[1]], "\n"),
		})
	}
	for _, m := range reGQLCall.FindAllStringSubmatchIndex(text, -1) {
		lit := text[m[2]:m[3]]
		out = append(out, gqlBlock{
			text: unquoteJS(lit),
			line: strings.Count(text[:m[0]], "\n"),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].line < out[j].line })
	return out
}

// scanTemplateEnd returns the index just past the closing backtick of the
// template literal opening at start, honoring backslash escapes and ${...}
// interpolation (a backtick inside an interpolation does not close the literal).
func scanTemplateEnd(text string, start int) int {
	interp := 0
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '`':
			if interp == 0 {
				return i + 1
			}
		case '$':
			if i+1 < len(text) && text[i+1] == '{' {
				interp++
				i++
			}
		case '}':
			if interp > 0 {
				interp--
			}
		}
	}
	return len(text)
}

// parseGraphQLBlock extracts entities and endpoints from one GraphQL schema
// block. base is the 0-based line where the block starts in its source file, so
// every returned row carries an accurate absolute source line.
func parseGraphQLBlock(rel, raw string, base int) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	masked := maskGQLTokens(raw)
	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, m := range reGQLDecl.FindAllStringSubmatchIndex(masked, -1) {
		kind := masked[m[2]:m[3]]
		name := masked[m[4]:m[5]]
		open := m[1] - 1 // the regex match ends right after '{'
		close := closingBraceAt(masked, open)
		if close < 0 {
			continue
		}
		body := masked[m[1]:close]
		declLine := base + 1 + strings.Count(masked[:m[0]], "\n")
		if isGQLOperationType(name) {
			for _, f := range gqlFields(body) {
				line := declLine + strings.Count(body[:f.index], "\n")
				eps = append(eps, &store.IntelEndpoint{
					Method:       gqlOperationMethod(name),
					Path:         f.name,
					ResponseType: f.typ,
					RequestJSON:  f.args,
					SourceFile:   rel,
					SourceLine:   line,
				})
			}
			continue
		}
		if kind == "enum" {
			for _, v := range gqlEnumValues(body) {
				line := declLine + strings.Count(body[:v.index], "\n")
				ents = append(ents, &store.IntelEntity{
					Entity:     name,
					TableName:  strings.ToLower(name),
					ColumnName: v.value,
					FieldType:  "enum",
					SourceFile: rel,
					SourceLine: line,
				})
			}
			continue
		}
		for _, f := range gqlFields(body) {
			line := declLine + strings.Count(body[:f.index], "\n")
			ents = append(ents, &store.IntelEntity{
				Entity:     name,
				TableName:  strings.ToLower(name),
				ColumnName: f.name,
				FieldType:  f.typ,
				SourceFile: rel,
				SourceLine: line,
			})
		}
	}
	return ents, eps
}

// gqlOperationMethod maps a GraphQL operation root type name to the endpoint
// method token ("" when the name is not an operation root).
func gqlOperationMethod(name string) string {
	switch name {
	case "Query":
		return "QUERY"
	case "Mutation":
		return "MUTATION"
	case "Subscription":
		return "SUBSCRIPTION"
	}
	return ""
}

// isGQLOperationType reports whether a type name is a GraphQL operation root
// (Query/Mutation/Subscription), which yields endpoints rather than entities.
func isGQLOperationType(name string) bool {
	return gqlOperationMethod(name) != ""
}

// gqlFields splits an object body into top-level segments and extracts every
// field declaration from them.
func gqlFields(body string) []gqlField {
	out := make([]gqlField, 0)
	for _, seg := range splitGQLBody(body) {
		for _, m := range reGQLField.FindAllStringSubmatchIndex(seg.text, -1) {
			args := ""
			if m[4] >= 0 {
				args = seg.text[m[4]:m[5]]
			}
			out = append(out, gqlField{
				name:  seg.text[m[2]:m[3]],
				args:  args,
				typ:   seg.text[m[6]:m[7]],
				index: seg.index + m[0],
			})
		}
	}
	return out
}

// gqlEnumValues splits an enum body into its values (bare identifiers separated
// by whitespace or commas), skipping @directives.
func gqlEnumValues(body string) []gqlEnumValue {
	out := make([]gqlEnumValue, 0)
	for _, seg := range splitGQLBody(body) {
		off := 0
		for _, f := range strings.Fields(seg.text) {
			if strings.HasPrefix(f, "@") {
				continue
			}
			idx := strings.Index(seg.text[off:], f)
			if idx < 0 {
				continue
			}
			out = append(out, gqlEnumValue{value: f, index: seg.index + off + idx})
			off += idx + len(f)
		}
	}
	return out
}

// splitGQLBody slices a declaration body at depth-0 commas and newlines,
// keeping the slice start indices for line anchoring.
func splitGQLBody(body string) []gqlSegment {
	var out []gqlSegment
	depth := 0
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',', '\n':
			if depth == 0 {
				out = append(out, gqlSegment{text: body[start:i], index: start})
				start = i + 1
			}
		}
	}
	if start < len(body) {
		out = append(out, gqlSegment{text: body[start:], index: start})
	}
	return out
}

// closingBraceAt returns the index of the brace matching the one at open.
func closingBraceAt(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// maskGQLTokens blanks out string literals (single and """ block strings) and
// # comments while preserving newlines, so brace balancing and field splitting
// ignore their content while line numbers stay anchored.
func maskGQLTokens(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr := false
	i := 0
	for i < len(s) {
		c := s[i]
		if inStr {
			if c == '\\' && i+1 < len(s) {
				b.WriteString("  ")
				i += 2
				continue
			}
			if c == '"' {
				b.WriteByte(' ')
				i++
				inStr = false
				continue
			}
			if c == '\n' {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
			i++
			continue
		}
		switch {
		case c == '"' && strings.HasPrefix(s[i:], `"""`):
			b.WriteString("   ")
			i += 3
			inStr = true
		case c == '"':
			b.WriteByte(' ')
			i++
			inStr = true
		case c == '#':
			for i < len(s) && s[i] != '\n' {
				b.WriteByte(' ')
				i++
			}
		case c == '\n':
			b.WriteByte('\n')
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
