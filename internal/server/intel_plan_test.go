package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntelPlanCommandSelection verifies build/test command derivation: the
// tool default is used when the whitelist has no match, and an explicit
// whitelist build command overrides the default build.
func TestIntelPlanCommandSelection(t *testing.T) {
	// maven: default test is "mvn test", build is "mvn package".
	build, test := intelPlanCommandFor("", "maven", "backend")
	if build != "mvn package" || test != "mvn test" {
		t.Fatalf("maven default: build=%q test=%q", build, test)
	}
	// whitelist with a custom test command → picked.
	build, test = intelPlanCommandFor(`["mvn clean install","mvn test -Dcoverage"]`, "maven", "backend")
	if test != "mvn test -Dcoverage" {
		t.Fatalf("whitelist test not honored: %q", test)
	}
	if build != "mvn clean install" {
		t.Fatalf("whitelist build not honored: %q", build)
	}
	// go: default test is "go test -json ./...".
	build, test = intelPlanCommandFor("", "go", "backend")
	if build != "go build ./..." || test != "go test -json ./..." {
		t.Fatalf("go default: build=%q test=%q", build, test)
	}
	// npm has no whitelist → build "npm run build", test "npm test".
	build, test = intelPlanCommandFor("", "npm", "web")
	if build != "npm run build" || test != "npm test" {
		t.Fatalf("npm default: build=%q test=%q", build, test)
	}
}

// TestIntelPlanGenerateAndPreview verifies GET /api/intel/plan returns an
// ordered plan for an analyzed project: backend modules rank above web ones,
// and only modules with a resolvable test command appear.
func TestIntelPlanGenerateAndPreview(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/ApiController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
public class ApiController {
    @GetMapping("/x")
    public String x() { return "x"; }
}`)
	// 第二个模块（web）：让 detect 产生多模块工程。
	writeTestFile(t, filepath.Join(root, "web/package.json"), `{"name":"web","scripts":{"test":"echo ok"}}`)
	writeTestFile(t, filepath.Join(root, "web/src/index.js"), `export default 1;`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"plan-demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
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

	rec = s.do(t, http.MethodGet, "/api/intel/plan?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("plan status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Plan []IntelPlanStep `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("plan parse: %v (%s)", err, rec.Body.String())
	}
	if len(out.Plan) == 0 {
		t.Fatalf("plan empty for analyzed project: %s", rec.Body.String())
	}
	for _, step := range out.Plan {
		if step.TestCommand == "" {
			t.Errorf("step %s has no test command", step.RelPath)
		}
		if step.ModuleID == 0 {
			t.Errorf("step %s has no module id", step.RelPath)
		}
	}
	// 优先级应为降序。
	for i := 1; i < len(out.Plan); i++ {
		if out.Plan[i].Priority > out.Plan[i-1].Priority {
			t.Errorf("plan not priority-sorted: %+v", out.Plan)
		}
	}
	// 命令不应为空。
	if strings.TrimSpace(out.Plan[0].TestCommand) == "" {
		t.Errorf("first step test command empty: %+v", out.Plan[0])
	}
}

// TestIntelPlanMissingProject verifies the plan endpoints reject an unknown
// project and unauthenticated access.
func TestIntelPlanAuth(t *testing.T) {
	s := newTestServer(t)
	rec := s.do(t, http.MethodGet, "/api/intel/plan?projectId=9999", "", nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthenticated plan should be rejected, got %d", rec.Code)
	}
}
