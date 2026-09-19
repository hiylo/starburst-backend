package server

// Read-side inventory APIs: endpoints, entities, module bindings, modules,
// gateway routes, impact and overview -- including the read-time merge of
// human corrections into the rows they return.

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

import (
	"github.com/hiylo/starburst-backend/internal/intel/gateway"
	"github.com/hiylo/starburst-backend/internal/store"
)

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
	// The route is registered as /api/intel/modules, so the project can only
	// come from ?projectId= — reading the id out of the path (as this used to)
	// never matched and silently answered 200 with an empty body.
	projectID, ok := s.intelQueryProject(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	mods, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load modules failed")
		return
	}
	s.applyModuleOverrides(ctx, projectID, mods)
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
