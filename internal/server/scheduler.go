package server

import (
	"context"
	"log"
	"time"

	"github.com/hiylo/startburst-backend/internal/automation"
	"github.com/hiylo/startburst-backend/internal/store"
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
	if err := s.store.CreateTask(ctx, clone); err != nil {
		log.Printf("scheduler: clone %s: %v", t.ID, err)
		return
	}
	if err := s.store.SetTaskLastFiredAt(ctx, t.ID, now); err != nil {
		log.Printf("scheduler: record fire %s: %v", t.ID, err)
	}
	s.pushTaskEvent(store.TaskQueued, map[string]any{"id": clone.ID, "status": store.TaskQueued})
}
