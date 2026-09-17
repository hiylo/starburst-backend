package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// runGit executes a git command in the given repo root and returns its stdout.
func runGit(root string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// intelResolveRepoPath joins repoRel under root and rejects any result that
// escapes root (e.g. a "../" traversal or an absolute path), so suggestions and
// backups sourced from the DB/LLM can never read or write outside the project
// working tree.
func intelResolveRepoPath(root, repoRel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(repoRel))
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("非法路径（绝对路径）: %s", repoRel)
	}
	abs := filepath.Join(root, clean)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("非法路径（越出工作目录）: %s", repoRel)
	}
	return abs, nil
}

// simpleTreeHash hashes a deterministic projection of the directory tree
// (relative paths + sizes) so staleness checks work without git.
func simpleTreeHash(root string) (string, error) {
	h := sha256.New()
	files := make([]string, 0)
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if err == nil && (filepath.Base(path) == ".git" || filepath.Base(path) == "target" ||
				filepath.Base(path) == "node_modules" || filepath.Base(path) == ".gradle") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	sort.Strings(files)
	for _, f := range files {
		// Skip VCS metadata and build output from the snapshot so local
		// edits are what invalidate caches, not churn from tooling.
		if shouldSkipSnapshot(f) {
			continue
		}
		_, _ = h.Write([]byte(f))
		_, _ = h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shouldSkipSnapshot reports whether a relative path is excluded from the
// staleness snapshot (VCS internals and build output).
func shouldSkipSnapshot(rel string) bool {
	lower := strings.ToLower(rel)
	for _, part := range []string{".git/", "node_modules/", "/target/", ".gradle/", "dist/"} {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}
