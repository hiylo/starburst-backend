package tasks

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/push"
	"github.com/hiylo/starburst-backend/internal/store"
)

// concurrencyProbe tracks how many goroutines are inside a critical section at
// the same time and remembers the highest observed count.
type concurrencyProbe struct {
	current int64
	peak    int64
}

func (p *concurrencyProbe) enter() {
	cur := atomic.AddInt64(&p.current, 1)
	for {
		old := atomic.LoadInt64(&p.peak)
		if cur <= old || atomic.CompareAndSwapInt64(&p.peak, old, cur) {
			return
		}
	}
}

func (p *concurrencyProbe) leave() { atomic.AddInt64(&p.current, -1) }

func (p *concurrencyProbe) Peak() int64 { return atomic.LoadInt64(&p.peak) }

// newTestEnv wires a store, hub and fake upstream into an executor.
func newTestEnv(t *testing.T, upstream http.HandlerFunc) (*Executor, store.Store, *push.Hub) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)

	dsn := store.SQLiteDSN(filepath.Join(t.TempDir(), "test.db"))
	st, err := store.OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	hub := push.NewHub()
	go hub.Run()

	exec := NewExecutor(st, hub, up.URL)
	return exec, st, hub
}

// fakeUpstream returns a handler simulating createSession + admitted prompt +
// idle session + assistant message.
func fakeUpstream(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_test123","directory":"/w"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_test123/prompt":
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_x","sessionID":"ses_test123"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			_, _ = w.Write([]byte(`{"ses_test123":{"type":"idle"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_test123/message":
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"这是结果"}]}]`))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestExecutorRunsTaskToCompletion(t *testing.T) {
	exec, st, _ := newTestEnv(t, fakeUpstream(t))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Seed a task.
	task := &store.Task{ID: "task_1", Directory: "/w", Prompt: "run something"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Run the worker loop in the background briefly.
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	// Wait for completion.
	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "task_1")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil {
		t.Fatalf("task not found")
	}
	if got.Status != store.TaskSucceeded {
		t.Fatalf("status %q want %q (err=%s)", got.Status, store.TaskSucceeded, got.Error)
	}
	if got.Result != "这是结果" {
		t.Fatalf("result %q want %q", got.Result, "这是结果")
	}
	if got.SessionID != "ses_test123" {
		t.Fatalf("session id %q not written back", got.SessionID)
	}
}

func TestExecutorFailureMarksTaskFailed(t *testing.T) {
	// Upstream always 500s on prompt. Retries disabled so the failure surfaces
	// immediately as failed (with retries enabled the task would park in backoff
	// and stay queued, which the retry test below exercises instead).
	exec0, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_fail"}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	})
	exec := exec0.WithMaxRetries(0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task := &store.Task{ID: "task_fail", Directory: "/w", Prompt: "will fail"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "task_fail")
		if got != nil && got.Status != store.TaskQueued && got.Status != store.TaskRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil {
		t.Fatalf("task not found")
	}
	if got.Status != store.TaskFailed {
		t.Fatalf("status %q want %q", got.Status, store.TaskFailed)
	}
	if got.Error == "" {
		t.Fatalf("expected non-empty error")
	}
}

// TestExecutorRetriesTransientFailure verifies a task that fails on its first
// attempt is actually re-queued (backoff + second attempt) instead of being
// permanently failed. This regresses the RetryTask status-precondition bug where
// a running task failed the `status = failed` guard and skipped retry entirely.
func TestExecutorRetriesTransientFailure(t *testing.T) {
	attempts := 0
	envExec, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			_, _ = w.Write([]byte(`{"id":"ses_retry"}`))
			return
		}
		if attempts <= 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/session/ses_retry/message":
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"这是结果"}]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			_, _ = w.Write([]byte(`{"ses_retry":{"type":"idle"}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_x","sessionID":"ses_retry"}}`))
		}
	})
	exec := envExec.WithMaxRetries(2)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "task_retry", Directory: "/w", Prompt: "flaky"}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var got *store.Task
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "task_retry")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil || got.Status != store.TaskSucceeded {
		t.Fatalf("task should succeed after retry, status=%v", func() any {
			if got == nil {
				return "nil"
			}
			return got.Status
		}())
	}
	if attempts < 2 {
		t.Fatalf("expected at least 2 execution attempts, got %d", attempts)
	}
}

