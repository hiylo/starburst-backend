package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// IntelPlanStep is one module's planned build→test pair, ordered by priority.
// The plan is generated from the analyzed snapshot (modules + reviewed command
// whitelist + recent failures/impact), giving 智能测试 a real "规划测试计划"
// capability instead of running every module blindly.
type IntelPlanStep struct {
	ModuleID     int64  `json:"moduleId"`
	RelPath      string `json:"relPath"`
	KindType     string `json:"kindType"`
	KindRole     string `json:"kindRole"`
	BuildCommand string `json:"buildCommand,omitempty"`
	TestCommand  string `json:"testCommand"`
	Priority     int    `json:"priority"`
	Reason       string `json:"reason"`
}

// intelPlanRoleWeight orders module roles so critical backend modules are
// planned before secondary web/app/ios ones.
func intelPlanRoleWeight(role string) int {
	switch strings.ToLower(role) {
	case "backend", "bff":
		return 3
	case "web", "app", "android":
		return 2
	case "ios":
		return 1
	default:
		return 0
	}
}

// intelPlanCommandFor picks the planned build and test command for a module.
// The whitelist (projects.commands_json) is preferred when it offers a matching
// command; otherwise the tool's deterministic defaults are used. Commands are
// argv-parsed (no shell), so edited whitelist entries cannot inject shell
// metacharacters.
func intelPlanCommandFor(commandsJSON, buildTool, kindType string) (build, test string) {
	cmd, _ := testCommandFor(buildTool, kindType)
	test = strings.Join(cmd, " ")
	if wl := whitelistedTestCommand(commandsJSON, buildTool, kindType); wl != nil {
		test = strings.Join(wl, " ")
	}
	// Go 测试计划执行必须禁用缓存并强制 -json：命中缓存的包只输出
	// "ok (cached)"，没有逐用例 JSON 事件；缺 -json 的裸命令同样解析不出用例。
	// 计划要拿到真实逐用例结果，故 Go 命令只要缺 -count=1 就整条**覆写**为可解析
	// 的默认命令（不保留白名单原文）；其他工具按白名单原样采用。单模块 run 走
	// testCommandFor 的默认命令，白名单命中时仍按原文执行，不受此约束。
	if buildTool == "go" && !strings.Contains(test, "-count=1") {
		test = "go test -json -count=1 ./..."
	}
	switch buildTool {
	case "go":
		build = "go build ./..."
	case "maven":
		build = "mvn package"
	case "gradle":
		if kindType == "android" {
			build = "./gradlew assembleDebug"
		} else {
			build = "./gradlew build"
		}
	case "npm":
		build = "npm run build"
	default:
		build = ""
	}
	if wl := whitelistedBuildCommand(commandsJSON, buildTool); wl != "" {
		build = wl
	}
	return build, test
}

// whitelistedBuildCommand picks a build/package command from the reviewed
// project whitelist, preferring an explicit build-intent entry. It returns ""
// when the whitelist has none (fall back to the tool default).
func whitelistedBuildCommand(commandsJSON, buildTool string) string {
	if commandsJSON == "" {
		return ""
	}
	var wl []string
	if err := json.Unmarshal([]byte(commandsJSON), &wl); err != nil {
		return ""
	}
	buildIntent := func(argv []string) bool {
		for _, a := range argv {
			switch a {
			case "package", "install", "verify", "build", "assembleDebug", "assemble", "compile":
				return true
			}
		}
		return false
	}
	for _, line := range wl {
		argv := strings.Fields(line)
		if len(argv) == 0 {
			continue
		}
		switch buildTool {
		case "go":
			if argv[0] == "go" && len(argv) >= 2 && (argv[1] == "build" || argv[1] == "vet") {
				return strings.Join(argv, " ")
			}
		case "maven":
			if argv[0] == "mvn" && buildIntent(argv) {
				return strings.Join(argv, " ")
			}
		case "gradle":
			if argv[0] == "./gradlew" && buildIntent(argv) {
				return strings.Join(argv, " ")
			}
		case "npm":
			if argv[0] == "npm" && buildIntent(argv) {
				return strings.Join(argv, " ")
			}
		}
	}
	return ""
}

