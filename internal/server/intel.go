package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel"
	"github.com/hiylo/starburst-backend/internal/intel/delta"
	"github.com/hiylo/starburst-backend/internal/intel/deps"
	"github.com/hiylo/starburst-backend/internal/intel/enrich"
	"github.com/hiylo/starburst-backend/internal/intel/envdetect"
	"github.com/hiylo/starburst-backend/internal/intel/feature"
	"github.com/hiylo/starburst-backend/internal/intel/gateway"
	"github.com/hiylo/starburst-backend/internal/intel/sbom"
	"github.com/hiylo/starburst-backend/internal/intel/testassets"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelProjects lists (GET) and creates (POST) test-intelligence
// projects. Requires a web session or APP token.
func (s *Server) handleIntelProjects(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		s.listIntelProjects(w, r)
	case http.MethodPost:
		s.createIntelProject(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelProjectByID handles a single project: GET detail (with modules),
// DELETE removal, PUT update.
func (s *Server) handleIntelProjectByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	id, ok := intelPathID(w, r, "/api/intel/projects/")
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getIntelProject(w, r, id)
	case http.MethodDelete:
		s.deleteIntelProject(w, r, id)
	case http.MethodPut:
		s.updateIntelProject(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelAnalyze triggers a full analyze: module detection + contract
// scanning, persisted into the intel tables. Synchronous for M1 (async via
// tasks lands with the execution milestone).
func (s *Server) handleIntelAnalyze(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	if err := s.runIntelAnalyze(ctx, req.ProjectID); err != nil {
		log.Printf("intel analyze project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "analyze failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIntelEndpoints lists endpoint contracts for a project (and optional
// module).
func (s *Server) handleIntelEndpoints(w http.ResponseWriter, r *http.Request) {
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
	moduleID := intQuery(r, "moduleId")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	eps, err := s.store.ListIntelEndpoints(ctx, projectID, moduleID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load endpoints failed")
		return
	}
	s.enrichGatewayRoutes(ctx, projectID, eps)
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
}

// enrichGatewayRoutes attaches the matched public gateway path patterns to each
// endpoint, so the contract view distinguishes the internal service path from
// its gateway exposure.
func (s *Server) enrichGatewayRoutes(ctx context.Context, projectID int64, eps []*store.IntelEndpoint) {
	if len(eps) == 0 {
		return
	}
	stored, err := s.store.ListIntelGatewayRoutes(ctx, projectID)
	if err != nil || len(stored) == 0 {
		return
	}
	routes := make([]*gateway.Route, 0, len(stored))
	for _, r := range stored {
		var paths []string
		if json.Unmarshal([]byte(r.PathsJSON), &paths) == nil {
			routes = append(routes, &gateway.Route{Service: r.Service, Paths: paths})
		}
	}
	for _, ep := range eps {
		ep.GatewayRoutes = gateway.Match(routes, ep.Path)
	}
}

// handleIntelEntities lists entity↔table↔column mappings for a project (and
// optional module).
func (s *Server) handleIntelEntities(w http.ResponseWriter, r *http.Request) {
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
	moduleID := intQuery(r, "moduleId")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ents, err := s.store.ListIntelEntities(ctx, projectID, moduleID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load entities failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": ents})
}

// handleIntelModules lists the sub-modules of a project.
func (s *Server) handleIntelModules(w http.ResponseWriter, r *http.Request) {
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
	id, ok := s.intelIDFromPath(r, "/api/intel/projects/")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	mods, err := s.store.ListIntelModules(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load modules failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"modules": mods})
}

// handleIntelGatewayRoutes lists the gateway routes (public exposure) of a
// project, discovered from gateway config and Nacos metadata.
func (s *Server) handleIntelGatewayRoutes(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	routes, err := s.store.ListIntelGatewayRoutes(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load gateway routes failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"gatewayRoutes": routes})
}

// handleIntelImpact returns the project's latest incremental-impact snapshot.
func (s *Server) handleIntelImpact(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	imp, err := s.store.GetIntelImpact(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"impact": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"impact": imp})
}

// handleIntelOverview returns the project's latest dependency/environment/SBOM
// overview snapshot.
func (s *Server) handleIntelOverview(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ov, err := s.store.GetIntelOverview(ctx, projectID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"overview": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overview": ov})
}

// ---- implementation ----

// intelPathID parses a trailing integer id from a URL path prefix.
func intelPathID(w http.ResponseWriter, r *http.Request, prefix string) (int64, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return id, true
}

// intelIDFromPath parses a project id from a path that may continue with a
// sub-resource (e.g. /api/intel/projects/3/modules).
func (s *Server) intelIDFromPath(r *http.Request, prefix string) (int64, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	idStr := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		idStr = rest[:i]
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// intelQueryProject reads the projectId query parameter, writing a 400 when
// missing/invalid.
func (s *Server) intelQueryProject(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("projectId"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId query parameter is required")
		return 0, false
	}
	return id, true
}

func intQuery(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}

func (s *Server) listIntelProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	projects, err := s.store.ListIntelProjects(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list projects failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) createIntelProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name      string `json:"name"`
		Source    string `json:"source"`
		LocalPath string `json:"localPath"`
		GitURL    string `json:"gitUrl"`
		GitRef    string `json:"gitRef"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Source = strings.TrimSpace(req.Source)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	req.GitURL = strings.TrimSpace(req.GitURL)
	req.Name = strings.TrimSpace(req.Name)
	if req.Source == "" {
		req.Source = "local"
	}
	if req.Source == "git" && req.GitURL == "" {
		writeErr(w, http.StatusBadRequest, "gitUrl is required for source=git")
		return
	}
	if req.Source == "local" && req.LocalPath == "" {
		writeErr(w, http.StatusBadRequest, "localPath is required for source=local")
		return
	}
	if req.Name == "" {
		req.Name = deriveProjectName(req.LocalPath, req.GitURL)
	}
	p := &store.IntelProject{
		Name:      req.Name,
		Source:    req.Source,
		LocalPath: req.LocalPath,
		GitURL:    req.GitURL,
		GitRef:    req.GitRef,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.store.CreateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "create project failed")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.store.GetIntelProject(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	mods, err := s.store.ListIntelModules(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load modules failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": p, "modules": mods})
}

func (s *Server) deleteIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.store.DeleteIntelProject(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete project failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) updateIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name     string `json:"name"`
		GitRef   string `json:"gitRef"`
		Commands string `json:"commandsJson"`
		EnvName  string `json:"envName"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.store.GetIntelProject(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if req.Name != "" {
		p.Name = req.Name
	}
	if req.GitRef != "" {
		p.GitRef = req.GitRef
	}
	if req.Commands != "" {
		p.CommandsJSON = req.Commands
	}
	if req.EnvName != "" {
		p.EnvName = req.EnvName
	}
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "update project failed")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// runIntelAnalyze resolves the project's working directory, detects modules,
// scans contracts per module and persists everything. It records snapshot sha
// (HEAD for git, directory-mtime-hash for local non-git).
func (s *Server) runIntelAnalyze(ctx context.Context, projectID int64) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return errNotDir(root)
	}
	mods, err := intel.DetectModules(root)
	if err != nil {
		return err
	}
	if err := s.store.ReplaceIntelModules(ctx, projectID, mods); err != nil {
		return err
	}
	mods, _ = s.store.ListIntelModules(ctx, projectID)
	var allEndpoints []*store.IntelEndpoint
	var allEntities []*store.IntelEntity
	for _, m := range mods {
		sum, err := intel.ScanModule(root, m.RelPath)
		if err != nil {
			continue
		}
		if len(sum.Entities) > 0 {
			if err := s.store.ReplaceIntelEntities(ctx, projectID, m.ID, sum.Entities); err != nil {
				return err
			}
			allEntities = append(allEntities, sum.Entities...)
		}
		if len(sum.Endpoints) > 0 {
			if err := s.store.ReplaceIntelEndpoints(ctx, projectID, m.ID, sum.Endpoints); err != nil {
				return err
			}
			allEndpoints = append(allEndpoints, sum.Endpoints...)
		}
		if err := s.persistTestAssets(ctx, projectID, m, root); err != nil {
			return err
		}
	}
	if err := s.persistFeatures(ctx, projectID, allEndpoints); err != nil {
		return err
	}
	if err := s.persistGatewayRoutes(ctx, projectID, root); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, root)
	s.persistOverview(ctx, projectID, p, root)
	sha, err := snapshotSHA(root)
	if err != nil {
		sha = ""
	}
	if err := s.runIntelComplianceScan(ctx, projectID, root); err != nil {
		log.Printf("intel compliance scan project %d: %v", projectID, err)
	}
	if err := s.runIntelSecurityScan(ctx, projectID, allEntities); err != nil {
		log.Printf("intel security scan project %d: %v", projectID, err)
	}
	s.recordImpact(ctx, projectID, p, root, sha)
	return s.store.MarkIntelProjectAnalyzed(ctx, projectID, sha)
}

// persistTestAssets discovers and stores a module's test assets.
func (s *Server) persistTestAssets(ctx context.Context, projectID int64, m *store.IntelModule, root string) error {
	assets, err := testassets.Discover(root, m.RelPath)
	if err != nil || len(assets) == 0 {
		return nil
	}
	cases := make([]*store.TestCase, 0, len(assets))
	for _, a := range assets {
		tags := "[]"
		if b, err := json.Marshal(a.Tags); err == nil {
			tags = string(b)
		}
		cases = append(cases, &store.TestCase{
			Module:    m.RelPath,
			Kind:      a.Kind,
			Framework: a.Framework,
			Class:     a.Class,
			Method:    a.Method,
			Path:      a.Path,
			Tags:      tags,
		})
	}
	return s.store.ReplaceIntelTestCases(ctx, projectID, m.ID, cases)
}

// persistFeatures clusters the extracted endpoints into candidate feature
// points and stores them (human rename/merge/split/order comes later).
func (s *Server) persistFeatures(ctx context.Context, projectID int64, endpoints []*store.IntelEndpoint) error {
	feats := feature.Cluster(endpoints)
	if len(feats) == 0 {
		return nil
	}
	storeFeats := make([]*store.IntelFeature, 0, len(feats))
	for i, f := range feats {
		ends, _ := json.Marshal(f.Ends)
		storeFeats = append(storeFeats, &store.IntelFeature{
			Name:      f.Name,
			EndsJSON:  string(ends),
			SortOrder: i,
			Source:    "auto",
			Anchor:    f.Anchor,
			Status:    "active",
		})
	}
	return s.store.ReplaceIntelFeatures(ctx, projectID, storeFeats)
}

// persistGatewayRoutes discovers gateway route configuration under root and
// persists it so the endpoint view can map internal paths to public exposure.
func (s *Server) persistGatewayRoutes(ctx context.Context, projectID int64, root string) error {
	routes, err := gateway.Discover(root)
	if err != nil {
		return err
	}
	storeRoutes := make([]*store.IntelGatewayRoute, 0, len(routes))
	for _, r := range routes {
		paths, _ := json.Marshal(r.Paths)
		storeRoutes = append(storeRoutes, &store.IntelGatewayRoute{
			Service:    r.Service,
			PathsJSON:  string(paths),
			URI:        r.URI,
			Source:     r.Source,
			SourceLine: r.SourceLine,
		})
	}
	return s.store.ReplaceIntelGatewayRoutes(ctx, projectID, storeRoutes)
}

// enrichIntelWithLLM runs the optional LLM document-analysis pass: it feeds the
// project's Markdown docs plus the extracted endpoint contracts to the
// orchestration LLM and stores per-endpoint business summaries and (clearly
// marked) suggested gateway routes. It is a no-op unless the LLM is configured.
func (s *Server) enrichIntelWithLLM(ctx context.Context, projectID int64, root string) {
	if s.llm == nil || !s.llm.Enabled() {
		return
	}
	eps, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil || len(eps) == 0 {
		return
	}
	docs := enrich.Docs(root, 30000)
	if docs == "" {
		return
	}
	hints := make([]enrich.EndpointHint, 0, len(eps))
	for _, ep := range eps {
		hints = append(hints, enrich.EndpointHint{Method: ep.Method, Path: ep.Path, ResponseType: ep.ResponseType})
	}
	var result enrich.Result
	if err := s.llm.CompleteJSON(ctx, enrich.SystemPrompt(), enrich.UserPrompt(docs, hints), &result); err != nil {
		log.Printf("intel llm enrich project %d: %v", projectID, err)
		return
	}
	for _, es := range result.Endpoints {
		summary := strings.TrimSpace(es.Summary)
		if summary == "" {
			continue
		}
		if err := s.store.UpdateIntelEndpointSummary(ctx, projectID, es.Method, es.Path, summary); err != nil {
			log.Printf("intel llm summary %s %s: %v", es.Method, es.Path, err)
		}
	}
	s.storeLLMSuggestedRoutes(ctx, projectID, result.Routes)
}

// storeLLMSuggestedRoutes inserts LLM-suggested gateway routes that are not
// already covered by config-derived routes, marking their source as "llm" so
// the deterministic config routes remain authoritative.
func (s *Server) storeLLMSuggestedRoutes(ctx context.Context, projectID int64, hints []enrich.RouteHint) {
	if len(hints) == 0 {
		return
	}
	existing, err := s.store.ListIntelGatewayRoutes(ctx, projectID)
	if err != nil {
		return
	}
	covered := make(map[string]bool)
	for _, r := range existing {
		var paths []string
		if json.Unmarshal([]byte(r.PathsJSON), &paths) != nil {
			continue
		}
		for _, p := range paths {
			covered[p] = true
		}
	}
	var add []*store.IntelGatewayRoute
	for _, h := range hints {
		var fresh []string
		for _, p := range h.Paths {
			if !covered[p] {
				fresh = append(fresh, p)
				covered[p] = true
			}
		}
		if len(fresh) == 0 {
			continue
		}
		pathsJSON, _ := json.Marshal(fresh)
		add = append(add, &store.IntelGatewayRoute{Service: h.Service, PathsJSON: string(pathsJSON), Source: "llm"})
	}
	if len(add) > 0 {
		if err := s.store.AddIntelGatewayRoutes(ctx, projectID, add); err != nil {
			log.Printf("intel llm routes project %d: %v", projectID, err)
		}
	}
}

// recordImpact computes and persists the incremental impact between the
// project's previous snapshot and the current HEAD. It only applies to
// git-backed projects; a non-ancestor base (force-push/reset) or a missing
// previous snapshot records a full rescan instead of a diff.
func (s *Server) recordImpact(ctx context.Context, projectID int64, p *store.IntelProject, root, head string) {
	if head == "" || p.SnapshotSHA == "" || p.SnapshotSHA == head {
		return
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return
	}
	imp := delta.Impact{}
	if !delta.IsAncestor(root, p.SnapshotSHA, head) {
		imp.FullRescan = true
	} else if files, err := delta.DiffFiles(root, p.SnapshotSHA); err == nil {
		imp = delta.ComputeImpact(files)
	} else {
		return
	}
	b, err := json.Marshal(imp)
	if err != nil {
		return
	}
	if err := s.store.ReplaceIntelImpact(ctx, projectID, &store.IntelImpact{
		ProjectID:  projectID,
		BaseSHA:    p.SnapshotSHA,
		HeadSHA:    head,
		ImpactJSON: string(b),
	}); err != nil {
		log.Printf("intel impact project %d: %v", projectID, err)
	}
}

// persistOverview aggregates the project's dependency list, environment
// requirements and CycloneDX SBOM, then stores them as a single overview
// snapshot for the detail view.
func (s *Server) persistOverview(ctx context.Context, projectID int64, p *store.IntelProject, root string) {
	all := collectDependencies(root)
	reqs, _ := envdetect.Detect(root, "")
	depsJSON, _ := json.Marshal(all)
	envJSON, _ := json.Marshal(reqs)
	sbomJSON, err := sbom.Generate(p.Name, all)
	if err != nil {
		sbomJSON = nil
	}
	if err := s.store.ReplaceIntelOverview(ctx, projectID, &store.IntelOverview{
		ProjectID: projectID,
		DepsJSON:  string(depsJSON),
		EnvJSON:   string(envJSON),
		SbomJSON:  string(sbomJSON),
	}); err != nil {
		log.Printf("intel overview project %d: %v", projectID, err)
	}
}

// collectDependencies walks the repository for dependency manifests and returns
// a deduplicated dependency list across ecosystems.
func collectDependencies(root string) []deps.Dependency {
	seen := make(map[string]bool)
	out := make([]deps.Dependency, 0)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "target" ||
				name == ".gradle" || name == "build_artifacts" || name == "dist" {
				return filepath.SkipDir
			}
			return nil
		}
		var parsed []deps.Dependency
		switch d.Name() {
		case "pom.xml":
			if data, err := os.ReadFile(path); err == nil {
				parsed, _ = deps.ParsePom(data)
			}
		case "go.mod":
			if data, err := os.ReadFile(path); err == nil {
				parsed, _ = deps.ParseGoMod(data)
			}
		case "package.json":
			if data, err := os.ReadFile(path); err == nil {
				parsed, _ = deps.ParsePackageJSON(data)
			}
		case "build.gradle", "build.gradle.kts":
			if data, err := os.ReadFile(path); err == nil {
				parsed, _ = deps.ParseGradle(data)
			}
		}
		for _, dep := range parsed {
			key := dep.Ecosystem + "|" + dep.Group + "|" + dep.Name + "|" + dep.Version
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, dep)
		}
		return nil
	})
	return out
}

