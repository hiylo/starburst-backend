// Package intel implements the Test Intelligence subsystem: repository
// understanding, contract extraction, feature-point detection and scanning.
// It is deterministic-first: table/field/required facts come from static code
// extraction; LLM only explains, completes and attributes.
package intel

import (
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

// DetectType infers the module type from its relative file set. The registry is
// consulted in a fixed priority order; the returned type is empty when nothing
// matches (unknown/static content directory).
func DetectType(files []string) string {
	hasWebDep := false
	hasGoDep := false
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "package.json") {
			hasWebDep = true
		}
		if filepath.Base(f) == "go.mod" {
			hasGoDep = true
		}
	}
	for _, p := range profiles {
		if !p.hasAnchor(files) {
			continue
		}
		// package.json marks web (vue/react) only when the dependency set says
		// so; otherwise it is a generic node service.
		if p.Type == "web" || p.Type == "node" {
			if hasGoDep {
				continue
			}
			if p.Type == "web" && !hasWebDep {
				continue
			}
		}
		return p.Type
	}
	if hasGoDep {
		return "go"
	}
	if hasWebDep {
		return "node"
	}
	return ""
}
