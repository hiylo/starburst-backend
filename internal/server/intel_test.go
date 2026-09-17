package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelFlow verifies M1 acceptance end-to-end through the HTTP API:
// register a local Java repository → analyze → the project resolves to one
// java module and exposes entity↔table↔column mappings and endpoint contracts
// with source_file:line provenance.
func TestIntelFlow(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// Build a minimal Java repo under a temp dir: a controller + an entity.
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<demo.UserEntity> list() { return null; }
    @PostMapping
    public UserEntity create(@RequestBody UserEntity user) { return user; }
}`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "nickname")
    private String nickname;
}`)

	// Register the project.
	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create project status %d: %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project response: %s", rec.Body.String())
	}

	// Analyze.
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+itoa2(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Detail: must list one java module.
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+itoa2(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Project struct {
			AnalyzedAt *string `json:"analyzedAt"`
		} `json:"project"`
		Modules []struct {
			RelPath  string `json:"relPath"`
			KindType string `json:"kindType"`
			KindRole string `json:"kindRole"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if detail.Project.AnalyzedAt == nil || *detail.Project.AnalyzedAt == "" {
		t.Errorf("project not marked analyzed")
	}
	foundJava := false
	for _, m := range detail.Modules {
		if m.RelPath == "." && m.KindType == "java" {
			foundJava = true
		}
	}
	if !foundJava {
		t.Errorf("no java module detected, got %+v", detail.Modules)
	}

	// Endpoints: the controller should yield GET /api/users/list and POST /api/users.
	rec = s.do(t, http.MethodGet, "/api/intel/endpoints?projectId="+itoa2(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoints status %d", rec.Code)
	}
	var epResp struct {
		Endpoints []struct {
			Method     string `json:"method"`
			Path       string `json:"path"`
			SourceFile string `json:"sourceFile"`
			SourceLine int    `json:"sourceLine"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &epResp); err != nil {
		t.Fatalf("endpoints parse: %v", err)
	}
	haveGet, havePost := false, false
	for _, ep := range epResp.Endpoints {
		if ep.SourceLine == 0 || ep.SourceFile == "" {
			t.Errorf("endpoint %s missing provenance", ep.Path)
		}
		if ep.Method == "GET" && ep.Path == "/api/users/list" {
			haveGet = true
		}
		if ep.Method == "POST" && ep.Path == "/api/users" {
			havePost = true
		}
	}
	if !haveGet {
		t.Errorf("missing GET /api/users/list, got %+v", epResp.Endpoints)
	}
	if !havePost {
		t.Errorf("missing POST /api/users, got %+v", epResp.Endpoints)
	}

	// Entities: sys_user must have id (non-nullable PK) and nickname columns.
	rec = s.do(t, http.MethodGet, "/api/intel/entities?projectId="+itoa2(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("entities status %d", rec.Code)
	}
	var entResp struct {
		Entities []struct {
			Table      string `json:"table"`
			Column     string `json:"column"`
			Nullable   bool   `json:"nullable"`
			SourceLine int    `json:"sourceLine"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entResp); err != nil {
		t.Fatalf("entities parse: %v", err)
	}
	cols := map[string]bool{}
	for _, e := range entResp.Entities {
		if e.Table != "sys_user" {
			t.Errorf("entity table = %q, want sys_user", e.Table)
		}
		if e.SourceLine == 0 {
			t.Errorf("entity %s missing provenance", e.Column)
		}
		cols[e.Column] = e.Nullable
	}
	if nullable, ok := cols["id"]; !ok || nullable {
		t.Errorf("id column should be non-nullable PK, got %+v", cols)
	}
	if _, ok := cols["nickname"]; !ok {
		t.Errorf("nickname column missing, got %+v", cols)
	}

	// List projects then delete.
	rec = s.do(t, http.MethodGet, "/api/intel/projects", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status %d", rec.Code)
	}
	rec = s.do(t, http.MethodDelete, "/api/intel/projects/"+itoa2(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIntelAnalyzeDeterministicOutputs verifies the deterministic analyze
// enrichments beyond the M1 scan: sensitive-field findings, gateway routes and
// the dependency/environment/SBOM overview.
func TestIntelAnalyzeDeterministicOutputs(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><mysql.version>8.0.33</mysql.version></properties>
  <dependencies>
    <dependency>
      <groupId>mysql</groupId>
      <artifactId>mysql-connector-java</artifactId>
      <version>${mysql.version}</version>
    </dependency>
  </dependencies>
</project>`)
	writeTestFile(t, filepath.Join(root, "src/main/resources/bootstrap.yml"), `spring:
  application:
    name: user-service
  cloud:
    nacos:
      discovery:
        metadata:
          gateway.paths: /api/users/**
`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "password")
    private String password;
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project response: %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Security finding: the password column is detected as sensitive.
	rec = s.do(t, http.MethodGet, "/api/intel/findings?projectId="+jsonInt(proj.ID), "", wh)
	var fResp struct {
		Findings []struct {
			Detector  string `json:"detector"`
			Category  string `json:"category"`
			Severity  string `json:"severity"`
			Location  string `json:"location"`
			CveOrRule string `json:"cveOrRuleId"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil {
		t.Fatalf("findings parse: %v", err)
	}
	foundPassword := false
	for _, f := range fResp.Findings {
		if f.Detector == "security" && f.CveOrRule == "password" && f.Severity == "critical" {
			foundPassword = true
		}
	}
	if !foundPassword {
		t.Errorf("no security finding for password, got %+v", fResp.Findings)
	}

	// Gateway routes: user-service → /api/users/**.
	rec = s.do(t, http.MethodGet, "/api/intel/gateway-routes?projectId="+jsonInt(proj.ID), "", wh)
	var gResp struct {
		GatewayRoutes []struct {
			Service   string `json:"service"`
			PathsJSON string `json:"pathsJson"`
		} `json:"gatewayRoutes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gResp); err != nil {
		t.Fatalf("gateway parse: %v", err)
	}
	foundRoute := false
	for _, r := range gResp.GatewayRoutes {
		if r.Service == "user-service" && stringsContains(r.PathsJSON, "/api/users/**") {
			foundRoute = true
		}
	}
	if !foundRoute {
		t.Errorf("no user-service gateway route, got %+v", gResp.GatewayRoutes)
	}

	// Overview: deps (mysql-connector-java) + env (mysql/nacos) + sbom.
	rec = s.do(t, http.MethodGet, "/api/intel/overview?projectId="+jsonInt(proj.ID), "", wh)
	var oResp struct {
		Overview *struct {
			DepsJSON string `json:"depsJson"`
			EnvJSON  string `json:"envJson"`
			SbomJSON string `json:"sbomJson"`
		} `json:"overview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &oResp); err != nil {
		t.Fatalf("overview parse: %v", err)
	}
	if oResp.Overview == nil {
		t.Fatal("overview is nil")
	}
	if !stringsContains(oResp.Overview.DepsJSON, "mysql-connector-java") {
		t.Errorf("deps missing mysql-connector-java: %s", oResp.Overview.DepsJSON)
	}
	if !stringsContains(oResp.Overview.EnvJSON, "mysql") {
		t.Errorf("env missing mysql: %s", oResp.Overview.EnvJSON)
	}
	if oResp.Overview.SbomJSON == "" {
		t.Error("sbom is empty")
	}
}

// TestIntelAnalyzeNoDuplicateAccumulation verifies that analyzing the same
// project twice does not duplicate entities, endpoints, test cases or findings
// (module ids change per scan; the child tables must be replaced by project).
func TestIntelAnalyzeNoDuplicateAccumulation(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<demo.UserEntity> list() { return null; }
}`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "password")
    private String password;
}`)
	writeTestFile(t, filepath.Join(root, "src/test/java/demo/UserControllerTest.java"), `package demo;
import org.junit.jupiter.api.Test;
public class UserControllerTest {
    @Test
    public void listWorks() {}
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	count := func(path, field string) int {
		t.Helper()
		rec := s.do(t, http.MethodGet, path+"?projectId="+jsonInt(proj.ID), "", wh)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status %d: %s", path, rec.Code, rec.Body.String())
		}
		raw := map[string][]json.RawMessage{}
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("%s parse: %v", path, err)
		}
		return len(raw[field])
	}

	for round := 0; round < 2; round++ {
		rec = s.do(t, http.MethodPost, "/api/intel/analyze",
			`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
		if rec.Code != http.StatusOK {
			t.Fatalf("analyze round %d status %d: %s", round, rec.Code, rec.Body.String())
		}
	}

	entities := count("/api/intel/entities", "entities")
	endpoints := count("/api/intel/endpoints", "endpoints")
	cases := count("/api/intel/test-cases", "testCases")
	findings := count("/api/intel/findings", "findings")

	if entities != 2 {
		t.Errorf("entities = %d, want 2 (id + password, no duplicates)", entities)
	}
	if endpoints != 1 {
		t.Errorf("endpoints = %d, want 1", endpoints)
	}
	if cases != 1 {
		t.Errorf("test cases = %d, want 1", cases)
	}

	// A third analyze must not grow the finding count (dedup by rule+location).
	before := findings
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("third analyze status %d: %s", rec.Code, rec.Body.String())
	}
	after := count("/api/intel/findings", "findings")
	if after != before {
		t.Errorf("findings grew %d -> %d across re-analyze (accumulation)", before, after)
	}
}

// TestIntelGitProjectClone verifies that a source=git project is cloned into
// the configured repos_dir on first analyze and then analyzed like a local one.
func TestIntelGitProjectClone(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// Build a git repo with a Java module.
	repo := t.TempDir()
	writeTestFile(t, filepath.Join(repo, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(repo, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
}`)
	gitRun(t, repo, "init", "-b", "main")
	gitRun(t, repo, "config", "user.email", "test@example.com")
	gitRun(t, repo, "config", "user.name", "test")
	gitRun(t, repo, "add", ".")
	gitRun(t, repo, "commit", "-m", "init")

	reposDir := t.TempDir()
	if err := s.store.SetSetting(context.Background(), "intel.repos_dir", reposDir); err != nil {
		t.Fatalf("set repos_dir: %v", err)
	}

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"gitdemo","source":"git","gitUrl":"`+filepath.ToSlash(repo)+`","gitRef":"main"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create git project status %d: %s", rec.Code, rec.Body.String())
	}
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create response: %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze git project status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodGet, "/api/intel/entities?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("entities status %d", rec.Code)
	}
	var entResp struct {
		Entities []struct {
			Table  string `json:"table"`
			Column string `json:"column"`
		} `json:"entities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &entResp); err != nil {
		t.Fatalf("entities parse: %v", err)
	}
	if len(entResp.Entities) != 1 || entResp.Entities[0].Column != "id" {
		t.Fatalf("git clone analyze did not yield the id column, got %+v", entResp.Entities)
	}
}

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

// TestIntelAndroidBindings verifies the client field-binding pipeline: an
// Android module's DataBinding layouts are extracted into the must-display
// field list and exposed through /api/intel/android-bindings.
func TestIntelAndroidBindings(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "clients/android/build.gradle.kts"),
		`plugins { id("com.android.application") }`)
	writeTestFile(t, filepath.Join(root, "clients/android/src/main/res/layout/activity_main.xml"),
		`<layout>
  <LinearLayout>
    <TextView android:text="@{viewModel.userName}" />
    <ImageView android:src="@{viewModel.bannerList.title}" />
  </LinearLayout>
</layout>`)

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

	rec = s.do(t, http.MethodGet, "/api/intel/android-bindings?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("android-bindings status %d: %s", rec.Code, rec.Body.String())
	}
	var bResp struct {
		Bindings []struct {
			Page       string `json:"page"`
			FieldPath  string `json:"fieldPath"`
			Widget     string `json:"widget"`
			SourceFile string `json:"sourceFile"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bResp); err != nil {
		t.Fatalf("bindings parse: %v", err)
	}
	if len(bResp.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2: %+v", len(bResp.Bindings), bResp.Bindings)
	}
	got := map[string]string{}
	for _, b := range bResp.Bindings {
		got[b.FieldPath] = b.Widget
	}
	if got["userName"] != "TextView" {
		t.Errorf("userName widget = %q, want TextView", got["userName"])
	}
	if got["bannerList.title"] != "ImageView" {
		t.Errorf("bannerList.title widget = %q, want ImageView", got["bannerList.title"])
	}
	if bResp.Bindings[0].Page != "activity_main" {
		t.Errorf("page = %q, want activity_main", bResp.Bindings[0].Page)
	}
}

// TestIntelFeatureSingleTest verifies the feature single-test: it calls a
// feature's endpoints against a live base URL and validates the response body
// against the extracted field contract.
func TestIntelFeatureSingleTest(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	// A live backend that returns a matching DTO-shaped response.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"id":1,"nickname":"alice"}]`))
	}))
	defer backend.Close()

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<demo.UserEntity> list() { return null; }
}`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
    @Column(name = "id", nullable = false)
    private Long id;
    @Column(name = "nickname")
    private String nickname;
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

	// Resolve the auto-clustered feature (controller name "User").
	rec = s.do(t, http.MethodGet, "/api/intel/features?projectId="+jsonInt(proj.ID), "", wh)
	var fResp struct {
		Features []struct {
			ID     int64  `json:"id"`
			Anchor string `json:"anchor"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil {
		t.Fatalf("features parse: %v", err)
	}
	if len(fResp.Features) != 1 || fResp.Features[0].Anchor != "User" {
		t.Fatalf("features = %+v, want one User feature", fResp.Features)
	}
	featID := fResp.Features[0].ID

	rec = s.do(t, http.MethodPost, "/api/intel/features/test",
		`{"projectId":`+jsonInt(proj.ID)+`,"featureId":`+jsonInt(featID)+`,"baseUrl":"`+backend.URL+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("feature test status %d: %s", rec.Code, rec.Body.String())
	}
	var tResp struct {
		Results []struct {
			Method   string `json:"method"`
			Path     string `json:"path"`
			Status   int    `json:"status"`
			OK       bool   `json:"ok"`
			Contract *struct {
				Passed int `json:"passed"`
				Failed int `json:"failed"`
			} `json:"contract"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tResp); err != nil {
		t.Fatalf("feature test parse: %v", err)
	}
	if len(tResp.Results) != 1 {
		t.Fatalf("results = %d, want 1: %s", len(tResp.Results), rec.Body.String())
	}
	r := tResp.Results[0]
	if r.Status != 200 || !r.OK {
		t.Errorf("endpoint not reachable: status=%d ok=%v", r.Status, r.OK)
	}
	if r.Contract == nil || r.Contract.Failed != 0 {
		t.Errorf("contract check failed: %+v", r.Contract)
	}
}

// TestIntelWebBindings verifies the Web client field-binding pipeline: a Vue
// module's template bindings are extracted into the must-display field list and
// exposed through /api/intel/web-bindings.
func TestIntelWebBindings(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "clients/admin-vue/package.json"),
		`{"dependencies":{"vue":"3.4.0"}}`)
	writeTestFile(t, filepath.Join(root, "clients/admin-vue/src/views/users/UserDetail.vue"),
		`<template>
  <div>
    <span>{{ detail.userId }}</span>
    <span>{{ detail.nickname || '-' }}</span>
    <input v-model="activeTab" />
  </div>
</template>
<script>export default { name: 'UserDetail' }</script>`)

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

	rec = s.do(t, http.MethodGet, "/api/intel/web-bindings?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("web-bindings status %d: %s", rec.Code, rec.Body.String())
	}
	var bResp struct {
		Bindings []struct {
			Page      string `json:"page"`
			FieldPath string `json:"fieldPath"`
			Slot      string `json:"slot"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bResp); err != nil {
		t.Fatalf("bindings parse: %v", err)
	}
	if len(bResp.Bindings) != 3 {
		t.Fatalf("bindings = %d, want 3: %+v", len(bResp.Bindings), bResp.Bindings)
	}
	got := map[string]string{}
	for _, b := range bResp.Bindings {
		got[b.FieldPath] = b.Slot
	}
	if got["detail.userId"] != "interpolation" {
		t.Errorf("detail.userId slot = %q, want interpolation", got["detail.userId"])
	}
	if got["detail.nickname"] != "interpolation" {
		t.Errorf("detail.nickname slot = %q, want interpolation", got["detail.nickname"])
	}
	if got["activeTab"] != "v-model" {
		t.Errorf("activeTab slot = %q, want v-model", got["activeTab"])
	}
}

