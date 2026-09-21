// Package report parses test framework reports (Maven Surefire XML, go test
// -json output, Playwright JSON) into a unified per-case result model for the
// Test Intelligence subsystem. It depends only on the standard library so the
// upper layer can persist results into test_results / test_case_results.
package report

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strconv"
	"strings"
)

// CaseResult is a single normalized per-test-case result.
type CaseResult struct {
	Suite string `json:"suite"`
	Class string `json:"class"`
	Name  string `json:"name"`
	// FullTitle carries the framework-level fully qualified title of the case
	// when the report provides one (currently only Playwright): the string
	// `--grep` is matched against, so a flaky re-run can select exactly this
	// case by anchoring it with ^...$. Empty for frameworks without such a
	// notion. It is a report-layer field and is never persisted into store.
	FullTitle  string `json:"fullTitle,omitempty"`
	Status     string `json:"status"` // passed | failed | skipped | error
	DurationMs int64  `json:"durationMs"`
	ErrorXML   string `json:"errorXml"` // failure stack / error message, truncated
}

// Summary aggregates one run's per-case results.
type Summary struct {
	Total      int   `json:"total"`
	Passed     int   `json:"passed"`
	Failed     int   `json:"failed"`
	Skipped    int   `json:"skipped"`
	Errors     int   `json:"errors"`
	DurationMs int64 `json:"durationMs"`
}

// maxErrorLen bounds the retained failure/error text so persisted rows stay
// small while still carrying the meaningful head of a stack trace.
const maxErrorLen = 8192

// Summarize computes a run summary from normalized case results. Duration is
// the sum of individual case durations.
func Summarize(results []CaseResult) Summary {
	s := Summary{}
	for _, r := range results {
		s.Total++
		s.DurationMs += r.DurationMs
		switch r.Status {
		case "passed":
			s.Passed++
		case "failed":
			s.Failed++
		case "skipped":
			s.Skipped++
		case "error":
			s.Errors++
		}
	}
	return s
}

// truncate trims and caps error text to maxErrorLen bytes.
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen]
}

// joinMsg combines an optional short message with a longer body into one
// error string, mirroring the "first line + stack" shape of a failure report.
func joinMsg(msg, body string) string {
	msg = strings.TrimSpace(msg)
	body = strings.TrimSpace(body)
	switch {
	case msg != "" && body != "":
		return msg + "\n" + body
	case msg != "":
		return msg
	default:
		return body
	}
}

// secondsToMillis parses a decimal seconds value into milliseconds. Invalid or
// empty input yields 0 rather than failing the whole report.
func secondsToMillis(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int64(f * 1000)
}

// surefireSuite mirrors the root <testsuite> element of a Maven Surefire
// TEST-*.xml report.
type surefireSuite struct {
	Name  string         `xml:"name,attr"`
	Time  string         `xml:"time,attr"`
	Cases []surefireCase `xml:"testcase"`
}

// surefireCase mirrors a single <testcase> element.
type surefireCase struct {
	Name      string           `xml:"name,attr"`
	Classname string           `xml:"classname,attr"`
	Time      string           `xml:"time,attr"`
	Failure   *surefireFailure `xml:"failure"`
	Error     *surefireError   `xml:"error"`
	Skipped   *surefireSkipped `xml:"skipped"`
}

// surefireFailure mirrors a <failure> child element.
type surefireFailure struct {
	Message string `xml:"message,attr"`
	Content string `xml:",chardata"`
}

// surefireError mirrors an <error> child element.
type surefireError struct {
	Message string `xml:"message,attr"`
	Content string `xml:",chardata"`
}

// surefireSkipped mirrors a <skipped> child element.
type surefireSkipped struct {
	Message string `xml:"message,attr"`
	Content string `xml:",chardata"`
}

// ParseSurefireXML parses a Maven Surefire TEST-*.xml report into normalized
// case results. A testcase's status is derived from its child elements:
// <failure> → failed, <error> → error, <skipped> → skipped, otherwise passed.
func ParseSurefireXML(data []byte) ([]CaseResult, error) {
	var suite surefireSuite
	if err := xml.Unmarshal(data, &suite); err != nil {
		return nil, err
	}
	results := make([]CaseResult, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		r := CaseResult{
			Suite:      suite.Name,
			Class:      c.Classname,
			Name:       c.Name,
			Status:     c.status(),
			DurationMs: secondsToMillis(c.Time),
		}
		if r.Status != "passed" {
			r.ErrorXML = truncate(c.errorXML())
		}
		results = append(results, r)
	}
	return results, nil
}

