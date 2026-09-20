package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelFeatureSingleTest verifies the feature single-test: it calls a
// feature's endpoints against a live base URL and validates the response body
// against the extracted field contract.
func TestIntelFeatureSingleTest(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// A live backend that returns a matching DTO-shaped response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":1,"nickname":"alice"}]`))
	}))
	defer backend.Close()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<demo.UserEntity> list() { return null; }
}`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "nickname")
    private String nickname;
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Resolve the auto-clustered feature (controller name "User").
	rec = s.do(t, http.MethodGet, "/api/intel/features?projectId="+jsonInt(proj.ID), "", wh)
	var fResp struct {
		Features []struct {
			ID     int64  `json:"id"`
			Anchor string `json:"anchor"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil {
		t.Fatalf("features parse: %v", err)
	}
	if len(fResp.Features) != 1 || fResp.Features[0].Anchor != "User" {
		t.Fatalf("features = %+v, want one User feature", fResp.Features)
	}
	featID := fResp.Features[0].ID

	rec = s.do(t, http.MethodPost, "/api/intel/features/test",
		`{"projectId":`+jsonInt(proj.ID)+`,"featureId":`+jsonInt(featID)+`,"baseUrl":"`+backend.URL+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("feature test status %d: %s", rec.Code, rec.Body.String())
	}
	var tResp struct {
		Results []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Status   int    `json:"status"`
			OK       bool   `json:"ok"`
			Contract *struct {
				Passed int `json:"passed"`
				Failed int `json:"failed"`
			} `json:"contract"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tResp); err != nil {
		t.Fatalf("feature test parse: %v", err)
	}
	if len(tResp.Results) != 1 {
		t.Fatalf("results = %d, want 1: %s", len(tResp.Results), rec.Body.String())
	}
	r := tResp.Results[0]
	if r.Status != 200 || !r.OK {
		t.Errorf("endpoint not reachable: status=%d ok=%v", r.Status, r.OK)
	}
	if r.Contract == nil || r.Contract.Failed != 0 {
		t.Errorf("contract check failed: %+v", r.Contract)
	}
}

// TestWhitelistedTestCommand verifies that a reviewed command whitelist is
// honored at run time: matching test intent is chosen over the tool default,
// non-matching entries fall back to default, and JSON is parsed without a shell.
func TestWhitelistedTestCommand(t *testing.T) {
	// Human-reviewed whitelist with an extra coverage goal -> that entry wins.
	got := whitelistedTestCommand(`["mvn clean install","mvn test -Dcoverage"]`, "maven", "")
	if len(got) != 3 || got[0] != "mvn" || got[1] != "test" {
		t.Errorf("maven whitelist = %v, want [mvn test -Dcoverage]", got)
	}
	// go: only "go test" entries qualify.
	got = whitelistedTestCommand(`["go vet ./...","go test ./internal/..."]`, "go", "")
	if len(got) != 3 || got[1] != "test" || got[2] != "./internal/..." {
		t.Errorf("go whitelist = %v, want [go test ./internal/...]", got)
	}
	// gradle android: testDebugUnitTest intent matches.
	got = whitelistedTestCommand(`["./gradlew assembleDebug"]`, "gradle", "android")
	if got != nil {
		t.Errorf("assembleDebug should not match test intent: %v", got)
	}
	got = whitelistedTestCommand(`["./gradlew assembleDebug","./gradlew testDebugUnitTest"]`, "gradle", "android")
	if len(got) != 2 || got[1] != "testDebugUnitTest" {
		t.Errorf("gradle android whitelist = %v", got)
	}
	// Empty / invalid whitelist falls back to default.
	if whitelistedTestCommand("", "go", "") != nil {
		t.Error("empty whitelist should fall back")
	}
	if whitelistedTestCommand("not-json", "go", "") != nil {
		t.Error("invalid whitelist should fall back")
	}
	// npm (web/BFF modules): test intent matches and the tool default exists.
	got = whitelistedTestCommand(`["npm run build","npm test"]`, "npm", "")
	if len(got) != 2 || got[0] != "npm" || got[1] != "test" {
		t.Errorf("npm whitelist = %v, want [npm test]", got)
	}
	if argv, _ := testCommandFor("npm", "web"); len(argv) != 2 || argv[0] != "npm" {
		t.Errorf("testCommandFor(npm) = %v, want [npm test]", argv)
	}
	// The go default must carry both flags: without -json there are no per-case
	// events, and without -count=1 an unchanged package reports "ok (cached)" and
	// the run ends up green with zero cases.
	goArgv, goKind := testCommandFor("go", "")
	if goKind != "go" {
		t.Errorf("testCommandFor(go) kind = %q, want go", goKind)
	}
	joined := strings.Join(goArgv, " ")
	if !strings.Contains(joined, "-json") || !strings.Contains(joined, "-count=1") {
		t.Errorf("testCommandFor(go) = %q, want both -json and -count=1", joined)
	}
}

// TestFlakyRetry verifies the deterministic flaky governance: a failed Go case
// that passes on the bounded retry is marked flaky (recorded as passed) and is
// not turned into a bug; a still-failing rerun keeps it failed.
func TestFlakyRetry(t *testing.T) {
	old := runCmd
	defer func() { runCmd = old }()
	s := newTestServer(t)

	// Rerun passes -> flaky.
	calls := 0
	runCmd = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		return []byte(`{"Action":"pass","Test":"TestFoo","Package":"pkg","Elapsed":0.01}
{"Action":"pass","Test":"TestBar","Package":"pkg","Elapsed":0.02}
`), nil
	}
	results := []*store.TestResult{
		{Endpoint: ".TestFoo", Kind: "go", Passed: false, FailuresJSON: `{"error":"x"}`},
		{Endpoint: ".TestBar", Kind: "go", Passed: false, FailuresJSON: `{"error":"y"}`},
		{Endpoint: ".TestOk", Kind: "go", Passed: true},
	}
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "go", results)
	if calls != 1 {
		t.Fatalf("rerun calls = %d, want 1", calls)
	}
	if !results[0].Passed {
		t.Error("TestFoo should be marked flaky-passed")
	}
	if !results[1].Passed {
		t.Error("TestBar should be marked flaky-passed")
	}
	if !strings.Contains(results[0].FailuresJSON, "flaky") {
		t.Errorf("flaky marker missing: %s", results[0].FailuresJSON)
	}
	if !results[2].Passed {
		t.Error("passing case must stay passed")
	}

	// Non-Go report kinds never re-execute.
	calls = 0
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "surefire", results)
	if calls != 0 {
		t.Errorf("surefire triggered rerun (%d calls)", calls)
	}

	// Rerun still fails -> stays failed.
	runCmd = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		return []byte(`{"Action":"fail","Test":"TestFoo","Package":"pkg","Output":"boom"}
`), nil
	}
	results2 := []*store.TestResult{{Endpoint: ".TestFoo", Kind: "go", Passed: false}}
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "go", results2)
	if results2[0].Passed {
		t.Error("still-failing case must stay failed")
	}
}

// TestIntelRunRemoteNode verifies the run routing: with a node specified, the
// test command is driven over SSH (stubbed) and the stdout go-JSON is parsed
// into results instead of running locally.
func TestIntelRunRemoteNode(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// A reachable node target (keeps reachable=true).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"go-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"linux-docker","workDir":"/srv/repos/echo"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	old := envagent.RunSSH
	defer func() { envagent.RunSSH = old }()
	called := false
	var sshArgs []string
	envagent.RunSSH = func(ctx context.Context, args ...string) (string, error) {
		called = true
		sshArgs = args
		return `{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`, nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"node":`+jsonInt(node.Node.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote run status %d: %s", rec.Code, rec.Body.String())
	}
	// 异步执行：等待后台任务真正路由到 RunSSH。
	waitIntelCondition(t, func() bool { return called }, "RunSSH 未在后台任务中被调用")
	if !called {
		t.Error("RunSSH was not invoked (run did not route to node)")
	}
	joined := strings.Join(sshArgs, " ")
	if !strings.Contains(joined, "127.0.0.1") {
		t.Errorf("ssh args missing node host: %v", sshArgs)
	}
	if !strings.Contains(joined, "go test") {
		t.Errorf("ssh args missing test command: %v", sshArgs)
	}
	if !strings.Contains(joined, "cd /srv/repos/echo &&") {
		t.Errorf("ssh args missing workDir cd: %v", sshArgs)
	}
}

// TestIntelRunCancel verifies a running test run can be cancelled: the async
// executor registers a cancel func per run, and POST .../cancel aborts it,
// driving the run to a terminal state.
func TestIntelRunCancel(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// A reachable node target (keeps reachable=true).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"go-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"linux-docker","workDir":"/srv/repos/echo"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	// RunSSH blocks until its ctx is cancelled, letting us cancel mid-run.
	old := envagent.RunSSH
	defer func() { envagent.RunSSH = old }()
	envagent.RunSSH = func(ctx context.Context, args ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"node":`+jsonInt(node.Node.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rec.Code, rec.Body.String())
	}
	var enq struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enq); err != nil || enq.Run.ID == 0 {
		t.Fatalf("run enqueue parse: %s", rec.Body.String())
	}
	// 等 run 进入 running（cancel 注册完成）再取消。
	waitIntelCondition(t, func() bool {
		rec := s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(enq.Run.ID), "", wh)
		var resp struct {
			Run struct {
				Status string `json:"status"`
			} `json:"run"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Run.Status == "running"
	}, "run 未进入 running 状态")

	rec = s.do(t, http.MethodPost, "/api/intel/runs/"+jsonInt(enq.Run.ID)+"/cancel", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status %d: %s", rec.Code, rec.Body.String())
	}
	status := waitIntelRunFinished(t, s, wh, enq.Run.ID)
	if status != "failed" {
		t.Fatalf("cancelled run status = %q, want failed", status)
	}
	// 已结束的 run 再次取消应 404。
	rec = s.do(t, http.MethodPost, "/api/intel/runs/"+jsonInt(enq.Run.ID)+"/cancel", "", wh)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-cancel status %d, want 404", rec.Code)
	}
}

