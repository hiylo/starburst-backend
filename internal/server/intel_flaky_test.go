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
// gradle --tests and pytest -k, plus the early-return kinds that have no
// deterministic selector.
func TestFlakyRerunArgs(t *testing.T) {
	cases := []struct {
		name       string
		reportKind string
		buildTool  string
		names      []string
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := flakyRerunArgs(c.reportKind, c.buildTool, c.names)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("flakyRerunArgs(%q,%q,%v) = %v, want %v", c.reportKind, c.buildTool, c.names, got, c.want)
			}
		})
	}

	// Kinds without a deterministic re-run selector produce no command.
	none := []struct {
		reportKind, buildTool string
	}{
		{"playwright", "npm"},
		{"xctest", "xcode"},
		{"npm", "npm"},
		{"surefire", "unknown"},
	}
	for _, c := range none {
		if got := flakyRerunArgs(c.reportKind, c.buildTool, []string{"x"}); got != nil {
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
