package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestIntelAnalyzeMultiSource verifies analysis covers associated source repos:
// modules from a second repo are detected under a "@<端>/" prefix and their
// endpoint contracts are aggregated alongside the primary repo's.
func TestIntelAnalyzeMultiSource(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	mainRoot := t.TempDir()
	writeTestFile(t, filepath.Join(mainRoot, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(mainRoot, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<demo.UserEntity> list() { return null; }
}`)

	bffRoot := t.TempDir()
	writeTestFile(t, filepath.Join(bffRoot, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(bffRoot, "src/main/java/bff/OrderController.java"), `package bff;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/bff/orders")
public class OrderController {
    @GetMapping("/list")
    public java.util.List<Object> list() { return null; }
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"multi","source":"local","localPath":"`+filepath.ToSlash(mainRoot)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"bff","source":"local","localPath":"`+filepath.ToSlash(bffRoot)+`"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sources status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// 模块列表应包含 "@bff/" 前缀模块（来自关联仓库）。
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Modules []struct {
			RelPath string `json:"relPath"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	prefixed := false
	for _, m := range detail.Modules {
		if strings.HasPrefix(m.RelPath, "@bff/") {
			prefixed = true
		}
	}
	if !prefixed {
		t.Fatalf("no @bff prefixed module found: %+v", detail.Modules)
	}

	// 关联仓库的端点在契约列表里（与主仓库契约一同聚合）。
	rec = s.do(t, http.MethodGet, "/api/intel/endpoints?projectId="+jsonInt(proj.ID), "", wh)
	var epResp struct {
		Endpoints []struct {
			Path string `json:"path"`
		} `json:"endpoints"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &epResp); err != nil {
		t.Fatalf("endpoints parse: %v", err)
	}
	foundUsers, foundOrders := false, false
	for _, e := range epResp.Endpoints {
		if strings.Contains(e.Path, "/api/users") {
			foundUsers = true
		}
		if strings.Contains(e.Path, "/api/bff/orders") {
			foundOrders = true
		}
	}
	if !foundUsers || !foundOrders {
		t.Errorf("expected both primary and associated endpoints, got %+v (users=%v orders=%v)",
			epResp.Endpoints, foundUsers, foundOrders)
	}
}

// TestIntelAnalyzeIncremental verifies the module add/remove incremental path:
// adding an associated source repo appends its module's contracts without
// wiping the untouched main-module data, and removing it deletes that module's
// data again. The auto-incremental analyzes run in background (fired by the
// sources PUT handler), so the test polls for the async side-effects.
func TestIntelAnalyzeIncremental(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	mainRoot := t.TempDir()
	writeTestFile(t, filepath.Join(mainRoot, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(mainRoot, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController {
    @GetMapping("/list")
    public java.util.List<Object> list() { return null; }
}`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"inc","source":"local","localPath":"`+filepath.ToSlash(mainRoot)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	// 显式全量分析（等创建时的后台首次分析完成后执行，保证结果是稳定的）。
	rec = s.do(t, http.MethodPost, "/api/intel/analyze", `{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	projectModules := func() []string {
		r := s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
		var d struct {
			Modules []struct {
				RelPath string `json:"relPath"`
			} `json:"modules"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &d)
		out := make([]string, 0, len(d.Modules))
		for _, mod := range d.Modules {
			out = append(out, mod.RelPath)
		}
		return out
	}
	endpointPaths := func() []string {
		r := s.do(t, http.MethodGet, "/api/intel/endpoints?projectId="+jsonInt(proj.ID), "", wh)
		var d struct {
			Endpoints []struct {
				Path string `json:"path"`
			} `json:"endpoints"`
		}
		_ = json.Unmarshal(r.Body.Bytes(), &d)
		out := make([]string, 0, len(d.Endpoints))
		for _, e := range d.Endpoints {
			out = append(out, e.Path)
		}
		return out
	}
	hasRel := func(mods []string, target string) bool {
		for _, m := range mods {
			if strings.HasPrefix(m, target) {
				return true
			}
		}
		return false
	}
	hasPath := func(paths []string, sub string) bool {
		for _, p := range paths {
			if strings.Contains(p, sub) {
				return true
			}
		}
		return false
	}
	pollUntil := func(cond func() bool, what string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s (modules=%v endpoints=%v)", what, projectModules(), endpointPaths())
	}

	// 主仓库全量分析后：主模块 + 主端点。
	if !hasRel(projectModules(), ".") {
		t.Fatalf("main module missing after analyze: %v", projectModules())
	}
	if !hasPath(endpointPaths(), "/api/users") {
		t.Fatalf("main endpoint missing after analyze: %v", endpointPaths())
	}

	// 添加关联源码仓库 → 增量分析应把 @web 模块与其契约追加进来，且不动主模块数据。
	webRoot := t.TempDir()
	writeTestFile(t, filepath.Join(webRoot, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(webRoot, "src/main/java/web/OrderController.java"), `package web;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/orders")
public class OrderController {
    @GetMapping("/list")
    public java.util.List<Object> list() { return null; }
}`)
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"web","source":"local","localPath":"`+filepath.ToSlash(webRoot)+`"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sources status %d: %s", rec.Code, rec.Body.String())
	}
	pollUntil(func() bool {
		return hasRel(projectModules(), "@web/") && hasPath(endpointPaths(), "/api/orders")
	}, "associated web module scanned")
	if !hasRel(projectModules(), ".") || !hasPath(endpointPaths(), "/api/users") {
		t.Fatalf("main module data lost after incremental add: modules=%v endpoints=%v",
			projectModules(), endpointPaths())
	}

	// 移除关联源码 → 增量应删除 @web 模块及其契约，主仓库数据保留。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources", `{"sources":[]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear sources status %d: %s", rec.Code, rec.Body.String())
	}
	pollUntil(func() bool {
		return !hasRel(projectModules(), "@web/") && !hasPath(endpointPaths(), "/api/orders")
	}, "associated web module removed")
	if !hasPath(endpointPaths(), "/api/users") {
		t.Fatalf("main endpoint missing after incremental remove: %v", endpointPaths())
	}
}

// TestEnsureModuleSummaryNoLLM verifies the module summary is empty (pure
// deterministic stats) when the orchestration LLM is not configured.
func TestEnsureModuleSummaryNoLLM(t *testing.T) {
	s := newTestServer(t)
	mod := &store.IntelModule{ID: 1, RelPath: "x", KindType: "java"}
	got := s.ensureModuleSummary(context.Background(), mod, nil, nil)
	if got != "" {
		t.Errorf("summary without LLM = %q, want empty", got)
	}
}
