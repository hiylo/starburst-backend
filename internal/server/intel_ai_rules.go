package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelAIRules lists (GET) or creates (POST) AI suggestion rules. Rules
// are global prompt rules; enabling/ordering drives the scan pass.
func (s *Server) handleIntelAIRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		rules, err := s.store.ListIntelAIRules(ctx, r.URL.Query().Get("enabled") == "true")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load rules failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
	case http.MethodPost:
		var req struct {
			Name      string `json:"name"`
			Prompt    string `json:"prompt"`
			Scope     string `json:"scope"`
			Target    string `json:"target"`
			Severity  string `json:"severity"`
			Enabled   *bool  `json:"enabled"`
			SortOrder *int   `json:"sortOrder"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Prompt) == "" {
			writeErr(w, http.StatusBadRequest, "name and prompt are required")
			return
		}
		rule := &store.IntelAIRule{
			Name:     strings.TrimSpace(req.Name),
			Prompt:   strings.TrimSpace(req.Prompt),
			Scope:    defStr(req.Scope, "all"),
			Target:   defStr(req.Target, "risk"),
			Severity: defStr(req.Severity, "medium"),
			Enabled:  true,
		}
		if req.Enabled != nil {
			rule.Enabled = *req.Enabled
		}
		if req.SortOrder != nil {
			rule.SortOrder = *req.SortOrder
		}
		if err := s.store.CreateIntelAIRule(ctx, rule); err != nil {
			writeErr(w, http.StatusInternalServerError, "create rule failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rule": rule})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelAIRuleByID updates, deletes or polishes one AI rule. Path forms:
// /api/intel/ai-rules/{id} (make it also carry the id from path).
func (s *Server) handleIntelAIRuleByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/ai-rules/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid rule id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if len(parts) == 2 && parts[1] == "polish" {
		s.polishIntelAIRule(w, r, ctx, id)
		return
	}

	switch r.Method {
	case http.MethodPut:
		rule, err := s.store.GetIntelAIRule(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "rule not found")
			return
		}
		var req struct {
			Name      string `json:"name"`
			Prompt    string `json:"prompt"`
			Scope     string `json:"scope"`
			Target    string `json:"target"`
			Severity  string `json:"severity"`
			Enabled   *bool  `json:"enabled"`
			SortOrder *int   `json:"sortOrder"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.Name != "" {
			rule.Name = strings.TrimSpace(req.Name)
		}
		if req.Prompt != "" {
			rule.Prompt = strings.TrimSpace(req.Prompt)
		}
		if req.Scope != "" {
			rule.Scope = req.Scope
		}
		if req.Target != "" {
			rule.Target = req.Target
		}
		if req.Severity != "" {
			rule.Severity = req.Severity
		}
		if req.Enabled != nil {
			rule.Enabled = *req.Enabled
		}
		if req.SortOrder != nil {
			rule.SortOrder = *req.SortOrder
		}
		if err := s.store.UpdateIntelAIRule(ctx, rule); err != nil {
			writeErr(w, http.StatusInternalServerError, "update rule failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rule": rule})
	case http.MethodDelete:
		if err := s.store.DeleteIntelAIRule(ctx, id); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete rule failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// polishIntelAIRule asks the orchestration LLM to refine a rule's prompt and
// returns the polished draft (it does not persist until the user confirms).
func (s *Server) polishIntelAIRule(w http.ResponseWriter, r *http.Request, ctx context.Context, id int64) {
	if r.Method != http.MethodPut {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusBadRequest, "orchestration LLM is not configured")
		return
	}
	rule, err := s.store.GetIntelAIRule(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "rule not found")
		return
	}
	var draft struct {
		Prompt string `json:"prompt"`
	}
	system := "你是测试提示词优化助手。基于现有规则提示词，输出改写后的更精确版本（JSON {\"prompt\":\"...\"}）。保持原文意图。"
	if err := s.llm.CompleteJSON(ctx, system, rule.Prompt, &draft); err != nil {
		writeErr(w, http.StatusInternalServerError, "LLM 润色失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"polished": draft.Prompt})
}

// handleIntelRuleScan runs the selected AI rules over the project's indexed
// knowledge (contracts, docs, bindings) via the orchestration LLM and persists
// the surfaced issues as findings with detector=ai-rule.
func (s *Server) handleIntelRuleScan(w http.ResponseWriter, r *http.Request) {
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
		ProjectID int64   `json:"projectId"`
		RuleIDs   []int64 `json:"ruleIds"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusBadRequest, "orchestration LLM is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	rules, err := s.store.ListIntelAIRules(ctx, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load rules failed")
		return
	}
	chunks, err := s.buildIntelChunks(ctx, req.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load knowledge failed")
		return
	}
	selected := selectedRules(rules, req.RuleIDs)
	count := 0
	for _, rule := range selected {
		for _, chunk := range chunks {
			if len(chunk.Content) > 2000 {
				chunk.Content = chunk.Content[:2000] + "…"
			}
			loc := chunk.SourceFile
			if chunk.SourceLine > 0 {
				loc += ":" + strconv.Itoa(chunk.SourceLine)
			}
			ok, err := s.runSingleAIRule(ctx, req.ProjectID, rule, chunk.Title, chunk.Content, loc)
			if err != nil {
				log.Printf("intel ai-rule %d chunk %s: %v", rule.ID, chunk.Title, err)
				continue
			}
			if ok {
				count++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": count, "rules": len(selected)})
}

// selectedRules filters rules to the given ids (all when ids are empty).
func selectedRules(rules []*store.IntelAIRule, ids []int64) []*store.IntelAIRule {
	if len(ids) == 0 {
		return rules
	}
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]*store.IntelAIRule, 0, len(ids))
	for _, r := range rules {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// runSingleAIRule asks the LLM whether a rule applies to one knowledge chunk and
// persists a finding when it does. It returns whether a finding was created.
func (s *Server) runSingleAIRule(ctx context.Context, projectID int64, rule *store.IntelAIRule, title, content, location string) (bool, error) {
	var result struct {
		Find   bool   `json:"find"`
		Reason string `json:"reason"`
	}
	system := loadPrompt("ai_rule_scan", "你是规则扫描助手。判断给定规则是否命中给定内容片段，只输出 JSON {find:true/false, reason}。")
	user := fmt.Sprintf("规则[%s]:%s\n\n内容[%s]:\n%s", rule.Name, rule.Prompt, title, content)
	if err := s.llm.CompleteJSON(ctx, system, user, &result); err != nil {
		return false, err
	}
	if !result.Find {
		return false, nil
	}
	reason := cleanLLMText(result.Reason, 500)
	if reason == "" {
		reason = rule.Name + " 命中"
	}
	f := &store.IntelFinding{
		ProjectID:   projectID,
		Detector:    "ai-rule",
		Severity:    rule.Severity,
		Category:    rule.Target,
		CveOrRuleID: strconv.FormatInt(rule.ID, 10),
		Location:    location,
		Summary:     reason,
		Status:      "open",
	}
	_, err := s.store.CreateIntelFindingIfAbsent(ctx, f)
	return err == nil, err
}

func defStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
