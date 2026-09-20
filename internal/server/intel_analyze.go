package server

// Analysis engine: module detection, the auto-analyze interval guard, and
// the full / incremental analyze passes that refresh a project's inventory.

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

import (
	"github.com/hiylo/starburst-backend/internal/intel"
	"github.com/hiylo/starburst-backend/internal/intel/testassets"
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelAnalyze triggers a full analyze: module detection + contract
// scanning, persisted into the intel tables. Synchronous for M1 (async via
// tasks lands with the execution milestone).
func (s *Server) handleIntelAnalyze(w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	// 与后台自动分析（创建/改源码触发）共用同一把项目级锁，避免全量替换并发竞态。
	mu := s.intelAnalyzeMutex(req.ProjectID)
	mu.Lock()
	defer mu.Unlock()
	if err := s.runIntelAnalyze(ctx, req.ProjectID); err != nil {
		log.Printf("intel analyze project %d: %v", req.ProjectID, err)
		if merr := s.store.MarkIntelAnalyzeFailed(ctx, req.ProjectID); merr != nil {
			log.Printf("intel analyze fail marker project %d: %v", req.ProjectID, merr)
		}
		writeErr(w, http.StatusInternalServerError, "analyze failed: "+err.Error())
		return
	}
	// 分析成功后后台重建知识库索引，让契约问答立即反映最新扫描结果。
	go s.reindexAfterAnalyze(req.ProjectID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// intelDetectedModules is the outcome of detecting a project's module set
// across its primary root and any associated source repos.
type intelDetectedModules struct {
	root     string
	allRoots []string
	mods     []*store.IntelModule
	sources  []*store.IntelProjectSource
	scans    []intelModuleScan
}

// detectIntelModules resolves the project's working directory, detects modules
// across the primary root and every associated source repo (multi-端 multi-repo),
// and pairs each module with its owning repo root. Modules from associated repos
// get a "@<端>/" rel_path prefix so they stay unique against the main repo.
func (s *Server) detectIntelModules(ctx context.Context, p *store.IntelProject) (*intelDetectedModules, error) {
	root, err := s.projectRoot(ctx, p)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return nil, errNotDir(root)
	}
	out := &intelDetectedModules{root: root, allRoots: []string{root}}
	out.sources, _ = s.store.ListIntelProjectSources(ctx, p.ID)
	mainMods, err := intel.DetectModules(root)
	if err != nil {
		return nil, err
	}
	out.mods = append(out.mods, mainMods...)
	for _, src := range out.sources {
		srcRoot, err := s.resolveIntelSourceRoot(ctx, p, src)
		if err != nil {
			log.Printf("intel project %d source %d root: %v", p.ID, src.ID, err)
			continue
		}
		out.allRoots = append(out.allRoots, srcRoot)
		srcMods, err := intel.DetectModules(srcRoot)
		if err != nil {
			continue
		}
		prefix := intelSourcePrefix(src)
		for _, m := range srcMods {
			m.RelPath = prefix + m.RelPath
		}
		out.mods = append(out.mods, srcMods...)
	}
	// 每个模块的工作目录与其归属仓库根目录（绑定/来源路径按各自仓库相对）。
	for _, m := range out.mods {
		scanRoot, rel := root, m.RelPath
		if srcRoot, srcRel, ok := s.sourceModuleRoot(ctx, p, out.sources, m.RelPath); ok {
			scanRoot, rel = srcRoot, srcRel
		}
		dir := scanRoot
		if rel != "." {
			dir = filepath.Join(scanRoot, rel)
		}
		out.scans = append(out.scans, intelModuleScan{m: m, dir: dir, root: scanRoot, rel: rel})
	}
	return out, nil
}

// isIntelAutoAllowed debounces the source-change auto-analyze trigger: a
// project is allowed at most once every intelAutoInterval, preventing a page
// poll / rapid detail opens from queueing several full analyzes back to back.
const intelAutoInterval = 2 * time.Minute

func (s *Server) isIntelAutoAllowed(projectID int64) bool {
	s.intelAutoMu.Lock()
	defer s.intelAutoMu.Unlock()
	last, ok := s.intelAutoLast[projectID]
	now := time.Now()
	if ok && now.Sub(last) < intelAutoInterval {
		return false
	}
	s.intelAutoLast[projectID] = now
	return true
}

// autoAnalyzeIntel runs a full or incremental analysis for a project outside
// the request's short-lived context. Full analysis runs on project creation;
// incremental runs whenever the project's associated source directory/repo is
// changed, so only added/removed modules are re-scanned. Errors are logged but
// never fail the enclosing request (the project itself was already saved).
// Analyses of the same project are serialized by intelAnalyzeMu; callers fire
// this via `go` so the HTTP handler never blocks on a multi-minute analyze.
func (s *Server) autoAnalyzeIntel(projectID int64, incremental bool) {
	mu := s.intelAnalyzeMutex(projectID)
	mu.Lock()
	defer mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var err error
	if incremental {
		err = s.runIntelAnalyzeIncremental(ctx, projectID)
	} else {
		err = s.runIntelAnalyze(ctx, projectID)
	}
	if err != nil {
		log.Printf("intel auto-analyze project %d (incremental=%v): %v", projectID, incremental, err)
		if merr := s.store.MarkIntelAnalyzeFailed(ctx, projectID); merr != nil {
			log.Printf("intel analyze fail marker project %d: %v", projectID, merr)
		}
		return
	}
	go s.reindexAfterAnalyze(projectID)
}

// intelAnalyzeMutex returns the per-project analyze mutex (serializes full and
// incremental analyses so concurrent creates/source-changes cannot corrupt the
// intel tables with racing replaces).
func (s *Server) intelAnalyzeMutex(projectID int64) *sync.Mutex {
	mu, _ := s.intelAnalyzeMu.LoadOrStore(projectID, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// runIntelAnalyze resolves the project's working directory, detects modules,
// scans contracts per module and persists everything. It records snapshot sha
// (HEAD for git, directory-mtime-hash for local non-git).
func (s *Server) runIntelAnalyze(ctx context.Context, projectID int64) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := s.store.MarkIntelAnalyzeStarted(ctx, projectID); err != nil {
		log.Printf("intel analyze start marker project %d: %v", projectID, err)
	}
	detected, err := s.detectIntelModules(ctx, p)
	if err != nil {
		return err
	}
	root := detected.root
	allRoots := detected.allRoots
	mods := detected.mods
	scans := detected.scans
	if err := s.store.ReplaceIntelModules(ctx, projectID, mods); err != nil {
		return err
	}
	mods, _ = s.store.ListIntelModules(ctx, projectID)
	p.CommandsJSON = intel.ProjectCommandsJSON(mods)
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		log.Printf("intel project commands %d: %v", projectID, err)
	}
	// 重新读取模块以拿到稳定 id（ReplaceIntelModules 返回后模块行才确定），
	// 再用新 id 覆盖 scan 中的模块引用。
	modsByRel := make(map[string]*store.IntelModule, len(mods))
	for _, m := range mods {
		modsByRel[m.RelPath] = m
	}
	for i := range scans {
		if m, ok := modsByRel[scans[i].m.RelPath]; ok {
			scans[i].m = m
		}
	}
	var allEndpoints []*store.IntelEndpoint
	var allEntities []*store.IntelEntity
	var allCases []*store.TestCase
	for _, sc := range scans {
		sum, err := intel.ScanModule(sc.root, sc.rel)
		if err != nil {
			continue
		}
		for i := range sum.Entities {
			sum.Entities[i].ModuleID = sc.m.ID
		}
		for i := range sum.Endpoints {
			sum.Endpoints[i].ModuleID = sc.m.ID
		}
		allEntities = append(allEntities, sum.Entities...)
		allEndpoints = append(allEndpoints, sum.Endpoints...)
		assets, err := testassets.Discover(sc.root, sc.rel)
		if err == nil {
			allCases = append(allCases, buildTestCases(sc.m, assets)...)
		}
	}
	if err := s.store.ReplaceIntelEntities(ctx, projectID, allEntities); err != nil {
		return err
	}
	if err := s.store.ReplaceIntelEndpoints(ctx, projectID, allEndpoints); err != nil {
		return err
	}
	if err := s.store.ReplaceIntelTestCases(ctx, projectID, allCases); err != nil {
		return err
	}
	if err := s.persistFeatures(ctx, projectID, allEndpoints); err != nil {
		return err
	}
	if err := s.persistAndroidBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel android bindings project %d: %v", projectID, err)
	}
	if err := s.persistWebBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel web bindings project %d: %v", projectID, err)
	}
	if err := s.persistIosBindings(ctx, projectID, scans); err != nil {
		log.Printf("intel ios bindings project %d: %v", projectID, err)
	}
	if err := s.persistGatewayRoutes(ctx, projectID, allRoots); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, allRoots)
	s.persistOverview(ctx, projectID, p, allRoots)
	s.persistEnvRequirements(ctx, projectID, allRoots)
	sha, err := snapshotSHA(root)
	if err != nil {
		sha = ""
	}
	if err := s.runIntelComplianceScan(ctx, projectID, allRoots); err != nil {
		log.Printf("intel compliance scan project %d: %v", projectID, err)
	}
	if err := s.runIntelSecurityScan(ctx, projectID, allEntities); err != nil {
		log.Printf("intel security scan project %d: %v", projectID, err)
	}
	s.recordImpact(ctx, projectID, p, root, sha)
	if err := s.store.MarkIntelProjectAnalyzed(ctx, projectID, sha); err != nil {
		return err
	}
	// 依赖链（dependsOn 的轻量版）：分析完成后若开启 intel.auto_run，自动触发
	// 全量回归，避免「改代码→分析→手动点运行」的往返。
	if v, err := s.store.GetSetting(ctx, "intel.auto_run"); err == nil && v == "1" {
		if run, err := s.enqueueIntelRunAll(context.Background(), projectID, false); err != nil {
			log.Printf("intel analyze->run project %d: %v", projectID, err)
		} else {
			log.Printf("intel analyze->run project %d enqueued run %d", projectID, run.ID)
		}
	}
	return nil
}

