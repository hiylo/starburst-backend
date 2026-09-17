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
	"sync"
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
	// 关联源码目录/仓库子资源：/api/intel/projects/{id}/sources
	if strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/intel/projects/"+strconv.FormatInt(id, 10)), "/") == "sources" {
		s.handleIntelProjectSources(w, r, id)
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
	// 与后台自动分析（创建/改源码触发）共用同一把项目级锁，避免全量替换并发竞态。
	mu := s.intelAnalyzeMutex(req.ProjectID)
	mu.Lock()
	defer mu.Unlock()
	if err := s.runIntelAnalyze(ctx, req.ProjectID); err != nil {
		log.Printf("intel analyze project %d: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "analyze failed: "+err.Error())
		return
	}
	// 分析成功后后台重建知识库索引，让契约问答立即反映最新扫描结果。
	go s.reindexAfterAnalyze(req.ProjectID)
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
	s.applyEndpointOverrides(ctx, projectID, eps)
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps})
}

// handleIntelEndpointOverrides saves a human/LLM correction for one endpoint
// (field=summary) into the overrides layer, keyed by its natural key
// "METHOD path" so it survives re-analysis (only auto values are rebuilt).
func (s *Server) handleIntelEndpointOverrides(w http.ResponseWriter, r *http.Request) {
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
	id, ok := s.intelIDFromPath(r, "/api/intel/endpoints/")
	if !ok {
		return
	}
	var req struct {
		Summary string `json:"summary"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		writeErr(w, http.StatusBadRequest, "summary is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	ep, err := s.store.GetIntelEndpoint(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "endpoint not found")
		return
	}
	if err := s.store.UpsertIntelOverride(ctx, &store.IntelOverride{
		ProjectID:   ep.ProjectID,
		Target:      "endpoint",
		RowKey:      ep.Method + " " + ep.Path,
		Field:       "summary",
		ManualValue: summary,
		Confidence:  "high",
		Status:      "applied",
		Source:      "manual",
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "save correction failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": ep.Method + " " + ep.Path})
}

// applyEndpointOverrides merges human-confirmed endpoint corrections (target=
// endpoint, row_key="METHOD path", field=summary) into the endpoint list at read
// time, so manual/LLM fixes survive re-analysis (only auto values are rebuilt).
func (s *Server) applyEndpointOverrides(ctx context.Context, projectID int64, eps []*store.IntelEndpoint) {
	overrides, err := s.store.ListIntelOverrides(ctx, projectID, false)
	if err != nil {
		return
	}
	byKey := make(map[string]string)
	for _, o := range overrides {
		if o.Status != "applied" || o.Target != "endpoint" || o.Field != "summary" || o.ManualValue == "" {
			continue
		}
		byKey[o.RowKey] = o.ManualValue
	}
	for _, ep := range eps {
		if ep == nil {
			continue
		}
		key := ep.Method + " " + ep.Path
		if v, ok := byKey[key]; ok {
			ep.Summary = v
		}
	}
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
	s.applyModuleOverrides(ctx, id, mods)
	writeJSON(w, http.StatusOK, map[string]any{"modules": mods})
}

// applyModuleOverrides merges human-confirmed overrides (status=applied,
// target=module, field=role) into the module list at read time, so the manual
// correction is the authoritative value while the auto-detected one stays in
// the DB for comparison.
func (s *Server) applyModuleOverrides(ctx context.Context, projectID int64, mods []*store.IntelModule) {
	for _, m := range mods {
		s.applyModuleOverridesTo(ctx, m)
	}
}

// applyFeatureOverrides merges human-confirmed name overrides (status=applied,
// target=feature, field=name) into the feature list at read time, keyed by the
// feature id (rowKey = feature id as string) so renames flow from the
// 待确认队列 without touching the auto-derived row.
func (s *Server) applyFeatureOverrides(ctx context.Context, projectID int64, feats []*store.IntelFeature) {
	overrides, err := s.store.ListIntelOverrides(ctx, projectID, false)
	if err != nil {
		return
	}
	byID := make(map[string]string)
	for _, o := range overrides {
		if o.Status != "applied" || o.Target != "feature" || o.Field != "name" || o.ManualValue == "" {
			continue
		}
		byID[o.RowKey] = o.ManualValue
	}
	for _, f := range feats {
		key := strconv.FormatInt(f.ID, 10)
		if v, ok := byID[key]; ok {
			f.Name = v
		}
	}
}

// handleIntelModuleCommands serves the per-module detail (GET), force
// regeneration of the AI summary (POST .../resummarize) and manual overrides
// for human-corrected role/summary (PUT .../overrides). The command whitelist
// lives at project level (projects.commands_json).
func (s *Server) handleIntelModuleCommands(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	id, ok := s.intelIDFromPath(r, "/api/intel/modules/")
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	mod, err := s.store.GetIntelModule(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "module not found")
		return
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/intel/modules/"), "/")
	s.applyModuleOverridesTo(ctx, mod)

	// POST .../resummarize: clear the cached summary and regenerate now.
	if r.Method == http.MethodPost && strings.HasSuffix(rest, "/resummarize") {
		mod.Summary = ""
		_ = s.store.UpdateIntelModuleSummary(ctx, mod.ID, "")
		endpoints, _ := s.store.ListIntelEndpoints(ctx, mod.ProjectID, mod.ID)
		entities, _ := s.store.ListIntelEntities(ctx, mod.ProjectID, mod.ID)
		mod.Summary = s.ensureModuleSummary(ctx, mod, endpoints, entities)
		writeJSON(w, http.StatusOK, map[string]any{"module": mod, "summary": mod.Summary})
		return
	}

	// PUT .../overrides: store human corrections (role/summary) as applied
	// overrides so re-analysis never overwrites them.
	if r.Method == http.MethodPut && strings.HasSuffix(rest, "/overrides") {
		var req struct {
			Role    *string `json:"role"`
			Summary *string `json:"summary"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Role != nil && strings.TrimSpace(*req.Role) != "" {
			_ = s.store.UpsertIntelOverride(ctx, &store.IntelOverride{
				ProjectID:   mod.ProjectID,
				Target:      "module",
				RowKey:      mod.RelPath,
				Field:       "kind_role",
				ManualValue: strings.TrimSpace(*req.Role),
				Confidence:  "high",
				Status:      "applied",
				Source:      "manual",
			})
			mod.KindRole = strings.TrimSpace(*req.Role)
		}
		if req.Summary != nil && strings.TrimSpace(*req.Summary) != "" {
			_ = s.store.UpsertIntelOverride(ctx, &store.IntelOverride{
				ProjectID:   mod.ProjectID,
				Target:      "module",
				RowKey:      mod.RelPath,
				Field:       "summary",
				ManualValue: strings.TrimSpace(*req.Summary),
				Confidence:  "high",
				Status:      "applied",
				Source:      "manual",
			})
			mod.Summary = strings.TrimSpace(*req.Summary)
		}
		writeJSON(w, http.StatusOK, map[string]any{"module": mod})
		return
	}

	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Module detail: basic info + per-module stats of the linked assets.
	endpoints, _ := s.store.ListIntelEndpoints(ctx, mod.ProjectID, mod.ID)
	entities, _ := s.store.ListIntelEntities(ctx, mod.ProjectID, mod.ID)
	cases, _ := s.store.ListIntelTestCases(ctx, mod.ProjectID, mod.ID)
	if mod.Summary == "" {
		mod.Summary = s.ensureModuleSummary(ctx, mod, endpoints, entities)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"module": mod,
		"stats": map[string]int{
			"endpoints": len(endpoints),
			"entities":  len(entities),
			"cases":     len(cases),
		},
	})
}