// TestIntelResultRootcause verifies the per-case attribution endpoint: a stored
// root-cause report is returned parsed alongside the result.
func TestIntelResultRootcause(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	ctx := context.Background()
	if err := s.store.AddIntelTestResults(ctx, []*store.TestResult{{
		RunID:         1,
		Endpoint:      "pkg.TestFoo",
		Passed:        false,
		FailuresJSON:  `{"suite":"pkg"}`,
		RootcauseJSON: `{"type":"panic","hint":"nil pointer dereference"}`,
	}}); err != nil {
		t.Fatalf("add result: %v", err)
	}
	results, err := s.store.ListIntelTestResults(ctx, 1)
	if err != nil || len(results) != 1 {
		t.Fatalf("list results: %v len=%d", err, len(results))
	}

	rec := s.do(t, http.MethodGet, "/api/intel/results/"+jsonInt(results[0].ID)+"/rootcause", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("rootcause status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			Endpoint string `json:"endpoint"`
			Passed   bool   `json:"passed"`
		} `json:"result"`
		Rootcause map[string]any `json:"rootcause"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("rootcause parse: %v", err)
	}
	if resp.Result.Endpoint != "pkg.TestFoo" || resp.Result.Passed {
		t.Errorf("result = %+v", resp.Result)
	}
	if resp.Rootcause["hint"] != "nil pointer dereference" {
		t.Errorf("rootcause = %+v", resp.Rootcause)
	}
}

// TestIntelRunAll verifies the one-click regression: every module's test
// command is executed and aggregated (a trivial go module passes with zero
// cases since there are no test functions).
func TestIntelRunAll(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run-all",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run-all status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Run.ID == 0 {
		t.Fatalf("run-all parse: %s", rec.Body.String())
	}
	// 异步聚合执行：等待 run 进入终态，再拉 runs 列表校验模块级状态。
	status := waitIntelRunFinished(t, s, wh, resp.Run.ID)
	if status != "passed" {
		t.Fatalf("run-all status = %q, want passed", status)
	}
	rec = s.do(t, http.MethodGet, "/api/intel/runs?projectId="+jsonInt(proj.ID), "", wh)
	var runs struct {
		Runs []struct {
			Scope  string `json:"scope"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("runs parse: %v", err)
	}
	moduleRuns := 0
	for _, r := range runs.Runs {
		if r.Scope == "module" {
			moduleRuns++
			if r.Status != "passed" {
				t.Errorf("module run status = %q, want passed", r.Status)
			}
		}
	}
	if moduleRuns != 1 {
		t.Errorf("module runs = %d, want 1", moduleRuns)
	}
}

