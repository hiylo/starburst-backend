package fix

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApplyDryRun(t *testing.T) {
	cases := []struct {
		name    string
		content string
		s       *Suggestion
		want    string
		wantErr bool
	}{
		{
			name:    "simple replace",
			content: "hello world\n",
			s:       &Suggestion{File: "a.txt", OldText: "world", NewText: "there", Line: 1},
			want:    "hello there\n",
		},
		{
			name:    "multi-line replace",
			content: "one\ntwo\nthree\n",
			s:       &Suggestion{File: "a.txt", OldText: "two\n", NewText: "TWO\n", Line: 2},
			want:    "one\nTWO\nthree\n",
		},
		{
			name:    "deletion",
			content: "one\ntwo\nthree\n",
			s:       &Suggestion{File: "a.txt", OldText: "two\n", NewText: "", Line: 2},
			want:    "one\nthree\n",
		},
		{
			name:    "insertion",
			content: "one\nthree\n",
			s:       &Suggestion{File: "a.txt", OldText: "", NewText: "two\n", Line: 2},
			want:    "one\ntwo\nthree\n",
		},
		{
			name:    "not found",
			content: "hello world\n",
			s:       &Suggestion{File: "a.txt", OldText: "nope", NewText: "x", Line: 1},
			wantErr: true,
		},
		{
			name:    "ambiguous",
			content: "a a a\n",
			s:       &Suggestion{File: "a.txt", OldText: "a", NewText: "b", Line: 1},
			wantErr: true,
		},
		{
			name:    "nil suggestion",
			content: "x\n",
			s:       nil,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyDryRun(tc.content, tc.s)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ApplyDryRun() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyDryRun() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Fatalf("ApplyDryRun() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBackupPlan(t *testing.T) {
	cases := []struct {
		name  string
		suggs []*Suggestion
		want  []string
	}{
		{
			name:  "dedup and sort",
			suggs: []*Suggestion{{File: "b.txt"}, {File: "a.txt"}, {File: "b.txt"}, {File: "c.txt"}},
			want:  []string{"a.txt", "b.txt", "c.txt"},
		},
		{
			name:  "nil and empty skipped",
			suggs: []*Suggestion{{File: "x.txt"}, nil, {File: ""}, {File: "x.txt"}},
			want:  []string{"x.txt"},
		},
		{
			name:  "empty",
			suggs: []*Suggestion{},
			want:  []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BackupPlan(tc.suggs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BackupPlan() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGeneratePatch(t *testing.T) {
	cases := []struct {
		name      string
		content   map[string]string
		suggs     []*Suggestion
		wantErr   bool
		wantParts []string
		notWant   []string
	}{
		{
			name:    "single suggestion",
			content: map[string]string{"a.txt": "one\ntwo\nthree\n"},
			suggs:   []*Suggestion{{File: "a.txt", OldText: "two\n", NewText: "TWO\n", Line: 2, Confidence: "high"}},
			wantParts: []string{
				"diff --git a/a.txt b/a.txt",
				"--- a/a.txt",
				"+++ b/a.txt",
				"@@ -1,3 +1,3 @@",
				" one",
				"-two",
				"+TWO",
				" three",
			},
		},
		{
			name:    "multiple suggestions same file merged",
			content: map[string]string{"a.txt": "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"},
			suggs: []*Suggestion{
				{File: "a.txt", OldText: "b\n", NewText: "B\n", Line: 2},
				{File: "a.txt", OldText: "i\n", NewText: "I\n", Line: 9},
			},
			wantParts: []string{
				"diff --git a/a.txt b/a.txt",
				"@@ -1,5 +1,5 @@",
				"-b",
				"+B",
				"@@ -6,5 +6,5 @@",
				"-i",
				"+I",
			},
		},
		{
			name:    "multiple files merged sorted",
			content: map[string]string{"z.txt": "z1\n", "a.txt": "a1\n"},
			suggs: []*Suggestion{
				{File: "z.txt", OldText: "z1\n", NewText: "Z1\n", Line: 1},
				{File: "a.txt", OldText: "a1\n", NewText: "A1\n", Line: 1},
			},
			wantParts: []string{
				"diff --git a/a.txt b/a.txt",
				"diff --git a/z.txt b/z.txt",
			},
		},
		{
			name:    "missing content errors",
			content: map[string]string{},
			suggs:   []*Suggestion{{File: "a.txt", OldText: "x", NewText: "y", Line: 1}},
			wantErr: true,
		},
		{
			name:    "old text not found errors",
			content: map[string]string{"a.txt": "hello\n"},
			suggs:   []*Suggestion{{File: "a.txt", OldText: "nope", NewText: "y", Line: 1}},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := GeneratePatch(tc.content, tc.suggs)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("GeneratePatch() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("GeneratePatch() error = %v, want nil", err)
			}
			for _, p := range tc.wantParts {
				if !strings.Contains(got, p) {
					t.Fatalf("GeneratePatch() missing %q in:\n%s", p, got)
				}
			}
			for _, p := range tc.notWant {
				if strings.Contains(got, p) {
					t.Fatalf("GeneratePatch() should not contain %q in:\n%s", p, got)
				}
			}
		})
	}
}

func TestGeneratePatchGitApply(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	cases := []struct {
		name    string
		content map[string]string
		suggs   []*Suggestion
	}{
		{
			name:    "single line replace",
			content: map[string]string{"main.go": "package p\n\nfunc main() {\n\tx := 1\n\t_ = x\n}\n"},
			suggs:   []*Suggestion{{File: "main.go", OldText: "x := 1", NewText: "x := 2", Line: 4}},
		},
		{
			name:    "multi-line replace",
			content: map[string]string{"f.txt": "a\nb\nc\nd\ne\nf\ng\n"},
			suggs:   []*Suggestion{{File: "f.txt", OldText: "c\nd\ne\n", NewText: "C\nD\nE\nF\n", Line: 3}},
		},
		{
			name:    "two far apart suggestions",
			content: map[string]string{"f.txt": "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nm\nn\n"},
			suggs: []*Suggestion{
				{File: "f.txt", OldText: "b\n", NewText: "B\n", Line: 2},
				{File: "f.txt", OldText: "m\n", NewText: "M\n", Line: 13},
			},
		},
		{
			name:    "two close suggestions merged",
			content: map[string]string{"f.txt": "a\nb\nc\nd\ne\nf\ng\n"},
			suggs: []*Suggestion{
				{File: "f.txt", OldText: "b\n", NewText: "B\n", Line: 2},
				{File: "f.txt", OldText: "d\n", NewText: "D\n", Line: 4},
			},
		},
		{
			name:    "deletion",
			content: map[string]string{"f.txt": "a\nb\nc\nd\ne\n"},
			suggs:   []*Suggestion{{File: "f.txt", OldText: "c\n", NewText: "", Line: 3}},
		},
		{
			name: "multi-file",
			content: map[string]string{
				"x/a.txt": "1\n2\n3\n",
				"x/b.txt": "foo\nbar\nbaz\n",
			},
			suggs: []*Suggestion{
				{File: "x/a.txt", OldText: "2\n", NewText: "TWO\n", Line: 2},
				{File: "x/b.txt", OldText: "bar\n", NewText: "BAR\n", Line: 2},
			},
		},
		{
			name:    "no trailing newline",
			content: map[string]string{"f.txt": "a\nb\nc"},
			suggs:   []*Suggestion{{File: "f.txt", OldText: "b", NewText: "B", Line: 2}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for rel, content := range tc.content {
				p := filepath.Join(dir, rel)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			patch, err := GeneratePatch(tc.content, tc.suggs)
			if err != nil {
				t.Fatalf("GeneratePatch() error = %v", err)
			}
			applyCheck(t, dir, patch)
		})
	}
}

// applyCheck verifies a patch applies cleanly with `git apply --check`.
func applyCheck(t *testing.T, dir, patch string) {
	t.Helper()
	p := filepath.Join(dir, "fix.patch")
	if err := os.WriteFile(p, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", "--check", "fix.patch")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply --check failed: %v\n%s\npatch:\n%s", err, out, patch)
	}
}
