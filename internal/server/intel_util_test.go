package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSimpleTreeHashDetectsContentChange verifies the staleness snapshot hash
// changes when a file's content changes, not only when its name/set changes
// (regression: the previous path-only hash missed local edits, so the
// source-change auto-analyze never fired for 非 git 本地项目).
func TestSimpleTreeHashDetectsContentChange(t *testing.T) {
	root := t.TempDir()
	mk := func(name, content string) {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mk("UserController.java", "package demo;\nclass UserController {}\n")
	h1, err := simpleTreeHash(root)
	if err != nil {
		t.Fatalf("first hash: %v", err)
	}

	// 内容变化（同路径）→ hash 必须变化。
	mk("UserController.java", "package demo;\nclass UserController { /* 修复版权头 */ }\n")
	h2, err := simpleTreeHash(root)
	if err != nil {
		t.Fatalf("second hash: %v", err)
	}
	if h1 == h2 {
		t.Fatalf("content change must change the snapshot hash: %s == %s", h1, h2)
	}

	// 无变化 → hash 稳定。
	h3, _ := simpleTreeHash(root)
	if h3 != h2 {
		t.Fatalf("stable tree must hash equal: %s vs %s", h3, h2)
	}
}

// TestSimpleTreeHashIgnoresBuildOutput verifies .git/target/node_modules are
// excluded so tooling churn does not invalidate the snapshot.
func TestSimpleTreeHashIgnoresBuildOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src/main"), 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(name, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mk("src/main/A.java", "a")
	mk("target/classes/A.class", "churn")
	mk(".git/HEAD", "ref: refs/heads/main")
	h1, _ := simpleTreeHash(root)

	mk("target/classes/A.class", "rebuilt")
	h2, _ := simpleTreeHash(root)
	if h1 != h2 {
		t.Fatalf("build output churn must not change snapshot: %s vs %s", h1, h2)
	}
}

// TestIntelAutoAnalyzeDebounce verifies the same project is only allowed to
// auto-trigger analysis once per interval.
func TestIntelAutoAnalyzeDebounce(t *testing.T) {
	s := &Server{intelAutoLast: make(map[int64]time.Time)}
	if !s.isIntelAutoAllowed(1) {
		t.Fatal("first trigger should be allowed")
	}
	if s.isIntelAutoAllowed(1) {
		t.Fatal("second immediate trigger should be debounced")
	}
	if !s.isIntelAutoAllowed(2) {
		t.Fatal("a different project should be allowed independently")
	}
}