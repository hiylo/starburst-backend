package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/hiylo/starburst-backend/internal/alerts"
)

// handleAlerts reads (GET) and updates (POST) the hardware threshold alert
// settings. Requires a web session (admin) or an APP token.
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	switch r.Method {
	case http.MethodGet:
		if s.alerts == nil {
			writeJSON(w, http.StatusOK, alerts.Snapshot{})
			return
		}
		writeJSON(w, http.StatusOK, s.alerts.Snapshot(r.Context()))
	case http.MethodPost:
		var req struct {
			Enabled *bool    `json:"enabled"`
			CPUPct  *float64 `json:"cpuPct"`
			MemPct  *float64 `json:"memPct"`
			DiskPct *float64 `json:"diskPct"`
		}
		if !readBody(w, r, &req) {
			return
		}
		if err := s.setAlertBool(r, alerts.SettingEnabled, req.Enabled); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		for _, p := range []struct {
			key string
			val *float64
		}{
			{alerts.SettingCPU, req.CPUPct},
			{alerts.SettingMem, req.MemPct},
			{alerts.SettingDisk, req.DiskPct},
		} {
			if err := s.setAlertPct(r, p.key, p.val); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if s.alerts == nil {
			writeJSON(w, http.StatusOK, alerts.Snapshot{})
			return
		}
		writeJSON(w, http.StatusOK, s.alerts.Snapshot(r.Context()))
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// setAlertBool persists a boolean alert setting when val is non-nil.
func (s *Server) setAlertBool(r *http.Request, key string, val *bool) error {
	if val == nil {
		return nil
	}
	v := "false"
	if *val {
		v = "true"
	}
	return s.store.SetSetting(r.Context(), key, v)
}

// setAlertPct persists a percentage alert setting, validating the range.
func (s *Server) setAlertPct(r *http.Request, key string, val *float64) error {
	if val == nil {
		return nil
	}
	if *val < 0 || *val > 100 {
		return fmt.Errorf("threshold %s must be within 0-100", key)
	}
	return s.store.SetSetting(r.Context(), key, strconv.FormatFloat(*val, 'f', -1, 64))
}
