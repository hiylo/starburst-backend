package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/contract"
)

// handleIntelContractCheck validates a pasted API response against an
// endpoint's extracted field contract (FieldsJSON) and returns per-field
// results, so a human can confirm the real response shape matches the code
// contract without running a full API test harness.
func (s *Server) handleIntelContractCheck(w http.ResponseWriter, r *http.Request) {
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
		EndpointID   int64  `json:"endpointId"`
		ResponseJSON string `json:"responseJson"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.EndpointID <= 0 || req.ResponseJSON == "" {
		writeErr(w, http.StatusBadRequest, "endpointId and responseJson are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ep, err := s.store.GetIntelEndpoint(ctx, req.EndpointID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "endpoint not found")
		return
	}
	var specs []contract.FieldSpec
	if ep.FieldsJSON != "" {
		if err := json.Unmarshal([]byte(ep.FieldsJSON), &specs); err != nil {
			writeErr(w, http.StatusInternalServerError, "endpoint has invalid field contract")
			return
		}
	}
	if len(specs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"results": []contract.CheckResult{}, "passed": 0, "failed": 0, "note": "该接口暂无响应字段契约"})
		return
	}

	results, err := contract.CheckResponse(specs, []byte(req.ResponseJSON))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "responseJson 不是合法 JSON: "+err.Error())
		return
	}
	passed, failed := 0, 0
	for _, res := range results {
		if res.Status == "passed" {
			passed++
		} else {
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "passed": passed, "failed": failed})
}
