package server

// Project management for the test-intelligence module: list/create entry
// handlers plus the CRUD and source-list operations they dispatch to.

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

import (
	"github.com/hiylo/starburst-backend/internal/store"
)

// handleIntelProjects lists (GET) and creates (POST) test-intelligence
// projects. Listing requires a web session or APP token; creating requires a
// web session — see createIntelProject.
func (s *Server) handleIntelProjects(w http.ResponseWriter, r *http.Request) {
	isWeb := s.requireWeb(r)
	if !isWeb {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	if r.Method == http.MethodPost && !isWeb {
		writeErr(w, http.StatusForbidden, "创建项目需要管理员 web 会话")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listIntelProjects(w, r)
	case http.MethodPost:
		s.createIntelProject(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleIntelProjectByID handles a single project: GET detail (with modules),
// DELETE removal, PUT update.
func (s *Server) handleIntelProjectByID(w http.ResponseWriter, r *http.Request) {
	isWeb := s.requireWeb(r)
	if !isWeb {
		if _, ok := s.requireToken(r); !ok {
			writeErr(w, http.StatusUnauthorized, "web session or APP token required")
			return
		}
	}
	// 写操作（PUT 改源码目录、DELETE 删项目）收紧到管理员：PUT 决定后续被 walk
	// + ReadFile 读哪里，DELETE 不可逆；GET 仍对 APP token 开放。
	if (r.Method == http.MethodPut || r.Method == http.MethodDelete) && !isWeb {
		writeErr(w, http.StatusForbidden, "修改或删除项目需要管理员 web 会话")
		return
	}
	id, ok := intelPathID(w, r, "/api/intel/projects/")
	if !ok {
		return
	}
	// 关联源码目录/仓库子资源：/api/intel/projects/{id}/sources
	if strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/intel/projects/"+strconv.FormatInt(id, 10)), "/") == "sources" {
		s.handleIntelProjectSources(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getIntelProject(w, r, id)
	case http.MethodDelete:
		s.deleteIntelProject(w, r, id)
	case http.MethodPut:
		s.updateIntelProject(w, r, id)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listIntelProjects(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	projects, err := s.store.ListIntelProjects(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list projects failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

func (s *Server) createIntelProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		LocalPath   string `json:"localPath"`
		GitURL      string `json:"gitUrl"`
		GitRef      string `json:"gitRef"`
		Description string `json:"description"`
	}
	if !readBody(w, r, &req) {
		return
	}
	req.Source = strings.TrimSpace(req.Source)
	req.LocalPath = strings.TrimSpace(req.LocalPath)
	req.GitURL = strings.TrimSpace(req.GitURL)
	req.Name = strings.TrimSpace(req.Name)
	req.Description = strings.TrimSpace(req.Description)
	if req.Source == "" {
		req.Source = "local"
	}
	if req.Source == "git" && req.GitURL == "" {
		writeErr(w, http.StatusBadRequest, "gitUrl is required for source=git")
		return
	}
	if req.Source == "local" && req.LocalPath == "" {
		writeErr(w, http.StatusBadRequest, "localPath is required for source=local")
		return
	}
	if req.Source == "local" {
		if err := validateIntelLocalPath(req.LocalPath); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		req.LocalPath = filepath.Clean(req.LocalPath)
	}
	if req.Name == "" {
		req.Name = deriveProjectName(req.LocalPath, req.GitURL)
	}
	p := &store.IntelProject{
		Name:        req.Name,
		Source:      req.Source,
		LocalPath:   req.LocalPath,
		GitURL:      req.GitURL,
		GitRef:      req.GitRef,
		Description: req.Description,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.store.CreateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "create project failed")
		return
	}
	// 添加项目即自动执行首次全量分析。先在处理协程里占住项目级锁再在后台跑，
	// 保证首次分析必然先于后续的显式分析/增量分析执行（否则两次全量分析谁后到
	// 谁重建 endpoint id，会失效客户端已拿到的引用）。创建响应不被阻塞。
	mu := s.intelAnalyzeMutex(p.ID)
	mu.Lock()
	go func() {
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.runIntelAnalyze(ctx, p.ID); err != nil {
			log.Printf("intel auto-analyze project %d: %v", p.ID, err)
			if merr := s.store.MarkIntelAnalyzeFailed(ctx, p.ID); merr != nil {
				log.Printf("intel analyze fail marker project %d: %v", p.ID, merr)
			}
			return
		}
		go s.reindexAfterAnalyze(p.ID)
	}()
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) getIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.store.GetIntelProject(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	// 源码变更自动分析：对已分析过的本地项目，对比当前 snapshotSHA 与记录值，
	// 发现变化（代码改动）即后台触发全量分析，让契约/告警/画像自动跟上改动，
	// 无需用户手动点分析。git 项目以 HEAD 为准（snapshotSHA 即 HEAD）；本地
	// 非 git 项目用目录 hash（已含文件大小+mtime）。分析进行中（running）不
	// 重复触发；先判 SHA 变化、确认真正有变更才消耗防抖预算（否则普通的详情
	// 查看会把预算耗尽），且同项目 2 分钟防抖避免排队堆积全量分析。
	if p.AnalyzedAt != nil && p.AnalysisStatus != "running" && p.Source == "local" && p.LocalPath != "" {
		if cur, err := snapshotSHA(p.LocalPath); err == nil && cur != "" && cur != p.SnapshotSHA {
			if s.isIntelAutoAllowed(id) {
				log.Printf("intel project %d source changed (%s…), auto-triggering full analyze", id, cur[:min(8, len(cur))])
				go s.autoAnalyzeIntel(id, false)
			}
		}
	}
	mods, err := s.store.ListIntelModules(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load modules failed")
		return
	}
	s.applyModuleOverrides(ctx, id, mods)
	writeJSON(w, http.StatusOK, map[string]any{"project": p, "modules": mods})
}

func (s *Server) deleteIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.store.DeleteIntelProject(ctx, id); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete project failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) updateIntelProject(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name        string `json:"name"`
		Source      string `json:"source"`
		LocalPath   string `json:"localPath"`
		GitURL      string `json:"gitUrl"`
		GitRef      string `json:"gitRef"`
		Description string `json:"description"`
		Commands    string `json:"commandsJson"`
		EnvName     string `json:"envName"`
	}
	if !readBody(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := s.store.GetIntelProject(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	// 记录来源相关字段是否发生变更：只有关联源码目录/仓库变化才触发增量分析，
	// 仅改名称/描述/命令白名单/环境不触发。
	prevSource := p.Source
	prevLocal := p.LocalPath
	prevGit := p.GitURL
	prevRef := p.GitRef
	prevDesc := p.Description
	if req.Name != "" {
		p.Name = req.Name
	}
	if req.Description != "" {
		p.Description = req.Description
	}
	// 关联源码目录/仓库：详情页「项目管理」可调整来源与路径，与创建语义一致。
	switch req.Source {
	case "":
		// 未携带 source 时按增量更新处理（如仅改命令白名单/名称）。
		if req.GitRef != "" {
			p.GitRef = req.GitRef
		}
	case "local", "git":
		req.LocalPath = strings.TrimSpace(req.LocalPath)
		req.GitURL = strings.TrimSpace(req.GitURL)
		req.GitRef = strings.TrimSpace(req.GitRef)
		if req.Source == "local" {
			if req.LocalPath == "" {
				writeErr(w, http.StatusBadRequest, "local 来源必须填写源码目录路径")
				return
			}
			if err := validateIntelLocalPath(req.LocalPath); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
			p.Source = "local"
			p.LocalPath = filepath.Clean(req.LocalPath)
			p.GitURL = ""
		} else {
			if req.GitURL == "" {
				writeErr(w, http.StatusBadRequest, "git 来源必须填写仓库 URL")
				return
			}
			p.Source = "git"
			p.GitURL = req.GitURL
			p.GitRef = req.GitRef
			p.LocalPath = ""
		}
	default:
		writeErr(w, http.StatusBadRequest, "source 只能是 local 或 git")
		return
	}
	if req.Commands != "" {
		// 命令白名单是项目级别：必须是 JSON 字符串数组，且逐条去除首尾空白、
		// 丢弃空串，再落库（运行期按 strings.Fields 拆分 argv、不经 shell 执行）。
		var list []string
		if err := json.Unmarshal([]byte(req.Commands), &list); err != nil {
			writeErr(w, http.StatusBadRequest, "commandsJson 必须是 JSON 字符串数组")
			return
		}
		clean := make([]string, 0, len(list))
		for _, c := range list {
			c = strings.TrimSpace(c)
			if c == "" {
				continue
			}
			clean = append(clean, c)
		}
		b, err := json.Marshal(clean)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "encode commands failed")
			return
		}
		p.CommandsJSON = string(b)
	}
	if req.EnvName != "" {
		p.EnvName = req.EnvName
	}
	if err := s.store.UpdateIntelProject(ctx, p); err != nil {
		writeErr(w, http.StatusInternalServerError, "update project failed")
		return
	}
	// 关联源码目录/仓库发生变更时自动执行增量分析：仅扫描新增/移除的模块，
	// 已有模块数据保持不变（在独立后台 context 下进行，不阻塞保存响应）。
	sourceChanged := prevSource != p.Source || prevLocal != p.LocalPath ||
		prevGit != p.GitURL || prevRef != p.GitRef
	if sourceChanged {
		go s.autoAnalyzeIntel(id, true)
	}
	// 描述变更时刷新知识库 overview chunk，让可检索画像与新描述一致（问答侧靠
	// 实时注入始终正确；若来源也变更，增量分析已包含索引重建，无需重复触发）。
	if p.Description != prevDesc && !sourceChanged {
		go s.reindexAfterAnalyze(id)
	}
	writeJSON(w, http.StatusOK, p)
}

// handleIntelProjectSources manages the project's associated source repos
// (多端多仓库): GET lists them, PUT replaces the whole list.
func (s *Server) handleIntelProjectSources(w http.ResponseWriter, r *http.Request, projectID int64) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		sources, err := s.store.ListIntelProjectSources(ctx, projectID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "load project sources failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
	case http.MethodPut:
		var req struct {
			Sources []*struct {
				EndName   string `json:"endName"`
				Source    string `json:"source"`
				LocalPath string `json:"localPath"`
				GitURL    string `json:"gitUrl"`
				GitRef    string `json:"gitRef"`
			} `json:"sources"`
		}
		if !readBody(w, r, &req) {
			return
		}
		sources := make([]*store.IntelProjectSource, 0, len(req.Sources))
		seen := make(map[string]bool)
		for _, item := range req.Sources {
			if item == nil {
				continue
			}
			src := &store.IntelProjectSource{
				EndName:   strings.TrimSpace(item.EndName),
				Source:    strings.TrimSpace(item.Source),
				LocalPath: strings.TrimSpace(item.LocalPath),
				GitURL:    strings.TrimSpace(item.GitURL),
				GitRef:    strings.TrimSpace(item.GitRef),
			}
			if src.Source == "" {
				src.Source = "local"
			}
			switch src.Source {
			case "local":
				if src.LocalPath == "" {
					writeErr(w, http.StatusBadRequest, "local 关联源码必须填写目录路径")
					return
				}
				if err := validateIntelLocalPath(src.LocalPath); err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				src.LocalPath = filepath.Clean(src.LocalPath)
				src.GitURL = ""
				src.GitRef = ""
			case "git":
				if src.GitURL == "" {
					writeErr(w, http.StatusBadRequest, "git 关联源码必须填写仓库 URL")
					return
				}
				src.LocalPath = ""
			default:
				writeErr(w, http.StatusBadRequest, "source 只能是 local 或 git")
				return
			}
			if src.EndName != "" {
				if seen[src.EndName] {
					writeErr(w, http.StatusBadRequest, "端名不能重复: "+src.EndName)
					return
				}
				seen[src.EndName] = true
			}
			sources = append(sources, src)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		prev, _ := s.store.ListIntelProjectSources(ctx, projectID)
		if err := s.store.ReplaceIntelProjectSources(ctx, projectID, sources); err != nil {
			writeErr(w, http.StatusInternalServerError, "save project sources failed")
			return
		}
		// 关联源码列表发生变更时自动执行增量分析（模块增删按 rel_path 差分）。
		if !intelSourcesEqual(prev, sources) {
			go s.autoAnalyzeIntel(projectID, true)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sources": sources})
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
