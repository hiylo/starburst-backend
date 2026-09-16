// Package compliance implements deterministic source-code compliance rule
// scanning. It checks source files against configurable rules derived from the
// project hard rules (copyright headers, Javadoc, forbidden annotations,
// formatting and secret hygiene) and yields findings carrying file:line
// provenance for the intel_findings persistence layer (detector=rule).
package compliance

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Finding is a single compliance rule hit.
type Finding struct {
	RuleID   string `json:"ruleId"`
	Severity string `json:"severity"` // critical | high | medium | low | info
	File     string `json:"file"`
	Line     int    `json:"line"`
	Message  string `json:"message"`
}

// Rule describes one toggleable compliance rule.
type Rule struct {
	ID       string
	Severity string
	Enabled  bool
	Check    func(path string, lines []string) []Finding
}

// maxFileLines is the single-file line limit enforced by rule file-too-long.
const maxFileLines = 1500

// sourceExts lists the source extensions scanned by ScanDir.
var sourceExts = map[string]bool{
	".go": true, ".java": true, ".kt": true, ".js": true, ".ts": true, ".vue": true, ".py": true,
}

// skipDirs lists directory names excluded from ScanDir recursion.
var skipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true, ".idea": true, ".vscode": true,
	"node_modules": true, "target": true, "build": true, "dist": true, ".gradle": true,
	"vendor": true, "coverage": true,
}

