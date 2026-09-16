// Package deps parses ecosystem dependency manifests (Maven pom.xml, Go
// go.mod, npm package.json and Gradle build.gradle) into a unified
// Dependency list. The output feeds the vulnerability scan layer (Trivy/OSV)
// and the dependency snapshot cache used for scan de-duplication.
//
// This is pure static parsing: it never executes a build tool, reaches the
// network or resolves transitive dependencies.
package deps

import (
	"encoding/json"
	"encoding/xml"
	"regexp"
	"sort"
	"strings"
)

// Dependency is one normalized dependency coordinate across ecosystems.
type Dependency struct {
	Ecosystem string `json:"ecosystem"` // maven | go | npm | gradle
	Group     string `json:"group"`     // maven groupId / npm scope; empty for go/gradle
	Name      string `json:"name"`      // artifactId / module path / package name
	Version   string `json:"version"`   // version, empty when unresolved
	Scope     string `json:"scope"`     // compile | test | runtime | provided, empty when absent
}

// propertyRef matches a Maven ${prop} version placeholder.
var propertyRef = regexp.MustCompile(`\$\{([^}]+)\}`)

// gradleDep matches "config 'group:name:version'" and "config(\"group:name:version\")".
var gradleDep = regexp.MustCompile(
	`\b(implementation|api|compileOnly|runtimeOnly|testImplementation|testRuntimeOnly|testCompileOnly|compile|runtime)` +
		`\s*\(?\s*['"]([^'"]+):([^'"]+):([^'"]+)['"]`)

// ParsePom parses a Maven pom.xml payload and returns the resolved dependency
// list. Versions are resolved against <properties> and <dependencyManagement>
// (including versions inherited via the parent) using simple ${prop}
// substitution; a placeholder that cannot be resolved is kept verbatim.
func ParsePom(data []byte) ([]Dependency, error) {
	var proj pomProject
	if err := xml.Unmarshal(data, &proj); err != nil {
		return nil, err
	}

	props := map[string]string{}
	for _, p := range proj.Properties.Items {
		props[p.XMLName.Local] = strings.TrimSpace(p.Value)
	}
	if proj.Version != "" {
		props["project.version"] = strings.TrimSpace(proj.Version)
	}
	if proj.Parent != nil {
		pv := strings.TrimSpace(proj.Parent.Version)
		props["parent.version"] = pv
		if proj.Version == "" {
			props["project.version"] = pv
		}
	}

	managed := map[string]string{}
	for _, d := range proj.DependencyManagement.Dependencies {
		key := strings.TrimSpace(d.GroupID) + ":" + strings.TrimSpace(d.ArtifactID)
		managed[key] = substituteProps(strings.TrimSpace(d.Version), props)
	}

	var deps []Dependency
	for _, d := range proj.Dependencies.Items {
		group := strings.TrimSpace(d.GroupID)
		name := strings.TrimSpace(d.ArtifactID)
		version := strings.TrimSpace(d.Version)
		if version == "" {
			version = managed[group+":"+name]
		}
		version = substituteProps(version, props)
		deps = append(deps, Dependency{
			Ecosystem: "maven",
			Group:     group,
			Name:      name,
			Version:   version,
			Scope:     strings.TrimSpace(d.Scope),
		})
	}

	sortDeps(deps)
	return deps, nil
}

// ParseGoMod parses a go.mod payload and returns its require list. Indirect
// dependencies keep their version; the trailing "// indirect" marker is
// dropped. Modules listed without a version yield an empty Version.
func ParseGoMod(data []byte) ([]Dependency, error) {
	var deps []Dependency
	inBlock := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "require (" {
			inBlock = true
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if d, ok := parseGoRequire(line); ok {
				deps = append(deps, d)
			}
			continue
		}
		if strings.HasPrefix(line, "require ") {
			rest := strings.TrimSpace(strings.TrimPrefix(line, "require "))
			if d, ok := parseGoRequire(rest); ok {
				deps = append(deps, d)
			}
		}
	}
	sortDeps(deps)
	return deps, nil
}

