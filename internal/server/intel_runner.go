package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// intelExecConcurrency bounds how many test processes run at once across all
// projects. run-all parallelizes within a project but never exceeds this global
// cap, so a wide regression cannot saturate the host. This is the default; the
// runtime value is adjustable via the intel.workers setting (see intelSem).
const intelExecConcurrency = 2

// intelSem is a process-wide concurrency gate for test executions whose
// capacity can be changed at runtime (intel.workers) without a restart. It
// replaces a fixed-capacity channel so raising/lowering concurrency applies
// immediately to the in-flight waiters.
type intelSem struct {
	mu  sync.Mutex
	cur int
	cap int
	ch  chan struct{}
}

func newIntelSem(cap int) *intelSem {
	if cap < 1 {
		cap = 1
	}
	return &intelSem{cap: cap, ch: make(chan struct{}, 1)}
}

// tryAcquire takes a slot when one is free, without blocking.
func (s *intelSem) tryAcquire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur >= s.cap {
		return false
	}
	s.cur++
	return true
}

// release returns a slot and wakes one waiter.
func (s *intelSem) release() {
	s.mu.Lock()
	if s.cur > 0 {
		s.cur--
	}
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// setCap changes the concurrency cap (clamped to >= 1) and wakes a waiter in
// case the higher cap admits it immediately.
func (s *intelSem) setCap(cap int) {
	if cap < 1 {
		cap = 1
	}
	s.mu.Lock()
	s.cap = cap
	s.mu.Unlock()
	select {
	case s.ch <- struct{}{}:
	default:
	}
}

// capValue returns the current concurrency cap.
func (s *intelSem) capValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cap
}

// acquire blocks until a slot is free or ctx is done.
func (s *intelSem) acquire(ctx context.Context) bool {
	for {
		if s.tryAcquire() {
			return true
		}
		select {
		case <-s.ch:
		case <-ctx.Done():
			return false
		}
	}
}

// intelRunTimeout bounds a single module's test execution (local or remote).
// The previous handler let a run block up to 10/30 minutes with no isolation;
// a bounded per-module budget keeps a wedged suite from blocking others.
const intelRunTimeout = 10 * time.Minute

// intelOutputLimit caps the captured output stored on a run so a chatty test
// cannot balloon test_runs. The tail is kept (latest lines are most useful).
const intelOutputLimit = 256 << 10

// intelRunEvent is pushed to the hub on every run state change so connected
// clients (web workbench, App) can render live progress without polling.
func (s *Server) pushIntelRunEvent(run *store.TestRun) {
	b, err := json.Marshal(map[string]any{"run": run})
	if err != nil {
		return
	}
	s.hub.Broadcast(push.Message{Type: "intel.run.event", Payload: b})
}

// updateIntelRunProgress writes a bounded progress update and streams it to
// connected clients. Errors are logged, never fatal: progress is best-effort.
func (s *Server) updateIntelRunProgress(ctx context.Context, run *store.TestRun, status, progress string) {
	run.Status = status
	run.Progress = progress
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel run %d progress update: %v", run.ID, err)
		return
	}
	s.pushIntelRunEvent(run)
}

