package server

import (
	"context"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// newNeverFiredTemplate 建一个从未触发的周期模板（last_fired_at 为 NULL，走
// ClaimRecurringFire 的 IS NULL 分支，不受 SQLite 时间文本编码 CAS 失配影响），
// 并等待超过 CreatedAt+1s 使其到期（next = CreatedAt+1s）。
func newNeverFiredTemplate(t *testing.T, s *Server) *store.Task {
	t.Helper()
	ctx := context.Background()
	tpl := &store.Task{ID: newTaskID(), Name: "daily", Prompt: "p", Cron: "* * * * * *"}
	if err := s.store.CreateTaskWithStatus(ctx, tpl, store.TaskScheduled); err != nil {
		t.Fatalf("create template: %v", err)
	}
	got, err := s.store.GetTask(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("reload template: %v", err)
	}
	if got.LastFiredAt != nil {
		t.Fatalf("template should start with nil last_fired_at")
	}
	// CreatedAt 在 SQLite 里是秒级截断的 UTC 文本，next = CreatedAt+1s；多等一点
	// 确保无论插入落在哪一秒，调用时都已到期。
	time.Sleep(1200 * time.Millisecond)
	return got
}

// TestSchedulerFireRecurringClonesAndAdvancesCursor 验证先建 clone 再抢占游标的
// 主路径：到期触发克隆出一个 queued 任务，并把 last_fired_at 从 NULL 推进到 now。
func TestSchedulerFireRecurringClonesAndAdvancesCursor(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	tpl := newNeverFiredTemplate(t, s)
	now := time.Now()

	s.fireRecurring(ctx, tpl, now)

	queued, err := s.store.ListTasks(ctx, store.TaskQueued, 10, 0)
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("expected 1 clone, got %d", len(queued))
	}
	if queued[0].Prompt != "p" || queued[0].Name != "daily" {
		t.Fatalf("clone fields wrong: %+v", queued[0])
	}
	got, err := s.store.GetTask(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("get template: %v", err)
	}
	if got.LastFiredAt == nil {
		t.Fatalf("cursor not advanced, last_fired_at still nil")
	}
	if !got.LastFiredAt.After(tpl.CreatedAt) {
		t.Fatalf("cursor not advanced past created_at: %v vs %v", got.LastFiredAt, tpl.CreatedAt)
	}
}

// TestSchedulerFireRecurringClaimLostCancelsClone 验证并发调度场景：后到的调度者
// 用陈旧视图（last_fired_at 仍为 NULL）触发时 claim CAS 失败，会取消自己刚建的
// clone，避免同一触发被执行两次。
func TestSchedulerFireRecurringClaimLostCancelsClone(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	tpl := newNeverFiredTemplate(t, s)

	// 调度者 A 抢占成功，保留 clone 并推进游标。
	s.fireRecurring(ctx, tpl, time.Now())
	queued, _ := s.store.ListTasks(ctx, store.TaskQueued, 10, 0)
	if len(queued) != 1 {
		t.Fatalf("after A: expected 1 queued clone, got %d", len(queued))
	}

	// 调度者 B 持有陈旧视图（LastFiredAt 仍为 nil）：claim CAS 失败，取消 B 的 clone。
	stale := &store.Task{ID: tpl.ID, Name: tpl.Name, Prompt: tpl.Prompt, Cron: tpl.Cron}
	s.fireRecurring(ctx, stale, time.Now())

	queued, _ = s.store.ListTasks(ctx, store.TaskQueued, 10, 0)
	if len(queued) != 1 {
		t.Fatalf("after B: expected still 1 queued clone, got %d", len(queued))
	}
	canceled, _ := s.store.ListTasks(ctx, store.TaskCanceled, 10, 0)
	if len(canceled) != 1 {
		t.Fatalf("expected 1 canceled rollback clone, got %d", len(canceled))
	}
}
