package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
