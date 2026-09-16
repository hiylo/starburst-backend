package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/hiylo/starburst-backend/internal/llm"
	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// Executor runs claimed tasks against the OpenCode server and reports
// progress through the push hub. Run spawns a pool of worker goroutines that
// claim tasks concurrently.
type Executor struct {
	store        store.Store
	hub          *push.Hub
	openCodeBase string // base URL of the OpenCode HTTP server
	httpClient   *http.Client
	llm          *llm.Client   // optional orchestration LLM for summaries/self-healing
	maxRetries   int           // additional attempts after the first failure
	workers      int           // concurrent task executions
	retention    time.Duration // finished-task retention; 0 keeps them forever
	sem          chan struct{} // global concurrency cap; nil = no extra cap beyond workers
	gateMu       sync.Mutex    // guards gates
	gates        map[string]*sessionGate
	dirGates     map[string]*sessionGate // per-directory gates for new-session tasks
	busyMu       sync.Mutex              // guards busyCache
	busyCache    map[string]busyEntry    // session busy-status short TTL
}

// busyTTL bounds how long a /session/status lookup is cached, so a busy task
// polling every 3s does not hammer the upstream status endpoint.
const busyTTL = 2 * time.Second

type busyEntry struct {
	at   time.Time
	busy bool
}

// sessionGate serializes concurrent executions that target the same upstream
// session id. Each execution reads the last assistant message of its session,
// so two tasks explicitly sharing a session must not run at the same time.
type sessionGate struct {
	sessionID string
	mu        sync.Mutex // serializes executions sharing sessionID
	holders   int        // goroutines waiting on or holding mu
}

// NewExecutor wires an executor. openCodeBase is required; the executor only
// sends prompts over the upstream HTTP API, it never proxies raw traffic.
func NewExecutor(st store.Store, hub *push.Hub, openCodeBase string) *Executor {
	return &Executor{
		store:        st,
		hub:          hub,
		openCodeBase: openCodeBase,
		httpClient:   &http.Client{Timeout: 90 * time.Second},
		maxRetries:   2,
		workers:      4,
		gates:        make(map[string]*sessionGate),
		dirGates:     make(map[string]*sessionGate),
		busyCache:    make(map[string]busyEntry),
	}
}

// WithConcurrencyCap caps the number of concurrently running tasks across all
// workers. A value <= 0 leaves the worker count as the only limit.
func (e *Executor) WithConcurrencyCap(n int) *Executor {
	if n > 0 {
		e.sem = make(chan struct{}, n)
	}
	return e
}

// WithMaxRetries sets how many times a failed task is re-queued before it is
// permanently marked failed. Returns the receiver for chaining.
func (e *Executor) WithMaxRetries(n int) *Executor {
	if n < 0 {
		n = 0
	}
	e.maxRetries = n
	return e
}

// WithWorkers sets how many tasks may run concurrently. 1 restores the original
// serial behavior. Values below 1 are clamped to 1.
func (e *Executor) WithWorkers(n int) *Executor {
	if n < 1 {
		n = 1
	}
	e.workers = n
	return e
}

// WithRetention sets how long finished (succeeded/failed/canceled) tasks stay
// in the database before the janitor deletes them. Zero or negative keeps them
// forever. Negative input is clamped to 0 rather than rejected.
func (e *Executor) WithRetention(d time.Duration) *Executor {
	if d < 0 {
		d = 0
	}
	e.retention = d
	return e
}

// WithLLM wires the optional orchestration LLM used for result summaries and
// failure self-healing decisions. Returns the receiver for chaining.
func (e *Executor) WithLLM(c *llm.Client) *Executor {
	e.llm = c
	return e
}

