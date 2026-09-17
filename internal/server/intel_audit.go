package server

import (
	"context"
	"encoding/json"
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

// handleIntelFixAction applies, rejects or rolls back a fix suggestion. Apply
// writes the approved edit back to the project working tree (with a backup for
// one-click rollback); rollback restores the original content.
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

	fx, err := s.store.GetIntelFix(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "fix not found")
		return
	}
	switch action {
	case "apply":
		if fx.Status != "proposed" {
			writeErr(w, http.StatusBadRequest, "fix is not in proposed state")
			return
		}
		if err := s.applyIntelFix(ctx, fx); err != nil {
			writeErr(w, http.StatusInternalServerError, "应用修复失败: "+err.Error())
			return
		}
		s.markFindingFixed(ctx, fx.FindingID)
	case "reject":
		fx.Status = "rejected"
	case "rollback":
		if fx.Status != "applied" {
			writeErr(w, http.StatusBadRequest, "fix is not applied")
			return
		}
		if err := s.rollbackIntelFix(ctx, fx); err != nil {
			writeErr(w, http.StatusInternalServerError, "回滚失败: "+err.Error())
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "action must be apply|reject|rollback")
		return
	}
	if err := s.store.UpdateIntelFix(ctx, fx); err != nil {
		writeErr(w, http.StatusInternalServerError, "update fix failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": fx.Status})
}

// applyIntelFix writes a proposed fix's edits into the project working tree,
// validating each oldText against the current file content. The original
// content of every touched file is captured into AppliedBackup for rollback.
func (s *Server) applyIntelFix(ctx context.Context, rec *store.IntelFix) error {
	var suggestions []*fix.Suggestion
	if err := json.Unmarshal([]byte(rec.DiffJSON), &suggestions); err != nil {
		return fmt.Errorf("diff_json 不是合法的建议列表: %w", err)
	}
	if len(suggestions) == 0 {
		return fmt.Errorf("diff_json 为空")
	}
	p, err := s.store.GetIntelProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return err
	}
	backups := make(map[string]string)
	for _, sg := range suggestions {
		if sg == nil || sg.File == "" {
			continue
		}
		abs, err := intelResolveRepoPath(root, sg.File)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("读取 %s: %w", sg.File, err)
		}
		if _, ok := backups[sg.File]; !ok {
			backups[sg.File] = string(data)
		}
		newContent, err := fix.ApplyDryRun(string(data), sg)
		if err != nil {
			return fmt.Errorf("应用 %s: %w", sg.File, err)
		}
		if err := os.WriteFile(abs, []byte(newContent), 0o644); err != nil {
			return fmt.Errorf("写入 %s: %w", sg.File, err)
		}
	}
	buf, _ := json.Marshal(backups)
	rec.AppliedBackup = string(buf)
	rec.Status = "applied"
	return nil
}

// markFindingFixed closes the loop when a fix is applied: the linked finding
// moves to fixed so the audit view reflects the human-confirmed remediation.
func (s *Server) markFindingFixed(ctx context.Context, findingID int64) {
	if findingID <= 0 {
		return
	}
	f, err := s.store.GetIntelFinding(ctx, findingID)
	if err != nil {
		return
	}
	f.Status = "fixed"
	_ = s.store.UpdateIntelFinding(ctx, f)
}

// rollbackIntelFix restores the pre-apply content of every file a fix touched.
func (s *Server) rollbackIntelFix(ctx context.Context, rec *store.IntelFix) error {
	var backups map[string]string
	if err := json.Unmarshal([]byte(rec.AppliedBackup), &backups); err != nil || len(backups) == 0 {
		return fmt.Errorf("无可用备份，无法回滚")
	}
	p, err := s.store.GetIntelProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return err
	}
	for file, orig := range backups {
		abs, err := intelResolveRepoPath(root, file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(abs, []byte(orig), 0o644); err != nil {
			return fmt.Errorf("回滚 %s: %w", file, err)
		}
	}
	rec.Status = "rolled_back"
	return nil
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
	system := loadPrompt("fix_generate", "你是代码修复助手，提出最小精确修复，输出 JSON {title,oldText,newText}，oldText 必须逐字来自源文件。")
	user := fmt.Sprintf("告警：%s\n位置：%s:%d\n\n源文件 %s 片段：\n%s",
		finding.Summary, relFile, line, relFile, content)
	if err := s.llm.CompleteJSON(ctx, system, user, &proposal); err != nil {
		return nil, err
	}
	proposal.OldText = strings.TrimSpace(proposal.OldText)
	proposal.NewText = cleanLLMText(proposal.NewText, 4000)
	if proposal.OldText == "" || proposal.OldText == proposal.NewText {
		return nil, fmt.Errorf("model returned no usable edit")
	}
	if n := strings.Count(content, proposal.OldText); n != 1 {
		return nil, fmt.Errorf("model oldText is not a unique match (%d occurrences)", n)
	}

	suggestions := []*fix.Suggestion{{
		File:       relFile,
		OldText:    proposal.OldText,
		NewText:    proposal.NewText,
		Line:       line,
		Confidence: "high",
	}}
	suggJSON, err := json.Marshal(suggestions)
	if err != nil {
		return nil, err
	}
	title := cleanLLMText(proposal.Title, 200)
	if title == "" {
		title = finding.Summary
	}
	stored := &store.IntelFix{
		ProjectID: projectID,
		FindingID: findingID,
		Kind:      "ai-suggest",
		Title:     title,
		DiffJSON:  string(suggJSON),
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

// runIntelComplianceScan runs the deterministic compliance rules over every
// scan root (main + associated repos) and persists findings (detector=rule). It
// is invoked after a successful analyze so the audit view reflects the code.
func (s *Server) runIntelComplianceScan(ctx context.Context, projectID int64, roots []string) error {
	for _, root := range roots {
		findings, err := compliance.ScanDir(root)
		if err != nil {
			continue
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
