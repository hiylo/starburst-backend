package server

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

// TestFlakyRerunArgs covers the deterministic re-run command builder for every
// supported framework combination: go filter, surefire maven -Dtest, surefire
// gradle --tests, pytest -k, playwright --grep (both the full-title anchor and
// the bare-title substring fallback) and xctest with a -scheme, plus the
// early-return kinds that have no deterministic selector (xctest without a
// scheme, npm).
func TestFlakyRerunArgs(t *testing.T) {
	cases := []struct {
		name       string
		reportKind string
		buildTool  string
		names      []string
		fullTitles []string
		runArgs    []string
		want       []string
	}{
		{
			name:       "go",
			reportKind: "go",
			buildTool:  "go",
			names:      []string{"TestFoo", "TestBar"},
			want:       []string{"go", "test", "-count=1", "-run", "^(TestFoo|TestBar)$", "./..."},
		},
		{
			name:       "surefire maven",
			reportKind: "surefire",
			buildTool:  "maven",
			names:      []string{"com.example.Suite.ok", "com.example.Suite.boom"},
			want:       []string{"mvn", "test", "-Dtest=com.example.Suite#ok+com.example.Suite#boom"},
		},
		{
			name:       "surefire maven method only",
			reportKind: "surefire",
			buildTool:  "maven",
			names:      []string{"test_only"},
			want:       []string{"mvn", "test", "-Dtest=#test_only"},
		},
		{
			name:       "surefire gradle",
			reportKind: "surefire",
			buildTool:  "gradle",
			names:      []string{"com.example.Suite.ok", "com.example.Suite.boom"},
			want:       []string{"./gradlew", "test", "--tests", "com.example.Suite.ok", "--tests", "com.example.Suite.boom"},
		},
		{
			name:       "pytest",
			reportKind: "pytest",
			buildTool:  "pytest",
			names:      []string{"test_alpha", "demo.test_beta"},
			want:       []string{"pytest", "-q", "--junitxml=junit.xml", "-k", "test_alpha or test_beta"},
		},
		{
			name:       "playwright",
			reportKind: "playwright",
			buildTool:  "npm",
			names:      []string{"example.spec.ts.should render", "auth.spec.ts.login"},
			want:       []string{"npx", "playwright", "test", "--reporter=json", "--grep", `should render|login`},
		},
		{
			name:       "playwright title with regex meta",
			reportKind: "playwright",
			buildTool:  "node",
			names:      []string{"home.spec.ts.total is $42"},
			want:       []string{"npx", "playwright", "test", "--reporter=json", "--grep", `total is \$42`},
		},
		{
			name:       "playwright full title anchored",
			reportKind: "playwright",
			buildTool:  "npm",
			names:      []string{"example.spec.ts.should render", "example.spec.ts.should render the modal"},
			fullTitles: []string{"chromium example.spec.ts should render", "chromium example.spec.ts should render the modal"},
			want:       []string{"npx", "playwright", "test", "--reporter=json", "--grep", `^chromium example\.spec\.ts should render$|^chromium example\.spec\.ts should render the modal$`},
		},
		{
			name:       "playwright full title with describe chain",
			reportKind: "playwright",
			buildTool:  "npm",
			names:      []string{"example.spec.ts.should login"},
			fullTitles: []string{"chromium example.spec.ts Auth should login"},
			want:       []string{"npx", "playwright", "test", "--reporter=json", "--grep", `^chromium example\.spec\.ts Auth should login$`},
		},
		{
			name:       "playwright no full title falls back to title substring",
			reportKind: "playwright",
			buildTool:  "npm",
			names:      []string{"example.spec.ts.should render", "example.spec.ts.should render the modal"},
			fullTitles: []string{"", ""},
			want:       []string{"npx", "playwright", "test", "--reporter=json", "--grep", `should render|should render the modal`},
		},
		{
			name:       "xctest with scheme",
			reportKind: "xctest",
			buildTool:  "xcode",
			names:      []string{"LoginTests.testValidLogin", "LoginTests.testWrongPassword"},
			runArgs:    []string{"xcodebuild", "test", "-workspace", "App.xcworkspace", "-scheme", "MyApp", "-destination", "platform=iOS Simulator,name=iPhone 15"},
			want:       []string{"xcodebuild", "test", "-scheme", "MyApp", "-workspace", "App.xcworkspace", "-destination", "platform=iOS Simulator,name=iPhone 15", "-only-testing:LoginTests/testValidLogin", "-only-testing:LoginTests/testWrongPassword"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flakyRerunArgs(c.reportKind, c.buildTool, c.names, c.fullTitles, c.runArgs)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("flakyRerunArgs(%q,%q,%v,%v,%v) = %v, want %v", c.reportKind, c.buildTool, c.names, c.fullTitles, c.runArgs, got, c.want)
			}
		})
	}

	// Kinds without a deterministic re-run selector produce no command: xctest
	// without an xcodebuild -scheme, npm test has no per-case selector, and
	// surefire without a known build tool (maven or gradle) cannot select
	// deterministically either.
	none := []struct {
		reportKind, buildTool string
		runArgs               []string
	}{
		{"xctest", "xcode", nil},
		{"xctest", "xcode", []string{"xcodebuild", "test", "-workspace", "App.xcworkspace"}},
		{"npm", "npm", nil},
		{"surefire", "unknown", nil},
	}
	for _, c := range none {
		if got := flakyRerunArgs(c.reportKind, c.buildTool, []string{"x"}, nil, c.runArgs); got != nil {
			t.Errorf("flakyRerunArgs(%q,%q) = %v, want nil", c.reportKind, c.buildTool, got)
		}
	}
}

