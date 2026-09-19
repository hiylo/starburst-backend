package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelContractCheck verifies the response-contract validation endpoint:
// analyze a repo with a DTO-returning controller, then validate a pasted
// response that matches and one that violates the extracted field contract.
func TestIntelContractCheck(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/ActivityController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/activity")
public class ActivityController {
    @GetMapping("/get")
    public demo.ActivityDto get() { return null; }
}`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/ActivityDto.java"), `package demo;
public class ActivityDto {
    private Long id;
    private String title;
    private Boolean active;
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodGet, "/api/intel/endpoints?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoints status %d", rec.Code)
	}
	var epResp struct {
		Endpoints []struct {
			ID         int64  `json:"id"`
			FieldsJSON string `json:"fieldsJson"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &epResp); err != nil {
		t.Fatalf("endpoints parse: %v", err)
	}
	if len(epResp.Endpoints) != 1 {
		t.Fatalf("endpoints = %d, want 1", len(epResp.Endpoints))
	}
	ep := epResp.Endpoints[0]
	if !stringsContains(ep.FieldsJSON, "title") {
		t.Fatalf("endpoint fieldsJson missing title: %s", ep.FieldsJSON)
	}

	// Matching response: all present, correct types.
	rec = s.do(t, http.MethodPost, "/api/intel/contracts/check",
		`{"endpointId":`+jsonInt(ep.ID)+`,"responseJson":"{\"id\":1,\"title\":\"hi\",\"active\":true}"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("contract check status %d: %s", rec.Code, rec.Body.String())
	}
	var okResp struct {
		Passed int `json:"passed"`
		Failed int `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &okResp); err != nil {
		t.Fatalf("contract check parse: %v", err)
	}
	if okResp.Failed != 0 {
		t.Errorf("matching response has %d failures: %s", okResp.Failed, rec.Body.String())
	}

	// Violating response: title is a number instead of string.
	rec = s.do(t, http.MethodPost, "/api/intel/contracts/check",
		`{"endpointId":`+jsonInt(ep.ID)+`,"responseJson":"{\"id\":1,\"title\":7,\"active\":true}"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("contract check status %d: %s", rec.Code, rec.Body.String())
	}
	var badResp struct {
		Failed  int `json:"failed"`
		Results []struct {
			Field  string `json:"field"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &badResp); err != nil {
		t.Fatalf("contract check parse: %v", err)
	}
	if badResp.Failed == 0 {
		t.Fatalf("violating response reported 0 failures: %s", rec.Body.String())
	}
	for _, r := range badResp.Results {
		if r.Field == "title" && r.Status != "type_mismatch" {
			t.Errorf("title status = %q, want type_mismatch", r.Status)
		}
	}
}

// TestIntelOverridesPending verifies the 待确认队列 workflow: a batch apply
// persists confirmed overrides; a manually created pending row shows in the
// queue and is finalized via confirm.
func TestIntelOverridesPending(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/intel/overrides",
		`{"projectId":1,"overrides":[{"target":"module","rowKey":"app","field":"role","manualValue":"app","confidence":"high"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch apply status %d: %s", rec.Code, rec.Body.String())
	}
	var applyResp struct {
		Applied int `json:"applied"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &applyResp); err != nil {
		t.Fatalf("apply parse: %v", err)
	}
	if applyResp.Applied != 1 {
		t.Errorf("applied = %d, want 1", applyResp.Applied)
	}

	// Directly seed a pending override for the confirm path.
	pending := &store.IntelOverride{
		ProjectID:     1,
		ModuleID:      0,
		Target:        "feature",
		RowKey:        "banner",
		Field:         "name",
		ManualValue:   "",
		Confidence:    "medium",
		Status:        "pending",
		Source:        "llm-suggest",
		AutoValueJSON: `{"name":"Banner"}`,
	}
	ctx := context.Background()
	if err := s.store.CreateIntelOverride(ctx, pending); err != nil {
		t.Fatalf("create pending: %v", err)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/pending?projectId=1", "", wh)
	var pendingResp struct {
		Pending []struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pendingResp); err != nil {
		t.Fatalf("pending parse: %v", err)
	}
	if len(pendingResp.Pending) != 1 || pendingResp.Pending[0].ID != pending.ID {
		t.Fatalf("pending = %+v, want 1 row", pendingResp.Pending)
	}

	rec = s.do(t, http.MethodPost, "/api/intel/pending/"+jsonInt(pending.ID)+"/confirm",
		`{"manualValue":"App 首页 Banner"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/pending?projectId=1", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &pendingResp); err != nil {
		t.Fatalf("pending parse: %v", err)
	}
	if len(pendingResp.Pending) != 0 {
		t.Errorf("pending after confirm = %d, want 0", len(pendingResp.Pending))
	}
}

// TestIntelContractCheckBatch verifies the one-click project contract check:
// every endpoint is called against a live base URL and its response validated
// against the extracted field contract.
func TestIntelContractCheckBatch(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"nickname":"alice"}]`))
	}))
	defer backend.Close()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController { @GetMapping("/list") public java.util.List<demo.UserEntity> list() { return null; } }`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
public class UserEntity { private Long id; private String nickname; }`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/GraphQLController.java"), `package demo;
import org.springframework.graphql.data.method.annotation.*;
@Controller
public class GraphQLController {
    @QueryMapping
    public String hello() { return "hi"; }
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/contracts/check-batch",
		`{"projectId":`+jsonInt(proj.ID)+`,"baseUrl":"`+backend.URL+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("check-batch status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Total          int `json:"total"`
		ContractReady  int `json:"contractReady"`
		Reachable      int `json:"reachable"`
		Passed         int `json:"passed"`
		Failed         int `json:"failed"`
		GraphQLSkipped int `json:"graphqlSkipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("check-batch parse: %v", err)
	}
	if resp.Total != 2 || resp.GraphQLSkipped != 1 {
		t.Errorf("check-batch total/skipped = %d/%d, want 2/1", resp.Total, resp.GraphQLSkipped)
	}
	if resp.ContractReady != 1 || resp.Reachable != 1 {
		t.Errorf("check-batch = %+v, want contract/reachable all 1", resp)
	}
	if resp.Passed != 1 || resp.Failed != 0 {
		t.Errorf("check-batch passed/failed = %d/%d, want 1/0", resp.Passed, resp.Failed)
	}
}

// TestIntelFeatureNameOverride verifies the overrides layer renames a feature
// from the 待确认队列 (applied override keyed by feature id).
func TestIntelFeatureNameOverride(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController { @GetMapping("/list") public String list() { return "x"; } }`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/features?projectId="+jsonInt(proj.ID), "", wh)
	var fResp struct {
		Features []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil || len(fResp.Features) != 1 {
		t.Fatalf("features: %s", rec.Body.String())
	}
	feat := fResp.Features[0]

	if err := s.store.CreateIntelOverride(context.Background(), &store.IntelOverride{
		ProjectID:   proj.ID,
		Target:      "feature",
		RowKey:      jsonInt(feat.ID),
		Field:       "name",
		ManualValue: "用户管理（人工改名）",
		Confidence:  "high",
		Status:      "applied",
		Source:      "manual",
	}); err != nil {
		t.Fatalf("create override: %v", err)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/features?projectId="+jsonInt(proj.ID), "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil {
		t.Fatalf("features parse: %v", err)
	}
	if len(fResp.Features) != 1 || fResp.Features[0].Name != "用户管理（人工改名）" {
		t.Errorf("feature name after override = %+v", fResp.Features)
	}
}

// TestIntelOverrideEnqueue verifies that override drafts flow into the 待确认
// queue as pending rows (human reviews before they take effect).
func TestIntelOverrideEnqueue(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/intel/overrides/enqueue",
		`{"projectId":1,"drafts":[{"target":"module","rowKey":"app","field":"role","autoValue":"provider","manualValue":"app","confidence":"high"},{"target":"feature","rowKey":"3","field":"name","manualValue":"改名"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("enqueue status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Queued int `json:"queued"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("enqueue parse: %v", err)
	}
	if resp.Queued != 2 {
		t.Errorf("queued = %d, want 2", resp.Queued)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/pending?projectId=1", "", wh)
	var pending struct {
		Pending []struct {
			Status string `json:"status"`
			Source string `json:"source"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pending); err != nil {
		t.Fatalf("pending parse: %v", err)
	}
	if len(pending.Pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(pending.Pending))
	}
	for _, p := range pending.Pending {
		if p.Status != "pending" || p.Source != "llm-suggest" {
			t.Errorf("pending row = %+v", p)
		}
	}
}
