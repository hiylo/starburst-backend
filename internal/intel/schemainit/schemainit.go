// Package schemainit discovers a project's database schema initialization
// scripts (Flyway migrations, Liquibase changelogs, plain SQL) so the test
// system can run them against a ready middleware container before integration
// tests. It is a pure static scanner: it only reports candidate files.
package schemainit

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Script is one discovered SQL file with a repo-relative path.
type Script struct {
	Rel string `json:"rel"` // path relative to the project root
	Abs string `json:"abs"`
}

// reJDBCURL captures the database name from a Spring-style JDBC URL
// jdbc:mysql://host:port/dbname (also tolerates postgresql).
var reJDBCURL = regexp.MustCompile(`(?i)jdbc:(mysql|postgresql)://[^/]+/([A-Za-z0-9_]+)`)

// DBName derives the default database name a Spring project points at, by
// scanning application*.yml/yaml/properties for the datasource JDBC URL. It
// returns "" when no JDBC datasource is declared (scripts then run without a
// preselected schema, relying on CREATE DATABASE/USE inside the script).
func DBName(root string) string {
	seen := map[string]bool{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "target" ||
				name == ".gradle" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		base := info.Name()
		if !strings.HasPrefix(base, "application") && !strings.HasPrefix(base, "bootstrap") {
			return nil
		}
		switch filepath.Ext(base) {
		case ".yml", ".yaml", ".properties":
		default:
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		if m := reJDBCURL.FindSubmatch(data); m != nil {
			db := string(m[2])
			if !seen[db] {
				seen[db] = true
			}
		}
		return nil
	})
	if len(seen) == 0 {
		return ""
	}
	names := make([]string, 0, len(seen))
	for db := range seen {
		names = append(names, db)
	}
	sort.Strings(names)
	return names[0]
}

// Discover walks the repository for schema SQL files and returns them sorted by
// relative path (Flyway's V1__/V2__ prefix convention sorts version order).
func Discover(root string) []Script {
	seen := map[string]bool{}
	out := []Script{}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "target" ||
				name == ".gradle" || name == "build_artifacts" || name == "dist" ||
				name == ".idea" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".sql") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			return nil
		}
		seen[rel] = true
		out = append(out, Script{Rel: rel, Abs: path})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out
}
