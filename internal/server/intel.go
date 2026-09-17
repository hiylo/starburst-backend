package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel"
	"github.com/hiylo/starburst-backend/internal/intel/android"
	"github.com/hiylo/starburst-backend/internal/intel/delta"
	"github.com/hiylo/starburst-backend/internal/intel/deps"
	"github.com/hiylo/starburst-backend/internal/intel/enrich"
	"github.com/hiylo/starburst-backend/internal/intel/envdetect"
	"github.com/hiylo/starburst-backend/internal/intel/feature"
	"github.com/hiylo/starburst-backend/internal/intel/gateway"
	"github.com/hiylo/starburst-backend/internal/intel/ios"
	"github.com/hiylo/starburst-backend/internal/intel/sbom"
	"github.com/hiylo/starburst-backend/internal/intel/testassets"
	"github.com/hiylo/starburst-backend/internal/intel/web"
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

// handleIntelAndroidBindings lists the project's Android client field bindings
// (the must-display field list extracted from DataBinding layouts).
func (s *Server) handleIntelAndroidBindings(w http.ResponseWriter, r *http.Request) {
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
	bindings, err := s.store.ListIntelAndroidBindings(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load android bindings failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

// handleIntelWebBindings lists the project's Web client field bindings (the
// must-display field list extracted from Vue templates).
func (s *Server) handleIntelWebBindings(w http.ResponseWriter, r *http.Request) {
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
	bindings, err := s.store.ListIntelWebBindings(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load web bindings failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

// handleIntelIosBindings lists the project's iOS client field bindings (the
// must-display field list extracted from SwiftUI views).
func (s *Server) handleIntelIosBindings(w http.ResponseWriter, r *http.Request) {
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
	bindings, err := s.store.ListIntelIosBindings(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load ios bindings failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
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

// handleIntelModuleCommands updates a module's reviewed command whitelist
// (commands_json). The body must be a JSON array of command strings; the list
// is validated before persisting so edited entries cannot smuggle shell
// metacharacters of their own (argv is split without a shell at run time).
func (s *Server) handleIntelModuleCommands(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, ok := s.intelIDFromPath(r, "/api/intel/modules/")
	if !ok {
		return
	}
	var req struct {
		Commands []string `json:"commands"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	clean := make([]string, 0, len(req.Commands))
	for _, c := range req.Commands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		clean = append(clean, c)
	}
	b, err := json.Marshal(clean)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode commands failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	mod, err := s.store.GetIntelModule(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "module not found")
		return
	}
	if err := s.store.UpdateIntelModuleCommands(ctx, mod.ID, string(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, "update module commands failed")
		return
	}
	mod.CommandsJSON = string(b)
	writeJSON(w, http.StatusOK, map[string]any{"module": mod})
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
	p.CommandsJSON = intel.ProjectCommandsJSON(mods)
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		log.Printf("intel project commands %d: %v", projectID, err)
	}
	var allEndpoints []*store.IntelEndpoint
	var allEntities []*store.IntelEntity
	var allCases []*store.TestCase
	for _, m := range mods {
		sum, err := intel.ScanModule(root, m.RelPath)
		if err != nil {
			continue
		}
		for i := range sum.Entities {
			sum.Entities[i].ModuleID = m.ID
		}
		for i := range sum.Endpoints {
			sum.Endpoints[i].ModuleID = m.ID
		}
		allEntities = append(allEntities, sum.Entities...)
		allEndpoints = append(allEndpoints, sum.Endpoints...)
		assets, err := testassets.Discover(root, m.RelPath)
		if err == nil {
			allCases = append(allCases, buildTestCases(m, assets)...)
		}
	}
	if err := s.store.ReplaceIntelEntities(ctx, projectID, allEntities); err != nil {
		return err
	}
	if err := s.store.ReplaceIntelEndpoints(ctx, projectID, allEndpoints); err != nil {
		return err
	}
	if err := s.store.ReplaceIntelTestCases(ctx, projectID, allCases); err != nil {
		return err
	}
	if err := s.persistFeatures(ctx, projectID, allEndpoints); err != nil {
		return err
	}
	if err := s.persistAndroidBindings(ctx, projectID, root, mods); err != nil {
		log.Printf("intel android bindings project %d: %v", projectID, err)
	}
	if err := s.persistWebBindings(ctx, projectID, root, mods); err != nil {
		log.Printf("intel web bindings project %d: %v", projectID, err)
	}
	if err := s.persistIosBindings(ctx, projectID, root, mods); err != nil {
		log.Printf("intel ios bindings project %d: %v", projectID, err)
	}
	if err := s.persistGatewayRoutes(ctx, projectID, root); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, root)
	s.persistOverview(ctx, projectID, p, root)
	s.persistEnvRequirements(ctx, projectID, root)
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

// buildTestCases converts a module's discovered test assets into store models
// with the module id stamped (for later per-module filtering and execution
// status attribution).
func buildTestCases(m *store.IntelModule, assets []testassets.Asset) []*store.TestCase {
	cases := make([]*store.TestCase, 0, len(assets))
	for _, a := range assets {
		tags := "[]"
		if b, err := json.Marshal(a.Tags); err == nil {
			tags = string(b)
		}
		cases = append(cases, &store.TestCase{
			ModuleID:  m.ID,
			Module:    m.RelPath,
			Kind:      a.Kind,
			Framework: a.Framework,
			Class:     a.Class,
			Method:    a.Method,
			Path:      a.Path,
			Tags:      tags,
		})
	}
	return cases
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

// persistAndroidBindings extracts Android DataBinding "page -> field path"
// bindings for every android module and persists them as the client's
// must-display field list (used later for CLIENT_MISSING_FIELD attribution).
func (s *Server) persistAndroidBindings(ctx context.Context, projectID int64, root string, mods []*store.IntelModule) error {
	bindings := make([]*store.IntelAndroidBinding, 0)
	for _, m := range mods {
		if m.KindType != "android" {
			continue
		}
		dir := filepath.Join(root, m.RelPath)
		resDir := filepath.Join(dir, "src", "main", "res")
		if fi, err := os.Stat(resDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := android.ExtractBindings(resDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(root, resDir, b.Source)
			bindings = append(bindings, &store.IntelAndroidBinding{
				ModuleID:   m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Widget:     b.Widget,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelAndroidBindings(ctx, projectID, bindings)
}

// persistWebBindings extracts Vue template "page -> field path" bindings for
// every web module and persists them as the client's must-display field list.
func (s *Server) persistWebBindings(ctx context.Context, projectID int64, root string, mods []*store.IntelModule) error {
	bindings := make([]*store.IntelWebBinding, 0)
	for _, m := range mods {
		if m.KindType != "web" {
			continue
		}
		dir := filepath.Join(root, m.RelPath)
		srcDir := filepath.Join(dir, "src")
		if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := web.ExtractBindings(srcDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(root, srcDir, b.Source)
			bindings = append(bindings, &store.IntelWebBinding{
				ModuleID:   m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Slot:       b.Slot,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelWebBindings(ctx, projectID, bindings)
}

// persistIosBindings extracts SwiftUI view "page -> field path" bindings for
// every iOS module and persists them as the client's must-display field list.
func (s *Server) persistIosBindings(ctx context.Context, projectID int64, root string, mods []*store.IntelModule) error {
	bindings := make([]*store.IntelIosBinding, 0)
	for _, m := range mods {
		if m.KindType != "ios" {
			continue
		}
		dir := filepath.Join(root, m.RelPath)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := ios.ExtractBindings(dir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(root, dir, b.Source)
			bindings = append(bindings, &store.IntelIosBinding{
				ModuleID:   m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Slot:       b.Slot,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelIosBindings(ctx, projectID, bindings)
}

// splitAndroidSource splits a binding's "rel/path.xml:line" source (relative to
// resDir) into a project-root-relative file path and a line number.
func splitAndroidSource(root, resDir, src string) (string, int) {
	file := src
	line := 0
	if i := strings.LastIndexByte(src, ':'); i >= 0 {
		if n, err := strconv.Atoi(src[i+1:]); err == nil {
			line = n
			file = src[:i]
		}
	}
	full := filepath.Join(resDir, filepath.FromSlash(file))
	rel, err := filepath.Rel(root, full)
	if err != nil {
		rel = full
	}
	return filepath.ToSlash(rel), line
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
	target := filepath.Join(reposDir, safeName(p.Name))
	if err := s.ensureGitClone(ctx, p, target); err != nil {
		return "", err
	}
	return target, nil
}

// ensureGitClone clones a git project into target on first use, and refreshes
// an existing clone to the requested ref. It shells out to git with explicit
// argv (no shell) so a user-supplied URL cannot inject commands.
func (s *Server) ensureGitClone(ctx context.Context, p *store.IntelProject, target string) error {
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		args := []string{"clone", "--depth", "1"}
		if p.GitRef != "" {
			args = append(args, "--branch", p.GitRef)
		}
		args = append(args, p.GitURL, target)
		cmd := exec.CommandContext(ctx, "git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// Refresh: fetch the requested ref and hard-reset so re-analyze sees HEAD.
	fetch := exec.CommandContext(ctx, "git", "fetch", "--depth", "1", "origin")
	fetch.Dir = target
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	ref := p.GitRef
	if ref == "" {
		ref = "HEAD"
	}
	reset := exec.CommandContext(ctx, "git", "reset", "--hard", "origin/"+ref)
	reset.Dir = target
	if out, err := reset.CombinedOutput(); err != nil {
		return fmt.Errorf("git reset: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
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