// Run is the worker pool: each worker claims one queued task and executes it,
// repeating until ctx is canceled. It returns once every worker has drained.
func (e *Executor) Run(ctx context.Context) {
	// Recover tasks left "running" by a previous crash/restart.
	if n, err := e.store.RecoverStaleRunning(ctx); err != nil {
		log.Printf("tasks: recover stale running: %v", err)
	} else if n > 0 {
		log.Printf("tasks: recovered %d stale running tasks back to queued", n)
	}

	var wg sync.WaitGroup
	if e.retention > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.purgeLoop(ctx)
		}()
	}
	for i := 0; i < e.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if e.workers > 1 {
				log.Printf("tasks: worker %d/%d started", id+1, e.workers)
			}
			e.worker(ctx)
		}(i)
	}
	wg.Wait()
}

// worker is the claim-execute loop of a single worker goroutine.
func (e *Executor) worker(ctx context.Context) {
	backoff := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		t, err := e.store.ClaimNextTask(ctx)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// No work: exponential backoff (max 10s) with jitter, so idle
				// workers do not hammer the claim query.
				if !sleep(ctx, backoff) {
					return
				}
				delay := backoff*2 + time.Duration(randIntN(int(backoff)))
				backoff = minDuration(delay, 10*time.Second)
				continue
			}
			log.Printf("tasks: claim failed: %v", err)
			if !sleep(ctx, 2*time.Second) {
				return
			}
			continue
		}
		backoff = 2 * time.Second
		e.runClaimed(ctx, t)
	}
}

// runClaimed drives one claimed task to completion. Tasks that explicitly
// share an upstream session id are serialized against each other; tasks that
// create a new session are serialized per working directory; a global
// concurrency cap may additionally limit simultaneous executions.
func (e *Executor) runClaimed(ctx context.Context, t *store.Task) {
	// Global concurrency cap (if configured): acquire before the session gate so
	// a low cap does not let many goroutines pile up holding per-session locks.
	if e.sem != nil {
		select {
		case e.sem <- struct{}{}:
			defer func() { <-e.sem }()
		case <-ctx.Done():
			// Canceled while waiting for a slot: return the task to the queue.
			// The claimed task is still 'running'; requeue it explicitly.
			ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if ok, err := e.store.RequeueRunning(ctx2, t.ID); err == nil && ok {
				e.pushTaskEvent(store.TaskQueued, map[string]any{"id": t.ID, "status": store.TaskQueued, "reason": "shutdown requeue"})
			} else if err != nil {
				log.Printf("tasks: requeue %s on shutdown: %v", t.ID, err)
			}
			cancel()
			return
		}
	}
	// Serialize by session id (explicit shared session) ...
	g := e.hold(t.SessionID, e.gates)
	if g != nil {
		defer e.release(e.gates, g)
		g.mu.Lock()
		defer g.mu.Unlock()
	}
	// ... or by directory when the task creates its own session in a repo.
	if t.SessionID == "" && t.Directory != "" {
		dg := e.hold(t.Directory, e.dirGates)
		if dg != nil {
			defer e.release(e.dirGates, dg)
			dg.mu.Lock()
			defer dg.mu.Unlock()
		}
	}
	// A panic in execute must not kill the whole process (Go does not isolate
	// goroutine panics): mark the task failed so it can be inspected and retried,
	// then return to the worker loop.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("tasks: worker panic on task %s: %v\n%s", t.ID, r, debug.Stack())
			ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = e.store.FailTask(ctx2, t.ID, fmt.Sprintf("panic: %v", r))
		}
	}()
	e.execute(ctx, t)
}

// hold returns the serialization gate for key in the given map (session id or
// directory), registering one holder. Tasks without an explicit session id or
// directory create their own upstream session and need no serialization.
func (e *Executor) hold(key string, m map[string]*sessionGate) *sessionGate {
	if key == "" {
		return nil
	}
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	g, ok := m[key]
	if !ok {
		g = &sessionGate{sessionID: key}
		m[key] = g
	}
	g.holders++
	return g
}

// release drops one holder registered by hold, freeing the gate once no worker
// is waiting on it any more.
func (e *Executor) release(m map[string]*sessionGate, g *sessionGate) {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	g.holders--
	if g.holders <= 0 {
		delete(m, g.sessionID)
	}
}

