package report

import (
	"strings"
	"testing"
)

func TestSummarize(t *testing.T) {
	cases := []struct {
		name    string
		results []CaseResult
		want    Summary
	}{
		{
			name: "empty",
			want: Summary{},
		},
		{
			name: "mixed",
			results: []CaseResult{
				{Status: "passed", DurationMs: 10},
				{Status: "passed", DurationMs: 20},
				{Status: "failed", DurationMs: 30},
				{Status: "skipped", DurationMs: 0},
				{Status: "error", DurationMs: 40},
			},
			want: Summary{Total: 5, Passed: 2, Failed: 1, Skipped: 1, Errors: 1, DurationMs: 100},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Summarize(tc.results); got != tc.want {
				t.Errorf("Summarize() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseSurefireXML(t *testing.T) {
	const suitePrefix = `<testsuite name="com.example.UserServiceTest" time="2.5" tests="1" errors="0" skipped="0" failures="0">`
	const suiteSuffix = `</testsuite>`

	cases := []struct {
		name        string
		xml         string
		wantName    string
		wantStatus  string
		wantDurMs   int64
		errContains string
	}{
		{
			name:       "pass",
			xml:        suitePrefix + `<testcase name="shouldCreate" classname="com.example.UserServiceTest" time="0.100"/>` + suiteSuffix,
			wantName:   "shouldCreate",
			wantStatus: "passed",
			wantDurMs:  100,
		},
		{
			name: "failure",
			xml: suitePrefix + `<testcase name="shouldFind" classname="com.example.UserServiceTest" time="0.200">` +
				`<failure message="expected:&lt;true&gt; but was:&lt;false&gt;" type="java.lang.AssertionError">` +
				"java.lang.AssertionError: expected:&lt;true&gt; but was:&lt;false&gt;\n\tat com.example.UserServiceTest.shouldFind(UserServiceTest.java:42)\n" +
				`</failure></testcase>` + suiteSuffix,
			wantName:    "shouldFind",
			wantStatus:  "failed",
			wantDurMs:   200,
			errContains: "AssertionError",
		},
		{
			name: "skipped",
			xml: suitePrefix + `<testcase name="shouldSkip" classname="com.example.UserServiceTest" time="0.001">` +
				`<skipped message="not implemented yet"/></testcase>` + suiteSuffix,
			wantName:    "shouldSkip",
			wantStatus:  "skipped",
			wantDurMs:   1,
			errContains: "not implemented yet",
		},
		{
			name: "error",
			xml: suitePrefix + `<testcase name="shouldError" classname="com.example.UserServiceTest" time="0.300">` +
				`<error message="connection refused" type="java.sql.SQLException">` +
				"java.sql.SQLException: connection refused\n\tat com.example.UserServiceTest.shouldError(UserServiceTest.java:50)\n" +
				`</error></testcase>` + suiteSuffix,
			wantName:    "shouldError",
			wantStatus:  "error",
			wantDurMs:   300,
			errContains: "connection refused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSurefireXML([]byte(tc.xml))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("ParseSurefireXML() returned %d results, want 1", len(got))
			}
			r := got[0]
			if r.Suite != "com.example.UserServiceTest" {
				t.Errorf("Suite = %q, want com.example.UserServiceTest", r.Suite)
			}
			if r.Class != "com.example.UserServiceTest" {
				t.Errorf("Class = %q, want com.example.UserServiceTest", r.Class)
			}
			if r.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", r.Name, tc.wantName)
			}
			if r.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", r.Status, tc.wantStatus)
			}
			if r.DurationMs != tc.wantDurMs {
				t.Errorf("DurationMs = %d, want %d", r.DurationMs, tc.wantDurMs)
			}
			if tc.errContains != "" && !strings.Contains(r.ErrorXML, tc.errContains) {
				t.Errorf("ErrorXML = %q, want to contain %q", r.ErrorXML, tc.errContains)
			}
			if tc.errContains == "" && r.ErrorXML != "" {
				t.Errorf("ErrorXML = %q, want empty", r.ErrorXML)
			}
		})
	}
}

