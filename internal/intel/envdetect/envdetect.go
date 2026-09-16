// Package envdetect derives the test environment dependencies (middleware and
// toolchain) of a project from its build and configuration files. It is a pure
// static parser: it never probes the local machine; it only reads declarations
// so an environment gate can check and install each item one by one.
package envdetect

import (
	"encoding/json"
	"encoding/xml"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Requirement is one detected environment dependency.
type Requirement struct {
	Service  string `json:"service"`  // mysql | redis | nacos | rabbitmq | elasticsearch | jdk | gradle | node | go | android-sdk
	Category string `json:"category"` // middleware | toolchain
	Version  string `json:"version"`  // expected version ("8", "17"), empty when unknown
	Source   string `json:"source"`   // relative path of the declaring file
}

// Detect scans the build/configuration files under root (limited to
// moduleRelPath when non-empty), derives the environment dependencies and
// returns them deduplicated by Service and sorted by Service.
func Detect(root, moduleRelPath string) ([]Requirement, error) {
	base := filepath.Clean(root)
	if moduleRelPath != "" {
		base = filepath.Join(base, filepath.Clean(moduleRelPath))
	}
	if fi, err := os.Stat(base); err == nil && !fi.IsDir() {
		base = filepath.Dir(base)
	}
	var reqs []Requirement
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		reqs = append(reqs, detectFile(root, path, d.Name())...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return dedupe(reqs), nil
}

// detectFile dispatches a build/config file to its parser and stamps every
// produced requirement with the file's repo-relative path.
func detectFile(root, path, name string) []Requirement {
	var rs []Requirement
	switch {
	case name == "pom.xml":
		rs = parsePom(path)
	case name == "build.gradle" || name == "build.gradle.kts":
		rs = parseGradle(path)
	case name == "go.mod":
		rs = parseGoMod(path)
	case name == "package.json":
		rs = parsePackageJSON(path)
	case isConfigFile(name):
		rs = parseConfig(path)
	}
	if len(rs) == 0 {
		return nil
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	for i := range rs {
		rs[i].Source = rel
	}
	return rs
}

// skipDirName reports whether a directory should be excluded from the walk.
func skipDirName(name string) bool {
	switch name {
	case ".git", ".svn", ".hg", "node_modules", "target", "build_artifacts", ".gradle", "dist", ".idea", "vendor":
		return true
	}
	return false
}

// isConfigFile reports whether a filename is a Spring Boot config file.
func isConfigFile(name string) bool {
	if !strings.HasPrefix(name, "application") && !strings.HasPrefix(name, "bootstrap") {
		return false
	}
	ext := filepath.Ext(name)
	return ext == ".yml" || ext == ".yaml" || ext == ".properties"
}

// dedupe collapses requirements by Service. The first declaration wins; a later
// declaration only replaces it when it carries a version and the current one
// does not (so the Source stays aligned with the resolved Version).
func dedupe(reqs []Requirement) []Requirement {
	seen := make(map[string]Requirement)
	names := make([]string, 0)
	for _, r := range reqs {
		cur, ok := seen[r.Service]
		if !ok {
			seen[r.Service] = r
			names = append(names, r.Service)
			continue
		}
		if cur.Version == "" && r.Version != "" {
			seen[r.Service] = r
		}
	}
	sort.Strings(names)
	out := make([]Requirement, 0, len(names))
	for _, n := range names {
		out = append(out, seen[n])
	}
	return out
}

// --- pom.xml (Java/Maven) -------------------------------------------------

type pomXML struct {
	Parent       pomParent `xml:"parent"`
	Dependencies []pomDep  `xml:"dependencies>dependency"`
	DepMgmtDeps  []pomDep  `xml:"dependencyManagement>dependencies>dependency"`
}

type pomParent struct {
	ArtifactID   string `xml:"artifactId"`
	RelativePath string `xml:"relativePath"`
}

type pomDep struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
}

var (
	rePropsBlock = regexp.MustCompile(`(?s)<properties>(.*?)</properties>`)
	rePropEntry  = regexp.MustCompile(`<([A-Za-z0-9_.\-]+)>\s*([^<]*?)\s*</[A-Za-z0-9_.\-]+>`)
)

// parsePom reads a Maven pom.xml and derives the JDK toolchain plus middleware
// from its dependency coordinates.
func parsePom(path string) []Requirement {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc pomXML
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	props := extractProps(string(raw))
	for k, v := range parentProps(path, doc.Parent) {
		if _, ok := props[k]; !ok {
			props[k] = v
		}
	}
	var out []Requirement
	if v := jdkVersion(props); v != "" {
		out = append(out, Requirement{Service: "jdk", Category: "toolchain", Version: v})
	}
	deps := make([]pomDep, 0, len(doc.Dependencies)+len(doc.DepMgmtDeps))
	deps = append(deps, doc.Dependencies...)
	deps = append(deps, doc.DepMgmtDeps...)
	for _, d := range deps {
		svc := pomDepService(d)
		if svc == "" {
			continue
		}
		out = append(out, Requirement{Service: svc, Category: "middleware",
			Version: resolveProp(d.Version, props)})
	}
	return out
}

// extractProps collects the <properties> entries of a pom document (all blocks,
// e.g. root + profile properties) into a name->value map.
func extractProps(raw string) map[string]string {
	props := make(map[string]string)
	for _, block := range rePropsBlock.FindAllStringSubmatch(raw, -1) {
		for _, e := range rePropEntry.FindAllStringSubmatch(block[1], -1) {
			props[e[1]] = strings.TrimSpace(e[2])
		}
	}
	return props
}

// parentProps resolves the parent pom (via <relativePath>, default ../pom.xml)
// and returns its properties so child ${property} version references resolve.
func parentProps(childPath string, p pomParent) map[string]string {
	if p.ArtifactID == "" {
		return nil
	}
	rel := p.RelativePath
	if rel == "" {
		rel = "../pom.xml"
	}
	ppath := filepath.Join(filepath.Dir(childPath), filepath.FromSlash(rel))
	if !strings.HasSuffix(ppath, ".xml") {
		ppath = filepath.Join(ppath, "pom.xml")
	}
	raw, err := os.ReadFile(ppath)
	if err != nil {
		return nil
	}
	return extractProps(string(raw))
}

// jdkVersion picks the Java toolchain version, preferring java.version over
// maven.compiler.source, and resolving property references.
func jdkVersion(props map[string]string) string {
	for _, key := range []string{"java.version", "maven.compiler.source"} {
		if v := resolveProp(props[key], props); v != "" {
			return v
		}
	}
	return ""
}

// resolveProp expands a Maven ${property} reference (possibly chained) against
// the merged property map. Unresolvable references yield "".
func resolveProp(value string, props map[string]string) string {
	v := strings.TrimSpace(value)
	for strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") {
		key := strings.TrimSuffix(strings.TrimPrefix(v, "${"), "}")
		nv, ok := props[key]
		if !ok || nv == "" {
			return ""
		}
		v = strings.TrimSpace(nv)
	}
	return v
}

// pomDepService maps a Maven dependency coordinate to a middleware service name
// ("" when it does not signal a managed middleware).
func pomDepService(d pomDep) string {
	g := strings.ToLower(d.GroupID)
	a := strings.ToLower(d.ArtifactID)
	switch {
	case strings.Contains(a, "mysql-connector") || strings.Contains(g, "mysql"):
		return "mysql"
	case a == "spring-boot-starter-data-redis" || strings.Contains(a, "redisson"):
		return "redis"
	case strings.Contains(a, "nacos") || strings.Contains(g, "nacos"):
		return "nacos"
	case a == "spring-boot-starter-amqp" || strings.Contains(a, "rabbitmq") ||
		strings.Contains(g, "rabbitmq"):
		return "rabbitmq"
	case a == "spring-boot-starter-data-elasticsearch" || strings.Contains(a, "elasticsearch") ||
		strings.Contains(g, "elasticsearch"):
		return "elasticsearch"
	}
	return ""
}

// --- build.gradle(.kts) (Android/Gradle) -----------------------------------

var (
	reCompileSdk   = regexp.MustCompile(`compileSdk(?:Version)?\s*[=:]?\s*["']?(?:android-)?(\d+)`)
	reAgpClasspath = regexp.MustCompile(`com\.android\.tools\.build:gradle:([0-9][0-9A-Za-z.\-]*)`)
	reAgpPlugin    = regexp.MustCompile(`id\s*\(\s*"com\.android\.(?:application|library)"\s*\)\s*version\s+"([0-9][0-9A-Za-z.\-]*)"`)
)

// parseGradle reads an Android Gradle build file and derives the Android SDK
// (from compileSdk) and Gradle toolchain (from the Android Gradle Plugin version).
func parseGradle(path string) []Requirement {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := string(raw)
	var out []Requirement
	if m := reCompileSdk.FindStringSubmatch(s); m != nil {
		out = append(out, Requirement{Service: "android-sdk", Category: "toolchain", Version: m[1]})
	}
	if m := reAgpClasspath.FindStringSubmatch(s); m != nil {
		out = append(out, Requirement{Service: "gradle", Category: "toolchain", Version: m[1]})
	} else if m := reAgpPlugin.FindStringSubmatch(s); m != nil {
		out = append(out, Requirement{Service: "gradle", Category: "toolchain", Version: m[1]})
	}
	return out
}

// --- go.mod (Go) -----------------------------------------------------------

var reGoDirective = regexp.MustCompile(`(?m)^go\s+(\d+(?:\.\d+)*)`)

// parseGoMod reads a go.mod and derives the Go toolchain version from the "go"
// directive.
func parseGoMod(path string) []Requirement {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	m := reGoDirective.FindStringSubmatch(string(raw))
	if m == nil {
		return nil
	}
	return []Requirement{{Service: "go", Category: "toolchain", Version: m[1]}}
}

// --- package.json (Node/Web) ----------------------------------------------

type pkgFile struct {
	Engines         pkgEngines        `json:"engines"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

type pkgEngines struct {
	Node string `json:"node"`
}

var reVersionNum = regexp.MustCompile(`\d+(?:\.\d+)*`)

// parsePackageJSON reads a package.json and derives the Node toolchain from
// engines.node or from a Node test runner (playwright/vitest/jest) dependency.
func parsePackageJSON(path string) []Requirement {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg pkgFile
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil
	}
	version := ""
	need := false
	if v := strings.TrimSpace(pkg.Engines.Node); v != "" {
		version = engineVersion(v)
		need = true
	}
	if !need && (hasDep(pkg.Dependencies, "playwright") ||
		hasDep(pkg.DevDependencies, "playwright") ||
		hasDep(pkg.Dependencies, "vitest") || hasDep(pkg.DevDependencies, "vitest") ||
		hasDep(pkg.Dependencies, "jest") || hasDep(pkg.DevDependencies, "jest")) {
		need = true
	}
	if !need {
		return nil
	}
	return []Requirement{{Service: "node", Category: "toolchain", Version: version}}
}

// hasDep reports whether a dependency map contains a package whose name
// contains keyword.
func hasDep(deps map[string]string, keyword string) bool {
	for k := range deps {
		if strings.Contains(k, keyword) {
			return true
		}
	}
	return false
}

// engineVersion normalizes an engines.node value (e.g. ">=20.0.0") to its bare
// version number.
func engineVersion(s string) string {
	if m := reVersionNum.FindString(s); m != "" {
		return m
	}
	return strings.TrimSpace(s)
}

// --- application.yml / application.properties (Spring config) -------------

// parseConfig reads a Spring Boot config file and derives middleware from the
// datasource URL and the spring.* key namespaces.
func parseConfig(path string) []Requirement {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(raw), "\n")
	isProps := filepath.Ext(path) == ".properties"
	keys := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if isProps {
			k := t
			if i := strings.IndexAny(t, "=:"); i >= 0 {
				k = t[:i]
			}
			keys = append(keys, strings.TrimSpace(k))
		}
	}
	if !isProps {
		keys = dottedKeys(lines)
	}
	flags := make(map[string]bool)
	for _, k := range keys {
		low := strings.ToLower(k)
		switch {
		case strings.Contains(low, "spring.data.redis") || strings.Contains(low, "spring.redis"):
			flags["redis"] = true
		case strings.Contains(low, "spring.cloud.nacos"):
			flags["nacos"] = true
		case strings.Contains(low, "spring.rabbitmq"):
			flags["rabbitmq"] = true
		case strings.Contains(low, "spring.elasticsearch"):
			flags["elasticsearch"] = true
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.Contains(strings.ToLower(line), "jdbc:mysql") {
			flags["mysql"] = true
		}
	}
	out := make([]Requirement, 0, len(flags))
	for _, svc := range []string{"mysql", "redis", "nacos", "rabbitmq", "elasticsearch"} {
		if flags[svc] {
			out = append(out, Requirement{Service: svc, Category: "middleware"})
		}
	}
	return out
}

// dottedKeys flattens a YAML document into dotted key paths (one per key line)
// so nested spring.redis / spring.cloud.nacos blocks can be matched without a
// full YAML parser.
func dottedKeys(lines []string) []string {
	type level struct {
		indent int
		key    string
	}
	stack := make([]level, 0, 8)
	out := make([]string, 0, len(lines))
	for _, raw := range lines {
		line := stripYamlComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		content := strings.TrimSpace(line)
		if strings.HasPrefix(content, "- ") {
			content = strings.TrimPrefix(content, "- ")
			indent++
		}
		key := content
		if i := strings.IndexByte(content, ':'); i >= 0 {
			key = content[:i]
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		stack = append(stack, level{indent: indent, key: key})
		parts := make([]string, len(stack))
		for i, l := range stack {
			parts[i] = l.key
		}
		out = append(out, strings.Join(parts, "."))
	}
	return out
}

// stripYamlComment removes a trailing " #comment" from a line.
func stripYamlComment(line string) string {
	if i := strings.Index(line, " #"); i >= 0 {
		return line[:i]
	}
	return line
}
