package bff_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hiylo/starburst-backend/internal/intel"
)

// writeTree writes content into dir/rel, creating parent directories.
func writeTree(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The ScanModule dispatch tests live in an external test package (bff_test) so
// the intel.ScanModule entry point can be exercised without an import cycle.

func TestScanModuleDispatchBFF(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "package.json", `{"dependencies":{"express":"4.18.0"}}`)
	writeTree(t, root, "server.js", `const app = require('express')();
app.get('/api/health', (req, res) => {});
app.post('/api/users', (req, res) => {});
`)
	sum, err := intel.ScanModule(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, ep := range sum.Endpoints {
		got[ep.Method+" "+ep.Path] = true
	}
	if !got["GET /api/health"] || !got["POST /api/users"] {
		t.Errorf("bff REST endpoints missing after dispatch, got %v", got)
	}
}

func TestScanModuleDispatchWeb(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, "package.json", `{"dependencies":{"vue":"3.0.0"}}`)
	writeTree(t, root, "api.ts", `import axios from 'axios';
axios.get('/api/banners');
`)
	sum, err := intel.ScanModule(root, ".")
	if err != nil {
		t.Fatal(err)
	}
	// A pure Web frontend must not produce backend entities...
	if len(sum.Entities) != 0 {
		t.Errorf("web module should yield no entities, got %+v", sum.Entities)
	}
	// ...and must still yield the axios binding endpoints (no regression).
	found := false
	for _, ep := range sum.Endpoints {
		if ep.Method == "GET" && ep.Path == "/api/banners" {
			found = true
		}
	}
	if !found {
		t.Errorf("web module should still yield axios binding endpoints, got %+v", sum.Endpoints)
	}
}
