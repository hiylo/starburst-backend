package web

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Web endpoint contract scanning: deterministic, line-anchored extraction of
// HTTP calls from Vue/TS/JS frontends. Like the Java controller scanner it is
// deliberately regex/line based — dynamic paths (variables / template literals)
// are skipped, and every extracted row carries a verifiable source_file:line.
//
// Recognized call shapes:
//   - verb form: <prefix>.<verb>('path') with prefix in axios|http|api|request
//     and verb in get|post|put|delete|patch;
//   - object form: <prefix>({ method: 'get', url: 'path' }) where the config
//     object may span multiple lines (method defaults to GET when absent);
//   - fetch('path', { method: 'POST', ... }) with method defaulting to GET.
//
// Endpoints are deduplicated by (method, path), keeping the first occurrence.

var (
	// The literal must be the complete first argument (followed by ',' or ')'),
	// so concatenations like axios.get('/a/' + id) are skipped; the trailing
	// character is validated in code because RE2 has no lookahead.
	reCallVerb = regexp.MustCompile(`\b(?:axios|http|api|request)\.(get|post|put|delete|patch)\s*\(\s*((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'))`)
	reFetch    = regexp.MustCompile(`\bfetch\s*\(\s*((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'))`)
	// reMethodField matches a method: 'get' style key/value used both in fetch
	// options and in the axios object-call config.
	reMethodField = regexp.MustCompile(`method\s*:\s*['"](\w+)['"]`)
	reObjectStart = regexp.MustCompile(`\b(?:axios|http|api|request)\s*\(\s*\{`)
	reObjURL      = regexp.MustCompile(`url\s*:\s*((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'))`)
)

// ScanWeb scans dir (recursively) for .vue/.ts/.js files, extracts the HTTP API
// endpoints they call and returns them deduplicated by (method, path).
// SourceFile is the path relative to dir; SourceLine is the 1-based line number.
func ScanWeb(dir string) ([]*store.IntelEndpoint, error) {
	eps := make([]*store.IntelEndpoint, 0)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipWebDir(info.Name()) && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".vue", ".ts", ".js":
		default:
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		fileEps, rerr := scanWebFile(filepath.ToSlash(rel), path)
		if rerr != nil {
			return nil
		}
		eps = append(eps, fileEps...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dedupeWebEndpoints(eps), nil
}

// skipWebDir reports whether a directory name is excluded from the endpoint
// scan (VCS metadata, dependency and build output).
func skipWebDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build":
		return true
	}
	return false
}

// dedupeWebEndpoints collapses endpoints sharing a (method, path) key, keeping
// the first occurrence (the earliest file/line in walk order).
func dedupeWebEndpoints(eps []*store.IntelEndpoint) []*store.IntelEndpoint {
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

// scanWebFile scans a single source file line by line. The axios object-call
// config may span multiple lines, so a small brace-depth accumulator collects
// the block before parsing its method/url fields.
func scanWebFile(rel, path string) ([]*store.IntelEndpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := make([]*store.IntelEndpoint, 0)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var objBuf strings.Builder
	objDepth := 0
	objStartLine := 0
	line := 0
	for sc.Scan() {
		line++
		l := sc.Text()
		if objDepth > 0 {
			objBuf.WriteString(l)
			objBuf.WriteByte('\n')
			objDepth += braceDelta(l)
			if objDepth <= 0 {
				scanObjectCall(&objBuf, objStartLine, rel, &out)
				objBuf.Reset()
				objDepth = 0
			}
			continue
		}
		if reObjectStart.MatchString(l) {
			objStartLine = line
			objBuf.Reset()
			objBuf.WriteString(l)
			objBuf.WriteByte('\n')
			objDepth = braceDelta(l)
			if objDepth <= 0 {
				scanObjectCall(&objBuf, objStartLine, rel, &out)
				objBuf.Reset()
				objDepth = 0
			}
			continue
		}
		scanLineCalls(l, line, rel, &out)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// scanLineCalls extracts fetch(...) and <prefix>.<verb>('path') calls on one
// line. fetch method comes from the options object on the same line, defaulting
// to GET.
func scanLineCalls(l string, line int, rel string, out *[]*store.IntelEndpoint) {
	for _, loc := range reFetch.FindAllStringSubmatchIndex(l, -1) {
		if !literalEndsArg(l, loc) {
			continue
		}
		path := unquote(l[loc[2]:loc[3]])
		if path == "" {
			continue
		}
		method := "GET"
		if mm := reMethodField.FindStringSubmatch(l); mm != nil {
			method = strings.ToUpper(mm[1])
		}
		*out = append(*out, &store.IntelEndpoint{
			Method:     method,
			Path:       path,
			SourceFile: rel,
			SourceLine: line,
		})
	}
	for _, loc := range reCallVerb.FindAllStringSubmatchIndex(l, -1) {
		if !literalEndsArg(l, loc) {
			continue
		}
		path := unquote(l[loc[4]:loc[5]])
		if path == "" {
			continue
		}
		*out = append(*out, &store.IntelEndpoint{
			Method:     strings.ToUpper(l[loc[2]:loc[3]]),
			Path:       path,
			SourceFile: rel,
			SourceLine: line,
		})
	}
}

// literalEndsArg reports whether the string-literal capture of a match is
// immediately followed (after whitespace) by ',' or ')' — i.e. it stands alone
// as the argument rather than being part of a concatenation.
func literalEndsArg(l string, loc []int) bool {
	rest := strings.TrimLeft(l[loc[1]:], " \t")
	if rest == "" {
		return false
	}
	return rest[0] == ',' || rest[0] == ')'
}

// scanObjectCall parses an accumulated <prefix>({...}) config block and emits
// the endpoint with the block's start line as provenance. method defaults to
// GET when the config carries no method field.
func scanObjectCall(buf *strings.Builder, startLine int, rel string, out *[]*store.IntelEndpoint) {
	loc := reObjURL.FindStringSubmatchIndex(buf.String())
	if loc == nil {
		return
	}
	rest := strings.Trim(buf.String()[loc[1]:], " \t\r\n,});")
	// url must be a standalone value: anything meaningful after it (a
	// concatenation, template literal, etc.) voids the extraction.
	if rest != "" {
		return
	}
	path := unquote(buf.String()[loc[2]:loc[3]])
	if path == "" {
		return
	}
	method := "GET"
	if mm := reMethodField.FindStringSubmatch(buf.String()); mm != nil {
		method = strings.ToUpper(mm[1])
	}
	*out = append(*out, &store.IntelEndpoint{
		Method:     method,
		Path:       path,
		SourceFile: rel,
		SourceLine: startLine,
	})
}

// braceDelta returns the net brace balance of a line after stripping string
// literals, so config blocks with nested objects still accumulate correctly.
func braceDelta(l string) int {
	s := reStr.ReplaceAllString(l, " ")
	return strings.Count(s, "{") - strings.Count(s, "}")
}

// unquote strips the surrounding quotes of a JS string literal and resolves the
// common escape sequences so the recorded path matches the runtime value.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if s[len(s)-1] != q {
		return s
	}
	inner := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c == '\\' && i+1 < len(inner) {
			i++
			switch inner[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(inner[i])
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
