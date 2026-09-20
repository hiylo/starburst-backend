package server

import (
	"context"
	"net/http"
	"strconv"
)

// intelWorkersSetting is the settings key for the test-execution concurrency cap.
const intelWorkersSetting = "intel.workers"

// intelWorkersMax bounds the adjustable concurrency so a misconfiguration cannot
// saturate the host with a flood of parallel test processes.
const intelWorkersMax = 16

// TriggerIntelRunAll is the callback entry point used by the automation engine
// when an intel-run automation rule fires: it enqueues a full regression
// (scope=all) run for the project after passing the environment gate.
func (s *Server) TriggerIntelRunAll(ctx context.Context, projectID int64) error {
	_, err := s.enqueueIntelRunAll(ctx, projectID, false)
	return err
}

// SetIntelWorkers applies a runtime test-execution concurrency cap (clamped to
// >= 1). Used at startup to restore the persisted intel.workers setting.
func (s *Server) SetIntelWorkers(n int) {
	s.intelSem.setCap(n)
}

// handleIntelSettings reads (GET) and updates (POST) the test-intelligence
// global settings. Currently only the test-execution concurrency (intel.workers)
// is adjustable; other settings remain code-fixed. Requires a web session
// (admin) or an APP token.
func (s *Server) handleIntelSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"workers": s.intelSem.capValue()})
	case http.MethodPost:
		var req struct {
			Workers *int `json:"workers"`
		}
		if !readBody(w, r, &req) {
			return
		}
		if req.Workers == nil {
			writeErr(w, http.StatusBadRequest, "workers is required")
			return
		}
		if *req.Workers < 1 || *req.Workers > intelWorkersMax {
			writeErr(w, http.StatusBadRequest, "workers must be within 1-"+strconv.Itoa(intelWorkersMax))
			return
		}
		if err := s.store.SetSetting(r.Context(), intelWorkersSetting, strconv.Itoa(*req.Workers)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.intelSem.setCap(*req.Workers)
		writeJSON(w, http.StatusOK, map[string]any{"workers": *req.Workers})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
