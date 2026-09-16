package delta

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestClassifyFile(t *testing.T) {
	cases := []struct {
		name string
		path string
		want Class
	}{
		{"maven pom", "pom.xml", ClassBuild},
		{"gradle groovy", "build.gradle", ClassBuild},
		{"gradle kotlin", "build.gradle.kts", ClassBuild},
		{"go mod", "go.mod", ClassBuild},
		{"go sum", "go.sum", ClassBuild},
		{"npm lock", "package-lock.json", ClassBuild},
		{"yarn lock", "yarn.lock", ClassBuild},
		{"dockerfile", "Dockerfile", ClassBuild},
		{"dockerfile variant", "Dockerfile.prod", ClassBuild},
		{"docker compose", "docker-compose.yml", ClassBuild},
		{"gitlab ci", ".gitlab-ci.yml", ClassBuild},
		{"github workflow", ".github/workflows/ci.yml", ClassBuild},
		{"gradle wrapper", "gradle/wrapper/gradle-wrapper.properties", ClassBuild},
		{"application yml", "application.yml", ClassConfig},
		{"application yaml", "src/main/resources/application.yaml", ClassConfig},
		{"application properties", "application-dev.properties", ClassConfig},
		{"bootstrap yml", "bootstrap.yml", ClassConfig},
		{"flyway migration", "db/migration/V1__init.sql", ClassConfig},
		{"plain sql", "scripts/data.sql", ClassConfig},
		{"go test", "foo_test.go", ClassTest},
		{"java test", "src/test/java/FooTest.java", ClassTest},
		{"java tests", "FooTests.java", ClassTest},
		{"ts spec", "Foo.spec.ts", ClassTest},
		{"ts test", "Foo.test.tsx", ClassTest},
		{"python test", "test_user.py", ClassTest},
		{"test tree helper", "src/test/java/helper/TestUtil.java", ClassTest},
		{"java source", "src/main/java/Foo.java", ClassSource},
		{"controller", "src/main/java/demo/UserController.java", ClassSource},
		{"go source", "main.go", ClassSource},
		{"kotlin source", "app/src/main/kotlin/Foo.kt", ClassSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFile(tc.path); got != tc.want {
				t.Fatalf("ClassifyFile(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestComputeImpact(t *testing.T) {
	cases := []struct {
		name    string
		changed []string
		want    Impact
	}{
		{
			name:    "empty",
			changed: []string{},
			want: Impact{
				Files:           []string{},
				WidenedModules:  []string{},
				AffectedTests:   []string{},
				AffectedSources: []string{},
				ChangedTests:    []string{},
			},
		},
		{
			name:    "single module build plus source and test",
			changed: []string{"pom.xml", "src/main/java/Foo.java", "src/test/java/FooTest.java"},
			want: Impact{
				Files:           []string{"pom.xml", "src/main/java/Foo.java", "src/test/java/FooTest.java"},
				FullRescan:      true,
				WidenedModules:  []string{"."},
				AffectedTests:   []string{"src/test/java/FooTest.java"},
				AffectedSources: []string{"src/main/java/Foo.java"},
				ChangedTests:    []string{"src/test/java/FooTest.java"},
			},
		},
		{
			name: "multi module builds and config",
			changed: []string{
				"services/order/pom.xml",
				"services/order/src/main/resources/application.yml",
				"clients/android/build.gradle.kts",
				"clients/android/app/src/main/kotlin/Foo.kt",
			},
			want: Impact{
				Files: []string{
					"clients/android/app/src/main/kotlin/Foo.kt",
					"clients/android/build.gradle.kts",
					"services/order/pom.xml",
					"services/order/src/main/resources/application.yml",
				},
				FullRescan: true,
				WidenedModules: []string{
					"clients/android",
					"services/order",
					"services/order/src/main/resources",
				},
				AffectedTests:   []string{},
				AffectedSources: []string{"clients/android/app/src/main/kotlin/Foo.kt"},
				ChangedTests:    []string{},
			},
		},
		{
			name:    "source only no full rescan",
			changed: []string{"src/main/java/OrderService.java", "internal/x/y.go"},
			want: Impact{
				Files:           []string{"internal/x/y.go", "src/main/java/OrderService.java"},
				FullRescan:      false,
				WidenedModules:  []string{},
				AffectedTests:   []string{},
				AffectedSources: []string{"internal/x/y.go", "src/main/java/OrderService.java"},
				ChangedTests:    []string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeImpact(tc.changed)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ComputeImpact(%v) =\n%+v\nwant\n%+v", tc.changed, got, tc.want)
			}
		})
	}
}

func TestBuildModule(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pom.xml":                `<project></project>`,
		"services/order/pom.xml": `<project></project>`,
		"services/order/src/main/resources/application.yml": "spring: {}",
	})
	cases := []struct {
		name string
		path string
		want string
	}{
		{"root pom", "pom.xml", "."},
		{"nested pom", "services/order/pom.xml", "services/order"},
		{"config resolves to module", "services/order/src/main/resources/application.yml", "services/order"},
		{"source resolves to module", "services/order/src/main/java/X.java", "services/order"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildModule(root, tc.path); got != tc.want {
				t.Fatalf("BuildModule(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	empty := t.TempDir()
	if got := BuildModule(empty, "a/b/c.txt"); got != "" {
		t.Fatalf("BuildModule with no build anchor = %q, want empty", got)
	}
}

func TestDiffFilesAndIsAncestor(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available in PATH")
	}
	_ = gitPath
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")
	writeFile(t, root, "a.txt", "1")
	git("add", "a.txt")
	git("commit", "-q", "-m", "first")
	first := git("rev-parse", "HEAD")
	writeFile(t, root, "b.txt", "2")
	git("add", "b.txt")
	git("commit", "-q", "-m", "second")

	files, err := DiffFiles(root, first)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	if want := []string{"b.txt"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("DiffFiles = %v, want %v", files, want)
	}

	if !IsAncestor(root, first, "HEAD") {
		t.Fatal("IsAncestor(first, HEAD) = false, want true")
	}
	if IsAncestor(root, "HEAD", first) {
		t.Fatal("IsAncestor(HEAD, first) = true, want false")
	}

	empty, err := DiffFiles(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if empty != nil {
		t.Fatalf("DiffFiles with empty lastSHA = %v, want nil", empty)
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeFile(t, root, rel, content)
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
