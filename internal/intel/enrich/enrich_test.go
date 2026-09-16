package enrich

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocsGathersAndBounds(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "README.md", "hello world")
	writeFile(t, root, "docs/arch.md", "architecture notes")
	writeFile(t, root, "docs/skip/node_modules/x.md", "should be skipped")
	writeFile(t, root, "big.md", strings.Repeat("a", 5000))

	docs := Docs(root, 100)
	if !strings.Contains(docs, "hello world") {
		t.Fatalf("docs should contain README content, got: %q", docs)
	}
	if strings.Contains(docs, "should be skipped") {
		t.Fatalf("node_modules md must be skipped")
	}
	// 100-byte bound keeps output small despite a 5KB file present.
	if len(docs) > 200 {
		t.Fatalf("docs length %d exceeds a small bound", len(docs))
	}
}

func TestUserPrompt(t *testing.T) {
	p := UserPrompt("## 文档\n内容", []EndpointHint{{Method: "GET", Path: "/activity/list", ResponseType: "Page"}})
	if !strings.Contains(p, "GET /activity/list -> Page") {
		t.Fatalf("prompt should list endpoint, got: %q", p)
	}
	if !strings.Contains(p, "内容") {
		t.Fatalf("prompt should embed docs, got: %q", p)
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
