package server

// Source-workspace helpers: resolving a project's on-disk roots, keeping git
// clones in sync, and the name/snapshot hashing the analysis passes rely on.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

import (
	"github.com/hiylo/starburst-backend/internal/store"
)

// projectRoot returns the local working directory of a project: LocalPath for
// source=local, or the repos_dir cache clone for source=git. The cache clone is
// materialized right here by ensureGitClone: on first use it runs
// `git clone --depth 1 --no-single-branch` into <repos_dir>/<safe-name>, it
// re-clones when the cached origin URL no longer matches, and it refreshes an
// existing clone (fetch + hard reset) when git_ref changes. repos_dir has no
// default value: when the setting is unset or unreadable the caller gets an
// explicit "not configured" error instead of a usable path.
func (s *Server) projectRoot(ctx context.Context, p *store.IntelProject) (string, error) {
	if p.Source == "local" && p.LocalPath != "" {
		return p.LocalPath, nil
	}
	reposDir, err := s.store.GetSetting(ctx, "intel.repos_dir")
	if err != nil || reposDir == "" {
		// 未配置时回退到默认缓存路径（~/.local/share/starburst-backend/intel-repos），
		// 使 git 项目无需手工配置即可分析。
		reposDir = intelDefaultReposDir()
	}
	target := filepath.Join(reposDir, safeName(p.Name))
	if err := s.ensureGitClone(ctx, p, target); err != nil {
		return "", err
	}
	return target, nil
}

// intelDefaultReposDir returns the default git-clone cache directory used when
// the intel.repos_dir setting is unset.
func intelDefaultReposDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".local", "share", "starburst-backend", "intel-repos")
	}
	return filepath.Join(os.TempDir(), "starburst-intel-repos")
}

// intelReposDir resolves the effective git-clone cache directory (setting or
// the default fallback).
func (s *Server) intelReposDir(ctx context.Context) string {
	if d, err := s.store.GetSetting(ctx, "intel.repos_dir"); err == nil && d != "" {
		return d
	}
	return intelDefaultReposDir()
}

// intelSourcePrefix builds the unique rel-path token prefix for modules that
// belong to an associated source repo, e.g. "@android/". The end name is
// sanitized; a numeric id fallback keeps it unique when the end name is blank.
func intelSourcePrefix(src *store.IntelProjectSource) string {
	token := strings.TrimSpace(src.EndName)
	if token == "" {
		token = strconv.FormatInt(src.ID, 10)
	}
	var b strings.Builder
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return "@" + b.String() + "/"
}

// intelSourcesEqual reports whether two associated-source lists are equivalent,
// ignoring database ids (the id is not part of the PUT payload; the comparison
// is by the source-defining fields only).
func intelSourcesEqual(a, b []*store.IntelProjectSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.EndName != y.EndName || x.Source != y.Source ||
			x.LocalPath != y.LocalPath || x.GitURL != y.GitURL || x.GitRef != y.GitRef {
			return false
		}
	}
	return true
}

// resolveIntelSourceRoot resolves the working directory of an associated source
// repo: the local path when source=local, or a per-source git clone under the
// repos_dir cache (distinct from the main project clone).
func (s *Server) resolveIntelSourceRoot(ctx context.Context, p *store.IntelProject, src *store.IntelProjectSource) (string, error) {
	if src.Source == "local" && src.LocalPath != "" {
		return src.LocalPath, nil
	}
	reposDir, err := s.store.GetSetting(ctx, "intel.repos_dir")
	if err != nil || reposDir == "" {
		reposDir = intelDefaultReposDir()
	}
	target := filepath.Join(reposDir, safeName(p.Name)+"-src-"+strconv.FormatInt(src.ID, 10))
	clone := &store.IntelProject{Name: p.Name, GitURL: src.GitURL, GitRef: src.GitRef}
	if err := s.ensureGitClone(ctx, clone, target); err != nil {
		return "", err
	}
	return target, nil
}

// sourceModuleRoot resolves the working directory for a module whose rel_path
// may belong to an associated source repo ("@end/..."). ok is false when the
// module lives in the project's primary root.
func (s *Server) sourceModuleRoot(ctx context.Context, p *store.IntelProject, sources []*store.IntelProjectSource, relPath string) (string, string, bool) {
	if !strings.HasPrefix(relPath, "@") {
		return "", "", false
	}
	slash := strings.IndexByte(relPath, '/')
	if slash <= 0 {
		return "", "", false
	}
	token := relPath[1:slash]
	for _, src := range sources {
		prefixToken := strings.TrimSuffix(strings.TrimPrefix(intelSourcePrefix(src), "@"), "/")
		if token != prefixToken {
			continue
		}
		root, err := s.resolveIntelSourceRoot(ctx, p, src)
		if err != nil {
			return "", "", false
		}
		return root, relPath[slash+1:], true
	}
	return "", "", false
}

