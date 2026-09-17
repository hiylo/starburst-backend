package schemainit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("src/main/resources/db/migration/V1__init.sql", "-- init")
	write("src/main/resources/db/migration/V2__data.sql", "-- data")
	write("db/changelog/changelog.sql", "-- changelog")
	write("src/main/resources/application.yml", "spring:\n  datasource:\n    url: jdbc:mysql://x")

	scripts := Discover(root)
	if len(scripts) != 3 {
		t.Fatalf("Discover = %d scripts, want 3: %+v", len(scripts), scripts)
	}
	want := []string{
		"db/changelog/changelog.sql",
		"src/main/resources/db/migration/V1__init.sql",
		"src/main/resources/db/migration/V2__data.sql",
	}
	for i, w := range want {
		if scripts[i].Rel != w {
			t.Errorf("scripts[%d].Rel = %q, want %q", i, scripts[i].Rel, w)
		}
	}
	// Flyway version order preserved (V1 before V2).
	if !(scripts[1].Rel < scripts[2].Rel) {
		t.Errorf("V1 not before V2: %+v", scripts)
	}
}

func TestDBName(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Two modules declare different datasources: deterministic pick = first alphabetically.
	write("a/src/main/resources/application.yml", "spring:\n  datasource:\n    url: jdbc:mysql://127.0.0.1:3306/app_a\n")
	write("b/src/main/resources/application.properties", "spring.datasource.url=jdbc:mysql://127.0.0.1:3306/app_b\n")

	if got := DBName(root); got != "app_a" {
		t.Errorf("DBName = %q, want app_a", got)
	}

	empty := t.TempDir()
	if got := DBName(empty); got != "" {
		t.Errorf("DBName(no config) = %q, want empty", got)
	}
}
