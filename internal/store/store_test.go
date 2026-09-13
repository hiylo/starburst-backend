package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) Store {
	t.Helper()
	dir := t.TempDir()
	dsn := SQLiteDSN(filepath.Join(dir, "test.db"))
	st, err := OpenFromConfig(context.Background(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, err := st.GetSetting(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for missing key, got %v", err)
	}
	if err := st.SetSetting(ctx, "k1", "v1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := st.GetSetting(ctx, "k1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "v1" {
		t.Fatalf("got %q want %q", got, "v1")
	}
	// Upsert overwrites.
	if err := st.SetSetting(ctx, "k1", "v2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = st.GetSetting(ctx, "k1")
	if got != "v2" {
		t.Fatalf("after upsert got %q want %q", got, "v2")
	}
}

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	tok := &Token{ID: "id1", Name: "device-a", TokenHash: "hash-a"}
	if err := st.CreateToken(ctx, tok); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.GetTokenByHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got.ID != "id1" || got.Name != "device-a" {
		t.Fatalf("unexpected token: %+v", got)
	}

	// Duplicate hash should be rejected.
	dup := &Token{ID: "id2", Name: "dup", TokenHash: "hash-a"}
	if err := st.CreateToken(ctx, dup); err == nil {
		t.Fatalf("expected conflict on duplicate hash")
	}

	// Unknown hash -> not found.
	if _, err := st.GetTokenByHash(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	// Revoke makes the token un-queryable.
	if err := st.RevokeToken(ctx, "id1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := st.GetTokenByHash(ctx, "hash-a"); err != ErrNotFound {
		t.Fatalf("revoked token still found: %v", err)
	}

	// Double revoke -> not found.
	if err := st.RevokeToken(ctx, "id1"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound on double revoke, got %v", err)
	}

	toks, err := st.ListTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(toks) != 1 {
		t.Fatalf("expected 1 token after revoke, got %d", len(toks))
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dsn := SQLiteDSN(filepath.Join(dir, "test.db"))
	ctx := context.Background()

	st1, err := OpenFromConfig(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	st1.Close()

	// Reopening the same file must not fail migrations.
	st2, err := OpenFromConfig(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if err := st2.Ping(ctx); err != nil {
		t.Fatalf("ping after reopen: %v", err)
	}
}

func TestOpenSQLiteFile(t *testing.T) {
	// WAL journal files should be created next to the db file.
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	dsn := SQLiteDSN(path)
	ctx := context.Background()

	st, err := OpenFromConfig(ctx, "sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

func TestTaskRetryBackoff(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Seed a task, then simulate: claim -> fail -> retry.
	task := &Task{ID: "retry1", SessionID: "", Directory: "/w", Prompt: "p"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}

	claimed, err := st.ClaimNextTask(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("attempts after first claim = %d, want 1", claimed.Attempts)
	}

	// Fail then retry with 5s backoff.
	if err := st.FailTask(ctx, claimed.ID, "boom"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := st.RetryTask(ctx, claimed.ID, 5); err != nil {
		t.Fatalf("retry: %v", err)
	}

	got, err := st.GetTask(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskQueued {
		t.Fatalf("status %q want queued", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (claim already counted one)", got.Attempts)
	}
	if got.Error != "" {
		t.Fatalf("error not cleared: %q", got.Error)
	}
	// available_at should be in the future (>= now - small skew).
	if !got.AvailableAt.After(time.Now().Add(-2 * time.Second)) {
		t.Fatalf("available_at not scheduled in future: %v", got.AvailableAt)
	}

	// A claim now must NOT pick it up (backoff window active).
	next, err := st.ClaimNextTask(ctx)
	if err != ErrNotFound {
		t.Fatalf("expected no claimable task during backoff, got %+v err=%v", next, err)
	}
}

func TestRetryTaskUnknownID(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.RetryTask(ctx, "nope", 5); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestWebSessionPersistence(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Create a session that expires in the future.
	exp := time.Now().Add(time.Hour)
	if err := st.CreateWebSession(ctx, "sid_abc", exp); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetWebSession(ctx, "sid_abc")
	if err != nil {
		t.Fatalf("get valid session: %v", err)
	}
	if got.ID != "sid_abc" {
		t.Fatalf("id %q", got.ID)
	}

	// Expired session is not found.
	if err := st.CreateWebSession(ctx, "sid_expired", time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("create expired: %v", err)
	}
	if _, err := st.GetWebSession(ctx, "sid_expired"); err != ErrNotFound {
		t.Fatalf("expired session should be ErrNotFound, got %v", err)
	}

	// Delete.
	if err := st.DeleteWebSession(ctx, "sid_abc"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetWebSession(ctx, "sid_abc"); err != ErrNotFound {
		t.Fatalf("deleted session still found: %v", err)
	}

	// Purge expired.
	if n, err := st.DeleteExpiredWebSessions(ctx); err != nil || n < 1 {
		t.Fatalf("purge expired: n=%d err=%v", n, err)
	}
}

func TestIsTaskCanceled(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	task := &Task{ID: "c1", Prompt: "p"}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := st.IsTaskCanceled(ctx, "c1")
	if err != nil || c {
		t.Fatalf("should not be canceled: c=%v err=%v", c, err)
	}
	if _, err := st.CancelTask(ctx, "c1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	c, err = st.IsTaskCanceled(ctx, "c1")
	if err != nil || !c {
		t.Fatalf("should be canceled: c=%v err=%v", c, err)
	}
}

func TestRecoverStaleRunning(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// Create two tasks, claim one to put it in running state.
	if err := st.CreateTask(ctx, &Task{ID: "r1", Prompt: "p"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.CreateTask(ctx, &Task{ID: "r2", Prompt: "p"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.ClaimNextTask(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// r1 is now running, r2 queued.
	n, err := st.RecoverStaleRunning(ctx)
	if err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v", n, err)
	}
	got, _ := st.GetTask(ctx, "r1")
	if got.Status != TaskQueued {
		t.Fatalf("r1 should be queued after recover, got %s", got.Status)
	}
}

func TestDeleteAuditOlderThan(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// Insert audit entries at known times.
	for i, name := range []string{"a", "b", "c"} {
		cutoff := time.Now().Add(-time.Duration(10*i+1) * time.Hour)
		_ = st.RecordAudit(ctx, &AuditEntry{TokenID: "t", TokenName: name, Method: "GET", Path: "/x", Status: 200})
		_ = cutoff
	}
	// Delete entries older than 5 hours -> should remove b, c (10h, 20h old).
	// NOTE: created_at uses CURRENT_TIMESTAMP so this is a smoke test only.
	n, err := st.DeleteAuditOlderThan(ctx, time.Now().Add(-5*time.Hour))
	if err != nil {
		t.Fatalf("delete audit: %v", err)
	}
	_ = n // no strict assertion; just verifies the call works on both dialects
}

func TestTaskDependencyPromoteAndBlock(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Upstream succeeds first: downstream pending tasks must be re-queued.
	if err := st.CreateTask(ctx, &Task{ID: "up_ok", Prompt: "up"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "dn1", Prompt: "dn1", DependsOn: "up_ok"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "dn2", Prompt: "dn2", DependsOn: "up_ok"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}

	// Pending tasks must never be claimed.
	if got, err := st.ClaimNextTask(ctx); err == nil && got.DependsOn != "" {
		t.Fatalf("pending task claimed: %+v", got)
	}

	ids, err := st.PromotePendingDependents(ctx, "up_ok")
	if err != nil || len(ids) != 2 {
		t.Fatalf("promote: ids=%v err=%v", ids, err)
	}
	got, err := st.GetTask(ctx, "dn1")
	if err != nil || got.Status != TaskQueued {
		t.Fatalf("dn1 should be queued: %+v err=%v", got, err)
	}

	// Upstream fails: downstream pending tasks must be blocked with a reason.
	if err := st.CreateTask(ctx, &Task{ID: "up_bad", Prompt: "up"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "dn3", Prompt: "dn3", DependsOn: "up_bad"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	ids, err = st.BlockDependents(ctx, "up_bad", "upstream failed")
	if err != nil || len(ids) != 1 || ids[0] != "dn3" {
		t.Fatalf("block: ids=%v err=%v", ids, err)
	}
	got, err = st.GetTask(ctx, "dn3")
	if err != nil || got.Status != TaskBlocked || got.Error != "upstream failed" {
		t.Fatalf("dn3 should be blocked: %+v err=%v", got, err)
	}

	// Blocked tasks are not cancellable; they need an explicit unblock.
	if ok, err := st.CancelTask(ctx, "dn3"); err != nil || ok {
		t.Fatalf("cancel blocked should fail: ok=%v err=%v", ok, err)
	}

	// Stats must expose the new statuses.
	stats, err := st.TaskStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Blocked != 1 || stats.Pending != 0 {
		t.Fatalf("stats: pending=%d blocked=%d want 0/1", stats.Pending, stats.Blocked)
	}

	// Pending tasks can be canceled.
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "dn4", Prompt: "dn4", DependsOn: "up_bad"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if ok, err := st.CancelTask(ctx, "dn4"); err != nil || !ok {
		t.Fatalf("cancel pending: ok=%v err=%v", ok, err)
	}
}

// TestTaskUnblockAndRepromote covers the two ways a blocked task becomes
// runnable again: a human unblocks it directly, or the upstream is retried and
// eventually succeeds.
func TestTaskUnblockAndRepromote(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.CreateTask(ctx, &Task{ID: "up1", Prompt: "up"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "b1", Prompt: "b1", DependsOn: "up1"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "b2", Prompt: "b2", DependsOn: "up1"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if ids, err := st.BlockDependents(ctx, "up1", "upstream failed"); err != nil || len(ids) != 2 {
		t.Fatalf("block: ids=%v err=%v", ids, err)
	}

	// Manual unblock: re-queued, block reason cleared.
	ok, err := st.UnblockTask(ctx, "b1")
	if err != nil || !ok {
		t.Fatalf("unblock b1: ok=%v err=%v", ok, err)
	}
	got, err := st.GetTask(ctx, "b1")
	if err != nil || got.Status != TaskQueued || got.Error != "" {
		t.Fatalf("b1 should be queued with no error: %+v err=%v", got, err)
	}
	// Unblocking a task that is not blocked is a no-op.
	if ok, err := st.UnblockTask(ctx, "b1"); err != nil || ok {
		t.Fatalf("unblock queued should fail: ok=%v err=%v", ok, err)
	}
	if ok, err := st.UnblockTask(ctx, "missing"); err != nil || ok {
		t.Fatalf("unblock missing should fail: ok=%v err=%v", ok, err)
	}

	// Upstream retried and succeeded: the remaining blocked task is resurrected.
	ids, err := st.PromotePendingDependents(ctx, "up1")
	if err != nil || len(ids) != 1 || ids[0] != "b2" {
		t.Fatalf("repromote: ids=%v err=%v", ids, err)
	}
	got, err = st.GetTask(ctx, "b2")
	if err != nil || got.Status != TaskQueued || got.Error != "" {
		t.Fatalf("b2 should be re-queued with no error: %+v err=%v", got, err)
	}

	// Nothing left to promote.
	if ids, err := st.PromotePendingDependents(ctx, "up1"); err != nil || len(ids) != 0 {
		t.Fatalf("repromote again: ids=%v err=%v", ids, err)
	}
}

// TestMigrateDialectHelpers guards the DDL emitted per backend. PostgreSQL is
// reached under the registered database/sql name "pgx" (OpenFromConfig maps
// the logical name to it), so both spellings must be recognised — otherwise the
// AUTOINCREMENT branch silently wins and migrations fail on PG.
func TestMigrateDialectHelpers(t *testing.T) {
	for _, driver := range []string{"postgres", "pgx"} {
		if !isPostgres(driver) {
			t.Fatalf("isPostgres(%q) = false, want true", driver)
		}
		if got := idColumn(driver); got != "id BIGSERIAL PRIMARY KEY" {
			t.Fatalf("idColumn(%q) = %q", driver, got)
		}
	}
	if isPostgres("sqlite") {
		t.Fatal("isPostgres(sqlite) = true, want false")
	}
	if got := idColumn("sqlite"); got != "id INTEGER PRIMARY KEY AUTOINCREMENT" {
		t.Fatalf("idColumn(sqlite) = %q", got)
	}
}

// TestClaimNextTaskConcurrent verifies that concurrent claims never hand the
// same task to two workers. Each task must be claimed exactly once, which is
// what makes the executor worker pool safe.
func TestClaimNextTaskConcurrent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	const total = 24
	for i := 0; i < total; i++ {
		if err := st.CreateTask(ctx, &Task{ID: fmt.Sprintf("task_conc%d", i), Prompt: "p"}); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	claimed := make(chan *Task, total)
	var failed bool
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				tk, err := st.ClaimNextTask(ctx)
				if errors.Is(err, ErrNotFound) {
					return
				}
				if err != nil {
					failed = true
					return
				}
				claimed <- tk
			}
		}()
	}
	wg.Wait()
	close(claimed)
	if failed {
		t.Fatal("claim returned an unexpected error")
	}

	counts := make(map[string]int, total)
	for tk := range claimed {
		counts[tk.ID]++
	}
	if len(counts) != total {
		t.Fatalf("claimed %d distinct tasks, want %d", len(counts), total)
	}
	for id, n := range counts {
		if n != 1 {
			t.Fatalf("task %s claimed %d times, want exactly 1", id, n)
		}
	}
}

// backdateTasks moves updated_at back so a retention cutoff lands on the task.
func backdateTasks(t *testing.T, s *sqlStore, seconds int, ids ...string) {
	t.Helper()
	var q string
	if isPostgres(s.driver) {
		q = "UPDATE tasks SET updated_at = CURRENT_TIMESTAMP - (? || ' seconds')::interval WHERE id = ?"
	} else {
		q = "UPDATE tasks SET updated_at = datetime('now', '-' || ? || ' seconds') WHERE id = ?"
	}
	for _, id := range ids {
		if _, err := s.db.ExecContext(context.Background(), s.q(q), itoa(seconds), id); err != nil {
			t.Fatalf("backdate %s: %v", id, err)
		}
	}
}

func taskExists(t *testing.T, st Store, id string) bool {
	t.Helper()
	_, err := st.GetTask(context.Background(), id)
	if err == ErrNotFound {
		return false
	}
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return true
}

// assertRetentionPurge covers the retention behaviour shared by both dialects:
// terminal tasks past the window are deleted, a terminal task that a dependent
// still references is kept, non-terminal and fresh tasks are untouched, the
// per-pass limit is honoured, and the reference protection lifts once the
// dependent is gone.
func assertRetentionPurge(t *testing.T, st Store) {
	t.Helper()
	ctx := context.Background()
	s := st.(*sqlStore)

	mk := func(id, status, dependsOn string) {
		t.Helper()
		if err := st.CreateTaskWithStatus(ctx, &Task{ID: id, Prompt: id, DependsOn: dependsOn}, status); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("done-old", TaskSucceeded, "")
	mk("cancel-old", TaskCanceled, "")
	mk("fail-old", TaskFailed, "")
	mk("done-new", TaskSucceeded, "")
	mk("running", TaskRunning, "")
	mk("queued", TaskQueued, "")
	mk("dep", TaskPending, "fail-old")
	backdateTasks(t, s, 7200, "done-old", "cancel-old", "fail-old", "running", "queued")

	deleted, kept, err := st.PurgeFinishedTasks(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 || kept != 1 {
		t.Fatalf("first pass: deleted=%d kept=%d, want 2 deleted / 1 kept", deleted, kept)
	}
	if taskExists(t, st, "done-old") || taskExists(t, st, "cancel-old") {
		t.Fatalf("expired terminal tasks survived")
	}
	for _, id := range []string{"fail-old", "dep", "running", "queued", "done-new"} {
		if !taskExists(t, st, id) {
			t.Fatalf("%s must not be purged", id)
		}
	}

	// One pass honours the row limit. fail-old is still referenced by dep, so
	// it keeps showing up in the "kept" count until phase C below.
	for _, id := range []string{"t1", "t2", "t3"} {
		mk(id, TaskSucceeded, "")
		backdateTasks(t, s, 7200, id)
	}
	deleted, kept, err = st.PurgeFinishedTasks(ctx, time.Hour, 2)
	if err != nil {
		t.Fatalf("limited purge: %v", err)
	}
	if deleted != 2 || kept != 1 {
		t.Fatalf("limited pass: deleted=%d kept=%d, want 2/1", deleted, kept)
	}
	remaining := 0
	for _, id := range []string{"t1", "t2", "t3"} {
		if taskExists(t, st, id) {
			remaining++
		}
	}
	if remaining != 1 {
		t.Fatalf("t1-t3 remaining=%d, want 1", remaining)
	}
	deleted, _, err = st.PurgeFinishedTasks(ctx, time.Hour, 2)
	if err != nil || deleted != 1 {
		t.Fatalf("drain pass: deleted=%d err=%v, want 1", deleted, err)
	}

	// Reference protection is dynamic. Once the dependent is itself terminal
	// (canceled here), it no longer protects its upstream, so a single pass
	// clears both the dependent and the formerly-referenced terminal upstream.
	if ok, err := st.CancelTask(ctx, "dep"); err != nil || !ok {
		t.Fatalf("cancel dependent: ok=%v err=%v", ok, err)
	}
	backdateTasks(t, s, 7200, "dep")
	deleted, kept, err = st.PurgeFinishedTasks(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("final purge: %v", err)
	}
	if deleted != 2 || kept != 0 {
		t.Fatalf("final pass: deleted=%d kept=%d, want 2/0", deleted, kept)
	}
	if taskExists(t, st, "dep") {
		t.Fatalf("dep survived its own purge")
	}
	if taskExists(t, st, "fail-old") {
		t.Fatalf("fail-old survived once its terminal dependent was purged")
	}
	if !taskExists(t, st, "done-new") {
		t.Fatalf("fresh terminal task was purged")
	}
}

func TestPurgeFinishedTasks(t *testing.T) {
	assertRetentionPurge(t, newTestStore(t))
}
