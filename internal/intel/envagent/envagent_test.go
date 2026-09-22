package envagent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestContainerRunArgs(t *testing.T) {
	args := containerRunArgs(3, "mysql", "mysql:8", 3306, 3306,
		[]string{"MYSQL_ROOT_PASSWORD=abc"}, nil)
	joined := strings.Join(args, " ")
	for _, want := range []string{"run", "-d", "--name", "intel-3-mysql",
		"--restart=unless-stopped", "-p", "3306:3306", "-e", "MYSQL_ROOT_PASSWORD=abc", "mysql:8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("containerRunArgs missing %q in: %s", want, joined)
		}
	}

	// A host port differing from the container port (collision offset).
	off := containerRunArgs(3, "redis", "redis:7", 16379, 6379,
		[]string{"REDIS_PASSWORD=pw"}, []string{"redis-server", "--requirepass", "pw"})
	oj := strings.Join(off, " ")
	if !strings.Contains(oj, "-p 16379:6379") {
		t.Errorf("redis args missing offset mapping in: %s", oj)
	}

	redis := containerRunArgs(3, "redis", "redis:7", 6379, 6379,
		[]string{"REDIS_PASSWORD=pw"}, []string{"redis-server", "--requirepass", "pw"})
	rj := strings.Join(redis, " ")
	for _, want := range []string{"-p", "6379:6379", "redis:7", "redis-server", "--requirepass", "pw"} {
		if !strings.Contains(rj, want) {
			t.Errorf("redis args missing %q in: %s", want, rj)
		}
	}
}

func TestContainerName(t *testing.T) {
	if got := ContainerName(42, "mysql"); got != "intel-42-mysql" {
		t.Errorf("ContainerName = %q, want intel-42-mysql", got)
	}
}

func TestProbeMissingWhenDockerReadyNoContainer(t *testing.T) {
	ctx := context.Background()
	old := RunDocker
	defer func() { RunDocker = old }()
	RunDocker = func(ctx context.Context, args ...string) (string, error) {
		if args[0] == "info" {
			return "24.0.0", nil
		}
		if args[0] == "ps" {
			return "", nil
		}
		return "", nil
	}
	status, provider, name, _, err := Probe(ctx, 3, "mysql", "middleware", "8")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusMissing || provider != ProviderContainer || name != "intel-3-mysql" {
		t.Errorf("Probe = %q/%q/%q, want missing/container/intel-3-mysql", status, provider, name)
	}
}

func TestProbeUnsupportedWhenNoDocker(t *testing.T) {
	ctx := context.Background()
	old := RunDocker
	defer func() { RunDocker = old }()
	RunDocker = func(ctx context.Context, args ...string) (string, error) {
		return "docker: command not found", &execErr{}
	}
	status, _, _, _, err := Probe(ctx, 3, "mysql", "middleware", "8")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusUnsupported {
		t.Errorf("Probe = %q, want unsupported", status)
	}
}

type execErr struct{}

func (e *execErr) Error() string { return "exec error" }

func TestRandomPassword(t *testing.T) {
	p := RandomPassword(16)
	if len(p) != 16 {
		t.Errorf("RandomPassword(16) length = %d, want 16", len(p))
	}
	if p == RandomPassword(16) {
		t.Error("two passwords should differ")
	}
}

func TestToolchainSet(t *testing.T) {
	for _, svc := range []string{"jdk", "node", "go", "gradle", "maven", "android-sdk"} {
		if !Toolchain(svc) {
			t.Errorf("Toolchain(%q) = false, want true", svc)
		}
	}
	if Toolchain("mysql") {
		t.Error("mysql should not be a toolchain")
	}
}

func TestToolchainInstallCommand(t *testing.T) {
	cases := []struct {
		service, version string
		want             string
	}{
		{"jdk", "17", "openjdk-17-jdk"},
		{"jdk", "1.8", "openjdk-8-jdk"},
		{"node", "", "nodejs"},
		{"go", "", "golang"},
		{"gradle", "", "gradle"},
		{"maven", "", "maven"},
	}
	for _, c := range cases {
		argv := ToolchainInstallCommand(c.service, c.version)
		if len(argv) == 0 {
			t.Errorf("ToolchainInstallCommand(%q,%q) empty", c.service, c.version)
			continue
		}
		found := false
		for _, a := range argv {
			if a == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("ToolchainInstallCommand(%q,%q) missing %q: %v", c.service, c.version, c.want, argv)
		}
	}
	if ToolchainInstallCommand("android-sdk", "") != nil {
		t.Error("android-sdk should have no auto-install command")
	}
}

