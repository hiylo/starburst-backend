package envagent

import (
	"context"
	"strings"
	"testing"
)

func TestContainerRunArgs(t *testing.T) {
	args := containerRunArgs(3, "mysql", "mysql:8", 3306,
		[]string{"MYSQL_ROOT_PASSWORD=abc"}, nil)
	joined := strings.Join(args, " ")
	for _, want := range []string{"run", "-d", "--name", "intel-3-mysql",
		"--restart=unless-stopped", "-p", "3306:3306", "-e", "MYSQL_ROOT_PASSWORD=abc", "mysql:8"} {
		if !strings.Contains(joined, want) {
			t.Errorf("containerRunArgs missing %q in: %s", want, joined)
		}
	}

	redis := containerRunArgs(3, "redis", "redis:7", 6379,
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
