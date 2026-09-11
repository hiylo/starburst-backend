package tasks

import (
	"context"
	"log"
	"time"
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
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.purgeOnce(ctx)
		}
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