func TestExecutorPromotesDependents(t *testing.T) {
	exec, st, _ := newTestEnv(t, fakeUpstream(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "task_up", Prompt: "upstream"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &store.Task{ID: "task_dn", Prompt: "downstream", DependsOn: "task_up"}, store.TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	// The dependent must travel pending -> queued -> succeeded on its own.
	var dn *store.Task
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		dn, _ = st.GetTask(ctx, "task_dn")
		if dn != nil && dn.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done

	if dn == nil || dn.Status != store.TaskSucceeded {
		t.Fatalf("dependent was not promoted and run: %+v", dn)
	}
}

func TestExecutorBlocksDependents(t *testing.T) {
	exec, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			_, _ = w.Write([]byte(`{"id":"ses_fail"}`))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	exec.WithMaxRetries(0)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "task_up_fail", Prompt: "upstream"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &store.Task{ID: "task_dn_fail", Prompt: "downstream", DependsOn: "task_up_fail"}, store.TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var up *store.Task
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		up, _ = st.GetTask(ctx, "task_up_fail")
		if up != nil && up.Status == store.TaskFailed {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Read the dependent before cancelling: blocking happens right after the
	// upstream is marked failed, so it is already settled at this point.
	dn, err := st.GetTask(ctx, "task_dn_fail")
	cancel()
	<-done

	if up == nil || up.Status != store.TaskFailed {
		t.Fatalf("upstream did not fail: %+v", up)
	}
	if err != nil {
		t.Fatalf("get dependent: %v", err)
	}
	if dn.Status != store.TaskBlocked {
		t.Fatalf("dependent status = %q, want blocked", dn.Status)
	}
	if dn.Error == "" {
		t.Fatalf("blocked dependent should record a reason")
	}
}

// TestExecutorRepromotesBlockedDependents covers the human-intervention path:
// a dependent was blocked because its upstream failed, then a human retries the
// upstream and it succeeds — the blocked dependent must become runnable again
// instead of staying blocked forever.
func TestExecutorRepromotesBlockedDependents(t *testing.T) {
	exec, st, _ := newTestEnv(t, fakeUpstream(t))
	ctx := context.Background()

	if err := st.CreateTask(ctx, &store.Task{ID: "task_up2", Prompt: "upstream"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &store.Task{ID: "task_b2", Prompt: "downstream", DependsOn: "task_up2"}, store.TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	// Upstream fails: the dependent is blocked with a reason.
	exec.resolveDependents(ctx, "task_up2", false, "upstream failed")
	blocked, err := st.GetTask(ctx, "task_b2")
	if err != nil {
		t.Fatalf("get dependent: %v", err)
	}
	if blocked.Status != store.TaskBlocked || blocked.Error != "upstream failed" {
		t.Fatalf("dependent = %+v, want blocked with reason", blocked)
	}

	// Human retries the upstream and it now succeeds: the blocked dependent is
	// re-queued and its block reason is cleared.
	exec.resolveDependents(ctx, "task_up2", true, "")
	released, err := st.GetTask(ctx, "task_b2")
	if err != nil {
		t.Fatalf("get dependent: %v", err)
	}
	if released.Status != store.TaskQueued {
		t.Fatalf("dependent status = %q, want queued", released.Status)
	}
	if released.Error != "" {
		t.Fatalf("block reason should be cleared, got %q", released.Error)
	}

	// Nothing left to release.
	exec.resolveDependents(ctx, "task_up2", true, "")
	stats, err := st.TaskStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Blocked != 0 {
		t.Fatalf("blocked count = %d, want 0", stats.Blocked)
	}
}

func TestExecutorReusesExistingSession(t *testing.T) {
	upstream := fakeUpstream(t)
	exec, st, _ := newTestEnv(t, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Task already targets an existing session; it must not call createSession.
	task := &store.Task{ID: "task_existing", SessionID: "ses_test123", Prompt: "use existing"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()
	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "task_existing")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
	if got == nil || got.Status != store.TaskSucceeded {
		t.Fatalf("task did not succeed: %+v", got)
	}
}

func TestParsePromptResponseEmptyText(t *testing.T) {
	// Ensure a session with no assistant messages returns the sentinel.
	upstream := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_empty"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_empty/prompt":
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1}}`))
		case r.URL.Path == "/session/status":
			_, _ = w.Write([]byte(`{"ses_empty":{"type":"idle"}}`))
		case r.URL.Path == "/session/ses_empty/message":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}
	exec, st, _ := newTestEnv(t, upstream)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	task := &store.Task{ID: "task_empty", Prompt: "hello"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()
	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "task_empty")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done
	if got == nil || got.Status != store.TaskSucceeded {
		t.Fatalf("did not succeed: %+v", got)
	}
	if got.Result != "(no assistant text)" {
		t.Fatalf("result %q", got.Result)
	}
}
func TestSeverityMapping(t *testing.T) {
	cases := map[string]string{
		"failed":    push.Critical,
		"retrying":  push.Warning,
		"blocked":   push.Warning,
		"running":   push.Info,
		"succeeded": push.Info,
	}
	for status, want := range cases {
		if got := severityFor(status); got != want {
			t.Fatalf("severityFor(%q) = %q, want %q", status, got, want)
		}
	}
}

// TestExecutorRunsTasksInParallel verifies the worker pool actually executes
// tasks concurrently instead of serializing them one by one.
func TestExecutorRunsTasksInParallel(t *testing.T) {
	var probe concurrencyProbe
	var sessions int64
	upstream := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			n := atomic.AddInt64(&sessions, 1)
			_, _ = fmt.Fprintf(w, `{"id":"ses_%d","directory":"/w"}`, n)
		case r.Method == http.MethodGet && r.URL.Path == "/session/status":
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/prompt"):
			probe.enter()
			time.Sleep(300 * time.Millisecond)
			probe.leave()
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"msg_p","sessionID":"ses_1"}}`))
		default: // GET /session/{id}/message
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"并行结果"}]}]`))
		}
	}

	exec, st, _ := newTestEnv(t, upstream)
	exec.WithWorkers(4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const n = 4
	for i := 1; i <= n; i++ {
		// 不同目录才能并行（同目录已被目录互斥串行化）。
		if err := st.CreateTask(ctx, &store.Task{ID: fmt.Sprintf("task_par%d", i), Directory: fmt.Sprintf("/w%d", i), Prompt: "parallel"}); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := st.TaskStats(ctx)
		if err == nil && stats.Succeeded == n {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done

	if got := probe.Peak(); got < 2 {
		t.Fatalf("tasks ran serially: peak concurrent executions = %d", got)
	}
	if got := atomic.LoadInt64(&sessions); got != n {
		t.Fatalf("created %d upstream sessions, want %d (each task should get its own)", got, n)
	}
}

// TestExecutorSerializesSameDirectory verifies that tasks creating a new
// session in the SAME working directory never overlap (per-directory mutex).
func TestExecutorSerializesSameDirectory(t *testing.T) {
	var probe concurrencyProbe
	upstream := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = fmt.Fprintf(w, `{"id":"ses_x","directory":"/w"}`)
		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/prompt"):
			probe.enter()
			time.Sleep(200 * time.Millisecond)
			probe.leave()
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"m","sessionID":"ses_x"}}`))
		default:
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]`))
		}
	}
	exec, st, _ := newTestEnv(t, upstream)
	exec.WithWorkers(4)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := 1; i <= 3; i++ {
		if err := st.CreateTask(ctx, &store.Task{ID: fmt.Sprintf("task_dir%d", i), Directory: "/w", Prompt: "p"}); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); exec.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := st.TaskStats(ctx)
		if err == nil && stats.Succeeded == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done
	if got := probe.Peak(); got > 1 {
		t.Fatalf("same-directory tasks overlapped: peak concurrent executions = %d", got)
	}
}

// TestExecutorConcurrencyCap verifies the global running-task cap.
func TestExecutorConcurrencyCap(t *testing.T) {
	var probe concurrencyProbe
	upstream := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = fmt.Fprintf(w, `{"id":"ses_cap","directory":"/w"}`)
		case strings.HasPrefix(r.URL.Path, "/api/session/") && strings.HasSuffix(r.URL.Path, "/prompt"):
			probe.enter()
			time.Sleep(200 * time.Millisecond)
			probe.leave()
			_, _ = w.Write([]byte(`{"data":{"admittedSeq":1,"id":"m","sessionID":"ses_cap"}}`))
		default:
			_, _ = w.Write([]byte(`[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]`))
		}
	}
	exec, st, _ := newTestEnv(t, upstream)
	exec.WithWorkers(6).WithConcurrencyCap(2)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := 1; i <= 4; i++ {
		if err := st.CreateTask(ctx, &store.Task{ID: fmt.Sprintf("task_cap%d", i), Directory: fmt.Sprintf("/w%d", i), Prompt: "p"}); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); exec.Run(ctx) }()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := st.TaskStats(ctx)
		if err == nil && stats.Succeeded == 4 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	<-done
	if got := probe.Peak(); got > 2 {
		t.Fatalf("global concurrency cap = 2 but observed peak %d", got)
	}
}

// TestSessionGateSerializes verifies that executions sharing one upstream
// session id never overlap, and that released gates are dropped from the map so
// it does not grow without bound.
func TestSessionGateSerializes(t *testing.T) {
	exec := &Executor{gates: make(map[string]*sessionGate)}
	var probe concurrencyProbe
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g := exec.hold("ses_shared", exec.gates)
			defer exec.release(exec.gates, g)
			g.mu.Lock()
			defer g.mu.Unlock()
			probe.enter()
			time.Sleep(20 * time.Millisecond)
			probe.leave()
		}()
	}
	wg.Wait()

	if got := probe.Peak(); got != 1 {
		t.Fatalf("peak concurrency = %d, want 1 (shared session must be serialized)", got)
	}
	exec.gateMu.Lock()
	left := len(exec.gates)
	exec.gateMu.Unlock()
	if left != 0 {
		t.Fatalf("%d session gate(s) left after all holders released", left)
	}
	if g := exec.hold("", exec.gates); g != nil {
		t.Fatal("tasks without an explicit session id must not be serialized")
	}
}

// TestExecutorPurgeOnceKeepsFreshTasks guards the janitor against nuking recent
// work: a pass with a one-day window must leave tasks created moments ago alone
// and report nothing deleted. Timestamp arithmetic itself is covered by the
// store-level retention tests.
func TestExecutorPurgeOnceKeepsFreshTasks(t *testing.T) {
	e, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	e.WithRetention(24 * time.Hour)

	ctx := context.Background()
	for _, id := range []string{"fresh-a", "fresh-b"} {
		if err := st.CreateTaskWithStatus(ctx, &store.Task{ID: id, Prompt: id}, store.TaskSucceeded); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	deleted, kept, err := e.purgeOnce(ctx)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 0 || kept != 0 {
		t.Fatalf("fresh tasks touched: deleted=%d kept=%d", deleted, kept)
	}
	for _, id := range []string{"fresh-a", "fresh-b"} {
		if _, err := st.GetTask(ctx, id); err != nil {
			t.Fatalf("%s disappeared: %v", id, err)
		}
	}
}

// TestExecutorIntelRunnerExecutesKindTask verifies a kind=test-run tracking task
// is executed by the injected intel runner (never the prompt-session/upstream
// path) and completed with the callback's result.
func TestExecutorIntelRunnerExecutesKindTask(t *testing.T) {
	exec, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // kind tasks must not touch the OpenCode upstream
	})
	runs := 0
	exec.WithIntelRunner(func(ctx context.Context, tk *store.Task) (string, error) {
		runs++
		if tk.Kind != "test-run" {
			t.Errorf("runner got kind %q, want test-run", tk.Kind)
		}
		if tk.Prompt != `{"runId":7}` {
			t.Errorf("runner prompt = %q", tk.Prompt)
		}
		return "intel summary", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "kind1", Kind: "test-run", Prompt: `{"runId":7}`}); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "kind1")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil || got.Status != store.TaskSucceeded {
		t.Fatalf("kind task did not succeed: %+v", got)
	}
	if got.Result != "intel summary" {
		t.Fatalf("result = %q, want intel summary", got.Result)
	}
	if runs != 1 {
		t.Fatalf("intel runner called %d times, want 1", runs)
	}
}

// TestExecutorKindTaskNoRunnerFails verifies the defensive path: a kind=test-run
// task without a wired intel runner is marked failed instead of stuck in limbo.
func TestExecutorKindTaskNoRunnerFails(t *testing.T) {
	exec, st, _ := newTestEnv(t, http.NotFound)
	exec.WithMaxRetries(0)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "kind_fail", Kind: "test-run", Prompt: `{"runId":8}`}); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "kind_fail")
		if got != nil && got.Status != store.TaskQueued && got.Status != store.TaskRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil || got.Status != store.TaskFailed {
		t.Fatalf("status = %q, want failed", func() any {
			if got == nil {
				return "nil"
			}
			return got.Status
		}())
	}
	if !strings.Contains(got.Error, "intel runner") {
		t.Fatalf("error = %q, want it to mention the intel runner", got.Error)
	}
}

// TestExecutorDocRunnerExecutesKindTask verifies kind=doc-generate tasks are
// dispatched to the injected doc runner (not the intel runner) by the dedicated
// doc worker, and that the result is stored.
func TestExecutorDocRunnerExecutesKindTask(t *testing.T) {
	exec, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // doc tasks must not touch the OpenCode upstream
	})
	runs := 0
	exec.WithDocRunner(func(ctx context.Context, tk *store.Task) (string, error) {
		runs++
		if tk.Kind != "doc-generate" {
			t.Errorf("doc runner got kind %q, want doc-generate", tk.Kind)
		}
		if tk.Prompt != `{"type":"xlsx","prompt":"排期表"}` {
			t.Errorf("doc runner prompt = %q", tk.Prompt)
		}
		return "文档已生成 #1（排期表）", nil
	})
	// 只注册 doc runner，不注册 intel runner：若 kind 分发错会走 intel 的
	// 防御性失败，测试即失败。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := st.CreateTask(ctx, &store.Task{ID: "doc1", Kind: "doc-generate", Prompt: `{"type":"xlsx","prompt":"排期表"}`}); err != nil {
		t.Fatalf("create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	var got *store.Task
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ = st.GetTask(ctx, "doc1")
		if got != nil && got.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if got == nil || got.Status != store.TaskSucceeded {
		t.Fatalf("doc-generate task did not succeed: %+v", got)
	}
	if got.Result != "文档已生成 #1（排期表）" {
		t.Fatalf("result = %q", got.Result)
	}
	if runs != 1 {
		t.Fatalf("doc runner called %d times, want 1", runs)
	}
}

// TestExecutorDocRunnerDispatchIsKindSpecific verifies test-run and doc-generate
// go to their own runners and never cross-dispatch.
func TestExecutorDocRunnerDispatchIsKindSpecific(t *testing.T) {
	exec, st, _ := newTestEnv(t, http.NotFound)
	intelRuns, docRuns := 0, 0
	exec.WithIntelRunner(func(ctx context.Context, tk *store.Task) (string, error) {
		intelRuns++
		if tk.Kind != "test-run" {
			t.Errorf("intel runner got kind %q", tk.Kind)
		}
		return "intel ok", nil
	})
	exec.WithDocRunner(func(ctx context.Context, tk *store.Task) (string, error) {
		docRuns++
		if tk.Kind != "doc-generate" {
			t.Errorf("doc runner got kind %q", tk.Kind)
		}
		return "doc ok", nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_ = st.CreateTask(ctx, &store.Task{ID: "d1", Kind: "doc-generate", Prompt: "{}"})
	_ = st.CreateTask(ctx, &store.Task{ID: "i1", Kind: "test-run", Prompt: "{}"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		exec.Run(ctx)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a, _ := st.GetTask(ctx, "d1")
		b, _ := st.GetTask(ctx, "i1")
		if a != nil && a.Status == store.TaskSucceeded && b != nil && b.Status == store.TaskSucceeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	if docRuns != 1 || intelRuns != 1 {
		t.Fatalf("docRuns=%d intelRuns=%d, want 1/1", docRuns, intelRuns)
	}
}