func TestParseSurefireXMLMultiple(t *testing.T) {
	xml := `<testsuite name="com.example.Suite" time="3.0" tests="4" errors="1" skipped="1" failures="1">
  <testcase name="a" classname="com.example.Suite" time="0.1"/>
  <testcase name="b" classname="com.example.Suite" time="0.2"><failure message="boom">boom</failure></testcase>
  <testcase name="c" classname="com.example.Suite" time="0.0"><skipped/></testcase>
  <testcase name="d" classname="com.example.Suite" time="0.3"><error message="oops">oops</error></testcase>
</testsuite>`
	got, err := ParseSurefireXML([]byte(xml))
	if err != nil {
		t.Fatal(err)
	}
	wantStatuses := []string{"passed", "failed", "skipped", "error"}
	if len(got) != len(wantStatuses) {
		t.Fatalf("got %d results, want %d", len(got), len(wantStatuses))
	}
	for i, want := range wantStatuses {
		if got[i].Status != want {
			t.Errorf("result[%d].Status = %q, want %q", i, got[i].Status, want)
		}
	}
}

func TestParseSurefireXMLInvalid(t *testing.T) {
	if _, err := ParseSurefireXML([]byte("<testsuite><unclosed>")); err == nil {
		t.Fatal("ParseSurefireXML() = nil error, want error for malformed XML")
	}
}

