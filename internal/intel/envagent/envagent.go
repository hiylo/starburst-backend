// Package envagent operates on the local machine to probe and provision the
// test environment required by a project: it checks whether Docker is
// available, probes middleware containers and installed toolchains, and
// provisions middleware as disposable Docker containers (the container path of
// the environment layer). Toolchain install and external config fall back to
// instructions; everything is deterministic and idempotent.
package envagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/netguard"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
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

// RunDockerInput runs a docker CLI command feeding payload on stdin (used for
// "docker exec -i ... mysql < script" schema initialization). Also overridable
// in tests.
var RunDockerInput = runDockerInputExec

// RunADB runs an adb CLI command (argv direct, no shell) for device discovery
// and wireless connect. Tests substitute a deterministic stub.
var RunADB = runADBExec

// runDockerExec is the default docker runner (argv direct, no shell).
func runDockerExec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runDockerInputExec is the default stdin-fed docker runner (argv direct).
func runDockerInputExec(ctx context.Context, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runADBExec is the default adb runner (argv direct, no shell).
func runADBExec(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "adb", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ADBDevice is one device line from "adb devices -l".
type ADBDevice struct {
	Serial string // e.g. emulator-5554 or 192.0.2.5:5555
	State  string // device | offline | unauthorized
	Name   string
}

// ADBDeviceList runs "adb devices -l" and parses the attached devices.
func ADBDeviceList(ctx context.Context) ([]ADBDevice, error) {
	out, err := RunADB(ctx, "devices", "-l")
	if err != nil {
		return nil, err
	}
	devs := []ADBDevice{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "* ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := ADBDevice{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "model:") {
				d.Name = strings.TrimPrefix(f, "model:")
			}
		}
		if d.Name == "" {
			d.Name = d.Serial
		}
		devs = append(devs, d)
	}
	return devs, nil
}