// applyModuleOverridesTo merges human-confirmed overrides for one module:
// kind_role and summary (the fields a human can correct on the page).
func (s *Server) applyModuleOverridesTo(ctx context.Context, mod *store.IntelModule) {
	overrides, err := s.store.ListIntelOverrides(ctx, mod.ProjectID, false)
	if err != nil {
		return
	}
	for _, o := range overrides {
		if o.Status != "applied" || o.Target != "module" || o.RowKey != mod.RelPath || o.ManualValue == "" {
			continue
		}
		switch o.Field {
		case "kind_role", "role":
			mod.KindRole = o.ManualValue
		case "summary":
			mod.Summary = o.ManualValue
		}
	}
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
		Name        string `json:"name"`
		Source      string `json:"source"`
		LocalPath   string `json:"localPath"`
		GitURL      string `json:"gitUrl"`
		GitRef      string `json:"gitRef"`
		Description string `json:"description"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Source = strings.TrimSpace(req.Source)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	req.GitURL = strings.TrimSpace(req.GitURL)
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
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
		Name:        req.Name,
		Source:      req.Source,
		LocalPath:   req.LocalPath,
		GitURL:      req.GitURL,
		GitRef:      req.GitRef,
		Description: req.Description,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.store.CreateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "create project failed")
		return
	}
	// 添加项目即自动执行首次全量分析。先在处理协程里占住项目级锁再在后台跑，
	// 保证首次分析必然先于后续的显式分析/增量分析执行（否则两次全量分析谁后到
	// 谁重建 endpoint id，会失效客户端已拿到的引用）。创建响应不被阻塞。
	mu := s.intelAnalyzeMutex(p.ID)
	mu.Lock()
	go func() {
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.runIntelAnalyze(ctx, p.ID); err != nil {
			log.Printf("intel auto-analyze project %d: %v", p.ID, err)
		}
	}()
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
	s.applyModuleOverrides(ctx, id, mods)
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
		Name        string `json:"name"`
		Source      string `json:"source"`
		LocalPath   string `json:"localPath"`
		GitURL      string `json:"gitUrl"`
		GitRef      string `json:"gitRef"`
		Description string `json:"description"`
		Commands    string `json:"commandsJson"`
		EnvName     string `json:"envName"`
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
	// 记录来源相关字段是否发生变更：只有关联源码目录/仓库变化才触发增量分析，
	// 仅改名称/描述/命令白名单/环境不触发。
	prevSource := p.Source
	prevLocal := p.LocalPath
	prevGit := p.GitURL
	prevRef := p.GitRef
	if req.Name != "" {
		p.Name = req.Name
	}
	if req.Description != "" {
		p.Description = req.Description
	}
	// 关联源码目录/仓库：详情页「项目管理」可调整来源与路径，与创建语义一致。
	switch req.Source {
	case "":
		// 未携带 source 时按增量更新处理（如仅改命令白名单/名称）。
		if req.GitRef != "" {
			p.GitRef = req.GitRef
		}
	case "local", "git":
		req.LocalPath = strings.TrimSpace(req.LocalPath)
		req.GitURL = strings.TrimSpace(req.GitURL)
		req.GitRef = strings.TrimSpace(req.GitRef)
		if req.Source == "local" {
			if req.LocalPath == "" {
				writeErr(w, http.StatusBadRequest, "local 来源必须填写源码目录路径")
				return
			}
			if fi, err := os.Stat(req.LocalPath); err != nil || !fi.IsDir() {
				writeErr(w, http.StatusBadRequest, "源码目录不存在或不是目录: "+req.LocalPath)
				return
			}
			p.Source = "local"
			p.LocalPath = req.LocalPath
			p.GitURL = ""
		} else {
			if req.GitURL == "" {
				writeErr(w, http.StatusBadRequest, "git 来源必须填写仓库 URL")
				return
			}
			p.Source = "git"
			p.GitURL = req.GitURL
			p.GitRef = req.GitRef
			p.LocalPath = ""
		}
	default:
		writeErr(w, http.StatusBadRequest, "source 只能是 local 或 git")
		return
	}
	if req.Commands != "" {
		// 命令白名单是项目级别：必须是 JSON 字符串数组，且逐条去除首尾空白、
		// 丢弃空串，再落库（运行期按 strings.Fields 拆分 argv、不经 shell 执行）。
		var list []string
		if err := json.Unmarshal([]byte(req.Commands), &list); err != nil {
			writeErr(w, http.StatusBadRequest, "commandsJson 必须是 JSON 字符串数组")
			return
		}
		clean := make([]string, 0, len(list))
		for _, c := range list {
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
		p.CommandsJSON = string(b)
	}
	if req.EnvName != "" {
		p.EnvName = req.EnvName
	}
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "update project failed")
		return
	}
	// 关联源码目录/仓库发生变更时自动执行增量分析：仅扫描新增/移除的模块，
	// 已有模块数据保持不变（在独立后台 context 下进行，不阻塞保存响应）。
	sourceChanged := prevSource != p.Source || prevLocal != p.LocalPath ||
		prevGit != p.GitURL || prevRef != p.GitRef
	if sourceChanged {
		go s.autoAnalyzeIntel(id, true)
	}
	writeJSON(w, http.StatusOK, p)
}

// handleIntelProjectSources manages the project's associated source repos
// (多端多仓库): GET lists them, PUT replaces the whole list.
func (s *Server) handleIntelProjectSources(w http.ResponseWriter, r *http.Request, projectID int64) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		sources, err := s.store.ListIntelProjectSources(ctx, projectID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load project sources failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
	case http.MethodPut:
		var req struct {
			Sources []*struct {
				EndName   string `json:"endName"`
				Source    string `json:"source"`
				LocalPath string `json:"localPath"`
				GitURL    string `json:"gitUrl"`
				GitRef    string `json:"gitRef"`
			} `json:"sources"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		sources := make([]*store.IntelProjectSource, 0, len(req.Sources))
		seen := make(map[string]bool)
		for _, item := range req.Sources {
			if item == nil {
				continue
			}
			src := &store.IntelProjectSource{
				EndName:   strings.TrimSpace(item.EndName),
				Source:    strings.TrimSpace(item.Source),
				LocalPath: strings.TrimSpace(item.LocalPath),
				GitURL:    strings.TrimSpace(item.GitURL),
				GitRef:    strings.TrimSpace(item.GitRef),
			}
			if src.Source == "" {
				src.Source = "local"
			}
			switch src.Source {
			case "local":
				if src.LocalPath == "" {
					writeErr(w, http.StatusBadRequest, "local 关联源码必须填写目录路径")
					return
				}
				if fi, err := os.Stat(src.LocalPath); err != nil || !fi.IsDir() {
					writeErr(w, http.StatusBadRequest, "关联源码目录不存在或不是目录: "+src.LocalPath)
					return
				}
				src.GitURL = ""
				src.GitRef = ""
			case "git":
				if src.GitURL == "" {
					writeErr(w, http.StatusBadRequest, "git 关联源码必须填写仓库 URL")
					return
				}
				src.LocalPath = ""
			default:
				writeErr(w, http.StatusBadRequest, "source 只能是 local 或 git")
				return
			}
			if src.EndName != "" {
				if seen[src.EndName] {
					writeErr(w, http.StatusBadRequest, "端名不能重复: "+src.EndName)
					return
				}
				seen[src.EndName] = true
			}
			sources = append(sources, src)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		prev, _ := s.store.ListIntelProjectSources(ctx, projectID)
		if err := s.store.ReplaceIntelProjectSources(ctx, projectID, sources); err != nil {
			writeErr(w, http.StatusInternalServerError, "save project sources failed")
			return
		}
		// 关联源码列表发生变更时自动执行增量分析（模块增删按 rel_path 差分）。
		if !intelSourcesEqual(prev, sources) {
			go s.autoAnalyzeIntel(projectID, true)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sources": sources})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// intelDetectedModules is the outcome of detecting a project's module set
// across its primary root and any associated source repos.
type intelDetectedModules struct {
	root     string
	allRoots []string
	mods     []*store.IntelModule
	sources  []*store.IntelProjectSource
	scans    []intelModuleScan
}

// detectIntelModules resolves the project's working directory, detects modules
// across the primary root and every associated source repo (multi-端 multi-repo),
// and pairs each module with its owning repo root. Modules from associated repos
// get a "@<端>/" rel_path prefix so they stay unique against the main repo.
func (s *Server) detectIntelModules(ctx context.Context, p *store.IntelProject) (*intelDetectedModules, error) {
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, errNotDir(root)
	}
	out := &intelDetectedModules{root: root, allRoots: []string{root}}
	out.sources, _ = s.store.ListIntelProjectSources(ctx, p.ID)
	mainMods, err := intel.DetectModules(root)
	if err != nil {
		return nil, err
	}
	out.mods = append(out.mods, mainMods...)
	for _, src := range out.sources {
		srcRoot, err := s.resolveIntelSourceRoot(ctx, p, src)
		if err != nil {
			log.Printf("intel project %d source %d root: %v", p.ID, src.ID, err)
			continue
		}
		out.allRoots = append(out.allRoots, srcRoot)
		srcMods, err := intel.DetectModules(srcRoot)
		if err != nil {
			continue
		}
		prefix := intelSourcePrefix(src)
		for _, m := range srcMods {
			m.RelPath = prefix + m.RelPath
		}
		out.mods = append(out.mods, srcMods...)
	}
	// 每个模块的工作目录与其归属仓库根目录（绑定/来源路径按各自仓库相对）。
	for _, m := range out.mods {
		scanRoot, rel := root, m.RelPath
		if srcRoot, srcRel, ok := s.sourceModuleRoot(ctx, p, out.sources, m.RelPath); ok {
			scanRoot, rel = srcRoot, srcRel
		}
		dir := scanRoot
		if rel != "." {
			dir = filepath.Join(scanRoot, rel)
		}
		out.scans = append(out.scans, intelModuleScan{m: m, dir: dir, root: scanRoot, rel: rel})
	}
	return out, nil
}

// autoAnalyzeIntel runs a full or incremental analysis for a project outside
// the request's short-lived context. Full analysis runs on project creation;
// incremental runs whenever the project's associated source directory/repo is
// changed, so only added/removed modules are re-scanned. Errors are logged but
// never fail the enclosing request (the project itself was already saved).
// Analyses of the same project are serialized by intelAnalyzeMu; callers fire
// this via `go` so the HTTP handler never blocks on a multi-minute analyze.
func (s *Server) autoAnalyzeIntel(projectID int64, incremental bool) {
	mu := s.intelAnalyzeMutex(projectID)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var err error
	if incremental {
		err = s.runIntelAnalyzeIncremental(ctx, projectID)
	} else {
		err = s.runIntelAnalyze(ctx, projectID)
	}
	if err != nil {
		log.Printf("intel auto-analyze project %d (incremental=%v): %v", projectID, incremental, err)
		return
	}
	go s.reindexAfterAnalyze(projectID)
}

// intelAnalyzeMutex returns the per-project analyze mutex (serializes full and
// incremental analyses so concurrent creates/source-changes cannot corrupt the
// intel tables with racing replaces).
func (s *Server) intelAnalyzeMutex(projectID int64) *sync.Mutex {
	mu, _ := s.intelAnalyzeMu.LoadOrStore(projectID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// runIntelAnalyze resolves the project's working directory, detects modules,
// scans contracts per module and persists everything. It records snapshot sha
// (HEAD for git, directory-mtime-hash for local non-git).
func (s *Server) runIntelAnalyze(ctx context.Context, projectID int64) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	detected, err := s.detectIntelModules(ctx, p)
	if err != nil {
		return err
	}
	root := detected.root
	allRoots := detected.allRoots
	mods := detected.mods
	scans := detected.scans
	if err := s.store.ReplaceIntelModules(ctx, projectID, mods); err != nil {
		return err
	}
	mods, _ = s.store.ListIntelModules(ctx, projectID)
	p.CommandsJSON = intel.ProjectCommandsJSON(mods)
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		log.Printf("intel project commands %d: %v", projectID, err)
	}
	// 重新读取模块以拿到稳定 id（ReplaceIntelModules 返回后模块行才确定），
	// 再用新 id 覆盖 scan 中的模块引用。
	modsByRel := make(map[string]*store.IntelModule, len(mods))
	for _, m := range mods {
		modsByRel[m.RelPath] = m
	}
	for i := range scans {
		if m, ok := modsByRel[scans[i].m.RelPath]; ok {
			scans[i].m = m
		}
	}
	var allEndpoints []*store.IntelEndpoint
	var allEntities []*store.IntelEntity
	var allCases []*store.TestCase
	for _, sc := range scans {
		sum, err := intel.ScanModule(sc.root, sc.rel)
		if err != nil {
			continue
		}
		for i := range sum.Entities {
			sum.Entities[i].ModuleID = sc.m.ID
		}
		for i := range sum.Endpoints {
			sum.Endpoints[i].ModuleID = sc.m.ID
		}
		allEntities = append(allEntities, sum.Entities...)
		allEndpoints = append(allEndpoints, sum.Endpoints...)
		assets, err := testassets.Discover(sc.root, sc.rel)
		if err == nil {
			allCases = append(allCases, buildTestCases(sc.m, assets)...)
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
	if err := s.persistAndroidBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel android bindings project %d: %v", projectID, err)
	}
	if err := s.persistWebBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel web bindings project %d: %v", projectID, err)
	}
	if err := s.persistIosBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel ios bindings project %d: %v", projectID, err)
	}
	if err := s.persistGatewayRoutes(ctx, projectID, allRoots); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, allRoots)
	s.persistOverview(ctx, projectID, p, allRoots)
	s.persistEnvRequirements(ctx, projectID, allRoots)
	sha, err := snapshotSHA(root)
	if err != nil {
		sha = ""
	}
	if err := s.runIntelComplianceScan(ctx, projectID, allRoots); err != nil {
		log.Printf("intel compliance scan project %d: %v", projectID, err)
	}
	if err := s.runIntelSecurityScan(ctx, projectID, allEntities); err != nil {
		log.Printf("intel security scan project %d: %v", projectID, err)
	}
	s.recordImpact(ctx, projectID, p, root, sha)
	return s.store.MarkIntelProjectAnalyzed(ctx, projectID, sha)
}

