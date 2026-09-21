package server

// Issue actions: the closed-loop endpoints for a single intel issue —
// acknowledge (mark resolved) and link a feature point.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelIssueAction routes POST actions on a single issue:
// /api/intel/issues/{id}/ack and /api/intel/issues/{id}/link-feature.
func (s *Server) handleIntelIssueAction(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/issues/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		writeErr(w, http.StatusBadRequest, "expected /api/intel/issues/{id}/{action}")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid issue id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	switch parts[1] {
	case "ack":
		if err := s.store.MarkIntelIssueResolved(ctx, id, "resolved"); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "issue not found")
				return
			}
			log.Printf("intel issue %d ack: %v", id, err)
			writeErr(w, http.StatusInternalServerError, "ack failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": "resolved"})
	case "link-feature":
		var req struct {
			FeatureID int64 `json:"featureId"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if req.FeatureID <= 0 {
			writeErr(w, http.StatusBadRequest, "featureId is required")
			return
		}
		feat, err := s.store.GetIntelFeature(ctx, req.FeatureID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "feature not found")
			return
		}
		if err := s.store.LinkIntelIssueFeature(ctx, id, feat.ID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "issue not found")
				return
			}
			log.Printf("intel issue %d link-feature %d: %v", id, feat.ID, err)
			writeErr(w, http.StatusInternalServerError, "link-feature failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "featureId": feat.ID})
	default:
		writeErr(w, http.StatusBadRequest, "action must be ack|link-feature")
	}
}
