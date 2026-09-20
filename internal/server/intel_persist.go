package server

// Persistence of assets derived during analysis: test cases, module
// summaries, features, gateway routes, per-platform bindings, LLM
// enrichment, impact records, the overview snapshot and requirement/dep scans.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

import (
	"github.com/hiylo/starburst-backend/internal/intel/android"
	"github.com/hiylo/starburst-backend/internal/intel/delta"
	"github.com/hiylo/starburst-backend/internal/intel/deps"
	"github.com/hiylo/starburst-backend/internal/intel/enrich"
	"github.com/hiylo/starburst-backend/internal/intel/envdetect"
	"github.com/hiylo/starburst-backend/internal/intel/feature"
	"github.com/hiylo/starburst-backend/internal/intel/gateway"
	"github.com/hiylo/starburst-backend/internal/intel/ios"
	"github.com/hiylo/starburst-backend/internal/intel/sbom"
	"github.com/hiylo/starburst-backend/internal/intel/testassets"
	"github.com/hiylo/starburst-backend/internal/intel/web"
	"github.com/hiylo/starburst-backend/internal/store"
)

// buildTestCases converts a module's discovered test assets into store models
// with the module id stamped (for later per-module filtering and execution
// status attribution).
func buildTestCases(m *store.IntelModule, assets []testassets.Asset) []*store.TestCase {
	cases := make([]*store.TestCase, 0, len(assets))
	for _, a := range assets {
		tags := "[]"
		if b, err := json.Marshal(a.Tags); err == nil {
			tags = string(b)
		}
		cases = append(cases, &store.TestCase{
			ModuleID:  m.ID,
			Module:    m.RelPath,
			Kind:      a.Kind,
			Framework: a.Framework,
			Class:     a.Class,
			Method:    a.Method,
			Path:      a.Path,
			Tags:      tags,
		})
	}
	return cases
}

// ensureModuleSummary lazily generates (via the orchestration LLM) a concise
// business summary for a module from its endpoint contracts and entities, and
// caches it in project_modules.summary so repeated views hit the DB. When the
// LLM is not configured the module detail stays purely deterministic.
func (s *Server) ensureModuleSummary(ctx context.Context, mod *store.IntelModule, endpoints []*store.IntelEndpoint, entities []*store.IntelEntity) string {
	if s.llm == nil || !s.llm.Enabled() {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "子模块：%s（类型 %s / 角色 %s / 构建 %s）\n", mod.RelPath, mod.KindType, mod.KindRole, mod.BuildTool)
	if len(endpoints) > 0 {
		sb.WriteString("\n接口契约：\n")
		for i, ep := range endpoints {
			if i >= 30 {
				break
			}
			fmt.Fprintf(&sb, "- %s %s 返回=%s%s\n", ep.Method, ep.Path, ep.ResponseType,
				summarySuffix(ep.Summary))
		}
	}
	if len(entities) > 0 {
		sb.WriteString("\n实体 / 表：\n")
		seen := map[string]bool{}
		for _, e := range entities {
			if seen[e.TableName] {
				continue
			}
			seen[e.TableName] = true
			fmt.Fprintf(&sb, "- %s\n", e.TableName)
		}
	}
	if routes, err := s.store.ListIntelGatewayRoutes(ctx, mod.ProjectID); err == nil && len(routes) > 0 {
		sb.WriteString("\n网关路由上下文：\n")
		for i, rt := range routes {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&sb, "- %s -> %s %s\n", rt.Service, rt.URI, rt.PathsJSON)
		}
		sb.WriteString("（未列入网关直通路由的路径由网关兜底按服务名转发，认证类路径一般由网关放行/认证中心处理）\n")
	}
	var out struct {
		Summary string `json:"summary"`
	}
	system := loadPrompt("module_analyze", "你是子模块分析助手，基于给定接口与实体清单总结模块职责，输出 JSON {summary}。")
	if err := s.llm.CompleteJSON(ctx, system, sb.String(), &out); err != nil {
		return ""
	}
	summary := cleanLLMText(out.Summary, 800)
	if summary != "" {
		_ = s.store.UpdateIntelModuleSummary(ctx, mod.ID, summary)
		mod.Summary = summary
	}
	return summary
}

// summarySuffix appends a non-empty LLM summary hint for an endpoint.
func summarySuffix(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return "（" + s + "）"
}

