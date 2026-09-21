// Package bff deterministically extracts Node/BFF backend contracts: GraphQL
// schema types and operations plus REST routes (Express/Fastify/Nest). Like the
// Android/iOS/Web scanners it is deliberately regex/text based — no LLM, no
// third-party parser — and every extracted row carries a verifiable
// source_file:line provenance. GraphQL operation types (Query/Mutation/
// Subscription) become endpoints with Method QUERY/MUTATION/SUBSCRIPTION and
// Path = field name, matching the Java Spring GraphQL extraction in
// internal/intel/java.go.
package bff

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hiylo/starburst-backend/internal/store"
)

// nodeEntryNames are the conventional Node service entry files looked for
// directly under a module and under its src/ directory.
var nodeEntryNames = []string{
	"server.ts", "server.js", "app.ts", "app.js", "main.ts", "main.js",
	"index.ts", "index.js",
}

// backendMarkers are the package-manifest dependency names that mark a Node
// module as backend/BFF. "apollo-server" is matched as a prefix so the whole
// apollo-server-express / apollo-server-core family counts.
var backendMarkers = []string{
	"express", "fastify", "@nestjs/core",
	"apollo-server", "@apollo/server",
	"@graphql-yoga", "graphql-yoga", "mercurius",
}

// LooksLikeBackend reports whether a package.json-owned directory is a Node
// backend/BFF rather than a pure Web (Vue/React) frontend. Any of the following
// holds:
//
//  1. a Node service entry file exists (server/app/main/index.ts/js, directly or
//     under src/);
//  2. package.json dependencies/devDependencies declare a backend framework
//     (express, fastify, @nestjs/core, apollo-server*, @apollo/server,
//     @graphql-yoga, graphql-yoga, mercurius);
//  3. the directory tree contains a GraphQL schema file (*.graphql, *.gql,
//     *.graphqls).
//
// It is a pure function (plus deterministic file existence checks) so it can be
// unit-tested standalone.
func LooksLikeBackend(dir string, pkg map[string]any) bool {
	if hasNodeEntryFile(dir) {
		return true
	}
	if pkgHasBackendMarker(pkg) {
		return true
	}
	return hasGraphQLSchema(dir)
}

// hasNodeEntryFile reports whether any conventional Node service entry file
// exists directly under dir or under dir/src.
func hasNodeEntryFile(dir string) bool {
	for _, n := range nodeEntryNames {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
		if _, err := os.Stat(filepath.Join(dir, "src", n)); err == nil {
			return true
		}
	}
	return false
}

// pkgHasBackendMarker reports whether a parsed package.json (map form) declares
// a backend framework name in dependencies or devDependencies.
func pkgHasBackendMarker(pkg map[string]any) bool {
	if pkg == nil {
		return false
	}
	for _, key := range []string{"dependencies", "devDependencies"} {
		m, ok := pkg[key].(map[string]any)
		if !ok {
			continue
		}
		for name := range m {
			if isBackendPackage(strings.ToLower(name)) {
				return true
			}
		}
	}
	return false
}

// isBackendPackage reports whether a normalized package name matches a declared
// backend marker.
func isBackendPackage(name string) bool {
	for _, marker := range backendMarkers {
		if marker == "apollo-server" {
			if strings.HasPrefix(name, marker) {
				return true
			}
			continue
		}
		if name == marker {
			return true
		}
	}
	return false
}

// hasGraphQLSchema reports whether any *.graphql, *.gql or *.graphqls schema
// file exists anywhere under dir (build/vendor output excluded).
func hasGraphQLSchema(dir string) bool {
	found := false
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			if skipBffDir(info.Name()) && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".graphql", ".gql", ".graphqls":
			found = true
		}
		return nil
	})
	return found
}

// ScanBFF scans dir (recursively) for GraphQL schema sources (.graphql/.gql/
// .graphqls files, `gql`...` / `graphql`...` template literals and
// .graphql('...') literals in TS/JS) and REST routes (Express/Fastify/Nest in
// .ts/.tsx/.js/.mjs files), returning entity mappings and endpoint contracts
// deduplicated and deterministically sorted. SourceFile paths are relative to
// dir. .git, build, node_modules, dist and test/build output directories are
// skipped.
func ScanBFF(dir string) ([]*store.IntelEntity, []*store.IntelEndpoint, error) {
	var graphqlFiles, jsFiles []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipBffDir(info.Name()) && path != dir {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".graphql", ".gql", ".graphqls":
			graphqlFiles = append(graphqlFiles, path)
		case ".ts", ".tsx", ".js", ".mjs":
			jsFiles = append(jsFiles, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	ents := make([]*store.IntelEntity, 0)
	eps := make([]*store.IntelEndpoint, 0)
	for _, f := range graphqlFiles {
		data, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		e, p := parseGraphQLBlock(relPath(dir, f), string(data), 0)
		ents = append(ents, e...)
		eps = append(eps, p...)
	}
	for _, f := range jsFiles {
		rel := relPath(dir, f)
		for _, b := range graphQLBlocksInFile(f) {
			e, p := parseGraphQLBlock(rel, b.text, b.line)
			ents = append(ents, e...)
			eps = append(eps, p...)
		}
		eps = append(eps, scanRESTFile(rel, f)...)
	}

	ents = dedupeBFFEntities(ents)
	eps = dedupeBFFEndpoints(eps)
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].Entity != ents[j].Entity {
			return ents[i].Entity < ents[j].Entity
		}
		return ents[i].ColumnName < ents[j].ColumnName
	})
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].Path != eps[j].Path {
			return eps[i].Path < eps[j].Path
		}
		return eps[i].Method < eps[j].Method
	})
	return ents, eps, nil
}

// skipBffDir reports whether a directory name is generated/vendored output that
// must not be scanned for Node contracts.
func skipBffDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "build", ".gradle", "build_artifacts", "coverage":
		return true
	}
	return false
}

// relPath renders a path relative to dir with forward slashes (falls back to
// the absolute path when file is outside dir).
func relPath(dir, file string) string {
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		return filepath.ToSlash(file)
	}
	return filepath.ToSlash(rel)
}

// dedupeBFFEntities collapses entity rows sharing (Entity, TableName,
// ColumnName, FieldType), keeping the first occurrence.
func dedupeBFFEntities(ents []*store.IntelEntity) []*store.IntelEntity {
	seen := make(map[string]bool)
	out := make([]*store.IntelEntity, 0, len(ents))
	for _, e := range ents {
		key := e.Entity + "|" + e.TableName + "|" + e.ColumnName + "|" + e.FieldType
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

// dedupeBFFEndpoints collapses endpoints sharing a (Method, Path) key, keeping
// the first occurrence.
func dedupeBFFEndpoints(eps []*store.IntelEndpoint) []*store.IntelEndpoint {
	seen := make(map[string]bool)
	out := make([]*store.IntelEndpoint, 0, len(eps))
	for _, ep := range eps {
		key := ep.Method + " " + ep.Path
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ep)
	}
	return out
}
