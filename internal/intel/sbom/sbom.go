// Package sbom renders a normalized dependency list as a CycloneDX 1.5 JSON
// software bill of materials for external delivery and audit reconciliation.
// It is pure data transformation: no network, filesystem or tool execution.
package sbom

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/hiylo/starburst-backend/internal/intel/deps"
)

// bomFormat is the CycloneDX format identifier embedded in every document.
const bomFormat = "CycloneDX"

// specVersion is the CycloneDX JSON schema version this package emits.
const specVersion = "1.5"

// bom is the top-level CycloneDX 1.5 JSON document.
type bom struct {
	BomFormat   string      `json:"bomFormat"`
	SpecVersion string      `json:"specVersion"`
	Version     int         `json:"version"`
	Metadata    bomMetadata `json:"metadata"`
	Components  []component `json:"components"`
}

// bomMetadata holds the document-level component describing the project.
type bomMetadata struct {
	Component metadataComponent `json:"component"`
}

// metadataComponent identifies the application the SBOM describes.
type metadataComponent struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// component is a single library entry in the SBOM component list.
type component struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Purl    string `json:"purl"`
	BomRef  string `json:"bom-ref"`
}

// Generate turns a dependency list into a CycloneDX 1.5 JSON document. The
// project name lands in metadata.component.name; dependencies are de-duplicated
// by ecosystem + name before being emitted as library components.
func Generate(projectName string, deps []deps.Dependency) ([]byte, error) {
	unique := dedup(deps)
	components := make([]component, 0, len(unique))
	for _, d := range unique {
		purl := ComponentPurl(d)
		components = append(components, component{
			Type:    "library",
			Name:    d.Name,
			Version: d.Version,
			Purl:    purl,
			BomRef:  purl,
		})
	}

	doc := bom{
		BomFormat:   bomFormat,
		SpecVersion: specVersion,
		Version:     1,
		Metadata: bomMetadata{
			Component: metadataComponent{
				Type: "application",
				Name: projectName,
			},
		},
		Components: components,
	}
	return json.Marshal(doc)
}

// ComponentPurl builds the package-url for a single dependency: maven becomes
// pkg:maven/<group>/<name>@<version>, npm pkg:npm/<name>@<version>, go
// pkg:golang/<name>@<version> and gradle pkg:generic/<name>@<version>. Scoped
// npm names (@scope/name) drop the leading @. An empty version omits the
// trailing @version qualifier.
func ComponentPurl(d deps.Dependency) string {
	var purl string
	switch d.Ecosystem {
	case "maven":
		purl = "pkg:maven/" + d.Group + "/" + d.Name
	case "npm":
		purl = "pkg:npm/" + strings.TrimPrefix(d.Name, "@")
	case "go":
		purl = "pkg:golang/" + d.Name
	default:
		purl = "pkg:generic/" + d.Name
	}
	if d.Version != "" {
		purl += "@" + d.Version
	}
	return purl
}

// dedup collapses duplicate ecosystem + name coordinates, keeping the entry
// with the most specific (non-empty) version. Sorting is deterministic so the
// first entry per coordinate is stable.
func dedup(list []deps.Dependency) []deps.Dependency {
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Ecosystem != b.Ecosystem {
			return a.Ecosystem < b.Ecosystem
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if (a.Version == "") != (b.Version == "") {
			return a.Version != ""
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Scope < b.Scope
	})

	unique := make([]deps.Dependency, 0, len(list))
	for _, d := range list {
		if len(unique) > 0 {
			prev := unique[len(unique)-1]
			if prev.Ecosystem == d.Ecosystem && prev.Name == d.Name {
				continue
			}
		}
		unique = append(unique, d)
	}
	return unique
}