// sleep waits for d or for ctx to be canceled, reporting whether the wait was
// interrupted by cancellation.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// execute drives one task to completion and updates the store + pushes events.
// On failure it either re-queues the task for another attempt (bounded by
// maxRetries, with exponential backoff) or marks it permanently failed.
func (e *Executor) execute(ctx context.Context, t *store.Task) {
	pushTask := func(status string, _ *store.Task) {
		e.pushTaskEvent(status, map[string]any{"id": t.ID, "status": status})
	}

	failWithRetry := func(errMsg string) {
		if t.Attempts > e.maxRetries {
			// Deterministic retries exhausted. Consult the LLM once for a
			// final self-healing decision: grant a single extra retry if the
			// failure looks transient, otherwise escalate to permanent fail.
			if !strings.Contains(t.Progress, "llm-retry") && e.llm != nil && e.llm.Enabled() {
				c, cancel := context.WithTimeout(ctx, 30*time.Second)
				d, err := e.decideFailure(c, t.Prompt, errMsg)
				cancel()
				if err == nil && d.Action == "retry" {
					_ = e.store.RetryTask(ctx, t.ID, 60)
					_ = e.store.UpdateTaskProgress(ctx, t.ID, "llm-retry: "+d.Reason)
					pushTask("retrying", t)
					log.Printf("tasks: %s LLM decided to retry after max attempts: %s", t.ID, d.Reason)
					return
				}
				if err == nil && d.Reason != "" {
					errMsg = errMsg + " | " + d.Reason
				}
			}
			_ = e.store.FailTask(ctx, t.ID, errMsg)
			e.resolveDependents(ctx, t.ID, false, errMsg)
			pushTask("failed", t)
			e.pushFailureAnalysis(ctx, t, errMsg)
			return
		}
		// Re-queue for another attempt with exponential backoff.
		backoff := 5 * (1 << (t.Attempts - 1)) // 5s, 10s, 20s...
		if backoff > 120 {
			backoff = 120
		}
		if err := e.store.RetryTask(ctx, t.ID, backoff); err != nil {
			_ = e.store.FailTask(ctx, t.ID, errMsg)
			e.resolveDependents(ctx, t.ID, false, errMsg)
			pushTask("failed", t)
			return
		}
		pushTask("retrying", t)
		log.Printf("tasks: %s failed (attempt %d/%d), will retry in %ds: %s", t.ID, t.Attempts, e.maxRetries, backoff, errMsg)
	}

	pushTask("running", t)

	// 单任务超时：timeout_seconds > 0 时限制本次执行总时长（超时按失败重试处理）。
	runCtx := ctx
	var cancelRun context.CancelFunc
	if t.TimeoutSec > 0 {
		runCtx, cancelRun = context.WithTimeout(ctx, time.Duration(t.TimeoutSec)*time.Second)
		defer cancelRun()
	}

	// canceled reports whether this task was canceled mid-execution.
	canceled := func() bool {
		c, err := e.store.IsTaskCanceled(ctx, t.ID)
		return err == nil && c
	}

	// 提前取消（会话还没创建）：直接收尾。
	sessionID := t.SessionID
	if sessionID == "" {
		if canceled() {
			e.resolveDependents(ctx, t.ID, false, "upstream task canceled")
			pushTask("canceled", t)
			return
		}
		// 同目录存在空闲会话则复用，避免每次任务都新建会话造成会话膨胀。
		sessionID, _ = e.findIdleSession(runCtx, t.Directory)
		if sessionID == "" {
			created, err := e.createSession(runCtx, t.Directory)
			if err != nil {
				failWithRetry("create session: " + err.Error())
				return
			}
			sessionID = created
			_ = e.store.SetTaskSession(ctx, t.ID, sessionID)
			_ = e.store.UpdateTaskProgress(ctx, t.ID, "session created")
		} else {
			_ = e.store.SetTaskSession(ctx, t.ID, sessionID)
			_ = e.store.UpdateTaskProgress(ctx, t.ID, "reused idle session")
		}
	}

	// Step 2: prompt the session and drain the response.
	result, err := e.promptSession(runCtx, sessionID, t.Prompt, t.Directory, canceled, func(partial string) {
		_ = e.store.UpdateTaskProgress(ctx, t.ID, partial)
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			failWithRetry(fmt.Sprintf("任务超时（%d 秒）", t.TimeoutSec))
			return
		}
		if errors.Is(err, errCanceled) {
			// 让上游真正停止生成，避免留下孤儿任务继续跑。
			e.abortSession(sessionID, t.Directory)
			e.resolveDependents(ctx, t.ID, false, "upstream task canceled")
			pushTask("canceled", t)
			return
		}
		failWithRetry(err.Error())
		return
	}

	_ = e.store.CompleteTask(ctx, t.ID, result)
	e.resolveDependents(ctx, t.ID, true, "")
	pushTask("succeeded", t)
	e.pushSummary(ctx, t, result)
}

