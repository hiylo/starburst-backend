package intel

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// maxModuleDepth bounds the recursive anchor walk so huge monorepos are not
// walked exhaustively during type detection.
const maxModuleDepth = 5

// DetectModules walks a repository root and identifies its sub-projects
// (modules): every directory (including the root) that owns a build anchor is
// a module; monorepo layouts (services/, clients/, app/) and Maven multi-module
// <modules> fall out naturally. Each module gets a detected type and role.
func DetectModules(root string) ([]*store.IntelModule, error) {
	mods := make(map[string]*store.IntelModule) // key: relPath
	registerDir(root, ".", mods)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		if rel == "." {
			return nil
		}
		if skipDir(rel) {
			return filepath.SkipDir
		}
		if depth(rel) > maxModuleDepth {
			return filepath.SkipDir
		}
		registerDir(path, rel, mods)
		return nil
	})

	out := make([]*store.IntelModule, 0, len(mods))
	for _, m := range mods {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// registerDir registers a module for dir (with its anchors) when it owns a
// build anchor directly.
func registerDir(dir, rel string, mods map[string]*store.IntelModule) {
	anchors := anchorsInDir(dir)
	if len(anchors) == 0 {
		return
	}
	t := DetectType(anchorNames(anchors))
	if t == "" {
		return
	}
	mods[rel] = &store.IntelModule{
		RelPath:   rel,
		KindType:  t,
		KindRole:  roleForType(t),
		BuildTool: buildToolForNames(anchors),
	}
}

// anchorsInDir lists build anchor filenames present directly under dir.
func anchorsInDir(dir string) []string {
	out := make([]string, 0)
	for _, a := range buildAnchors {
		fi, err := os.Stat(filepath.Join(dir, a))
		if err == nil && !fi.IsDir() {
			out = append(out, a)
		}
	}
	// .xcodeproj / .xcworkspace are directories, not files.
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".xcodeproj") {
				out = append(out, e.Name())
			}
			if e.IsDir() && strings.HasSuffix(e.Name(), ".xcworkspace") {
				out = append(out, e.Name())
			}
		}
	}
	return out
}

// buildAnchors lists the filesystem anchors evaluated for every directory.
var buildAnchors = []string{"pom.xml", "build.gradle", "build.gradle.kts",
	"settings.gradle", "settings.gradle.kts", "go.mod", "package.json", "Package.swift"}

func anchorNames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

// buildToolForNames maps anchor filenames to a build tool.
func buildToolForNames(anchors []string) string {
	for _, a := range anchors {
		switch {
		case a == "pom.xml":
			return "maven"
		case strings.HasPrefix(a, "build.gradle") || strings.HasPrefix(a, "settings.gradle"):
			return "gradle"
		case a == "go.mod":
			return "go"
		case a == "package.json":
			return "npm"
		case a == "Package.swift":
			return "swiftpm"
		case strings.HasSuffix(a, ".xcodeproj") || strings.HasSuffix(a, ".xcworkspace"):
			return "xcode"
		}
	}
	return ""
}

func depth(rel string) int {
	if rel == "." {
		return 0
	}
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// skipDir reports whether a directory path is excluded from module detection.
func skipDir(rel string) bool {
	skip := map[string]bool{".git": true, ".svn": true, ".hg": true, "node_modules": true,
		"target": true, "build_artifacts": true, ".gradle": true, "dist": true, ".idea": true}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if skip[part] {
			return true
		}
	}
	return false
}

// roleForType returns a deterministic default business role for a type. The
// role is later refined by LLM + human overrides (overrides layer).
func roleForType(t string) string {
	switch t {
	case "android", "ios":
		return "app"
	case "java", "go", "node":
		return "backend"
	case "web":
		return "web"
	default:
		return ""
	}
}

// scanSummary is the persisted outcome of one analyze run for a module's
// contracts, keyed by module rel path.
type scanSummary struct {
	Entities  []*store.IntelEntity   `json:"entities"`
	Endpoints []*store.IntelEndpoint `json:"endpoints"`
}

// ScanModule scans a single module directory for its contracts. For a java
// module it extracts entities (JPA @Entity/@Table/@Column) and endpoints
// (Controller @*Mapping) with per-file:line provenance; non-Java types return
// an empty (not error) result for now (their scanners land with later
// milestones).
func ScanModule(root, relPath string) (*scanSummary, error) {
	dir := filepath.Join(root, relPath)
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	pom, _ := filepath.Glob(filepath.Join(dir, "pom.xml"))
	gradle, _ := filepath.Glob(filepath.Join(dir, "build.gradle*"))
	if len(pom) == 0 && len(gradle) == 0 {
		return &scanSummary{}, nil
	}
	files := make([]string, 0)
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirRel(path, dir) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".java") {
			files = append(files, path)
		}
		return nil
	})
	if len(files) == 0 {
		return &scanSummary{}, nil
	}
	ents, eps := scanJavaFiles(files)
	return &scanSummary{Entities: ents, Endpoints: eps}, nil
}

// skipDirRel reports whether a subdirectory (relative to the module dir) is
// build/vendor output excluded from scanning.
func skipDirRel(path, base string) bool {
	name := filepath.Base(path)
	return name == ".git" || name == "target" || name == "node_modules" ||
		name == ".gradle" || name == "build_artifacts" || name == "dist"
}