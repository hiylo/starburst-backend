package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hiylo/opencode-backend/internal/push"
	"github.com/hiylo/opencode-backend/internal/store"
)

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
	// Upstream always 500s on prompt.
	exec, st, _ := newTestEnv(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			_, _ = w.Write([]byte(`{"id":"ses_fail"}`))
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	})
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
		"running":   push.Info,
		"succeeded": push.Info,
	}
	for status, want := range cases {
		if got := severityFor(status); got != want {
			t.Fatalf("severityFor(%q) = %q, want %q", status, got, want)
		}
	}
}
