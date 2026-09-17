// Package envagent operates on the local machine to probe and provision the
// test environment required by a project: it checks whether Docker is
// available, probes middleware containers and installed toolchains, and
// provisions middleware as disposable Docker containers (the container path of
// the environment layer). Toolchain install and external config fall back to
// instructions; everything is deterministic and idempotent.
package envagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Status values for a probed environment item.
const (
	StatusReady       = "ready"
	StatusMissing     = "missing"
	StatusUnsupported = "unsupported"
)

// Provider values describing how an item is satisfied.
const (
	ProviderContainer = "container"
	ProviderInstalled = "installed"
)

// RunDocker runs a docker CLI command and returns its combined output. It is a
// package variable so tests can substitute a deterministic stub (nothing in
// the production path runs real containers unless this executes docker).
var RunDocker = runDockerExec

// runDockerExec is the default docker runner (argv direct, no shell).
func runDockerExec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// DockerReady reports whether the docker CLI and daemon are usable.
func DockerReady(ctx context.Context) bool {
	out, err := RunDocker(ctx, "info", "--format", "{{.ServerVersion}}")
	return err == nil && strings.TrimSpace(out) != ""
}

// ContainerName is the deterministic container name for a project's middleware.
func ContainerName(projectID int64, service string) string {
	return fmt.Sprintf("intel-%d-%s", projectID, service)
}

// Middleware reports the image metadata of a known middleware service, and
// whether the service can be auto-provisioned as a container.
func Middleware(service string) (spec MiddlewareSpec, ok bool) {
	s, ok := middlewareSpecs[service]
	return s, ok
}

// MiddlewareSpec describes how to launch one middleware container.
type MiddlewareSpec struct {
	Image string
	Port  int
	Env   []string
	User  string
}

// middlewareSpecs is the registry of auto-provisionable middleware. Passwords
// are generated per provision and injected at runtime (never in the repo).
var middlewareSpecs = map[string]MiddlewareSpec{
	"mysql":         {Image: "mysql", Port: 3306, User: "root"},
	"redis":         {Image: "redis", Port: 6379},
	"nacos":         {Image: "nacos/nacos-server", Port: 8848, User: "nacos"},
	"rabbitmq":      {Image: "rabbitmq", Port: 5672, User: "intel"},
	"elasticsearch": {Image: "docker.elastic.co/elasticsearch/elasticsearch", Port: 9200},
}

// Toolchain reports whether a service is a toolchain (installed on host).
func Toolchain(service string) bool {
	switch service {
	case "jdk", "node", "go", "gradle", "maven", "android-sdk":
		return true
	}
	return false
}

// Probe determines the live status of one requirement on this machine. It never
// mutates anything.
func Probe(ctx context.Context, projectID int64, service, category, version string) (status, provider string, containerName string, healthy bool, err error) {
	if Toolchain(service) {
		if toolchainReady(ctx, service) {
			return StatusReady, ProviderInstalled, "", true, nil
		}
		return StatusMissing, ProviderInstalled, "", false, nil
	}
	name := ContainerName(projectID, service)
	if !DockerReady(ctx) {
		return StatusUnsupported, ProviderContainer, name, false, nil
	}
	running := containerRunning(ctx, name)
	if !running {
		return StatusMissing, ProviderContainer, name, false, nil
	}
	spec, ok := middlewareSpecs[service]
	if !ok {
		return StatusReady, ProviderContainer, name, true, nil
	}
	healthy = portOpen(ctx, "127.0.0.1", spec.Port)
	return StatusReady, ProviderContainer, name, healthy, nil
}

// ProvisionWaitRetries bounds the post-launch port wait loop (500ms each).
// Tests override it to keep provisioning fast without a real container.
var ProvisionWaitRetries = 20

