package server

// HTTP-level tests for the §11偏差#8 intel trigger/issue endpoints:
// POST /api/intel/sbom, POST /api/intel/scan/findings, POST /api/intel/sync/scan,
// POST /api/intel/issues/{id}/ack, POST /api/intel/issues/{id}/link-feature and
// GET /api/intel/runs/{id}/results. No real test commands are executed; runs
// are seeded through the store only.

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelScanTriggerSbom verifies the on-demand SBOM/overview regeneration and
// the compliance/security findings rescan endpoints end to end.
func TestIntelScanTriggerSbom(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	// .py 文件走合规 secret-hardcode 规则；Java 实体带 password 列触发 security。
	writeTestFile(t, filepath.Join(root, "app_config.py"), "DB_PASSWORD = \"s3cr3t\"\n")
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
		t.Fatalf("create project: %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// SBOM 重新生成：返回带 CycloneDX sbomJson 的 overview。
	rec = s.do(t, http.MethodPost, "/api/intel/sbom",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("sbom status %d: %s", rec.Code, rec.Body.String())
	}
	var sbomResp struct {
		Overview struct {
			SbomJSON string `json:"sbomJson"`
		} `json:"overview"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sbomResp); err != nil {
		t.Fatalf("sbom parse: %v", err)
	}
	if !strings.Contains(sbomResp.Overview.SbomJSON, "CycloneDX") {
		t.Errorf("sbomJson missing CycloneDX marker: %s", sbomResp.Overview.SbomJSON)
	}

	// 分析后再加一个含密钥的文件，重扫应产生新 finding。
	writeTestFile(t, filepath.Join(root, "extra_secret.py"), "API_KEY = \"abcd1234\"\n")
	rec = s.do(t, http.MethodPost, "/api/intel/scan/findings",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("scan/findings status %d: %s", rec.Code, rec.Body.String())
	}
	var scanResp struct {
		Created  int `json:"created"`
		Findings []struct {
			Detector string `json:"detector"`
			RuleID   string `json:"cveOrRuleId"`
			Location string `json:"location"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("scan/findings parse: %v", err)
	}
	if scanResp.Created < 1 {
		t.Errorf("expected at least 1 new finding, got %d", scanResp.Created)
	}
	foundRule := false
	for _, f := range scanResp.Findings {
		if f.Detector == "rule" && strings.Contains(f.Location, "extra_secret.py") {
			foundRule = true
		}
	}
	if !foundRule {
		t.Errorf("new extra_secret.py finding not reported: %+v", scanResp.Findings)
	}

	// sync/scan 是同语义触发别名。
	rec = s.do(t, http.MethodPost, "/api/intel/sync/scan",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync/scan status %d: %s", rec.Code, rec.Body.String())
	}
	var syncResp struct {
		Created int `json:"created"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &syncResp); err != nil {
		t.Fatalf("sync/scan parse: %v", err)
	}

	// detectors 过滤：只跑 security 检测器。
	rec = s.do(t, http.MethodPost, "/api/intel/scan/findings",
		`{"projectId":`+jsonInt(proj.ID)+`,"detectors":["security"]}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("scan/findings security status %d: %s", rec.Code, rec.Body.String())
	}
	var secResp struct {
		Findings []struct {
			Detector string `json:"detector"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &secResp); err != nil {
		t.Fatalf("security scan parse: %v", err)
	}
	for _, f := range secResp.Findings {
		if f.Detector != "security" {
			t.Errorf("detector-filtered scan returned non-security finding: %+v", f)
		}
	}

	// 缺失 projectId → 400。
	rec = s.do(t, http.MethodPost, "/api/intel/sbom", `{}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("sbom without projectId should 400, got %d", rec.Code)
	}
	// GET → 405。
	rec = s.do(t, http.MethodGet, "/api/intel/sbom?projectId="+jsonInt(proj.ID), "", wh)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/intel/sbom should 405, got %d", rec.Code)
	}
}

// TestIntelIssueAckAndLinkFeature verifies the closed-loop issue actions:
// ack marks the issue resolved (status + resolvedAt), link-feature anchors it
// to an existing feature point, and invalid ids get proper 4xx responses.
func TestIntelIssueAckAndLinkFeature(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app_config.py"), "TOKEN = \"xyz\"\n")
	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	// issue 只能由失败 run 生成，测试直接种一行 fixture。
	ctx := context.Background()
	issue := &store.IntelIssue{
		ProjectID:  proj.ID,
		Key:        "demo.UserTest.testLogin",
		Kind:       "bug",
		Severity:   "medium",
		Location:   "src/test/java/demo/UserTest.java:42",
		Status:     "open",
		DetailJSON: "{}",
	}
	if err := s.store.CreateIntelIssue(ctx, issue); err != nil {
		t.Fatalf("seed issue: %v", err)
	}

	// ack → resolved + resolvedAt 落库。
	rec = s.do(t, http.MethodPost, "/api/intel/issues/"+jsonInt(issue.ID)+"/ack", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack status %d: %s", rec.Code, rec.Body.String())
	}
	var ackResp struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ackResp); err != nil {
		t.Fatalf("ack parse: %v", err)
	}
	if !ackResp.OK || ackResp.Status != "resolved" {
		t.Errorf("ack response mismatch: %+v", ackResp)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/issues?projectId="+jsonInt(proj.ID)+"&status=resolved", "", wh)
	var listResp struct {
		Issues []struct {
			ID         int64   `json:"id"`
			Status     string  `json:"status"`
			ResolvedAt *string `json:"resolvedAt"`
			FeatureID  int64   `json:"featureId"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("issues parse: %v", err)
	}
	if len(listResp.Issues) != 1 || listResp.Issues[0].ID != issue.ID {
		t.Fatalf("resolved issue not listed: %+v", listResp.Issues)
	}
	if listResp.Issues[0].Status != "resolved" || listResp.Issues[0].ResolvedAt == nil {
		t.Errorf("issue not marked resolved: %+v", listResp.Issues[0])
	}

	// 新建功能点 → link-feature 挂载。
	rec = s.do(t, http.MethodPost, "/api/intel/features?projectId="+jsonInt(proj.ID),
		`{"name":"orders-service","ends":["GET /orders"],"anchor":""}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create feature status %d: %s", rec.Code, rec.Body.String())
	}
	var featResp struct {
		Feature struct {
			ID int64 `json:"id"`
		} `json:"feature"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &featResp); err != nil || featResp.Feature.ID == 0 {
		t.Fatalf("create feature parse: %s", rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/issues/"+jsonInt(issue.ID)+"/link-feature",
		`{"featureId":`+jsonInt(featResp.Feature.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("link-feature status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/issues?projectId="+jsonInt(proj.ID), "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("issues parse: %v", err)
	}
	if len(listResp.Issues) != 1 || listResp.Issues[0].FeatureID != featResp.Feature.ID {
		t.Errorf("issue featureId not linked: %+v", listResp.Issues)
	}

	// 错误路径：不存在的 issue → 404，不存在的 feature → 400，非法动作 → 400。
	rec = s.do(t, http.MethodPost, "/api/intel/issues/999999/ack", "", wh)
	if rec.Code != http.StatusNotFound {
		t.Errorf("ack missing issue should 404, got %d", rec.Code)
	}
	rec = s.do(t, http.MethodPost, "/api/intel/issues/"+jsonInt(issue.ID)+"/link-feature",
		`{"featureId":999999}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("link-feature missing feature should 400, got %d", rec.Code)
	}
	rec = s.do(t, http.MethodPost, "/api/intel/issues/"+jsonInt(issue.ID)+"/nope", "", wh)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown action should 400, got %d", rec.Code)
	}
	rec = s.do(t, http.MethodGet, "/api/intel/issues/"+jsonInt(issue.ID)+"/ack", "", wh)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET ack should 405, got %d", rec.Code)
	}
}

// TestIntelRunResultsEmpty verifies GET /api/intel/runs/{id}/results returns a
// 200 with an empty list for a run that has no per-case results yet.
func TestIntelRunResultsEmpty(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app_config.py"), "TOKEN = \"xyz\"\n")
	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	// 只种一个 run 行，不执行真实测试命令。
	run := &store.TestRun{ProjectID: proj.ID, Scope: "module", Status: "queued"}
	if err := s.store.CreateIntelTestRun(context.Background(), run); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	rec = s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(run.ID)+"/results", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run results status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("run results parse: %v", err)
	}
	if resp.Results == nil || len(resp.Results) != 0 {
		t.Errorf("expected empty results list, got %v", resp.Results)
	}

	// 原有 run 详情端点不受影响。
	rec = s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(run.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("run detail status %d: %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Run     *store.TestRun `json:"run"`
		Results []any          `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("run detail parse: %v", err)
	}
	if detail.Run == nil || detail.Run.ID != run.ID {
		t.Errorf("run detail mismatch: %+v", detail.Run)
	}
}
