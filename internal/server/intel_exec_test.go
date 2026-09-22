package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/store"
	"github.com/hiylo/starburst-backend/internal/tasks"
)

// startTestIntelRunner wires a shared-executor worker pool that claims
// kind=test-run tasks (the intel run-all and single-module mirrors) and drives
// them through the server's RunIntelTask dispatcher callback. Both run-all and
// single-module runs are executor-driven, so these tests must run the executor
// for the run to actually execute. The pool's prompt workers find no prompt
// tasks here and only idle, and retries are disabled so a failing run fails the
// mirror deterministically without re-running the suite.
func startTestIntelRunner(t *testing.T, s *Server) {
	t.Helper()
	exec := tasks.NewExecutor(s.store, s.hub, "http://127.0.0.1:1")
	exec.WithIntelRunner(s.RunIntelTask)
	exec.WithMaxRetries(0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go exec.Run(ctx)
}

// findIntelTrackingTask polls the task list for a kind=test-run tracking task
// whose instruction references runID, waiting for it to reach a terminal
// status. The association lives in the Prompt: CreateTask does not persist the
// pre-set Result column (the executor writes the completion summary into it),
// so the instruction JSON is parsed to match the run.
func findIntelTrackingTask(t *testing.T, s *Server, runID int64) *store.Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tasks, _ := s.store.ListTasks(context.Background(), "", 200, 0)
		for _, tk := range tasks {
			if tk == nil || tk.Kind != "test-run" {
				continue
			}
			var instr intelRunTaskInstr
			if json.Unmarshal([]byte(tk.Prompt), &instr) != nil || instr.RunID != runID {
				continue
			}
			if tk.Status != store.TaskQueued && tk.Status != store.TaskRunning {
				return tk
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

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
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "go", "go", results)
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

	// Report kinds without a deterministic re-run selector never re-execute
	// (xctest without a scheme / npm).
	calls = 0
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "xctest", "xcode", results)
	if calls != 0 {
		t.Errorf("xctest triggered rerun (%d calls)", calls)
	}

	// Rerun still fails -> stays failed.
	runCmd = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		return []byte(`{"Action":"fail","Test":"TestFoo","Package":"pkg","Output":"boom"}
`), nil
	}
	results2 := []*store.TestResult{{Endpoint: ".TestFoo", Kind: "go", Passed: false}}
	s.flakyRetry(context.Background(), 0, 0, "/tmp", "go", "go", results2)
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
	startTestIntelRunner(t, s)

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
		`{"name":"go-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"go linux-docker","workDir":"/srv/repos/echo"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	called := false
	var sshArgs []string
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
		called = true
		sshArgs = []string{host, strconv.Itoa(port), user, command}
		return `{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`, nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"node":`+jsonInt(node.Node.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote run status %d: %s", rec.Code, rec.Body.String())
	}
	// 异步执行：等待后台任务真正路由到 RunCommandOn。
	waitIntelCondition(t, func() bool { return called }, "RunCommandOn 未在后台任务中被调用")
	if !called {
		t.Error("RunCommandOn was not invoked (run did not route to node)")
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
	startTestIntelRunner(t, s)

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
		`{"name":"go-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"go linux-docker","workDir":"/srv/repos/echo"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	// RunCommandOn blocks until its ctx is cancelled, letting us cancel mid-run.
	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
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
	startTestIntelRunner(t, s)

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
	startTestIntelRunner(t, s)

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

// TestHasShellMeta covers the remote-command injection guard: any whitespace /
// C0 control character (directory names carrying a newline or space shift shell
// word boundaries) plus the classic metacharacters must be rejected.
func TestHasShellMeta(t *testing.T) {
	safe := []string{"go", "test", "./...", "mvn", "test", "npm", "test", "pkg/foo", "-run", "TestX", "a-b_c"}
	for _, s := range safe {
		if hasShellMeta(s) {
			t.Errorf("hasShellMeta(%q) = true, want false", s)
		}
	}
	unsafe := []string{"test; rm -rf /", "a|b", "a>b", "a$(id)", "a`id`", "a&b", "a'", "a\"b", "$PATH", "a\\b",
		"a b", "dir\nname", "dir\rname", "dir\tname", "a\vname", "dir\x1fname", "a\x7fname"}
	for _, s := range unsafe {
		if !hasShellMeta(s) {
			t.Errorf("hasShellMeta(%q) = false, want true", s)
		}
	}
}

// TestHasShellMetaCommand covers the whole-command backstop: the separator
// spaces and `&&` we insert are allowed, everything else (control chars,
// metachars) still rejected.
func TestHasShellMetaCommand(t *testing.T) {
	ok := []string{"cd /srv/repos/demo && go test -json -count=1 ./...",
		"cd /home/runner/app && mvn test", "cd /srv && npm test"}
	for _, s := range ok {
		if hasShellMetaCommand(s) {
			t.Errorf("hasShellMetaCommand(%q) = true, want false", s)
		}
	}
	bad := []string{"cd /srv\n&& rm -rf /", "cd /srv && rm -rf /; echo x",
		"cd /srv && true && false && a=b && $(id)", "cd x && y <- z"}
	for _, s := range bad {
		if !hasShellMetaCommand(s) {
			t.Errorf("hasShellMetaCommand(%q) = false, want true", s)
		}
	}
}

// TestRemoteCapabilityRouting verifies the deterministic build-tool→capability
// mapping and the label matcher used to route runs to capability-annotated
// nodes.
func TestRemoteCapabilityRouting(t *testing.T) {
	mapping := []struct {
		buildTool, kindType, want string
	}{
		{"go", "", "go"},
		{"maven", "", "java"},
		{"gradle", "", "gradle"},
		{"gradle", "android", "android"},
		{"npm", "web", "node"},
		{"pytest", "", "python"},
		{"xcode", "", "ios"},
		{"swiftpm", "", ""},
	}
	for _, c := range mapping {
		if got := remoteCapabilityFor(c.buildTool, c.kindType); got != c.want {
			t.Errorf("remoteCapabilityFor(%q,%q) = %q, want %q", c.buildTool, c.kindType, got, c.want)
		}
	}

	matches := []struct {
		caps, required string
		want           bool
	}{
		{"linux-docker", "go", false},
		{"go linux-docker", "go", true},
		{"go,linux-docker", "go", true},
		{"android-sdk", "android", true},
		{"android-sdk ios-xcode", "ios", true},
		{"java maven", "java", true},
		{"ios-xcode", "go", false},
		{"", "go", false},
		{"  ", "go", false},
	}
	for _, m := range matches {
		if got := nodeHasCapability(m.caps, m.required); got != m.want {
			t.Errorf("nodeHasCapability(%q,%q) = %v, want %v", m.caps, m.required, got, m.want)
		}
	}
}

// TestRemoteReportPullParsesSurefire verifies the report pull-back plumbing: a
// remote surefire TEST-*.xml fetched as a path->bytes map materializes into a
// temp dir that parseReport then reads into per-case results.
func TestRemoteReportPullParsesSurefire(t *testing.T) {
	old := envagent.PullArtifacts
	defer func() { envagent.PullArtifacts = old }()
	var gotAuth string
	envagent.PullArtifacts = func(ctx context.Context, host, user string, port int, authText, workDir string, relPaths []string) (map[string][]byte, error) {
		gotAuth = authText
		if len(relPaths) != 1 || relPaths[0] != "target/surefire-reports" {
			t.Errorf("relPaths = %v, want [target/surefire-reports]", relPaths)
		}
		return map[string][]byte{
			"target/surefire-reports/TEST-demo.UserTest.xml": []byte(`<testsuite name="demo.UserTest" tests="2">
  <testcase classname="demo.UserTest" name="ok" time="0.01"/>
  <testcase classname="demo.UserTest" name="boom" time="0.02"><failure message="boom">boom stack</failure></testcase>
</testsuite>`),
		}, nil
	}

	// 拉报告必须透传节点 Auth：仅接受节点凭据的节点不能回退本机默认 key。
	node := &store.RemoteNode{Host: "192.0.2.10", User: "runner", Port: 22, Auth: "node-secret"}
	pulled, err := pullRemoteReports(context.Background(), node, "/srv/demo", "surefire")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(pulled)
	if gotAuth != "node-secret" {
		t.Errorf("pull auth = %q, want node-secret", gotAuth)
	}
	if pulled == "" {
		t.Fatal("surefire must pull into a temp dir")
	}
	results := parseReport("surefire", pulled, nil)
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	byName := map[string]*store.TestResult{}
	for _, r := range results {
		byName[r.Endpoint] = r
	}
	if !byName["demo.UserTest.ok"].Passed {
		t.Error("ok case should pass")
	}
	if byName["demo.UserTest.boom"].Passed {
		t.Error("boom case should fail")
	}
}

// TestIntelRunRemoteEndRepoWD verifies the @end/ multi-repo remote path
// alignment: a module that lives in an associated source repo maps its remote
// working directory to node WorkDir + the repo-relative module path, and the
// SSH command runs with that aligned `cd`.
func TestIntelRunRemoteEndRepoWD(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

	// A reachable node target (keeps reachable=true).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	mainRoot := t.TempDir()
	writeTestFile(t, filepath.Join(mainRoot, "pom.xml"), `<project></project>`)
	endRoot := t.TempDir()
	writeTestFile(t, filepath.Join(endRoot, "core", "go.mod"), "module demo\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(endRoot, "core", "main.go"), "package main\nfunc main() {}\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"multi","source":"local","localPath":"`+filepath.ToSlash(mainRoot)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"end","source":"local","localPath":"`+filepath.ToSlash(endRoot)+`"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sources status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Locate the "@end/core" module id.
	var endModID int64
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Modules []struct {
			ID      int64  `json:"id"`
			RelPath string `json:"relPath"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	for _, m := range detail.Modules {
		if m.RelPath == "@end/core" {
			endModID = m.ID
		}
	}
	if endModID == 0 {
		t.Fatalf("@end/core module not found: %+v", detail.Modules)
	}

	// Node workDir is the *associated repo checkout root* on the node.
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"end-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"go linux-docker","workDir":"/srv/end"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	var sshArgs []string
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
		sshArgs = []string{host, strconv.Itoa(port), user, command}
		return `{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`, nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"moduleId":`+jsonInt(endModID)+`,"node":`+jsonInt(node.Node.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("remote run status %d: %s", rec.Code, rec.Body.String())
	}
	waitIntelCondition(t, func() bool { return len(sshArgs) > 0 }, "RunCommandOn 未在后台任务中被调用")
	joined := strings.Join(sshArgs, " ")
	// The module's repo-relative path ("core") must be appended to WorkDir.
	if !strings.Contains(joined, "cd /srv/end/core &&") {
		t.Errorf("ssh args workDir not aligned to associated repo subdir: %v", sshArgs)
	}
	if !strings.Contains(joined, "go test") {
		t.Errorf("ssh args missing test command: %v", sshArgs)
	}
}

// TestIntelRunAutoPickNode verifies capability-based auto node selection:
// with nodeID=0 and a reachable node declaring the matching capability, the run
// routes over SSH to that node's workDir instead of running locally.
func TestIntelRunAutoPickNode(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

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
		`{"name":"go-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"go linux-docker","workDir":"/srv/go"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	var sshArgs []string
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
		sshArgs = []string{host, strconv.Itoa(port), user, command}
		return `{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`, nil
	}

	// No node specified: auto-pick must route the go module to the go node.
	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-pick run status %d: %s", rec.Code, rec.Body.String())
	}
	waitIntelCondition(t, func() bool { return len(sshArgs) > 0 }, "RunCommandOn 未在后台任务中被调用（未路由到自动选择的节点）")
	joined := strings.Join(sshArgs, " ")
	if !strings.Contains(joined, "cd /srv/go &&") {
		t.Errorf("ssh args workDir wrong: %v", sshArgs)
	}
	if !strings.Contains(joined, "go test") {
		t.Errorf("ssh args missing test command: %v", sshArgs)
	}
}

// TestIntelRunAutoPickFallbackLocal verifies that when no reachable node
// declares the required capability, a nodeID=0 run falls back to local
// execution (runCmd is invoked, RunSSH never). A reachable but
// capability-mismatched node and a capability-matching but unreachable node
// must both be skipped by the picker.
func TestIntelRunAutoPickFallbackLocal(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

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

	// A reachable node with the WRONG capability (skipped by the picker).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"python-runner","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"python","workDir":"/srv/py"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create python node: %s", rec.Body.String())
	}
	// A capability-matching node that is NOT reachable (closed port): skipped.
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"go-dead","host":"127.0.0.1","port":`+jsonInt(int64(deadPort))+`,"capabilities":"go","workDir":"/srv/go"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create dead node: %s", rec.Body.String())
	}
	var deadNode struct {
		Node struct {
			Reachable bool `json:"reachable"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deadNode); err != nil {
		t.Fatalf("dead node parse: %v", err)
	}
	if deadNode.Node.Reachable {
		t.Fatal("closed-port node should be recorded unreachable")
	}

	oldCmd := runCmd
	defer func() { runCmd = oldCmd }()
	called := false
	runCmd = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		called = true
		return []byte(`{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`), nil
	}
	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	sshCalled := false
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
		sshCalled = true
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rec.Code, rec.Body.String())
	}
	var enq struct {
		Run struct {
			ID int64 `json:"id"`
		} `json:"run"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &enq)
	waitIntelCondition(t, func() bool { return called }, "本地回退未调用 runCmd")
	if sshCalled {
		t.Error("RunCommandOn 不应被调用（无匹配节点应回退本地）")
	}
	if status := waitIntelRunFinished(t, s, wh, enq.Run.ID); status != "passed" {
		t.Fatalf("run status = %q, want passed", status)
	}
}

// TestIntelRunRemoteSurefire verifies §11 偏差#2: a non-go report kind runs on
// a remote node — the test command is driven over SSH and the on-disk surefire
// XML is pulled back (fake tar on the stub ssh) and parsed into per-case
// results instead of being rejected.
func TestIntelRunRemoteSurefire(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>demo</groupId><artifactId>demo</artifactId><version>1.0</version>
</project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/Main.java"), `package demo;
public class Main { public static void main(String[] a) {} }`)
	writeTestFile(t, filepath.Join(root, "src/test/java/demo/UserTest.java"), `package demo;
import org.junit.Test;
public class UserTest {
    @Test public void ok() {}
    @Test public void boom() { throw new RuntimeException("boom"); }
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
	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"java-ci","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"java linux-docker","workDir":"/srv/repos/demo"}`, wh)
	var node struct {
		Node struct {
			ID int64 `json:"id"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &node); err != nil || node.Node.ID == 0 {
		t.Fatalf("create node: %s", rec.Body.String())
	}

	old := envagent.RunCommandOn
	defer func() { envagent.RunCommandOn = old }()
	oldPull := envagent.PullArtifacts
	defer func() { envagent.PullArtifacts = oldPull }()
	surefireXML := `<testsuite name="demo.UserTest" tests="2">
  <testcase classname="demo.UserTest" name="ok" time="0.01"/>
  <testcase classname="demo.UserTest" name="boom" time="0.02"><failure message="boom">boom stack</failure></testcase>
</testsuite>`
	envagent.RunCommandOn = func(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
		return "BUILD SUCCESS", nil
	}
	envagent.PullArtifacts = func(ctx context.Context, host, user string, port int, authText, workDir string, relPaths []string) (map[string][]byte, error) {
		if len(relPaths) != 1 || relPaths[0] != "target/surefire-reports" {
			t.Errorf("relPaths = %v, want [target/surefire-reports]", relPaths)
		}
		return map[string][]byte{
			"target/surefire-reports/TEST-demo.UserTest.xml": []byte(surefireXML),
		}, nil
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
	if status := waitIntelRunFinished(t, s, wh, enq.Run.ID); status != "failed" {
		t.Fatalf("run status = %q, want failed (boom case)", status)
	}
	rec = s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(enq.Run.ID), "", wh)
	var detail struct {
		Results []struct {
			Endpoint string `json:"endpoint"`
			Passed   bool   `json:"passed"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("run detail parse: %v", err)
	}
	passed, failed := 0, 0
	for _, r := range detail.Results {
		if r.Passed {
			passed++
		} else {
			failed++
		}
	}
	if passed != 1 || failed != 1 {
		t.Fatalf("results = %d passed / %d failed, want 1/1: %s", passed, failed, rec.Body.String())
	}
}

// TestIntelModuleRunViaExecutor verifies the single-module run is executed by
// the shared executor's test-run worker (no standalone goroutine): the run
// reaches "passed" and a kind=test-run tracking task is recorded as succeeded
// with the run id in Result and the module-suffixed Name.
func TestIntelModuleRunViaExecutor(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

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
	ctx := context.Background()
	mods, err := s.store.ListIntelModules(ctx, proj.ID)
	if err != nil || len(mods) == 0 {
		t.Fatalf("list modules: %v len=%d", err, len(mods))
	}

	// force bypasses the env gate so the test-run worker can drive the run
	// regardless of the local toolchain gate.
	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"moduleId":`+jsonInt(mods[0].ID)+`,"force":true}`, wh)
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
	if status := waitIntelRunFinished(t, s, wh, enq.Run.ID); status != "passed" {
		t.Fatalf("module run status = %q, want passed", status)
	}

	// The run is mirrored on a kind=test-run tracking task that the worker
	// completed. The single-module task carries the module-suffixed Name; the
	// applied run id lives in the instruction Prompt (CreateTask does not
	// persist the pre-set Result column, only the completion summary).
	got := findIntelTrackingTask(t, s, enq.Run.ID)
	if got == nil {
		t.Fatalf("no kind=test-run tracking task for run %d", enq.Run.ID)
	}
	if got.Name != "智能测试 · 模块" {
		t.Errorf("task name = %q, want %q", got.Name, "智能测试 · 模块")
	}
	if got.Status != store.TaskSucceeded {
		t.Errorf("task status = %q, want succeeded", got.Status)
	}
}

// TestIntelModuleRunFailingTests covers the single-module path for a suite
// that fails: the run row reaches "failed" with per-case results recorded, and
// the tracking task is marked succeeded (the module callback never returns the
// execution outcome as an error — see RunIntelModuleTask).
func TestIntelModuleRunFailingTests(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)
	startTestIntelRunner(t, s)

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

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"force":true}`, wh)
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
	if status := waitIntelRunFinished(t, s, wh, enq.Run.ID); status != "failed" {
		t.Fatalf("module run status = %q, want failed", status)
	}

	// Per-case results are recorded against the run.
	rec = s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(enq.Run.ID), "", wh)
	var detail struct {
		Results []struct {
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
		t.Fatalf("expected at least one failed case: %s", rec.Body.String())
	}

	// The tracking task is succeeded (module callback swallows the outcome).
	got := findIntelTrackingTask(t, s, enq.Run.ID)
	if got == nil {
		t.Fatalf("no kind=test-run tracking task for run %d", enq.Run.ID)
	}
	if got.Status != store.TaskSucceeded {
		t.Errorf("tracking task status = %q, want succeeded (outcome swallowed)", got.Status)
	}
}

// TestIntelModuleRunSyncRetry exercises the runIntelJobSync internal retry: an
// execution failure (command error) on the first attempt is retried with a
// fresh run row up to intelMaxRunAttempts. The original attempt is marked
// failed and the retry row carries the passing outcome. It also documents that
// the executor's own retry would double this, which is why RunIntelModuleTask
// never returns the outcome as an error.
func TestIntelModuleRunSyncRetry(t *testing.T) {
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
	ctx := context.Background()
	mods, err := s.store.ListIntelModules(ctx, proj.ID)
	if err != nil || len(mods) == 0 {
		t.Fatalf("list modules: %v len=%d", err, len(mods))
	}

	// Create a queued run row directly (bypassing enqueueIntelRun's task
	// creation) so runIntelJobSync is exercised in isolation.
	run := &store.TestRun{
		ProjectID: proj.ID,
		ModuleID:  mods[0].ID,
		Scope:     "module",
		Status:    "queued",
		Progress:  "排队中",
	}
	if err := s.store.CreateIntelTestRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// runCmd: first call errors (command spawn failure), second call returns
	// passing go-test JSON. flakyRetry finds no failing case on the retry, so
	// there is no further re-run.
	old := runCmd
	defer func() { runCmd = old }()
	calls := 0
	runCmd = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("go: command not found")
		}
		return []byte(`{"Action":"pass","Test":"TestPing","Package":"demo","Elapsed":0.01}
`), nil
	}

	if err := s.runIntelJobSync(ctx, proj.ID, mods[0].ID, 0, true, run); err != nil {
		t.Fatalf("runIntelJobSync returned error: %v", err)
	}
	if calls != 2 {
		t.Errorf("runCmd calls = %d, want 2 (attempt + retry)", calls)
	}

	runs, err := s.store.ListIntelTestRuns(ctx, proj.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	var orig, retry *store.TestRun
	for _, r := range runs {
		if r.ID == run.ID {
			orig = r
		} else {
			retry = r
		}
	}
	if orig == nil {
		t.Fatal("original run row not found")
	}
	if orig.Status != "failed" {
		t.Errorf("original run status = %q, want failed (execution failed, retried)", orig.Status)
	}
	if !strings.Contains(orig.Progress, "自动重试") {
		t.Errorf("original run progress = %q, want retry note", orig.Progress)
	}
	if retry == nil {
		t.Fatal("retry run row not created")
	}
	if retry.Status != "passed" {
		t.Errorf("retry run status = %q, want passed", retry.Status)
	}
}

