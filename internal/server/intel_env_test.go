package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/store"
)

// TestIntelEnvEnsureAndInstall verifies the environment layer: requirements are
// detected during analyze, ensure probes each one (with a stubbed docker), and
// a one-click install provisions a middleware container and flips it to ready.
func TestIntelEnvEnsureAndInstall(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
    <dependency><groupId>org.springframework.boot</groupId><artifactId>spring-boot-starter-data-redis</artifactId><version>3.2.0</version></dependency>
  </dependencies>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	pid := proj.ID

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(pid)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Stub docker: daemon reachable, no containers running initially.
	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	oldRetries := envagent.ProvisionWaitRetries
	envagent.ProvisionWaitRetries = 2
	defer func() { envagent.ProvisionWaitRetries = oldRetries }()
	running := map[string]bool{}
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "info":
			return "24.0.0", nil
		case "ps":
			name := dockerFilterName(args)
			if name != "" && running[name] {
				return name, nil
			}
			return "", nil
		case "run":
			name := dockerRunName(args)
			running[name] = true
			return "abc123def456", nil
		case "rm":
			delete(running, args[len(args)-1])
			return "", nil
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/ensure",
		`{"projectId":`+jsonInt(pid)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("ensure status %d: %s", rec.Code, rec.Body.String())
	}
	var envResp struct {
		Services []struct {
			Service  string `json:"service"`
			Category string `json:"category"`
			Status   string `json:"status"`
			Provider string `json:"provider"`
		} `json:"services"`
		DockerReady bool `json:"dockerReady"`
		Ready       int  `json:"ready"`
		Missing     int  `json:"missing"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envResp); err != nil {
		t.Fatalf("ensure parse: %v", err)
	}
	if !envResp.DockerReady {
		t.Error("dockerReady should be true with stubbed docker")
	}
	status := map[string]string{}
	for _, svc := range envResp.Services {
		status[svc.Service+"|"+svc.Category] = svc.Status
	}
	if status["mysql|middleware"] != "missing" {
		t.Errorf("mysql status = %q, want missing (docker up, no container)", status["mysql|middleware"])
	}
	if status["redis|middleware"] != "missing" {
		t.Errorf("redis status = %q, want missing", status["redis|middleware"])
	}
	if status["jdk|toolchain"] != "" && status["jdk|toolchain"] == "missing" && envResp.Missing == 0 {
		t.Errorf("unexpected missing tally: %+v", envResp)
	}

	// One-click install mysql -> container "runs", probe flips to ready.
	rec = s.do(t, http.MethodPost, "/api/intel/env/install",
		`{"projectId":`+jsonInt(pid)+`,"service":"mysql"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", rec.Code, rec.Body.String())
	}
	var installResp struct {
		Service struct {
			Service       string `json:"service"`
			Status        string `json:"status"`
			Provider      string `json:"provider"`
			Port          int    `json:"port"`
			ContainerName string `json:"containerName"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &installResp); err != nil {
		t.Fatalf("install parse: %v", err)
	}
	if installResp.Service.Status != "ready" {
		t.Errorf("post-install mysql status = %q, want ready", installResp.Service.Status)
	}
	if installResp.Service.Provider != "container" {
		t.Errorf("mysql provider = %q, want container", installResp.Service.Provider)
	}
	if installResp.Service.Port != 3306 {
		t.Errorf("mysql port = %d, want 3306", installResp.Service.Port)
	}

	// Stop -> back to missing.
	rec = s.do(t, http.MethodPost, "/api/intel/env/stop",
		`{"projectId":`+jsonInt(pid)+`,"service":"mysql"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("stop status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/status?projectId="+jsonInt(pid), "", wh)
	var statusResp struct {
		Services []struct {
			Service string `json:"service"`
			Status  string `json:"status"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("status parse: %v", err)
	}
	for _, svc := range statusResp.Services {
		if svc.Service == "mysql" && svc.Status != "missing" {
			t.Errorf("mysql status after stop = %q, want missing", svc.Status)
		}
	}
}

// dockerFilterName extracts "name" from docker ps filter arg "name=^/name$".
func dockerFilterName(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "name=^/") && strings.HasSuffix(a, "$") {
			return strings.TrimSuffix(strings.TrimPrefix(a, "name=^/"), "$")
		}
	}
	return ""
}

// dockerRunName extracts the container name following "--name".
func dockerRunName(args []string) string {
	for i, a := range args {
		if a == "--name" && i+1 < len(args) {
			return args[i+1]
		}
	}
	if len(args) >= 4 {
		return args[3]
	}
	return ""
}

// TestIntelEnvGateBlocksRun verifies the environment gate: a run is rejected
// before any command executes when required middleware is missing.
func TestIntelEnvGateBlocksRun(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
  </dependencies>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}

	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "info" {
			return "24.0.0", nil
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code == 200 {
		t.Fatalf("run should be gated, got 200: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "环境门禁") {
		t.Errorf("run failure should mention 环境门禁: %s", rec.Body.String())
	}

	// Force bypasses the gate: the run proceeds (outcome depends on the local
	// toolchain, but the error must NOT be the gate message).
	rec = s.do(t, http.MethodPost, "/api/intel/run",
		`{"projectId":`+jsonInt(proj.ID)+`,"force":true}`, wh)
	if rec.Code == 200 {
		return // force allowed a passing run
	}
	if strings.Contains(rec.Body.String(), "环境门禁") {
		t.Errorf("force run should bypass the gate: %s", rec.Body.String())
	}
}

// TestIntelEnvExternalConfig verifies an externally provided middleware
// endpoint: it is probed once, persisted with provider=external, and survives
// subsequent re-probes (the gate treats it as ready when reachable).
func TestIntelEnvExternalConfig(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <dependencies>
    <dependency><groupId>com.mysql</groupId><artifactId>mysql-connector-j</artifactId><version>8.0.33</version></dependency>
  </dependencies>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// A reachable endpoint to point the external config at.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rec = s.do(t, http.MethodPost, "/api/intel/env/external",
		`{"projectId":`+jsonInt(proj.ID)+`,"service":"mysql","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"username":"root","password":"secret"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("external status %d: %s", rec.Code, rec.Body.String())
	}
	var extResp struct {
		Service struct {
			Service  string `json:"service"`
			Status   string `json:"status"`
			Provider string `json:"provider"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &extResp); err != nil {
		t.Fatalf("external parse: %v", err)
	}
	if extResp.Service.Status != "ready" || extResp.Service.Provider != "external" {
		t.Errorf("external = %+v, want ready/external", extResp.Service)
	}

	// Re-probe keeps the external row (docker stub would otherwise mark missing).
	old := envagent.RunDocker
	defer func() { envagent.RunDocker = old }()
	envagent.RunDocker = func(ctx context.Context, args ...string) (string, error) {
		return "", nil
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/status?projectId="+jsonInt(proj.ID), "", wh)
	var stResp struct {
		Services []struct {
			Service  string `json:"service"`
			Provider string `json:"provider"`
			Status   string `json:"status"`
		} `json:"services"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stResp); err != nil {
		t.Fatalf("status parse: %v", err)
	}
	found := false
	for _, svc := range stResp.Services {
		if svc.Service == "mysql" {
			found = true
			if svc.Provider != "external" || svc.Status != "ready" {
				t.Errorf("re-probe lost external config: %+v", svc)
			}
		}
	}
	if !found {
		t.Errorf("mysql missing from status services: %+v", stResp.Services)
	}
}

// TestIntelEnvSchemaInit verifies lib initialization: discovered SQL scripts
// are fed to a ready MySQL container via stubbed "docker exec -i mysql".
func TestIntelEnvSchemaInit(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src/main/resources/db/migration/V1__init.sql"),
		"CREATE TABLE t (id INT);\n")
	writeTestFile(t, filepath.Join(root, "src/main/resources/application.yml"),
		"spring:\n  datasource:\n    url: jdbc:mysql://127.0.0.1:3306/db")

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	// Mark mysql as a ready container with a known password (skips real docker).
	now := time.Now()
	s.store.UpsertIntelEnvServices(context.Background(), proj.ID, []*store.IntelEnvService{{
		ProjectID:     proj.ID,
		Service:       "mysql",
		Category:      "middleware",
		Provider:      "container",
		Status:        "ready",
		ContainerName: "intel-X-mysql",
		Password:      "pw",
		HealthCheckAt: &now,
	}})

	// Stub the stdin-fed docker runner to record the script content.
	old := envagent.RunDockerInput
	defer func() { envagent.RunDockerInput = old }()
	got := ""
	count := 0
	envagent.RunDockerInput = func(ctx context.Context, stdin string, args ...string) (string, error) {
		got = stdin
		count++
		if len(args) < 2 || args[0] != "exec" || args[1] != "-i" {
			t.Errorf("docker exec args = %v", args)
		}
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/schema-init",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("schema-init status %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Executed int `json:"executed"`
		Total    int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("schema-init parse: %v", err)
	}
	if resp.Executed != 1 || resp.Total != 1 {
		t.Errorf("schema-init executed=%d total=%d, want 1/1", resp.Executed, resp.Total)
	}
	if count != 1 {
		t.Errorf("docker exec call count = %d, want 1", count)
	}
	if !strings.Contains(got, "CREATE TABLE t") {
		t.Errorf("script not fed to docker exec: %q", got)
	}
}

// TestIntelDevices verifies the Android device management: wireless connect
// (stubbed adb), list, bind to a project, and delete.
func TestIntelDevices(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	old := envagent.RunADB
	defer func() { envagent.RunADB = old }()
	envagent.RunADB = func(ctx context.Context, args ...string) (string, error) {
		if len(args) == 0 {
			return "", nil
		}
		switch args[0] {
		case "connect":
			return "connected to " + args[1], nil
		case "devices":
			return "List of devices attached\n192.0.2.9:5555 device product:echo model:EchoPhone\n", nil
		}
		return "", nil
	}

	rec := s.do(t, http.MethodPost, "/api/intel/env/devices/connect",
		`{"ip":"192.0.2.9","port":5555}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("connect status %d: %s", rec.Code, rec.Body.String())
	}
	var cResp struct {
		Device struct {
			ID     int64  `json:"id"`
			Serial string `json:"serial"`
			Status string `json:"status"`
		} `json:"device"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cResp); err != nil {
		t.Fatalf("connect parse: %v", err)
	}
	if cResp.Device.Serial != "192.0.2.9:5555" {
		t.Errorf("device serial = %q", cResp.Device.Serial)
	}
	deviceID := cResp.Device.ID

	rec = s.do(t, http.MethodGet, "/api/intel/env/devices", "", wh)
	var list struct {
		Devices []struct {
			ID    int64 `json:"id"`
			Bound bool  `json:"bound"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Devices) != 1 {
		t.Fatalf("devices = %d, want 1", len(list.Devices))
	}

	rec = s.do(t, http.MethodPut, "/api/intel/env/devices/"+jsonInt(deviceID)+"/bind",
		`{"projectId":1}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodDelete, "/api/intel/env/devices/"+jsonInt(deviceID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/env/devices", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Devices) != 0 {
		t.Errorf("devices after delete = %d, want 0", len(list.Devices))
	}
}

// TestIntelRemoteNodes verifies remote execution node management: a node
// pointing at a reachable TCP port is marked reachable, one on a closed port is
// not, and delete removes it.
func TestIntelRemoteNodes(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	rec := s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"linux-ci","host":"127.0.0.1","port":`+jsonInt(int64(port))+`,"capabilities":"linux-docker"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("create reachable node status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Node struct {
			ID        int64  `json:"id"`
			Reachable bool   `json:"reachable"`
			Auth      string `json:"auth"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("create parse: %v", err)
	}
	if !created.Node.Reachable {
		t.Error("reachable node marked unreachable")
	}
	// Auth must not leak in the response.
	if created.Node.Auth != "" {
		t.Errorf("auth leaked in response")
	}

	rec = s.do(t, http.MethodPost, "/api/intel/nodes",
		`{"name":"down","host":"127.0.0.1","port":1}`, wh)
	var down struct {
		Node struct {
			Reachable bool `json:"reachable"`
		} `json:"node"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &down); err != nil {
		t.Fatalf("down parse: %v", err)
	}
	if down.Node.Reachable {
		t.Error("closed port node should be unreachable")
	}

	rec = s.do(t, http.MethodGet, "/api/intel/nodes", "", wh)
	var list struct {
		Nodes []struct {
			ID int64 `json:"id"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(list.Nodes))
	}

	rec = s.do(t, http.MethodDelete, "/api/intel/nodes/"+jsonInt(created.Node.ID), "", wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete status %d: %s", rec.Code, rec.Body.String())
	}
	rec = s.do(t, http.MethodGet, "/api/intel/nodes", "", wh)
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list parse: %v", err)
	}
	if len(list.Nodes) != 1 {
		t.Errorf("nodes after delete = %d, want 1", len(list.Nodes))
	}
}

// TestIntelEnvToolchainInstall verifies the deterministic toolchain install
// path: the handler issues the per-item apt command (stubbed) and persists the
// resulting service row.
func TestIntelEnvToolchainInstall(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	old := envagent.RunSystem
	defer func() { envagent.RunSystem = old }()
	got := ""
	envagent.RunSystem = func(ctx context.Context, args ...string) (string, error) {
		got = strings.Join(args, " ")
		return "Setting up openjdk-17-jdk...", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/install",
		`{"projectId":`+jsonInt(proj.ID)+`,"service":"jdk"}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("install status %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(got, "openjdk-17-jdk") {
		t.Errorf("install command = %q, want openjdk-17-jdk", got)
	}
	var inst struct {
		Service struct {
			Service string `json:"service"`
			Status  string `json:"status"`
		} `json:"service"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inst); err != nil {
		t.Fatalf("install parse: %v", err)
	}
	if inst.Service.Service != "jdk" {
		t.Errorf("service = %q, want jdk", inst.Service.Service)
	}
}

// TestIntelEnvToolchainRequiresRoot verifies the toolchain install interaction:
// without root or passwordless sudo, the handler returns the exact command so
// the user can run it interactively instead of failing silently.
func TestIntelEnvToolchainRequiresRoot(t *testing.T) {
	s := newTestServer(t)
	wh := loginWeb(t, s)

	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "pom.xml"), `<project>
  <properties><java.version>17</java.version></properties>
</project>`)

	rec := s.do(t, http.MethodPost, "/api/intel/projects",
		`{"name":"demo","source":"local","localPath":"`+filepath.ToSlash(root)+`"}`, wh)
	var proj struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &proj); err != nil || proj.ID == 0 {
		t.Fatalf("create project: %s", rec.Body.String())
	}
	rec = s.do(t, http.MethodPost, "/api/intel/analyze",
		`{"projectId":`+jsonInt(proj.ID)+`}`, wh)
	if rec.Code != http.StatusOK {
		t.Fatalf("analyze status %d: %s", rec.Code, rec.Body.String())
	}

	// Simulate non-root without passwordless sudo.
	oldRoot := envagent.IsRootCheck
	defer func() { envagent.IsRootCheck = oldRoot }()
	envagent.IsRootCheck = func() bool { return false }
	oldSudo := envagent.CheckSudo
	defer func() { envagent.CheckSudo = oldSudo }()
	envagent.CheckSudo = func(ctx context.Context) bool { return false }
	oldRun := envagent.RunSystem
	defer func() { envagent.RunSystem = oldRun }()
	called := false
	envagent.RunSystem = func(ctx context.Context, args ...string) (string, error) {
		called = true
		return "", nil
	}

	rec = s.do(t, http.MethodPost, "/api/intel/env/install",
		`{"projectId":`+jsonInt(proj.ID)+`,"service":"jdk"}`, wh)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("install status = %d, want 400 (root required)", rec.Code)
	}
	if called {
		t.Error("install command must not run without elevation")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "openjdk-17-jdk") {
		t.Errorf("response should include the sudo command: %s", body)
	}
	if !strings.Contains(body, "root") {
		t.Errorf("response should explain root requirement: %s", body)
	}
}