// runIntelAnalyzeIncremental refreshes a project's analysis without a full
// rescan: it diffs the freshly-detected module set against the persisted one,
// deletes the removed modules' derived data (entities/endpoints/test cases) and
// only scans the newly-added modules (append). Project-level profiles — feature
// points, client bindings, gateway routes, overview, env requirements,
// compliance, security, impact and LLM summaries — are refreshed so the
// snapshot stays coherent without re-analyzing untouched modules. It is
// triggered automatically when a project's associated source directory or
// repository changes (module add/remove), and runs a full analysis otherwise.
func (s *Server) runIntelAnalyzeIncremental(ctx context.Context, projectID int64) error {
	p, err := s.store.GetIntelProject(ctx, projectID)
	if err != nil {
		return err
	}
	if err := s.store.MarkIntelAnalyzeStarted(ctx, projectID); err != nil {
		log.Printf("intel analyze start marker project %d: %v", projectID, err)
	}
	detected, err := s.detectIntelModules(ctx, p)
	if err != nil {
		return err
	}
	existing, err := s.store.ListIntelModules(ctx, projectID)
	if err != nil {
		return err
	}
	existingByRel := make(map[string]*store.IntelModule, len(existing))
	for _, m := range existing {
		existingByRel[m.RelPath] = m
	}
	newByRel := make(map[string]*store.IntelModule, len(detected.mods))
	for _, m := range detected.mods {
		newByRel[m.RelPath] = m
	}
	var added, removed []*store.IntelModule
	for rel, m := range newByRel {
		if _, ok := existingByRel[rel]; !ok {
			added = append(added, m)
		}
	}
	for rel, m := range existingByRel {
		if _, ok := newByRel[rel]; !ok {
			removed = append(removed, m)
		}
	}
	// 移除已删除模块的实体/接口/用例数据（按 project_id + module_id 删除）。
	for _, m := range removed {
		if err := s.store.DeleteIntelModuleEntities(ctx, projectID, m.ID); err != nil {
			return err
		}
		if err := s.store.DeleteIntelModuleEndpoints(ctx, projectID, m.ID); err != nil {
			return err
		}
		if err := s.store.DeleteIntelModuleTestCases(ctx, projectID, m.ID); err != nil {
			return err
		}
	}
	// 同步模块表：保留稳定 id，插入新增模块，删除移除模块。
	if err := s.store.ReplaceIntelModules(ctx, projectID, detected.mods); err != nil {
		return err
	}
	mods, _ := s.store.ListIntelModules(ctx, projectID)
	modsByRel := make(map[string]*store.IntelModule, len(mods))
	for _, m := range mods {
		modsByRel[m.RelPath] = m
	}
	// 项目级命令白名单随模块增删重新聚合（与全量分析一致）。
	p.CommandsJSON = intel.ProjectCommandsJSON(mods)
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		log.Printf("intel incremental commands %d: %v", projectID, err)
	}
	// 仅对新加模块做契约扫描并追加（已保留的模块数据不动）。
	for _, m := range added {
		persisted := modsByRel[m.RelPath]
		if persisted == nil {
			continue
		}
		var sc intelModuleScan
		for _, cand := range detected.scans {
			if cand.m.RelPath == m.RelPath {
				sc = cand
				sc.m = persisted
				break
			}
		}
		sum, err := intel.ScanModule(sc.root, sc.rel)
		if err != nil {
			continue
		}
		for i := range sum.Entities {
			sum.Entities[i].ModuleID = persisted.ID
		}
		for i := range sum.Endpoints {
			sum.Endpoints[i].ModuleID = persisted.ID
		}
		if err := s.store.AppendIntelEntities(ctx, projectID, sum.Entities); err != nil {
			return err
		}
		if err := s.store.AppendIntelEndpoints(ctx, projectID, sum.Endpoints); err != nil {
			return err
		}
		if assets, err := testassets.Discover(sc.root, sc.rel); err == nil {
			if err := s.store.AppendIntelTestCases(ctx, projectID, buildTestCases(persisted, assets)); err != nil {
				return err
			}
		}
	}
	// 用落库后的稳定 id 重建扫描列表，供绑定等全量刷新使用。
	freshScans := make([]intelModuleScan, 0, len(mods))
	for _, m := range mods {
		for _, cand := range detected.scans {
			if cand.m.RelPath == m.RelPath {
				cand.m = m
				freshScans = append(freshScans, cand)
				break
			}
		}
	}
	// 功能点随分析重算：基于当前全量接口重新聚类（保留人工标记）。
	endpoints, err := s.store.ListIntelEndpoints(ctx, projectID, 0)
	if err != nil {
		return err
	}
	if err := s.persistFeatures(ctx, projectID, endpoints); err != nil {
		return err
	}
	// 项目级画像刷新：绑定 / 网关 / 概览 / 环境 / 合规 / 安全 / 影响 / LLM。
	if err := s.persistAndroidBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel android bindings project %d: %v", projectID, err)
	}
	if err := s.persistWebBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel web bindings project %d: %v", projectID, err)
	}
	if err := s.persistIosBindings(ctx, projectID, freshScans); err != nil {
		log.Printf("intel ios bindings project %d: %v", projectID, err)
	}
	if err := s.persistGatewayRoutes(ctx, projectID, detected.allRoots); err != nil {
		log.Printf("intel gateway routes project %d: %v", projectID, err)
	}
	s.enrichIntelWithLLM(ctx, projectID, detected.allRoots)
	s.persistOverview(ctx, projectID, p, detected.allRoots)
	s.persistEnvRequirements(ctx, projectID, detected.allRoots)
	sha, err := snapshotSHA(detected.root)
	if err != nil {
		sha = ""
	}
	if err := s.runIntelComplianceScan(ctx, projectID, detected.allRoots); err != nil {
		log.Printf("intel compliance scan project %d: %v", projectID, err)
	}
	entities, err := s.store.ListIntelEntities(ctx, projectID, 0)
	if err != nil {
		return err
	}
	if err := s.runIntelSecurityScan(ctx, projectID, entities); err != nil {
		log.Printf("intel security scan project %d: %v", projectID, err)
	}
	s.recordImpact(ctx, projectID, p, detected.root, sha)
	return s.store.MarkIntelProjectAnalyzed(ctx, projectID, sha)
}
