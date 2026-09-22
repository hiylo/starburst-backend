package automation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Engine evaluates automation rules and enqueues tasks when a rule fires.
// Cron rules are polled on a ticker; git rules watch for new commits in their
// target repository; http rules fire via webhooks.
type Engine struct {
	store store.Store
	// interval is the poll period for cron rules.
	interval time.Duration
	// now is a clock hook for tests.
	now func() time.Time
	// gitHeads caches the last seen HEAD per git rule (ruleID -> commit).
	gitHeads map[string]string
	// onIntelRun, when set, is invoked instead of creating a prompt task for a
	// rule whose IntelProjectID is non-zero (test-intelligence run-all).
	onIntelRun func(ctx context.Context, projectID int64) error
}

// NewEngine creates an automation engine polling cron rules every interval.
func NewEngine(st store.Store, interval time.Duration) *Engine {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &Engine{store: st, interval: interval, now: time.Now, gitHeads: make(map[string]string)}
}

// SetIntelRunner installs a callback used to trigger a test-intelligence
// run-all regression for rules whose IntelProjectID is non-zero.
func (e *Engine) SetIntelRunner(fn func(ctx context.Context, projectID int64) error) {
	e.onIntelRun = fn
}

// Run polls enabled cron rules and git repositories and fires any that are
// due. It blocks until ctx is canceled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.pollCron(ctx); err != nil {
				log.Printf("automation: cron poll: %v", err)
			}
			if err := e.pollGit(ctx); err != nil {
				log.Printf("automation: git poll: %v", err)
			}
		}
	}
}

// pollCron checks all enabled cron rules and fires due ones.
func (e *Engine) pollCron(ctx context.Context) error {
	rules, err := e.store.ListRules(ctx)
	if err != nil {
		return err
	}
	now := e.now()
	for _, r := range rules {
		if !r.Enabled || r.Kind != store.TriggerCron {
			continue
		}
		if due, err := e.cronDue(r, now); err != nil {
			log.Printf("automation: rule %s cron parse: %v", r.ID, err)
			continue
		} else if due {
			if err := e.Fire(ctx, r.ID); err != nil {
				log.Printf("automation: fire rule %s: %v", r.ID, err)
			}
		}
	}
	return nil
}

// pollGit watches git-kind rules: each rule points to a repository directory
// (Schedule) and fires when a new commit appears (HEAD hash changes).
func (e *Engine) pollGit(ctx context.Context) error {
	rules, err := e.store.ListRules(ctx)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if !r.Enabled || r.Kind != store.TriggerGit {
			continue
		}
		dir := r.Schedule
		if dir == "" {
			dir = r.Directory
		}
		if dir == "" {
			continue
		}
		head, err := gitHead(ctx, dir)
		if err != nil {
			log.Printf("automation: git head %s: %v", r.ID, err)
			continue
		}
		prev, seen := e.gitHeads[r.ID]
		if !seen {
			// First observation: record baseline without firing.
			e.gitHeads[r.ID] = head
			continue
		}
		if prev != head {
			log.Printf("automation: git rule %s fired (head %s -> %s)", r.ID, prev, head)
			if err := e.Fire(ctx, r.ID); err != nil {
				log.Printf("automation: fire git rule %s: %v", r.ID, err)
				// 建任务失败：不推进基线，下一轮 poll 重试而不是吞掉这次提交。
				continue
			}
			e.gitHeads[r.ID] = head
		}
	}
	return nil
}

// gitHead returns the current HEAD commit of a git repository.
func gitHead(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Fire enqueues a task for the given rule (cron/git/http all use this path).
// Returns ErrNotFound if the rule does not exist.
func (e *Engine) Fire(ctx context.Context, ruleID string) error {
	rule, err := e.store.GetRule(ctx, ruleID)
	if err != nil {
		return err
	}
	if !rule.Enabled {
		return nil
	}
	// intel-run 规则：触发测试智能 run-all 回归，而非 prompt 任务。
	if rule.IntelProjectID != 0 {
		if e.onIntelRun == nil {
			return fmt.Errorf("rule %s: intel-run 回调未配置", ruleID)
		}
		if err := e.onIntelRun(ctx, rule.IntelProjectID); err != nil {
			return err
		}
		if err := e.store.RecordRuleExecution(ctx, ruleID, fmt.Sprintf("intel-run-%d", rule.IntelProjectID)); err != nil {
			log.Printf("automation: record execution for %s: %v", ruleID, err)
		}
		return e.store.MarkRuleFired(ctx, ruleID)
	}
	t := &store.Task{
		ID:        newRuleTaskID(ruleID),
		SessionID: rule.SessionID,
		Directory: rule.Directory,
		Prompt:    rule.Prompt,
	}
	if err := e.store.CreateTask(ctx, t); err != nil {
		return err
	}
	if err := e.store.RecordRuleExecution(ctx, ruleID, t.ID); err != nil {
		// Non-fatal: the task is created even if history logging fails.
		log.Printf("automation: record execution for %s: %v", ruleID, err)
	}
	return e.store.MarkRuleFired(ctx, ruleID)
}

// FireKind enqueues a task for the first enabled rule of the given kind whose
// schedule matches target. Returns (fired bool). Used by webhook handlers that
// do not know the rule id but carry a target (repo path / http path).
func (e *Engine) FireKind(ctx context.Context, kind, target string) (bool, error) {
	rules, err := e.store.ListRules(ctx)
	if err != nil {
		return false, err
	}
	for _, r := range rules {
		if !r.Enabled || r.Kind != kind {
			continue
		}
		if target != "" && r.Schedule != "" && !matchTarget(r.Schedule, target) {
			continue
		}
		if err := e.Fire(ctx, r.ID); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// cronDue reports whether a cron rule's schedule has elapsed since last fire.
// The schedule uses simple "N s/m/h" or cron "* * * * * *" (second-level).
// For MVP only interval-like schedules are evaluated; full cron is parsed
// loosely to avoid pulling an external dependency offline.
func (e *Engine) cronDue(r *store.Rule, now time.Time) (bool, error) {
	s := strings.TrimSpace(r.Schedule)
	if s == "" {
		return false, nil
	}
	last := r.LastFiredAt
	if last == nil {
		return true, nil // never fired -> fire immediately
	}
	// Interval syntax: "30s", "5m", "1h".
	if d, err := time.ParseDuration(s); err == nil {
		return now.Sub(*last) >= d, nil
	}
	// Cron syntax (6 fields, second-level). Compute next fire from last fire.
	next, err := nextCron(*last, strings.Fields(s))
	if err != nil {
		return false, err
	}
	return !now.Before(next), nil
}

func matchTarget(schedule, target string) bool {
	return schedule == target
}

// newRuleTaskID builds a unique task id for a rule firing. A random suffix is
// required because two firings of the same rule within one second would
// otherwise collide on the primary key.
func newRuleTaskID(ruleID string) string {
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return "task_" + ruleID + "_" + time.Now().Format("20060102150405") + "_" + hex.EncodeToString(buf)
}