func TestParseGoTestJSON(t *testing.T) {
	const goJSON = `{"Action":"run","Package":"example.com/demo","Test":"TestAdd"}
{"Action":"output","Package":"example.com/demo","Test":"TestAdd","Output":"=== RUN   TestAdd\n"}
{"Action":"output","Package":"example.com/demo","Test":"TestAdd","Output":"--- PASS: TestAdd (0.00s)\n"}
{"Action":"pass","Package":"example.com/demo","Test":"TestAdd","Elapsed":0.005}
{"Action":"run","Package":"example.com/demo","Test":"TestFail"}
{"Action":"output","Package":"example.com/demo","Test":"TestFail","Output":"=== RUN   TestFail\n"}
{"Action":"output","Package":"example.com/demo","Test":"TestFail","Output":"    demo_test.go:10: expected 1, got 2\n"}
{"Action":"output","Package":"example.com/demo","Test":"TestFail","Output":"--- FAIL: TestFail (0.00s)\n"}
{"Action":"fail","Package":"example.com/demo","Test":"TestFail","Elapsed":0.001}
{"Action":"run","Package":"example.com/demo","Test":"TestSkip"}
{"Action":"skip","Package":"example.com/demo","Test":"TestSkip","Elapsed":0}
{"Action":"output","Package":"example.com/demo","Output":"PASS\n"}
{"Action":"pass","Package":"example.com/demo","Elapsed":0.006}
`
	got, err := ParseGoTestJSON([]byte(goJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3 (package-level events skipped)", len(got))
	}
	byName := map[string]CaseResult{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if r := byName["TestAdd"]; r.Status != "passed" || r.Suite != "example.com/demo" || r.DurationMs != 5 {
		t.Errorf("TestAdd = %+v, want passed in example.com/demo at 5ms", r)
	}
	if r := byName["TestFail"]; r.Status != "failed" || !strings.Contains(r.ErrorXML, "expected 1, got 2") {
		t.Errorf("TestFail = %+v, want failed with output", r)
	}
	if r := byName["TestSkip"]; r.Status != "skipped" {
		t.Errorf("TestSkip = %+v, want skipped", r)
	}
}

func TestParseGoTestJSONInvalid(t *testing.T) {
	if _, err := ParseGoTestJSON([]byte("not-json\n")); err == nil {
		t.Fatal("ParseGoTestJSON() = nil error, want error for invalid line")
	}
}

func TestParsePlaywrightJSON(t *testing.T) {
	const pwJSON = `{
  "suites": [
    {
      "title": "example.spec.ts",
      "specs": [
        {
          "title": "example.spec.ts",
          "tests": [
            {
              "projectName": "chromium",
              "title": "should render",
              "status": "passed",
              "duration": 100,
              "results": [{"status": "passed", "duration": 123, "error": null}]
            },
            {
              "projectName": "chromium",
              "title": "should submit",
              "status": "failed",
              "results": [{"status": "failed", "duration": 456, "error": {"message": "expect(true).toBe(false)", "stack": "at example.spec.ts:10:5"}}]
            },
            {
              "projectName": "firefox",
              "title": "should skip",
              "status": "skipped",
              "results": []
            },
            {
              "projectName": "webkit",
              "title": "should timeout",
              "status": "timedOut",
              "results": [{"status": "timedOut", "duration": 30000, "error": {"message": "Test timeout of 30000ms exceeded"}}]
            }
          ]
        }
      ]
    }
  ]
}`
	got, err := ParsePlaywrightJSON([]byte(pwJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d results, want 4", len(got))
	}
	byName := map[string]CaseResult{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if r := byName["should render"]; r.Status != "passed" || r.DurationMs != 100 || r.Suite != "chromium" || r.Class != "example.spec.ts" {
		t.Errorf("should render = %+v, want passed/100ms/chromium/example.spec.ts", r)
	}
	if r := byName["should submit"]; r.Status != "failed" || r.DurationMs != 456 || !strings.Contains(r.ErrorXML, "expect(true).toBe(false)") {
		t.Errorf("should submit = %+v, want failed/456ms with error", r)
	}
	if r := byName["should skip"]; r.Status != "skipped" {
		t.Errorf("should skip = %+v, want skipped", r)
	}
	if r := byName["should timeout"]; r.Status != "error" || r.DurationMs != 30000 {
		t.Errorf("should timeout = %+v, want error/30000ms", r)
	}
}

func TestParseGoTestText(t *testing.T) {
	out := `=== RUN   TestBoom
--- FAIL: TestBoom (0.12s)
    main_test.go:6: boom
    panic: something
=== RUN   TestOK
--- PASS: TestOK (0.00s)
--- SKIP: TestSkip (0.00s)
    main_test.go:9: not on this platform
FAIL
FAIL	demo	0.200s
FAIL
`
	got := ParseGoTestText([]byte(out))
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3: %+v", len(got), got)
	}
	byName := map[string]CaseResult{}
	for _, r := range got {
		byName[r.Name] = r
	}
	if r := byName["TestBoom"]; r.Status != "failed" || r.DurationMs != 120 ||
		!strings.Contains(r.ErrorXML, "main_test.go:6: boom") {
		t.Errorf("TestBoom = %+v, want failed/120ms with the log lines", r)
	}
	if r := byName["TestOK"]; r.Status != "passed" || r.ErrorXML != "" {
		t.Errorf("TestOK = %+v, want passed with no error text", r)
	}
	if r := byName["TestSkip"]; r.Status != "skipped" || r.ErrorXML != "" {
		t.Errorf("TestSkip = %+v, want skipped with no error text", r)
	}

	// Subtests arrive indented under their parent and keep their slash name.
	subs := ParseGoTestText([]byte("--- FAIL: TestOuter (0.00s)\n" +
		"    --- FAIL: TestOuter/sub (0.01s)\n" +
		"        outer_test.go:12: nope\n"))
	if len(subs) != 2 || subs[1].Name != "TestOuter/sub" ||
		!strings.Contains(subs[1].ErrorXML, "outer_test.go:12: nope") {
		t.Errorf("subtests = %+v, want TestOuter + TestOuter/sub carrying the detail", subs)
	}

	// A build failure carries no case lines at all: the caller must be able to
	// tell "nothing failed" from "nothing ran", so this stays empty.
	if got := ParseGoTestText([]byte("# demo\n./main.go:3:2: undefined: foo\nFAIL\tdemo [build failed]\n")); len(got) != 0 {
		t.Errorf("build failure = %+v, want no cases", got)
	}
}