// runIntelAnalyzeIncremental refreshes a project's analysis without a full
// rescan: it diffs the freshly-detected module set against the persisted one,
// deletes the removed modules' derived data (entities/endpoints/test cases) and
// only scans the newly-added modules (append). Project-level profiles — feature
// points, client bindings, gateway routes, overview, env requirements,
// compliance, security, impact and LLM summaries — are refreshed so the
// snapshot stays coherent without re-analyzing untouched modules. It is
// triggered automatically when a project's associated source directory or
// repository changes (module add/remove), and runs a full analysis otherwise.
func (s *Server) runIntelAnalyzeIncremental(ctx context.Context, projectID int64) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	detected, err := s.detectIntelModules(ctx, p)
	if err != nil {
		return err
	}
	existing, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return err
	}
	existingByRel := make(map[string]*store.IntelModule, len(existing))
	for _, m := range existing {
		existingByRel[m.RelPath] = m
	}
	newByRel := make(map[string]*store.IntelModule, len(detected.mods))
	for _, m := range detected.mods {
		newByRel[m.RelPath] = m
	}
	var added, removed []*store.IntelModule
	for rel, m := range newByRel {
		if _, ok := existingByRel[rel]; !ok {
			added = append(added, m)
		}
	}
	for rel, m := range existingByRel {
		if _, ok := newByRel[rel]; !ok {
			removed = append(removed, m)
		}
	}
	// 移除已删除模块的实体/接口/用例数据（按 project_id + module_id 删除）。
	for _, m := range removed {
		if err := s.store.DeleteIntelModuleEntities(ctx, projectID, m.ID); err != nil {
			return err
		}
		if err := s.store.DeleteIntelModuleEndpoints(ctx, projectID, m.ID); err != nil {
			return err
		}
		if err := s.store.DeleteIntelModuleTestCases(ctx, projectID, m.ID); err != nil {
			return err
		}
	}
	// 同步模块表：保留稳定 id，插入新增模块，删除移除模块。
	if err := s.store.ReplaceIntelModules(ctx, projectID, detected.mods); err != nil {
		return err
	}
	mods, _ := s.store.ListIntelModules(ctx, projectID)
	modsByRel := make(map[string]*store.IntelModule, len(mods))
	for _, m := range mods {
		modsByRel[m.RelPath] = m
	}
	// 项目级命令白名单随模块增删重新聚合（与全量分析一致）。
	p.CommandsJSON = intel.ProjectCommandsJSON(mods)
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		log.Printf("intel incremental commands %d: %v", projectID, err)
	}
	// 仅对新加模块做契约扫描并追加（已保留的模块数据不动）。
	for _, m := range added {
		persisted := modsByRel[m.RelPath]
		if persisted == nil {
			continue
		}
		var sc intelModuleScan
		for _, cand := range detected.scans {
			if cand.m.RelPath == m.RelPath {
				sc = cand
				sc.m = persisted
				break
			}
		}
		sum, err := intel.ScanModule(sc.root, sc.rel)
		if err != nil {
			continue
		}
		for i := range sum.Entities {
			sum.Entities[i].ModuleID = persisted.ID
		}
		for i := range sum.Endpoints {
			sum.Endpoints[i].ModuleID = persisted.ID
		}
		if err := s.store.AppendIntelEntities(ctx, projectID, sum.Entities); err != nil {
			return err
		}
		if err := s.store.AppendIntelEndpoints(ctx, projectID, sum.Endpoints); err != nil {
			return err
		}
		if assets, err := testassets.Discover(sc.root, sc.rel); err == nil {
			if err := s.store.AppendIntelTestCases(ctx, projectID, buildTestCases(persisted, assets)); err != nil {
				return err
			}
		}
	}
	// 用落库后的稳定 id 重建扫描列表，供绑定等全量刷新使用。
	freshScans := make([]intelModuleScan, 0, len(mods))
	for _, m := range mods {
		for _, cand := range detected.scans {
			if cand.m.RelPath == m.RelPath {
				cand.m = m
				freshScans = append(freshScans, cand)
				break
			}
		}
	}
	// 功能点随分析重算：基于当前全量接口重新聚类（保留人工标记）。
	endpoints, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil {
		return err
	}
	if err := s.persistFeatures(ctx, projectID, endpoints); err != nil {
		return err
	}
	// 项目级画像刷新：绑定 / 网关 / 概览 / 环境 / 合规 / 安全 / 影响 / LLM。
	if err := s.persistAndroidBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel android bindings project %d: %v", projectID, err)
	}
	if err := s.persistWebBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel web bindings project %d: %v", projectID, err)
	}
	if err := s.persistIosBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel ios bindings project %d: %v", projectID, err)
	}
	if err := s.persistGatewayRoutes(ctx, projectID, detected.allRoots); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, detected.allRoots)
	s.persistOverview(ctx, projectID, p, detected.allRoots)
	s.persistEnvRequirements(ctx, projectID, detected.allRoots)
	sha, err := snapshotSHA(detected.root)
	if err != nil {
		sha = ""
	}
	if err := s.runIntelComplianceScan(ctx, projectID, detected.allRoots); err != nil {
		log.Printf("intel compliance scan project %d: %v", projectID, err)
	}
	entities, err := s.store.ListIntelEntities(ctx, projectID, 0)
	if err != nil {
		return err
	}
	if err := s.runIntelSecurityScan(ctx, projectID, entities); err != nil {
		log.Printf("intel security scan project %d: %v", projectID, err)
	}
	s.recordImpact(ctx, projectID, p, detected.root, sha)
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

