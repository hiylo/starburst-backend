package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelFixApplyAndRollback verifies the fix workflow end-to-end: a proposed
// fix applies its edit into the project working tree with a backup, then a
// one-click rollback restores the original content.
func TestIntelFixApplyAndRollback(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.properties"), "host=127.0.0.1\npassword=plain\n")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	fx := &store.IntelFix{
		ProjectID: proj.ID,
		Kind:      "ai-suggest",
		Title:     "mask password",
		DiffJSON:  `[{"file":"app.properties","oldText":"password=plain","newText":"password=*****","line":2,"confidence":"high"}]`,
		Status:    "proposed",
	}
	ctx := context.Background()
	if err := s.store.CreateIntelFix(ctx, fx); err != nil {
		t.Fatalf("create fix: %v", err)
	}

	// Apply -> file updated.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/apply", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("apply status %d: %s", rec.Code, rec.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(root, "app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=*****") {
		t.Errorf("after apply file has: %q", string(data))
	}
	if strings.Contains(string(data), "password=plain") {
		t.Errorf("after apply old password still present: %q", string(data))
	}

	// Rollback -> restored.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/rollback", "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("rollback status %d: %s", rec.Code, rec.Body.String())
	}
	data, err = os.ReadFile(filepath.Join(root, "app.properties"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "password=plain") {
		t.Errorf("after rollback file has: %q", string(data))
	}

	// Invalid state: applying a rolled-back fix is rejected.
	rec = s.do(t, http.MethodPost, "/api/intel/fixes/"+jsonInt(fx.ID)+"/apply", "", wh)
	if rec.Code == 200 {
		t.Errorf("apply after rollback should be rejected")
	}
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
