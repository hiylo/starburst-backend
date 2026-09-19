package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestIntelProjectCommandsUpdate verifies the project-level command whitelist
// edit: a PUT replaces projects.commands_json and the updated list is returned.
func TestIntelProjectCommandsUpdate(t *testing.T) {
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

	// 项目级默认白名单应聚合出 mvn test。
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Project struct {
			CommandsJSON string `json:"commandsJson"`
		} `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if !strings.Contains(detail.Project.CommandsJSON, "mvn test") {
		t.Errorf("default project commands missing mvn test: %s", detail.Project.CommandsJSON)
	}

	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"commandsJson":"[\"mvn clean install\",\"mvn test -Dcoverage\"]"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("update commands status %d: %s", rec.Code, rec.Body.String())
	}
	var upd struct {
		CommandsJSON string `json:"commandsJson"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("update parse: %v", err)
	}
	if !strings.Contains(upd.CommandsJSON, "mvn clean install") {
		t.Errorf("updated commands missing entry: %s", upd.CommandsJSON)
	}

	// 非法白名单（非 JSON 字符串数组）应被拒绝。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"commandsJson":"mvn test"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("invalid commandsJson should 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestIntelProjectSourceAssociationUpdate verifies the detail-page "项目管理"
// edit of the associated source directory/repo (source/localPath/gitUrl/gitRef).
func TestIntelProjectSourceAssociationUpdate(t *testing.T) {
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

	// 切到 git 来源：关联仓库 URL + 分支。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"source":"git","gitUrl":"https://gitlab.example.com/group/demo.git","gitRef":"release-1.0"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("update to git status %d: %s", rec.Code, rec.Body.String())
	}
	var git struct {
		Source    string `json:"source"`
		LocalPath string `json:"localPath"`
		GitURL    string `json:"gitUrl"`
		GitRef    string `json:"gitRef"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &git); err != nil {
		t.Fatalf("git update parse: %v", err)
	}
	if git.Source != "git" || git.GitURL != "https://gitlab.example.com/group/demo.git" || git.GitRef != "release-1.0" {
		t.Errorf("git association mismatch: %+v", git)
	}
	if git.LocalPath != "" {
		t.Errorf("local path should be cleared on git source: %+v", git)
	}

	// git 来源但未填仓库 URL → 400。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"source":"git","gitUrl":"  "}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty git url should 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// 切回本地来源：不存在/非目录路径 → 400。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"source":"local","localPath":"/no/such/dir-xyz"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing local dir should 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// 本地来源：有效目录 → 关联源码目录落库并清空 gitUrl。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID),
		`{"source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("update to local status %d: %s", rec.Code, rec.Body.String())
	}
	var local struct {
		Source    string `json:"source"`
		LocalPath string `json:"localPath"`
		GitURL    string `json:"gitUrl"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &local); err != nil {
		t.Fatalf("local update parse: %v", err)
	}
	if local.Source != "local" || local.LocalPath != filepath.ToSlash(root) || local.GitURL != "" {
		t.Errorf("local association mismatch: %+v", local)
	}
}

// TestIntelProjectSourcesCRUD verifies the associated source repos (多端多仓库)
// list/replace API: add/mix git+local, duplicate end names rejected, empty list
// clears the project's sources.
func TestIntelProjectSourcesCRUD(t *testing.T) {
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

	// 关联两个源：本地 android + git bff。
	androidRoot := t.TempDir()
	writeTestFile(t, filepath.Join(androidRoot, "pom.xml"), `<project></project>`)
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"android","source":"local","localPath":"`+filepath.ToSlash(androidRoot)+`"},
		              {"endName":"bff","source":"git","gitUrl":"https://gitlab.example.com/group/bff.git","gitRef":"main"}]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("put sources status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("get sources status %d", rec.Code)
	}
	var listResp struct {
		Sources []struct {
			EndName   string `json:"endName"`
			Source    string `json:"source"`
			LocalPath string `json:"localPath"`
			GitURL    string `json:"gitUrl"`
			GitRef    string `json:"gitRef"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("sources parse: %v", err)
	}
	if len(listResp.Sources) != 2 {
		t.Fatalf("expected 2 sources, got %+v", listResp.Sources)
	}
	if listResp.Sources[0].EndName != "android" || listResp.Sources[0].Source != "local" ||
		listResp.Sources[0].LocalPath != filepath.ToSlash(androidRoot) {
		t.Errorf("android source mismatch: %+v", listResp.Sources[0])
	}
	if listResp.Sources[1].EndName != "bff" || listResp.Sources[1].Source != "git" ||
		listResp.Sources[1].GitURL != "https://gitlab.example.com/group/bff.git" || listResp.Sources[1].GitRef != "main" {
		t.Errorf("bff source mismatch: %+v", listResp.Sources[1])
	}

	// 空列表清空。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear sources status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources", "", wh)
	var cleared struct {
		Sources []any `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("cleared parse: %v", err)
	}
	if len(cleared.Sources) != 0 {
		t.Errorf("sources not cleared: %+v", cleared.Sources)
	}

	// 重复端名拒绝。
	rec = s.do(t, http.MethodPut, "/api/intel/projects/"+jsonInt(proj.ID)+"/sources",
		`{"sources":[{"endName":"ios","source":"local","localPath":"`+filepath.ToSlash(androidRoot)+`"},
		              {"endName":"ios","source":"git","gitUrl":"https://x/y.git"}]}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("duplicate end name should 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