// ADBConnect connects to a wireless device host:port and reports whether the
// device reached the "device" state.
func ADBConnect(ctx context.Context, host string, port int) error {
	// adb 是自己解析主机的外部命令，netguard 的拨号包装管不到它，只能在这里先校验。
	if err := netguard.CheckHost(ctx, host); err != nil {
		return err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if _, err := RunADB(ctx, "connect", addr); err != nil {
		return err
	}
	return nil
}

// ADBReady reports whether an adb binary is present and a server can start.
func ADBReady(ctx context.Context) bool {
	_, err := RunADB(ctx, "start-server")
	return err == nil
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
	hostPort = findFreePort(ctx, spec.Port)
	args := containerRunArgs(projectID, service, image, hostPort, spec.Port, env, extra)
	out, err := RunDocker(ctx, args...)
	if err != nil {
		return "", 0, fmt.Errorf("docker run: %v (%s)", err, out)
	}
	id := strings.TrimSpace(out)
	if i := strings.IndexByte(id, '\n'); i >= 0 {
		id = id[:i]
	}
	for i := 0; i < ProvisionWaitRetries; i++ {
		if portOpen(ctx, "127.0.0.1", hostPort) {
			break
		}
		select {
		case <-ctx.Done():
			return id, hostPort, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return id, hostPort, nil
}

// findFreePort returns the lowest host port >= base that is not currently open
// locally, probing at most 50 candidates. Collisions with in-use middleware
// ports therefore shift the mapping deterministically instead of failing.
func findFreePort(ctx context.Context, base int) int {
	if base <= 0 {
		base = 10000
	}
	for p := base; p < base+50; p++ {
		if !portOpen(ctx, "127.0.0.1", p) {
			return p
		}
	}
	return base
}

// StopContainer stops and removes the project's middleware container (reset).
func StopContainer(ctx context.Context, projectID int64, service string) error {
	name := ContainerName(projectID, service)
	_, _ = RunDocker(ctx, "rm", "-f", name)
	return nil
}

// containerRunArgs builds the argv for "docker run" provisioning one
// middleware (pure, so it is unit-tested without docker). hostPort is the port
// published on the host side, containerPort the process port inside the image.
func containerRunArgs(projectID int64, service, image string, hostPort, containerPort int, env, extra []string) []string {
	args := []string{"run", "-d", "--name", ContainerName(projectID, service),
		"--restart=unless-stopped", "-p", strconv.Itoa(hostPort) + ":" + strconv.Itoa(containerPort)}
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

// portOpen dials host:port with a short timeout. The dial goes through
// netguard so a caller-supplied host cannot point the backend at link-local or
// metadata addresses; the budget covers name resolution as well as connect.
func portOpen(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, err := netguard.Dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeExternal reports whether an externally provided endpoint (host:port) is
// reachable from this machine. Used to accept user-supplied middleware instead
// of provisioning a container.
func ProbeExternal(ctx context.Context, host string, port int) bool {
	if host == "" || port <= 0 {
		return false
	}
	return portOpen(ctx, host, port)
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

// RunSystem executes a system install command (argv direct) after optionally
// wrapping with sudo when running as root-less but passwordless sudo works.
// Overridable for tests.
var RunSystem = runSystemExec

// IsRootCheck reports whether the process runs as root (install needs no
// elevation). A var so tests can simulate a non-root install.
var IsRootCheck = func() bool { return os.Geteuid() == 0 }

func runSystemExec(ctx context.Context, args ...string) (string, error) {
	if !IsRootCheck() && CheckSudo(ctx) {
		args = append([]string{"sudo"}, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CheckSudo reports whether non-interactive passwordless sudo is available, so
// a toolchain install can elevate without a prompt. Tests override it.
var CheckSudo = checkSudoExec

func checkSudoExec(ctx context.Context) bool {
	if IsRootCheck() {
		return true
	}
	cmd := exec.CommandContext(ctx, "sudo", "-n", "true")
	return cmd.Run() == nil
}

// ToolchainInstallCommand returns the deterministic per-item install argv for
// a toolchain service (apt-get based on Debian/Ubuntu hosts). An empty result
// means the toolchain has no supported auto-install path on this platform.
func ToolchainInstallCommand(service, version string) []string {
	switch service {
	case "jdk":
		v := "17"
		if version != "" {
			v = strings.TrimPrefix(version, "1.")
		}
		return []string{"apt-get", "install", "-y", "openjdk-" + v + "-jdk"}
	case "node":
		return []string{"apt-get", "install", "-y", "nodejs", "npm"}
	case "go":
		return []string{"apt-get", "install", "-y", "golang"}
	case "gradle":
		return []string{"apt-get", "install", "-y", "gradle"}
	case "maven":
		return []string{"apt-get", "install", "-y", "maven"}
	}
	return nil
}

// RunSSH executes a command on a remote node via the library ssh client
// (no system ssh binary). Args follow SSHCommandArgs; authentication comes from
// the ssh-agent or default ~/.ssh keys (defaultKeyAuth), matching the old
// system-ssh non-interactive behaviour. Tests substitute a deterministic stub.
var RunSSH = runSSHExec

func runSSHExec(ctx context.Context, args ...string) (string, error) {
	host, user, command, port, timeout, err := parseSSHArgs(args)
	if err != nil {
		return "", err
	}
	client, err := sshDialTimeout(ctx, host, port, user, "", timeout)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, err := SSHCommand(ctx, client, command)
	return string(out), err
}

// RunSSHStream is the streaming sibling of RunSSH: stdout chunks are handed to
// onStdout as they are produced (edge output of long test commands) while the
// full stdout is still returned. Args and authentication behave exactly like
// RunSSH.
func RunSSHStream(ctx context.Context, onStdout func([]byte), args ...string) (string, error) {
	host, user, command, port, timeout, err := parseSSHArgs(args)
	if err != nil {
		return "", err
	}
	client, err := sshDialTimeout(ctx, host, port, user, "", timeout)
	if err != nil {
		return "", err
	}
	defer client.Close()
	var full tailBuffer
	full.limit = outputTailLimit
	_, err = SSHSessionOutput(ctx, client, command, func(p []byte) {
		if onStdout != nil {
			onStdout(p)
		}
		full.Write(p)
	})
	return full.buf.String(), err
}

// RunCommandOn executes a command on a remote node via the library ssh client,
// authenticating with authText (same semantics as SSHClient: private key,
// password, or agent/default keys when empty). It is a package variable so the
// server layer can substitute a deterministic stub in tests without dialing a
// node.
var RunCommandOn = runCommandOnSSH

func runCommandOnSSH(ctx context.Context, host string, port int, user, authText, command string) (string, error) {
	client, err := SSHClient(ctx, host, port, user, authText)
	if err != nil {
		return "", err
	}
	defer client.Close()
	out, err := SSHCommand(ctx, client, command)
	return string(out), err
}

// SSHCommandArgs builds the ssh argv to run command on user@host:port. Options
// are pinned so the call is non-interactive and bounded: BatchMode (no password
// prompt) + ConnectTimeout 10s. StrictHostKeyChecking accept-new records first
// contact and fails on changed keys deterministically.
func SSHCommandArgs(host, user string, port int, command string) []string {
	args := []string{"-p", strconv.Itoa(port),
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "IdentitiesOnly=yes",
	}
	target := host
	if user != "" {
		target = user + "@" + host
	}
	args = append(args, target, command)
	return args
}

// DialSSH connects to a node host:port with the library ssh client, using an
// empty authText (agent/default keys — see defaultKeyAuth), and returns the
// connection. It is a package variable so pull tests can substitute a fake that
// records the issued commands instead of dialing a real node.
var DialSSH = dialSSHClient

func dialSSHClient(ctx context.Context, host string, port int, user, authText string) (SSHConnection, error) {
	return SSHClient(ctx, host, port, user, authText)
}

// sshDialTimeout dials like SSHClient but with an explicit connect budget,
// used by the argv-compatible RunSSH/RunSSHStream path.
func sshDialTimeout(ctx context.Context, host string, port int, user, authText string, connectTimeout time.Duration) (*ssh.Client, error) {
	return SSHClientWithOptions(ctx, host, port, user, authText, SSHClientOptions{ConnectTimeout: connectTimeout})
}

// SSHClientOptions configures SSHClientWithOptions dialing.
type SSHClientOptions struct {
	// HostKeyCallback verifies the node host key; nil selects the strict
	// known_hosts check described in hostKeyCallback.
	HostKeyCallback ssh.HostKeyCallback
	// HostKeyPath is the known_hosts file to load (default
	// ~/.ssh/known_hosts). Empty falls back per hostKeyCallback.
	HostKeyPath string
	// ConnectTimeout bounds the TCP dial + handshake (default 10s).
	ConnectTimeout time.Duration
	// InsecureHostKey disables host-key verification (ssh.InsecureIgnoreHostKey)
	// when true. Default false keeps the fail-closed known_hosts check; only
	// enable this when the caller explicitly accepts first-contact MITM risk —
	// never pick it up implicitly from an unreadable known_hosts file.
	InsecureHostKey bool
}

// SSHClient dials host:port with the library ssh client and authenticates with
// authText: a PEM/OpenSSH private key ("-----BEGIN ... PRIVATE KEY") is parsed
// as a public-key credential, any other non-empty value is treated as a
// password, and an empty value falls back to the ssh-agent then default ~/.ssh
// keys. The TCP dial goes through netguard so a caller-supplied node host
// cannot point the backend at link-local or metadata addresses.
func SSHClient(ctx context.Context, host string, port int, user, authText string) (*ssh.Client, error) {
	return SSHClientWithOptions(ctx, host, port, user, authText, SSHClientOptions{})
}

// SSHClientWithOptions is SSHClient with explicit connect options.
func SSHClientWithOptions(ctx context.Context, host string, port int, user, authText string, opt SSHClientOptions) (*ssh.Client, error) {
	if opt.ConnectTimeout <= 0 {
		opt.ConnectTimeout = 10 * time.Second
	}
	methods, err := sshAuthMethods(authText)
	if err != nil {
		return nil, err
	}
	if len(methods) == 0 {
		return nil, errors.New("envagent: 没有可用的认证方式：请填写口令/私钥，或配置 SSH_AUTH_SOCK agent / ~/.ssh 私钥")
	}
	if user == "" {
		user = defaultSSHUser()
	}
	if opt.HostKeyCallback == nil {
		if opt.InsecureHostKey {
			opt.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		} else {
			cb, err := hostKeyCallback(opt.HostKeyPath)
			if err != nil {
				return nil, err
			}
			opt.HostKeyCallback = cb
		}
	}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            methods,
		HostKeyCallback: opt.HostKeyCallback,
	}
	ctx, cancel := context.WithTimeout(ctx, opt.ConnectTimeout)
	defer cancel()
	conn, err := netguard.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	type handshake struct {
		cc  ssh.Conn
		ch  <-chan ssh.NewChannel
		req <-chan *ssh.Request
		err error
	}
	done := make(chan handshake, 1)
	go func() {
		cc, ch, req, herr := ssh.NewClientConn(conn, addr, cfg)
		done <- handshake{cc, ch, req, herr}
	}()
	select {
	case h := <-done:
		if h.err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("ssh handshake: %w", h.err)
		}
		return ssh.NewClient(h.cc, h.ch, h.req), nil
	case <-ctx.Done():
		_ = conn.Close()
		return nil, fmt.Errorf("ssh connect %s: %w", addr, ctx.Err())
	}
}

// hostKeyCallback returns a strict known_hosts host-key verifier for the given
// file (default ~/.ssh/known_hosts). fail-closed：known_hosts 不可读或不存在时
// 返回 error 拒绝建立连接。后端常以服务账号运行、known_hosts 缺失，若此处回退
// 为不校验 host key，任何能伪造节点的人都能 MITM，且 defaultKeyAuth 还会收走
// 后端本机私钥。如需首连信任，请管理员把节点 host key 手工写入 known_hosts；
// 只有调用方显式传入 InsecureHostKey 时才允许关闭校验。
func hostKeyCallback(knownHostsPath string) (ssh.HostKeyCallback, error) {
	if knownHostsPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			knownHostsPath = filepath.Join(home, ".ssh", "known_hosts")
		}
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("known_hosts %q 不可读，拒绝建立连接（如需首连信任请手工把节点 host key 写入该文件）：%w", knownHostsPath, err)
	}
	return cb, nil
}

// sshAuthMethods turns a node's Auth text into ssh auth methods: a
// PEM/OpenSSH private key ("-----BEGIN ... PRIVATE KEY") is parsed as a
// public-key signer, any other non-empty value is used as a password, and an
// empty value (the legacy argv path) falls back to defaultKeyAuth. A password
// protected private key is rejected: RemoteNode.Auth carries a single text
// field with no passphrase slot, so the admin must store an unencrypted key or
// a password instead.
func sshAuthMethods(authText string) ([]ssh.AuthMethod, error) {
	authText = strings.TrimSpace(authText)
	if strings.HasPrefix(authText, "-----BEGIN") {
		signer, err := ssh.ParsePrivateKey([]byte(authText))
		if err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return nil, errors.New("envagent: Auth 是口令加密的私钥，RemoteNode.Auth 无法携带 passphrase，请改用未加密私钥或口令认证")
			}
			return nil, fmt.Errorf("envagent: parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if authText == "" {
		return defaultKeyAuth(), nil
	}
	return []ssh.AuthMethod{ssh.Password(authText)}, nil
}

// defaultKeyAuth returns ssh-agent + default ~/.ssh key auth, mirroring what
// the old system-ssh non-interactive invocation relied on (BatchMode +
// IdentitiesOnly with no explicit identity, so only the agent) plus the common
// default keys. nil means no credential source exists at all.
func defaultKeyAuth() []ssh.AuthMethod {
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
		data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			continue
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}
	return nil
}

// defaultSSHUser mirrors the system-ssh behaviour of using the local user as
// the remote user when the node row does not configure one.
func defaultSSHUser() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if cu, err := user.Current(); err == nil && cu.Username != "" {
		return cu.Username
	}
	return ""
}

// SSHConnection is the ssh client surface the command helpers need. *ssh.Client
// satisfies it; tests substitute a recording fake.
type SSHConnection interface {
	NewSession() (*ssh.Session, error)
	Close() error
}

// outputTailLimit 是远程/本地命令输出保留的尾部上限：有界缓冲丢弃最旧字节、
// 保留最后 N 字节（与 server 侧 intelOutputLimit 同一常量），避免 go test
// -json / playwright 数百 MB 输出把内存打满。
const outputTailLimit = 256 << 10

// tailBuffer 是有界保留尾部的 writer：写入超过 limit 时丢弃最旧字节，只保留
// 最后 limit 字节。用于命令输出的内存上限控制。
type tailBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if n := t.buf.Len() - t.limit; n > 0 {
		t.buf.Next(n)
	}
	return len(p), nil
}

// SSHCommand runs command on an existing ssh connection and returns its
// combined stdout+stderr (tail-bounded to outputTailLimit). ctx cancellation
// sends SIGKILL and closes the session so a stalled remote command cannot block
// the caller forever.
func SSHCommand(ctx context.Context, c SSHConnection, command string) ([]byte, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var buf tailBuffer
	buf.limit = outputTailLimit
	sess.Stdout = &buf
	sess.Stderr = &buf
	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(command) }()
	select {
	case err := <-errCh:
		return buf.buf.Bytes(), err
	case <-ctx.Done():
		// 取消时先发 SIGKILL 再关会话：仅 Close 时 sess.Run 在远端进程持续运行
		// 且网络黑洞下可能永不返回，导致 SSHCommand 永久挂起、连接与 intelSem
		// 槽位泄漏。SIGKILL 后给 errCh 留 5s 兜底，不无限等待。
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
		return buf.buf.Bytes(), ctx.Err()
	}
}

// chunkWriter forwards each write to onChunk so a session's stdout streams out
// in bounded chunks instead of being buffered until the command completes.
type chunkWriter struct{ onChunk func([]byte) }

func (w chunkWriter) Write(p []byte) (int, error) {
	w.onChunk(append([]byte(nil), p...))
	return len(p), nil
}

// SSHSessionOutput runs command on an existing ssh connection streaming stdout
// to onStdout as chunks arrive (edge-produced output for long test commands);
// stderr is collected (tail-bounded) and returned together with the run error.
// Whether the exit status signals an ssh.ExitError is the caller's concern.
func SSHSessionOutput(ctx context.Context, c SSHConnection, command string, onStdout func([]byte)) ([]byte, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var stderr tailBuffer
	stderr.limit = outputTailLimit
	sess.Stderr = &stderr
	if onStdout != nil {
		sess.Stdout = chunkWriter{onChunk: onStdout}
	}
	errCh := make(chan error, 1)
	go func() { errCh <- sess.Run(command) }()
	select {
	case err := <-errCh:
		return stderr.buf.Bytes(), err
	case <-ctx.Done():
		// 与 SSHCommand 相同：SIGKILL + Close 后给 errCh 留 5s 兜底，避免远端
		// 进程持续运行且网络黑洞下永久挂起。
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
		}
		return stderr.buf.Bytes(), ctx.Err()
	}
}

