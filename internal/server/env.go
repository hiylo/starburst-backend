package server

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/intel/envdetect"
	"github.com/hiylo/starburst-backend/internal/store"
)

// persistEnvRequirements runs static requirement detection over the project and
// persists the per-item declared environment dependencies.
func (s *Server) persistEnvRequirements(ctx context.Context, projectID int64, root string) {
	reqs, err := envdetect.Detect(root, "")
	if err != nil {
		log.Printf("intel env detect project %d: %v", projectID, err)
		return
	}
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
	s.persistEnvRequirements(ctx, req.ProjectID, root)

	reqs, _ := s.store.ListIntelEnvRequirements(ctx, req.ProjectID)
	services, err := s.probeEnvServices(ctx, req.ProjectID, root, reqs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "probe env failed: "+err.Error())
		return
	}
	if err := s.store.ReplaceIntelEnvServices(ctx, req.ProjectID, services); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist env status failed")
		return
	}
	writeEnvResponse(w, req.ProjectID, reqs, services)
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
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "resolve project root failed")
		return
	}
	reqs, _ := s.store.ListIntelEnvRequirements(ctx, projectID)
	services, err := s.probeEnvServices(ctx, projectID, root, reqs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "probe env failed")
		return
	}
	if err := s.store.ReplaceIntelEnvServices(ctx, projectID, services); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist env status failed")
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
		writeErr(w, http.StatusBadRequest, "工具链请在本机手动安装（暂无自动安装）")
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

// probeEnvServices probes every requirement on the local machine and builds
// the per-service status rows, preserving previously persisted credentials so a
// re-probe keeps working connections valid.
func (s *Server) probeEnvServices(ctx context.Context, projectID int64, root string, reqs []*store.IntelEnvRequirement) ([]*store.IntelEnvService, error) {
	existing := map[string]*store.IntelEnvService{}
	if prev, err := s.store.ListIntelEnvServices(ctx, projectID); err == nil {
		for _, svc := range prev {
			existing[svc.Service] = svc
		}
	}
	services := make([]*store.IntelEnvService, 0, len(reqs))
	for _, req := range reqs {
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
