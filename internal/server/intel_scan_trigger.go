package server

// Manual trigger endpoints for artifacts that the analyze pass regenerates
// implicitly: SBOM/overview regeneration and the compliance/security findings
// rescan. POST /api/intel/scan/findings and POST /api/intel/sync/scan share one
// implementation; both scan only, never executing tests.

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// intelAllRoots resolves the project's primary working directory together with
// every associated source repo root — the same root set analyze feeds into
// persistOverview and the compliance scan. Source roots that fail to resolve
// are skipped so a broken association degrades to the primary root only.
func (s *Server) intelAllRoots(ctx context.Context, p *store.IntelProject) ([]string, error) {
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, err
	}
	roots := []string{root}
	sources, _ := s.store.ListIntelProjectSources(ctx, p.ID)
	for _, src := range sources {
		srcRoot, err := s.resolveIntelSourceRoot(ctx, p, src)
		if err != nil {
			log.Printf("intel project %d source %d root: %v", p.ID, src.ID, err)
			continue
		}
		roots = append(roots, srcRoot)
	}
	return roots, nil
}

// handleIntelSbomTrigger regenerates the project's overview snapshot (deps +
// env + CycloneDX SBOM) on demand and returns the fresh overview. SBOM
// generation is best-effort: when it fails the stored overview still carries
// the deps/env JSON and the caller gets a 200 with a note instead of a 500.
func (s *Server) handleIntelSbomTrigger(w http.ResponseWriter, r *http.Request) {
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
	roots, err := s.intelAllRoots(ctx, p)
	if err != nil {
		log.Printf("intel sbom project %d roots: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "resolve project root failed")
		return
	}
	// 复用 analyze 的概览/SBOM 生成逻辑，避免两处实现漂移。
	s.persistOverview(ctx, req.ProjectID, p, roots)
	ov, err := s.store.GetIntelOverview(ctx, req.ProjectID)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"overview": nil, "note": "overview unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overview": ov})
}

// handleIntelScanFindings re-runs the compliance/security scans for a project
// and reports the findings that were newly created. detectors filters which
// scans run ("rule" = compliance rules, "security" = sensitive-field detector);
// an empty list runs both. Only scanning happens here — no tests are executed.
func (s *Server) handleIntelScanFindings(w http.ResponseWriter, r *http.Request) {
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
		ProjectID int64    `json:"projectId"`
		Detectors []string `json:"detectors"`
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
	roots, err := s.intelAllRoots(ctx, p)
	if err != nil {
		log.Printf("intel scan project %d roots: %v", req.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "resolve project root failed")
		return
	}
	wants := func(det string) bool {
		if len(req.Detectors) == 0 {
			return true
		}
		for _, d := range req.Detectors {
			if strings.TrimSpace(d) == det {
				return true
			}
		}
		return false
	}
	// 记录扫描前的 finding id 集合，用于统计本次真正新产生的行。
	before := make(map[int64]bool)
	if prev, err := s.store.ListIntelFindings(ctx, req.ProjectID, "", ""); err == nil {
		for _, f := range prev {
			before[f.ID] = true
		}
	}
	if wants("rule") {
		if err := s.runIntelComplianceScan(ctx, req.ProjectID, roots); err != nil {
			log.Printf("intel compliance scan project %d: %v", req.ProjectID, err)
		}
	}
	if wants("security") {
		entities, err := s.store.ListIntelEntities(ctx, req.ProjectID, 0)
		if err != nil {
			log.Printf("intel scan project %d entities: %v", req.ProjectID, err)
		}
		if err := s.runIntelSecurityScan(ctx, req.ProjectID, entities); err != nil {
			log.Printf("intel security scan project %d: %v", req.ProjectID, err)
		}
	}
	after, err := s.store.ListIntelFindings(ctx, req.ProjectID, "", "")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load findings failed")
		return
	}
	fresh := make([]*store.IntelFinding, 0, len(after))
	for _, f := range after {
		if !before[f.ID] {
			fresh = append(fresh, f)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": len(fresh), "findings": fresh})
}
