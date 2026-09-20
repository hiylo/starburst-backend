package ios

import (
	"bufio"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Swift contract scanning for iOS modules: deterministic, line-anchored
// extraction of Codable entities (struct Xxx: Codable/Decodable property lines)
// and URLSession endpoint contracts (URL(string: "...") literals paired with a
// nearby request.httpMethod assignment inside the same function scope). Mirrors
// the Android scanner's "every row carries source_file:line provenance"
// guarantee; multi-line URL construction and inherited models are left to the
// LLM assist layer later.

var (
	// reSwiftStruct matches a `struct Xxx: Conformances` declaration; the second
	// capture is the conformance list up to the opening brace.
	reSwiftStruct = regexp.MustCompile(`\bstruct\s+([A-Za-z_][A-Za-z0-9_]*)\s*:\s*([A-Za-z0-9_,\s]*)`)
	// reCodable matches a Codable/Decodable conformance token (a Swift struct
	// must conform to one of them to be JSON-decoded by URLSession).
	reCodable = regexp.MustCompile(`\b(?:Codable|Decodable)\b`)
	// reSwiftProperty matches a `let/var name: Type[?]` property line. The type
	// capture runs up to , = { } or newline so multi-declaration and computed
	// property lines split cleanly; a trailing `?` marks nullability.
	reSwiftProperty = regexp.MustCompile(`\b(let|var)\s+([A-Za-z_][A-Za-z0-9_]*)\s*:\s*([^,=\n{}]+)`)
	// reSwiftURL matches a URL(string: "https://...") literal.
	reSwiftURL = regexp.MustCompile(`URL\(\s*string\s*:\s*"([^"]+)"`)
	// reHTTPMethod matches a request.httpMethod = "POST" assignment.
	reHTTPMethod = regexp.MustCompile(`httpMethod\s*=\s*"([A-Za-z]+)"`)
	// reSwiftFunc matches a func declaration (opens an endpoint scope).
	reSwiftFunc = regexp.MustCompile(`\bfunc\s+[A-Za-z0-9_]+`)
)

// swEntityCtx is the entity currently being scanned: its owning struct name and
// the brace depth at which the struct was declared.
type swEntityCtx struct {
	name   string
	entity bool
	base   int
	line   int
	opened bool // true once the struct body contour rose above base (has a {})
}

// swField is one property extracted from an entity struct.
type swField struct {
	name     string
	typ      string
	nullable bool
}

// swURLRef is one URL(string: "...") literal reference with its line.
type swURLRef struct {
	raw  string
	line int
}

// swMethodRef is one request.httpMethod assignment with its line.
type swMethodRef struct {
	name string
	line int
}

// swScope is one function scope under scan: it collects the URL literals and
// httpMethod assignments so each URL can be paired with the nearest method.
type swScope struct {
	base    int
	urls    []swURLRef
	methods []swMethodRef
}

// ScanSwift scans dir for .swift files and returns the extracted Codable
// entity mappings and URLSession endpoint contracts with provenance. .git,
// build, DerivedData, node_modules, .build and Pods directories are skipped.
// Endpoints are deduplicated by (Method, Path); entity/endpoint SourceFile
// paths are relative to dir.
func ScanSwift(dir string) ([]*store.IntelEntity, []*store.IntelEndpoint, error) {
	files := make([]string, 0)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipSwiftDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(info.Name(), ".swift") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, f := range files {
		e, p := scanSwiftFile(dir, f)
		ents = append(ents, e...)
		eps = append(eps, p...)
	}
	return ents, dedupeSwiftEndpoints(eps), nil
}

// skipSwiftDir reports whether a directory name is generated/vendored output
// that must not be scanned for Swift sources.
func skipSwiftDir(name string) bool {
	switch name {
	case ".git", "build", "DerivedData", "node_modules", ".build", "Pods":
		return true
	}
	return false
}

// scanSwiftFile extracts entities and endpoints from a single .swift file.
// Source paths are rendered relative to dir (slash-normalized).
func scanSwiftFile(dir, file string) ([]*store.IntelEntity, []*store.IntelEndpoint) {
	lines, err := readSwiftLines(file)
	if err != nil {
		return nil, nil
	}
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		rel = file
	}
	rel = filepath.ToSlash(rel)

	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)

	cur := 0 // brace depth at the start of the current line
	var ctx *swEntityCtx
	// Pending annotation state carried to the next property declaration.
	pendingPrimary := false
	// Function scopes for httpMethod pairing, innermost last.
	scopes := make([]*swScope, 0)
	// URL literals found outside any function scope (default GET).
	var topLevelURLs []swURLRef

	for i, l := range lines {
		// A func declaration opens a scope whose base is the depth at the start
		// of the line (its own opening brace will lift the depth above base).
		if reSwiftFunc.MatchString(l) {
			scopes = append(scopes, &swScope{base: cur})
		}

		// Endpoint URL literals and httpMethod assignments land in the innermost
		// active function scope (or stay top-level for default-GET endpoints).
		if us := swURLsIn(l, i); len(us) > 0 {
			if s := topScope(scopes); s != nil {
				s.urls = append(s.urls, us...)
			} else {
				topLevelURLs = append(topLevelURLs, us...)
			}
		}
		if m := reHTTPMethod.FindStringSubmatch(l); m != nil {
			if s := topScope(scopes); s != nil {
				s.methods = append(s.methods, swMethodRef{name: m[1], line: i})
			}
		}

		// Entity declaration: struct Xxx: Codable/Decodable [...].
		if sm := reSwiftStruct.FindStringSubmatch(l); sm != nil {
			if reCodable.MatchString(sm[2]) {
				ctx = &swEntityCtx{name: sm[1], entity: true, base: cur, line: i}
				pendingPrimary = false
				for _, f := range swFieldsIn(l) {
					ents = append(ents, swiftEntity(ctx, f, rel, i+1, isSwiftPrimary(f.name, false)))
				}
			} else {
				ctx = &swEntityCtx{name: sm[1], entity: false, base: cur, line: i}
			}
		}

		// Property-level @Attribute(.primaryKey) sitting on a line before the field.
		if strings.Contains(l, "@Attribute(.primaryKey)") && !reSwiftProperty.MatchString(l) {
			pendingPrimary = true
		}
		linePrimary := strings.Contains(l, "@Attribute(.primaryKey)")

		// Fields of the current entity (declared after the struct header).
		if ctx != nil && ctx.entity && i > ctx.line && cur > ctx.base {
			fields := swFieldsIn(l)
			for _, f := range fields {
				ents = append(ents, swiftEntity(ctx, f, rel, i+1, isSwiftPrimary(f.name, linePrimary || pendingPrimary)))
			}
			if len(fields) > 0 {
				pendingPrimary = false
			}
		}

		cur += braceDelta(l)

		// Close contexts once a brace-owning struct returns to base depth.
		if ctx != nil && ctx.opened && i > ctx.line && cur <= ctx.base {
			ctx = nil
		}
		if ctx != nil && cur > ctx.base {
			ctx.opened = true
		}
		// Pop function scopes whose body closed, emitting their endpoints.
		for len(scopes) > 0 && cur <= scopes[len(scopes)-1].base {
			s := scopes[len(scopes)-1]
			scopes = scopes[:len(scopes)-1]
			eps = append(eps, swiftScopeEndpoints(s, rel)...)
		}
	}

	// Flush any scope still open at EOF, then top-level URL literals.
	for len(scopes) > 0 {
		s := scopes[len(scopes)-1]
		scopes = scopes[:len(scopes)-1]
		eps = append(eps, swiftScopeEndpoints(s, rel)...)
	}
	for _, u := range topLevelURLs {
		if ep := swiftEndpoint(u, "GET", rel); ep != nil {
			eps = append(eps, ep)
		}
	}
	return ents, eps
}

