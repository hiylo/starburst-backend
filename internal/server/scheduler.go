package server

import (
	"context"
	"log"
	"time"

	"github.com/hiylo/starburst-backend/internal/automation"
	"github.com/hiylo/starburst-backend/internal/store"
)

// schedulerInterval is how often scheduled tasks are evaluated. One-shot tasks
// are promoted as soon as their time arrives; recurring cron templates clone a
// concrete task when their next fire time is reached.
const schedulerInterval = 15 * time.Second

// RunScheduler polls scheduled tasks until ctx is canceled. It promotes due
// one-shot tasks to queued and clones due recurring cron templates.
func (s *Server) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.pollScheduled(ctx); err != nil {
				log.Printf("scheduler: poll: %v", err)
			}
		}
	}
}

func (s *Server) pollScheduled(ctx context.Context) error {
	tasks, err := s.store.ListScheduledTasks(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, t := range tasks {
		if t.Cron != "" {
			s.fireRecurring(ctx, t, now)
		} else if t.ScheduledAt != nil && !t.ScheduledAt.After(now) {
			if ok, err := s.store.PromoteScheduledTask(ctx, t.ID); err != nil {
				log.Printf("scheduler: promote %s: %v", t.ID, err)
			} else if ok {
				s.pushTaskEvent(store.TaskQueued, map[string]any{"id": t.ID, "status": store.TaskQueued})
			}
		}
	}
	return nil
}

// fireRecurring clones a concrete task when the cron template's next fire time
// has arrived, and records the fire so the next occurrence is computed from it.
//
// Order of operations matters: the clone is created first and the cursor is
// claimed second. Creating first means the crash window between the two calls
// leaves an orphan clone (which will run) but NOT an advanced cursor, so the
// next tick re-clones and the occurrence is never permanently lost. Claiming
// loses the old race protection: the claim is still an atomic CAS against the
// previous last_fired_at, so of two schedulers only one keeps its clone while
// the loser cancels its own to avoid double execution. Combined, the premise
// is "an extra duplicate run beats a permanently lost trigger" for automation.
func (s *Server) fireRecurring(ctx context.Context, t *store.Task, now time.Time) {
	// The first occurrence is computed from the task's creation time. Using
	// `now` here would never fire: NextCron is strictly after its base, so
	// next.After(now) is always true and LastFiredAt is only written after a
	// successful clone, leaving the template stuck in "scheduled" forever.
	base := t.CreatedAt
	if t.LastFiredAt != nil {
		base = *t.LastFiredAt
	}
	next, err := automation.NextCron(base, t.Cron)
	if err != nil {
		log.Printf("scheduler: cron %s parse: %v", t.ID, err)
		return
	}
	if next.After(now) {
		return
	}
	clone := &store.Task{
		ID:        newTaskID(),
		SessionID: t.SessionID,
		Directory: t.Directory,
		Name:      t.Name,
		Prompt:    t.Prompt,
	}
	// 先建实体任务再抢占游标：抢占只是 cursor 的 CAS，成功后保留 clone，
	// 失败则取消刚建的 clone 回滚（建立时游标未推进，下一 tick 会重试）。
	if err := s.store.CreateTask(ctx, clone); err != nil {
		log.Printf("scheduler: clone %s: %v", t.ID, err)
		// clone 未建成、游标也未推进，无需回滚，下一 tick 天然重试。
		return
	}
	claimed, err := s.store.ClaimRecurringFire(ctx, t.ID, t.LastFiredAt, now)
	if err != nil {
		// CLAIM 出错：游标大概率未推进，取消刚建的 clone 避免游离任务堆积。
		log.Printf("scheduler: claim %s: %v", t.ID, err)
		if _, cErr := s.store.CancelTask(ctx, clone.ID); cErr != nil {
			log.Printf("scheduler: rollback clone %s: %v", clone.ID, cErr)
		}
		return
	}
	if !claimed {
		// 已被其它调度者抢占：取消自己的 clone，避免同一触发被建两次。
		if _, cErr := s.store.CancelTask(ctx, clone.ID); cErr != nil {
			log.Printf("scheduler: rollback duplicate clone %s: %v", clone.ID, cErr)
		}
		return
	}
	s.pushTaskEvent(store.TaskQueued, map[string]any{"id": clone.ID, "status": store.TaskQueued})
}
