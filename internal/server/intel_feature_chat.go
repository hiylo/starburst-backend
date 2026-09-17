package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hiylo/starburst-backend/internal/intel/feature"
	"github.com/hiylo/starburst-backend/internal/store"
)

// buildFeatureChatContext deterministically assembles the context for a
// feature Q&A: the feature record, its endpoint contracts (from the anchor
// match), the latest run's results for those endpoints, and linked issues. No
// LLM is involved; the assembled context is stored verbatim for review.
func (s *Server) buildFeatureChatContext(ctx context.Context, projectID, featureID int64) (string, error) {
	feat, err := s.store.GetIntelFeature(ctx, featureID)
	if err != nil {
		return "", err
	}
	eps, _ := s.store.ListIntelEndpoints(ctx, projectID, 0)
	matched := feature.MatchEndpoints(eps, feat.Anchor)

	var sb strings.Builder
	fmt.Fprintf(&sb, "功能点：%s（anchor=%s，涉及端=%s）\n", feat.Name, feat.Anchor, feat.EndsJSON)
	sb.WriteString("\n关联接口契约：\n")
	endpointSet := map[string]bool{}
	for _, ep := range matched {
		endpointSet[ep.Method+" "+ep.Path] = true
		fmt.Fprintf(&sb, "- %s %s  返回=%s 请求=%s 字段=%s\n",
			ep.Method, ep.Path, ep.ResponseType, truncateJSON(ep.RequestJSON, 300), truncateJSON(ep.FieldsJSON, 400))
	}

	sb.WriteString("\n最近一次运行的实测结果：\n")
	if runs, err := s.store.ListIntelTestRuns(ctx, projectID); err == nil && len(runs) > 0 {
		latest := runs[0]
		fmt.Fprintf(&sb, "运行 #%d（%s，%s）：\n", latest.ID, latest.Status, latest.CreatedAt.Format(time.RFC3339))
		if results, err := s.store.ListIntelTestResults(ctx, latest.ID); err == nil {
			found := false
			for _, res := range results {
				key := endpointSetKey(res)
				if !endpointSet[key] && !containsEndpoint(matched, res.Endpoint) {
					continue
				}
				found = true
				state := "通过"
				if !res.Passed {
					state = "失败 " + truncateJSON(res.FailuresJSON, 300)
				}
				fmt.Fprintf(&sb, "- %s：%s\n", res.Endpoint, state)
			}
			if !found {
				sb.WriteString("- 该功能点接口本次运行无结果\n")
			}
		}
	} else {
		sb.WriteString("- 尚无运行记录\n")
	}

	sb.WriteString("\n挂载问题：\n")
	if issues, err := s.store.ListIntelIssues(ctx, projectID, ""); err == nil {
		n := 0
		for _, iss := range issues {
			if iss.FeatureID == featureID {
				n++
				fmt.Fprintf(&sb, "- %s %s %s（%s）%s\n", iss.Kind, iss.Severity, iss.Key, iss.Status, truncateJSON(iss.DetailJSON, 200))
			}
		}
		if n == 0 {
			sb.WriteString("- 无挂载问题\n")
		}
	}
	return sb.String(), nil
}

// endpointSetKey maps a TestResult to the endpoint key the scanner records.
func endpointSetKey(res *store.TestResult) string {
	if res == nil {
		return ""
	}
	// TestResult.Endpoint is "Class.method" or "path"; try to match loosely by
	// suffix against the feature endpoints instead of strict equality.
	return " " + res.Endpoint
}

// containsEndpoint reports whether any matched endpoint path is a suffix of
// (or equal to) the test result endpoint.
func containsEndpoint(matched []*store.IntelEndpoint, endpoint string) bool {
	for _, ep := range matched {
		p := ep.Path
		if endpoint == p || strings.HasPrefix(endpoint, p) {
			return true
		}
	}
	return false
}

// handleIntelFeaturesByIDOrChat dispatches feature-level actions:
// .../{id}/chats (history), .../{id}/chat (ask), .../{id} (rename/update).
func (s *Server) handleIntelFeaturesByIDOrChat(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/features/")
	rest = strings.TrimSuffix(rest, "/")
	switch {
	case strings.HasSuffix(rest, "/chats"):
		s.handleIntelFeatureChats(w, r)
	case strings.HasSuffix(rest, "/chat"):
		s.handleIntelFeatureChat(w, r)
	default:
		s.handleIntelFeatureByID(w, r)
	}
}

// handleIntelFeatureChats lists a feature's AI Q&A history.
func (s *Server) handleIntelFeatureChats(w http.ResponseWriter, r *http.Request) {
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
	id, ok := s.intelIDFromPath(r, "/api/intel/features/")
	if !ok {
		return
	}
	projectID, ok2 := s.intelQueryProject(w, r)
	if !ok2 {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	chats, err := s.store.ListIntelFeatureChats(ctx, projectID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load chats failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

// handleIntelFeatureChat answers a question about a feature: it assembles the
// deterministic context (contracts + test results + issues), stores the Q&A,
// then asks the orchestration LLM with that context if configured.
func (s *Server) handleIntelFeatureChat(w http.ResponseWriter, r *http.Request) {
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
	rest := strings.TrimPrefix(r.URL.Path, "/api/intel/features/")
	rest = strings.TrimSuffix(rest, "/")
	rest = strings.TrimSuffix(rest, "/chat")
	rest = strings.TrimSuffix(rest, "/")
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid feature id")
		return
	}
	var req struct {
		ProjectID int64  `json:"projectId"`
		Question  string `json:"question"`
	}
	if err := readJSONLimited(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.ProjectID <= 0 || strings.TrimSpace(req.Question) == "" {
		writeErr(w, http.StatusBadRequest, "projectId and question are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	feat, err := s.store.GetIntelFeature(ctx, id)
	if err != nil || feat.ProjectID != req.ProjectID {
		writeErr(w, http.StatusNotFound, "feature not found")
		return
	}
	contextJSON, err := s.buildFeatureChatContext(ctx, req.ProjectID, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "组装上下文失败")
		return
	}

	answer := ""
	label := ""
	if s.llm != nil && s.llm.Enabled() {
		system := "你是测试智能归因助手。基于给定上下文（功能点接口契约、最近运行实测结果、挂载问题）回答用户关于该功能点的问题，并给出结论依据。上下文全部来源于仓库自动分析。"
		answer = cleanLLMText(s.llmComplete(ctx, system, contextJSON+"\n\n问题："+req.Question), llmTextMax)
		label = "ai"
	} else {
		answer = "（未配置编排 LLM，已保存上下文；配置后可自动回答）"
		label = "no-llm"
	}

	rec := &store.IntelFeatureChat{
		FeatureID:   id,
		ProjectID:   req.ProjectID,
		Question:    req.Question,
		ContextJSON: contextJSON,
		Answer:      answer,
	}
	if err := s.store.CreateIntelFeatureChat(ctx, rec); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存对话失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"chat": rec, "mode": label})
}

// llmComplete calls the orchestration LLM with a system+user prompt and returns
// its plain-text response.
func (s *Server) llmComplete(ctx context.Context, system, user string) string {
	out, err := s.llm.Complete(ctx, system, user)
	if err != nil {
		return "（LLM 调用失败：" + err.Error() + "）"
	}
	return strings.TrimSpace(out)
}

// truncateJSON caps a JSON/string field for context assembly (deterministic).
func truncateJSON(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	if s == "" {
		return "-"
	}
	return s
}