// buildIntelTestPlan assembles the ordered test plan for a project. Priority is
// derived from module role weight plus recent failing test cases, so regressing
// backend modules surface first. Only modules with a resolvable test command are
// planned.
func (s *Server) buildIntelTestPlan(ctx context.Context, projectID int64) ([]IntelPlanStep, error) {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if len(mods) == 0 {
		return []IntelPlanStep{}, nil
	}

	// Recent per-module failure count (within the project's test_cases).
	failures := map[int64]int{}
	cases, _ := s.store.ListIntelTestCases(ctx, projectID, 0)
	for _, c := range cases {
		if c.LastStatus == "failed" {
			failures[c.ModuleID]++
		}
	}

	steps := make([]IntelPlanStep, 0, len(mods))
	for _, m := range mods {
		build, test := intelPlanCommandFor(p.CommandsJSON, m.BuildTool, m.KindType)
		if test == "" {
			continue // 无法解析测试命令的模块不入计划
		}
		priority := intelPlanRoleWeight(m.KindRole)*100 + failures[m.ID]
		reason := "角色优先级"
		if failures[m.ID] > 0 {
			reason = "角色优先级 + 最近失败用例 " + strconv.Itoa(failures[m.ID]) + " 个"
		}
		steps = append(steps, IntelPlanStep{
			ModuleID:     m.ID,
			RelPath:      m.RelPath,
			KindType:     m.KindType,
			KindRole:     m.KindRole,
			BuildCommand: build,
			TestCommand:  test,
			Priority:     priority,
			Reason:       reason,
		})
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].Priority > steps[j].Priority })
	return steps, nil
}

