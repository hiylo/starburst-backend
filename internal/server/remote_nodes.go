package server

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/netguard"
	"github.com/hiylo/starburst-backend/internal/store"
)

// checkNodeReachable probes host:port (TCP) to determine node reachability.
// The host comes from the request or from a stored node, so the dial is guarded:
// a device token must not turn this into a port scanner for metadata addresses.
func checkNodeReachable(ctx context.Context, host string, port int) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := netguard.Dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// pickRemoteNode returns the first node, in node-list order, that is stored
// reachable and whose capability labels satisfy the required label. A nil
// result means no such node exists and is not an error — the caller falls back
// to local execution. Only nodes that declare matching capability labels are
// eligible: a legacy node with no labels cannot be verified to own the
// toolchain, so auto-routing never picks it (explicit nodeID selection keeps
// accepting legacy nodes). Selection is first-match on the stored Reachable
// field, consistent with the explicit-node path; load-balancing across several
// matching nodes is a future refinement.
func (s *Server) pickRemoteNode(ctx context.Context, need string) (*store.RemoteNode, error) {
	if need == "" {
		return nil, nil
	}
	nodes, err := s.store.ListRemoteNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n == nil || !n.Reachable {
			continue
		}
		if nodeHasCapability(n.Capabilities, need) {
			return n, nil
		}
	}
	return nil, nil
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
			WorkDir      string `json:"workDir"`
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
			WorkDir:      strings.TrimSpace(req.WorkDir),
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
