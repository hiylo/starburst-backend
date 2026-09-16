// Package delta implements the Git-delta impact analysis of the Test
// Intelligence subsystem. Given the changed file set between the last tested
// commit and HEAD, it classifies every changed file (build / config / test /
// source) and aggregates an Impact that decides whether a module must widen to a
// full rescan or can stay incremental. It is deterministic-first and mostly
// pure; only DiffFiles and IsAncestor shell out to git.
package delta

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Class is the impact classification of a single changed file.
type Class string

// Change file impact classes.
const (
	ClassBuild  Class = "build"  // build/dependency/infra file, widens its module to a full rescan
	ClassConfig Class = "config" // config/resource/migration file, affects module integration + contract checks
	ClassTest   Class = "test"   // a test file, added to the affected scope directly
	ClassSource Class = "source" // business source, contract reverse lookup by source_file upstream
)

// buildFiles are build/dependency files recognized by base name. A change here
// cannot be localized, so the owning module widens to a full rescan.
var buildFiles = map[string]struct{}{
	"pom.xml":             {},
	"build.gradle":        {},
	"build.gradle.kts":    {},
	"settings.gradle":     {},
	"settings.gradle.kts": {},
	"go.mod":              {},
	"go.sum":              {},
	"package.json":        {},
	"package-lock.json":   {},
	"yarn.lock":           {},
	"pnpm-lock.yaml":      {},
	"npm-shrinkwrap.json": {},
	"docker-compose.yml":  {},
	"docker-compose.yaml": {},
	".dockerignore":       {},
	"gradlew":             {},
	"gradlew.bat":         {},
	"Jenkinsfile":         {},
	".gitlab-ci.yml":      {},
	".travis.yml":         {},
}

// buildDirPrefixes are directory prefixes that mark build/infra content (CI
// workflows, gradle wrapper) regardless of base name.
var buildDirPrefixes = []string{
	".github/workflows/",
	".circleci/",
	"gradle/wrapper/",
}

// testSuffixes are file name suffixes that mark a test file.
var testSuffixes = []string{
	"_test.go",
	"Test.java", "Tests.java", "TestCase.java",
	"Test.kt", "Tests.kt",
	"Test.groovy", "Tests.groovy", "Spec.groovy",
	"Test.swift", "Tests.swift",
	".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
	".test.ts", ".test.tsx", ".test.js", ".test.jsx",
	"_test.py",
}

// testDirMarkers are lower-cased path substrings that indicate the file lives in
// a test tree even when its base name does not match a test suffix.
var testDirMarkers = []string{
	"src/test/", "src/androidtest/", "src/testintegration/", "__tests__",
}

// ClassifyFile classifies a single changed file (relative path, forward slashes
// preferred) into one of the four impact classes. The check order matters:
// build first, then config, then test, everything else is source.
func ClassifyFile(filePath string) Class {
	p := strings.TrimPrefix(filepath.ToSlash(filePath), "./")
	base := path.Base(p)
	if isBuildFile(p, base) {
		return ClassBuild
	}
	if isConfigFile(p, base) {
		return ClassConfig
	}
	if isTestFile(p, base) {
		return ClassTest
	}
	return ClassSource
}

// isBuildFile reports whether a path/base pair is a build, dependency or
// infrastructure file.
func isBuildFile(p, base string) bool {
	if _, ok := buildFiles[base]; ok {
		return true
	}
	if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") {
		return true
	}
	for _, prefix := range buildDirPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// isConfigFile reports whether a path/base pair is a configuration or resource
// file: Spring-style application/bootstrap YAML/properties and SQL migrations
// (Flyway/Liquibase or any *.sql script).
func isConfigFile(p, base string) bool {
	if strings.HasSuffix(base, ".sql") {
		return true
	}
	for _, stem := range []string{"application", "bootstrap"} {
		if strings.HasPrefix(base, stem) {
			lower := strings.ToLower(base)
			for _, ext := range []string{".yml", ".yaml", ".properties"} {
				if strings.HasSuffix(lower, ext) {
					return true
				}
			}
		}
	}
	return false
}

// isTestFile reports whether a path/base pair is a test file.
func isTestFile(p, base string) bool {
	lower := strings.ToLower(p)
	for _, marker := range testDirMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	for _, suffix := range testSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py")
}

