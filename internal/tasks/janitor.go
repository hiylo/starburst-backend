package tasks

import (
	"context"
	"log"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// purgeInterval is how often the janitor sweeps finished tasks out of the DB.
const purgeInterval = time.Hour

// purgeBatch caps the rows a single pass removes, so a backlogged install
// drains over a few passes instead of holding one long transaction.
const purgeBatch = 500

// purgeTimeout bounds one pass so a slow database cannot stall the janitor.
const purgeTimeout = 30 * time.Second

// purgeLoop deletes finished tasks once at startup and then every
// purgeInterval, keeping the tasks table bounded on long-lived installs.
func (e *Executor) purgeLoop(ctx context.Context) {
	e.purgeOnce(ctx)
	e.stuckSweepOnce(ctx)
	purgeTicker := time.NewTicker(purgeInterval)
	stuckTicker := time.NewTicker(stuckInterval)
	defer purgeTicker.Stop()
	defer stuckTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-purgeTicker.C:
			e.purgeOnce(ctx)
		case <-stuckTicker.C:
			e.stuckSweepOnce(ctx)
		}
	}
}

// stuckInterval is how often the janitor sweeps for stuck running tasks.
const stuckInterval = 15 * time.Minute

// taskStuckAfter is how long a running task may go without any progress update
// (the executor heartbeats updated_at while polling) before it is considered
// stuck and failed.
const taskStuckAfter = 10 * time.Minute

// stuckSweepOnce fails running tasks whose heartbeat went silent for
// taskStuckAfter, aborts their upstream session and blocks their dependents.
// A live run keeps updated_at fresh (the executor polls every few seconds), so
// a stale timestamp means the run is genuinely wedged.
func (e *Executor) stuckSweepOnce(ctx context.Context) {
	limited, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()
	stuck, err := e.store.ListStuckRunning(limited, time.Now().Add(-taskStuckAfter))
	if err != nil {
		log.Printf("tasks: stuck sweep: %v", err)
		return
	}
	for _, t := range stuck {
		if e.retention > 0 && time.Since(t.UpdatedAt) > e.retention {
			continue
		}
		log.Printf("tasks: %s stuck (no progress since %s), failing", t.ID, t.UpdatedAt.Format(time.RFC3339))
		if t.SessionID != "" {
			e.abortSession(t.SessionID, t.Directory)
		}
		_ = e.store.FailTask(limited, t.ID, "任务卡死：超过 "+taskStuckAfter.String()+" 无进度")
		e.resolveDependents(limited, t.ID, false, "upstream task stuck")
		e.pushTaskEvent(store.TaskFailed, map[string]any{"id": t.ID, "status": store.TaskFailed, "reason": "stuck"})
	}
}

// purgeOnce runs a single retention pass and logs it when anything happened.
// It returns the number of deleted tasks and the number of expired tasks that
// were kept because a dependent still references them.
func (e *Executor) purgeOnce(ctx context.Context) (int, int, error) {
	limited, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()

	deleted, kept, err := e.store.PurgeFinishedTasks(limited, e.retention, purgeBatch)
	if err != nil {
		log.Printf("tasks: purge finished tasks: %v", err)
		return 0, 0, err
	}
	if deleted > 0 || kept > 0 {
		log.Printf("tasks: purged %d finished task(s) older than %s, kept %d still referenced by dependents",
			deleted, e.retention, kept)
	}
	return deleted, kept, nil
}