// persistFeatures clusters the extracted endpoints into candidate feature
// points and stores them. Human-created (manual) features are preserved across
// rescans: only auto-derived rows are replaced (人工优先，重扫不覆盖).
func (s *Server) persistFeatures(ctx context.Context, projectID int64, endpoints []*store.IntelEndpoint) error {
	var manual []*store.IntelFeature
	if prev, err := s.store.ListIntelFeatures(ctx, projectID); err == nil {
		for _, f := range prev {
			if f.Source == "manual" {
				manual = append(manual, f)
			}
		}
	}
	feats := feature.Cluster(endpoints)
	if len(feats) == 0 && len(manual) == 0 {
		return nil
	}
	storeFeats := make([]*store.IntelFeature, 0, len(feats)+len(manual))
	for i, f := range feats {
		ends, _ := json.Marshal(f.Ends)
		storeFeats = append(storeFeats, &store.IntelFeature{
			Name:      f.Name,
			EndsJSON:  string(ends),
			SortOrder: i,
			Source:    "auto",
			Anchor:    f.Anchor,
			Status:    "active",
		})
	}
	storeFeats = append(storeFeats, manual...)
	return s.store.ReplaceIntelFeatures(ctx, projectID, storeFeats)
}

// intelModuleScan couples one analyzed module with its working directory and
// owning repo root, so per-end (多端多仓库) modules resolve bindings/sources
// against their own repository rather than the project's primary root.
type intelModuleScan struct {
	m    *store.IntelModule
	dir  string // module working directory (repo root + rel path)
	root string // owning repo root
	rel  string // module rel path within root (no "@end/" prefix)
}

// persistGatewayRoutes discovers gateway route configuration under every scan
// root and replaces the project's route list once (multi-repo aware).
func (s *Server) persistGatewayRoutes(ctx context.Context, projectID int64, roots []string) error {
	routes := make([]*store.IntelGatewayRoute, 0)
	for _, root := range roots {
		found, err := gateway.Discover(root)
		if err != nil {
			continue
		}
		for _, r := range found {
			paths, _ := json.Marshal(r.Paths)
			routes = append(routes, &store.IntelGatewayRoute{
				Service:    r.Service,
				PathsJSON:  string(paths),
				URI:        r.URI,
				Source:     r.Source,
				SourceLine: r.SourceLine,
			})
		}
	}
	return s.store.ReplaceIntelGatewayRoutes(ctx, projectID, routes)
}