// TestFlakyRetrySurefireMaven verifies the cross-framework flaky chain: a
// failed surefire case that passes on the bounded maven re-run is marked flaky
// (recorded as passed) with the flaky marker, and the rerun command carries the
// class#method selection.
func TestFlakyRetrySurefireMaven(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()
	// Simulate the re-run writing fresh surefire reports with both cases green.
	writeTestFile(t, filepath.Join(dir, "target/surefire-reports/TEST-demo.Suite.xml"),
		`<testsuite name="demo.Suite" tests="2">
  <testcase classname="demo.Suite" name="ok" time="0.01"/>
  <testcase classname="demo.Suite" name="boom" time="0.02"/>
</testsuite>`)

	var gotCmd []string
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		gotCmd = append([]string{name}, args...)
		return []byte("BUILD SUCCESS"), nil
	}
	results := []*store.TestResult{
		{Endpoint: "demo.Suite.ok", Kind: "surefire", Passed: false, FailuresJSON: `{"error":"x"}`},
		{Endpoint: "demo.Suite.boom", Kind: "surefire", Passed: false, FailuresJSON: `{"error":"y"}`},
	}
	s.flakyRetry(context.Background(), 0, 0, dir, "surefire", "maven", results)

	want := "mvn test -Dtest=demo.Suite#ok+demo.Suite#boom"
	if strings.Join(gotCmd, " ") != want {
		t.Errorf("rerun command = %q, want %q", strings.Join(gotCmd, " "), want)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s should be marked flaky-passed", r.Endpoint)
		}
		if !strings.Contains(r.FailuresJSON, "flaky") {
			t.Errorf("flaky marker missing on %s: %s", r.Endpoint, r.FailuresJSON)
		}
	}
}

// TestFlakyRetryPytest verifies the same flaky chain for pytest: failed cases
// (module-level, empty class) that pass on the -k re-run are marked flaky.
func TestFlakyRetryPytest(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "junit.xml"),
		`<testsuite name="pytest" tests="2">
  <testcase classname="" name="test_alpha" time="0.01"/>
  <testcase classname="" name="test_beta" time="0.02"/>
</testsuite>`)

	var gotCmd []string
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		gotCmd = append([]string{name}, args...)
		return []byte("2 passed"), nil
	}
	results := []*store.TestResult{
		{Endpoint: ".test_alpha", Kind: "pytest", Passed: false, FailuresJSON: `{"error":"x"}`},
		{Endpoint: ".test_beta", Kind: "pytest", Passed: false, FailuresJSON: `{"error":"y"}`},
	}
	s.flakyRetry(context.Background(), 0, 0, dir, "pytest", "pytest", results)

	want := "pytest -q --junitxml=junit.xml -k test_alpha or test_beta"
	if strings.Join(gotCmd, " ") != want {
		t.Errorf("rerun command = %q, want %q", strings.Join(gotCmd, " "), want)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s should be marked flaky-passed", r.Endpoint)
		}
		if !strings.Contains(r.FailuresJSON, "flaky") {
			t.Errorf("flaky marker missing on %s: %s", r.Endpoint, r.FailuresJSON)
		}
	}
}