// status maps a testcase's child elements to a unified status.
func (c *surefireCase) status() string {
	switch {
	case c.Failure != nil:
		return "failed"
	case c.Error != nil:
		return "error"
	case c.Skipped != nil:
		return "skipped"
	default:
		return "passed"
	}
}

// errorXML returns the raw failure/error/skip text for a testcase, or "" when
// the testcase passed.
func (c *surefireCase) errorXML() string {
	switch {
	case c.Failure != nil:
		return joinMsg(c.Failure.Message, c.Failure.Content)
	case c.Error != nil:
		return joinMsg(c.Error.Message, c.Error.Content)
	case c.Skipped != nil:
		return joinMsg(c.Skipped.Message, c.Skipped.Content)
	default:
		return ""
	}
}

// junitTestSuites mirrors a <testsuites> root element that wraps one or more
// <testsuite> elements, as emitted by Xcode XCTest tooling and fastlane/scan.
// The inner suites reuse surefireSuite because the element layout is shared.
type junitTestSuites struct {
	Suites []surefireSuite `xml:"testsuite"`
}

// ParseXCTestJUnit parses an Xcode XCTest JUnit XML report (fastlane/scan or
// xcresulttool exports) into normalized case results. Both a <testsuites>
// root wrapping multiple suites and a bare <testsuite> root are accepted.
// XCTest encodes the owning class and method in classname as "Class.method"
// (fastlane/scan may prefix the bundle: "Bundle.Class.method"); splitXCTestName
// splits at the last dot so Class holds bundle+class and Name the method, and
// a classname without a dot falls back to the name attribute. Status is
// derived from the <failure>/<error>/<skipped> child elements.
func ParseXCTestJUnit(data []byte) ([]CaseResult, error) {
	var ts junitTestSuites
	if err := xml.Unmarshal(data, &ts); err != nil {
		return nil, err
	}
	if len(ts.Suites) == 0 {
		var s surefireSuite
		if err := xml.Unmarshal(data, &s); err != nil {
			return nil, err
		}
		ts.Suites = []surefireSuite{s}
	}
	var results []CaseResult
	for _, s := range ts.Suites {
		for _, c := range s.Cases {
			class, name := splitXCTestName(c.Classname, c.Name)
			r := CaseResult{
				Suite:      s.Name,
				Class:      class,
				Name:       name,
				Status:     c.status(),
				DurationMs: secondsToMillis(c.Time),
			}
			if r.Status != "passed" {
				r.ErrorXML = truncate(c.errorXML())
			}
			results = append(results, r)
		}
	}
	return results, nil
}

// splitXCTestName maps an XCTest classname onto (Class, Name). The classname's
// last dot separates the method name; everything left of it is the class, which
// may itself contain dots for the bundle prefix — fastlane/scan emits both
// "Class.method" and "Bundle.Class.method", and the multi-dot form also matches
// the class part of "-only-testing:Class/method", which Xcode 13+ accepts with
// the target/bundle omitted. A classname without a dot yields an empty Class
// and uses the name attribute as the Name fallback.
func splitXCTestName(classname, fallback string) (class, name string) {
	idx := strings.LastIndexByte(classname, '.')
	if idx < 0 {
		return "", fallback
	}
	return classname[:idx], classname[idx+1:]
}

// ParsePytestJUnit parses a pytest --junitxml report into normalized case
// results. The structure matches Maven Surefire, but pytest may omit
// classname (module-level tests), in which case Class stays empty and Name
// carries the test name. Status is derived from the child elements: <failure>
// → failed, <error> → error, <skipped> → skipped, otherwise passed.
func ParsePytestJUnit(data []byte) ([]CaseResult, error) {
	var suite surefireSuite
	if err := xml.Unmarshal(data, &suite); err != nil {
		return nil, err
	}
	results := make([]CaseResult, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		r := CaseResult{
			Suite:      suite.Name,
			Class:      c.Classname,
			Name:       c.Name,
			Status:     c.status(),
			DurationMs: secondsToMillis(c.Time),
		}
		if r.Status != "passed" {
			r.ErrorXML = truncate(c.errorXML())
		}
		results = append(results, r)
	}
	return results, nil
}

// goTestEvent is one line of `go test -json` output. Package-level events have
// an empty Test field.
type goTestEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Output  string  `json:"Output"`
	Elapsed float64 `json:"Elapsed"`
}