var (
	rePublicClass  = regexp.MustCompile(`\bpublic\s+(?:abstract\s+|final\s+|strictfp\s+)*class\s+[A-Za-z0-9_]+`)
	rePublicMethod = regexp.MustCompile(`^\s*public\s+(?:(?:static|final|synchronized|native|abstract)\s+)*[A-Za-z0-9_<>\[\],.?$]+\s+[A-Za-z0-9_]+\s*\(`)
	reSwagger      = regexp.MustCompile(`@(?:ApiOperation|ApiImplicitParams?|ApiModelProperty|ApiResponses?|ApiParam|ApiIgnore|ApiModel|Api|Operation|Tag|Schema|Parameter|Hidden)\b`)
	reSecret       = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret[_-]?key|secret|api[_-]?key|token|access[_-]?token|auth[_-]?token)\s*[:=]`)
)

// pemMarkers are private-key block headers (matched case-insensitively) that
// signal a hard-coded key.
var pemMarkers = []string{
	"begin rsa private key",
	"begin private key",
	"begin openssh private key",
	"begin ec private key",
}

// Rules returns the built-in, toggleable rule set.
func Rules() []Rule {
	return []Rule{
		{ID: "copyright-header", Severity: "medium", Enabled: true, Check: checkCopyright},
		{ID: "missing-javadoc", Severity: "low", Enabled: true, Check: checkJavadoc},
		{ID: "swagger-annotation", Severity: "high", Enabled: true, Check: checkSwagger},
		{ID: "trailing-whitespace", Severity: "low", Enabled: true, Check: checkTrailingWhitespace},
		{ID: "file-too-long", Severity: "info", Enabled: true, Check: checkFileLength},
		{ID: "secret-hardcode", Severity: "critical", Enabled: true, Check: checkSecrets},
	}
}

// ScanFile scans a single file's content (path is the display path, lines are
// the file content split by line) with the built-in rule set.
func ScanFile(path string, lines []string) []Finding {
	var out []Finding
	for _, r := range Rules() {
		if !r.Enabled {
			continue
		}
		out = append(out, r.Check(path, lines)...)
	}
	return out
}

// ScanDir recursively scans source files under root (filtered by extension and
// skip directories) and returns all findings, sorted by file, line and rule.
func ScanDir(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !sourceExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		lines, rerr := readLines(path)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, ScanFile(rel, lines)...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortFindings(out)
	return out, nil
}

// checkCopyright verifies Java/Kotlin files open with a block comment that
// contains "Copyright".
func checkCopyright(path string, lines []string) []Finding {
	if !isJavaLike(path) {
		return nil
	}
	if strings.Contains(strings.ToLower(leadingBlock(lines, 15)), "copyright") {
		return nil
	}
	return []Finding{newFinding("copyright-header", "medium", path, 1, "missing copyright header")}
}

// checkJavadoc flags public classes and methods in .java files whose preceding
// Javadoc comment is missing.
func checkJavadoc(path string, lines []string) []Finding {
	if !isJavaFile(path) {
		return nil
	}
	var out []Finding
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			continue
		}
		if !rePublicClass.MatchString(l) && !rePublicMethod.MatchString(l) {
			continue
		}
		if !hasJavadocAbove(lines, i) {
			out = append(out, newFinding("missing-javadoc", "low", path, i+1, "missing Javadoc on public declaration"))
		}
	}
	return out
}

// checkSwagger flags residual Swagger/OpenAPI annotations.
func checkSwagger(path string, lines []string) []Finding {
	var out []Finding
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*") {
			continue
		}
		if reSwagger.MatchString(l) {
			out = append(out, newFinding("swagger-annotation", "high", path, i+1, "Swagger/OpenAPI annotation residual: "+t))
		}
	}
	return out
}

// checkTrailingWhitespace flags lines that end with spaces or tabs.
func checkTrailingWhitespace(path string, lines []string) []Finding {
	var out []Finding
	for i, l := range lines {
		if len(l) != len(strings.TrimRight(l, " \t")) {
			out = append(out, newFinding("trailing-whitespace", "low", path, i+1, "trailing whitespace"))
		}
	}
	return out
}

// checkFileLength flags files whose line count exceeds maxFileLines.
func checkFileLength(path string, lines []string) []Finding {
	if len(lines) <= maxFileLines {
		return nil
	}
	msg := fmt.Sprintf("file has %d lines, exceeding the %d-line limit", len(lines), maxFileLines)
	return []Finding{newFinding("file-too-long", "info", path, 1, msg)}
}

// checkSecrets flags suspected hard-coded secrets and private-key blocks.
func checkSecrets(path string, lines []string) []Finding {
	var out []Finding
	for i, l := range lines {
		if reSecret.MatchString(l) {
			out = append(out, newFinding("secret-hardcode", "critical", path, i+1, "possible hard-coded secret: "+strings.TrimSpace(l)))
			continue
		}
		low := strings.ToLower(l)
		for _, p := range pemMarkers {
			if strings.Contains(low, p) {
				out = append(out, newFinding("secret-hardcode", "critical", path, i+1, "possible hard-coded private key: "+strings.TrimSpace(l)))
				break
			}
		}
	}
	return out
}

// leadingBlock collects the leading comment/blank lines (up to max) before the
// first line of code.
func leadingBlock(lines []string, max int) string {
	n := len(lines)
	if n > max {
		n = max
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "//") || strings.HasPrefix(l, "/*") || strings.HasPrefix(l, "*") {
			b.WriteString(l)
			b.WriteByte(' ')
			continue
		}
		break
	}
	return b.String()
}

// hasJavadocAbove reports whether the declaration at line i is preceded by a
// Javadoc comment (a /** ... */ block ending directly above it).
func hasJavadocAbove(lines []string, i int) bool {
	for j := i - 1; j >= 0; j-- {
		l := strings.TrimSpace(lines[j])
		switch {
		case l == "":
			continue
		case strings.HasPrefix(l, "/**"):
			return true
		case strings.HasPrefix(l, "/*"):
			return false
		case strings.HasPrefix(l, "*"):
			continue
		default:
			return false
		}
	}
	return false
}

func isJavaLike(path string) bool {
	lp := strings.ToLower(path)
	return strings.HasSuffix(lp, ".java") || strings.HasSuffix(lp, ".kt")
}

func isJavaFile(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".java")
}

func newFinding(ruleID, severity, file string, line int, message string) Finding {
	return Finding{RuleID: ruleID, Severity: severity, File: file, Line: line, Message: message}
}

func sortFindings(fs []Finding) {
	sort.Slice(fs, func(i, j int) bool {
		if fs[i].File != fs[j].File {
			return fs[i].File < fs[j].File
		}
		if fs[i].Line != fs[j].Line {
			return fs[i].Line < fs[j].Line
		}
		return fs[i].RuleID < fs[j].RuleID
	})
}

func readLines(path string) ([]string, error) {
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
