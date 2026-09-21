package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	if r.Method == http.MethodPost {
		// 解除 flaky 隔离：把某个用例从「隔离区」移除，恢复参与后续回归。
		var req struct {
			ProjectID int64  `json:"projectId"`
			ModuleID  int64  `json:"moduleId"`
			Endpoint  string `json:"endpoint"`
		}
		if !readBody(w, r, &req) {
			return
		}
		class, method := splitEndpoint(req.Endpoint)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.store.UnquarantineIntelTestCase(ctx, req.ProjectID, req.ModuleID, class, method); err != nil {
			log.Printf("intel unquarantine %q: %v", req.Endpoint, err)
			writeErr(w, http.StatusInternalServerError, "unquarantine failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
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
		s.applyFeatureOverrides(ctx, projectID, feats)
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

// handleIntelRun enqueues a test run (M3: deterministic command per build tool,
// report parsing, results persisted). The run executes asynchronously under a
// per-project lock and a global concurrency cap; progress flows through the
// push hub and the run row, and the caller receives the run id immediately.
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
		Force     bool  `json:"force"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	run, err := s.enqueueIntelRun(ctx, req.ProjectID, req.ModuleID, req.NodeID, req.Force)
	if err != nil {
		log.Printf("intel run enqueue project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "run failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run})
}

// handleIntelResultRootcause returns a single per-case result with its parsed
// root-cause report (the attribution view for a failed case).
func (s *Server) handleIntelResultRootcause(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/results/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, "/rootcause")
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid result id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	res, err := s.store.GetIntelTestResult(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "result not found")
		return
	}
	var rootcause any
	if res.RootcauseJSON != "" {
		_ = json.Unmarshal([]byte(res.RootcauseJSON), &rootcause)
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "rootcause": rootcause})
}

// handleIntelRunAll enqueues a one-click regression across every module of a
// project. The aggregate run executes asynchronously; each module gets its own
// run row with per-module progress, and the summary is aggregated at the end.
func (s *Server) handleIntelRunAll(w http.ResponseWriter, r *http.Request) {
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
		Force     bool  `json:"force"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	run, err := s.enqueueIntelRunAll(ctx, req.ProjectID, req.Force)
	if err != nil {
		log.Printf("intel run-all enqueue project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "run-all failed: "+err.Error())
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

// handleIntelRunByID returns one run with its per-case results (GET), or
// cancels a running run (POST .../cancel).
func (s *Server) handleIntelRunByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/runs/")
	rest = strings.TrimSuffix(rest, "/")
	if strings.HasSuffix(rest, "/cancel") && r.Method == http.MethodPost {
		id, err := strconv.ParseInt(strings.TrimSuffix(rest, "/cancel"), 10, 64)
		if err != nil || id <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid run id")
			return
		}
		if !s.cancelIntelRun(id) {
			writeErr(w, http.StatusNotFound, "run not running or already finished")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
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
func (s *Server) runIntelTests(ctx context.Context, projectID, moduleID, nodeID int64, force bool, run *store.TestRun, overrideCmd []string) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return err
	}

	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return err
	}
	module, ok := pickIntelModule(mods, moduleID)
	if !ok {
		return fmt.Errorf("module %d not found", moduleID)
	}

	// 模块可能来自关联源码仓库（多端多仓库，"@end/" 前缀）：用该仓库的根目录，
	// 否则用项目主源码根目录。
	dir := filepath.Join(root, module.RelPath)
	if module.RelPath == "." {
		dir = root
	}
	sources, _ := s.store.ListIntelProjectSources(ctx, projectID)
	if srcRoot, srcRel, ok := s.sourceModuleRoot(ctx, p, sources, module.RelPath); ok {
		dir = filepath.Join(srcRoot, srcRel)
		if srcRel == "." {
			dir = srcRoot
		}
	}
	cmdArgs, reportKind := testCommandFor(module.BuildTool, module.KindType)
	if len(cmdArgs) == 0 && reportKind == "" {
		return fmt.Errorf("unsupported build tool %q for module %s", module.BuildTool, module.RelPath)
	}
	// Honor the human-reviewed project-level command whitelist
	// (projects.commands_json) when it offers a matching test command for this
	// module's build tool; the whitelist is parsed into argv (no shell), so
	// edited entries cannot inject shell metacharacters.
	commandSpecified := false
	if wl := whitelistedTestCommand(p.CommandsJSON, module.BuildTool, module.KindType); wl != nil {
		cmdArgs = wl
		commandSpecified = true
	}
	// 测试计划执行（POST /api/intel/plan）可覆盖命令：计划预览的命令必须先
	// 经 argv 解析（无 shell）且无 shell 元字符，否则回退到默认/白名单命令。
	if len(overrideCmd) > 0 {
		safe := true
		for _, tok := range overrideCmd {
			if hasShellMeta(tok) {
				safe = false
				break
			}
		}
		if safe {
			cmdArgs = overrideCmd
			commandSpecified = true
		}
	}
	// Playwright 项目（web/node 检出 playwright.config 或 @playwright/test）：
	// 默认 `npm test` 不产逐用例 JSON，页面只剩一条整轮结果；在用户未人工
	// 指定命令时切换为 `npx playwright test --reporter=json`，stdout 输出可
	// 解析的逐用例 JSON。人工指定的命令（白名单/覆盖）优先，不被覆盖。
	if reportKind == "npm" && !commandSpecified && detectPlaywright(dir) {
		cmdArgs = []string{"npx", "playwright", "test", "--reporter=json"}
		reportKind = "playwright"
	}
	// 默认无确定性命令的工具（xcode 需 -scheme 等）：依赖命令白名单/测试计划
	// 提供；仍未配置时明确报错，避免空命令执行。
	if len(cmdArgs) == 0 {
		return fmt.Errorf("module %s (build tool %q) 未配置测试命令：请在命令白名单或测试计划中指定",
			module.RelPath, module.BuildTool)
	}

	run.ModuleID = module.ID
	run.Kind = reportKind
	run.Command = strings.Join(cmdArgs, " ")

	// Execution target: local (default) or a remote SSH node when specified.
	remoteNode := (*store.RemoteNode)(nil)
	if nodeID > 0 {
		node, err := s.store.GetRemoteNode(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("remote node %d not found", nodeID)
		}
		if !node.Reachable {
			return fmt.Errorf("remote node %q 不可达（请先在节点列表检查）", node.Name)
		}
		// Capability routing (§11 偏差#2)：节点声明了 capability 标签但没有涵盖
		// 本模块测试所需标签时拒绝执行；未标注 capability 的旧节点视为不设防，
		// 全部接受以保持向后兼容。
		if need := remoteCapabilityFor(module.BuildTool, module.KindType); need != "" &&
			strings.TrimSpace(node.Capabilities) != "" && !nodeHasCapability(node.Capabilities, need) {
			return fmt.Errorf("远程节点 %q 缺少 capability %q（已声明：%q），无法执行 %s 模块的测试",
				node.Name, need, node.Capabilities, module.BuildTool)
		}
		remoteNode = node
	} else if !force {
		if err := s.envGate(ctx, projectID); err != nil {
			// Environment gate (§3.6): reject the run before executing when
			// required middleware/toolchains are missing, with a per-item list.
			// Local runs are gated unless force bypasses; routed runs rely on the
			// node's capability labels instead.
			return err
		}
	}

	var wd string
	var output []byte
	var execErr error
	if remoteNode != nil {
		command := strings.Join(cmdArgs, " ")
		wd = remoteNode.WorkDir
		if wd == "" {
			wd = "~"
		}
		// ssh runs the remote command through the login shell, so any token with
		// shell metacharacters would be interpreted on the node. Reject such
		// commands deterministically (argv is safe locally; this guards the
		// remote interpretation layer).
		for _, tok := range cmdArgs {
			if hasShellMeta(tok) {
				return fmt.Errorf("远程命令含 shell 元字符，已拒绝执行（参数：%q）", tok)
			}
		}
		if hasShellMeta(wd) {
			return fmt.Errorf("远程工作目录含 shell 元字符：%q", wd)
		}
		if !strings.HasPrefix(strings.TrimSpace(command), "cd ") {
			command = "cd " + wd + " && " + command
		}
		sshArgs := envagent.SSHCommandArgs(remoteNode.Host, remoteNode.User, remoteNode.Port, command)
		var out string
		out, execErr = envagent.RunSSH(ctx, sshArgs...)
		output = []byte(out)
	} else {
		output, execErr = runCommand(ctx, dir, cmdArgs[0], cmdArgs[1:]...)
	}
	// A test framework signals failures through its exit code, so a non-zero
	// exit still comes with a parseable report. Bailing out on any error meant
	// the runs that most need per-case results were exactly the ones that got
	// none; only a command that never ran (spawn failure, cancelled ctx) is
	// terminal here.
	var exitErr *exec.ExitError
	if execErr != nil && !errors.As(execErr, &exitErr) {
		// 保留输出尾部（截断到上限），失败原因一并记录，供详情页排查。
		run.Output = truncateOutput(output)
		return fmt.Errorf("test command failed: %w", execErr)
	}

	run.Output = truncateOutput(output)
	// 远程节点的报告产物在节点文件系统上，stdout 不包含 XML：把需要落盘的
	// 报告（surefire/pytest/xctest）拉回本地临时目录再解析；go/playwright/npm
	// 的输出 stdout 自包含，直接解析即可。临时目录用完即清理。
	reportDir := dir
	if remoteNode != nil {
		switch reportKind {
		case "surefire", "pytest", "xctest":
			pulled, err := pullRemoteReports(ctx, remoteNode, wd, reportKind)
			if err != nil {
				return err
			}
			reportDir = pulled
			defer os.RemoveAll(reportDir)
		}
	}
	results := parseReport(reportKind, reportDir, output)
	// 关键：parseReport 构造的结果不带 run/project/module 归属（默认 0），
	// 写入前必须回填，否则 test_results 的 run_id/project_id/module_id 全是
	// 0，运行详情页永远查不到本 run 的逐用例结果（既有 bug，真实用例被解析
	// 出来后暴露）。
	for _, res := range results {
		res.RunID = run.ID
		res.ProjectID = projectID
		res.ModuleID = module.ID
	}
	s.flakyRetry(ctx, projectID, module.ID, dir, reportKind, results)
	// npm/other script runners produce no per-case report on stdout; synthesize a
	// single whole-run result so the run has a definite pass/fail to display.
	if len(results) == 0 && reportKind == "npm" {
		results = append(results, &store.TestResult{
			Kind:     "npm",
			Endpoint: "npm test",
			Passed:   exitErr == nil,
		})
	}
	if err := s.store.AddIntelTestResults(ctx, results); err != nil {
		return err
	}
	s.recordTestCaseOutcomes(ctx, projectID, results)
	s.recordRunIssues(ctx, run, projectID, module.ID, results)

	failed := 0
	for _, res := range results {
		if !res.Passed {
			failed++
		}
	}
	switch {
	case failed > 0:
		run.Status = "failed"
		run.Progress = fmt.Sprintf("失败 %d 个用例", failed)
	case exitErr != nil:
		// Non-zero exit with nothing parsed as failed — a compile error, a bad
		// command, a crash before the report was written. Not a pass.
		run.Status = "failed"
		run.Progress = fmt.Sprintf("命令退出码 %d，未解析到失败用例", exitErr.ExitCode())
	default:
		run.Status = "passed"
		run.Progress = fmt.Sprintf("通过 %d 个用例", len(results))
	}
	finished := time.Now()
	run.FinishedAt = &finished
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		return err
	}
	s.pushIntelRunEvent(run)
	return nil
}

// truncateOutput bounds captured test output to intelOutputLimit bytes, keeping
// the tail so the most relevant (failure) lines survive.
func truncateOutput(out []byte) string {
	if len(out) <= intelOutputLimit {
		return string(out)
	}
	tail := out[len(out)-intelOutputLimit:]
	return "[输出已截断，保留末尾 " + strconv.Itoa(intelOutputLimit) + " 字节]\n" + string(tail)
}

// testCommandFor returns the deterministic test command + report kind for a
// build tool. The command is fixed per tool (whitelist-reviewed in a later
// milestone); it never accepts arbitrary user input.
func testCommandFor(buildTool, kindType string) ([]string, string) {
	switch buildTool {
	case "go":
		// -count=1 matches the plan path: a cached package emits no per-test JSON
		// events, so without it a re-run of unchanged code yields zero cases and
		// the run is recorded as a green "通过 0 个用例".
		return []string{"go", "test", "-json", "-count=1", "./..."}, "go"
	case "maven":
		return []string{"mvn", "test"}, "surefire"
	case "gradle":
		if kindType == "android" {
			return []string{"./gradlew", "testDebugUnitTest"}, "surefire"
		}
		return []string{"./gradlew", "test"}, "surefire"
	case "npm":
		return []string{"npm", "test"}, "npm"
	case "xcode":
		// xcodebuild 需要 -scheme 等参数，无法确定性默认；命令由命令白名单或
		// 测试计划提供，reportKind 固定为 xctest 以解析 JUnit 报告。
		return nil, "xctest"
	case "pytest":
		return []string{"pytest", "-q", "--junitxml=junit.xml"}, "pytest"
	}
	return nil, ""
}

// detectPlaywright reports whether a web/node module uses Playwright (a
// playwright.config.* file at the module root, or the @playwright/test /
// playwright dependency in package.json). Such projects run with
// `npx playwright test --reporter=json` so per-case results are parseable
// instead of collapsing into a single whole-run row.
func detectPlaywright(dir string) bool {
	matches, _ := filepath.Glob(filepath.Join(dir, "playwright.config.*"))
	if len(matches) > 0 {
		return true
	}
	pj, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return false
	}
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if json.Unmarshal(pj, &manifest) != nil {
		return false
	}
	for dep := range manifest.Dependencies {
		if dep == "@playwright/test" || dep == "playwright" {
			return true
		}
	}
	for dep := range manifest.DevDependencies {
		if dep == "@playwright/test" || dep == "playwright" {
			return true
		}
	}
	return false
}

// whitelistedTestCommand picks a test command from the project-level reviewed
// command whitelist (projects.commands_json, a JSON array of command strings).
// It returns the matching argv (split with strings.Fields, never a shell) or nil
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
		case "xcode":
			matched = argv[0] == "xcodebuild" && hasTestIntent(argv)
		case "pytest":
			matched = argv[0] == "pytest" && hasTestIntent(argv)
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
func (s *Server) flakyRetry(ctx context.Context, projectID, moduleID int64, dir, reportKind string, results []*store.TestResult) {
	if reportKind != "go" {
		return
	}
	// 已隔离的用例不再参与 flaky 重跑（已连续 flaky 达阈值，重跑无意义）。
	var quarantined map[string]bool
	if q, err := s.store.ListQuarantinedIntelTestCases(ctx, projectID, moduleID); err == nil {
		quarantined = q
	}
	failed := make([]*store.TestResult, 0)
	seen := map[string]bool{}
	names := make([]string, 0)
	for _, r := range results {
		if r == nil || r.Passed {
			continue
		}
		if quarantined[r.Endpoint] {
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

// recordTestCaseOutcomes writes each run result back to its discovered test
// case asset (last_status / duration / flaky_count), keeping the 测试资产清单
// consistent with actual runs.
func (s *Server) recordTestCaseOutcomes(ctx context.Context, projectID int64, results []*store.TestResult) {
	for _, res := range results {
		if res == nil {
			continue
		}
		class, method := splitEndpoint(res.Endpoint)
		flaky := isFlakyResult(res.FailuresJSON)
		_ = s.store.UpdateIntelTestCaseOutcome(ctx, projectID, res.ModuleID, class, method, res.Passed, 0, flaky)
	}
}

// splitEndpoint splits a result endpoint ("Class.method" or ".method") into its
// class (may be empty) and method parts.
func splitEndpoint(endpoint string) (string, string) {
	idx := strings.LastIndexByte(endpoint, '.')
	if idx < 0 {
		return "", endpoint
	}
	if idx == 0 {
		return "", endpoint[1:]
	}
	return endpoint[:idx], endpoint[idx+1:]
}

// isFlakyResult reports whether a result's failures JSON carries the flaky
// marker written by flakyRetry.
func isFlakyResult(failuresJSON string) bool {
	if !strings.Contains(failuresJSON, "flaky") {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(failuresJSON), &m) != nil {
		return false
	}
	v, ok := m["flaky"].(bool)
	return ok && v
}

// hasShellMeta reports whether a token contains characters that a POSIX shell
// would interpret (metacharacters, quotes, expansions). Used to keep remote
// command arguments from being shell-evaluated on the node.
func hasShellMeta(s string) bool {
	return strings.ContainsAny(s, ";&|<>`$()*?[]{}\\!\"'#~")
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
		if len(cases) == 0 {
			cases = report.ParseGoTestText(output)
		}
	case "surefire":
		files, _ := filepath.Glob(filepath.Join(dir, "target", "surefire-reports", "TEST-*.xml"))
		for _, f := range files {
			if data, err := os.ReadFile(f); err == nil {
				c, _ := report.ParseSurefireXML(data)
				cases = append(cases, c...)
			}
		}
	case "playwright":
		// `--reporter=json` 的 stdout 是纯 JSON；runCommand 合并了 stderr，
		// 浏览器日志可能混入，直接解析失败时提取 JSON 段再试。
		if c, err := report.ParsePlaywrightJSON(output); err == nil {
			cases = c
		} else if i := bytes.IndexByte(output, '{'); i >= 0 {
			if j := bytes.LastIndexByte(output, '}'); j > i {
				if c, err := report.ParsePlaywrightJSON(output[i : j+1]); err == nil {
					cases = c
				}
			}
		}
	case "xctest":
		// XCTest JUnit（xcodebuild/fastlane 导出）：stdout 可能是报告本身，
		// 否则在模块下递归找 junit XML 文件（命令可能落盘）。
		if c, err := report.ParseXCTestJUnit(output); err == nil && len(c) > 0 {
			cases = c
			break
		}
		for _, f := range collectJUnitXML(dir, 3) {
			if data, err := os.ReadFile(f); err == nil {
				if c, err := report.ParseXCTestJUnit(data); err == nil {
					cases = append(cases, c...)
				}
			}
		}
	case "pytest":
		if data, err := os.ReadFile(filepath.Join(dir, "junit.xml")); err == nil {
			cases, _ = report.ParsePytestJUnit(data)
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
	var quarantined map[string]bool
	if q, err := s.store.ListQuarantinedIntelTestCases(ctx, projectID, moduleID); err == nil {
		quarantined = q
	}
	for _, res := range results {
		if res.Passed {
			continue
		}
		// 隔离中的 flaky 用例不生成 issue，避免不稳定用例反复进问题清单。
		if quarantined[res.Endpoint] {
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

// collectJUnitXML walks dir (bounded depth) collecting JUnit-style XML report
// files (name contains "junit" and ends in .xml) written to disk by an XCTest
// or pytest command that doesn't print the report to stdout.
func collectJUnitXML(dir string, maxDepth int) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		base := strings.ToLower(d.Name())
		if strings.Contains(base, "junit") && strings.HasSuffix(base, ".xml") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// remoteCapabilityFor derives the capability label a remote node must declare
// to execute a module's tests, from its build tool and kind type. The mapping
// is deterministic and reviewed; an empty result means the module is not
// capability-routed (unlabelled nodes still accept it, keeping nodes created
// before capability labels working).
func remoteCapabilityFor(buildTool, kindType string) string {
	switch buildTool {
	case "go":
		return "go"
	case "maven":
		return "java"
	case "gradle":
		if kindType == "android" {
			return "android"
		}
		return "gradle"
	case "npm":
		return "node"
	case "pytest":
		return "python"
	case "xcode":
		return "ios"
	}
	return ""
}

// nodeHasCapability reports whether a node's capability labels (whitespace or
// comma separated, e.g. "ios-xcode android-sdk linux-docker go java node
// python") include the required label. A token matches when it equals the label
// or carries it as a "-"/"_" prefix, so "android-sdk" satisfies "android" and
// "ios-xcode" satisfies "ios".
func nodeHasCapability(capabilities, required string) bool {
	if required == "" {
		return true
	}
	for _, tok := range strings.FieldsFunc(capabilities, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	}) {
		tok = strings.TrimSpace(tok)
		if tok == required || strings.HasPrefix(tok, required+"-") || strings.HasPrefix(tok, required+"_") {
			return true
		}
	}
	return false
}

// remoteReportRelPaths picks the node-relative paths that hold a report kind's
// on-disk output, relative to the node workDir the test command runs in.
// xctest reports are discovered recursively and pulled by PullJUnitXML instead
// of this explicit list.
func remoteReportRelPaths(reportKind string) []string {
	switch reportKind {
	case "surefire":
		return []string{"target/surefire-reports"}
	case "pytest":
		return []string{"junit.xml"}
	}
	return nil
}

// pullRemoteReports fetches a remote node's on-disk framework reports
// (surefire/pytest/xctest) into a fresh local temp dir so parseReport can read
// them — the SSH command's stdout is the test output, not the report. Report
// kinds that are self-contained on stdout (go/playwright/npm) return "" since
// parseReport's dir argument is unused for them. The caller must remove the
// returned dir.
func pullRemoteReports(ctx context.Context, node *store.RemoteNode, workDir, reportKind string) (string, error) {
	var files map[string][]byte
	var err error
	switch reportKind {
	case "surefire", "pytest":
		files, err = envagent.PullArtifacts(ctx, node.Host, node.User, node.Port, workDir, remoteReportRelPaths(reportKind))
	case "xctest":
		files, err = envagent.PullJUnitXML(ctx, node.Host, node.User, node.Port, workDir)
	default:
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("拉取远程 %s 报告失败：%w", reportKind, err)
	}
	return materializeReportFiles(files)
}

// materializeReportFiles writes an in-memory artifact map (relative path ->
// bytes) under a fresh temp dir, preserving the relative structure so report
// parsers can glob/walk it. Returns the temp dir; the caller must remove it.
func materializeReportFiles(files map[string][]byte) (string, error) {
	tmp, err := os.MkdirTemp("", "intel-reports-")
	if err != nil {
		return "", err
	}
	cleanup := func() {
		_ = os.RemoveAll(tmp)
	}
	for rel, data := range files {
		p := filepath.Join(tmp, filepath.FromSlash(rel))
		// filepath.Join cleans "..", so a traversal entry would silently escape
		// the temp dir unless checked against it explicitly.
		if !strings.HasPrefix(p, tmp+string(filepath.Separator)) {
			cleanup()
			return "", fmt.Errorf("report artifact %q escapes temp dir", rel)
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			cleanup()
			return "", err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			cleanup()
			return "", err
		}
	}
	return tmp, nil
}
