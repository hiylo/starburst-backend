package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelPending lists the project's pending (待确认) overrides.
func (s *Server) handleIntelPending(w http.ResponseWriter, r *http.Request) {
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
	pending, err := s.store.ListIntelOverrides(ctx, projectID, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load pending failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": pending})
}

// handleIntelPendingConfirm confirms a pending override: the human finalizes
// the manual value (kept) and the row moves to applied.
func (s *Server) handleIntelPendingConfirm(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/pending/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, "/confirm")
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid override id")
		return
	}
	var req struct {
		ManualValue *string `json:"manualValue"`
		Status      string  `json:"status"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	o, err := s.store.GetIntelOverride(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "override not found")
		return
	}
	if req.ManualValue != nil {
		o.ManualValue = *req.ManualValue
	}
	switch req.Status {
	case "applied", "rejected":
		o.Status = req.Status
	default:
		o.Status = "applied"
	}
	if err := s.store.UpdateIntelOverride(ctx, o); err != nil {
		writeErr(w, http.StatusInternalServerError, "confirm override failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"override": o})
}

// handleIntelOverrides applies a batch of confirmed overrides (人工最后拍板).
func (s *Server) handleIntelOverrides(w http.ResponseWriter, r *http.Request) {
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
		Overrides []struct {
			ModuleID    int64  `json:"moduleId"`
			Target      string `json:"target"`
			RowKey      string `json:"rowKey"`
			Field       string `json:"field"`
			ManualValue string `json:"manualValue"`
			Confidence  string `json:"confidence"`
			Source      string `json:"source"`
		} `json:"overrides"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || len(req.Overrides) == 0 {
		writeErr(w, http.StatusBadRequest, "projectId and overrides are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	applied := 0
	for _, ov := range req.Overrides {
		if strings.TrimSpace(ov.ManualValue) == "" {
			continue
		}
		o := &store.IntelOverride{
			ProjectID:   req.ProjectID,
			ModuleID:    ov.ModuleID,
			Target:      ov.Target,
			RowKey:      ov.RowKey,
			Field:       ov.Field,
			ManualValue: ov.ManualValue,
			Confidence:  ov.Confidence,
			Status:      "applied",
			Source:      ov.Source,
		}
		if o.Confidence == "" {
			o.Confidence = "high"
		}
		if o.Source == "" {
			o.Source = "manual"
		}
		if err := s.store.CreateIntelOverride(ctx, o); err != nil {
			continue
		}
		applied++
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": applied})
}

// handleIntelOverrideSuggest asks the orchestration LLM to propose pending
// overrides (drafts previewed, not applied). Requires LLM configuration.
func (s *Server) handleIntelOverrideSuggest(w http.ResponseWriter, r *http.Request) {
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
	if s.llm == nil || !s.llm.Enabled() {
		writeErr(w, http.StatusBadRequest, "orchestration LLM is not configured")
		return
	}
	var req struct {
		ProjectID   int64  `json:"projectId"`
		Instruction string `json:"instruction"`
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
	mods, err := s.store.ListIntelModules(ctx, req.ProjectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load modules failed")
		return
	}
	var sb strings.Builder
	sb.WriteString("项目子模块（rel_path / kind_role / kind_type）：\n")
	for _, m := range mods {
		fmt.Fprintf(&sb, "- %s / %s / %s\n", m.RelPath, m.KindRole, m.KindType)
	}
	type propose struct {
		Overrides []struct {
			Target      string `json:"target"`
			RowKey      string `json:"rowKey"`
			Field       string `json:"field"`
			AutoValue   string `json:"autoValue"`
			ManualValue string `json:"manualValue"`
			Reason      string `json:"reason"`
		} `json:"overrides"`
	}
	var draft propose
	system := "你是测试智能人工校正助手。基于子模块清单与用户指令，提出需要人工确认的字段覆写（JSON {\"overrides\":[{target,rowKey,field,autoValue,manualValue,reason}]}）。只提有明确依据的，宁缺毋滥。"
	user := sb.String()
	if strings.TrimSpace(req.Instruction) != "" {
		user += "\n用户指令：" + req.Instruction
	}
	if err := s.llm.CompleteJSON(ctx, system, user, &draft); err != nil {
		writeErr(w, http.StatusInternalServerError, "LLM 建议失败: "+err.Error())
		return
	}
	// Deterministic gate: reject drafts without an anchorable target/field and a
	// final manual value; cap the batch and field lengths.
	valid := make([]struct {
		Target      string `json:"target"`
		RowKey      string `json:"rowKey"`
		Field       string `json:"field"`
		AutoValue   string `json:"autoValue"`
		ManualValue string `json:"manualValue"`
		Reason      string `json:"reason"`
	}, 0, len(draft.Overrides))
	for _, o := range draft.Overrides {
		o.Target = cleanLLMText(o.Target, 64)
		o.RowKey = cleanLLMText(o.RowKey, 128)
		o.Field = cleanLLMText(o.Field, 64)
		o.AutoValue = cleanLLMText(o.AutoValue, 200)
		o.ManualValue = cleanLLMText(o.ManualValue, 200)
		o.Reason = cleanLLMText(o.Reason, 300)
		if o.Target == "" || o.Field == "" || o.ManualValue == "" {
			continue
		}
		valid = append(valid, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"drafts": capLLMList(valid, 20)})
}
