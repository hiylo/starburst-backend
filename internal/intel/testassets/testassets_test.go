package testassets

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeTree writes a set of slash-relative files under root for discovery tests.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// mixedRepo builds a repository covering every classification rule plus
// directories that must be skipped.
func mixedRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/test/java/demo/UserServiceTest.java": `package demo;
public class UserServiceTest {
    @Test
    public void testFind() {
    }
    @Test
    public void testCreate() {
    }
}`,
		"src/test/java/demo/OrderIT.java": `package demo;
@SpringBootTest
public class OrderIT {
    @Test
    public void testOrder() {
    }
}`,
		"pkg/user_test.go": "package pkg",
		"src/util.spec.ts": `describe("util", () => {});`,
		"e2e/home.spec.ts": `test("home", async () => {});`,
		"app/src/androidTest/java/ExampleInstrumentedTest.kt": "class ExampleInstrumentedTest",
		"app/src/test/java/ExampleUnitTest.kt":                "class ExampleUnitTest",
		"AppTests/LoginTests.swift":                           "import XCTest",
		"AppTests/LoginUITests.swift":                         "import XCTest",
		"tests/test_api.py":                                   "def test_api(): pass",
		"src/test/java/demo/TestHelper.java":                  "public class TestHelper {}",
		"node_modules/pkg/x.spec.ts":                          "ignored",
		"target/classes/YTest.java":                           "ignored",
	})
	return root
}

func TestDiscoverClassifies(t *testing.T) {
	assets, err := Discover(mixedRepo(t), "")
	if err != nil {
		t.Fatal(err)
	}

	byKey := map[string]Asset{}
	for _, a := range assets {
		key := a.Path
		if a.Method != "" {
			key += "#" + a.Method
		}
		byKey[key] = a
	}

	cases := []struct {
		key       string
		module    string
		kind      string
		framework string
		class     string
		method    string
		tags      []string
	}{
		{"src/test/java/demo/UserServiceTest.java#testFind", "", "unit", "junit", "UserServiceTest", "testFind", nil},
		{"src/test/java/demo/UserServiceTest.java#testCreate", "", "unit", "junit", "UserServiceTest", "testCreate", nil},
		{"src/test/java/demo/OrderIT.java#testOrder", "", "integration", "junit", "OrderIT", "testOrder", []string{"integration"}},
		{"pkg/user_test.go", "", "unit", "go-test", "", "", nil},
		{"src/util.spec.ts", "", "unit", "jest", "", "", nil},
		{"e2e/home.spec.ts", "", "e2e-ui", "playwright", "", "", nil},
		{"app/src/androidTest/java/ExampleInstrumentedTest.kt", "", "android-ui", "gradle", "ExampleInstrumentedTest", "", []string{"instrumentation"}},
		{"app/src/test/java/ExampleUnitTest.kt", "", "unit", "gradle", "ExampleUnitTest", "", nil},
		{"AppTests/LoginTests.swift", "", "unit", "xctest", "LoginTests", "", nil},
		{"AppTests/LoginUITests.swift", "", "ios-ui", "xctest", "LoginUITests", "", nil},
		{"tests/test_api.py", "", "unit", "pytest", "", "", nil},
	}

	for _, c := range cases {
		got, ok := byKey[c.key]
		if !ok {
			t.Errorf("missing asset %q (have %d assets)", c.key, len(assets))
			continue
		}
		if got.ModuleRelPath != c.module {
			t.Errorf("%s moduleRelPath = %q, want %q", c.key, got.ModuleRelPath, c.module)
		}
		if got.Kind != c.kind {
			t.Errorf("%s kind = %q, want %q", c.key, got.Kind, c.kind)
		}
		if got.Framework != c.framework {
			t.Errorf("%s framework = %q, want %q", c.key, got.Framework, c.framework)
		}
		if got.Class != c.class {
			t.Errorf("%s class = %q, want %q", c.key, got.Class, c.class)
		}
		if got.Method != c.method {
			t.Errorf("%s method = %q, want %q", c.key, got.Method, c.method)
		}
		if !reflect.DeepEqual(got.Tags, c.tags) {
			t.Errorf("%s tags = %v, want %v", c.key, got.Tags, c.tags)
		}
	}

	if len(assets) != len(cases) {
		t.Errorf("asset count = %d, want %d (%+v)", len(assets), len(cases), assets)
	}
}

func TestDiscoverPlaywrightConfig(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"playwright.config.ts": `export default {};`,
		"src/login.spec.ts":    `test("login", async () => {});`,
	})
	assets, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 {
		t.Fatalf("assets = %d, want 1 (%+v)", len(assets), assets)
	}
	if assets[0].Framework != "playwright" || assets[0].Kind != "e2e-ui" {
		t.Errorf("asset = %+v, want playwright e2e-ui", assets[0])
	}
}

func TestDiscoverVitestConfig(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"vitest.config.ts": `export default {};`,
		"src/x.test.ts":    `it("x", () => {});`,
	})
	assets, err := Discover(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 {
		t.Fatalf("assets = %d, want 1 (%+v)", len(assets), assets)
	}
	if assets[0].Framework != "vitest" || assets[0].Kind != "unit" {
		t.Errorf("asset = %+v, want vitest unit", assets[0])
	}
}

func TestDiscoverModuleFilter(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/src/test/java/ExampleUnitTest.kt":     "class ExampleUnitTest",
		"app/src/androidTest/java/ExampleTest.kt":  "class ExampleTest",
		"backend/src/test/java/demo/DemoTest.java": `public class DemoTest { @Test public void a() {} }`,
	})
	assets, err := Discover(root, "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 2 {
		t.Fatalf("assets = %d, want 2 (%+v)", len(assets), assets)
	}
	for _, a := range assets {
		if a.ModuleRelPath != "app" {
			t.Errorf("moduleRelPath = %q, want app", a.ModuleRelPath)
		}
		if !stringsHasPrefix(a.Path, "app/") {
			t.Errorf("path = %q, want under app/", a.Path)
		}
	}
}

func TestDiscoverMissingDir(t *testing.T) {
	root := t.TempDir()
	if _, err := Discover(root, "does/not/exist"); err == nil {
		t.Fatal("expected error for missing module dir, got nil")
	}
}

func stringsHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