// TestIntelIosBindings verifies the iOS client field-binding pipeline: an iOS
// module's SwiftUI views are extracted into the must-display field list and
// exposed through /api/intel/ios-bindings.
func TestIntelIosBindings(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "clients/echo-app-ios/EchoApp.xcodeproj/project.pbxproj"), ``)
	writeTestFile(t, filepath.Join(root, "clients/echo-app-ios/EchoApp/Views/Home/PostCardView.swift"),
		`import SwiftUI
struct PostCardView: View {
    let post: Post
    var body: some View {
        VStack {
            Text(post.content)
            EchoAsyncImage(url: post.coverURL)
        }
    }
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

	rec = s.do(t, http.MethodGet, "/api/intel/ios-bindings?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("ios-bindings status %d: %s", rec.Code, rec.Body.String())
	}
	var bResp struct {
		Bindings []struct {
			Page      string `json:"page"`
			FieldPath string `json:"fieldPath"`
			Slot      string `json:"slot"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &bResp); err != nil {
		t.Fatalf("bindings parse: %v", err)
	}
	if len(bResp.Bindings) != 2 {
		t.Fatalf("bindings = %d, want 2: %+v", len(bResp.Bindings), bResp.Bindings)
	}
	got := map[string]string{}
	for _, b := range bResp.Bindings {
		got[b.FieldPath] = b.Slot
	}
	if got["post.content"] != "text" {
		t.Errorf("post.content slot = %q, want text", got["post.content"])
	}
	if got["post.coverURL"] != "image" {
		t.Errorf("post.coverURL slot = %q, want image", got["post.coverURL"])
	}
}