// intelExecMutex returns the per-project execution mutex.
func (s *Server) intelExecMutex(projectID int64) *sync.Mutex {
	mu, _ := s.intelExecMu.LoadOrStore(projectID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// enqueueIntelRun creates a queued run and executes it asynchronously. The
// caller gets the run id immediately; progress flows through the hub and the
// run row. Same-project runs serialize on intelExecMutex. When nodeID is 0 and
// force is false the environment gate is checked synchronously so a gate
// rejection surfaces to the caller before anything is enqueued.
func (s *Server) enqueueIntelRun(ctx context.Context, projectID, moduleID, nodeID int64, force bool) (*store.TestRun, error) {
	if nodeID == 0 && !force {
		if err := s.envGate(ctx, projectID); err != nil {
			return nil, err
		}
	}
	run := &store.TestRun{
		ProjectID: projectID,
		ModuleID:  moduleID,
		Scope:     "module",
		Status:    "queued",
		Progress:  "排队中",
	}
	if err := s.store.CreateIntelTestRun(ctx, run); err != nil {
		return nil, err
	}
	s.pushIntelRunEvent(run)
	go s.runIntelJob(projectID, moduleID, nodeID, force, run)
	return run, nil
}

// intelMaxRunAttempts bounds automatic retries of a module run whose execution
// itself failed (command error, timeout, build failure) — a bounded substitute
// for the full task-state-machine retry while intel stays on its own runner.
const intelMaxRunAttempts = 2

// runIntelJob executes one queued module run under the per-project lock and
// the global concurrency cap.
func (s *Server) runIntelJob(projectID, moduleID, nodeID int64, force bool, run *store.TestRun) {
	mu := s.intelExecMutex(projectID)
	mu.Lock()
	defer mu.Unlock()

	// 全局并发水位：拿不到槽位时保持排队状态等待。
	semCtx, cancelSem := context.WithTimeout(context.Background(), intelRunTimeout)
	defer cancelSem()
	if !s.intelSem.acquire(semCtx) {
		s.failIntelRun(projectID, run, "排队超时，未获得执行槽位")
		return
	}
	defer s.intelSem.release()

	execCtx, cancel := context.WithTimeout(context.Background(), intelRunTimeout)
	s.registerIntelCancel(run.ID, cancel)
	defer s.unregisterIntelCancel(run.ID)
	defer cancel()

	now := time.Now()
	run.StartedAt = &now
	run.Status = "running"
	run.Progress = "执行中"
	if err := s.store.UpdateIntelTestRun(execCtx, run); err != nil {
		log.Printf("intel run %d start update: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)

	if err := s.runIntelTests(execCtx, projectID, moduleID, nodeID, force, run, nil); err != nil {
		if execCtx.Err() != nil {
			s.failIntelRun(projectID, run, "执行超时或已取消："+execCtx.Err().Error())
			return
		}
		// 执行异常（命令失败/构建失败）自动重试，最多 intelMaxRunAttempts 次。
		// 测试用例失败走 run.Status=failed 且 err==nil 路径，不重试。
		if run.Attempts < intelMaxRunAttempts {
			retry := &store.TestRun{
				ProjectID: projectID,
				ModuleID:  moduleID,
				Scope:     "module",
				Status:    "queued",
				Progress:  fmt.Sprintf("自动重试（第 %d 次）", run.Attempts+1),
				Attempts:  run.Attempts + 1,
			}
			if cerr := s.store.CreateIntelTestRun(context.Background(), retry); cerr == nil {
				s.pushIntelRunEvent(retry)
				run.Progress = fmt.Sprintf("执行失败，已自动重试（原因为 %s）", err.Error())
				if uerr := s.store.UpdateIntelTestRun(context.Background(), run); uerr != nil {
					log.Printf("intel run %d retry note update: %v", run.ID, uerr)
				}
				go s.runIntelJob(projectID, moduleID, nodeID, force, retry)
				return
			} else {
				log.Printf("intel run %d retry create failed: %v", run.ID, cerr)
			}
		}
		s.failIntelRun(projectID, run, err.Error())
		return
	}
}

// failIntelRun marks a run failed with the given reason and broadcasts it.
func (s *Server) failIntelRun(projectID int64, run *store.TestRun, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now()
	run.Status = "failed"
	run.FinishedAt = &now
	run.Progress = "失败：" + reason
	if run.Output == "" {
		run.Output = reason
	}
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel run %d fail update: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)
}

// enqueueIntelRunAll creates a scope=all run and executes every module's test
// command, parallelizing across modules up to the global concurrency cap.
// Each module gets its own run row so per-module status/progress is visible.
func (s *Server) enqueueIntelRunAll(ctx context.Context, projectID int64, force bool) (*store.TestRun, error) {
	if !force {
		if err := s.envGate(ctx, projectID); err != nil {
			return nil, err
		}
	}
	run := &store.TestRun{
		ProjectID: projectID,
		Scope:     "all",
		Status:    "queued",
		Progress:  "排队中",
	}
	if err := s.store.CreateIntelTestRun(ctx, run); err != nil {
		return nil, err
	}
	s.pushIntelRunEvent(run)
	go s.runIntelAll(projectID, force, run)
	return run, nil
}

// runIntelAll is the background driver for a scope=all run: it serializes on
// the per-project lock, lists modules and parallelizes each module's execution
// within the global cap.
func (s *Server) runIntelAll(projectID int64, force bool, run *store.TestRun) {
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
	run.Progress = "准备模块列表"
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel run-all %d start update: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)

	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		s.failIntelRun(projectID, run, "加载模块列表失败："+err.Error())
		return
	}
	if len(mods) == 0 {
		// Nothing to run is not a failure; the summary says why.
		s.finishIntelRunAll(ctx, run, "passed", "项目没有可测试模块")
		return
	}

	var (
		wg          sync.WaitGroup
		muStats     sync.Mutex
		passedCount int
		failedCount int
		errorsCount int
	)
	perModule := make(map[int64]*store.TestRun, len(mods))
	for i, m := range mods {
		mr := &store.TestRun{
			ProjectID: projectID,
			ModuleID:  m.ID,
			Scope:     "module",
			Status:    "queued",
			Progress:  "等待执行",
		}
		if err := s.store.CreateIntelTestRun(ctx, mr); err != nil {
			continue
		}
		perModule[m.ID] = mr
		s.pushIntelRunEvent(mr)

		wg.Add(1)
		go func(m *store.IntelModule, mr *store.TestRun, idx int) {
			defer wg.Done()
			status := s.execIntelModule(ctx, projectID, m, mr, force, idx, len(mods))
			muStats.Lock()
			switch status {
			case "passed":
				passedCount++
			case "failed":
				failedCount++
			default:
				errorsCount++
			}
			muStats.Unlock()
		}(m, mr, i+1)
	}
	wg.Wait()

	summary := fmt.Sprintf("全部完成：%d 通过 / %d 失败 / %d 错误（共 %d 模块）",
		passedCount, failedCount, errorsCount, len(mods))
	// The aggregate used to be hardcoded "passed", so a run where every module
	// failed still read green to anything that filters on status.
	status := "passed"
	switch {
	case failedCount > 0:
		status = "failed"
		summary = "部分失败：" + summary
	case errorsCount > 0:
		status = "error"
		summary = "部分失败：" + summary
	}
	s.finishIntelRunAll(ctx, run, status, summary)
}

// finishIntelRunAll marks the aggregate run finished with a summary.
func (s *Server) finishIntelRunAll(ctx context.Context, run *store.TestRun, status, summary string) {
	now := time.Now()
	run.Status = status
	run.FinishedAt = &now
	run.Progress = summary
	run.Output = summary
	if err := s.store.UpdateIntelTestRun(ctx, run); err != nil {
		log.Printf("intel run-all %d finish: %v", run.ID, err)
	}
	s.pushIntelRunEvent(run)
}

// execIntelModule runs a single module's test as part of a run-all, returning
// the effective status ("passed" | "failed" | "error").
func (s *Server) execIntelModule(ctx context.Context, projectID int64, m *store.IntelModule, mr *store.TestRun, force bool, idx, total int) string {
	// 全局并发水位：与其他项目/模块的测试进程共享。
	if !s.intelSem.acquire(ctx) {
		s.failIntelRun(projectID, mr, "已取消")
		return "error"
	}
	defer s.intelSem.release()

	mctx, cancel := context.WithTimeout(ctx, intelRunTimeout)
	defer cancel()

	now := time.Now()
	mr.StartedAt = &now
	mr.Status = "running"
	mr.Progress = fmt.Sprintf("执行中（%d/%d）", idx, total)
	if err := s.store.UpdateIntelTestRun(mctx, mr); err != nil {
		log.Printf("intel run %d progress: %v", mr.ID, err)
	}
	s.pushIntelRunEvent(mr)

	if err := s.runIntelTests(mctx, projectID, m.ID, 0, force, mr, nil); err != nil {
		reason := err.Error()
		if mctx.Err() != nil {
			reason = "执行超时或已取消"
		}
		s.failIntelRun(projectID, mr, reason)
		return "failed"
	}
	// runIntelTests 已把单模块 run 标成 passed/failed；这里读取状态用于聚合。
	status := "passed"
	if mr.Status == "failed" {
		status = "failed"
	}
	return status
}

// registerIntelCancel stores a run's cancel function so /cancel can abort it.
func (s *Server) registerIntelCancel(runID int64, cancel context.CancelFunc) {
	s.intelCancelMu.Lock()
	defer s.intelCancelMu.Unlock()
	s.intelCancels[runID] = cancel
}

// unregisterIntelCancel removes a run's cancel function once it finishes.
func (s *Server) unregisterIntelCancel(runID int64) {
	s.intelCancelMu.Lock()
	defer s.intelCancelMu.Unlock()
	delete(s.intelCancels, runID)
}

// cancelIntelRun aborts a running test run by id; returns false if not found.
func (s *Server) cancelIntelRun(runID int64) bool {
	s.intelCancelMu.Lock()
	defer s.intelCancelMu.Unlock()
	cancel, ok := s.intelCancels[runID]
	if !ok {
		return false
	}
	cancel()
	delete(s.intelCancels, runID)
	return true
}
