package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/compliance"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelFindings lists audit findings for a project, filterable by status
// and detector.
func (s *Server) handleIntelFindings(w http.ResponseWriter, r *http.Request) {
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
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	detector := strings.TrimSpace(r.URL.Query().Get("detector"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	findings, err := s.store.ListIntelFindings(ctx, projectID, status, detector)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load findings failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"findings": findings})
}

// handleIntelFindingWaive marks a finding as false-positive or waived (accepted
// risk) with a mandatory reason, or reopens it.
func (s *Server) handleIntelFindingWaive(w http.ResponseWriter, r *http.Request) {
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
	id, ok := intelPathID(w, r, "/api/intel/findings/")
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status"` // false_positive | waived | open
		Reason string `json:"reason"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Status = strings.TrimSpace(req.Status)
	if req.Status != "false_positive" && req.Status != "waived" && req.Status != "open" {
		writeErr(w, http.StatusBadRequest, "status must be false_positive|waived|open")
		return
	}
	if req.Status != "open" && strings.TrimSpace(req.Reason) == "" {
		writeErr(w, http.StatusBadRequest, "reason is required to waive")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	f := &store.IntelFinding{ID: id, Status: req.Status, WaivedReason: req.Reason}
	if err := s.store.UpdateIntelFinding(ctx, f); err != nil {
		writeErr(w, http.StatusInternalServerError, "update finding failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIntelFixes lists fix suggestions for a project.
func (s *Server) handleIntelFixes(w http.ResponseWriter, r *http.Request) {
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
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	fixes, err := s.store.ListIntelFixes(ctx, projectID, status)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load fixes failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fixes": fixes})
}

// handleIntelFixAction applies or rejects a fix suggestion.
func (s *Server) handleIntelFixAction(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/fixes/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusBadRequest, "expected /api/intel/fixes/{id}/{action}")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid fix id")
		return
	}
	action := parts[1]
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	fix, err := s.store.GetIntelFix(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "fix not found")
		return
	}
	switch action {
	case "apply":
		fix.Status = "applied"
	case "reject":
		fix.Status = "rejected"
	default:
		writeErr(w, http.StatusBadRequest, "action must be apply|reject")
		return
	}
	if err := s.store.UpdateIntelFix(ctx, fix); err != nil {
		writeErr(w, http.StatusInternalServerError, "update fix failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": fix.Status})
}

// runIntelComplianceScan runs the deterministic compliance rules over the
// project working tree and persists findings (detector=rule). It is invoked
// after a successful analyze so the audit view reflects the current code.
func (s *Server) runIntelComplianceScan(ctx context.Context, projectID int64, root string) error {
	findings, err := compliance.ScanDir(root)
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		return nil
	}
	for _, f := range findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		finding := &store.IntelFinding{
			ProjectID:   projectID,
			Detector:    "rule",
			Severity:    f.Severity,
			Category:    "compliance",
			CveOrRuleID: f.RuleID,
			Location:    loc,
			Summary:     f.Message,
			Status:      "open",
		}
		if err := s.store.CreateIntelFinding(ctx, finding); err != nil {
			log.Printf("intel compliance finding: %v", err)
		}
	}
	return nil
}