// readSwiftLines reads a file into its lines (buffered like the Kotlin scanner).
func readSwiftLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	return out, sc.Err()
}

// swiftEntity renders one IntelEntity from an entity context and a field. Swift
// has no table concept, so TableName stays empty.
func swiftEntity(ctx *swEntityCtx, f swField, rel string, line int, primary bool) *store.IntelEntity {
	return &store.IntelEntity{
		Entity:     ctx.name,
		TableName:  "",
		ColumnName: f.name,
		FieldType:  f.typ,
		Nullable:   f.nullable,
		IsPrimary:  primary,
		SourceFile: rel,
		SourceLine: line,
	}
}

// swFieldsIn extracts every let/var property declaration on a line.
func swFieldsIn(l string) []swField {
	out := make([]swField, 0)
	for _, m := range reSwiftProperty.FindAllStringSubmatch(l, -1) {
		raw := strings.TrimSpace(m[3])
		// Drop a trailing // comment from the type capture.
		if idx := strings.Index(raw, "//"); idx >= 0 {
			raw = strings.TrimSpace(raw[:idx])
		}
		if raw == "" {
			continue
		}
		nullable := strings.HasSuffix(raw, "?")
		typ := strings.TrimSpace(strings.TrimSuffix(raw, "?"))
		if typ == "" {
			continue
		}
		out = append(out, swField{name: m[2], typ: typ, nullable: nullable})
	}
	return out
}

