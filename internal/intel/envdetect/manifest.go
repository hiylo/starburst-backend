// This file is part of the envdetect package: it parses the manually declared
// "intel-env.yaml" manifest and merges its declarations over the auto-detected
// requirements. The manifest lets a project pin the exact middleware/toolchain
// versions and declare schema/data init scripts that static build-file parsing
// cannot reliably infer.
package envdetect

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifestFileName is the repo-root file that carries manual environment
// declarations for a project.
const manifestFileName = "intel-env.yaml"

// InitScript is a manually declared schema/data initialization step.
type InitScript struct {
	Service string `json:"service"`
	Script  string `json:"script"`
}

// manifestFile is the lenient YAML document backing intel-env.yaml.
type manifestFile struct {
	Services    []manifestService `yaml:"services"`
	InitScripts []InitScript      `yaml:"init_scripts"`
}

// manifestService is one entry under the "services" list.
type manifestService struct {
	Service string `yaml:"service"`
	Version string `yaml:"version"`
	Port    int    `yaml:"port"` // informational, not surfaced in Requirement
}

// ParseManifest parses intel-env.yaml content into environment requirements and
// init scripts. Parsing is lenient: missing sections or fields yield zero
// values, only a malformed YAML document returns an error (with empty slices).
// Every requirement is stamped with the given source (e.g. "intel-env.yaml").
func ParseManifest(data []byte, source string) ([]Requirement, []InitScript, error) {
	var doc manifestFile
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	reqs := make([]Requirement, 0, len(doc.Services))
	for _, s := range doc.Services {
		reqs = append(reqs, Requirement{
			Service:  s.Service,
			Category: categoryFor(s.Service),
			Version:  s.Version,
			Source:   source,
		})
	}
	scripts := make([]InitScript, 0, len(doc.InitScripts))
	scripts = append(scripts, doc.InitScripts...)
	return reqs, scripts, nil
}

// ManifestAt reports whether root contains an intel-env.yaml at its top level
// (one level only, never recursed) and returns its absolute path when present.
func ManifestAt(root string) (string, bool) {
	p := filepath.Join(root, manifestFileName)
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p, true
		}
		return abs, true
	}
	return "", false
}

// MergeWithDetect merges the manifest requirements over the auto-detected ones.
// When a service is declared in both, the manifest version/category/source win;
// auto-detected services absent from the manifest are kept. The result is
// deduplicated by Service and sorted by Service.
func MergeWithDetect(detected []Requirement, manifest []Requirement) []Requirement {
	byService := make(map[string]Requirement, len(detected)+len(manifest))
	names := make(map[string]bool, len(detected)+len(manifest))
	for _, r := range detected {
		if r.Service == "" {
			continue
		}
		byService[r.Service] = r
		names[r.Service] = true
	}
	for _, r := range manifest {
		if r.Service == "" {
			continue
		}
		if _, ok := byService[r.Service]; !ok {
			names[r.Service] = true
		}
		byService[r.Service] = r
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]Requirement, 0, len(sorted))
	for _, n := range sorted {
		out = append(out, byService[n])
	}
	return out
}

// categoryFor infers the Requirement category from a service name. Known
// middleware (databases, message queues, registries, object storage) map to
// "middleware"; known toolchains (language runtimes and build systems) map to
// "toolchain". Unknown services default to "middleware".
func categoryFor(service string) string {
	switch strings.ToLower(service) {
	case "jdk", "gradle", "node", "go", "android-sdk", "xcode", "python",
		"maven", "npm", "yarn", "pnpm", "rust", "golangci-lint":
		return "toolchain"
	default:
		return "middleware"
	}
}