// resolveDependents releases or blocks the tasks that wait on id, pushing one
// event per dependent so a human sees the state change in real time.
func (e *Executor) resolveDependents(ctx context.Context, id string, succeed bool, reason string) {
	if succeed {
		ids, err := e.store.PromotePendingDependents(ctx, id)
		if err != nil {
			log.Printf("tasks: promote dependents of %s: %v", id, err)
			return
		}
		if len(ids) == 0 {
			return
		}
		log.Printf("tasks: %s re-queued %d dependent task(s)", id, len(ids))
		for _, depID := range ids {
			e.pushTaskEvent(store.TaskQueued, map[string]any{
				"id":       depID,
				"status":   store.TaskQueued,
				"upstream": id,
			})
		}
		return
	}
	if reason == "" {
		reason = "upstream task failed"
	}
	ids, err := e.store.BlockDependents(ctx, id, reason)
	if err != nil {
		log.Printf("tasks: block dependents of %s: %v", id, err)
		return
	}
	if len(ids) == 0 {
		return
	}
	log.Printf("tasks: %s blocked %d dependent task(s): %s", id, len(ids), reason)
	for _, depID := range ids {
		e.pushTaskEvent(store.TaskBlocked, map[string]any{
			"id":       depID,
			"status":   store.TaskBlocked,
			"upstream": id,
			"reason":   reason,
		})
	}
}

var errCanceled = errors.New("task canceled")

// createSession creates a new OpenCode session via the HTTP API.
func (e *Executor) createSession(ctx context.Context, directory string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"directory": directory,
		"title":     "starburst-backend task",
	})
	resp, err := e.httpClient.Post(e.openCodeBase+"/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("create session: %s", resp.Status)
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		// Try alternative field naming used by some server versions.
		return "", fmt.Errorf("create session: parse: %w", err)
	}
	if out.ID == "" {
		return "", fmt.Errorf("create session: empty id in response")
	}
	return out.ID, nil
}

// promptSession submits a prompt using the V2 admitted-prompt API
// (POST /session/{id}/prompt), then waits for the session to become idle
// and returns the latest assistant text. Progress callbacks report phases.
// canceled is polled so a client cancellation aborts the wait promptly.
func (e *Executor) promptSession(ctx context.Context, sessionID, prompt, directory string, canceled func() bool, onProgress func(string)) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"id":       "msg_ocb" + time.Now().Format("20060102150405") + "_" + randSuffix(3),
		"prompt":   map[string]any{"text": prompt},
		"delivery": "steer",
		"resume":   true,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.openCodeBase+"/api/session/"+sessionID+"/prompt", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if directory != "" {
		req.Header.Set("x-opencode-directory", directory)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("prompt: %s: %s", resp.Status, string(raw))
	}
	if onProgress != nil {
		onProgress("prompt admitted")
	}

	// Wait for the session to stop generating, then pull the last assistant text.
	deadline := time.Now().Add(10 * time.Minute)
	heartbeat := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if canceled != nil && canceled() {
			return "", errCanceled
		}
		time.Sleep(3 * time.Second)
		busy, err := e.isSessionBusy(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if !busy {
			break
		}
		// 更新进度心跳，但仅约每 15s 一次，避免每轮忙等都写一次 DB。
		heartbeat++
		if onProgress != nil && heartbeat%5 == 0 {
			onProgress("agent working...")
		}
	}

	result, err := e.lastAssistantText(ctx, sessionID)
	if err != nil {
		return "", err
	}
	return result, nil
}