// TestFlakyRetryPlaywright verifies the flaky chain for playwright: failed
// cases (parsed from the JSON report as "<spec title>.<test title>") that pass
// on the --grep re-run are marked flaky (recorded as passed). The re-run's
// --grep anchors the Playwright full grep title (project + spec + test,
// recovered from the result's FailuresJSON "suite" and endpoint) with ^...$ so
// a title that prefixes a sibling's does not drag it in.
func TestFlakyRetryPlaywright(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()

	var gotCmd []string
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		gotCmd = append([]string{name}, args...)
		// The re-run writes a fresh playwright JSON report with both cases
		// green; parseReport reads it from stdout.
		return []byte(`{"suites":[{"title":"example.spec.ts","specs":[{"title":"example.spec.ts","tests":[
{"projectName":"chromium","title":"should render","status":"passed","duration":123,"results":[{"status":"passed","duration":123}]},
{"projectName":"chromium","title":"should submit","status":"passed","duration":456,"results":[{"status":"passed","duration":456}]}
]}]}]}`), nil
	}
	results := []*store.TestResult{
		{Endpoint: "example.spec.ts.should render", Kind: "playwright", Passed: false, FailuresJSON: `{"suite":"chromium","error":"x"}`},
		{Endpoint: "example.spec.ts.should submit", Kind: "playwright", Passed: false, FailuresJSON: `{"suite":"chromium","error":"y"}`},
	}
	s.flakyRetry(context.Background(), 0, 0, dir, "playwright", "npm", results)

	want := `npx playwright test --reporter=json --grep ^chromium example\.spec\.ts should render$|^chromium example\.spec\.ts should submit$`
	if strings.Join(gotCmd, " ") != want {
		t.Errorf("rerun command = %q, want %q", strings.Join(gotCmd, " "), want)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s should be marked flaky-passed", r.Endpoint)
		}
		if !strings.Contains(r.FailuresJSON, "flaky") {
			t.Errorf("flaky marker missing on %s: %s", r.Endpoint, r.FailuresJSON)
		}
	}
}

// TestFlakyRetryPlaywrightPrefixFallback verifies the defensive fallback: when
// the result carries no project name in FailuresJSON there is no exact full
// title to anchor on, so the re-run keeps the previous bare-title substring
// --grep (a title that prefixes a sibling may then re-run it, but the retry
// still works for the common case).
func TestFlakyRetryPlaywrightPrefixFallback(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()

	var gotCmd []string
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		gotCmd = append([]string{name}, args...)
		return []byte(`{"suites":[{"title":"example.spec.ts","specs":[{"title":"example.spec.ts","tests":[
{"projectName":"chromium","title":"should render","status":"passed","duration":123,"results":[{"status":"passed","duration":123}]}
]}]}]}`), nil
	}
	results := []*store.TestResult{
		{Endpoint: "example.spec.ts.should render", Kind: "playwright", Passed: false, FailuresJSON: `{"error":"x"}`},
		{Endpoint: "example.spec.ts.should render the modal", Kind: "playwright", Passed: false, FailuresJSON: `{"error":"y"}`},
	}
	s.flakyRetry(context.Background(), 0, 0, dir, "playwright", "npm", results)

	want := `npx playwright test --reporter=json --grep should render|should render the modal`
	if strings.Join(gotCmd, " ") != want {
		t.Errorf("rerun command = %q, want %q", strings.Join(gotCmd, " "), want)
	}
	if !results[0].Passed {
		t.Errorf("%s should be marked flaky-passed", results[0].Endpoint)
	}
	if results[1].Passed {
		t.Errorf("%s should stay failed (missing from the re-run report)", results[1].Endpoint)
	}
}

