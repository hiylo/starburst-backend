package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/store"
)

// checkNodeReachable probes host:port (TCP) to determine node reachability.
func checkNodeReachable(ctx context.Context, host string, port int) bool {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// handleIntelNodes lists (GET) or creates (POST) remote execution nodes.
func (s *Server) handleIntelNodes(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	switch r.Method {
	case http.MethodGet:
		nodes, err := s.store.ListRemoteNodes(ctx)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load nodes failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
	case http.MethodPost:
		var req struct {
			Name         string `json:"name"`
			Host         string `json:"host"`
			Port         int    `json:"port"`
			User         string `json:"user"`
			Auth         string `json:"auth"`
			Capabilities string `json:"capabilities"`
			Note         string `json:"note"`
		}
		if err := readJSONLimited(w, r, &req); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Host) == "" {
			writeErr(w, http.StatusBadRequest, "name and host are required")
			return
		}
		if req.Port <= 0 {
			req.Port = 22
		}
		now := time.Now()
		node := &store.RemoteNode{
			Name:         strings.TrimSpace(req.Name),
			Host:         strings.TrimSpace(req.Host),
			Port:         req.Port,
			User:         req.User,
			Auth:         req.Auth,
			Capabilities: strings.TrimSpace(req.Capabilities),
			Note:         req.Note,
			Reachable:    checkNodeReachable(ctx, req.Host, req.Port),
			LastCheckAt:  &now,
		}
		if node.Reachable {
			node.LastCheckAt = &now
		}
		if err := s.store.CreateRemoteNode(ctx, node); err != nil {
			writeErr(w, http.StatusInternalServerError, "create node failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"node": node})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelNodeByID checks (PUT .../{id}/check) or deletes (DELETE .../{id})
// a remote node.
func (s *Server) handleIntelNodeByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireWeb(r) {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/nodes/")
	rest = strings.TrimSuffix(rest, "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid node id")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid node id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if action == "check" && r.Method == http.MethodPut {
		node, err := s.store.GetRemoteNode(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, "node not found")
			return
		}
		now := time.Now()
		node.Reachable = checkNodeReachable(ctx, node.Host, node.Port)
		node.LastCheckAt = &now
		if err := s.store.UpdateRemoteNode(ctx, node); err != nil {
			writeErr(w, http.StatusInternalServerError, "update node failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"node": node})
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteRemoteNode(ctx, id); err != nil {
			writeErr(w, http.StatusInternalServerError, "delete node failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
}
