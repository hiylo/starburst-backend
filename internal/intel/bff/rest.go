package bff

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// REST route extraction is deterministic and regex/text based, mirroring the
// Java/Android/Web scanners' "every row carries source_file:line provenance"
// guarantee. Express/Router/Fastify verb calls and Nest @Controller/@Get-style
// decorators are recognized; dynamic (template-literal/concatenated) paths are
// skipped, exactly like the Web scanner.

var (
	// reExpressRoute matches Express/Router/Fastify route registrations:
	// (app|router|server|fastify).<verb>('path') where verb is a literal. Paths
	// must be string-literal first arguments, so concatenations are skipped.
	reExpressRoute = regexp.MustCompile(`\b(?:app|router|server|fastify)\.` +
		`(get|post|put|delete|patch|all)\s*\(\s*` +
		`((?:"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'))`)
	// reNestController matches a @Controller('base') decorator.
	reNestController = regexp.MustCompile(`@Controller\s*\(\s*['"]([^'"]*)['"]`)
	// reNestVerb matches any Nest HTTP-method decorator (@Get/@Post/@Put/
	// @Delete/@Patch). The \b guards against @GetMapping-style lookalikes.
	reNestVerb = regexp.MustCompile(`@(Get|Post|Put|Delete|Patch)\b`)
	// reNestVerbPath matches the quoted path argument of a Nest verb decorator.
	reNestVerbPath = regexp.MustCompile(`@(Get|Post|Put|Delete|Patch)\s*\(\s*['"]([^'"]*)['"]`)
	// nestKeywords are identifiers never interpreted as a Nest handler method.
	nestKeywords = map[string]bool{
		"constructor": true, "return": true, "if": true, "else": true,
		"while": true, "for": true, "switch": true, "catch": true,
		"function": true, "new": true, "typeof": true, "delete": true,
		"import": true,
	}
)

// nestCtl is the active Nest controller being scanned: its base path and the
// brace depth at which @Controller was seen.
type nestCtl struct {
	base   string
	depth  int
	line   int
	opened bool // true once the class body contour rose above base (has a {})
}

// scanRESTFile extracts Express/Router/Fastify and Nest route endpoints from a
// single TS/JS file, line-anchored to the source file (paths relative to the
// scanned module).
func scanRESTFile(rel, path string) []*store.IntelEndpoint {
	lines, err := readBFFLines(path)
	if err != nil {
		return nil
	}
	eps := make([]*store.IntelEndpoint, 0)
	cur := 0
	var ctl *nestCtl
	pendingVerb, pendingSub := "", ""
	pendingVerbLine := -1
	for i, l := range lines {
		// Express / Router / Fastify verb form.
		for _, m := range reExpressRoute.FindAllStringSubmatch(l, -1) {
			p := unquoteJS(m[2])
			if p == "" {
				continue
			}
			eps = append(eps, &store.IntelEndpoint{
				Method:     strings.ToUpper(m[1]),
				Path:       p,
				SourceFile: rel,
				SourceLine: i + 1,
			})
		}

		// Nest @Controller('base') opens a new controller scope.
		if m := reNestController.FindStringSubmatch(l); m != nil {
			ctl = &nestCtl{base: m[1], depth: cur, line: i}
			pendingVerb, pendingSub, pendingVerbLine = "", "", -1
		}

		// Nest verb decorators collect the pending handler route.
		if ctl != nil {
			if vm := reNestVerbPath.FindStringSubmatch(l); vm != nil {
				pendingVerb = strings.ToUpper(vm[1])
				pendingSub = vm[2]
				pendingVerbLine = i
			} else if vm := reNestVerb.FindStringSubmatch(l); vm != nil {
				pendingVerb = strings.ToUpper(vm[1])
				pendingSub = ""
				pendingVerbLine = i
			}
		}

		cur += braceDelta(l)
		if ctl != nil && cur > ctl.depth {
			ctl.opened = true
		}

		// A method declaration consumes the pending verb; private handlers are
		// skipped (their routes are not public contracts).
		if ctl != nil && ctl.opened && pendingVerb != "" && i >= pendingVerbLine {
			if name := nestMethodName(l); name != "" {
				if !strings.Contains(l, "private") && !strings.Contains(lines[pendingVerbLine], "private") {
					eps = append(eps, &store.IntelEndpoint{
						Method:     pendingVerb,
						Path:       joinNestPath(ctl.base, pendingSub),
						SourceFile: rel,
						SourceLine: pendingVerbLine + 1,
					})
				}
				pendingVerb, pendingSub, pendingVerbLine = "", "", -1
			}
		}

		// Close the controller scope once its braces collapse back to base.
		if ctl != nil && ctl.opened && i > ctl.line && cur <= ctl.depth {
			ctl = nil
			pendingVerb, pendingSub, pendingVerbLine = "", "", -1
		}
	}
	return eps
}

