package compliance

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func filterByRule(fs []Finding, id string) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.RuleID == id {
			out = append(out, f)
		}
	}
	return out
}

func assertFindings(t *testing.T, got, want []Finding) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d:\n got: %+v\nwant: %+v", len(got), len(want), got, want)
	}
	for i := range want {
		g := got[i]
		if g.RuleID != want[i].RuleID || g.Severity != want[i].Severity ||
			g.File != want[i].File || g.Line != want[i].Line {
			t.Errorf("finding[%d] = %+v, want %+v", i, g, want[i])
		}
		if g.Message == "" {
			t.Errorf("finding[%d] has empty message", i)
		}
	}
}

func TestScanFile(t *testing.T) {
	cases := []struct {
		name string
		path string
		src  string
		rule string
		want []Finding
	}{
		{
			name: "copyright missing java",
			path: "src/Foo.java",
			src:  "package demo;\n\npublic class Foo {\n}\n",
			rule: "copyright-header",
			want: []Finding{{RuleID: "copyright-header", Severity: "medium", File: "src/Foo.java", Line: 1}},
		},
		{
			name: "copyright missing kotlin",
			path: "src/Foo.kt",
			src:  "package demo\n\nclass Foo\n",
			rule: "copyright-header",
			want: []Finding{{RuleID: "copyright-header", Severity: "medium", File: "src/Foo.kt", Line: 1}},
		},
		{
			name: "copyright present",
			path: "src/Foo.java",
			src:  "/*\n * Copyright(c) 2026 Example. All rights reserved.\n */\npackage demo;\n",
			rule: "copyright-header",
			want: nil,
		},
		{
			name: "copyright not applicable go",
			path: "src/foo.go",
			src:  "package foo\n",
			rule: "copyright-header",
			want: nil,
		},
		{
			name: "javadoc missing class",
			path: "src/Foo.java",
			src:  "package demo;\n\npublic class Foo {\n}\n",
			rule: "missing-javadoc",
			want: []Finding{{RuleID: "missing-javadoc", Severity: "low", File: "src/Foo.java", Line: 3}},
		},
		{
			name: "javadoc present class",
			path: "src/Foo.java",
			src:  "package demo;\n\n/**\n * Foo.\n */\npublic class Foo {\n}\n",
			rule: "missing-javadoc",
			want: nil,
		},
		{
			name: "javadoc missing method",
			path: "src/Foo.java",
			src:  "package demo;\n\npublic class Foo {\n    public void run() {\n    }\n}\n",
			rule: "missing-javadoc",
			want: []Finding{
				{RuleID: "missing-javadoc", Severity: "low", File: "src/Foo.java", Line: 3},
				{RuleID: "missing-javadoc", Severity: "low", File: "src/Foo.java", Line: 4},
			},
		},
		{
			name: "javadoc not applicable kotlin",
			path: "src/Foo.kt",
			src:  "class Foo\n",
			rule: "missing-javadoc",
			want: nil,
		},
		{
			name: "swagger annotation residual",
			path: "src/Foo.java",
			src:  "package demo;\n\n@Api(tags = \"users\")\n@Operation(summary = \"x\")\npublic class Foo {\n    @ApiOperation(\"get\")\n    public void get() {}\n}\n",
			rule: "swagger-annotation",
			want: []Finding{
				{RuleID: "swagger-annotation", Severity: "high", File: "src/Foo.java", Line: 3},
				{RuleID: "swagger-annotation", Severity: "high", File: "src/Foo.java", Line: 4},
				{RuleID: "swagger-annotation", Severity: "high", File: "src/Foo.java", Line: 6},
			},
		},
		{
			name: "swagger none",
			path: "src/Foo.java",
			src:  "package demo;\n\npublic class Foo {\n}\n",
			rule: "swagger-annotation",
			want: nil,
		},
		{
			name: "trailing whitespace",
			path: "src/foo.go",
			src:  "package foo\n\nfunc main() { \n}\n",
			rule: "trailing-whitespace",
			want: []Finding{{RuleID: "trailing-whitespace", Severity: "low", File: "src/foo.go", Line: 3}},
		},
		{
			name: "secret assignment",
			path: "src/config.py",
			src:  "password = \"s3cr3t\"\ntoken = \"abc\"\n",
			rule: "secret-hardcode",
			want: []Finding{
				{RuleID: "secret-hardcode", Severity: "critical", File: "src/config.py", Line: 1},
				{RuleID: "secret-hardcode", Severity: "critical", File: "src/config.py", Line: 2},
			},
		},
		{
			name: "secret private key",
			path: "src/key.pem",
			src:  "-----BEGIN RSA PRIVATE KEY-----\nMIIB...\n-----END RSA PRIVATE KEY-----\n",
			rule: "secret-hardcode",
			want: []Finding{{RuleID: "secret-hardcode", Severity: "critical", File: "src/key.pem", Line: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterByRule(ScanFile(tc.path, strings.Split(tc.src, "\n")), tc.rule)
			assertFindings(t, got, tc.want)
		})
	}
}

func TestFileTooLong(t *testing.T) {
	exact := make([]string, maxFileLines)
	for i := range exact {
		exact[i] = "x = 1"
	}
	if got := filterByRule(ScanFile("big.py", exact), "file-too-long"); len(got) != 0 {
		t.Fatalf("exactly-limit file should not be flagged, got %+v", got)
	}
	over := make([]string, maxFileLines+1)
	for i := range over {
		over[i] = "x = 1"
	}
	got := filterByRule(ScanFile("big.py", over), "file-too-long")
	want := []Finding{{RuleID: "file-too-long", Severity: "info", File: "big.py", Line: 1}}
	assertFindings(t, got, want)
}

func TestRules(t *testing.T) {
	rules := Rules()
	want := map[string]string{
		"copyright-header":    "medium",
		"missing-javadoc":     "low",
		"swagger-annotation":  "high",
		"trailing-whitespace": "low",
		"file-too-long":       "info",
		"secret-hardcode":     "critical",
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d", len(rules), len(want))
	}
	for _, r := range rules {
		if r.Severity != want[r.ID] {
			t.Errorf("rule %s severity = %q, want %q", r.ID, r.Severity, want[r.ID])
		}
		if !r.Enabled {
			t.Errorf("rule %s should be enabled by default", r.ID)
		}
		if r.Check == nil {
			t.Errorf("rule %s has nil Check", r.ID)
		}
	}
}

func TestScanDir(t *testing.T) {
	root := t.TempDir()
	javaSrc := "package demo;\n\n@Api(tags = \"x\")\npublic class Foo {\n    public void run() {\n    }\n}\n"
	files := map[string]string{
		"src/Foo.java":              javaSrc,
		"src/notes.txt":             "@Api ignored\n",
		"node_modules/dep/index.js": "var token='x'\n",
		"build/out/main.py":         "password='x'\n",
		"src/main.go":               "package main\n\nfunc main() {\n}\n",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ScanDir(root)
	if err != nil {
		t.Fatal(err)
	}
	// Only src/Foo.java and src/main.go are scanned source files. Foo.java
	// yields four findings (copyright, swagger, two javadoc); main.go is clean
	// (copyright-header is not applicable to .go files).
	if len(got) != 4 {
		t.Fatalf("got %d findings, want 4:\n%+v", len(got), got)
	}
	wantKeys := map[string]bool{
		"copyright-header:1":   true,
		"swagger-annotation:3": true,
		"missing-javadoc:4":    true,
		"missing-javadoc:5":    true,
	}
	for _, f := range got {
		if f.File != filepath.Join("src", "Foo.java") {
			t.Errorf("unexpected file in finding: %+v", f)
			continue
		}
		key := f.RuleID + ":" + strconv.Itoa(f.Line)
		if !wantKeys[key] {
			t.Errorf("unexpected finding: %+v", f)
		}
	}
	if got[0].Line != 1 || got[1].Line != 3 || got[2].Line != 4 || got[3].Line != 5 {
		t.Errorf("unexpected ordering: %+v", got)
	}
}
