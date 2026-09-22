package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// newWorkflowAuth 复用登录+取 token 的标准流程，返回带鉴权的请求头。
func newWorkflowAuth(t *testing.T, s *Server) map[string]string {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/api/web/session", `{"password":"S3cureAdmin!"}`, nil)
	var login struct {
		Session string `json:"session"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)
	wh := map[string]string{"X-Web-Session": login.Session}
	rec = s.do(t, http.MethodPost, "/api/tokens", `{"name":"wf"}`, wh)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tok)
	return map[string]string{"Authorization": "Bearer " + tok.Token}
}

// TestWorkflowDoubleSlashCancelRejected 验证 `/api/workflow//cancel`（双斜杠）
// 返回 400，且不会把 workflow_id 为空串的独立任务误取消。
func TestWorkflowDoubleSlashCancelRejected(t *testing.T) {
	s := newTestServer(t)
	th := newWorkflowAuth(t, s)

	// 建一个正常的工作流（获得真实 workflowId）。
	rec := s.do(t, http.MethodPost, "/api/workflow",
		`{"name":"wf","directory":"/a","steps":[{"prompt":"step1"}]}`, th)
	if rec.Code != http.StatusOK {
		t.Fatalf("create workflow: %d %s", rec.Code, rec.Body.String())
	}
	var wf struct {
		WorkflowID string `json:"workflowId"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &wf)
	if wf.WorkflowID == "" {
		t.Fatalf("workflow create missing workflowId: %s", rec.Body.String())
	}

	// 再建一个独立任务（workflow_id 为空串）。
	rec = s.do(t, http.MethodPost, "/api/tasks", `{"prompt":"standalone"}`, th)
	var ind struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ind)
	if ind.ID == "" {
		t.Fatalf("independent task missing id: %s", rec.Body.String())
	}

	// 双斜杠 cancel 必须被拒：Go 的 http.ServeMux 会把含 `//` 的路径自动清净化并
	// 307 重定向（handler 根本不会被调用），即便绕过 mux 直连 handler，判空守卫
	// 也会 400。两种形态都算「被拒」，关键是独立任务绝不能被动到。
	rec = s.do(t, http.MethodPost, "/api/workflow//cancel", "", th)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("double-slash cancel = %d, want 400 or 307: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/tasks/"+ind.ID, "", th)
	var got struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != "queued" {
		t.Fatalf("independent task status = %q, want queued (must NOT be canceled)", got.Status)
	}

	// 单斜杠正常 cancel 真实工作流仍可用。
	rec = s.do(t, http.MethodPost, "/api/workflow/"+wf.WorkflowID+"/cancel", "", th)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid cancel = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// 双斜杠 rerun 同样必须被拒。
	rec = s.do(t, http.MethodPost, "/api/workflow//rerun", "", th)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("double-slash rerun = %d, want 400 or 307: %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowCreateRejectsTooManySteps 验证超过 maxWorkflowSteps 的 steps 被 400 拒绝。
func TestWorkflowCreateRejectsTooManySteps(t *testing.T) {
	s := newTestServer(t)
	th := newWorkflowAuth(t, s)

	var sb strings.Builder
	sb.WriteString(`{"name":"big","directory":"/a","steps":[`)
	for i := 0; i < maxWorkflowSteps+1; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(fmt.Sprintf(`{"prompt":"step%d"}`, i))
	}
	sb.WriteString(`]}`)

	rec := s.do(t, http.MethodPost, "/api/workflow", sb.String(), th)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("too-many-steps = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}