// handleIntelPlan generates (GET) a preview of the ordered test plan, and (POST)
// enqueues an execution of the plan. GET is read-only and cheap; POST creates a
// scope=plan aggregate run and executes every step serially (build→test).
func (s *Server) handleIntelPlan(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	var projectID int64
	if r.Method == http.MethodGet {
		var ok bool
		projectID, ok = s.intelQueryProject(w, r)
		if !ok {
			return
		}
	} else if r.Method == http.MethodPost {
		var req struct {
			ProjectID int64 `json:"projectId"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.ProjectID <= 0 {
			writeErr(w, http.StatusBadRequest, "projectId is required")
			return
		}
		projectID = req.ProjectID
	} else {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	steps, err := s.buildIntelTestPlan(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "build plan failed: "+err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"plan": steps})
	case http.MethodPost:
		run, err := s.enqueueIntelPlan(ctx, projectID, steps)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "enqueue plan failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"run": run, "plan": steps})
	}
}

// enqueueIntelPlan creates a scope=plan aggregate run and starts executing the
// planned steps in the background. Same-project runs serialize on the per-project
// execution mutex; the global concurrency cap is respected per module.
func (s *Server) enqueueIntelPlan(ctx context.Context, projectID int64, steps []IntelPlanStep) (*store.TestRun, error) {
	run := &store.TestRun{
		ProjectID: projectID,
		Scope:     "plan",
		Status:    "queued",
		Progress:  "测试计划已生成，排队中",
	}
	if err := s.store.CreateIntelTestRun(ctx, run); err != nil {
		return nil, err
	}
	s.pushIntelRunEvent(run)
	go s.runIntelPlan(projectID, run, steps)
	return run, nil
}

// runIntelPlan executes the planned steps serially under the per-project lock:
// each module first runs its build command (if any) then its test command. A
// build failure marks the module run failed and skips its tests. Every module
// gets its own scope=plan run row so per-module status/progress is visible.
func (s *Server) runIntelPlan(projectID int64, run *store.TestRun, steps []IntelPlanStep) {
	mu := s.intelExecMutex(projectID)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	s.registerIntelCancel(run.ID, cancel)
	defer s.unregisterIntelCancel(run.ID)
	defer cancel()

	now := time.Now()
	run.StartedAt = &now
	run.Status = "running"
	run.Progress = "按测试计划执行中"
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel plan %d start: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)

	if len(steps) == 0 {
		s.finishIntelPlan(ctx, run, "passed", "项目没有可执行模块")
		return
	}

	var (
		wg          sync.WaitGroup
		muStats     sync.Mutex
		passedCount int
		failedCount int
	)
	for i, step := range steps {
		step := step
		mr := &store.TestRun{
			ProjectID: projectID,
			ModuleID:  step.ModuleID,
			Scope:     "plan",
			Status:    "queued",
			Progress:  "等待执行",
		}
		if err := s.store.CreateIntelTestRun(ctx, mr); err != nil {
			continue
		}
		s.pushIntelRunEvent(mr)

		wg.Add(1)
		go func(idx int, total int, m *IntelPlanStep, mr *store.TestRun) {
			defer wg.Done()
			ok := s.execIntelPlanStep(ctx, projectID, m, mr, idx, total)
			muStats.Lock()
			if ok {
				passedCount++
			} else {
				failedCount++
			}
			muStats.Unlock()
		}(i+1, len(steps), &step, mr)
	}
	wg.Wait()

	summary := "测试计划完成：" + strconv.Itoa(passedCount) + " 模块通过 / " + strconv.Itoa(failedCount) + " 失败"
	status := "passed"
	if failedCount > 0 {
		status = "failed"
	}
	s.finishIntelPlan(ctx, run, status, summary)
}

// finishIntelPlan marks the aggregate plan run finished.
func (s *Server) finishIntelPlan(ctx context.Context, run *store.TestRun, status, summary string) {
	now := time.Now()
	run.Status = status
	run.FinishedAt = &now
	run.Progress = summary
	run.Output = summary
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel plan %d finish: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)
}

// execIntelPlanStep executes one module's build→test pair, returning success.
func (s *Server) execIntelPlanStep(ctx context.Context, projectID int64, step *IntelPlanStep, mr *store.TestRun, idx, total int) bool {
	// 全局并发水位：带 semWaitBudget 等待预算，避免在白项目锁下无限空等其他
	// 项目的执行槽位。
	semCtx, cancelSem := context.WithTimeout(ctx, semWaitBudget)
	defer cancelSem()
	if !s.intelSem.acquire(semCtx) {
		s.failIntelRun(projectID, mr, "排队超时，未获得执行槽位")
		return false
	}
	defer s.intelSem.release()

	mctx, cancel := context.WithTimeout(ctx, intelRunTimeout)
	defer cancel()

	now := time.Now()
	mr.StartedAt = &now
	mr.Status = "running"
	mr.Progress = "构建中"
	if err := s.store.UpdateIntelTestRun(mctx, mr); err != nil {
		log.Printf("intel plan %d start: %v", mr.ID, err)
	}
	s.pushIntelRunEvent(mr)

	// 构建阶段：失败则跳过测试，直接标记失败。
	if step.BuildCommand != "" {
		argv := strings.Fields(step.BuildCommand)
		if len(argv) > 0 {
			dir, err := s.planModuleDir(ctx, projectID, step.ModuleID)
			if err != nil {
				s.failIntelRun(projectID, mr, "解析模块目录失败："+err.Error())
				return false
			}
			mr.Command = step.BuildCommand
			_ = s.store.UpdateIntelTestRun(mctx, mr)
			out, err := runCommand(mctx, dir, argv[0], argv[1:]...)
			mr.Output = truncateOutput(out)
			if err != nil {
				mr.Progress = "构建失败（跳过测试）"
				s.failIntelRun(projectID, mr, "构建失败："+err.Error())
				return false
			}
		}
	}

	// 测试阶段：复用现有单模块执行（解析报告、落结果、闭环 issue），并传入
	// 计划预览选择的测试命令 argv（已做 shell 元字符校验，安全）。
	mr.Command = step.TestCommand
	mr.Progress = "测试中"
	_ = s.store.UpdateIntelTestRun(mctx, mr)
	s.pushIntelRunEvent(mr)

	if err := s.runIntelTests(mctx, projectID, step.ModuleID, 0, true, mr, strings.Fields(step.TestCommand)); err != nil {
		reason := err.Error()
		if mctx.Err() != nil {
			reason = "执行超时或已取消"
		}
		s.failIntelRun(projectID, mr, reason)
		return false
	}
	return mr.Status != "failed"
}

// planModuleDir resolves a module's working directory (associated-source-aware),
// mirroring runIntelTests' directory resolution.
func (s *Server) planModuleDir(ctx context.Context, projectID, moduleID int64) (string, error) {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return "", err
	}
	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return "", err
	}
	module, ok := pickIntelModule(mods, moduleID)
	if !ok {
		return "", fmt.Errorf("module %d not found", moduleID)
	}
	dir, err := moduleDir(root, module.RelPath)
	if err != nil {
		return "", err
	}
	sources, _ := s.store.ListIntelProjectSources(ctx, projectID)
	if srcRoot, srcRel, ok := s.sourceModuleRoot(ctx, p, sources, module.RelPath); ok {
		dir, err = moduleDir(srcRoot, srcRel)
		if err != nil {
			return "", err
		}
	}
	return dir, nil
}
