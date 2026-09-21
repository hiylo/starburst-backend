package envagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"path"
	"strings"
)

// PullArtifacts pulls a set of relative paths from a remote node by tarring
// them to stdout over SSH and unpacking the gzip stream in memory, returning a
// map of relative path -> file bytes. It is a package variable so tests can
// substitute a deterministic stub (a real pull needs a live node).
var PullArtifacts = pullArtifactsExec

// PullJUnitXML pulls every JUnit-style *.xml report found recursively under a
// remote node's workDir (the same discovery parseReport's collectJUnitXML
// performs locally for xctest), delivered as a single find|xargs tar pipeline.
// It is overridable in tests like RunSSH.
var PullJUnitXML = pullJUnitXMLExec

// pullArtifactsExec is the default artifact pull: run
// `tar -czf - -C <workDir> <relPaths...> 2>/dev/null` on the node. stdout is
// the gzip tar byte stream; stderr is dropped remotely so a tar warning cannot
// corrupt the archive. The trailing `; true` keeps a missing glob from
// surfacing as a terminal execution error — a suite that failed before writing
// its reports simply yields an empty (parseable) result. workDir/relPaths are
// reviewed constants or pass the caller's shell-metacharacter guard, so the
// interpolated shell string stays injectable-free.
func pullArtifactsExec(ctx context.Context, host, user string, port int, workDir string, relPaths []string) (map[string][]byte, error) {
	if len(relPaths) == 0 {
		return map[string][]byte{}, nil
	}
	command := "cd " + workDir + " && tar -czf - " + strings.Join(relPaths, " ") + " 2>/dev/null; true"
	return runTarPull(ctx, host, user, port, command)
}

// pullJUnitXMLExec is the recursive JUnit pull used for xctest reports: a
// remote find locates junit XML files under workDir and feeds them to tar.
// Pipeline failures and empty results both degrade to an empty map (the local
// parseReport path already tolerates missing reports).
func pullJUnitXMLExec(ctx context.Context, host, user string, port int, workDir string) (map[string][]byte, error) {
	command := "cd " + workDir + " && find . -iname '*junit*.xml' -print0 2>/dev/null | xargs -0 tar -czf - 2>/dev/null; true"
	return runTarPull(ctx, host, user, port, command)
}

// runTarPull runs a remote command whose stdout is a gzip tar byte stream and
// unpacks it into a path->bytes map. The command string is assembled from
// reviewed inputs only.
func runTarPull(ctx context.Context, host, user string, port int, command string) (map[string][]byte, error) {
	args := SSHCommandArgs(host, user, port, command)
	out, err := RunSSH(ctx, args...)
	if err != nil {
		return nil, err
	}
	return unpackTarGz([]byte(out))
}

// unpackTarGz decodes a gzip tar byte stream into a map keyed by cleaned
// relative path. Directory entries are skipped and path-traversal entries
// (absolute or ..) are dropped defensively, so a compromised/misbehaving node
// cannot have its archives written outside the destination directory when the
// map is later materialized.
func unpackTarGz(data []byte) (map[string][]byte, error) {
	if len(data) == 0 {
		return map[string][]byte{}, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		rel := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if rel == "." || path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		files[rel] = content
	}
	return files, nil
}
