package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleIntelFeatureOrder persists a user drag-reordered feature sequence
// (人工优先：重扫不覆盖 sort_order).
func (s *Server) handleIntelFeatureOrder(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		ProjectID int64   `json:"projectId"`
		Order     []int64 `json:"order"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || len(req.Order) == 0 {
		writeErr(w, http.StatusBadRequest, "projectId and order are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	for i, id := range req.Order {
		feat, err := s.store.GetIntelFeature(ctx, id)
		if err != nil || feat.ProjectID != req.ProjectID {
			continue
		}
		if feat.SortOrder == i {
			continue
		}
		feat.SortOrder = i
		_ = s.store.UpdateIntelFeature(ctx, feat)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleIntelFeatureByID renames a feature or adjusts its ends/status
// (PUT /api/intel/features/{id}).
func (s *Server) handleIntelFeatureByID(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/features/")
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		writeErr(w, http.StatusBadRequest, "expected /api/intel/features/{id}")
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid feature id")
		return
	}
	var req struct {
		Name    string   `json:"name"`
		Summary string   `json:"summary"`
		Ends    []string `json:"ends"`
		Status  string   `json:"status"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	feat, err := s.store.GetIntelFeature(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "feature not found")
		return
	}
	if strings.TrimSpace(req.Name) != "" {
		feat.Name = strings.TrimSpace(req.Name)
	}
	if req.Summary != "" {
		feat.Summary = req.Summary
	}
	if len(req.Ends) > 0 {
		ends := marshalStrings(req.Ends)
		feat.EndsJSON = ends
	}
	if req.Status != "" {
		feat.Status = req.Status
	}
	if err := s.store.UpdateIntelFeature(ctx, feat); err != nil {
		writeErr(w, http.StatusInternalServerError, "update feature failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feature": feat})
}

// marshalStrings renders a string slice as JSON text (best-effort).
func marshalStrings(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}
