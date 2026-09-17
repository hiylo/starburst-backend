package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/compliance"
	"github.com/hiylo/starburst-backend/internal/intel/fix"
	"github.com/hiylo/starburst-backend/internal/intel/security"
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

// handleIntelFixGenerate asks the orchestration LLM to propose a minimal code
// fix for a finding, renders it as a unified diff (via the fix package) and
// persists an ai-suggest fix for human review.
func (s *Server) handleIntelFixGenerate(w http.ResponseWriter, r *http.Request) {
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
		FindingID int64 `json:"findingId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || req.FindingID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId and findingId are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	created, err := s.generateFixForFinding(ctx, req.ProjectID, req.FindingID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate fix failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fix": created})
}

// generateFixForFinding loads the finding, reads the referenced source file,
// asks the LLM for a minimal edit and renders a diff draft. It validates that
// the model's oldText matches the file exactly once before storing.
func (s *Server) generateFixForFinding(ctx context.Context, projectID, findingID int64) (*store.IntelFix, error) {
	if s.llm == nil || !s.llm.Enabled() {
		return nil, fmt.Errorf("orchestration LLM is not configured")
	}
	finding, err := s.store.GetIntelFinding(ctx, findingID)
	if err != nil {
		return nil, fmt.Errorf("finding %d: %w", findingID, err)
	}
	relFile, line := splitLocation(finding.Location)
	if relFile == "" {
		return nil, fmt.Errorf("finding has no file location")
	}
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, err
	}
	abs := filepath.Join(root, filepath.FromSlash(relFile))
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", relFile, err)
	}
	content := string(data)
	if len(content) > 100*1024 {
		content = content[:100*1024]
	}

	var proposal struct {
		Title   string `json:"title"`
		OldText string `json:"oldText"`
		NewText string `json:"newText"`
	}
	system := "你是代码修复助手。根据告警信息和源文件片段，提出一个最小、精确的修复。" +
		"只输出 JSON：{\"title\":\"...\",\"oldText\":\"...\",\"newText\":\"...\"}。" +
		"oldText 必须逐字来自源文件（唯一出现），newText 是替换后的内容。"
	user := fmt.Sprintf("告警：%s\n位置：%s:%d\n\n源文件 %s 片段：\n%s",
		finding.Summary, relFile, line, relFile, content)
	if err := s.llm.CompleteJSON(ctx, system, user, &proposal); err != nil {
		return nil, err
	}
	proposal.OldText = strings.TrimSpace(proposal.OldText)
	if proposal.OldText == "" || proposal.OldText == proposal.NewText {
		return nil, fmt.Errorf("model returned no usable edit")
	}
	if n := strings.Count(content, proposal.OldText); n != 1 {
		return nil, fmt.Errorf("model oldText is not a unique match (%d occurrences)", n)
	}

	diff, err := fix.GeneratePatch(map[string]string{relFile: content}, []*fix.Suggestion{{
		File:    relFile,
		OldText: proposal.OldText,
		NewText: proposal.NewText,
		Line:    line,
	}})
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(proposal.Title)
	if title == "" {
		title = finding.Summary
	}
	stored := &store.IntelFix{
		ProjectID: projectID,
		FindingID: findingID,
		Kind:      "ai-suggest",
		Title:     title,
		DiffJSON:  diff,
		Status:    "proposed",
	}
	if err := s.store.CreateIntelFix(ctx, stored); err != nil {
		return nil, err
	}
	return stored, nil
}

// splitLocation parses a "path/file:line" location into its file and line
// parts. A location without a line yields line 0.
func splitLocation(loc string) (string, int) {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return "", 0
	}
	idx := strings.LastIndex(loc, ":")
	if idx < 0 {
		return loc, 0
	}
	line, err := strconv.Atoi(loc[idx+1:])
	if err != nil {
		return loc, 0
	}
	return loc[:idx], line
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
		if _, err := s.store.CreateIntelFindingIfAbsent(ctx, finding); err != nil {
			log.Printf("intel compliance finding: %v", err)
		}
	}
	return nil
}

// runIntelSecurityScan runs the deterministic sensitive-field detector over the
// scanned entity columns and records security findings (detector=security) so
// password/token/id-card/bank-card/mobile/amount fields are surfaced in the
// audit view. Findings are deduplicated by location+rule.
func (s *Server) runIntelSecurityScan(ctx context.Context, projectID int64, entities []*store.IntelEntity) error {
	if len(entities) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	for _, e := range entities {
		w := security.DetectField(security.Field{Name: e.ColumnName, Type: e.FieldType})
		if w == nil {
			continue
		}
		loc := fmt.Sprintf("%s:%d", e.SourceFile, e.SourceLine)
		key := loc + "|" + w.Field + "|" + w.Kind
		if seen[key] {
			continue
		}
		seen[key] = true
		finding := &store.IntelFinding{
			ProjectID:   projectID,
			ModuleID:    e.ModuleID,
			Detector:    "security",
			Severity:    string(w.Severity),
			Category:    "sensitive_field",
			CveOrRuleID: w.Kind,
			Location:    loc,
			Summary:     w.Message,
			Status:      "open",
		}
		if _, err := s.store.CreateIntelFindingIfAbsent(ctx, finding); err != nil {
			log.Printf("intel security finding: %v", err)
		}
	}
	return nil
}