// TestIntelRunTaskInstrRoundTrip covers the instruction serialization: a
// single-module instruction round-trips moduleId/nodeId; a run-all instruction
// (no module/node) unmarshals with nil ModuleID so the dispatcher routes to
// run-all; a root-module run carries moduleId=0 (pointer non-nil) so it still
// routes to the module pipeline.
func TestIntelRunTaskInstrRoundTrip(t *testing.T) {
	mid := int64(7)
	nid := int64(3)
	src := intelRunTaskInstr{ProjectID: 1, RunID: 2, Force: true, ModuleID: &mid, NodeID: &nid}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	var got intelRunTaskInstr
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != 1 || got.RunID != 2 || !got.Force {
		t.Errorf("round-trip base fields wrong: %+v", got)
	}
	if got.ModuleID == nil || *got.ModuleID != 7 {
		t.Errorf("round-trip moduleId = %v, want 7", got.ModuleID)
	}
	if got.NodeID == nil || *got.NodeID != 3 {
		t.Errorf("round-trip nodeId = %v, want 3", got.NodeID)
	}

	// A run-all instruction (as produced before the module fields existed)
	// unmarshals with nil ModuleID/NodeID.
	var all intelRunTaskInstr
	if err := json.Unmarshal([]byte(`{"projectId":9,"runId":5,"force":false}`), &all); err != nil {
		t.Fatal(err)
	}
	if all.ModuleID != nil || all.NodeID != nil {
		t.Errorf("run-all instr has module/node: %+v", all)
	}

	// A root-module run (moduleId=0) keeps a non-nil pointer so the dispatcher
	// routes it to the module pipeline, not run-all.
	var root intelRunTaskInstr
	if err := json.Unmarshal([]byte(`{"projectId":9,"runId":5,"force":false,"moduleId":0}`), &root); err != nil {
		t.Fatal(err)
	}
	if root.ModuleID == nil {
		t.Error("root-module instruction has nil moduleId (would misroute to run-all)")
	}
	if *root.ModuleID != 0 {
		t.Errorf("root-module moduleId = %d, want 0", *root.ModuleID)
	}
}