// parseSSHArgs decodes the argv produced by SSHCommandArgs
// ("-p port -o options... [user@]host command") back into its parts for the
// library ssh path. Recognised options: -p port and -o ConnectTimeout=N (dial
// budget); the remaining -o settings (BatchMode, IdentitiesOnly, ...) are
// subsumed by the library client's always non-interactive single-command
// session.
func parseSSHArgs(args []string) (host, user, command string, port int, connectTimeout time.Duration, err error) {
	port = 22
	connectTimeout = 10 * time.Second
	var target string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-p":
			if i+1 >= len(args) {
				return "", "", "", 0, 0, errors.New("envagent: -p 缺少端口参数")
			}
			i++
			p, perr := strconv.Atoi(args[i])
			if perr != nil || p <= 0 {
				return "", "", "", 0, 0, fmt.Errorf("envagent: 非法端口 %q", args[i])
			}
			port = p
		case "-o":
			if i+1 >= len(args) {
				return "", "", "", 0, 0, errors.New("envagent: -o 缺少选项")
			}
			i++
			if v, ok := strings.CutPrefix(args[i], "ConnectTimeout="); ok {
				if secs, serr := strconv.Atoi(v); serr == nil && secs > 0 {
					connectTimeout = time.Duration(secs) * time.Second
				}
			}
		default:
			if strings.HasPrefix(a, "-") {
				continue // 其它未知开关忽略，库会话本身保证非交互
			}
			if target == "" {
				target = a
				continue
			}
			command = strings.Join(args[i:], " ")
			i = len(args)
		}
	}
	if target == "" {
		return "", "", "", 0, 0, errors.New("envagent: 未找到远端目标 host")
	}
	if at := strings.LastIndexByte(target, '@'); at >= 0 {
		user = target[:at]
		host = target[at+1:]
	} else {
		host = target
	}
	if command == "" {
		return "", "", "", 0, 0, errors.New("envagent: 缺少远端命令")
	}
	return host, user, command, port, connectTimeout, nil
}
