// Shared test helpers for the test intelligence (intel) suite.
//
// The intel HTTP/API tests used to live in this single file; they are now split
// per subdomain across intel_projects_test.go, intel_analyze_test.go,
// intel_inventory_test.go, intel_exec_test.go, intel_env_test.go,
// intel_contract_test.go, intel_feature_test.go and intel_audit_test.go.
// Everything still declared here is used by two or more of those files, so it
// stays in one place.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// waitIntelRunFinished polls a run until it leaves "queued"/"running" and
// returns the final status. Async runs execute in the background, so tests that
// exercise /api/intel/run must wait for the terminal state.
func waitIntelRunFinished(t *testing.T, s *Server, wh map[string]string, runID int64) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	status := ""
	for time.Now().Before(deadline) {
		rec := s.do(t, http.MethodGet, "/api/intel/runs/"+jsonInt(runID), "", wh)
		if rec.Code == http.StatusOK {
			var resp struct {
				Run struct {
					Status string `json:"status"`
				} `json:"run"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			status = resp.Run.Status
			if status == "passed" || status == "failed" || status == "error" {
				return status
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %d not finished within 20s (last status %q)", runID, status)
	return status
}

// waitIntelCondition polls until fn returns true or the deadline elapses.
func waitIntelCondition(t *testing.T, fn func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within 20s: %s", msg)
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