// TestIntelEnvEnsureAndInstall verifies the environment layer: requirements are
// detected during analyze, ensure probes each one (with a stubbed docker), and
// a one-click install provisions a middleware container and flips it to ready.
func TestIntelEnvEnsureAndInstall(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
    <dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-data-redis</artifactId><version>3.2.0</version></dependency>
  </dependencies>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	pid := proj.ID

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(pid)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Stub docker: daemon reachable, no containers running initially.
	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	oldRetries := envagent.ProvisionWaitRetries
	envagent.ProvisionWaitRetries = 2
	defer func() { envagent.ProvisionWaitRetries = oldRetries }()
	running := map[string]bool{}
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "info":
			return "24.0.0", nil
		case "ps":
			name := dockerFilterName(args)
			if name != "" && running[name] {
				return name, nil
			}
			return "", nil
		case "run":
			name := dockerRunName(args)
			running[name] = true
			return "abc123def456", nil
		case "rm":
			delete(running, args[len(args)-1])
			return "", nil
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/ensure",
		`{"projectId":`+jsonInt(pid)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure status %d: %s", rec.Code, rec.Body.String())
	}
	var envResp struct {
		Services []struct {
			Service  string `json:"service"`
			Category string `json:"category"`
			Status   string `json:"status"`
			Provider string `json:"provider"`
		} `json:"services"`
		DockerReady bool `json:"dockerReady"`
		Ready       int  `json:"ready"`
		Missing     int  `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envResp); err != nil {
		t.Fatalf("ensure parse: %v", err)
	}
	if !envResp.DockerReady {
		t.Error("dockerReady should be true with stubbed docker")
	}
	status := map[string]string{}
	for _, svc := range envResp.Services {
		status[svc.Service+"|"+svc.Category] = svc.Status
	}
	if status["mysql|middleware"] != "missing" {
		t.Errorf("mysql status = %q, want missing (docker up, no container)", status["mysql|middleware"])
	}
	if status["redis|middleware"] != "missing" {
		t.Errorf("redis status = %q, want missing", status["redis|middleware"])
	}
	if status["jdk|toolchain"] != "" && status["jdk|toolchain"] == "missing" && envResp.Missing == 0 {
		t.Errorf("unexpected missing tally: %+v", envResp)
	}

	// One-click install mysql -> container "runs", probe flips to ready.
	rec = s.do(t, http.MethodPost, "/api/intel/env/install",
		`{"projectId":`+jsonInt(pid)+`,"service":"mysql"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", rec.Code, rec.Body.String())
	}
	var installResp struct {
		Service struct {
			Service       string `json:"service"`
			Status        string `json:"status"`
			Provider      string `json:"provider"`
			Port          int    `json:"port"`
			ContainerName string `json:"containerName"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &installResp); err != nil {
		t.Fatalf("install parse: %v", err)
	}
	if installResp.Service.Status != "ready" {
		t.Errorf("post-install mysql status = %q, want ready", installResp.Service.Status)
	}
	if installResp.Service.Provider != "container" {
		t.Errorf("mysql provider = %q, want container", installResp.Service.Provider)
	}
	if installResp.Service.Port != 3306 {
		t.Errorf("mysql port = %d, want 3306", installResp.Service.Port)
	}

	// Stop -> back to missing.
	rec = s.do(t, http.MethodPost, "/api/intel/env/stop",
		`{"projectId":`+jsonInt(pid)+`,"service":"mysql"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/status?projectId="+jsonInt(pid), "", wh)
	var statusResp struct {
		Services []struct {
			Service string `json:"service"`
			Status  string `json:"status"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("status parse: %v", err)
	}
	for _, svc := range statusResp.Services {
		if svc.Service == "mysql" && svc.Status != "missing" {
			t.Errorf("mysql status after stop = %q, want missing", svc.Status)
		}
	}
}

// dockerFilterName extracts "name" from docker ps filter arg "name=^/name$".
func dockerFilterName(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "name=^/") && strings.HasSuffix(a, "$") {
			return strings.TrimSuffix(strings.TrimPrefix(a, "name=^/"), "$")
		}
	}
	return ""
}

// dockerRunName extracts the container name following "--name".
func dockerRunName(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	if len(args) >= 4 {
		return args[3]
	}
	return ""
}

// TestIntelEnvGateBlocksRun verifies the environment gate: a run is rejected
// before any command executes when required middleware is missing.
func TestIntelEnvGateBlocksRun(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
  </dependencies>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "info" {
			return "24.0.0", nil
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code == 200 {
		t.Fatalf("run should be gated, got 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "环境门禁") {
		t.Errorf("run failure should mention 环境门禁: %s", rec.Body.String())
	}
}

// TestIntelFixApplyAndRollback verifies the fix workflow end-to-end: a proposed
// fix applies its edit into the project working tree with a backup, then a
// one-click rollback restores the original content.
func TestIntelFixApplyAndRollback(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.properties"), "host=127.0.0.1\npassword=plain\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	fx := &store.IntelFix{
		ProjectID: proj.ID,
		Kind:      "ai-suggest",
		Title:     "mask password",
		DiffJSON:  `[{"file":"app.properties","oldText":"password=plain","newText":"password=*****","line":2,"confidence":"high"}]`,
		Status:    "proposed",
	}
	ctx := context.Background()
	if err := s.store.CreateIntelFix(ctx, fx); err != nil {
		t.Fatalf("create fix: %v", err)
	}

	// Apply -> file updated.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/apply", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=*****") {
		t.Errorf("after apply file has: %q", string(data))
	}
	if strings.Contains(string(data), "password=plain") {
		t.Errorf("after apply old password still present: %q", string(data))
	}

	// Rollback -> restored.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/rollback", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status %d: %s", rec.Code, rec.Body.String())
	}
	data, err = os.ReadFile(filepath.Join(root, "app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=plain") {
		t.Errorf("after rollback file has: %q", string(data))
	}

	// Invalid state: applying a rolled-back fix is rejected.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/apply", "", wh)
	if rec.Code == 200 {
		t.Errorf("apply after rollback should be rejected")
	}
}

// TestWhitelistedTestCommand verifies that a reviewed command whitelist is
// honored at run time: matching test intent is chosen over the tool default,
// non-matching entries fall back to default, and JSON is parsed without a shell.
func TestWhitelistedTestCommand(t *testing.T) {
	// Human-reviewed whitelist with an extra coverage goal -> that entry wins.
	got := whitelistedTestCommand(`["mvn clean install","mvn test -Dcoverage"]`, "maven", "")
	if len(got) != 3 || got[0] != "mvn" || got[1] != "test" {
		t.Errorf("maven whitelist = %v, want [mvn test -Dcoverage]", got)
	}
	// go: only "go test" entries qualify.
	got = whitelistedTestCommand(`["go vet ./...","go test ./internal/..."]`, "go", "")
	if len(got) != 3 || got[1] != "test" || got[2] != "./internal/..." {
		t.Errorf("go whitelist = %v, want [go test ./internal/...]", got)
	}
	// gradle android: testDebugUnitTest intent matches.
	got = whitelistedTestCommand(`["./gradlew assembleDebug"]`, "gradle", "android")
	if got != nil {
		t.Errorf("assembleDebug should not match test intent: %v", got)
	}
	got = whitelistedTestCommand(`["./gradlew assembleDebug","./gradlew testDebugUnitTest"]`, "gradle", "android")
	if len(got) != 2 || got[1] != "testDebugUnitTest" {
		t.Errorf("gradle android whitelist = %v", got)
	}
	// Empty / invalid whitelist falls back to default.
	if whitelistedTestCommand("", "go", "") != nil {
		t.Error("empty whitelist should fall back")
	}
	if whitelistedTestCommand("not-json", "go", "") != nil {
		t.Error("invalid whitelist should fall back")
	}
}

// TestIntelModuleCommandsUpdate verifies the per-module command whitelist edit:
// a PUT replaces commands_json and the updated list is returned.
func TestIntelModuleCommandsUpdate(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)

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

	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Modules []struct {
			ID           int64  `json:"id"`
			CommandsJSON string `json:"commandsJson"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if len(detail.Modules) != 1 {
		t.Fatalf("modules = %d, want 1", len(detail.Modules))
	}
	mod := detail.Modules[0]
	if !strings.Contains(mod.CommandsJSON, "mvn test") {
		t.Errorf("default commands missing mvn test: %s", mod.CommandsJSON)
	}

	rec = s.do(t, http.MethodPut, "/api/intel/modules/"+jsonInt(mod.ID)+"/commands",
		`{"commands":["mvn clean install","mvn test -Dcoverage"]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("update commands status %d: %s", rec.Code, rec.Body.String())
	}
	var upd struct {
		Module struct {
			CommandsJSON string `json:"commandsJson"`
		} `json:"module"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("update parse: %v", err)
	}
	if !strings.Contains(upd.Module.CommandsJSON, "mvn clean install") {
		t.Errorf("updated commands missing entry: %s", upd.Module.CommandsJSON)
	}
}

// TestIntelEnvExternalConfig verifies an externally provided middleware
// endpoint: it is probed once, persisted with provider=external, and survives
// subsequent re-probes (the gate treats it as ready when reachable).
func TestIntelEnvExternalConfig(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
  </dependencies>
</project>`)

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

	// A reachable endpoint to point the external config at.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rec = s.do(t, http.MethodPost, "/api/intel/env/external",
		`{"projectId":`+jsonInt(proj.ID)+`,"service":"mysql","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"username":"root","password":"secret"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("external status %d: %s", rec.Code, rec.Body.String())
	}
	var extResp struct {
		Service struct {
			Service  string `json:"service"`
			Status   string `json:"status"`
			Provider string `json:"provider"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &extResp); err != nil {
		t.Fatalf("external parse: %v", err)
	}
	if extResp.Service.Status != "ready" || extResp.Service.Provider != "external" {
		t.Errorf("external = %+v, want ready/external", extResp.Service)
	}

	// Re-probe keeps the external row (docker stub would otherwise mark missing).
	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		return "", nil
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/status?projectId="+jsonInt(proj.ID), "", wh)
	var stResp struct {
		Services []struct {
			Service  string `json:"service"`
			Provider string `json:"provider"`
			Status   string `json:"status"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stResp); err != nil {
		t.Fatalf("status parse: %v", err)
	}
	found := false
	for _, svc := range stResp.Services {
		if svc.Service == "mysql" {
			found = true
			if svc.Provider != "external" || svc.Status != "ready" {
				t.Errorf("re-probe lost external config: %+v", svc)
			}
		}
	}
	if !found {
		t.Errorf("mysql missing from status services: %+v", stResp.Services)
	}
}

// TestIntelAIRulesCRUD verifies the AI suggestion rules management API.
func TestIntelAIRulesCRUD(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	rec := s.do(t, http.MethodPost, "/api/intel/ai-rules",
		`{"name":"避免裸 SQL","prompt":"识别代码中直接拼接 SQL 的位置并给出建议","target":"risk","severity":"high"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create rule status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Rule struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Prompt string `json:"prompt"`
			Target string `json:"target"`
		} `json:"rule"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create parse: %v", err)
	}
	if created.Rule.ID == 0 || created.Rule.Name != "避免裸 SQL" {
		t.Fatalf("created rule = %+v", created.Rule)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/ai-rules", "", wh)
	var list struct {
		Rules []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Rules) != 1 || list.Rules[0].ID != created.Rule.ID {
		t.Fatalf("list = %+v, want 1 rule", list.Rules)
	}

	rec = s.do(t, http.MethodPut, "/api/intel/ai-rules/"+jsonInt(created.Rule.ID),
		`{"enabled":false}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/ai-rules?enabled=true", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("enabled list parse: %v", err)
	}
	if len(list.Rules) != 0 {
		t.Errorf("enabled list = %d, want 0 after disabling", len(list.Rules))
	}

	rec = s.do(t, http.MethodDelete, "/api/intel/ai-rules/"+jsonInt(created.Rule.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/ai-rules", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Rules) != 0 {
		t.Errorf("after delete rules = %d, want 0", len(list.Rules))
	}
}

// TestIntelFeatureManagement verifies manual feature points: create, rename,
// reorder, and survival of manual features across a rescan (auto replaced only).
func TestIntelFeatureManagement(t *testing.T) {
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

	// Create a manual feature.
	rec = s.do(t, http.MethodPost, "/api/intel/features?projectId="+jsonInt(proj.ID),
		`{"name":"App 首页","ends":["android","bff"]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create feature status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Feature struct {
			ID     int64  `json:"id"`
			Source string `json:"source"`
			Name   string `json:"name"`
		} `json:"feature"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create parse: %v", err)
	}
	if created.Feature.Source != "manual" {
		t.Errorf("created feature source = %q, want manual", created.Feature.Source)
	}
	manualID := created.Feature.ID

	// Rename it.
	rec = s.do(t, http.MethodPut, "/api/intel/features/"+jsonInt(manualID),
		`{"name":"App 首页（改）"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename status %d: %s", rec.Code, rec.Body.String())
	}

	// Rescan: the manual feature must survive.
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	rec = s.do(t, http.MethodGet, "/api/intel/features?projectId="+jsonInt(proj.ID), "", wh)
	var list struct {
		Features []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	manualFound := false
	autoCount := 0
	for _, f := range list.Features {
		if f.Source == "manual" {
			manualFound = true
			if f.Name != "App 首页（改）" {
				t.Errorf("manual name after rescan = %q", f.Name)
			}
		} else {
			autoCount++
		}
	}
	if !manualFound {
		t.Error("manual feature lost after rescan")
	}
	if autoCount != 1 {
		t.Errorf("auto features = %d, want 1", autoCount)
	}
}

// TestIntelEnvSchemaInit verifies lib initialization: discovered SQL scripts
// are fed to a ready MySQL container via stubbed "docker exec -i mysql".
func TestIntelEnvSchemaInit(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src/main/resources/db/migration/V1__init.sql"),
		"CREATE TABLE t (id INT);\n")
	writeTestFile(t, filepath.Join(root, "src/main/resources/application.yml"),
		"spring:\n  datasource:\n    url: jdbc:mysql://127.0.0.1:3306/db")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	// Mark mysql as a ready container with a known password (skips real docker).
	now := time.Now()
	s.store.UpsertIntelEnvServices(context.Background(), proj.ID, []*store.IntelEnvService{{
		ProjectID:     proj.ID,
		Service:       "mysql",
		Category:      "middleware",
		Provider:      "container",
		Status:        "ready",
		ContainerName: "intel-X-mysql",
		Password:      "pw",
		HealthCheckAt: &now,
	}})

	// Stub the stdin-fed docker runner to record the script content.
	old := envagent.RunDockerInput
	defer func() { envagent.RunDockerInput = old }()
	got := ""
	count := 0
	envagent.RunDockerInput = func(ctx context.Context, stdin string, args ...string) (string, error) {
		got = stdin
		count++
		if len(args) < 2 || args[0] != "exec" || args[1] != "-i" {
			t.Errorf("docker exec args = %v", args)
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/schema-init",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("schema-init status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Executed int `json:"executed"`
		Total    int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("schema-init parse: %v", err)
	}
	if resp.Executed != 1 || resp.Total != 1 {
		t.Errorf("schema-init executed=%d total=%d, want 1/1", resp.Executed, resp.Total)
	}
	if count != 1 {
		t.Errorf("docker exec call count = %d, want 1", count)
	}
	if !strings.Contains(got, "CREATE TABLE t") {
		t.Errorf("script not fed to docker exec: %q", got)
	}
}

// TestIntelDevices verifies the Android device management: wireless connect
// (stubbed adb), list, bind to a project, and delete.
func TestIntelDevices(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	old := envagent.RunADB
	defer func() { envagent.RunADB = old }()
	envagent.RunADB = func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "connect":
			return "connected to " + args[1], nil
		case "devices":
			return "List of devices attached\n192.0.2.9:5555 device product:echo model:EchoPhone\n", nil
		}
		return "", nil
	}

	rec := s.do(t, http.MethodPost, "/api/intel/env/devices/connect",
		`{"ip":"192.0.2.9","port":5555}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status %d: %s", rec.Code, rec.Body.String())
	}
	var cResp struct {
		Device struct {
			ID     int64  `json:"id"`
			Serial string `json:"serial"`
			Status string `json:"status"`
		} `json:"device"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cResp); err != nil {
		t.Fatalf("connect parse: %v", err)
	}
	if cResp.Device.Serial != "192.0.2.9:5555" {
		t.Errorf("device serial = %q", cResp.Device.Serial)
	}
	deviceID := cResp.Device.ID

	rec = s.do(t, http.MethodGet, "/api/intel/env/devices", "", wh)
	var list struct {
		Devices []struct {
			ID    int64 `json:"id"`
			Bound bool  `json:"bound"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(list.Devices))
	}

	rec = s.do(t, http.MethodPut, "/api/intel/env/devices/"+jsonInt(deviceID)+"/bind",
		`{"projectId":1}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodDelete, "/api/intel/env/devices/"+jsonInt(deviceID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/devices", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Devices) != 0 {
		t.Errorf("devices after delete = %d, want 0", len(list.Devices))
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// loginWeb logs in as the web admin and returns the auth header map.
func loginWeb(t *testing.T, s *Server) map[string]string {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status %d: %s", rec.Code, rec.Body.String())
	}
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	if login.Session == "" {
		t.Fatal("no session id")
	}
	return map[string]string{"X-Web-Session": login.Session}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa2(n int64) string {
	return jsonInt(n)
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func stringsContains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestSplitLocation(t *testing.T) {
	cases := []struct {
		in   string
		file string
		line int
	}{
		{"activity-provider/src/main/java/A.java:68", "activity-provider/src/main/java/A.java", 68},
		{"A.java:3", "A.java", 3},
		{"A.java", "A.java", 0},
		{"", "", 0},
		{"path/A.java:x", "path/A.java:x", 0},
	}
	for _, c := range cases {
		f, l := splitLocation(c.in)
		if f != c.file || l != c.line {
			t.Errorf("splitLocation(%q) = (%q,%d), want (%q,%d)", c.in, f, l, c.file, c.line)
		}
	}
}
