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

// TestIntelModuleOverrideApplied verifies the overrides layer consumes a
// confirmed module-role override at read time (the manual value wins while the
// auto role stays stored).
func TestIntelModuleOverrideApplied(t *testing.T) {
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

	// A confirmed override targeting the root module's role.
	ctx := context.Background()
	if err := s.store.CreateIntelOverride(ctx, &store.IntelOverride{
		ProjectID:   proj.ID,
		Target:      "module",
		RowKey:      ".",
		Field:       "role",
		ManualValue: "app",
		Confidence:  "high",
		Status:      "applied",
		Source:      "manual",
	}); err != nil {
		t.Fatalf("create override: %v", err)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Modules []struct {
			RelPath  string `json:"relPath"`
			KindRole string `json:"kindRole"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if len(detail.Modules) != 1 {
		t.Fatalf("modules = %d, want 1", len(detail.Modules))
	}
	if detail.Modules[0].KindRole != "app" {
		t.Errorf("module role = %q, want overridden to app", detail.Modules[0].KindRole)
	}
}

// TestIntelModuleDetail verifies the sub-module detail endpoint returns basic
// module info plus per-module asset counts.
func TestIntelModuleDetail(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project></project>`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserController.java"), `package demo;
import org.springframework.web.bind.annotation.*;
@RestController
@RequestMapping("/api/users")
public class UserController { @GetMapping("/list") public demo.UserEntity list() { return null; } }`)
	writeTestFile(t, filepath.Join(root, "src/main/java/demo/UserEntity.java"), `package demo;
import javax.persistence.*;
@Entity
@Table(name = "sys_user")
public class UserEntity {
    @Id
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
	rec = s.do(t, http.MethodGet, "/api/intel/projects/"+jsonInt(proj.ID), "", wh)
	var detail struct {
		Modules []struct {
			ID       int64  `json:"id"`
			RelPath  string `json:"relPath"`
			KindType string `json:"kindType"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Modules) != 1 {
		t.Fatalf("detail modules: %s", rec.Body.String())
	}
	mod := detail.Modules[0]

	// The list route carries no path id, so the project can only come from
	// ?projectId= — it used to parse the path for a project id, never match, and
	// answer 200 with an empty body.
	rec = s.do(t, http.MethodGet, "/api/intel/modules?projectId="+jsonInt(proj.ID), "", wh)
	var listed struct {
		Modules []struct {
			ID int64 `json:"id"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || len(listed.Modules) != 1 || listed.Modules[0].ID != mod.ID {
		t.Fatalf("modules list: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec = s.do(t, http.MethodGet, "/api/intel/modules", "", wh); rec.Code != http.StatusBadRequest {
		t.Fatalf("modules without projectId: got %d want 400", rec.Code)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/modules/"+jsonInt(mod.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("module detail status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Module struct {
			RelPath   string `json:"relPath"`
			KindType  string `json:"kindType"`
			BuildTool string `json:"buildTool"`
		} `json:"module"`
		Stats struct {
			Endpoints int `json:"endpoints"`
			Entities  int `json:"entities"`
			Cases     int `json:"cases"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("module detail parse: %v", err)
	}
	if resp.Module.RelPath != "." || resp.Module.KindType != "java" || resp.Module.BuildTool != "maven" {
		t.Errorf("module basic info = %+v", resp.Module)
	}
	if resp.Stats.Endpoints != 1 || resp.Stats.Entities != 2 {
		t.Errorf("module stats = %+v, want endpoints=1 entities=2 (列数)", resp.Stats)
	}
}

// TestIntelModuleManualOverridesSurviveReanalyze verifies the human-edit
// mechanism: role/summary edits stored as applied overrides survive a
// re-analysis (auto values are rebuilt, overrides are merged on read).
func TestIntelModuleManualOverridesSurviveReanalyze(t *testing.T) {
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
			ID int64 `json:"id"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil || len(detail.Modules) != 1 {
		t.Fatalf("modules: %s", rec.Body.String())
	}
	modID := detail.Modules[0].ID

	// Human edit role + summary via the overrides endpoint.
	rec = s.do(t, http.MethodPut, "/api/intel/modules/"+jsonInt(modID)+"/overrides",
		`{"role":"app","summary":"人工修正的模块说明"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("overrides status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/modules/"+jsonInt(modID), "", wh)
	var md struct {
		Module struct {
			KindRole string `json:"kindRole"`
			Summary  string `json:"summary"`
		} `json:"module"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &md); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if md.Module.KindRole != "app" || md.Module.Summary != "人工修正的模块说明" {
		t.Fatalf("override not applied: %+v", md.Module)
	}

	// Re-analyze: auto values are rebuilt but overrides must still win.
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-analyze status %d", rec.Code)
	}
	// Module id is stable now; re-fetch by the same id.
	rec = s.do(t, http.MethodGet, "/api/intel/modules/"+jsonInt(modID), "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &md); err != nil {
		t.Fatalf("detail parse: %v", err)
	}
	if md.Module.KindRole != "app" || md.Module.Summary != "人工修正的模块说明" {
		t.Errorf("overrides lost after re-analyze: %+v", md.Module)
	}
}

// TestIntelEndpointSummaryOverrideSurvivesReanalyze verifies a manual endpoint
// summary correction is keyed by "METHOD path" and survives re-analysis.
func TestIntelEndpointSummaryOverrideSurvivesReanalyze(t *testing.T) {
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

	listEndpoints := func() ([]struct {
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Path    string `json:"path"`
		Summary string `json:"summary"`
	}, *httptest.ResponseRecorder) {
		rec := s.do(t, http.MethodGet, "/api/intel/endpoints?projectId="+jsonInt(proj.ID), "", wh)
		var resp struct {
			Endpoints []struct {
				ID      int64  `json:"id"`
				Method  string `json:"method"`
				Path    string `json:"path"`
				Summary string `json:"summary"`
			} `json:"endpoints"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return resp.Endpoints, rec
	}

	eps, rec := listEndpoints()
	if len(eps) == 0 {
		t.Fatalf("no endpoints: %s", rec.Body.String())
	}
	target := eps[0]

	// Human edit the summary.
	rec = s.do(t, http.MethodPut, "/api/intel/endpoints/"+jsonInt(target.ID)+"/overrides",
		`{"summary":"人工修正的接口说明"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("overrides status %d: %s", rec.Code, rec.Body.String())
	}

	// Verify it is applied at read time.
	eps, _ = listEndpoints()
	if len(eps) == 0 || eps[0].Summary != "人工修正的接口说明" {
		t.Fatalf("override not applied: %+v", eps)
	}

	// Re-analyze: auto summary is rebuilt but the manual correction must win.
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-analyze status %d", rec.Code)
	}
	eps, _ = listEndpoints()
	if len(eps) == 0 {
		t.Fatalf("endpoints lost after re-analyze")
	}
	found := false
	for _, ep := range eps {
		if ep.Method == target.Method && ep.Path == target.Path {
			found = true
			if ep.Summary != "人工修正的接口说明" {
				t.Errorf("endpoint override lost after re-analyze: %+v", ep)
			}
		}
	}
	if !found {
		t.Fatalf("endpoint %s %s not found after re-analyze", target.Method, target.Path)
	}
}
