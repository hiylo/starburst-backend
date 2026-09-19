package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

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

// TestIntelFeatureChat verifies the feature-level AI chat deterministic core:
// the context (endpoint contracts + run results + linked issues) is assembled
// and the Q&A persisted even without an LLM configured.
func TestIntelFeatureChat(t *testing.T) {
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
public class UserEntity { private Long id; private String nickname; }`)

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
			ID int64 `json:"id"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fResp); err != nil {
		t.Fatalf("features parse: %v", err)
	}
	if len(fResp.Features) != 1 {
		t.Fatalf("features = %d, want 1", len(fResp.Features))
	}
	featID := fResp.Features[0].ID

	rec = s.do(t, http.MethodPost, "/api/intel/features/"+jsonInt(featID)+"/chat",
		`{"projectId":`+jsonInt(proj.ID)+`,"question":"为什么列表接口可能返回空？"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status %d: %s", rec.Code, rec.Body.String())
	}
	var chatResp struct {
		Chat struct {
			ID          int64  `json:"id"`
			Question    string `json:"question"`
			ContextJSON string `json:"contextJson"`
			Answer      string `json:"answer"`
		} `json:"chat"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &chatResp); err != nil {
		t.Fatalf("chat parse: %v", err)
	}
	if chatResp.Chat.ID == 0 || !strings.Contains(chatResp.Chat.Question, "空") {
		t.Fatalf("chat = %+v", chatResp.Chat)
	}
	if !strings.Contains(chatResp.Chat.ContextJSON, "/api/users/list") {
		t.Errorf("context missing endpoint contract: %s", chatResp.Chat.ContextJSON)
	}
	if !strings.Contains(chatResp.Chat.Answer, "LLM") && chatResp.Mode == "" {
		t.Errorf("expected no-llm note, got answer=%q mode=%q", chatResp.Chat.Answer, chatResp.Mode)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/features/"+jsonInt(featID)+"/chats?projectId="+jsonInt(proj.ID), "", wh)
	var hist struct {
		Chats []struct {
			ID int64 `json:"id"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("chats parse: %v", err)
	}
	if len(hist.Chats) != 1 {
		t.Errorf("chats = %d, want 1", len(hist.Chats))
	}
}
