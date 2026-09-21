package envagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"strings"
	"testing"
)

// tarGzBytes encodes a map of path->content as a gzip tar stream, exactly what
// a remote `tar -czf - ...` would emit.
func tarGzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUnpackTarGz verifies the in-memory archive decoder: nested relative paths
// survive cleaning, "." prefixes are stripped, directory entries are skipped and
// empty input yields an empty map.
func TestUnpackTarGz(t *testing.T) {
	data := tarGzBytes(t, map[string]string{
		"./target/surefire-reports/TEST-a.xml": "<testsuite/>",
		"./junit.xml":                          "<testsuite/>",
		"a/under/reports/ok.xml":               "x",
	})
	files, err := unpackTarGz(data)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"target/surefire-reports/TEST-a.xml": "<testsuite/>",
		"junit.xml":                          "<testsuite/>",
		"a/under/reports/ok.xml":             "x",
	}
	if len(files) != len(want) {
		t.Fatalf("unpacked %d files, want %d: %v", len(files), len(want), files)
	}
	for name, content := range want {
		if got := string(files[name]); got != content {
			t.Errorf("file %q = %q, want %q", name, got, content)
		}
	}
}

// TestUnpackTarGzTraversalDropped verifies path traversal entries (absolute or
// ..) are silently dropped so a misbehaving node cannot escape the destination.
func TestUnpackTarGzTraversalDropped(t *testing.T) {
	data := tarGzBytes(t, map[string]string{
		"../evil.txt":       "escaped",
		"/etc/passwd":       "escaped",
		"a/../../evil2.txt": "escaped",
		"target/report.xml": "good",
	})
	files, err := unpackTarGz(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("unpacked %d files, want only the safe one: %v", len(files), files)
	}
	if _, ok := files["target/report.xml"]; !ok {
		t.Errorf("safe file missing: %v", files)
	}
}

// TestUnpackTarGzEmptyInput treats an empty stream (no report produced) as an
// empty map instead of a parse error.
func TestUnpackTarGzEmptyInput(t *testing.T) {
	files, err := unpackTarGz(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("empty input unpacked to %d files", len(files))
	}
}

// TestPullArtifactsCommandAndOutput drives pullArtifactsExec through a stubbed
// RunSSH: the ssh argv must carry a tar command rooted at the node workDir with
// the requested relative paths, and the returned bytes unpack into the map.
func TestPullArtifactsCommandAndOutput(t *testing.T) {
	ctx := context.Background()
	old := RunSSH
	defer func() { RunSSH = old }()

	var gotArgs []string
	RunSSH = func(ctx context.Context, args ...string) (string, error) {
		gotArgs = args
		return string(tarGzBytes(t, map[string]string{
			"target/surefire-reports/TEST-ok.xml": "<testsuite><testcase name=\"a\"/></testsuite>",
		})), nil
	}

	files, err := pullArtifactsExec(ctx, "192.0.2.10", "runner", 22, "/srv/repos/demo", []string{"target/surefire-reports"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"192.0.2.10", "cd /srv/repos/demo &&", "tar -czf -", "target/surefire-reports", "2>/dev/null"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args missing %q: %v", want, gotArgs)
		}
	}
	body, ok := files["target/surefire-reports/TEST-ok.xml"]
	if !ok || !strings.Contains(string(body), "testcase") {
		t.Fatalf("unpacked files = %v", files)
	}
}

// TestPullArtifactsEmptyRelPaths short-circuits before running any ssh command.
func TestPullArtifactsEmptyRelPaths(t *testing.T) {
	ctx := context.Background()
	old := RunSSH
	defer func() { RunSSH = old }()
	RunSSH = func(ctx context.Context, args ...string) (string, error) {
		t.Fatal("RunSSH must not be invoked for an empty path list")
		return "", nil
	}
	files, err := pullArtifactsExec(ctx, "192.0.2.10", "runner", 22, "/srv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("empty relPaths returned %d files", len(files))
	}
}

// TestPullJUnitXMLCommandAndOutput drives pullJUnitXMLExec through a stubbed
// RunSSH: the ssh argv must carry the recursive find|xargs pipeline and the
// found junit XML files come back keyed by their relative path.
func TestPullJUnitXMLCommandAndOutput(t *testing.T) {
	ctx := context.Background()
	old := RunSSH
	defer func() { RunSSH = old }()

	var gotArgs []string
	RunSSH = func(ctx context.Context, args ...string) (string, error) {
		gotArgs = args
		return string(tarGzBytes(t, map[string]string{
			"DerivedData/Logs/Test/report.junit.xml": "<testsuites/>",
		})), nil
	}

	files, err := pullJUnitXMLExec(ctx, "192.0.2.10", "runner", 22, "/srv/ios")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " ")
	for _, want := range []string{"cd /srv/ios &&", "find . -iname '*junit*.xml' -print0", "xargs -0 tar -czf -"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args missing %q: %v", want, gotArgs)
		}
	}
	if got, ok := files["DerivedData/Logs/Test/report.junit.xml"]; !ok || !strings.Contains(string(got), "testsuites") {
		t.Fatalf("unpacked files = %v", files)
	}
}

// TestPullArtifactsSSHError propagates a genuine ssh failure so the caller can
// surface it (only the remote tar exit status is swallowed by `; true`).
func TestPullArtifactsSSHError(t *testing.T) {
	ctx := context.Background()
	old := RunSSH
	defer func() { RunSSH = old }()
	RunSSH = func(ctx context.Context, args ...string) (string, error) {
		return "ssh: connect to host failed", &execErr{}
	}
	if _, err := pullArtifactsExec(ctx, "198.51.100.9", "u", 22, "/srv", []string{"junit.xml"}); err == nil {
		t.Error("expected ssh error to propagate")
	}
}