// isSessionBusy reports whether a session is currently generating, by reading
// the /session/status map.
func (e *Executor) isSessionBusy(ctx context.Context, sessionID string) (bool, error) {
	e.busyMu.Lock()
	if entry, ok := e.busyCache[sessionID]; ok && time.Since(entry.at) < busyTTL {
		b := entry.busy
		e.busyMu.Unlock()
		return b, nil
	}
	e.busyMu.Unlock()

	resp, err := e.httpClient.Get(e.openCodeBase + "/session/status")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var statuses map[string]struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &statuses); err != nil {
		return false, err
	}
	st, ok := statuses[sessionID]
	busy := ok && st.Type == "busy"
	e.busyMu.Lock()
	e.busyCache[sessionID] = busyEntry{at: time.Now(), busy: busy}
	// 防止缓存无界增长：仅保留最近访问的少量条目。
	if len(e.busyCache) > 4096 {
		now := time.Now()
		for k, v := range e.busyCache {
			if now.Sub(v.at) > busyTTL {
				delete(e.busyCache, k)
			}
		}
	}
	e.busyMu.Unlock()
	return busy, nil
}

// abortSession asks the upstream to stop generating for a session
// (POST /session/{id}/abort). Best-effort: a cancel that fails to abort just
// leaves the orphan generation running; the task itself is already terminal.
func (e *Executor) abortSession(sessionID, directory string) {
	aCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(aCtx, http.MethodPost, e.openCodeBase+"/session/"+sessionID+"/abort", nil)
	if err != nil {
		return
	}
	if directory != "" {
		req.Header.Set("x-starburst-directory", directory)
	}
	resp, err := e.httpClient.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
}