// TestTailBufferDropsOldest verifies the bounded tail buffer keeps only the
// last limit bytes (dropping the oldest), so chatty command output cannot
// balloon memory.
func TestTailBufferDropsOldest(t *testing.T) {
	var tb tailBuffer
	tb.limit = 10
	if _, err := tb.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if got := tb.buf.String(); got != "0123456789" {
		t.Errorf("under-limit write = %q", got)
	}
	if _, err := tb.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if got := tb.buf.String(); got != "6789abcdef" {
		t.Errorf("over-limit write = %q, want last-10 tail", got)
	}
}

// TestHostKeyCallbackFailClosed verifies known_hosts handling fails closed:
// an unreadable/absent file rejects the connection with an error instead of
// silently disabling host-key verification.
func TestHostKeyCallbackFailClosed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-known_hosts")
	if _, err := hostKeyCallback(missing); err == nil {
		t.Error("hostKeyCallback on a missing file must fail closed")
	}
}

func TestParseSSHArgs(t *testing.T) {
	host, user, command, port, timeout, err := parseSSHArgs(SSHCommandArgs("192.0.2.50", "runner", 2222,
		"cd /srv && go test -count=1 ./..."))
	if err != nil {
		t.Fatal(err)
	}
	if host != "192.0.2.50" || user != "runner" || port != 2222 {
		t.Errorf("parseSSHArgs = %q/%q/%d, want 192.0.2.50/runner/2222", host, user, port)
	}
	if command != "cd /srv && go test -count=1 ./..." {
		t.Errorf("command = %q", command)
	}
	if timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", timeout)
	}

	// Bare host (no user) defaults to port 22; ConnectTimeout is honoured.
	host, user, _, port, timeout, err = parseSSHArgs([]string{
		"-p", "22", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes", "node.int", "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if host != "node.int" || user != "" || port != 22 || timeout != 5*time.Second {
		t.Errorf("parseSSHArgs(bare) = %q/%q/%d/%v, want node.int//22/5s", host, user, port, timeout)
	}
}

// testSSHKey is a throwaway ed25519 key generated only for these tests; it is
// not a production credential.
const testSSHKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACC448ylQZEA5Zhv695fm/r4Q9LRdi0peAedv8pRFEiu+AAAAKhhOy5sYTsu
bAAAAAtzc2gtZWQyNTUxOQAAACC448ylQZEA5Zhv695fm/r4Q9LRdi0peAedv8pRFEiu+A
AAAEBUvYILe+TKCT7EO7k3c9h2EXDNV4pjLt18wnp5qMdmCLjjzKVBkQDlmG/r3l+b+vhD
0tF2LSl4B52/ylEUSK74AAAAH3Jvb3RAaGl5bG8tUHJlY2lzaW9uLTc5MjAtVG93ZXIBAg
MEBQY=
-----END OPENSSH PRIVATE KEY-----
`

// testSSHKeyEncrypted is the same throwaway ed25519 key, passphrase-protected
// with "secret-pass"; it cannot be authenticated through RemoteNode.Auth.
const testSSHKeyEncrypted = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAACmFlczI1Ni1jdHIAAAAGYmNyeXB0AAAAGAAAABCV030X6F
voDS1UMWT+aMSyAAAAGAAAAAEAAAAzAAAAC3NzaC1lZDI1NTE5AAAAICdXBQZGTgYN5dbY
xIrplvBzgUYNFcZ/+4fiFwyvjNwBAAAAsE/nGWob+4ucWvCN3wx36UvbUGrQ7xcNzajMyH
Tkp9dtJtaTbxQdF05i1XfjyBkKzAKtEKCweq2+lC52gfrB2oMK+FRwyhJ4JcPCYeyH1qyT
PT9qC2RjX8EsztfXjFJOGULUVCxNfnPx/cbM2RI+9OXUd/A8k6SwvaXvVmTZ7BGM+qJmxk
zPjBQrrQ5pXdMo0QxZdSkcsfe224cUTuB+RLR03mMCChklTLuGv8TTTL3f
-----END OPENSSH PRIVATE KEY-----
`

func TestSSHAuthMethods(t *testing.T) {
	// A private key text selects public-key signer auth.
	methods, err := sshAuthMethods(testSSHKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) == 0 {
		t.Fatal("private key produced no auth methods")
	}
	if _, ok := methods[0].(ssh.AuthMethod); !ok {
		t.Errorf("unexpected auth method type: %T", methods[0])
	}

	// Any other non-empty text is treated as a password.
	methods, err = sshAuthMethods("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 {
		t.Fatalf("password produced %d methods, want 1", len(methods))
	}

	// A password-protected private key cannot carry its passphrase in
	// RemoteNode.Auth: it must fail with a clear error mentioning passphrase.
	if _, err := sshAuthMethods(testSSHKeyEncrypted); err == nil ||
		!strings.Contains(err.Error(), "passphrase") {
		t.Errorf("encrypted key error = %v, want passphrase mention", err)
	}
}