// ensureGitClone clones a git project into target on first use, re-clones when
// the cached clone's origin no longer matches the requested URL, and refreshes
// an existing clone to the requested ref (branch switching included). It shells
// out to git with explicit argv (no shell) so a user-supplied URL cannot inject
// commands, and uses `--` to terminate option parsing on the URL positional arg.
func (s *Server) ensureGitClone(ctx context.Context, p *store.IntelProject, target string) error {
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		return s.cloneGitRepo(ctx, p, target)
	}
	// 已存在 clone：校验 origin 与当前 URL 一致，避免改 URL 后仍分析旧仓。
	origin, _ := runGit(target, "config", "--get", "remote.origin.url")
	if strings.TrimSpace(origin) != "" && strings.TrimSpace(origin) != p.GitURL {
		log.Printf("intel project %s origin changed (%q -> %q), re-cloning", target, strings.TrimSpace(origin), p.GitURL)
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		return s.cloneGitRepo(ctx, p, target)
	}
	// Refresh: fetch the requested ref (explicitly, so switching branches works
	// even though the clone is shallow) and hard-reset so re-analyze sees HEAD.
	fetch := exec.CommandContext(ctx, "git", "fetch", "--depth", "1", "origin")
	if ref := p.GitRef; ref != "" {
		fetch.Args = append(fetch.Args, ref)
	}
	fetch.Dir = target
	if env, cleanup := gitAuthEnv(p); len(env) > 0 {
		defer cleanup()
		fetch.Env = append(os.Environ(), env...)
	}
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ref := p.GitRef
	if ref == "" {
		ref = "HEAD"
	}
	reset := exec.CommandContext(ctx, "git", "reset", "--hard", "origin/"+ref)
	reset.Dir = target
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cloneGitRepo performs a shallow, all-branches clone so later GitRef changes
// can be fetched without an origin URL mismatch. The URL is passed as a
// positional arg after `--` so an option-prefixed URL cannot be smuggled.
// Project credentials (token / ssh key) are injected via the environment.
func (s *Server) cloneGitRepo(ctx context.Context, p *store.IntelProject, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	args := []string{"clone", "--depth", "1", "--no-single-branch"}
	if p.GitRef != "" {
		args = append(args, "--branch", p.GitRef)
	}
	args = append(args, "--", p.GitURL, target)
	cmd := exec.CommandContext(ctx, "git", args...)
	env, cleanup := gitAuthEnv(p)
	defer cleanup()
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitAuthEnv builds the environment needed to authenticate a git clone/fetch
// using the project's decrypted credentials: HTTP token via a temp GIT_ASKPASS
// script, SSH private key via GIT_SSH_COMMAND with a temp identity file. Both
// temp files are removed by the returned cleanup func.
func gitAuthEnv(p *store.IntelProject) ([]string, func()) {
	var env []string
	cleanups := make([]func(), 0, 2)
	if p.GitToken != "" {
		if f, err := os.CreateTemp("", "git-askpass-*"); err == nil {
			_ = f.Chmod(0o700)
			escaped := strings.ReplaceAll(p.GitToken, "'", `'\''`)
			_, _ = f.WriteString("#!/bin/sh\nprintf '%s\\n' '" + escaped + "'\n")
			_ = f.Close()
			env = append(env, "GIT_ASKPASS="+f.Name(), "GIT_TERMINAL_PROMPT=0")
			path := f.Name()
			cleanups = append(cleanups, func() { _ = os.Remove(path) })
		}
	}
	if p.SSHKey != "" {
		if f, err := os.CreateTemp("", "git-key-*"); err == nil {
			_ = f.Chmod(0o600)
			_, _ = f.WriteString(p.SSHKey)
			_ = f.Close()
			env = append(env, "GIT_SSH_COMMAND=ssh -i "+f.Name()+" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes")
			path := f.Name()
			cleanups = append(cleanups, func() { _ = os.Remove(path) })
		}
	}
	return env, func() {
		for _, c := range cleanups {
			c()
		}
	}
}

func errNotDir(root string) error { return &pathErr{msg: "project path is not a directory: " + root} }

type pathErr struct{ msg string }

func (e *pathErr) Error() string { return e.msg }

func errSettingMissing(msg string) error { return &pathErr{msg: msg} }

// safeName sanitizes a project name for use as a directory component.
func safeName(name string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", " ", "_", "..", "__")
	return repl.Replace(strings.TrimSpace(name))
}

// deriveProjectName produces a deterministic project name from a path or git URL.
func deriveProjectName(localPath, gitURL string) string {
	raw := localPath
	if raw == "" {
		raw = gitURL
	}
	raw = strings.TrimRight(raw, "/")
	base := filepath.Base(raw)
	if base == "." || base == "/" || base == "" {
		return "project"
	}
	return base
}

// snapshotSHA produces a deterministic snapshot identifier for cache-validation:
// HEAD sha for git repos, otherwise a hash of the directory listing.
func snapshotSHA(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return gitHeadSHA(root)
	}
	return dirHash(root)
}

// gitHeadSHA reads HEAD via git plumbing. Implemented as a defensive fallback
// that returns "" when git is unavailable (M2 fills delta logic).
func gitHeadSHA(root string) (string, error) {
	out, err := runGit(root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// dirHash hashes a stable projection of the directory tree for staleness checks.
func dirHash(root string) (string, error) {
	return simpleTreeHash(root)
}
