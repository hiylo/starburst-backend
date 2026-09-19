package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

func TestRulesCRUDAndWebhook(t *testing.T) {
	s := newTestServer(t)

	// Web session (rules are admin-managed).
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}

	// Create a cron rule.
	rec = s.do(t, http.MethodPost, "/api/rules", `{"name":"nightly","kind":"cron","schedule":"5m","directory":"/w","prompt":"run tests","enabled":true}`, wh)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create rule status %d: %s", rec.Code, rec.Body.String())
	}
	var rule struct {
		ID string `json:"ID"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)
	if rule.ID == "" {
		t.Fatalf("no rule id")
	}

	// List includes it.
	rec = s.do(t, http.MethodGet, "/api/rules", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("list rules status %d", rec.Code)
	}
	var list struct {
		Rules []map[string]any `json:"rules"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(list.Rules))
	}

	// Rules require web session (not token).
	rec = s.do(t, http.MethodGet, "/api/rules", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session, got %d", rec.Code)
	}

	// Delete rule.
	rec = s.do(t, http.MethodDelete, "/api/rules/"+rule.ID, "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete rule status %d", rec.Code)
	}
	rec = s.do(t, http.MethodDelete, "/api/rules/"+rule.ID, "", wh)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on double delete, got %d", rec.Code)
	}
}

func TestWebhookFiresRule(t *testing.T) {
	s := newTestServer(t)
	s.cfg.WebhookSecret = "topsecret"

	// Login and create an HTTP rule via web session.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}

	rec = s.do(t, http.MethodPost, "/api/rules", `{"name":"hook","kind":"http","schedule":"/workspaces/opencode","prompt":"run on hook","enabled":true}`, wh)
	var rule struct {
		ID string `json:"ID"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rule)

	// Fire webhook for matching target.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/workspaces/opencode", "", map[string]string{"X-Webhook-Secret": "topsecret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook status %d: %s", rec.Code, rec.Body.String())
	}
	var fired struct {
		Fired bool `json:"fired"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fired)
	if !fired.Fired {
		t.Fatalf("webhook did not fire")
	}

	// A task must have been created (visible with an APP token).
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"hooker"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 task from webhook, got %d", len(list.Tasks))
	}

	// Non-matching target -> 404.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/nope", "", map[string]string{"X-Webhook-Secret": "topsecret"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-match, got %d", rec.Code)
	}
}

func TestWebhookSecretProtection(t *testing.T) {
	s := newTestServer(t)
	s.cfg.WebhookSecret = "topsecret"

	// Login + create http rule.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	s.do(t, http.MethodPost, "/api/rules", `{"name":"hook","kind":"http","schedule":"/x","prompt":"p","enabled":true}`, wh)

	// Without secret -> 401.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret, got %d", rec.Code)
	}

	// Wrong secret -> 401.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", map[string]string{"X-Webhook-Secret": "wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong secret, got %d", rec.Code)
	}

	// Correct secret -> fired.
	rec = s.do(t, http.MethodPost, "/api/webhook?target=/x", "", map[string]string{"X-Webhook-Secret": "topsecret"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct secret, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestFireRecurringFirstOccurrence(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	tmpl := &store.Task{ID: "tmpl_1", Name: "每秒", Prompt: "recurring", Cron: "* * * * * *"}
	if err := s.store.CreateTaskWithStatus(ctx, tmpl, store.TaskScheduled); err != nil {
		t.Fatalf("create template: %v", err)
	}
	got, err := s.store.GetTask(ctx, "tmpl_1")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	if got.CreatedAt.IsZero() {
		t.Fatalf("template createdAt is zero, the fire base is unusable")
	}

	// First poll happens 30s after creation, so the first occurrence
	// (created+1s) is already due and must be cloned.
	due := got.CreatedAt.Add(30 * time.Second)
	s.fireRecurring(ctx, got, due)

	listed, err := s.store.ListTasks(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	clones := 0
	for _, task := range listed {
		if task.ID == "tmpl_1" || task.Prompt != "recurring" {
			continue
		}
		clones++
		if task.Status != store.TaskQueued {
			t.Fatalf("clone %s status %q, want queued", task.ID, task.Status)
		}
	}
	if clones != 1 {
		t.Fatalf("clones %d, want 1: a template must fire its first occurrence", clones)
	}

	got, err = s.store.GetTask(ctx, "tmpl_1")
	if err != nil {
		t.Fatalf("read template after fire: %v", err)
	}
	if got.LastFiredAt == nil {
		t.Fatalf("lastFiredAt was not recorded")
	}
	if d := got.LastFiredAt.Sub(due); d > 2*time.Second || d < -2*time.Second {
		t.Fatalf("lastFiredAt %v, want %v", got.LastFiredAt, due)
	}

	// A poll right after creation: the first occurrence is still ahead, so
	// nothing may be cloned yet.
	fresh := &store.Task{ID: "tmpl_2", Prompt: "later", Cron: "* * * * * *"}
	if err := s.store.CreateTaskWithStatus(ctx, fresh, store.TaskScheduled); err != nil {
		t.Fatalf("create fresh template: %v", err)
	}
	freshGot, err := s.store.GetTask(ctx, "tmpl_2")
	if err != nil {
		t.Fatalf("read fresh template: %v", err)
	}
	s.fireRecurring(ctx, freshGot, freshGot.CreatedAt.Add(500*time.Millisecond))

	listed, err = s.store.ListTasks(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("list again: %v", err)
	}
	for _, task := range listed {
		if task.Prompt == "later" && task.ID != "tmpl_2" {
			t.Fatalf("template fired before its first occurrence")
		}
	}
}