// persistAndroidBindings extracts Android DataBinding "page -> field path"
// bindings for every android module and persists them as the client's
// must-display field list (used later for CLIENT_MISSING_FIELD attribution).
func (s *Server) persistAndroidBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelAndroidBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "android" {
			continue
		}
		resDir := filepath.Join(sc.dir, "src", "main", "res")
		if fi, err := os.Stat(resDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := android.ExtractBindings(resDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, resDir, b.Source)
			bindings = append(bindings, &store.IntelAndroidBinding{
				ModuleID:   sc.m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Widget:     b.Widget,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelAndroidBindings(ctx, projectID, bindings)
}

// persistWebBindings extracts Vue template "page -> field path" bindings for
// every web module and persists them as the client's must-display field list.
func (s *Server) persistWebBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelWebBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "web" {
			continue
		}
		srcDir := filepath.Join(sc.dir, "src")
		if fi, err := os.Stat(srcDir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := web.ExtractBindings(srcDir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, srcDir, b.Source)
			bindings = append(bindings, &store.IntelWebBinding{
				ModuleID:   sc.m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Slot:       b.Slot,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelWebBindings(ctx, projectID, bindings)
}

// persistIosBindings extracts SwiftUI view "page -> field path" bindings for
// every iOS module and persists them as the client's must-display field list.
func (s *Server) persistIosBindings(ctx context.Context, projectID int64, scans []intelModuleScan) error {
	bindings := make([]*store.IntelIosBinding, 0)
	for _, sc := range scans {
		if sc.m.KindType != "ios" {
			continue
		}
		if fi, err := os.Stat(sc.dir); err != nil || !fi.IsDir() {
			continue
		}
		bs, err := ios.ExtractBindings(sc.dir)
		if err != nil {
			continue
		}
		for _, b := range bs {
			src, line := splitAndroidSource(sc.root, sc.dir, b.Source)
			bindings = append(bindings, &store.IntelIosBinding{
				ModuleID:   sc.m.ID,
				Page:       b.Page,
				FieldPath:  b.FieldPath,
				Slot:       b.Slot,
				SourceFile: src,
				SourceLine: line,
			})
		}
	}
	return s.store.ReplaceIntelIosBindings(ctx, projectID, bindings)
}

// splitAndroidSource splits a binding's "rel/path.xml:line" source (relative to
// resDir) into a project-root-relative file path and a line number.
func splitAndroidSource(root, resDir, src string) (string, int) {
	file := src
	line := 0
	if i := strings.LastIndexByte(src, ':'); i >= 0 {
		if n, err := strconv.Atoi(src[i+1:]); err == nil {
			line = n
			file = src[:i]
		}
	}
	full := filepath.Join(resDir, filepath.FromSlash(file))
	rel, err := filepath.Rel(root, full)
	if err != nil {
		rel = full
	}
	return filepath.ToSlash(rel), line
}

// enrichIntelWithLLM runs the optional LLM document-analysis pass: it feeds the
// project's Markdown docs (across every scan root) plus the extracted endpoint
// contracts to the orchestration LLM and stores per-endpoint business summaries
// and (clearly marked) suggested gateway routes. It is a no-op unless the LLM
// is configured.
func (s *Server) enrichIntelWithLLM(ctx context.Context, projectID int64, roots []string) {
	if s.llm == nil || !s.llm.Enabled() {
		return
	}
	eps, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil || len(eps) == 0 {
		return
	}
	var sb strings.Builder
	for _, root := range roots {
		if sb.Len() >= 45000 {
			break
		}
		doc := enrich.Docs(root, 45000-sb.Len())
		if doc == "" {
			continue
		}
		sb.WriteString(doc)
		sb.WriteString("\n\n")
	}
	docs := strings.TrimSpace(sb.String())
	if docs == "" {
		return
	}
	hints := make([]enrich.EndpointHint, 0, len(eps))
	for _, ep := range eps {
		hints = append(hints, enrich.EndpointHint{Method: ep.Method, Path: ep.Path, ResponseType: ep.ResponseType})
	}
	var result enrich.Result
	if err := s.llm.CompleteJSON(ctx, enrich.SystemPrompt(), enrich.UserPrompt(docs, hints), &result); err != nil {
		log.Printf("intel llm enrich project %d: %v", projectID, err)
		return
	}
	for _, es := range result.Endpoints {
		summary := strings.TrimSpace(es.Summary)
		if summary == "" {
			continue
		}
		if err := s.store.UpdateIntelEndpointSummary(ctx, projectID, es.Method, es.Path, summary); err != nil {
			log.Printf("intel llm summary %s %s: %v", es.Method, es.Path, err)
		}
	}
	s.storeLLMSuggestedRoutes(ctx, projectID, result.Routes)
}

// storeLLMSuggestedRoutes inserts LLM-suggested gateway routes that are not
// already covered by config-derived routes, marking their source as "llm" so
// the deterministic config routes remain authoritative.
func (s *Server) storeLLMSuggestedRoutes(ctx context.Context, projectID int64, hints []enrich.RouteHint) {
	if len(hints) == 0 {
		return
	}
	existing, err := s.store.ListIntelGatewayRoutes(ctx, projectID)
	if err != nil {
		return
	}
	covered := make(map[string]bool)
	for _, r := range existing {
		var paths []string
		if json.Unmarshal([]byte(r.PathsJSON), &paths) != nil {
			continue
		}
		for _, p := range paths {
			covered[p] = true
		}
	}
	var add []*store.IntelGatewayRoute
	for _, h := range hints {
		var fresh []string
		for _, p := range h.Paths {
			if !covered[p] {
				fresh = append(fresh, p)
				covered[p] = true
			}
		}
		if len(fresh) == 0 {
			continue
		}
		pathsJSON, _ := json.Marshal(fresh)
		add = append(add, &store.IntelGatewayRoute{Service: h.Service, PathsJSON: string(pathsJSON), Source: "llm"})
	}
	if len(add) > 0 {
		if err := s.store.AddIntelGatewayRoutes(ctx, projectID, add); err != nil {
			log.Printf("intel llm routes project %d: %v", projectID, err)
		}
	}
}

// recordImpact computes and persists the incremental impact between the
// project's previous snapshot and the current HEAD. It only applies to
// git-backed projects; a non-ancestor base (force-push/reset) or a missing
// previous snapshot records a full rescan instead of a diff.
func (s *Server) recordImpact(ctx context.Context, projectID int64, p *store.IntelProject, root, head string) {
	if head == "" || p.SnapshotSHA == "" || p.SnapshotSHA == head {
		return
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return
	}
	imp := delta.Impact{}
	if !delta.IsAncestor(root, p.SnapshotSHA, head) {
		imp.FullRescan = true
	} else if files, err := delta.DiffFiles(root, p.SnapshotSHA); err == nil {
		imp = delta.ComputeImpact(files)
	} else {
		return
	}
	b, err := json.Marshal(imp)
	if err != nil {
		return
	}
	if err := s.store.ReplaceIntelImpact(ctx, projectID, &store.IntelImpact{
		ProjectID:  projectID,
		BaseSHA:    p.SnapshotSHA,
		HeadSHA:    head,
		ImpactJSON: string(b),
	}); err != nil {
		log.Printf("intel impact project %d: %v", projectID, err)
	}
}

// persistOverview aggregates the project's dependency list, environment
// requirements and CycloneDX SBOM across every scan root, then stores them as a
// single overview snapshot for the detail view.
func (s *Server) persistOverview(ctx context.Context, projectID int64, p *store.IntelProject, roots []string) {
	all := collectDependencies(roots)
	reqs := detectRequirements(roots)
	depsJSON, _ := json.Marshal(all)
	envJSON, _ := json.Marshal(reqs)
	sbomJSON, err := sbom.Generate(p.Name, all)
	if err != nil {
		sbomJSON = nil
	}
	if err := s.store.ReplaceIntelOverview(ctx, projectID, &store.IntelOverview{
		ProjectID: projectID,
		DepsJSON:  string(depsJSON),
		EnvJSON:   string(envJSON),
		SbomJSON:  string(sbomJSON),
	}); err != nil {
		log.Printf("intel overview project %d: %v", projectID, err)
	}
}

// detectRequirements runs env detection across every scan root, overlaying any
// manual intel-env.yaml manifest, and merges the per-repo requirement lists
// (deduplicated by service+category).
func detectRequirements(roots []string) []envdetect.Requirement {
	seen := make(map[string]bool)
	out := make([]envdetect.Requirement, 0)
	for _, root := range roots {
		found, err := envdetect.Detect(root, "")
		if err != nil {
			found = nil
		}
		// 人工 manifest（intel-env.yaml）覆盖/补充自动探测结果。
		if path, ok := envdetect.ManifestAt(root); ok {
			if data, err := os.ReadFile(path); err == nil {
				if mreqs, _, err := envdetect.ParseManifest(data, "intel-env.yaml"); err == nil {
					found = envdetect.MergeWithDetect(found, mreqs)
				}
			}
		}
		for _, r := range found {
			key := r.Service
			if key == "" {
				key = r.Category
			}
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

// collectDependencies walks every repository root for dependency manifests and
// returns a deduplicated dependency list across ecosystems.
func collectDependencies(roots []string) []deps.Dependency {
	seen := make(map[string]bool)
	out := make([]deps.Dependency, 0)
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" || name == "target" ||
					name == ".gradle" || name == "build_artifacts" || name == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			var parsed []deps.Dependency
			switch d.Name() {
			case "pom.xml":
				if data, err := os.ReadFile(path); err == nil {
					parsed, _ = deps.ParsePom(data)
				}
			case "go.mod":
				if data, err := os.ReadFile(path); err == nil {
					parsed, _ = deps.ParseGoMod(data)
				}
			case "package.json":
				if data, err := os.ReadFile(path); err == nil {
					parsed, _ = deps.ParsePackageJSON(data)
				}
			case "build.gradle", "build.gradle.kts":
				if data, err := os.ReadFile(path); err == nil {
					parsed, _ = deps.ParseGradle(data)
				}
			}
			for _, dep := range parsed {
				key := dep.Ecosystem + "|" + dep.Group + "|" + dep.Name + "|" + dep.Version
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, dep)
			}
			return nil
		})
	}
	return out
}