// ProvisionContainer launches one middleware container for a project and waits
// briefly for its published port to answer. It is idempotent: an already
// running container is returned as-is.
func ProvisionContainer(ctx context.Context, projectID int64, service, version string) (containerID string, hostPort int, err error) {
	spec, ok := middlewareSpecs[service]
	if !ok {
		return "", 0, fmt.Errorf("service %q 不支持容器自动供给", service)
	}
	name := ContainerName(projectID, service)
	if containerRunning(ctx, name) {
		return existingContainerID(ctx, name), spec.Port, nil
	}
	password := RandomPassword(16)
	env := []string{}
	switch service {
	case "mysql":
		env = append(env, "MYSQL_ROOT_PASSWORD="+password)
	case "redis":
		env = append(env, "REDIS_PASSWORD="+password)
	case "rabbitmq":
		env = append(env, "RABBITMQ_DEFAULT_USER="+spec.User, "RABBITMQ_DEFAULT_PASS="+password)
	case "nacos":
		env = append(env, "MODE=standalone")
	case "elasticsearch":
		env = append(env, "discovery.type=single-node", "xpack.security.enabled=false")
	}
	extra := []string{}
	if service == "redis" {
		extra = append(extra, "redis-server", "--requirepass", password)
	}
	image := spec.Image
	if version != "" {
		image = spec.Image + ":" + version
	}
	args := containerRunArgs(projectID, service, image, spec.Port, env, extra)
	out, err := RunDocker(ctx, args...)
	if err != nil {
		return "", 0, fmt.Errorf("docker run: %v (%s)", err, out)
	}
	id := strings.TrimSpace(out)
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	for i := 0; i < ProvisionWaitRetries; i++ {
		if portOpen(ctx, "127.0.0.1", spec.Port) {
			break
		}
		select {
		case <-ctx.Done():
			return id, spec.Port, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return id, spec.Port, nil
}

// StopContainer stops and removes the project's middleware container (reset).
func StopContainer(ctx context.Context, projectID int64, service string) error {
	name := ContainerName(projectID, service)
	_, _ = RunDocker(ctx, "rm", "-f", name)
	return nil
}

// containerRunArgs builds the argv for "docker run" provisioning one
// middleware (pure, so it is unit-tested without docker).
func containerRunArgs(projectID int64, service, image string, port int, env, extra []string) []string {
	args := []string{"run", "-d", "--name", ContainerName(projectID, service),
		"--restart=unless-stopped", "-p", strconv.Itoa(port) + ":" + strconv.Itoa(port)}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, image)
	args = append(args, extra...)
	return args
}

// containerRunning reports whether the named container exists and is running.
func containerRunning(ctx context.Context, name string) bool {
	out, err := RunDocker(ctx, "ps", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
	return err == nil && strings.Contains(out, name)
}

// existingContainerID returns the id of the named container (may be empty).
func existingContainerID(ctx context.Context, name string) string {
	out, err := RunDocker(ctx, "ps", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.Split(out, "\n")[0])
}

// portOpen dials host:port with a short timeout.
func portOpen(ctx context.Context, host string, port int) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// toolchainReady checks whether a toolchain binary is on PATH and responds.
func toolchainReady(ctx context.Context, service string) bool {
	switch service {
	case "jdk":
		return binaryVersion(ctx, "java", "-version")
	case "node":
		return binaryVersion(ctx, "node", "--version")
	case "go":
		return binaryVersion(ctx, "go", "version")
	case "gradle":
		return binaryVersion(ctx, "gradle", "--version")
	case "maven":
		return binaryVersion(ctx, "mvn", "--version")
	case "android-sdk":
		if os.Getenv("ANDROID_HOME") != "" || os.Getenv("ANDROID_SDK_ROOT") != "" {
			return true
		}
		return binaryVersion(ctx, "adb", "version")
	}
	return false
}

// binaryVersion runs a binary with an argument expecting a version banner.
func binaryVersion(ctx context.Context, name string, arg string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, name, arg)
	return cmd.Run() == nil
}

// RandomPassword returns a URL-safe random alphanumeric password.
func RandomPassword(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)[:n]
}