// projectRoot returns the local working directory of a project: local path for
// source=local, or the repos_dir cache clone (not yet cloned for M1; git clone
// lands with M2). When source=git and the clone is absent we fall back to the
// configured repos_dir + project name so the UI stays usable.
func (s *Server) projectRoot(ctx context.Context, p *store.IntelProject) (string, error) {
	if p.Source == "local" && p.LocalPath != "" {
		return p.LocalPath, nil
	}
	reposDir, err := s.store.GetSetting(ctx, "intel.repos_dir")
	if err != nil || reposDir == "" {
		return "", errSettingMissing("intel.repos_dir not configured for git projects")
	}
	return filepath.Join(reposDir, safeName(p.Name)), nil
}

func errNotDir(root string) error { return &pathErr{msg: "project path is not a directory: " + root} }

type pathErr struct{ msg string }

func (e *pathErr) Error() string { return e.msg }

func errSettingMissing(msg string) error { return &pathErr{msg: msg} }

// safeName sanitizes a project name for use as a directory component.
func safeName(name string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", " ", "_", "..", "__")
	return repl.Replace(strings.TrimSpace(name))
}

// deriveProjectName produces a deterministic project name from a path or git URL.
func deriveProjectName(localPath, gitURL string) string {
	raw := localPath
	if raw == "" {
		raw = gitURL
	}
	raw = strings.TrimRight(raw, "/")
	base := filepath.Base(raw)
	if base == "." || base == "/" || base == "" {
		return "project"
	}
	return base
}

// snapshotSHA produces a deterministic snapshot identifier for cache-validation:
// HEAD sha for git repos, otherwise a hash of the directory listing.
func snapshotSHA(root string) (string, error) {
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		return gitHeadSHA(root)
	}
	return dirHash(root)
}

// gitHeadSHA reads HEAD via git plumbing. Implemented as a defensive fallback
// that returns "" when git is unavailable (M2 fills delta logic).
func gitHeadSHA(root string) (string, error) {
	out, err := runGit(root, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// dirHash hashes a stable projection of the directory tree for staleness checks.
func dirHash(root string) (string, error) {
	return simpleTreeHash(root)
}
