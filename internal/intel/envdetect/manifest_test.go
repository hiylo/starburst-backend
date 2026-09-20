package envdetect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		source  string
		reqs    []Requirement
		scripts []InitScript
	}{
		{
			name: "services and init scripts",
			data: `services:
  - service: mysql
    version: "8"
    port: 3306
  - service: redis
  - service: jdk
    version: "17"
init_scripts:
  - service: mysql
    script: db/migrations/schema.sql
`,
			source: "intel-env.yaml",
			reqs: []Requirement{
				{Service: "mysql", Category: "middleware", Version: "8", Source: "intel-env.yaml"},
				{Service: "redis", Category: "middleware", Source: "intel-env.yaml"},
				{Service: "jdk", Category: "toolchain", Version: "17", Source: "intel-env.yaml"},
			},
			scripts: []InitScript{
				{Service: "mysql", Script: "db/migrations/schema.sql"},
			},
		},
		{
			name:   "missing fields are zero values",
			data:   "services:\n  - service: mysql\n  - version: \"8\"\n",
			source: "intel-env.yaml",
			reqs: []Requirement{
				{Service: "mysql", Category: "middleware", Source: "intel-env.yaml"},
				{Category: "middleware", Version: "8", Source: "intel-env.yaml"},
			},
			scripts: []InitScript{},
		},
		{
			name:    "missing sections yield empty slices",
			data:    "unrelated: true\n",
			source:  "intel-env.yaml",
			reqs:    []Requirement{},
			scripts: []InitScript{},
		},
		{
			name:    "empty yaml yields empty slices",
			data:    "",
			source:  "intel-env.yaml",
			reqs:    []Requirement{},
			scripts: []InitScript{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs, scripts, err := ParseManifest([]byte(tt.data), tt.source)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reqs, tt.reqs) {
				t.Fatalf("ParseManifest() reqs = %#v, want %#v", reqs, tt.reqs)
			}
			if !reflect.DeepEqual(scripts, tt.scripts) {
				t.Fatalf("ParseManifest() scripts = %#v, want %#v", scripts, tt.scripts)
			}
		})
	}
}

func TestParseManifestMalformed(t *testing.T) {
	if _, _, err := ParseManifest([]byte("services:\n  - service: [unclosed"), "intel-env.yaml"); err == nil {
		t.Fatal("ParseManifest() should return an error for malformed YAML")
	}
}

func TestManifestAt(t *testing.T) {
	root := t.TempDir()
	if _, ok := ManifestAt(root); ok {
		t.Fatal("ManifestAt() should not find a manifest in an empty root")
	}
	if err := os.WriteFile(filepath.Join(root, manifestFileName), []byte("services: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, ok := ManifestAt(root)
	if !ok {
		t.Fatal("ManifestAt() should find intel-env.yaml at root")
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != manifestFileName {
		t.Fatalf("ManifestAt() = %q, want absolute path to %s", path, manifestFileName)
	}

	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, manifestFileName), []byte("services: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ManifestAt(root); !ok {
		t.Fatal("ManifestAt() must check only the top level, not recurse")
	}
}

func TestMergeWithDetect(t *testing.T) {
	detected := []Requirement{
		{Service: "mysql", Category: "middleware", Version: "5.7", Source: "pom.xml"},
		{Service: "redis", Category: "middleware", Source: "application.yml"},
		{Service: "jdk", Category: "toolchain", Version: "11", Source: "pom.xml"},
	}
	manifest := []Requirement{
		{Service: "mysql", Category: "middleware", Version: "8", Source: "intel-env.yaml"},
		{Service: "nacos", Category: "middleware", Source: "intel-env.yaml"},
	}
	got := MergeWithDetect(detected, manifest)
	want := []Requirement{
		{Service: "jdk", Category: "toolchain", Version: "11", Source: "pom.xml"},
		{Service: "mysql", Category: "middleware", Version: "8", Source: "intel-env.yaml"},
		{Service: "nacos", Category: "middleware", Source: "intel-env.yaml"},
		{Service: "redis", Category: "middleware", Source: "application.yml"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeWithDetect() = %#v, want %#v", got, want)
	}
}

func TestMergeWithDetectEmpty(t *testing.T) {
	if got := MergeWithDetect(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("MergeWithDetect(nil, nil) = %#v, want empty slice", got)
	}
	detected := []Requirement{{Service: "redis", Category: "middleware"}}
	if got := MergeWithDetect(detected, nil); !reflect.DeepEqual(got, detected) {
		t.Fatalf("MergeWithDetect(detected, nil) = %#v, want %#v", got, detected)
	}
}

func TestCategoryFor(t *testing.T) {
	middleware := []string{"mysql", "redis", "nacos", "rabbitmq", "elasticsearch", "minio", "mongo", "postgresql", "kafka", "unknown-service"}
	for _, svc := range middleware {
		if got := categoryFor(svc); got != "middleware" {
			t.Errorf("categoryFor(%q) = %q, want middleware", svc, got)
		}
	}
	toolchain := []string{"jdk", "gradle", "node", "go", "android-sdk", "xcode", "python", "maven"}
	for _, svc := range toolchain {
		if got := categoryFor(svc); got != "toolchain" {
			t.Errorf("categoryFor(%q) = %q, want toolchain", svc, got)
		}
	}
}
