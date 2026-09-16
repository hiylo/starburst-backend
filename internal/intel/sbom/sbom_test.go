package sbom

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hiylo/starburst-backend/internal/intel/deps"
)

func TestComponentPurl(t *testing.T) {
	tests := []struct {
		name string
		dep  deps.Dependency
		want string
	}{
		{
			name: "maven",
			dep:  deps.Dependency{Ecosystem: "maven", Group: "org.example", Name: "core", Version: "1.2.3"},
			want: "pkg:maven/org.example/core@1.2.3",
		},
		{
			name: "go",
			dep:  deps.Dependency{Ecosystem: "go", Name: "github.com/foo/bar", Version: "v1.0.0"},
			want: "pkg:golang/github.com/foo/bar@v1.0.0",
		},
		{
			name: "npm",
			dep:  deps.Dependency{Ecosystem: "npm", Name: "lodash", Version: "4.17.21"},
			want: "pkg:npm/lodash@4.17.21",
		},
		{
			name: "npm scoped",
			dep:  deps.Dependency{Ecosystem: "npm", Name: "@scope/pkg", Version: "2.0.0"},
			want: "pkg:npm/scope/pkg@2.0.0",
		},
		{
			name: "gradle",
			dep:  deps.Dependency{Ecosystem: "gradle", Name: "androidx.core", Version: "1.9.0"},
			want: "pkg:generic/androidx.core@1.9.0",
		},
		{
			name: "empty version",
			dep:  deps.Dependency{Ecosystem: "go", Name: "example.com/mod"},
			want: "pkg:golang/example.com/mod",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ComponentPurl(tt.dep); got != tt.want {
				t.Fatalf("ComponentPurl() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerateDeduplicates(t *testing.T) {
	out, err := Generate("demo", []deps.Dependency{
		{Ecosystem: "npm", Name: "lodash", Version: ""},
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.21"},
		{Ecosystem: "npm", Name: "lodash", Version: "4.17.21"},
		{Ecosystem: "maven", Group: "org.example", Name: "core", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var doc bom
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(doc.Components) != 2 {
		t.Fatalf("len(components) = %d, want 2", len(doc.Components))
	}
	if doc.Components[0].Name != "core" || doc.Components[0].Version != "1.0.0" {
		t.Fatalf("core component = %+v, want version 1.0.0", doc.Components[0])
	}
	if doc.Components[1].Name != "lodash" || doc.Components[1].Version != "4.17.21" {
		t.Fatalf("lodash component = %+v, want version 4.17.21", doc.Components[1])
	}
}

func TestGenerateStructure(t *testing.T) {
	out, err := Generate("my-project", []deps.Dependency{
		{Ecosystem: "maven", Group: "org.example", Name: "core", Version: "1.2.3"},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	var doc bom
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.BomFormat != "CycloneDX" {
		t.Fatalf("bomFormat = %q, want CycloneDX", doc.BomFormat)
	}
	if doc.SpecVersion != "1.5" {
		t.Fatalf("specVersion = %q, want 1.5", doc.SpecVersion)
	}
	if doc.Version != 1 {
		t.Fatalf("version = %d, want 1", doc.Version)
	}
	if doc.Metadata.Component.Type != "application" {
		t.Fatalf("metadata.component.type = %q, want application", doc.Metadata.Component.Type)
	}
	if doc.Metadata.Component.Name != "my-project" {
		t.Fatalf("metadata.component.name = %q, want my-project", doc.Metadata.Component.Name)
	}
	if len(doc.Components) != 1 {
		t.Fatalf("len(components) = %d, want 1", len(doc.Components))
	}
	c := doc.Components[0]
	if c.Type != "library" || c.Name != "core" || c.Version != "1.2.3" {
		t.Fatalf("component = %+v", c)
	}
	if c.Purl != "pkg:maven/org.example/core@1.2.3" {
		t.Fatalf("purl = %q", c.Purl)
	}
	if c.BomRef != c.Purl {
		t.Fatalf("bom-ref = %q, want purl %q", c.BomRef, c.Purl)
	}
}

func TestGenerateOmitsEmptyVersion(t *testing.T) {
	out, err := Generate("p", []deps.Dependency{
		{Ecosystem: "go", Name: "example.com/mod"},
	})
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if strings.Contains(string(out), `"version":""`) {
		t.Fatalf("empty version should be omitted, got %s", out)
	}
}

func TestGenerateEmpty(t *testing.T) {
	out, err := Generate("p", nil)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	var doc bom
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if doc.Components == nil || len(doc.Components) != 0 {
		t.Fatalf("components = %v, want empty non-nil slice", doc.Components)
	}
	if !strings.Contains(string(out), `"components":[]`) {
		t.Fatalf("components should marshal as [], got %s", out)
	}
}
