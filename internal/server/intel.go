package server

// Request-path helpers shared by every /api/intel handler: they resolve the
// project id from the URL path or the projectId query parameter.

import (
	"net/http"
	"strconv"
	"strings"
)

// intelPathID parses a trailing integer id from a URL path prefix.
func intelPathID(w http.ResponseWriter, r *http.Request, prefix string) (int64, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid project id")
		return 0, false
	}
	return id, true
}

// intelIDFromPath parses a project id from a path that may continue with a
// sub-resource (e.g. /api/intel/projects/3/modules).
func (s *Server) intelIDFromPath(r *http.Request, prefix string) (int64, bool) {
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	rest = strings.TrimSuffix(rest, "/")
	idStr := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		idStr = rest[:i]
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// intelQueryProject reads the projectId query parameter, writing a 400 when
// missing/invalid.
func (s *Server) intelQueryProject(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("projectId"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "projectId query parameter is required")
		return 0, false
	}
	return id, true
}

func intQuery(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}