// findIdleSession looks for an existing idle session in the same working
// directory so a directory-scoped task can reuse it instead of spawning yet
// another session (avoids session sprawl). Only sessions that report idle AND
// whose status is not busy are considered. Returns "" when nothing is reusable.
func (e *Executor) findIdleSession(ctx context.Context, directory string) (string, error) {
	if directory == "" {
		return "", nil
	}
	resp, err := e.httpClient.Get(e.openCodeBase + "/session")
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()

	var sessions []struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
		Status    struct {
			Type string `json:"type"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return "", nil // 列表解析失败保守降级为「新建」
	}
	for _, s := range sessions {
		if s.ID == "" || s.Directory != directory {
			continue
		}
		if s.Status.Type == "busy" {
			continue
		}
		if busy, err := e.isSessionBusy(ctx, s.ID); err == nil && !busy {
			return s.ID, nil
		}
	}
	return "", nil
}

// lastAssistantText loads a session's messages from /session/{id}/message and
// returns the last assistant text block.
func (e *Executor) lastAssistantText(ctx context.Context, sessionID string) (string, error) {
	resp, err := e.httpClient.Get(e.openCodeBase + "/session/" + sessionID + "/message")
	if err != nil {
		return "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("get session messages: %s", resp.Status)
	}

	var doc []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse session messages: %w", err)
	}
	for i := len(doc) - 1; i >= 0; i-- {
		m := doc[i]
		if m.Role != "assistant" {
			continue
		}
		var texts []string
		for _, c := range m.Content {
			if c.Type == "text" && c.Text != "" {
				texts = append(texts, c.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n"), nil
		}
	}
	return "(no assistant text)", nil
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// randSuffix returns n random hex bytes, used to make generated message ids
// unique even when two tasks prompt in the same second.
func randSuffix(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// randIntN returns a non-negative int in [0,n) using math/rand/v2 (non-crypto
// source: this is jitter for idle-claim backoff, not security). Returns 0 when
// n <= 0.
func randIntN(n int) int {
	if n <= 0 {
		return 0
	}
	return mrand.IntN(n)
}

// minDuration returns the smaller of a and b.
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// failureDecision is the LLM's self-healing verdict for a failed task.
type failureDecision struct {
	Action string `json:"action"` // "retry" or "escalate"
	Reason string `json:"reason"`
}

// summarySystem instructs the model to condense a task result to one line.
const summarySystem = `你是任务执行结果摘要助手。用一句话（不超过50字）中文概括任务执行结果的关键信息，直接输出摘要本身，不要任何解释。`

// failureDecisionSystem instructs the model to classify a failure.
const failureDecisionSystem = `你是任务失败决策助手。根据失败信息判断该重试还是升级告警。

只输出一个 JSON 对象：{"action":"retry"或"escalate","reason":"简要中文原因"}
- retry：错误是暂时性的（网络超时、暂时不可用、可重试的偶发故障），值得再试一次
- escalate：错误是确定性的（代码缺陷、配置错误、权限不足、输入错误），重试无意义，应立即告警`

// analyzeFailureSystem instructs the model to produce a root-cause summary.
const analyzeFailureSystem = `你是任务失败根因分析助手。用不超过80字中文说明失败的最可能原因和建议的下一步，直接输出分析文本，不要任何解释。`

// pushSummary asynchronously asks the LLM for a one-line result summary and
// broadcasts it as a task.summary event. No-op when the LLM is unavailable.
func (e *Executor) pushSummary(ctx context.Context, t *store.Task, result string) {
	if e.llm == nil || !e.llm.Enabled() {
		return
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		summary, err := e.llm.Complete(c, summarySystem, "任务指令："+t.Prompt+"\n\n执行结果：\n"+truncateStr(result, 4000))
		if err != nil {
			log.Printf("tasks: summarize %s: %v", t.ID, err)
			return
		}
		_ = e.store.SetTaskAISummary(c, t.ID, summary)
		e.hub.Broadcast(push.Message{
			Type:     "task.summary",
			Payload:  mustJSON(map[string]any{"id": t.ID, "summary": summary}),
			Severity: push.Info,
		})
	}()
}

// pushFailureAnalysis asynchronously asks the LLM for a root-cause analysis
// and broadcasts it as a critical task.failure event. No-op without an LLM.
func (e *Executor) pushFailureAnalysis(ctx context.Context, t *store.Task, errMsg string) {
	if e.llm == nil || !e.llm.Enabled() {
		return
	}
	go func() {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		analysis, err := e.llm.Complete(c, analyzeFailureSystem, "任务指令："+t.Prompt+"\n\n失败信息："+truncateStr(errMsg, 4000))
		if err != nil {
			log.Printf("tasks: analyze failure %s: %v", t.ID, err)
			return
		}
		_ = e.store.SetTaskAISummary(c, t.ID, analysis)
		e.hub.Broadcast(push.Message{
			Type:     "task.failure",
			Payload:  mustJSON(map[string]any{"id": t.ID, "analysis": analysis}),
			Severity: push.Critical,
		})
	}()
}

// decideFailure asks the LLM whether a failed task should be retried or
// escalated, returning the structured decision.
func (e *Executor) decideFailure(ctx context.Context, prompt, errMsg string) (failureDecision, error) {
	var d failureDecision
	err := e.llm.CompleteJSON(ctx, failureDecisionSystem,
		"任务指令："+prompt+"\n\n失败信息："+truncateStr(errMsg, 4000), &d)
	return d, err
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// pushTaskEvent broadcasts one task lifecycle event to all connected clients.
// Clients refresh the task list on any task.event, so dependents that were
// blocked or re-queued are surfaced without polling.
func (e *Executor) pushTaskEvent(status string, payload map[string]any) {
	e.hub.Broadcast(push.Message{
		Type:     "task.event",
		Payload:  mustJSON(payload),
		Severity: severityFor(status),
	})
}

// severityFor maps a task status to a push severity for notification routing.
// The single source of truth is push.SeverityFor so the executor and the HTTP
// layer classify events identically.
func severityFor(status string) string {
	return push.SeverityFor(status)
}