// ensureModuleSummary lazily generates (via the orchestration LLM) a concise
// business summary for a module from its endpoint contracts and entities, and
// caches it in project_modules.summary so repeated views hit the DB. When the
// LLM is not configured the module detail stays purely deterministic.
func (s *Server) ensureModuleSummary(ctx context.Context, mod *store.IntelModule, endpoints []*store.IntelEndpoint, entities []*store.IntelEntity) string {
	if s.llm == nil || !s.llm.Enabled() {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "子模块：%s（类型 %s / 角色 %s / 构建 %s）\n", mod.RelPath, mod.KindType, mod.KindRole, mod.BuildTool)
	if len(endpoints) > 0 {
		sb.WriteString("\n接口契约：\n")
		for i, ep := range endpoints {
			if i >= 30 {
				break
			}
			fmt.Fprintf(&sb, "- %s %s 返回=%s%s\n", ep.Method, ep.Path, ep.ResponseType,
				summarySuffix(ep.Summary))
		}
	}
	if len(entities) > 0 {
		sb.WriteString("\n实体 / 表：\n")
		seen := map[string]bool{}
		for _, e := range entities {
			if seen[e.TableName] {
				continue
			}
			seen[e.TableName] = true
			fmt.Fprintf(&sb, "- %s\n", e.TableName)
		}
	}
	if routes, err := s.store.ListIntelGatewayRoutes(ctx, mod.ProjectID); err == nil && len(routes) > 0 {
		sb.WriteString("\n网关路由上下文：\n")
		for i, rt := range routes {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&sb, "- %s -> %s %s\n", rt.Service, rt.URI, rt.PathsJSON)
		}
		sb.WriteString("（未列入网关直通路由的路径由网关兜底按服务名转发，认证类路径一般由网关放行/认证中心处理）\n")
	}
	var out struct {
		Summary string `json:"summary"`
	}
	system := loadPrompt("module_analyze", "你是子模块分析助手，基于给定接口与实体清单总结模块职责，输出 JSON {summary}。")
	if err := s.llm.CompleteJSON(ctx, system, sb.String(), &out); err != nil {
		return ""
	}
	summary := cleanLLMText(out.Summary, 800)
	if summary != "" {
		_ = s.store.UpdateIntelModuleSummary(ctx, mod.ID, summary)
		mod.Summary = summary
	}
	return summary
}

