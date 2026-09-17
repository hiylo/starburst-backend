// Package feature clusters endpoint contracts into candidate feature points.
package feature

import (
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/hiylo/starburst-backend/internal/store"
)

// Candidate is a candidate feature point produced by clustering endpoints.
type Candidate struct {
	Name        string   `json:"name"`
	Anchor      string   `json:"anchor"`
	EndpointIDs []int64  `json:"endpointIds"`
	Ends        []string `json:"ends"`
	Confidence  string   `json:"confidence"`
}

// Cluster groups a set of endpoint contracts into candidate feature points.
//
// Grouping is anchored first on the SourceFile controller class name (the file
// basename with its "Controller" suffix and extension stripped); endpoints
// without a controller are grouped by the first non-parameter segment of the
// Path. Candidate names are the controller name or a title-cased path segment.
// Results are sorted by name, and endpoint IDs ascending, so the output is
// stable and reproducible.
func Cluster(endpoints []*store.IntelEndpoint) []Candidate {
	if len(endpoints) == 0 {
		return []Candidate{}
	}

	type group struct {
		name   string
		anchor string
		conf   string
		ids    []int64
		ends   map[string]bool
	}

	groups := make(map[string]*group)
	for _, ep := range endpoints {
		if ep == nil {
			continue
		}
		key, anchor, name, conf := clusterKey(ep)
		if key == "" {
			continue
		}
		g, ok := groups[key]
		if !ok {
			g = &group{name: name, anchor: anchor, conf: conf, ends: make(map[string]bool)}
			groups[key] = g
		}
		g.ids = append(g.ids, ep.ID)
		for _, e := range detectedEnds(ep) {
			g.ends[e] = true
		}
	}

	out := make([]Candidate, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.ids, func(i, j int) bool { return g.ids[i] < g.ids[j] })
		ends := make([]string, 0, len(g.ends))
		for e := range g.ends {
			ends = append(ends, e)
		}
		sort.Strings(ends)
		out = append(out, Candidate{
			Name:        g.name,
			Anchor:      g.anchor,
			EndpointIDs: g.ids,
			Ends:        ends,
			Confidence:  g.conf,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// clusterKey resolves the grouping key plus the derived anchor, name and
// confidence for a single endpoint. It returns an empty key when the endpoint
// cannot be clustered.
func clusterKey(ep *store.IntelEndpoint) (key, anchor, name, conf string) {
	if c := controllerName(ep.SourceFile); c != "" {
		return "c:" + c, c, c, "high"
	}
	if seg := pathSegment(ep.Path); seg != "" {
		return "p:" + seg, "/" + seg, titleName(seg), "medium"
	}
	return "", "", "", ""
}

// controllerName extracts the controller class name from a source file path by
// taking the basename and stripping a trailing "Controller" plus the file
// extension (e.g. BannerController.java -> Banner).
func controllerName(sourceFile string) string {
	base := filepath.Base(sourceFile)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	if strings.HasSuffix(base, "Controller") && len(base) > len("Controller") {
		return strings.TrimSuffix(base, "Controller")
	}
	return ""
}

// pathSegment returns the first non-empty, non-parameter segment of an HTTP
// path. Leading slashes, ":param" and "{param}" segments are skipped.
func pathSegment(path string) string {
	path = strings.TrimPrefix(strings.TrimSpace(path), "/")
	for _, seg := range strings.Split(path, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || strings.HasPrefix(seg, ":") || strings.HasPrefix(seg, "{") {
			continue
		}
		return seg
	}
	return ""
}

// titleName uppercases the first rune of a segment to produce a readable name.
func titleName(seg string) string {
	if seg == "" {
		return ""
	}
	r := []rune(seg)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// detectedEnds derives the ends touched by an endpoint. GraphQL operations
// (@QueryMapping/@MutationMapping) belong to the BFF end; everything else is
// treated as the Java backend by default. Client-side keywords in the source
// file path or HTTP path add android, ios, h5 or web (case-insensitive).
func detectedEnds(ep *store.IntelEndpoint) []string {
	ends := []string{"java"}
	switch ep.Method {
	case "QUERY", "MUTATION", "SUBSCRIPTION":
		ends[0] = "bff"
	}
	low := strings.ToLower(ep.SourceFile + " " + ep.Path)
	for _, kw := range []string{"android", "ios", "h5", "web"} {
		if strings.Contains(low, kw) {
			ends = append(ends, kw)
		}
	}
	return ends
}
