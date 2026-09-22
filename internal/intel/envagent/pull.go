package envagent

import (
	"context"
	"path"
	"strings"
)

// PullArtifacts pulls a set of relative paths from a remote node by listing the
// regular files under them (a remote find) and streaming each back over its own
// cat session — a plain text transfer that needs no tar binary on the node.
// Returns a map of workDir-relative path -> file bytes. It is a package
// variable so tests can substitute a deterministic stub (a real pull needs a
// live node).
var PullArtifacts = pullArtifactsExec

// PullJUnitXML pulls every JUnit-style *.xml report found recursively under a
// remote node's workDir (the same discovery parseReport's collectJUnitXML
// performs locally for xctest). It is overridable in tests like RunSSH.
var PullJUnitXML = pullJUnitXMLExec

// pullArtifactsExec lists the regular files under each requested relative path
// with a remote find and reads them back one cat session at a time. Missing
// files / empty listings degrade to an empty map (a suite that failed before
// writing its reports simply yields nothing to parse). workDir/relPaths are
// reviewed constants or pass the caller's shell-metacharacter guard, so the
// interpolated shell strings stay injectable-free. authText authenticates the
// dial exactly like the command-execution path (see SSHClient), so a node that
// only accepts its configured credential accepts the report pull too. Output
// per cat session is tail-bounded by SSHCommand (outputTailLimit).
func pullArtifactsExec(ctx context.Context, host, user string, port int, authText, workDir string, relPaths []string) (map[string][]byte, error) {
	if len(relPaths) == 0 {
		return map[string][]byte{}, nil
	}
	c, err := DialSSH(ctx, host, port, user, authText)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	roots := make([]string, 0, len(relPaths))
	for _, rp := range relPaths {
		roots = append(roots, shellWord(rp))
	}
	listCmd := "cd " + shellWord(workDir) + " && find " + strings.Join(roots, " ") + " -type f -print 2>/dev/null; true"
	out, err := commandOn(ctx, c, listCmd)
	if err != nil {
		return nil, err
	}
	return catFiles(ctx, c, workDir, splitLines(string(out)))
}

// pullJUnitXMLExec lists junit XML files recursively under workDir with a
// remote find and reads each back via cat. Recursive discovery mirrors the
// local xctest path; pipeline failures and empty results both degrade to an
// empty map. authText authenticates the dial like pullArtifactsExec.
func pullJUnitXMLExec(ctx context.Context, host, user string, port int, authText, workDir string) (map[string][]byte, error) {
	c, err := DialSSH(ctx, host, port, user, authText)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	listCmd := "cd " + shellWord(workDir) + " && find . -iname '*junit*.xml' -type f -print 2>/dev/null; true"
	out, err := commandOn(ctx, c, listCmd)
	if err != nil {
		return nil, err
	}
	return catFiles(ctx, c, workDir, splitLines(string(out)))
}

// catFiles reads each workDir-relative path back over its own cat session and
// returns the path -> bytes map. Traversal entries (absolute / ..) are dropped
// defensively so a compromised node cannot smuggle arbitrary paths in.
func catFiles(ctx context.Context, c SSHConnection, workDir string, rels []string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, rel := range rels {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(rel, "./"))
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		remote := path.Join(workDir, clean)
		out, err := commandOn(ctx, c, "cat "+shellWord(remote)+"; true")
		if err != nil {
			return nil, err
		}
		files[clean] = out
	}
	return files, nil
}

// splitLines splits a remote command's stdout into its lines (an empty stream
// yields nil).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// shellWord returns s safe for interpolation into a remote shell command: a
// leading "~" stays unquoted so it expands to the remote home, and any
// space/quotes are single-quoted. Paths are reviewed constants or have passed
// the caller's shell-metacharacter guard, so no injection surface opens here.
func shellWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// commandOn runs one remote command on an existing ssh connection. It is a
// package variable so pull tests can record the issued find/cat command
// sequence and answer canned output without a live session.
var commandOn = commandOnSession

func commandOnSession(ctx context.Context, c SSHConnection, command string) ([]byte, error) {
	return SSHCommand(ctx, c, command)
}
