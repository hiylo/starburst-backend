package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/intel/report"
	"github.com/hiylo/starburst-backend/internal/intel/rootcause"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelTestCases lists discovered test cases for a project (and optional
// module).
func (s *Server) handleIntelTestCases(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	moduleID := intQuery(r, "moduleId")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cases, err := s.store.ListIntelTestCases(ctx, projectID, moduleID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load test cases failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"testCases": cases})
}

// handleIntelFeatures lists (GET) or creates (POST) feature points for a
// project. Manual features are stamped source=manual and survive rescans.
func (s *Server) handleIntelFeatures(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		projectID, ok := s.intelQueryProject(w, r)
		if !ok {
			return
		}
		feats, err := s.store.ListIntelFeatures(ctx, projectID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load features failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"features": feats})
	case http.MethodPost:
		projectID, ok := s.intelQueryProject(w, r)
		if !ok {
			return
		}
		var req struct {
			Name   string   `json:"name"`
			Ends   []string `json:"ends"`
			Anchor string   `json:"anchor"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeErr(w, http.StatusBadRequest, "name is required")
			return
		}
		ends, _ := json.Marshal(req.Ends)
		feat := &store.IntelFeature{
			ProjectID: projectID,
			Name:      strings.TrimSpace(req.Name),
			Summary:   "",
			EndsJSON:  string(ends),
			Source:    "manual",
			Anchor:    req.Anchor,
			Status:    "active",
		}
		if err := s.store.CreateIntelFeature(ctx, feat); err != nil {
			writeErr(w, http.StatusInternalServerError, "create feature failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"feature": feat})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelIssues lists issues for a project, optionally narrowed by status.
func (s *Server) handleIntelIssues(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	issues, err := s.store.ListIntelIssues(ctx, projectID, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load issues failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"issues": issues})
}

// handleIntelRun triggers a synchronous test run (M3: deterministic command per
// build tool, report parsing, results persisted). Async worker-pool execution
// lands with the execution milestone.
func (s *Server) handleIntelRun(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64 `json:"projectId"`
		ModuleID  int64 `json:"moduleId"`
		NodeID    int64 `json:"node"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	run, err := s.runIntelTests(ctx, req.ProjectID, req.ModuleID, req.NodeID)
	if err != nil {
		log.Printf("intel run project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "run failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// handleIntelRuns lists test runs for a project, newest first.
func (s *Server) handleIntelRuns(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	runs, err := s.store.ListIntelTestRuns(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load runs failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleIntelRunByID returns one run with its per-case results.
func (s *Server) handleIntelRunByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := intelPathID(w, r, "/api/intel/runs/")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	run, err := s.store.GetIntelTestRun(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return
	}
	results, err := s.store.ListIntelTestResults(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load results failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "results": results})
}

// runIntelTests executes the deterministic test command for a project/module,
// parses the framework report and persists the run + per-case results, turning
// failures into intel_issues. When nodeID > 0 the command is routed over SSH to
// a remote execution node instead of the local machine. It returns the run.
func (s *Server) runIntelTests(ctx context.Context, projectID, moduleID, nodeID int64) (*store.TestRun, error) {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, err
	}

	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return nil, err
	}
	module, ok := pickIntelModule(mods, moduleID)
	if !ok {
		return nil, fmt.Errorf("module %d not found", moduleID)
	}

	dir := filepath.Join(root, module.RelPath)
	if module.RelPath == "." {
		dir = root
	}
	cmdArgs, reportKind := testCommandFor(module.BuildTool, module.KindType)
	if len(cmdArgs) == 0 {
		return nil, fmt.Errorf("unsupported build tool %q for module %s", module.BuildTool, module.RelPath)
	}
	// Honor the human-reviewed command whitelist (commands_json) when it offers
	// a matching test command; the whitelist is parsed into argv (no shell), so
	// edited entries cannot inject shell metacharacters.
	if wl := whitelistedTestCommand(module.CommandsJSON, module.BuildTool, module.KindType); wl != nil {
		cmdArgs = wl
	}

	// Execution target: local (default) or a remote SSH node when specified.
	remoteNode := (*store.RemoteNode)(nil)
	if nodeID > 0 {
		node, err := s.store.GetRemoteNode(ctx, nodeID)
		if err != nil {
			return nil, fmt.Errorf("remote node %d not found", nodeID)
		}
		if !node.Reachable {
			return nil, fmt.Errorf("remote node %q 不可达（请先在节点列表检查）", node.Name)
		}
		if reportKind != "go" {
			return nil, fmt.Errorf("远程执行目前仅支持 go 报告（stdout 自包含）；%s 需本机运行", reportKind)
		}
		remoteNode = node
	} else if err := s.envGate(ctx, projectID); err != nil {
		// Environment gate (§3.6): reject the run before executing when required
		// middleware/toolchains are missing, with a per-item list. Local runs are
		// gated; routed runs rely on the node's capability labels instead.
		return nil, err
	}

	now := time.Now()
	run := &store.TestRun{
		ProjectID: projectID,
		ModuleID:  module.ID,
		Scope:     "module",
		Kind:      reportKind,
		Command:   strings.Join(cmdArgs, " "),
		Status:    "running",
		StartedAt: &now,
	}
	if err := s.store.CreateIntelTestRun(ctx, run); err != nil {
		return nil, err
	}

	var output []byte
	var execErr error
	if remoteNode != nil {
		command := strings.Join(cmdArgs, " ")
		if !strings.HasPrefix(strings.TrimSpace(command), "cd ") {
			command = "cd ~ && " + command
		}
		sshArgs := envagent.SSHCommandArgs(remoteNode.Host, remoteNode.User, remoteNode.Port, command)
		var out string
		out, execErr = envagent.RunSSH(ctx, sshArgs...)
		output = []byte(out)
	} else {
		output, execErr = runCommand(ctx, dir, cmdArgs[0], cmdArgs[1:]...)
	}
	finished := time.Now()
	if execErr != nil {
		run.Status = "failed"
		run.FinishedAt = &finished
		_ = s.store.UpdateIntelTestRun(ctx, run)
		return nil, fmt.Errorf("test command failed: %w", execErr)
	}

	results := parseReport(reportKind, dir, output)
	flakyRetry(ctx, dir, reportKind, results)
	if err := s.store.AddIntelTestResults(ctx, results); err != nil {
		return nil, err
	}
	s.recordRunIssues(ctx, run, projectID, module.ID, results)

	failed := 0
	for _, res := range results {
		if !res.Passed {
			failed++
		}
	}
	if failed > 0 {
		run.Status = "failed"
	} else {
		run.Status = "passed"
	}
	run.FinishedAt = &finished
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// testCommandFor returns the deterministic test command + report kind for a
// build tool. The command is fixed per tool (whitelist-reviewed in a later
// milestone); it never accepts arbitrary user input.
func testCommandFor(buildTool, kindType string) ([]string, string) {
	switch buildTool {
	case "go":
		return []string{"go", "test", "-json", "./..."}, "go"
	case "maven":
		return []string{"mvn", "test"}, "surefire"
	case "gradle":
		if kindType == "android" {
			return []string{"./gradlew", "testDebugUnitTest"}, "surefire"
		}
		return []string{"./gradlew", "test"}, "surefire"
	}
	return nil, ""
}

// whitelistedTestCommand picks a test command from the module's reviewed
// command whitelist (commands_json, a JSON array of command strings). It
// returns the matching argv (split with strings.Fields, never a shell) or nil
// to fall back to the tool default. Entries are matched by the tool's primary
// executable plus a "test" intent so a human-reviewed whitelist actually takes
// effect at run time.
func whitelistedTestCommand(commandsJSON, buildTool, kindType string) []string {
	if commandsJSON == "" {
		return nil
	}
	var wl []string
	if err := json.Unmarshal([]byte(commandsJSON), &wl); err != nil {
		return nil
	}
	for _, line := range wl {
		argv := strings.Fields(line)
		if len(argv) == 0 {
			continue
		}
		matched := false
		switch buildTool {
		case "go":
			matched = argv[0] == "go" && len(argv) >= 2 && argv[1] == "test"
		case "maven":
			matched = argv[0] == "mvn" && hasTestIntent(argv)
		case "gradle":
			matched = argv[0] == "./gradlew" && hasTestIntent(argv)
		case "npm":
			matched = argv[0] == "npm" && hasTestIntent(argv)
		}
		if matched {
			return argv
		}
	}
	return nil
}

// hasTestIntent reports whether any argument signals a test goal (mvn test,
// gradlew test, testDebugUnitTest, npm test ...).
func hasTestIntent(argv []string) bool {
	for _, a := range argv[1:] {
		if a == "test" || strings.HasPrefix(a, "test") {
			return true
		}
	}
	return false
}

// runCmd is the process runner used by the test flow (overridable in tests to
// stub the flaky-retry re-run).
var runCmd = runCommand

// flakyRetryMaxRetries is the fixed re-run budget for flaky detection: a case
// that fails on the first run but passes on a bounded retry is marked flaky
// (still recorded as passed) instead of becoming a bug issue.
const flakyRetryMaxRetries = 1

// flakyRetry re-runs the failed Go tests once (bounded by filter) and marks the
// results that then pass as flaky. Non-Go report kinds and build failures abort
// the retry deterministically (no unbounded re-execution).
func flakyRetry(ctx context.Context, dir, reportKind string, results []*store.TestResult) {
	if reportKind != "go" {
		return
	}
	failed := make([]*store.TestResult, 0)
	seen := map[string]bool{}
	names := make([]string, 0)
	for _, r := range results {
		if r == nil || r.Passed {
			continue
		}
		failed = append(failed, r)
		n := strings.TrimPrefix(r.Endpoint, ".")
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if len(failed) == 0 || len(names) == 0 {
		return
	}
	filter := "^(" + strings.Join(names, "|") + ")$"
	for attempt := 0; attempt < flakyRetryMaxRetries; attempt++ {
		out, err := runCmd(ctx, dir, "go", "test", "-count=1", "-run", filter, "./...")
		if err != nil {
			return // build/test command failure aborts flaky retry
		}
		rerun := parseReport("go", dir, out)
		passed := map[string]bool{}
		for _, r := range rerun {
			if r != nil && r.Passed {
				passed[strings.TrimPrefix(r.Endpoint, ".")] = true
			}
		}
		changed := false
		for _, r := range failed {
			n := strings.TrimPrefix(r.Endpoint, ".")
			if !passed[n] {
				continue
			}
			r.Passed = true
			r.FailuresJSON = encodeJSON(map[string]any{
				"flaky": true,
				"note":  fmt.Sprintf("首次失败，重跑通过（固定重试预算 %d 次）", flakyRetryMaxRetries),
			})
			changed = true
		}
		if changed {
			return
		}
	}
}

// runCommand runs an executable with a bounded timeout and returns stdout.
func runCommand(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// parseReport converts framework output into unified per-case results.
func parseReport(reportKind, dir string, output []byte) []*store.TestResult {
	var cases []report.CaseResult
	switch reportKind {
	case "go":
		cases, _ = report.ParseGoTestJSON(output)
	case "surefire":
		files, _ := filepath.Glob(filepath.Join(dir, "target", "surefire-reports", "TEST-*.xml"))
		for _, f := range files {
			if data, err := os.ReadFile(f); err == nil {
				c, _ := report.ParseSurefireXML(data)
				cases = append(cases, c...)
			}
		}
	}
	out := make([]*store.TestResult, 0, len(cases))
	for _, c := range cases {
		passed := c.Status == "passed"
		res := &store.TestResult{
			Kind:         reportKind,
			Passed:       passed,
			Endpoint:     c.Class + "." + c.Name,
			FailuresJSON: encodeJSON(map[string]any{"suite": c.Suite, "error": c.ErrorXML}),
		}
		if !passed {
			res.RootcauseJSON = encodeJSON(rootcause.Analyze(c.ErrorXML, nil))
		}
		out = append(out, res)
	}
	return out
}

// recordRunIssues turns failed results into intel_issues (closed-loop).
func (s *Server) recordRunIssues(ctx context.Context, run *store.TestRun, projectID, moduleID int64, results []*store.TestResult) {
	for _, res := range results {
		if res.Passed {
			continue
		}
		issue := &store.IntelIssue{
			ProjectID:  projectID,
			ModuleID:   moduleID,
			Key:        res.Endpoint,
			Kind:       "bug",
			Severity:   "medium",
			Location:   res.Endpoint,
			Status:     "open",
			DetailJSON: res.FailuresJSON,
		}
		if err := s.store.CreateIntelIssue(ctx, issue); err != nil {
			log.Printf("intel record issue: %v", err)
		}
	}
}

// pickIntelModule returns the requested module or the root module (rel ".") when
// moduleID is 0.
func pickIntelModule(mods []*store.IntelModule, moduleID int64) (*store.IntelModule, bool) {
	if moduleID > 0 {
		for _, m := range mods {
			if m.ID == moduleID {
				return m, true
			}
		}
		return nil, false
	}
	for _, m := range mods {
		if m.RelPath == "." {
			return m, true
		}
	}
	if len(mods) > 0 {
		return mods[0], true
	}
	return nil, false
}

func encodeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
