//go:build pgtest

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgreSQL is an optional backend that CI never exercises, so its dialect path
// is only covered when a developer opts in explicitly. Keep the password out of
// the DSN (PGPASSWORD / .pgpass) — AGENTS.md forbids credential-bearing strings:
//
//	STARBURST_PG_DSN='postgres://<user>@<pg-host>:5432/<db>?sslmode=disable' \
//	    go test -tags pgtest -run Postgres ./internal/store/
//
// Each test creates and drops its own scratch database, so the shared server is
// left untouched.

var scratchSeq int64

// openScratchStore creates a fresh scratch PostgreSQL database, runs the
// migrations through the public OpenFromConfig entry point and registers a
// cleanup that drops the database again.
func openScratchStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("STARBURST_PG_DSN")
	if dsn == "" {
		t.Skip("STARBURST_PG_DSN not set; PostgreSQL dialect not tested")
	}
	ctx := context.Background()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	admin.SetMaxOpenConns(1)
	dbName := fmt.Sprintf("ocb_test_%d_%d", os.Getpid(), atomic.AddInt64(&scratchSeq, 1))
	_, _ = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create scratch db %q: %v", dbName, err)
	}
	admin.Close()

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + dbName

	st, err := OpenFromConfig(ctx, "postgres", u.String())
	if err != nil {
		t.Fatalf("open postgres store (migrations failed): %v", err)
	}
	t.Cleanup(func() {
		_ = st.Close()
		cleanup, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop scratch db %q: %v", dbName, err)
		}
	})
	return st
}

func TestPostgresMigrationsAndDependentOps(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	// Pending -> queued on upstream success, for every dependent at once.
	if err := st.CreateTask(ctx, &Task{ID: "up_ok", Prompt: "up"}); err != nil {
		t.Fatalf("create upstream: %v", err)
	}
	for _, id := range []string{"dn1", "dn2"} {
		if err := st.CreateTaskWithStatus(ctx, &Task{ID: id, Prompt: id, DependsOn: "up_ok"}, TaskPending); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	if got, err := st.ClaimNextTask(ctx); err == nil && got.DependsOn != "" {
		t.Fatalf("pending task claimed on postgres: %+v", got)
	}
	ids, err := st.PromotePendingDependents(ctx, "up_ok")
	if err != nil || len(ids) != 2 {
		t.Fatalf("promote: ids=%v err=%v", ids, err)
	}

	// Pending -> blocked on upstream failure, with the reason persisted.
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
	got, err := st.GetTask(ctx, "dn3")
	if err != nil || got.Status != TaskBlocked || got.Error != "upstream failed" {
		t.Fatalf("dn3 should be blocked: %+v err=%v", got, err)
	}

	// Blocked -> queued via manual unblock, and again via upstream revival.
	if ok, err := st.UnblockTask(ctx, "dn3"); err != nil || !ok {
		t.Fatalf("unblock: ok=%v err=%v", ok, err)
	}
	got, err = st.GetTask(ctx, "dn3")
	if err != nil || got.Status != TaskQueued || got.Error != "" {
		t.Fatalf("dn3 after unblock: %+v err=%v", got, err)
	}
	if ok, err := st.UnblockTask(ctx, "dn3"); err != nil || ok {
		t.Fatalf("unblock again should fail: ok=%v err=%v", ok, err)
	}
	if ok, err := st.UnblockTask(ctx, "missing"); err != nil || ok {
		t.Fatalf("unblock missing should fail: ok=%v err=%v", ok, err)
	}

	// A fresh blocked dependent is revived when the upstream later succeeds.
	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "dn5", Prompt: "dn5", DependsOn: "up_bad"}, TaskPending); err != nil {
		t.Fatalf("create dependent: %v", err)
	}
	if ids, err = st.BlockDependents(ctx, "up_bad", "upstream failed again"); err != nil || len(ids) != 1 {
		t.Fatalf("re-block: ids=%v err=%v", ids, err)
	}
	ids, err = st.PromotePendingDependents(ctx, "up_bad")
	if err != nil || len(ids) != 1 || ids[0] != "dn5" {
		t.Fatalf("revive blocked: ids=%v err=%v", ids, err)
	}
	got, err = st.GetTask(ctx, "dn5")
	if err != nil || got.Status != TaskQueued || got.Error != "" {
		t.Fatalf("dn5 after revival: %+v err=%v", got, err)
	}

	stats, err := st.TaskStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Blocked != 0 {
		t.Fatalf("blocked=%d, want 0", stats.Blocked)
	}
}

