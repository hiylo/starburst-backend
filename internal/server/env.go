package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/intel/envdetect"
	"github.com/hiylo/starburst-backend/internal/intel/schemainit"
	"github.com/hiylo/starburst-backend/internal/store"
)

// persistEnvRequirements runs static requirement detection over the project and
// persists the per-item declared environment dependencies.
func (s *Server) persistEnvRequirements(ctx context.Context, projectID int64, roots []string) {
	reqs := detectRequirements(roots)
	storeReqs := make([]*store.IntelEnvRequirement, 0, len(reqs))
	for _, r := range reqs {
		storeReqs = append(storeReqs, &store.IntelEnvRequirement{
			Service:  r.Service,
			Category: r.Category,
			Version:  r.Version,
			Source:   r.Source,
		})
	}
	if err := s.store.ReplaceIntelEnvRequirements(ctx, projectID, storeReqs); err != nil {
		log.Printf("intel env persist requirements project %d: %v", projectID, err)
	}
}

// handleIntelEnvEnsure detects the project's environment requirements, probes
// each one on the local machine and persists per-item status (the environment
// gate input). Missing and unsupported items are listed for one-click install.
func (s *Server) handleIntelEnvEnsure(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64 `json:"projectId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	services, reqs, err := s.ensureEnv(ctx, req.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "检测环境失败: "+err.Error())
		return
	}
	writeEnvResponse(w, req.ProjectID, reqs, services)
}

// ensureEnv detects the project's environment requirements, probes each one on
// the local machine and persists per-item status. Shared by the ensure/status
// handlers and the run gate (which must evaluate readiness before executing).
func (s *Server) ensureEnv(ctx context.Context, projectID int64) ([]*store.IntelEnvService, []*store.IntelEnvRequirement, error) {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	s.persistEnvRequirements(ctx, projectID, []string{root})

	reqs, _ := s.store.ListIntelEnvRequirements(ctx, projectID)
	services, err := s.probeEnvServices(ctx, projectID, root, reqs)
	if err != nil {
		return nil, reqs, err
	}
	if err := s.store.ReplaceIntelEnvServices(ctx, projectID, services); err != nil {
		return nil, reqs, err
	}
	return services, reqs, nil
}

// envGate evaluates environment readiness before a test run: every required
// middleware and toolchain must be ready, otherwise the run is rejected with a
// per-item missing list (the environment gate of §3.6). A failure to determine
// the environment state is treated as "not ready" (hard gate): silently letting
// it through meant a broken probe also passed the gate, hiding missing
// middleware behind a failed detection.
func (s *Server) envGate(ctx context.Context, projectID int64) error {
	services, _, err := s.ensureEnv(ctx, projectID)
	if err != nil {
		// 硬门禁：探测失败 ≠ 就绪。此前「遇错放行」会让环境探测本身坏了时
		// 「缺失中间件」也被判为通过；改为拒绝并暴露探测错误。
		return fmt.Errorf("环境门禁探测失败：%v；请检查环境供给配置后重试", err)
	}
	var missing []string
	for _, svc := range services {
		if svc.Status != "ready" {
			missing = append(missing, fmt.Sprintf("%s(%s)", svc.Service, svc.Status))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("环境门禁未通过，缺失项：%s；请到「环境供给」Tab 逐项安装后重试",
			strings.Join(missing, "、"))
	}
	return nil
}

// handleIntelEnvStatus re-probes the project's environment requirements and
// returns the current per-item status.
func (s *Server) handleIntelEnvStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	services, reqs, err := s.ensureEnv(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "检测环境失败")
		return
	}
	writeEnvResponse(w, projectID, reqs, services)
}

// handleIntelEnvInstall provisions one missing environment item (a middleware
// container) and updates its status. Toolchain items are reported as requiring
// manual install for now.
func (s *Server) handleIntelEnvInstall(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Service   string `json:"service"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || req.Service == "" {
		writeErr(w, http.StatusBadRequest, "projectId and service are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	version := requirementVersion(ctx, s, req.ProjectID, req.Service)
	if envagent.Toolchain(req.Service) {
		cmds := envagent.ToolchainInstallCommand(req.Service, version)
		if len(cmds) == 0 {
			writeErr(w, http.StatusBadRequest, "该工具链暂不支持自动安装（请手动安装）")
			return
		}
		elevated := envagent.IsRootCheck() || envagent.CheckSudo(ctx)
		sudoCmd := "sudo " + strings.Join(cmds, " ")
		if !elevated {
			writeErr(w, http.StatusBadRequest,
				"该工具链安装需要 root 权限，但当前无免密 sudo。请在本机以 root 执行后重新点击「安装」复检（或先配置免密 sudo）：\n\n"+sudoCmd)
			return
		}
		out, err := envagent.RunSystem(ctx, cmds...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError,
				"安装失败: "+err.Error()+" "+truncateStr(out, 300)+"\n（可手动执行：\n"+sudoCmd+"\n）")
			return
		}
		status, provider, _, healthy, _ := envagent.Probe(ctx, req.ProjectID, req.Service, "toolchain", version)
		svc := &store.IntelEnvService{
			ProjectID: req.ProjectID,
			Service:   req.Service,
			Category:  "toolchain",
			Version:   version,
			Provider:  provider,
			Status:    status,
			Healthy:   healthy,
			Endpoint:  strings.TrimSpace(out),
		}
		if err := s.store.UpsertIntelEnvServices(ctx, req.ProjectID, []*store.IntelEnvService{svc}); err != nil {
			writeErr(w, http.StatusInternalServerError, "persist env status failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"service": svc, "elevated": elevated, "command": sudoCmd})
		return
	}
	if _, ok := envagent.Middleware(req.Service); !ok {
		writeErr(w, http.StatusBadRequest, "该服务不支持容器自动供给")
		return
	}
	containerID, port, err := envagent.ProvisionContainer(ctx, req.ProjectID, req.Service, version)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "供给失败: "+err.Error())
		return
	}
	status, provider, containerName, healthy, _ := envagent.Probe(ctx, req.ProjectID, req.Service, "middleware", version)
	svc := &store.IntelEnvService{
		ProjectID:     req.ProjectID,
		Service:       req.Service,
		Category:      "middleware",
		Version:       version,
		Provider:      provider,
		Status:        status,
		Host:          "127.0.0.1",
		Port:          port,
		Healthy:       healthy,
		ContainerName: containerName,
		ContainerID:   containerID,
	}
	if spec, ok := envagent.Middleware(req.Service); ok {
		svc.Username = spec.User
	}
	if err := s.store.UpsertIntelEnvServices(ctx, req.ProjectID, []*store.IntelEnvService{svc}); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist env status failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": svc})
}

// truncateStr caps a string for error/log surfaces (deterministic).
func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// handleIntelEnvStop stops and removes a project's middleware container,
// resetting it to missing.
func (s *Server) handleIntelEnvStop(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Service   string `json:"service"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || req.Service == "" {
		writeErr(w, http.StatusBadRequest, "projectId and service are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := envagent.StopContainer(ctx, req.ProjectID, req.Service); err != nil {
		writeErr(w, http.StatusInternalServerError, "stop failed")
		return
	}
	if err := s.store.UpsertIntelEnvServices(ctx, req.ProjectID, []*store.IntelEnvService{{
		ProjectID: req.ProjectID,
		Service:   req.Service,
		Status:    "missing",
	}}); err != nil {
		log.Printf("intel env stop persist project %d service %s: %v", req.ProjectID, req.Service, err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIntelEnvExternal accepts a user-supplied external middleware endpoint
// (host:port, optional credentials), probes its reachability and persists it as
// provider=external. External services survive subsequent re-probes (they are
// not re-probed as containers) and satisfy the environment gate when reachable.
func (s *Server) handleIntelEnvExternal(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Service   string `json:"service"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || req.Service == "" || req.Host == "" || req.Port <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId, service, host and port are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reachable := envagent.ProbeExternal(ctx, req.Host, req.Port)
	status := "missing"
	if reachable {
		status = "ready"
	}
	now := time.Now()
	svc := &store.IntelEnvService{
		ProjectID:     req.ProjectID,
		Service:       req.Service,
		Category:      "middleware",
		Provider:      "external",
		Status:        status,
		Host:          req.Host,
		Port:          req.Port,
		Endpoint:      net.JoinHostPort(req.Host, strconv.Itoa(req.Port)),
		Healthy:       reachable,
		Username:      req.Username,
		Password:      req.Password,
		HealthCheckAt: &now,
	}
	if err := s.store.UpsertIntelEnvServices(ctx, req.ProjectID, []*store.IntelEnvService{svc}); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist external service failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": svc})
}

// handleIntelEnvSchemaInit runs the project's discovered SQL migration scripts
// against a ready middleware container (currently MySQL) before integration
// tests: each script is fed via "docker exec -i <container> mysql ... on stdin".
// It reports per-script success and the number of scripts executed.
func (s *Server) handleIntelEnvSchemaInit(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Service   string `json:"service"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	if req.Service == "" {
		req.Service = "mysql"
	}
	if req.Service != "mysql" {
		writeErr(w, http.StatusBadRequest, "库初始化目前仅支持 mysql")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	var svc *store.IntelEnvService
	services, _ := s.store.ListIntelEnvServices(ctx, req.ProjectID)
	for _, x := range services {
		if x.Service == req.Service {
			svc = x
			break
		}
	}
	if svc == nil || svc.Status != "ready" || svc.ContainerName == "" {
		writeErr(w, http.StatusBadRequest, "mysql 环境未就绪（容器未启动或未在环境 Tab 供给）")
		return
	}
	p, err := s.store.GetIntelProject(ctx, req.ProjectID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve project root failed")
		return
	}
	scripts := schemainit.Discover(root)
	// 人工 intel-env.yaml 声明的 init_scripts（针对当前 service）补充进迁移列表。
	if path, ok := envdetect.ManifestAt(root); ok {
		if data, err := os.ReadFile(path); err == nil {
			if _, inits, err := envdetect.ParseManifest(data, "intel-env.yaml"); err == nil {
				seen := make(map[string]bool, len(scripts))
				for _, sc := range scripts {
					seen[sc.Rel] = true
				}
				for _, is := range inits {
					if is.Script == "" || seen[is.Script] {
						continue
					}
					if is.Service != "" && is.Service != req.Service {
						continue
					}
					seen[is.Script] = true
					scripts = append(scripts, schemainit.Script{Rel: is.Script, Abs: filepath.Join(root, is.Script)})
				}
			}
		}
	}
	if len(scripts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"scripts": []any{}, "executed": 0, "note": "未发现 SQL 迁移脚本（Flyway/Liquibase/*.sql）"})
		return
	}
	dbName := schemainit.DBName(root)

	type scriptResult struct {
		Rel string `json:"rel"`
		OK  bool   `json:"ok"`
		Err string `json:"error,omitempty"`
	}
	results := make([]scriptResult, 0, len(scripts))
	for _, script := range scripts {
		data, err := os.ReadFile(script.Abs)
		if err != nil {
			results = append(results, scriptResult{Rel: script.Rel, Err: err.Error()})
			continue
		}
		args := []string{"exec", "-i"}
		if svc.Password != "" {
			// MYSQL_PWD rather than -p<pw>: argv of the docker client is readable
			// by any local process via ps/proc, the env of another user's process is not.
			args = append(args, "-e", "MYSQL_PWD="+svc.Password)
		}
		args = append(args, svc.ContainerName, "mysql", "-uroot")
		if dbName != "" {
			args = append(args, dbName)
		}
		if _, err := envagent.RunDockerInput(ctx, string(data), args...); err != nil {
			results = append(results, scriptResult{Rel: script.Rel, Err: err.Error()})
			continue
		}
		results = append(results, scriptResult{Rel: script.Rel, OK: true})
	}
	executed := 0
	for _, r := range results {
		if r.OK {
			executed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"scripts":   results,
		"executed":  executed,
		"total":     len(results),
		"container": svc.ContainerName,
		"dbName":    dbName,
	})
}

// probeEnvServices probes every requirement on the local machine and builds
// the per-service status rows. Previously persisted credentials are preserved;
// services configured as provider=external are kept as-is (they are user-owned
// and only probed once at configuration time).
func (s *Server) probeEnvServices(ctx context.Context, projectID int64, root string, reqs []*store.IntelEnvRequirement) ([]*store.IntelEnvService, error) {
	existing := map[string]*store.IntelEnvService{}
	if prev, err := s.store.ListIntelEnvServices(ctx, projectID); err == nil {
		for _, svc := range prev {
			existing[svc.Service] = svc
		}
	}
	services := make([]*store.IntelEnvService, 0, len(reqs))
	for _, req := range reqs {
		if prev := existing[req.Service]; prev != nil && prev.Provider == "external" {
			prev.ProjectID = projectID
			services = append(services, prev)
			continue
		}
		status, provider, containerName, healthy, err := envagent.Probe(ctx, projectID, req.Service, req.Category, req.Version)
		if err != nil {
			continue
		}
		svc := &store.IntelEnvService{
			ProjectID:     projectID,
			Service:       req.Service,
			Category:      req.Category,
			Version:       req.Version,
			Provider:      provider,
			Status:        status,
			Host:          "127.0.0.1",
			ContainerName: containerName,
			Healthy:       healthy,
		}
		if prev := existing[req.Service]; prev != nil {
			svc.Password = prev.Password
			svc.Username = prev.Username
			svc.ContainerID = prev.ContainerID
		}
		if spec, ok := envagent.Middleware(req.Service); ok {
			svc.Port = spec.Port
			if svc.Username == "" {
				svc.Username = spec.User
			}
		}
		if envagent.Toolchain(req.Service) {
			svc.Port = 0
		}
		now := time.Now()
		svc.HealthCheckAt = &now
		services = append(services, svc)
	}
	return services, nil
}

// requirementVersion returns the declared version for a service, or "".
func requirementVersion(ctx context.Context, s *Server, projectID int64, service string) string {
	reqs, _ := s.store.ListIntelEnvRequirements(ctx, projectID)
	for _, r := range reqs {
		if r.Service == service {
			return r.Version
		}
	}
	return ""
}

// writeEnvResponse renders the environment gate view: requirements, per-service
// status, docker reachability for middleware and ready/missing/unsupported
// tallies.
func writeEnvResponse(w http.ResponseWriter, projectID int64, reqs []*store.IntelEnvRequirement, services []*store.IntelEnvService) {
	dockerReady := false
	sawMiddleware := false
	for _, svc := range services {
		if _, ok := envagent.Middleware(svc.Service); !ok {
			continue
		}
		sawMiddleware = true
		if svc.Status != "unsupported" {
			dockerReady = true
		}
	}
	ready, missing, unsupported := 0, 0, 0
	for _, svc := range services {
		switch svc.Status {
		case "ready":
			ready++
		case "unsupported":
			unsupported++
		default:
			missing++
		}
	}
	dockerNotice := ""
	if sawMiddleware && !dockerReady {
		dockerNotice = "本机未检测到可用的 Docker，中间件无法自动供给，请安装 Docker 或配置外部环境"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"requirements": reqs,
		"services":     services,
		"dockerReady":  dockerReady,
		"dockerNotice": dockerNotice,
		"ready":        ready,
		"missing":      missing,
		"unsupported":  unsupported,
		"portHint":     "中间件连接串以 127.0.0.1 配置的端口为准",
	})
}