// TestFlakyRetryXCTest verifies the flaky chain for xctest when the executed
// command carried an xcodebuild -scheme: the re-run selects the failed methods
// with -only-testing:<Class>/<method>, and the passing re-run is marked flaky.
func TestFlakyRetryXCTest(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()

	var gotCmd []string
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		gotCmd = append([]string{name}, args...)
		return []byte(`<testsuites>
  <testsuite name="LoginTests" tests="2">
    <testcase classname="LoginTests.testValidLogin" name="testValidLogin" time="0.01"/>
    <testcase classname="LoginTests.testWrongPassword" name="testWrongPassword" time="0.02"/>
  </testsuite>
</testsuites>`), nil
	}
	results := []*store.TestResult{
		{Endpoint: "LoginTests.testValidLogin", Kind: "xctest", Passed: false, FailuresJSON: `{"suite":"LoginTests","error":"x"}`},
		{Endpoint: "LoginTests.testWrongPassword", Kind: "xctest", Passed: false, FailuresJSON: `{"suite":"LoginTests","error":"y"}`},
	}
	runArgs := []string{"xcodebuild", "test", "-workspace", "App.xcworkspace", "-scheme", "MyApp", "-destination", "platform=iOS Simulator,name=iPhone 15"}
	s.flakyRetry(context.Background(), 0, 0, dir, "xctest", "xcode", results, runArgs...)

	want := "xcodebuild test -scheme MyApp -workspace App.xcworkspace -destination platform=iOS Simulator,name=iPhone 15 -only-testing:LoginTests/testValidLogin -only-testing:LoginTests/testWrongPassword"
	if strings.Join(gotCmd, " ") != want {
		t.Errorf("rerun command = %q, want %q", strings.Join(gotCmd, " "), want)
	}
	for _, r := range results {
		if !r.Passed {
			t.Errorf("%s should be marked flaky-passed", r.Endpoint)
		}
		if !strings.Contains(r.FailuresJSON, "flaky") {
			t.Errorf("flaky marker missing on %s: %s", r.Endpoint, r.FailuresJSON)
		}
	}
}

// TestFlakyRetryXCTestNoScheme verifies xctest without an xcodebuild -scheme in
// the executed command never re-executes (the selector cannot be built).
func TestFlakyRetryXCTestNoScheme(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	calls := 0
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls++
		return []byte(""), nil
	}
	results := []*store.TestResult{
		{Endpoint: "LoginTests.testValidLogin", Kind: "xctest", Passed: false},
	}
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "xctest", "xcode", results, "xcodebuild", "test", "-workspace", "App.xcworkspace")
	if calls != 0 {
		t.Errorf("xctest without -scheme triggered rerun (%d calls)", calls)
	}
	if results[0].Passed {
		t.Error("xctest without -scheme must not flip the case to passed")
	}
}

// TestFlakyRetrySurefireStillFailing verifies a re-run that still fails keeps
// the case failed (no flaky marker), mirroring the go branch.
func TestFlakyRetrySurefireStillFailing(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "target/surefire-reports/TEST-demo.Suite.xml"),
		`<testsuite name="demo.Suite" tests="1">
  <testcase classname="demo.Suite" name="boom" time="0.02"><failure message="boom">boom stack</failure></testcase>
</testsuite>`)
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		return []byte("BUILD FAILURE"), nil
	}
	results := []*store.TestResult{
		{Endpoint: "demo.Suite.boom", Kind: "surefire", Passed: false, FailuresJSON: `{"error":"y"}`},
	}
	s.flakyRetry(context.Background(), 0, 0, dir, "surefire", "maven", results)
	if results[0].Passed {
		t.Error("still-failing surefire case must stay failed")
	}
	if strings.Contains(results[0].FailuresJSON, "flaky") {
		t.Errorf("still-failing case must not carry the flaky marker: %s", results[0].FailuresJSON)
	}
}

// TestFlakyRetrySurefireNoBuildTool verifies that surefire without a known
// build tool (maven/gradle) never re-executes.
func TestFlakyRetrySurefireNoBuildTool(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)
	calls := 0
	runCmd = func(ctx context.Context, d, name string, args ...string) ([]byte, error) {
		calls++
		return []byte(""), nil
	}
	results := []*store.TestResult{
		{Endpoint: "demo.Suite.boom", Kind: "surefire", Passed: false},
	}
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "surefire", "swiftpm", results)
	if calls != 0 {
		t.Errorf("unknown build tool triggered rerun (%d calls)", calls)
	}
	if results[0].Passed {
		t.Error("unknown build tool must not flip the case to passed")
	}
}
