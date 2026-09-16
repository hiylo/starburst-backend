// Package gateway detects and parses API gateway route configuration so the
// Test Intelligence subsystem can model the public (gateway) exposure of an
// internal service endpoint. It reads two deterministic sources:
//
//   - Spring Cloud Gateway static routes (spring.cloud.gateway.routes): each
//     entry carries uri (lb://service) + Path= predicates.
//   - Nacos discovery metadata gateway.paths declared in a module's
//     bootstrap/application yaml, which is the dynamic-route matrix used by the
//     framework gateway (ServiceStateLoader).
package gateway

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Route is one gateway exposure: a backend service and the public path
// pattern(s) under which it is reachable. A single service commonly declares
// multiple paths (e.g. activity-provider → /activity/**,/order/**,/signup/**).
type Route struct {
	Service    string   `json:"service"`    // backend service id (spring.application.name or lb:// target)
	Paths      []string `json:"paths"`      // public gateway path patterns, e.g. /activity/**
	URI        string   `json:"uri"`        // raw route uri (static routes only), e.g. lb://payment-provider
	Source     string   `json:"source"`     // config file path relative to root
	SourceLine int      `json:"sourceLine"` // 1-based line of the declaration
}

// skipDir is a shared skip predicate matching the scanner's ignore set.
func skipDir(name string) bool {
	return name == ".git" || name == "target" || name == "node_modules" ||
		name == ".gradle" || name == "build_artifacts" || name == "dist"
}

// Discover walks root for gateway route configuration and returns the declared
// routes. It is deterministic and read-only.
func Discover(root string) ([]*Route, error) {
	routes := make([]*Route, 0)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isYAML(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			return nil
		}
		routes = append(routes, parseRoutes(rel, data, &node)...)
		return nil
	})
	return dedup(routes), err
}

// isYAML reports whether filename is a YAML configuration file.
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// parseRoutes extracts routes from a single YAML document: the gateway.paths
// metadata (dynamic route matrix) plus any spring.cloud.gateway.routes static
// entries.
func parseRoutes(rel string, data []byte, node *yaml.Node) []*Route {
	doc := decodeMap(node)
	routes := make([]*Route, 0)

	appName := yamlString(lookup(doc, "spring", "application", "name"))
	for _, p := range collectGatewayPaths(doc) {
		routes = append(routes, &Route{
			Service:    appName,
			Paths:      splitPaths(p),
			Source:     rel,
			SourceLine: lineOf(data, "gateway.paths"),
		})
	}

	for _, r := range staticRoutes(doc) {
		r.Source = rel
		r.SourceLine = lineOf(data, "Path=")
		routes = append(routes, r)
	}
	return routes
}

// decodeMap converts the top-level yaml node into a nested map, tolerating
// documents that are lists or scalars (returns empty map).
func decodeMap(node *yaml.Node) map[string]any {
	if node == nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := node.Decode(&out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

// lookup walks a nested key path through the decoded map.
func lookup(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = mm[k]
		if !ok {
			return nil
		}
	}
	return cur
}

func yamlString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// collectGatewayPaths finds every gateway.paths value in the document (a Nacos
// metadata key carrying a comma-separated list of public path patterns).
func collectGatewayPaths(m map[string]any) []string {
	out := make([]string, 0)
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "gateway.paths" {
					if s := yamlString(val); s != "" {
						out = append(out, s)
					}
					continue
				}
				walk(val)
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(m)
	return out
}

// staticRoutes extracts Spring Cloud Gateway static route entries.
func staticRoutes(doc map[string]any) []*Route {
	routesNode := lookup(doc, "spring", "cloud", "gateway", "routes")
	list, ok := routesNode.([]any)
	if !ok {
		return nil
	}
	out := make([]*Route, 0)
	for _, item := range list {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		uri := yamlString(entry["uri"])
		service := serviceFromURI(uri)
		paths := predicatePaths(entry["predicates"])
		if service == "" && len(paths) == 0 {
			continue
		}
		out = append(out, &Route{
			Service: service,
			Paths:   paths,
			URI:     uri,
		})
	}
	return out
}

// serviceFromURI strips the lb:// (or http(s)://) scheme from a route uri,
// returning the bare service id. Returns "" when uri is empty.
func serviceFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	if i := strings.Index(uri, "://"); i >= 0 {
		uri = uri[i+3:]
	}
	uri = strings.TrimRight(uri, "/")
	if i := strings.IndexAny(uri, "/:"); i >= 0 {
		uri = uri[:i]
	}
	return uri
}

// predicatePaths extracts Path=… patterns from a predicates node.
func predicatePaths(node any) []string {
	out := make([]string, 0)
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if strings.HasPrefix(t, "Path=") {
				out = append(out, strings.TrimPrefix(t, "Path="))
			}
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			for _, item := range t {
				walk(item)
			}
		}
	}
	walk(node)
	return out
}

// splitPaths splits a comma-separated gateway.paths value into individual
// patterns, trimming whitespace, dropping empties, and stripping a trailing
// StripPrefix annotation (e.g. /merchant/graphql/**=1 → /merchant/graphql/**).
func splitPaths(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.LastIndexByte(p, '='); i >= 0 {
			if _, err := strconv.Atoi(p[i+1:]); err == nil {
				p = p[:i]
			}
		}
		out = append(out, p)
	}
	return out
}

// lineOf returns the 1-based line number of the first occurrence of needle in
// data, or 0 when absent.
func lineOf(data []byte, needle string) int {
	if needle == "" {
		return 0
	}
	idx := strings.Index(string(data), needle)
	if idx < 0 {
		return 0
	}
	return strings.Count(string(data[:idx]), "\n") + 1
}

// dedup drops routes with identical (service, paths, uri, source) tuples.
func dedup(routes []*Route) []*Route {
	seen := make(map[string]bool)
	out := make([]*Route, 0, len(routes))
	for _, r := range routes {
		if r == nil {
			continue
		}
		key := r.Service + "|" + strings.Join(r.Paths, ",") + "|" + r.URI + "|" + r.Source
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// Match returns the public gateway path patterns (e.g. /activity/**) under
// which the given internal endpoint path is exposed. A path pattern matches
// when the endpoint path is a prefix-equal descendant of the pattern prefix.
func Match(routes []*Route, endpointPath string) []string {
	ep := strings.TrimSpace(endpointPath)
	if ep == "" {
		return nil
	}
	out := make([]string, 0)
	seen := make(map[string]bool)
	for _, r := range routes {
		for _, p := range r.Paths {
			prefix := strings.TrimSuffix(strings.TrimSpace(p), "/**")
			if prefix == "" || prefix == "/" {
				continue
			}
			if ep == prefix || strings.HasPrefix(ep, prefix+"/") {
				if !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
}