// TestPostgresConcurrentClaim guards the PostgreSQL claim statement. Without
// FOR UPDATE SKIP LOCKED two workers would read the same candidate row before
// either lock lands and both would mark that task running.
func TestPostgresConcurrentClaim(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	const total = 30
	for i := 0; i < total; i++ {
		if err := st.CreateTask(ctx, &Task{ID: fmt.Sprintf("task_pg%d", i), Prompt: "p"}); err != nil {
			t.Fatalf("create task: %v", err)
		}
	}

	const workers = 12
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

// TestPostgresIntervalBinding guards the pgx text-encoding pitfall: an int
// bound where the SQL does `? || ' seconds'` cannot be encoded, so every
// interval expression must receive the seconds as a string. Both the retry
// backoff and the retention cutoff were broken on PostgreSQL until they did.
func TestPostgresIntervalBinding(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "retry-1", Prompt: "r"}, TaskFailed); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.RetryTask(ctx, "retry-1", 120); err != nil {
		t.Fatalf("retry backoff: %v", err)
	}
	got, err := st.GetTask(ctx, "retry-1")
	if err != nil || got.Status != TaskQueued || got.Error != "" {
		t.Fatalf("after retry: %+v err=%v", got, err)
	}
	if !got.AvailableAt.After(time.Now()) {
		t.Fatalf("backoff not applied: available_at=%v", got.AvailableAt)
	}

	if err := st.CreateTaskWithStatus(ctx, &Task{ID: "old-1", Prompt: "o"}, TaskSucceeded); err != nil {
		t.Fatalf("create old: %v", err)
	}
	if _, err := st.CancelTask(ctx, "retry-1"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	backdateTasks(t, st.(*sqlStore), 7200, "old-1", "retry-1")
	deleted, kept, err := st.PurgeFinishedTasks(ctx, time.Hour, 10)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if deleted != 2 || kept != 0 {
		t.Fatalf("purge: deleted=%d kept=%d, want 2/0", deleted, kept)
	}
}

// TestPostgresPurgeFinishedTasks covers the PostgreSQL branch of the retention
// statement: the CURRENT_TIMESTAMP - interval cutoff and the DELETE-by-subselect
// limit, neither of which the SQLite dialect exercises.
func TestPostgresPurgeFinishedTasks(t *testing.T) {
	assertRetentionPurge(t, openScratchStore(t))
}

// TestPostgresClaimNextTaskOfKind guards the PostgreSQL kind-scoped claim
// statement (FOR UPDATE SKIP LOCKED on the kind= filter): a kind=test-run claim
// must only see its own kind and leave prompt tasks alone.
func TestPostgresClaimNextTaskOfKind(t *testing.T) {
	st := openScratchStore(t)
	ctx := context.Background()

	if err := st.CreateTask(ctx, &Task{ID: "pg_kind_tr", Kind: "test-run", Prompt: `{"runId":1}`}); err != nil {
		t.Fatalf("create test-run: %v", err)
	}
	if err := st.CreateTask(ctx, &Task{ID: "pg_kind_prompt", Prompt: "p"}); err != nil {
		t.Fatalf("create prompt: %v", err)
	}
	got, err := st.ClaimNextTaskOfKind(ctx, "test-run")
	if err != nil {
		t.Fatalf("kind claim: %v", err)
	}
	if got.ID != "pg_kind_tr" || got.Kind != "test-run" {
		t.Fatalf("kind claim = %+v, want pg_kind_tr", got)
	}
	if _, err := st.ClaimNextTaskOfKind(ctx, "test-run"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second kind claim err = %v, want ErrNotFound", err)
	}
	p, err := st.ClaimNextTask(ctx)
	if err != nil {
		t.Fatalf("prompt claim: %v", err)
	}
	if p.ID != "pg_kind_prompt" {
		t.Fatalf("prompt claim = %+v, want pg_kind_prompt", p)
	}
}