// ParseGoTestJSON parses `go test -json` output (one JSON object per line). A
// test is finalized when its pass/fail/skip action arrives; the preceding
// output lines for that test supply the failure text. Package-level events
// (empty Test) are ignored.
func ParseGoTestJSON(data []byte) ([]CaseResult, error) {
	var results []CaseResult
	output := make(map[string]string)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev goTestEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, err
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "output":
			output[ev.Test] += ev.Output
		case "pass", "fail", "skip":
			r := CaseResult{
				Suite:      ev.Package,
				Name:       ev.Test,
				Status:     goActionStatus(ev.Action),
				DurationMs: int64(ev.Elapsed * 1000),
			}
			if r.Status == "failed" {
				r.ErrorXML = truncate(output[ev.Test])
			}
			results = append(results, r)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// goActionStatus maps a go test action to a unified status.
func goActionStatus(action string) string {
	switch action {
	case "pass":
		return "passed"
	case "skip":
		return "skipped"
	case "fail":
		return "failed"
	default:
		return action
	}
}

// ParseGoTestText parses the classic (non -json) `go test` output, where every
// finalized case prints a "--- PASS|FAIL|SKIP: Name (0.00s)" line followed by
// its indented log lines. The module command carries -json, so this is the
// fallback for a project whose reviewed whitelist entry omits it — without it
// such a hand-run Go module yields no per-case results at all. go prints the
// PASS lines only under -v, so a plain passing package still yields nothing —
// the caller then relies on the process exit code for the run status.
func ParseGoTestText(data []byte) []CaseResult {
	var out []CaseResult
	var cur *CaseResult
	flush := func() {
		if cur == nil {
			return
		}
		if cur.Status == "failed" {
			cur.ErrorXML = truncate(cur.ErrorXML)
		}
		out = append(out, *cur)
		cur = nil
	}
	for _, line := range strings.Split(string(data), "\n") {
		if r, ok := goTextCaseLine(strings.TrimSpace(line)); ok {
			flush()
			cur = r
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if cur == nil || !indented {
			flush() // package summary lines, build output
			continue
		}
		if cur.Status == "failed" {
			cur.ErrorXML += strings.TrimSpace(line) + "\n"
		}
	}
	flush()
	return out
}

// goTextCaseLine recognises a "--- STATUS: Name (1.23s)" case line. Subtests
// arrive indented under their parent and keep their "Parent/sub" name.
func goTextCaseLine(trimmed string) (*CaseResult, bool) {
	rest, found := strings.CutPrefix(trimmed, "--- ")
	if !found {
		return nil, false
	}
	status, rest, found := strings.Cut(rest, ":")
	if !found || !strings.HasSuffix(rest, ")") {
		return nil, false
	}
	rest = strings.TrimSpace(rest)
	open := strings.LastIndex(rest, " (")
	if open <= 0 {
		return nil, false
	}
	name := rest[:open]
	var statusKind string
	switch strings.TrimSpace(status) {
	case "PASS":
		statusKind = "passed"
	case "FAIL":
		statusKind = "failed"
	case "SKIP":
		statusKind = "skipped"
	default:
		return nil, false
	}
	return &CaseResult{
		Name:       name,
		Status:     statusKind,
		DurationMs: secondsToMillis(strings.TrimSuffix(rest[open+2:len(rest)-1], "s")),
	}, true
}

// playwrightReport is the root of a Playwright JSON report.
type playwrightReport struct {
	Suites []playwrightSuite `json:"suites"`
}

// playwrightSuite is a suite in a Playwright JSON report. The top-level suites
// hold one file each (title mirrors the spec file path, file carries it too);
// describe blocks appear as nested suites (suites can nest arbitrarily). File
// is the report file path this suite maps to, present on the top-level file
// suites and any describe suite that carries one.
type playwrightSuite struct {
	Title  string            `json:"title"`
	File   string            `json:"file"`
	Specs  []playwrightSpec  `json:"specs"`
	Suites []playwrightSuite `json:"suites"`
}

// playwrightSpec is one spec within a suite (in this repo's normalized model
// spec.Title is the spec file name, e.g. "example.spec.ts", reused as the
// endpoint class part). File is the spec file path as emitted by the real
// Playwright reporter; it is preferred over Title when building the full grep
// title, with Title kept as the fallback for reports that omit it.
type playwrightSpec struct {
	Title string           `json:"title"`
	File  string           `json:"file"`
	Tests []playwrightTest `json:"tests"`
}

// playwrightTest is a single test within a spec.
type playwrightTest struct {
	Title       string             `json:"title"`
	ProjectName string             `json:"projectName"`
	Status      string             `json:"status"`
	Duration    int64              `json:"duration"`
	Results     []playwrightResult `json:"results"`
}

// playwrightResult is one run attempt of a test (retries produce several).
type playwrightResult struct {
	Status   string           `json:"status"`
	Duration int64            `json:"duration"`
	Error    *playwrightError `json:"error"`
}

// playwrightError carries the message and stack of a failed attempt.
type playwrightError struct {
	Message string `json:"message"`
	Stack   string `json:"stack"`
}

// ParsePlaywrightJSON parses a Playwright JSON report (suites → specs → tests,
// with describe blocks nesting as sub-suites) into normalized case results.
// timedOut counts as an error; the duration falls back to the first result's
// duration when the test-level value is absent. FullTitle carries the string
// Playwright matches its --grep regex against — the space-joined
// "<project name> <file> [<describe title>...] <test title>" grep title
// (Playwright joins test._grepTitleWithTags() parts with a space, project name
// first), where <file> is the closest spec/suite "file" path (falling back to
// the spec title for reports that omit it) — so a flaky re-run can select
// exactly one test by anchoring it with ^...$.
func ParsePlaywrightJSON(data []byte) ([]CaseResult, error) {
	var rep playwrightReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var results []CaseResult
	for _, s := range rep.Suites {
		results = appendPlaywrightSuite(results, s, nil)
	}
	return results, nil
}

// appendPlaywrightSuite flattens a (possibly nested) playwright suite into case
// results. describe carries the titles of the enclosing describe blocks in
// outer → inner order; the current suite's own title is never pushed here —
// only nested suites contribute their title when recursing. The top-level file
// suite (whose title mirrors the file path already used as the file component)
// is therefore not duplicated with the file segment.
func appendPlaywrightSuite(results []CaseResult, s playwrightSuite, describe []string) []CaseResult {
	return appendPlaywrightSuiteFile(results, s, describe, "")
}

// appendPlaywrightSuiteFile is the recursive worker for appendPlaywrightSuite;
// inheritedFile is the nearest enclosing suite's effective file path, used as a
// further fallback when a spec carries no file of its own.
func appendPlaywrightSuiteFile(results []CaseResult, s playwrightSuite, describe []string, inheritedFile string) []CaseResult {
	suiteFile := s.File
	if suiteFile == "" {
		suiteFile = inheritedFile
	}
	for _, spec := range s.Specs {
		file := spec.File
		if file == "" {
			file = suiteFile
		}
		if file == "" {
			file = spec.Title
		}
		for _, t := range spec.Tests {
			r := CaseResult{
				Suite:      t.ProjectName,
				Class:      spec.Title,
				Name:       t.Title,
				FullTitle:  playwrightFullTitle(t.ProjectName, file, describe, t.Title),
				Status:     playwrightStatus(t.Status),
				DurationMs: t.Duration,
			}
			if r.DurationMs == 0 && len(t.Results) > 0 {
				r.DurationMs = t.Results[0].Duration
			}
			if r.Status != "passed" && len(t.Results) > 0 && t.Results[0].Error != nil {
				r.ErrorXML = truncate(joinMsg(t.Results[0].Error.Message, t.Results[0].Error.Stack))
			}
			results = append(results, r)
		}
	}
	for _, sub := range s.Suites {
		next := describe
		if sub.Title != "" {
			next = append(append([]string{}, describe...), sub.Title)
		}
		results = appendPlaywrightSuiteFile(results, sub, next, suiteFile)
	}
	return results
}

// playwrightFullTitle builds the --grep match string for one test: the
// space-joined, empty-parts-omitted "<project name> <file> [<describe
// title>...] <test title>" grep title. This mirrors the string Playwright
// matches its --grep regex against, so anchoring it with ^...$ selects a
// single test (a title that prefixes a sibling's no longer drags it in).
func playwrightFullTitle(project, file string, describe []string, test string) string {
	parts := []string{project, file}
	parts = append(parts, describe...)
	parts = append(parts, test)
	return strings.Join(nonEmptyStrings(parts), " ")
}

// nonEmptyStrings drops empty strings while preserving order.
func nonEmptyStrings(ss []string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// playwrightStatus maps a Playwright status to a unified status; timedOut is
// treated as an error, and any other non-passed/skipped status as failed.
func playwrightStatus(s string) string {
	switch s {
	case "passed":
		return "passed"
	case "skipped":
		return "skipped"
	case "timedOut":
		return "error"
	default:
		return "failed"
	}
}
