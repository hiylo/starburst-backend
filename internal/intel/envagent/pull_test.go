package envagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fakeConn is a stub SSHConnection used to satisfy the DialSSH result; the
// command sequence is recorded by overriding commandOn, so its NewSession is
// never exercised.
type fakeConn struct{}

func (fakeConn) NewSession() (*ssh.Session, error) { return nil, errors.New("stub: no session") }
func (fakeConn) Close() error                      { return nil }

// stubDialRun installs a recording DialSSH + commandOn pair for one test. The
// handler receives each issued remote command (find, then one cat per file) and
// returns its canned stdout.
func stubDialRun(t *testing.T, handler func(cmd string) ([]byte, error)) *[]string {
	t.Helper()
	oldDial := DialSSH
	oldRun := commandOn
	t.Cleanup(func() { DialSSH = oldDial; commandOn = oldRun })
	cmds := new([]string)
	DialSSH = func(ctx context.Context, host string, port int, user, auth string) (SSHConnection, error) {
		return fakeConn{}, nil
	}
	commandOn = func(ctx context.Context, c SSHConnection, command string) ([]byte, error) {
		*cmds = append(*cmds, command)
		return handler(command)
	}
	return cmds
}

// TestPullArtifactsUsesFindAndCat drives pullArtifactsExec through a recording
// command sequence: a remote find lists the files under the requested relative
// path, then each file is read back with cat (no tar on the node).
func TestPullArtifactsUsesFindAndCat(t *testing.T) {
	ctx := context.Background()
	var gotDialHost string
	var gotDialPort int
	var gotDialUser string
	cmds := stubDialRun(t, func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "cd ") {
			return []byte("target/surefire-reports/TEST-ok.xml\n"), nil
		}
		return []byte(`<testsuite><testcase name="a"/></testsuite>`), nil
	})
	DialSSH = func(ctx context.Context, host string, port int, user, auth string) (SSHConnection, error) {
		gotDialHost, gotDialPort, gotDialUser = host, port, user
		return fakeConn{}, nil
	}

	files, err := pullArtifactsExec(ctx, "192.0.2.10", "runner", 22, "/srv/repos/demo", []string{"target/surefire-reports"})
	if err != nil {
		t.Fatal(err)
	}
	if gotDialHost != "192.0.2.10" || gotDialPort != 22 || gotDialUser != "runner" {
		t.Errorf("dial args = %q/%d/%q, want 192.0.2.10/22/runner", gotDialHost, gotDialPort, gotDialUser)
	}
	if len(*cmds) == 0 {
		t.Fatal("no remote commands issued")
	}
	find := (*cmds)[0]
	for _, want := range []string{"cd /srv/repos/demo &&", "find", "target/surefire-reports", "-type f"} {
		if !strings.Contains(find, want) {
			t.Errorf("find command missing %q: %s", want, find)
		}
	}
	if len(*cmds) != 2 {
		t.Fatalf("expected find + 1 cat, got %d commands: %v", len(*cmds), *cmds)
	}
	if cat := (*cmds)[1]; !strings.Contains(cat, "cat") || !strings.Contains(cat, "target/surefire-reports/TEST-ok.xml") {
		t.Errorf("cat command wrong: %s", cat)
	}
	body, ok := files["target/surefire-reports/TEST-ok.xml"]
	if !ok || !strings.Contains(string(body), "testcase") {
		t.Fatalf("unpacked files = %v", files)
	}
}

// TestPullArtifactsEmptyRelPaths short-circuits before dialing or running any
// remote command.
func TestPullArtifactsEmptyRelPaths(t *testing.T) {
	ctx := context.Background()
	old := DialSSH
	defer func() { DialSSH = old }()
	DialSSH = func(ctx context.Context, host string, port int, user, auth string) (SSHConnection, error) {
		t.Fatal("DialSSH must not be invoked for an empty path list")
		return nil, nil
	}
	files, err := pullArtifactsExec(ctx, "192.0.2.10", "runner", 22, "/srv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("empty relPaths returned %d files", len(files))
	}
}

// TestPullJUnitXMLUsesFindAndCat drives pullJUnitXMLExec through a recording
// command sequence: a recursive find locates junit XML files under workDir and
// each is read back with cat.
func TestPullJUnitXMLUsesFindAndCat(t *testing.T) {
	ctx := context.Background()
	cmds := stubDialRun(t, func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "cd ") {
			return []byte("./DerivedData/Logs/Test/report.junit.xml\n"), nil
		}
		return []byte("<testsuites/>"), nil
	})

	files, err := pullJUnitXMLExec(ctx, "192.0.2.10", "runner", 22, "/srv/ios")
	if err != nil {
		t.Fatal(err)
	}
	if len(*cmds) == 0 {
		t.Fatal("no remote commands issued")
	}
	find := (*cmds)[0]
	for _, want := range []string{"cd /srv/ios &&", "find .", "-iname '*junit*.xml'", "-type f"} {
		if !strings.Contains(find, want) {
			t.Errorf("find command missing %q: %s", want, find)
		}
	}
	if got, ok := files["DerivedData/Logs/Test/report.junit.xml"]; !ok || !strings.Contains(string(got), "testsuites") {
		t.Fatalf("unpacked files = %v", files)
	}
}

// TestPullArtifactsErrorPropagates surfaces a genuine ssh failure so the caller
// can report it.
func TestPullArtifactsErrorPropagates(t *testing.T) {
	ctx := context.Background()
	stubDialRun(t, func(cmd string) ([]byte, error) {
		return nil, errors.New("ssh: connect to host failed")
	})
	if _, err := pullArtifactsExec(ctx, "198.51.100.9", "u", 22, "/srv", []string{"junit.xml"}); err == nil {
		t.Error("expected ssh error to propagate")
	}
}

// TestCatFilesDropsTraversal verifies catFiles drops absolute and .. paths so a
// misbehaving node cannot smuggle arbitrary files into the result map.
func TestCatFilesDropsTraversal(t *testing.T) {
	ctx := context.Background()
	old := commandOn
	defer func() { commandOn = old }()
	commandOn = func(ctx context.Context, c SSHConnection, command string) ([]byte, error) {
		return []byte("x"), nil
	}
	files, err := catFiles(ctx, fakeConn{}, "/srv", []string{"./target/ok.xml", "../evil.xml", "/etc/passwd", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want only the safe one: %v", len(files), files)
	}
	if _, ok := files["target/ok.xml"]; !ok {
		t.Errorf("safe file missing: %v", files)
	}
}
