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
	Suite      string `json:"suite"`
	Class      string `json:"class"`
	Name       string `json:"name"`
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

// playwrightReport is the root of a Playwright JSON report.
type playwrightReport struct {
	Suites []playwrightSuite `json:"suites"`
}

// playwrightSuite is a top-level suite in a Playwright JSON report.
type playwrightSuite struct {
	Title string           `json:"title"`
	Specs []playwrightSpec `json:"specs"`
}

// playwrightSpec is one spec file within a suite.
type playwrightSpec struct {
	Title string           `json:"title"`
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

// ParsePlaywrightJSON parses a Playwright JSON report (suites → specs → tests)
// into normalized case results. timedOut counts as an error; the duration falls
// back to the first result's duration when the test-level value is absent.
func ParsePlaywrightJSON(data []byte) ([]CaseResult, error) {
	var rep playwrightReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, err
	}
	var results []CaseResult
	for _, s := range rep.Suites {
		for _, spec := range s.Specs {
			for _, t := range spec.Tests {
				r := CaseResult{
					Suite:      t.ProjectName,
					Class:      spec.Title,
					Name:       t.Title,
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
	}
	return results, nil
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