// readBFFLines reads a file into its lines (buffered like the Kotlin scanner).
func readBFFLines(path string) ([]string, error) {
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

// nestMethodName returns the handler method name when l declares a class method
// (a candidate handler for the pending Nest verb decorator), or "" when the
// line is not a method declaration. Leading decorators are stripped; keywords,
// constructors, arrow functions and variable declarations are rejected.
func nestMethodName(l string) string {
	s := strings.TrimSpace(l)
	if s == "" || strings.HasPrefix(s, "//") {
		return ""
	}
	// Strip leading decorators (@X and @X(...)) repeatedly.
	for strings.HasPrefix(s, "@") {
		open := strings.Index(s, "(")
		if open < 0 {
			rest := s[1:]
			sp := strings.IndexAny(rest, " \t")
			if sp < 0 {
				return ""
			}
			s = strings.TrimSpace(rest[sp:])
			continue
		}
		depth := 0
		i := open
		for ; i < len(s); i++ {
			if s[i] == '(' {
				depth++
			} else if s[i] == ')' {
				depth--
				if depth == 0 {
					i++
					break
				}
			}
		}
		if depth != 0 {
			return ""
		}
		s = strings.TrimSpace(s[i:])
		if s == "" {
			return ""
		}
	}
	open := strings.Index(s, "(")
	if open <= 0 {
		return ""
	}
	head := strings.Fields(strings.TrimSpace(s[:open]))
	name := head[len(head)-1]
	if nestKeywords[name] {
		return ""
	}
	// Balance the parameter list, then the tail must open a method body ({)
	// directly or after a return-type annotation.
	depth := 0
	close := -1
	for j := open; j < len(s); j++ {
		if s[j] == '(' {
			depth++
		} else if s[j] == ')' {
			depth--
			if depth == 0 {
				close = j
				break
			}
		}
	}
	if close < 0 {
		return ""
	}
	tail := strings.TrimSpace(s[close+1:])
	if tail == "" {
		return ""
	}
	if tail[0] == '{' {
		return name
	}
	if tail[0] == ':' && strings.Index(tail, "{") >= 0 {
		return name
	}
	return ""
}

// joinNestPath concatenates a controller base path and a method sub-path with
// '/' alignment and a single leading slash (matching the Java joinPath rules).
func joinNestPath(base, sub string) string {
	p := strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
	p = strings.Trim(p, "/")
	if p == "" {
		return "/"
	}
	return "/" + p
}

// braceDelta counts the net brace-depth change a line introduces, skipping
// string literals, template literals and // comments so nested {}/${...} do not
// confuse Nest controller scoping.
func braceDelta(l string) int {
	d := 0
	for i := 0; i < len(l); i++ {
		switch l[i] {
		case '/':
			if i+1 < len(l) && l[i+1] == '/' {
				return d
			}
		case '"', '\'':
			q := l[i]
			for i++; i < len(l); i++ {
				if l[i] == '\\' {
					i++
					continue
				}
				if l[i] == q {
					break
				}
			}
		case '`':
			for i++; i < len(l); i++ {
				if l[i] == '\\' {
					i++
					continue
				}
				if l[i] == '`' {
					break
				}
			}
		case '{':
			d++
		case '}':
			d--
		}
	}
	return d
}

// unquoteJS strips the surrounding quotes of a JS string literal and resolves
// the common escape sequences so the recorded path matches the runtime value.
func unquoteJS(s string) string {
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