// summarySuffix appends a non-empty LLM summary hint for an endpoint.
func summarySuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return "（" + s + "）"
}

// persistFeatures clusters the extracted endpoints into candidate feature
// points and stores them. Human-created (manual) features are preserved across
// rescans: only auto-derived rows are replaced (人工优先，重扫不覆盖).
func (s *Server) persistFeatures(ctx context.Context, projectID int64, endpoints []*store.IntelEndpoint) error {
	var manual []*store.IntelFeature
	if prev, err := s.store.ListIntelFeatures(ctx, projectID); err == nil {
		for _, f := range prev {
			if f.Source == "manual" {
				manual = append(manual, f)
			}
		}
	}
	feats := feature.Cluster(endpoints)
	if len(feats) == 0 && len(manual) == 0 {
		return nil
	}
	storeFeats := make([]*store.IntelFeature, 0, len(feats)+len(manual))
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
	storeFeats = append(storeFeats, manual...)
	return s.store.ReplaceIntelFeatures(ctx, projectID, storeFeats)
}

// intelModuleScan couples one analyzed module with its working directory and
// owning repo root, so per-end (多端多仓库) modules resolve bindings/sources
// against their own repository rather than the project's primary root.
type intelModuleScan struct {
	m    *store.IntelModule
	dir  string // module working directory (repo root + rel path)
	root string // owning repo root
	rel  string // module rel path within root (no "@end/" prefix)
}

