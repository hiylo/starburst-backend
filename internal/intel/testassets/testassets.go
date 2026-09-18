// Package testassets implements deterministic test asset discovery: it walks a
// repository root (or a single module directory) and classifies test files by
// framework and kind so the upper layer can persist them into test_cases.
// Classification is deliberately static and filename/annotation based; richer
// per-case attribution (flaky history, timing) is added later by the runner.
package testassets

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Asset is one discovered test asset. A single Java test file may yield one
// Asset per @Test method; other frameworks yield one Asset per file.
type Asset struct {
	ModuleRelPath string   `json:"moduleRelPath"` // owning module rel path ("" = repo root)
	Kind          string   `json:"kind"`          // unit | integration | api | e2e-ui | android-ui | ios-ui
	Framework     string   `json:"framework"`     // junit | go-test | vitest | jest | playwright | xctest | pytest | gradle
	Class         string   `json:"class"`         // class name (Java/JUnit), empty otherwise
	Method        string   `json:"method"`        // @Test method name, empty when none
	Path          string   `json:"path"`          // file path relative to the repo root (slash-separated)
	Tags          []string `json:"tags"`          // e.g. ["integration"], ["instrumentation"]
}

var (
	reTestMethod   = regexp.MustCompile(`@Test\b`)
	reJavaMethod   = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\s*\([^;{}]*\)\s*(?:throws\s+[\w\.,\s]+)?\s*\{`)
	reJavaTestFile = regexp.MustCompile(`^[A-Za-z0-9_$]+(?:Tests?|IT)\.java$`)
	reAndroidFile  = regexp.MustCompile(`^[A-Za-z0-9_$]+(?:Tests?|IT)\.(?:kt|java)$`)
)

// springTestAnnotations mark a Java test class as an integration test (the
// Spring Boot test slice annotations boot the application context).
var springTestAnnotations = []string{"@SpringBootTest", "@DataJpaTest", "@WebMvcTest"}

// skipNames are directory names excluded from the walk. They contain build
// output, dependencies and VCS metadata that must never surface as test assets.
var skipNames = map[string]bool{
	".git": true, ".svn": true, ".hg": true, ".idea": true,
	"node_modules": true, "target": true, "build": true, ".gradle": true,
	"dist": true, "build_artifacts": true, ".next": true, "coverage": true,
	"vendor": true,
}

// Discover walks root (limited to moduleRelPath when non-empty; "" means the
// whole root) and returns the classified test assets, sorted by path/class/
// method for deterministic output. Generated and vendored directories are
// skipped.
func Discover(root, moduleRelPath string) ([]Asset, error) {
	moduleRelPath = filepath.ToSlash(filepath.Clean(moduleRelPath))
	if moduleRelPath == "." {
		moduleRelPath = ""
	}
	base := root
	if moduleRelPath != "" {
		base = filepath.Join(root, moduleRelPath)
	}
	if fi, err := os.Stat(base); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, nil
	}

	isPlaywright := hasConfig(base, "playwright.config.")
	isVitest := hasConfig(base, "vitest.config.") || hasConfig(base, "vite.config.")

	out := make([]Asset, 0)
	_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipNames[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		out = append(out, classifyFile(path, relSlash, moduleRelPath, isPlaywright, isVitest)...)
		return nil
	})

	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Class != out[j].Class {
			return out[i].Class < out[j].Class
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}

// classifyFile maps a single regular file to zero or more test assets based on
// its relative path and file name. Rules are evaluated in a fixed order so a
// path matching two frameworks resolves deterministically.
func classifyFile(fullPath, relSlash, moduleRelPath string, isPlaywright, isVitest bool) []Asset {
	base := baseName(relSlash)

	// iOS: *Tests.swift → XCTest (UI target when the name contains "UITests").
	if strings.HasSuffix(base, "Tests.swift") {
		kind := "unit"
		if strings.Contains(base, "UITests") {
			kind = "ios-ui"
		}
		return []Asset{newAsset(moduleRelPath, kind, "xctest", trimExt(base), "", relSlash, nil)}
	}

	// Python: test_*.py / *_test.py → pytest.
	if (strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")) ||
		strings.HasSuffix(base, "_test.py") {
		return []Asset{newAsset(moduleRelPath, "unit", "pytest", "", "", relSlash, nil)}
	}

	// Go: *_test.go → go-test, one Asset per top-level function whose name starts
// with Test/Benchmark/Example (class = base name without _test.go, so the
// runner can backfill last_status per case). Files with no such top-level
// funcs still yield one file-level Asset so the file is not lost.
if strings.HasSuffix(base, "_test.go") {
		return classifyGo(fullPath, moduleRelPath, relSlash)
	}

	// Android instrumentation: src/androidTest/** → android-ui (instrumentation).
	if containsDir(relSlash, "src/androidTest") && reAndroidFile.MatchString(base) {
		return []Asset{newAsset(moduleRelPath, "android-ui", "gradle", trimExt(base), "",
			relSlash, []string{"instrumentation"})}
	}

	// Android local unit test: src/test/** Kotlin files → gradle unit.
	if containsDir(relSlash, "src/test") && strings.HasSuffix(base, ".kt") &&
		reAndroidFile.MatchString(base) {
		return []Asset{newAsset(moduleRelPath, "unit", "gradle", trimExt(base), "", relSlash, nil)}
	}

	// Java: src/test/java/** *Test/*Tests/*IT.java → junit (integration when a
	// Spring test slice annotation is present, otherwise unit).
	if containsDir(relSlash, "src/test/java") && reJavaTestFile.MatchString(base) {
		return classifyJava(fullPath, moduleRelPath, relSlash)
	}

	// JS/TS: *.spec.* / *.test.* → vitest/jest (unit) or playwright (e2e-ui).
	if isJSTestName(base) {
		if isPlaywright || (underE2E(relSlash) && isJSSpecName(base)) {
			return []Asset{newAsset(moduleRelPath, "e2e-ui", "playwright", "", "", relSlash, nil)}
		}
		framework := "jest"
		if isVitest {
			framework = "vitest"
		}
		return []Asset{newAsset(moduleRelPath, "unit", framework, "", "", relSlash, nil)}
	}

	return nil
}

// classifyGo parses a *_test.go file and returns one Asset per top-level test
// function (Test*/Benchmark*/Example*), so per-case status backfill matches the
// runner's per-case results. The file base name (without _test.go) is used as
// the class so results keyed "Class.method" resolve deterministically. When the
// file declares no top-level test funcs (helpers only), a single file-level
// Asset is emitted to keep the file visible in the asset list.
func classifyGo(fullPath, moduleRelPath, relSlash string) []Asset {
	base := baseName(relSlash)
	class := strings.TrimSuffix(base, "_test.go")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return []Asset{newAsset(moduleRelPath, "unit", "go-test", class, "", relSlash, nil)}
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fullPath, data, 0)
	if err != nil {
		return []Asset{newAsset(moduleRelPath, "unit", "go-test", class, "", relSlash, nil)}
	}
	names := make([]string, 0)
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		n := fn.Name.Name
		if strings.HasPrefix(n, "Test") || strings.HasPrefix(n, "Benchmark") || strings.HasPrefix(n, "Example") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []Asset{newAsset(moduleRelPath, "unit", "go-test", class, "", relSlash, nil)}
	}
	out := make([]Asset, 0, len(names))
	for _, n := range names {
		out = append(out, newAsset(moduleRelPath, "unit", "go-test", class, n, relSlash, nil))
	}
	return out
}

// classifyJava parses a JUnit test class: one Asset per @Test method (or a
// single Asset with an empty Method when the file has no annotated methods).
func classifyJava(fullPath, moduleRelPath, relSlash string) []Asset {
	base := baseName(relSlash)
	class := trimExt(base)
	lines := readLines(fullPath)

	kind := "unit"
	var tags []string
	if isIntegrationTest(lines) {
		kind = "integration"
		tags = []string{"integration"}
	}

	methods := testMethods(lines)
	if len(methods) == 0 {
		return []Asset{newAsset(moduleRelPath, kind, "junit", class, "", relSlash, tags)}
	}
	out := make([]Asset, 0, len(methods))
	for _, m := range methods {
		out = append(out, newAsset(moduleRelPath, kind, "junit", class, m, relSlash, tags))
	}
	return out
}

// testMethods extracts the method names annotated with @Test, de-duplicated and
// in source order. The method signature is expected within a few lines of the
// annotation (the common JUnit layout).
func testMethods(lines []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0)
	for i, l := range lines {
		if !reTestMethod.MatchString(l) {
			continue
		}
		for j := i; j < len(lines) && j <= i+4; j++ {
			if m := reJavaMethod.FindStringSubmatch(lines[j]); m != nil {
				if !seen[m[1]] {
					seen[m[1]] = true
					out = append(out, m[1])
				}
				break
			}
		}
	}
	return out
}

func isIntegrationTest(lines []string) bool {
	for _, l := range lines {
		for _, a := range springTestAnnotations {
			if strings.Contains(l, a) {
				return true
			}
		}
	}
	return false
}

func isJSTestName(base string) bool {
	for _, suf := range []string{".spec.ts", ".spec.js", ".test.ts", ".test.js"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

func isJSSpecName(base string) bool {
	return strings.HasSuffix(base, ".spec.ts") || strings.HasSuffix(base, ".spec.js")
}

// underE2E reports whether the path lives under a tests/ or e2e/ directory,
// which marks Playwright-style browser test directories.
func underE2E(relSlash string) bool {
	for _, part := range strings.Split(relSlash, "/") {
		if part == "tests" || part == "e2e" {
			return true
		}
	}
	return false
}

func newAsset(moduleRelPath, kind, framework, class, method, path string, tags []string) Asset {
	return Asset{
		ModuleRelPath: moduleRelPath,
		Kind:          kind,
		Framework:     framework,
		Class:         class,
		Method:        method,
		Path:          path,
		Tags:          tags,
	}
}

// containsDir reports whether relSlash contains a directory segment equal to
// dir, matching at any depth (including the repository root itself).
func containsDir(relSlash, dir string) bool {
	return strings.Contains("/"+relSlash+"/", "/"+dir+"/")
}

func baseName(relSlash string) string {
	if i := strings.LastIndex(relSlash, "/"); i >= 0 {
		return relSlash[i+1:]
	}
	return relSlash
}

func trimExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

func hasConfig(dir, prefix string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			return true
		}
	}
	return false
}

func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
