// Package intel implements the Test Intelligence subsystem: repository
// understanding, contract extraction, feature-point detection and scanning.
// It is deterministic-first: table/field/required facts come from static code
// extraction; LLM only explains, completes and attributes.
package intel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Profile describes one recognizable technology stack. Each registered Profile
// bundles its detection anchors, scanner, test commands, dependency-vuln tool,
// compliance rules and default command whitelist (registry = pluggable).
type Profile struct {
	Type        string   // java, go, android, ios, web, node, ...
	Role        string   // default business role guess (user, admin, client...)
	Anchors     []string // relative file names that mark this type
	AnchorMatch func(relPath string) bool
}

// profiles is the pluggable registry. Detection evaluates anchors by relative
// path within a module; the first profile whose anchor matches wins.
var profiles = []*Profile{
	{Type: "android", Role: "app", Anchors: []string{"build.gradle.kts", "settings.gradle.kts"}},
	{Type: "ios", Role: "app", Anchors: []string{".xcodeproj", ".xcworkspace", "Package.swift"}},
	{Type: "java", Role: "backend", Anchors: []string{"pom.xml"}},
	{Type: "go", Role: "backend", Anchors: []string{"go.mod"}},
	{Type: "web", Role: "web", Anchors: []string{"package.json"}},
	{Type: "node", Role: "service", Anchors: []string{"package.json"}},
	{Type: "bff", Role: "bff", Anchors: []string{"schema.graphqls"}},
}

// hasAnchor reports whether any file in files (relative paths) matches the
// profile's anchors.
func (p *Profile) hasAnchor(files []string) bool {
	for _, f := range files {
		base := filepath.Base(f)
		for _, a := range p.Anchors {
			if strings.Contains(f, a) || base == a {
				return true
			}
		}
	}
	return false
}

// DetectType infers the module type from its anchor file names alone (no file
// content). A bare package.json maps to "node" (the generic service case);
// callers that have a directory available should use DetectTypeInDir so a
// vue/react manifest can refine the result to "web". The returned type is empty
// when nothing matches (unknown/static content directory).
func DetectType(files []string) string {
	hasPkg := false
	for _, f := range files {
		if filepath.Base(f) == "package.json" {
			hasPkg = true
		}
	}
	// Non-package anchors win in registry priority order. go.mod sits ahead of
	// package.json so Go repos that carry a package.json for frontend tooling
	// are still recognized as Go.
	for _, p := range profiles {
		if p.Type == "web" || p.Type == "node" {
			continue
		}
		if p.hasAnchor(files) {
			return p.Type
		}
	}
	if hasPkg {
		return "node"
	}
	return ""
}

// DetectTypeInDir infers the module type for a directory, reading package.json
// content to distinguish a Web (vue/react) module from a generic Node service.
func DetectTypeInDir(dir string, anchors []string) string {
	t := DetectType(anchorNames(anchors))
	if t != "node" {
		return t
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "node"
	}
	if packageJSONIsWeb(data) {
		return "web"
	}
	return "node"
}

// packageJSONIsWeb reports whether a package.json manifest declares a vue or
// react dependency (in dependencies or devDependencies).
func packageJSONIsWeb(data []byte) bool {
	var manifest struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	for name := range manifest.Dependencies {
		if isWebFramework(name) {
			return true
		}
	}
	for name := range manifest.DevDependencies {
		if isWebFramework(name) {
			return true
		}
	}
	return false
}

// isWebFramework reports whether a package name refers to a vue or react
// framework (vue, @vue/*, react, react-dom, @react-*...).
func isWebFramework(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "vue") || strings.Contains(lower, "react")
}