// persistGatewayRoutes discovers gateway route configuration under every scan
// root and replaces the project's route list once (multi-repo aware).
func (s *Server) persistGatewayRoutes(ctx context.Context, projectID int64, roots []string) error {
	routes := make([]*store.IntelGatewayRoute, 0)
	for _, root := range roots {
		found, err := gateway.Discover(root)
		if err != nil {
			continue
		}
		for _, r := range found {
			paths, _ := json.Marshal(r.Paths)
			routes = append(routes, &store.IntelGatewayRoute{
				Service:    r.Service,
				PathsJSON:  string(paths),
				URI:        r.URI,
				Source:     r.Source,
				SourceLine: r.SourceLine,
			})
		}
	}
	return s.store.ReplaceIntelGatewayRoutes(ctx, projectID, routes)
}

// persistAndroidBindings extracts Android DataBinding "page -> field path"
// bindings for every android module and persists them as the client's
// must-display field list (used later for CLIENT_MISSING_FIELD attribution).
func (s *Server) persistAndroidBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelAndroidBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "android" {
			continue
		}
		resDir := filepath.Join(sc.dir, "src", "main", "res")
		if fi, err := os.Stat(resDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := android.ExtractBindings(resDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, resDir, b.Source)
			bindings = append(bindings, &store.IntelAndroidBinding{
				ModuleID:   sc.m.ID,
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
func (s *Server) persistWebBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelWebBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "web" {
			continue
		}
		srcDir := filepath.Join(sc.dir, "src")
		if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := web.ExtractBindings(srcDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, srcDir, b.Source)
			bindings = append(bindings, &store.IntelWebBinding{
				ModuleID:   sc.m.ID,
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
func (s *Server) persistIosBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelIosBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "ios" {
			continue
		}
		if fi, err := os.Stat(sc.dir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := ios.ExtractBindings(sc.dir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, sc.dir, b.Source)
			bindings = append(bindings, &store.IntelIosBinding{
				ModuleID:   sc.m.ID,
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
// project's Markdown docs (across every scan root) plus the extracted endpoint
// contracts to the orchestration LLM and stores per-endpoint business summaries
// and (clearly marked) suggested gateway routes. It is a no-op unless the LLM
// is configured.
func (s *Server) enrichIntelWithLLM(ctx context.Context, projectID int64, roots []string) {
	if s.llm == nil || !s.llm.Enabled() {
		return
	}
	eps, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil || len(eps) == 0 {
		return
	}
	var sb strings.Builder
	for _, root := range roots {
		if sb.Len() >= 45000 {
			break
		}
		doc := enrich.Docs(root, 45000-sb.Len())
		if doc == "" {
			continue
		}
		sb.WriteString(doc)
		sb.WriteString("\n\n")
	}
	docs := strings.TrimSpace(sb.String())
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
// requirements and CycloneDX SBOM across every scan root, then stores them as a
// single overview snapshot for the detail view.
func (s *Server) persistOverview(ctx context.Context, projectID int64, p *store.IntelProject, roots []string) {
	all := collectDependencies(roots)
	reqs := detectRequirements(roots)
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

// detectRequirements runs env detection across every scan root and merges the
// per-repo requirement lists (deduplicated by service+category).
func detectRequirements(roots []string) []envdetect.Requirement {
	seen := make(map[string]bool)
	out := make([]envdetect.Requirement, 0)
	for _, root := range roots {
		found, err := envdetect.Detect(root, "")
		if err != nil {
			continue
		}
		for _, r := range found {
			key := r.Service
			if key == "" {
				key = r.Category
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

// collectDependencies walks every repository root for dependency manifests and
// returns a deduplicated dependency list across ecosystems.
func collectDependencies(roots []string) []deps.Dependency {
	seen := make(map[string]bool)
	out := make([]deps.Dependency, 0)
	for _, root := range roots {
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
	}
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

// intelSourcePrefix builds the unique rel-path token prefix for modules that
// belong to an associated source repo, e.g. "@android/". The end name is
// sanitized; a numeric id fallback keeps it unique when the end name is blank.
func intelSourcePrefix(src *store.IntelProjectSource) string {
	token := strings.TrimSpace(src.EndName)
	if token == "" {
		token = strconv.FormatInt(src.ID, 10)
	}
	var b strings.Builder
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return "@" + b.String() + "/"
}

// intelSourcesEqual reports whether two associated-source lists are equivalent,
// ignoring database ids (the id is not part of the PUT payload; the comparison
// is by the source-defining fields only).
func intelSourcesEqual(a, b []*store.IntelProjectSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.EndName != y.EndName || x.Source != y.Source ||
			x.LocalPath != y.LocalPath || x.GitURL != y.GitURL || x.GitRef != y.GitRef {
			return false
		}
	}
	return true
}

// resolveIntelSourceRoot resolves the working directory of an associated source
// repo: the local path when source=local, or a per-source git clone under the
// repos_dir cache (distinct from the main project clone).
func (s *Server) resolveIntelSourceRoot(ctx context.Context, p *store.IntelProject, src *store.IntelProjectSource) (string, error) {
	if src.Source == "local" && src.LocalPath != "" {
		return src.LocalPath, nil
	}
	reposDir, err := s.store.GetSetting(ctx, "intel.repos_dir")
	if err != nil || reposDir == "" {
		return "", errSettingMissing("intel.repos_dir not configured for git project sources")
	}
	target := filepath.Join(reposDir, safeName(p.Name)+"-src-"+strconv.FormatInt(src.ID, 10))
	clone := &store.IntelProject{Name: p.Name, GitURL: src.GitURL, GitRef: src.GitRef}
	if err := s.ensureGitClone(ctx, clone, target); err != nil {
		return "", err
	}
	return target, nil
}

// sourceModuleRoot resolves the working directory for a module whose rel_path
// may belong to an associated source repo ("@end/..."). ok is false when the
// module lives in the project's primary root.
func (s *Server) sourceModuleRoot(ctx context.Context, p *store.IntelProject, sources []*store.IntelProjectSource, relPath string) (string, string, bool) {
	if !strings.HasPrefix(relPath, "@") {
		return "", "", false
	}
	slash := strings.IndexByte(relPath, '/')
	if slash <= 0 {
		return "", "", false
	}
	token := relPath[1:slash]
	for _, src := range sources {
		prefixToken := strings.TrimSuffix(strings.TrimPrefix(intelSourcePrefix(src), "@"), "/")
		if token != prefixToken {
			continue
		}
		root, err := s.resolveIntelSourceRoot(ctx, p, src)
		if err != nil {
			return "", "", false
		}
		return root, relPath[slash+1:], true
	}
	return "", "", false
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
