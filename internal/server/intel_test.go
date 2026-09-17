package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