// ParsePackageJSON parses a package.json payload and returns dependencies
// (scope=runtime) plus devDependencies (scope=test).
func ParsePackageJSON(data []byte) ([]Dependency, error) {
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, err
	}
	var deps []Dependency
	for name, version := range pkg.Dependencies {
		deps = append(deps, Dependency{
			Ecosystem: "npm",
			Name:      name,
			Version:   version,
			Scope:     "runtime",
		})
	}
	for name, version := range pkg.DevDependencies {
		deps = append(deps, Dependency{
			Ecosystem: "npm",
			Name:      name,
			Version:   version,
			Scope:     "test",
		})
	}
	sortDeps(deps)
	return deps, nil
}

// ParseGradle parses a build.gradle payload and returns dependencies declared
// in "group:name:version" string form across common configurations.
func ParseGradle(data []byte) ([]Dependency, error) {
	var deps []Dependency
	for _, m := range gradleDep.FindAllStringSubmatch(string(data), -1) {
		deps = append(deps, Dependency{
			Ecosystem: "gradle",
			Group:     m[2],
			Name:      m[3],
			Version:   m[4],
			Scope:     gradleScope(m[1]),
		})
	}
	sortDeps(deps)
	return deps, nil
}

// substituteProps replaces ${prop} references using props. Resolution loops a
// bounded number of times so chained references settle; unresolved references
// are left in place.
func substituteProps(s string, props map[string]string) string {
	for i := 0; i < 8; i++ {
		next := propertyRef.ReplaceAllStringFunc(s, func(m string) string {
			if v, ok := props[m[2:len(m)-1]]; ok {
				return v
			}
			return m
		})
		if next == s {
			return next
		}
		s = next
	}
	return s
}

// parseGoRequire parses one "module [version]" line.
func parseGoRequire(line string) (Dependency, bool) {
	if i := strings.Index(line, "//"); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return Dependency{}, false
	}
	if fields[0] == ")" {
		return Dependency{}, false
	}
	version := ""
	if len(fields) >= 2 {
		version = fields[1]
	}
	return Dependency{Ecosystem: "go", Name: fields[0], Version: version}, true
}

// gradleScope maps a Gradle configuration to the unified scope vocabulary.
func gradleScope(config string) string {
	switch config {
	case "compileOnly":
		return "provided"
	case "runtimeOnly", "runtime":
		return "runtime"
	case "testImplementation", "testRuntimeOnly", "testCompileOnly":
		return "test"
	default:
		return "compile"
	}
}

// sortDeps orders a dependency list deterministically for stable snapshots.
func sortDeps(deps []Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		a, b := deps[i], deps[j]
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Scope < b.Scope
	})
}

// pomProject models the subset of pom.xml required for dependency parsing.
type pomProject struct {
	XMLName              xml.Name                `xml:"project"`
	Version              string                  `xml:"version"`
	Parent               *pomParent              `xml:"parent"`
	Properties           pomProperties           `xml:"properties"`
	DependencyManagement pomDependencyManagement `xml:"dependencyManagement"`
	Dependencies         pomDependencies         `xml:"dependencies"`
}

// pomParent carries the parent coordinate used for version inheritance.
type pomParent struct {
	Version string `xml:"version"`
}

// pomProperties captures arbitrary <properties> children by tag name.
type pomProperties struct {
	Items []pomProperty `xml:",any"`
}

// pomProperty is one <key>value</key> entry under <properties>.
type pomProperty struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

// pomDependencyManagement wraps managed dependencies used for version lookup.
type pomDependencyManagement struct {
	Dependencies []pomDependency `xml:"dependencies>dependency"`
}

// pomDependencies wraps the direct dependency list.
type pomDependencies struct {
	Items []pomDependency `xml:"dependency"`
}

// pomDependency is a single <dependency> element.
type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
}

// packageJSON models the dependency-relevant fields of package.json.
type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}
