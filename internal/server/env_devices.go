package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/envagent"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelDevices lists devices, refreshing from adb when available (GET).
// Wireless connections are attached via adb connect before an adb-device is
// upserted, so a later run finds them without re-probing.
func (s *Server) handleIntelDevices(w http.ResponseWriter, r *http.Request) {
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
	projectID := int64(0)
	if v := r.URL.Query().Get("projectId"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			projectID = id
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	devs, err := s.store.ListIntelDevices(ctx, projectID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load devices failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devs})
}

// handleIntelDeviceConnect attaches a wireless device over adb and upserts it.
func (s *Server) handleIntelDeviceConnect(w http.ResponseWriter, r *http.Request) {
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
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Port <= 0 {
		req.Port = 5555
	}
	if strings.TrimSpace(req.IP) == "" {
		writeErr(w, http.StatusBadRequest, "ip is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := envagent.ADBConnect(ctx, req.IP, req.Port); err != nil {
		writeErr(w, http.StatusInternalServerError, "adb connect 失败（需手机端配合授权）: "+err.Error())
		return
	}
	devs, err := envagent.ADBDeviceList(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "adb 设备枚举失败")
		return
	}
	serial := netJoinHostPort(req.IP, req.Port)
	now := time.Now()
	var found *store.IntelDevice
	for _, d := range devs {
		if d.Serial == serial {
			found = &store.IntelDevice{
				ProjectID:  0,
				Name:       d.Name,
				Method:     "wireless",
				ADBHost:    req.IP,
				ADBPort:    req.Port,
				Serial:     d.Serial,
				Bound:      false,
				Status:     d.State,
				LastSeenAt: &now,
			}
			break
		}
	}
	if found == nil {
		found = &store.IntelDevice{
			ProjectID:  0,
			Name:       serial,
			Method:     "wireless",
			ADBHost:    req.IP,
			ADBPort:    req.Port,
			Serial:     serial,
			Bound:      false,
			Status:     "connected",
			LastSeenAt: &now,
		}
	}
	if err := s.store.UpsertIntelDevices(ctx, []*store.IntelDevice{found}); err != nil {
		writeErr(w, http.StatusInternalServerError, "persist device failed")
		return
	}
	// Re-fetch to obtain the assigned id (upsert does not return it).
	created := found
	if all, err := s.store.ListIntelDevices(ctx, 0); err == nil {
		for _, d := range all {
			if d.Serial == found.Serial {
				created = d
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": created})
}

// handleIntelDevicesByID dispatches per-device actions: PUT .../{id}/bind binds
// the device to a project; DELETE .../{id} removes it.
func (s *Server) handleIntelDevicesByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/env/devices/")
	rest = strings.TrimSuffix(rest, "/")
	switch {
	case strings.HasSuffix(rest, "/bind"):
		s.handleIntelDeviceBind(w, r)
	default:
		s.handleIntelDeviceDelete(w, r)
	}
}

// handleIntelDeviceBind binds a device to a project (人工绑定，手动才变).
func (s *Server) handleIntelDeviceBind(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/env/devices/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, "/bind")
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid device id")
		return
	}
	var req struct {
		ProjectID int64 `json:"projectId"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	device, err := s.store.GetIntelDevice(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "device not found")
		return
	}
	device.ProjectID = req.ProjectID
	device.Bound = true
	if err := s.store.UpdateIntelDevice(ctx, device); err != nil {
		writeErr(w, http.StatusInternalServerError, "bind device failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device": device})
}

// handleIntelDeviceDelete removes a device (unbind/remove).
func (s *Server) handleIntelDeviceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method != http.MethodDelete {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/env/devices/")
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid device id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.store.DeleteIntelDevice(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete device failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// netJoinHostPort is a tiny local wrapper to avoid importing net for one call.
func netJoinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		return "[" + host + "]:" + strconv.Itoa(port)
	}
	return host + ":" + strconv.Itoa(port)
}
