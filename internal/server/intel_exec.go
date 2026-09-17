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

// handleIntelFeatures lists feature points for a project.
func (s *Server) handleIntelFeatures(w http.ResponseWriter, r *http.Request) {
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
	feats, err := s.store.ListIntelFeatures(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load features failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"features": feats})
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
	run, err := s.runIntelTests(ctx, req.ProjectID, req.ModuleID)
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
// failures into intel_issues. It returns the persisted run.
func (s *Server) runIntelTests(ctx context.Context, projectID, moduleID int64) (*store.TestRun, error) {
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

	// Environment gate (§3.6): reject the run before executing when required
	// middleware/toolchains are missing, with a per-item list.
	if err := s.envGate(ctx, projectID); err != nil {
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

	output, err := runCommand(ctx, dir, cmdArgs[0], cmdArgs[1:]...)
	finished := time.Now()
	if err != nil {
		run.Status = "failed"
		run.FinishedAt = &finished
		_ = s.store.UpdateIntelTestRun(ctx, run)
		return nil, fmt.Errorf("test command failed: %w", err)
	}

	results := parseReport(reportKind, dir, output)
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