// isSwiftPrimary reports whether a property is the primary key: an id field or
// a property annotated @Attribute(.primaryKey).
func isSwiftPrimary(name string, annotated bool) bool {
	return name == "id" || annotated
}

// swURLsIn extracts every URL(string: "...") literal on a line.
func swURLsIn(l string, line int) []swURLRef {
	ms := reSwiftURL.FindAllStringSubmatch(l, -1)
	out := make([]swURLRef, 0, len(ms))
	for _, m := range ms {
		out = append(out, swURLRef{raw: m[1], line: line})
	}
	return out
}

// topScope returns the innermost active function scope, or nil at top level.
func topScope(scopes []*swScope) *swScope {
	if len(scopes) == 0 {
		return nil
	}
	return scopes[len(scopes)-1]
}

// swiftScopeEndpoints resolves the URLs of a function scope, pairing each with
// the nearest httpMethod assignment (default GET), and renders endpoints.
func swiftScopeEndpoints(s *swScope, rel string) []*store.IntelEndpoint {
	out := make([]*store.IntelEndpoint, 0, len(s.urls))
	for _, u := range s.urls {
		method := "GET"
		best := -1
		for _, m := range s.methods {
			d := m.line - u.line
			if d < 0 {
				d = -d
			}
			if best < 0 || d < best {
				best = d
				method = m.name
			}
		}
		if ep := swiftEndpoint(u, method, rel); ep != nil {
			out = append(out, ep)
		}
	}
	return out
}

// swiftEndpoint renders one IntelEndpoint from a URL literal. Only http/https
// URLs yield an endpoint; Path keeps the parsed path (without scheme/host).
func swiftEndpoint(u swURLRef, method, rel string) *store.IntelEndpoint {
	parsed, err := url.Parse(u.raw)
	if err != nil {
		return nil
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil
	}
	path := parsed.Path
	if path == "" {
		path = "/"
	}
	return &store.IntelEndpoint{
		Method:     method,
		Path:       path,
		SourceFile: rel,
		SourceLine: u.line + 1,
	}
}

// braceDelta counts the net brace-depth change a line introduces (strings and
// comments are not lexed; close enough for line-anchored scanning).
func braceDelta(l string) int {
	d := 0
	for _, r := range l {
		switch r {
		case '{':
			d++
		case '}':
			d--
		}
	}
	return d
}

// dedupeSwiftEndpoints collapses endpoints sharing a (Method, Path) key,
// keeping the first occurrence.
func dedupeSwiftEndpoints(eps []*store.IntelEndpoint) []*store.IntelEndpoint {
	seen := make(map[string]bool)
	out := make([]*store.IntelEndpoint, 0, len(eps))
	for _, ep := range eps {
		key := ep.Method + " " + ep.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ep)
	}
	return out
}
