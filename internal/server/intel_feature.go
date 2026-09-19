package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/contract"
	"github.com/hiylo/starburst-backend/internal/intel/feature"
	"github.com/hiylo/starburst-backend/internal/netguard"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelFeatureTest runs a single-test for a feature point: it calls each
// endpoint that belongs to the feature against a user-supplied base URL, checks
// HTTP reachability (status code) and validates the response body against the
// extracted field contract. Results are returned synchronously.
func (s *Server) handleIntelFeatureTest(w http.ResponseWriter, r *http.Request) {
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
		ProjectID int64  `json:"projectId"`
		FeatureID int64  `json:"featureId"`
		BaseURL   string `json:"baseUrl"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || req.FeatureID <= 0 || strings.TrimSpace(req.BaseURL) == "" {
		writeErr(w, http.StatusBadRequest, "projectId, featureId and baseUrl are required")
		return
	}
	baseURL, err := intelCheckBaseURL(r.Context(), req.BaseURL)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	feat, err := s.store.GetIntelFeature(ctx, req.FeatureID)
	if err != nil || feat.ProjectID != req.ProjectID {
		writeErr(w, http.StatusNotFound, "feature not found")
		return
	}
	eps, err := s.store.ListIntelEndpoints(ctx, req.ProjectID, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load endpoints failed")
		return
	}
	matched := feature.MatchEndpoints(eps, feat.Anchor)
	// 手动功能点（无 anchor）或控制器改名导致 anchor 失配时，回退到功能点
	// 的 EndsJSON（用户录入的 METHOD path 列表）匹配端点，避免单测静默返回空。
	if len(matched) == 0 && feat.EndsJSON != "" {
		matched = matchFeatureByEnds(eps, feat.EndsJSON)
	}

	results := make([]map[string]any, 0, len(matched))
	for _, ep := range matched {
		results = append(results, s.testFeatureEndpoint(ctx, baseURL, ep))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"feature":   feat,
		"baseUrl":   baseURL.String(),
		"endpoints": matched,
		"results":   results,
		"count":     len(results),
	})
}

// matchFeatureByEnds falls back to the feature's EndsJSON (a JSON array of
// "METHOD path" strings as recorded for manual features) when anchor matching
// yields nothing. It resolves each entry against the project's persisted
// endpoints so the single-test flow can still probe them.
func matchFeatureByEnds(eps []*store.IntelEndpoint, endsJSON string) []*store.IntelEndpoint {
	var wants []string
	if err := json.Unmarshal([]byte(endsJSON), &wants); err != nil {
		return nil
	}
	out := make([]*store.IntelEndpoint, 0, len(wants))
	for _, ep := range eps {
		if ep == nil {
			continue
		}
		key := ep.Method + " " + ep.Path
		for _, w := range wants {
			w = strings.TrimSpace(w)
			if w == "" {
				continue
			}
			// 精确匹配 METHOD path；也兼容只有 path（无方法）的录入。
			if w == key || w == ep.Path {
				out = append(out, ep)
				break
			}
		}
	}
	return out
}

// intelCheckBaseURL parses a caller-supplied base URL and rejects targets the
// backend must not reach. Scheme alone is not a defence: the host is resolved
// and every address checked, so a name pointing at link-local metadata is
// refused before any request is made.
func intelCheckBaseURL(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("baseUrl must be a valid http(s) URL")
	}
	if err := netguard.CheckHost(ctx, u.Hostname()); err != nil {
		return nil, fmt.Errorf("baseUrl target not allowed: %w", err)
	}
	return u, nil
}

// testFeatureEndpoint issues one HTTP request for an endpoint (filling path and
// query parameters with placeholder values) and returns the reachability and
// contract-check outcome.
func (s *Server) testFeatureEndpoint(ctx context.Context, base *url.URL, ep *store.IntelEndpoint) map[string]any {
	out := map[string]any{
		"method": ep.Method,
		"path":   ep.Path,
	}

	var params []struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Source string `json:"source"`
	}
	var bodyType string
	if ep.RequestJSON != "" {
		var rj struct {
			Params []struct {
				Name   string `json:"name"`
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"params"`
			BodyType string `json:"bodyType"`
		}
		if json.Unmarshal([]byte(ep.RequestJSON), &rj) == nil {
			params = rj.Params
			bodyType = rj.BodyType
		}
	}

	u := *base
	p := ep.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	q := u.Query()
	for _, pm := range params {
		val := placeholderValue(pm.Type, pm.Name)
		if pm.Source == "path" {
			p = strings.ReplaceAll(p, "{"+pm.Name+"}", val)
		} else {
			q.Set(pm.Name, val)
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + p
	u.RawQuery = q.Encode()
	out["url"] = u.String()

	method := ep.Method
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		if bodyType != "" {
			body = strings.NewReader("{}")
		} else {
			body = strings.NewReader("{}")
		}
	}

	// 目标由调用方给出：用受校验的 client（解析后按 IP 固定拨号、不跟随重定向），
	// 否则 baseUrl 就成了打向元数据/链路本地地址的 SSRF 入口。
	client := netguard.Client(15 * time.Second)
	httpReq, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out["status"] = resp.StatusCode
	out["ok"] = resp.StatusCode >= 200 && resp.StatusCode < 400

	if ep.FieldsJSON != "" && len(data) > 0 {
		var specs []contract.FieldSpec
		if json.Unmarshal([]byte(ep.FieldsJSON), &specs) == nil && len(specs) > 0 {
			results, err := contract.CheckResponse(specs, data)
			if err == nil {
				passed, failed := 0, 0
				for _, cr := range results {
					if cr.Status == "passed" {
						passed++
					} else {
						failed++
					}
				}
				out["contract"] = map[string]any{
					"passed":  passed,
					"failed":  failed,
					"results": results,
				}
			}
		}
	}
	return out
}

// placeholderValue returns a deterministic placeholder for a request parameter
// so a feature single-test can build a reachable URL without real data.
func placeholderValue(typeName, name string) string {
	t := strings.ToLower(typeName)
	if strings.Contains(t, "boolean") {
		return "true"
	}
	if strings.Contains(name, "id") {
		return "1"
	}
	if strings.Contains(t, "int") || strings.Contains(t, "long") ||
		strings.Contains(t, "short") || strings.Contains(t, "byte") ||
		strings.Contains(t, "double") || strings.Contains(t, "float") ||
		strings.Contains(t, "decimal") {
		return "1"
	}
	return "test"
}
