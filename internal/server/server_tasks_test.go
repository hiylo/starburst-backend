package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

func TestTasksCRUD(t *testing.T) {
	s := newTestServer(t)

	// Login and create a token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"tasker"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Create task.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"do stuff","directory":"/w"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create task status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     string `json:"ID"`
		Status string `json:"Status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" {
		t.Fatalf("no task id")
	}
	if created.Status != "" && created.Status != "queued" {
		t.Fatalf("unexpected initial status %q", created.Status)
	}

	// List includes it.
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(list.Tasks))
	}

	// Get single.
	rec = s.do(t, http.MethodGet, "/api/tasks/"+created.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status %d", rec.Code)
	}
}

func TestTaskRequiresToken(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

func TestTaskDependsOn(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"dep"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Upstream task starts queued.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"upstream"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create upstream: %d %s", rec.Code, rec.Body.String())
	}
	var up struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &up)

	// A dependent on a queued task must be pending, not queued.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"downstream","dependsOn":"`+up.ID+`"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create dependent: %d %s", rec.Code, rec.Body.String())
	}
	var dep struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		DependsOn string `json:"dependsOn"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &dep)
	if dep.Status != "pending" {
		t.Fatalf("dependent status = %q, want pending", dep.Status)
	}
	if dep.DependsOn != up.ID {
		t.Fatalf("dependsOn = %q, want %q", dep.DependsOn, up.ID)
	}

	// A typo in dependsOn fails fast instead of parking a pending task forever.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","dependsOn":"task_missing"}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown dependsOn: got %d want 400", rec.Code)
	}

	// Canceling the upstream blocks its pending dependents.
	rec = s.do(t, http.MethodDelete, "/api/tasks/"+up.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel upstream: %d %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		OK      bool `json:"ok"`
		Blocked int  `json:"blocked"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &blocked)
	if !blocked.OK || blocked.Blocked != 1 {
		t.Fatalf("cancel should block 1 dependent, got %+v", blocked)
	}
	rec = s.do(t, http.MethodGet, "/api/tasks/"+dep.ID, "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("get dependent: %d", rec.Code)
	}
	var depAfter struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &depAfter)
	if depAfter.Status != "blocked" || depAfter.Error == "" {
		t.Fatalf("dependent = %+v, want blocked with a reason", depAfter)
	}

	// A dependency that already finished is rejected.
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"x","dependsOn":"`+up.ID+`"}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("finished dependsOn: got %d want 400", rec.Code)
	}

	// A dependency that already succeeded runs immediately.
	if err := s.store.CreateTask(ctx, &store.Task{ID: "task_done", Prompt: "done"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.store.CompleteTask(ctx, "task_done", "ok"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"after done","dependsOn":"task_done"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create after done: %d %s", rec.Code, rec.Body.String())
	}
	var after struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &after)
	if after.Status != "queued" {
		t.Fatalf("status after succeeded upstream = %q, want queued", after.Status)
	}
}

func TestBatchCreatesMultipleTasks(t *testing.T) {
	s := newTestServer(t)

	// Login + token.
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"batch"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	// Batch create for two targets.
	rec = s.do(t, http.MethodPost, "/api/batch",
		`{"prompt":"add logging","targets":[{"directory":"/a"},{"sessionId":"ses_1"}]}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Count != 2 {
		t.Fatalf("expected 2 tasks, got %d", out.Count)
	}

	// Verify two tasks in store.
	rec = s.do(t, http.MethodGet, "/api/tasks", "", th)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(list.Tasks))
	}

	// Empty targets rejected.
	rec = s.do(t, http.MethodPost, "/api/batch", `{"prompt":"x","targets":[]}`, th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty targets, got %d", rec.Code)
	}

	// No token rejected.
	rec = s.do(t, http.MethodPost, "/api/batch", `{"prompt":"x","targets":[{"directory":"/a"}]}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec.Code)
	}
}

// TestTaskListPagination covers the paged task listing: limit/offset params,
// the total count, clamping of out-of-range values and the status filter.
func TestTaskListPagination(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"page"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	for i := 0; i < 5; i++ {
		rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"page task"}`, th)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create task %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}

	type page struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
		Total  int `json:"total"`
		Limit  int `json:"limit"`
		Offset int `json:"offset"`
	}
	get := func(query string) page {
		t.Helper()
		rec = s.do(t, http.MethodGet, "/api/tasks?"+query, "", th)
		if rec.Code != http.StatusOK {
			t.Fatalf("list ?%s: %d %s", query, rec.Code, rec.Body.String())
		}
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		return p
	}

	p1 := get("limit=2&offset=0")
	if len(p1.Tasks) != 2 || p1.Total != 5 || p1.Limit != 2 || p1.Offset != 0 {
		t.Fatalf("page 1: %+v", p1)
	}
	p2 := get("limit=2&offset=2")
	if len(p2.Tasks) != 2 || p2.Offset != 2 || p2.Total != 5 {
		t.Fatalf("page 2: %+v", p2)
	}
	for _, a := range p1.Tasks {
		for _, b := range p2.Tasks {
			if a.ID == b.ID {
				t.Fatalf("pages overlap on %s", a.ID)
			}
		}
	}
	p3 := get("limit=2&offset=4")
	if len(p3.Tasks) != 1 || p3.Total != 5 {
		t.Fatalf("page 3: %+v", p3)
	}

	// Defaults: limit 50, offset 0, whole list in one page here.
	d := get("")
	if len(d.Tasks) != 5 || d.Total != 5 || d.Limit != 50 || d.Offset != 0 {
		t.Fatalf("defaults: %+v", d)
	}
	if p := get("limit=9999"); p.Limit != 50 {
		t.Fatalf("limit above 500 must fall back to 50, got %+v", p)
	}
	if p := get("limit=0"); p.Limit != 50 {
		t.Fatalf("non-positive limit must fall back to 50, got %+v", p)
	}
	if p := get("limit=1&offset=-5"); p.Offset != 0 || len(p.Tasks) != 1 {
		t.Fatalf("negative offset must fall back to 0, got %+v", p)
	}
	if p := get("status=queued&limit=1"); p.Total != 5 || len(p.Tasks) != 1 {
		t.Fatalf("filtered page: %+v", p)
	}
	if p := get("status=blocked&limit=1"); p.Total != 0 || len(p.Tasks) != 0 {
		t.Fatalf("empty filtered page: %+v", p)
	}
}

func TestTaskCreateReturnsStoredTimestamps(t *testing.T) {
	s := newTestServer(t)

	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"ts"}`, map[string]string{"X-Web-Session": login.Session})
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	th := map[string]string{"Authorization": "Bearer " + tok.Token}

	before := time.Now()
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"timestamps"}`, th)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body.String())
	}
	var out store.Task
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != store.TaskQueued {
		t.Fatalf("status %q, want queued", out.Status)
	}
	for _, f := range []struct {
		name string
		ts   time.Time
	}{
		{"createdAt", out.CreatedAt},
		{"updatedAt", out.UpdatedAt},
		{"availableAt", out.AvailableAt},
	} {
		if f.ts.IsZero() {
			t.Fatalf("%s is zero in the create response", f.name)
		}
		if f.ts.Before(before.Add(-time.Minute)) || f.ts.After(time.Now().Add(time.Minute)) {
			t.Fatalf("%s %v is out of range", f.name, f.ts)
		}
	}
}
