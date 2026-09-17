// Package schemainit discovers a project's database schema initialization
// scripts (Flyway migrations, Liquibase changelogs, plain SQL) so the test
// system can run them against a ready middleware container before integration
// tests. It is a pure static scanner: it only reports candidate files.
package schemainit

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Script is one discovered SQL file with a repo-relative path.
type Script struct {
	Rel string `json:"rel"` // path relative to the project root
	Abs string `json:"abs"`
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