// DiffFiles returns the changed file paths (relative to the repo root) between
// lastSHA and HEAD, using `git diff --name-only <lastSHA>..HEAD` run in root.
// An empty lastSHA means first run / full scan: it returns a nil slice and no
// error, and the caller treats the empty result as full=true.
func DiffFiles(root, lastSHA string) ([]string, error) {
	if lastSHA == "" {
		return nil, nil
	}
	cmd := exec.Command("git", "diff", "--name-only", lastSHA+"..HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	files := make([]string, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// IsAncestor reports whether sha is an ancestor of head (`git merge-base
// --is-ancestor <sha> <head>`, exit code 0 means true). It is used to detect a
// force push / reset where lastSHA left the ancestry chain, forcing a full
// rescan instead of an incremental one.
func IsAncestor(root, sha, head string) bool {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", sha, head)
	cmd.Dir = root
	return cmd.Run() == nil
}

// Impact is the aggregated impact of one change set on the test scope.
type Impact struct {
	Files           []string `json:"files"`
	FullRescan      bool     `json:"fullRescan"`      // a build/config change widens the owning module to full
	WidenedModules  []string `json:"widenedModules"`  // modules that must widen to a full rescan
	AffectedTests   []string `json:"affectedTests"`   // test files pulled directly into scope
	AffectedSources []string `json:"affectedSources"` // business source files affecting contracts
	ChangedTests    []string `json:"changedTests"`    // the changed test files
}

// ComputeImpact classifies a set of changed files and aggregates their impact.
// It is a pure function: given the same input it returns the same, sorted,
// output. Build and config changes set FullRescan and widen the file's owning
// module (derived as its directory; build files live at the module root so this
// is exact — for config files nested under src/*/resources, use BuildModule to
// resolve the owning module precisely). Test files enter AffectedTests and
// ChangedTests; source files enter AffectedSources.
func ComputeImpact(changed []string) Impact {
	imp := Impact{
		Files:           append(make([]string, 0, len(changed)), changed...),
		WidenedModules:  make([]string, 0),
		AffectedTests:   make([]string, 0),
		AffectedSources: make([]string, 0),
		ChangedTests:    make([]string, 0),
	}
	widened := make(map[string]struct{})
	for _, f := range changed {
		switch ClassifyFile(f) {
		case ClassBuild:
			imp.FullRescan = true
			addWidened(&imp, widened, f)
		case ClassConfig:
			imp.FullRescan = true
			addWidened(&imp, widened, f)
		case ClassTest:
			imp.ChangedTests = append(imp.ChangedTests, f)
			imp.AffectedTests = append(imp.AffectedTests, f)
		case ClassSource:
			imp.AffectedSources = append(imp.AffectedSources, f)
		}
	}
	sort.Strings(imp.Files)
	sort.Strings(imp.WidenedModules)
	sort.Strings(imp.AffectedTests)
	sort.Strings(imp.AffectedSources)
	sort.Strings(imp.ChangedTests)
	return imp
}

// addWidened records the owning module (best-effort directory) of a build or
// config file, deduplicating.
func addWidened(imp *Impact, seen map[string]struct{}, f string) {
	m := filepath.Dir(f)
	if _, ok := seen[m]; ok {
		return
	}
	seen[m] = struct{}{}
	imp.WidenedModules = append(imp.WidenedModules, m)
}

// BuildModule derives the owning Maven module / Gradle sub-module relative path
// for a build/config file path, by walking up from the file's directory until a
// directory owning a pom.xml or build.gradle* is found. It returns "" when no
// build anchor is found up to (and including) root.
func BuildModule(root, filePath string) string {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	if !filepath.IsAbs(filePath) {
		filePath = filepath.Join(root, filePath)
	}
	dir := filepath.Dir(filePath)
	for {
		abs, aerr := filepath.Abs(dir)
		if aerr != nil || !strings.HasPrefix(abs, rootAbs) {
			return ""
		}
		if hasBuildAnchor(dir) {
			rel, rerr := filepath.Rel(root, dir)
			if rerr != nil {
				return ""
			}
			return rel
		}
		if abs == rootAbs {
			return ""
		}
		dir = filepath.Dir(dir)
	}
}

// hasBuildAnchor reports whether dir directly owns a pom.xml or build.gradle*.
func hasBuildAnchor(dir string) bool {
	for _, name := range []string{"pom.xml", "build.gradle", "build.gradle.kts"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}