// TestIntelRunAllFailingTests covers the run-all path for a suite that fails,
// which the old code got wrong twice over: a non-zero exit (how `go test`
// reports a failing case) short-circuited report parsing, so no per-case result
// was stored, and the aggregate run was then hardcoded "passed".
func TestIntelRunAllFailingTests(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module demo\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")
	writeTestFile(t, filepath.Join(root, "main_test.go"),
		"package main\n\nimport \"testing\"\n\nfunc TestBoom(t *testing.T) {\n\tt.Fatal(\"boom\")\n}\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run-all",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run-all status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Run.ID == 0 {
		t.Fatalf("run-all parse: %s", rec.Body.String())
	}
	if status := waitIntelRunFinished(t, s, wh, resp.Run.ID); status != "failed" {
		t.Fatalf("aggregate run status = %q, want failed", status)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/runs?projectId="+jsonInt(proj.ID), "", wh)
	var runs struct {
		Runs []struct {
			ID     int64  `json:"id"`
			Scope  string `json:"scope"`
			Status string `json:"status"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("runs parse: %v", err)
	}
	var moduleRunID int64
	for _, r := range runs.Runs {
		if r.Scope == "module" {
			moduleRunID = r.ID
			if r.Status != "failed" {
				t.Errorf("module run status = %q, want failed", r.Status)
			}
		}
	}
	if moduleRunID == 0 {
		t.Fatal("no module run recorded")
	}

	rec = s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(moduleRunID), "", wh)
	var detail struct {
		Results []struct {
			Kind     string `json:"kind"`
			Endpoint string `json:"endpoint"`
			Passed   bool   `json:"passed"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("run detail parse: %v", err)
	}
	failedCases := 0
	for _, r := range detail.Results {
		if !r.Passed {
			failedCases++
		}
	}
	if failedCases == 0 {
		t.Fatalf("failing suite produced %d failed case(s): %s", len(detail.Results), rec.Body.String())
	}
}

// TestSplitEndpointAndFlaky covers the per-case outcome mapping helpers.
func TestSplitEndpointAndFlaky(t *testing.T) {
	class, method := splitEndpoint("pkg.Class.TestFoo")
	if class != "pkg.Class" || method != "TestFoo" {
		t.Errorf("splitEndpoint(pkg.Class.TestFoo) = %q/%q", class, method)
	}
	class, method = splitEndpoint(".TestBar")
	if class != "" || method != "TestBar" {
		t.Errorf("splitEndpoint(.TestBar) = %q/%q", class, method)
	}
	if !isFlakyResult(`{"flaky":true,"note":"x"}`) {
		t.Error("flaky marker should be detected")
	}
	if isFlakyResult(`{"error":"boom"}`) {
		t.Error("non-flaky failure must not be flagged")
	}
	if isFlakyResult("") {
		t.Error("empty failures not flaky")
	}
}

// TestHasShellMeta covers the remote-command injection guard.
func TestHasShellMeta(t *testing.T) {
	safe := []string{"go", "test", "./...", "mvn", "test", "npm", "test", "pkg/foo", "-run", "TestX", "a-b_c"}
	for _, s := range safe {
		if hasShellMeta(s) {
			t.Errorf("hasShellMeta(%q) = true, want false", s)
		}
	}
	unsafe := []string{"test; rm -rf /", "a|b", "a>b", "a$(id)", "a`id`", "a&b", "a'", "a\"b", "$PATH", "a\\b"}
	for _, s := range unsafe {
		if !hasShellMeta(s) {
			t.Errorf("hasShellMeta(%q) = false, want true", s)
		}
	}
}
