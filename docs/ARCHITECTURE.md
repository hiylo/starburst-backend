# StarBurst Backend 架构文档

## 1. 定位与边界

StarBurst Backend 是运行在**开发机本机**的轻量编排层，位于 OpenCode 服务与客户端（APP / Web）之间。它**不是透传代理**——核心价值是对本机 OpenCode 做**编排**（调度任务、聚合会话、事件触发、归档）并为客户端提供**推送**；在此之上还内建了**测试智能子系统**（`internal/intel` + `internal/server/intel_*.go`）：静态理解被测仓库、提取接口与数据契约、发现并执行测试资产、做失败归因与修复建议。按路由数计，`/api/intel/*` 已占 `Routes()` 注册 98 条中的 56 条，是最大的一块 API 面。

```
┌──────────┐  直连(可选)   ┌──────────────────┐
│  APP/Web │ ────────────▶ │   OpenCode 服务   │
│          │               │  (127.0.0.1:4096) │
└────┬─────┘               └────────▲─────────┘
     │ Token + WS 推送              │  HTTP 编排调用 / 全局事件流 / 镜像代理
     ▼                              │
┌────────────────────────────────────┴───────────┐
│           starburst-backend（单二进制）          │
│  HTTP API + WebSocket 推送 + 任务调度 + 自动化    │
│  测试智能（扫描/契约/执行/归因/覆写/环境门禁）      │
│  会话事件采集 + 硬件阈值告警                       │
│  SQLite / PostgreSQL（pgvector 可选）             │
└─────────────────────────────────────────────────┘
```

进程内常驻的后台协程（均在 `cmd/starburst-backend/main.go` 挂载）：任务 worker 池、自动化引擎轮询、告警采样、会话事件采集器、异步审计批量落库、任务调度器（定时/周期）、上游健康心跳、6 小时一次的历史清理（审计保留 30 天、过期 Web 会话）。

## 2. 凭据模型

两套独立凭据，互不通用：

- **Web Session**（`X-Web-Session` header）：`POST /api/web/session` 用管理密码换取，写入 `web_sessions` 表（迁移 v7），有效期 24h，**重启不掉线**；过期行由 6 小时一次的清理协程回收。改密码（`POST /api/web/password`，必须带旧密码）会吊销除当前会话外的全部 Web Session。
- **APP Token**（`Authorization: Bearer ocb_...`，或 WS/SSE 场景下的 `?token=`）：DB 只存 sha256 哈希，多设备各一个、可撤销；每次命中按 token 节流（60s）更新 `last_used`。

### 2.1 五类鉴权边界

handler 里的**主流惯用法是双通道**——先 `requireWeb`，不通过再 `requireToken`，任一命中即放行；只有少数端点是单凭据的。逐条矩阵见 [docs/API.md](API.md) 的「鉴权类别速查」，此处只给分类与代表端点：

| 类别 | 判定方式（代码里） | 端点 |
|------|------------------|------|
| **Public** | handler 内不查凭据 | `GET /api/health`、`POST /api/web/session`（登录本身，带同 IP 限流）、`GET /`（配置页静态资源，登录态由页面自己管） |
| **共享密钥** | 校验 `X-Webhook-Secret` 常量时间比较 | `POST /api/webhook`；未配置 `--webhook-secret` 时一律 403 |
| **仅 APP Token** | 只调 `requireToken`（`?token=` 不适用） | `/api/batch`、`/api/tasks/generate`、`/api/llm/generate`、`/api/llm/complete`、`/api/stt/**` |
| **仅 Web Session** | 只调 `requireWeb` | `POST /api/web/password`、`DELETE /api/web/session`、`/api/llm`、`/api/embed` |
| **双通道** | `if !requireWeb { if !requireToken → 401 }` | 其余绝大多数：`/api/system`、`/api/tokens*`、`/api/ws`、`/api/stream`、`/api/projects*`、`/api/tasks*`、`/api/workflow*`、`/api/rules*`、`/api/audit`、`/api/stats`、`/api/sync`、`/api/archives*`、`/api/events`、`/api/unread*`、`/api/alerts`、`/api/opencode/*`、`/api/intel/*`（项目管理写操作除外，见下） |

几处例外值得设计时记住：

- `/api/opencode/*` 镜像代理入口是双通道，但**上游高危操作只允许 Web Session**（`proxyIsSensitive`：`/auth*` 写 provider key、`/pty*` 开终端、`/global/dispose*`、`/global/config*`、`/config*`），APP Token 命中这些路径返回 403；普通读取与 permission 回复保持开放，App 才能远程批准。唯一的放行例外是 `GET /config/providers`——两端模型下拉框都靠它，改由回程剥离兜底。
- 同一层还有**凭据剥离**（`providerCredentialPath` + `redactCredentials`）：`/config*`、`/provider*` 的响应里，字段名以 `key`/`token`/`secret`/`password` 一类结尾的字符串在回程置空，因为上游会把 provider 明文 API Key 塞进 provider 定义。触发不看 `Content-Type`（上游漏带类型不能变成放行理由），解析不了就 502，绝不退化成透传；请求体另有 64 MiB 上限（`maxProxyRequestBytes`）。
- `/api/ws` 与 `/api/stream` 除 Bearer 头外还接受 `?token=` / `?session=`（浏览器与 WS 无法自定义头）。
- **intel 项目管理的四个写操作只允许 Web Session**（`POST /api/intel/projects`、`PUT`/`DELETE /api/intel/projects/{id}`、`PUT /api/intel/projects/{id}/sources`，APP Token → 403）。判据是**这个接口决定了后端去读哪个目录**：`localPath` 之后会被当作工作目录 walk + ReadFile 并把内容回传，所以入口先过 `validateIntelLocalPath`（绝对路径、真实目录、软链接解析后仍不在 `/etc` `/proc` `/root` 等系统目录内），管理动作再收给管理员会话。同项目的 `GET` 保持双通道——安卓端仍要读列表。
- **出网目标校验**（`internal/netguard`）：凡「目标地址由客户端给出、由后端发请求」的接口（`features/test`、`contracts/check-batch`、环境探测与 `adb connect`、远程构建节点）都必须先 `CheckHost` 再 `Dial`。私网/回环**刻意放行**（被测服务就在本机与内网），禁的是链路本地元数据、未指定/组播地址和各家云的元数据常量；域名先解析且每个 IP 都要通过，随后按已校验的 IP 拨号并不跟随重定向，消掉 DNS rebinding 与一跳 302 两条 TOCTOU 旁路。
- **目录入参**（`internal/server/paths.go`）：`directory`（任务/批量/工作流/规则）与 intel 的 `localPath` 都是「后端会切进去操作」的客户端入参——前者成为 agent 工作目录（`kind=git` 规则还本机跑 `git -C`），后者整棵子树被 walk + 回传。两者共用系统目录禁区（`/boot` `/dev` `/etc` `/proc` `/root` `/sys` 及子树，软链接按解析后判定）与 `..` 越界判定；`localPath` 另加「绝对路径 + 真实目录 + 不能是顶层单段目录」。**没有做白名单根目录**：这层是编排器不是沙箱，仓库本就散落在用户磁盘上，需要更严格的隔离应当用操作系统手段（容器 / 独立用户），而不是在路径规则里假装成沙箱。

### 2.2 审计中间件

`logMiddleware` 只对**由 APP Token 认证**的 `/api/*` 请求记账（Web Session 调用不落审计表），入队后由 `StartAuditFlusher` 批量 INSERT；`/api/stt**` 整棵子树跳过——一段 10 秒录音会产生几十行噪音。

## 3. 模块划分

| 包 | 职责 |
|----|------|
| `internal/config` | flag + 环境变量解析（env 只提供默认值，显式 flag 覆盖 env），`--version`/`--health-check` 动作，`Version` 由 CI 打 tag 时 ldflags 注入 |
| `internal/store` | `database/sql` 抽象层：SQLite(modernc, WAL + busy_timeout) / PostgreSQL(pgx)；47 步版本化迁移；LLM/embedding/告警等运行时配置存 `settings` KV |
| `internal/auth` | bcrypt 管理密码 + `ocb_` Token（sha256 哈希存储、多设备、可撤销、首次运行可预置默认 token） |
| `internal/netguard` | 客户端指定出网目标的统一校验：先解析域名再判、按已校验 IP 拨号、不跟随重定向；放行回环/私网（被测服务），禁链路本地元数据与未指定/组播地址 |
| `internal/opencode` | OpenCode 客户端：健康、会话、消息拉取、Markdown 导出，以及镜像代理用的通用 `Do()`（流式转发不缓冲） |
| `internal/llm` | 可插拔 OpenAI 兼容 chat completions 客户端；未配置即禁用，调用短路，不阻塞主流程 |
| `internal/embed` | 可插拔 OpenAI 兼容 embeddings 客户端，供知识库向量检索；同样可缺省 |
| `internal/tasks` | 任务执行器：worker 池 claim→执行→重试（指数退避）、同 sessionId 串行门、保留期清理、LLM 结果摘要与失败自愈、severity 推送 |
| `internal/automation` | 规则引擎：6 字段秒级 cron + `git` / `http` 触发，命中即建任务并记 `rule_executions` |
| `internal/alerts` | 本机 CPU/内存/磁盘采样（Linux 读 `/proc`，其它平台降级）+ 阈值判定，越线广播 `alert.hardware` |
| `internal/push` | WS Hub：连接注册/广播、severity 分级、单连接写超时与半开回收 |
| `internal/intel` | 测试智能子系统（见 3.1）：确定性优先——表/字段/必填事实来自静态代码提取，LLM 只做解释、补全与归因 |
| `internal/server` | HTTP/WS 路由与 handler（`Routes()` 注册 98 条）、审计中间件、事件采集器、调度器、intel/rag/env/stt 等端点；`intel_*.go` 是测试智能的接入层 |
| `internal/webui` | `go:embed` 配置页静态资源：11 个入口（AI 工作台 / 任务 / 编排 / 实时流 / 项目与会话 / 自动化规则 / 会话归档 / 智能测试 / 审计日志 / Token / 设置），主题走 `html[data-theme]` token 覆盖，取值与安卓端 Theme.kt 对齐 |

### 3.1 internal/intel 子包（19 个）

| 子包 | 职责（取自各包 doc comment） |
|------|------------------------------|
| `testassets` | 遍历仓库按框架/类型分类既有测试文件（junit / go-test / vitest / jest / playwright / xctest / pytest / gradle），落 `test_cases` |
| `report` | 解析测试框架报告（Maven Surefire XML、`go test -json`、Playwright JSON）为统一的逐用例结果模型 |
| `rootcause` | 确定性失败归因：按关键字/模式把一次失败分类为 backend / database / client / env / contract |
| `contract` | 用接口字段契约校验实际响应 JSON |
| `feature` | 把接口契约聚类为候选功能点 |
| `delta` | Git 增量影响分析：分类变更文件并决定是否升级为全模块回归 |
| `deps` | 解析 Maven/Go/npm/Gradle 依赖清单为统一 Dependency 列表，供漏洞扫描层与快照去重 |
| `sbom` | 把依赖列表渲染为 CycloneDX 1.5 JSON SBOM（纯数据转换，无网络/文件/执行） |
| `compliance` | 确定性源码合规扫描（版权头、Javadoc、禁用注解、格式、密钥卫生），结论带 file:line 溯源 |
| `security` | 敏感字段检测，把普通 issue 升级为安全告警（固定 severity） |
| `gateway` | 解析 API 网关路由配置（Spring Cloud Gateway 静态路由 + Nacos 元数据），建模内网接口的公网暴露 |
| `android` / `web` / `ios` | 分别从 DataBinding 布局、Vue SFC 模板、SwiftUI 视图确定性地提取「页面 → 数据字段」绑定，即客户端必显字段清单 |
| `envdetect` | 纯静态推导测试环境依赖（中间件 + 工具链），不探测本机 |
| `envagent` | 在本机探测并供给环境：Docker 可用性、中间件容器、已装工具链，按需拉起可丢弃容器 |
| `schemainit` | 发现库初始化脚本（Flyway / Liquibase / 裸 SQL），供集成测试前执行 |
| `fix` | 生成修复建议的 unified diff 草稿并在内存里 dry-run 应用校验；本包自身不写文件 |
| `enrich` | 准备喂给 LLM 的文档分析素材（文档 + 接口清单），产出业务摘要与建议网关路由 |

根包（`profile.go` / `scan.go` / `java.go` / `go.go` / `commands.go`）持有技术栈 Profile 注册表（按锚点文件识别 android/ios/java/go/…）、模块识别与契约扫描入口、以及各技术栈默认构建/测试命令白名单模板。

## 4. 数据模型（迁移 v1-v47）

版本跟踪表 `schema_migrations(version, applied_at)`，两端迁移共用同一序列（`migrations` 切片当前 47 项，`migration.apply(ctx, driver, db)` 接收逻辑 driver，方言不同的 DDL 在步骤内分支）。

### 4.1 编排内核（v1-v17）

| 迁移 | 名称 | 表 / 列 | 说明 |
|------|------|---------|------|
| v1 | `initial` | `settings`, `tokens` | KV 配置；Token 哈希表 |
| v2 | `tasks` | `tasks` | 异步任务（含 attempts/状态机） |
| v3 | `tasks_available_at` | `tasks.available_at` | 重试退避门控 |
| v4 | `rules` | `rules` | 自动化规则（cron/git/http） |
| v5 | `audit_log` | `audit_log` | API 审计 |
| v6 | `archives` | `archives` | 会话归档 |
| v7 | `web_sessions` | `web_sessions` | Web 会话持久化（重启不掉线，按过期时间清理） |
| v8 | `rule_executions` | `rule_executions` | 规则触发 → 产生的任务 |
| v9 | `tasks_ai_summary` | `tasks.ai_summary` | LLM 结果摘要 |
| v10 | `tasks_depends_on` | `tasks.depends_on` | 任务依赖边 |
| v11 | `tasks_schedule` | `tasks.name/scheduled_at/cron/last_fired_at` | 一次性定时与周期模板任务 |
| v12 | `session_events` | `session_events` | 上游会话事件落库（看板历史） |
| v13 | `archives_raw_messages` | `archives.raw_messages` | 归档保留原始消息 |
| v14 | `session_unread` | `session_unread` | 跨端未读标记 |
| v15 | `tasks_priority_timeout_workflow` | `tasks.priority/timeout_seconds/workflow_id` | 优先级、超时、编排链 |
| v16 | `rules_session_id` | `rules.session_id` | 规则绑定会话 |
| v17 | `tasks_workflow_index` | `tasks` 索引 | 按 `workflow_id` 查链 |

### 4.2 测试智能与环境（v18-v47）

| 迁移 | 名称 | 表 / 列 | 说明 |
|------|------|---------|------|
| v18 | `intel` | `projects`, `project_modules`, `intel_entities`, `intel_endpoints` | 项目 / 子模块 / 实体↔表↔字段 / 接口契约 |
| v19 | `intel_field_meta` | `intel_entities.field_type,is_primary` | 字段类型与主键标记 |
| v20 | `intel_test_assets` | `test_cases`, `test_runs`, `test_results`, `intel_issues` | 用例、执行、逐用例结果、问题 |
| v21 | `intel_features` | `intel_features` | 功能点 |
| v22 | `intel_rag` | `intel_chunks` | 知识库分块；PG 下 `embedding vector(1024)` + HNSW 余弦索引并 `CREATE EXTENSION vector`，SQLite 退化为 TEXT |
| v23 | `intel_chat` | `intel_chats`, `intel_chat_messages` | 项目级多轮问答 |
| v24 | `intel_findings` | `intel_findings` | 审计结论（CVE / 合规 / 安全 / AI 规则） |
| v25 | `intel_fixes` | `intel_fixes` | 修复建议草稿与应用备份 |
| v26 | `intel_gateway_routes` | `intel_gateway_routes` | 网关暴露路由 |
| v27 | `intel_endpoint_summary` | `intel_endpoints.summary` | 接口业务摘要 |
| v28 | `intel_impacts` | `intel_impacts` | Git 增量影响快照 |
| v29 | `intel_overviews` | `intel_overviews` | 依赖 / 环境 / SBOM 概览快照 |
| v30 | `intel_fix_finding` | `intel_fixes.finding_id` | 修复关联到 finding |
| v31 | `intel_dedup` | — | 数据整理：按自然键删除实体/接口/用例/结论的重复行 |
| v32 | `intel_android_bindings` | `intel_android_bindings` | Android 布局字段绑定 |
| v33 | `intel_web_bindings` | `intel_web_bindings` | Vue 模板字段绑定 |
| v34 | `sync_bundle` | `sync_bundle` | 跨设备配置快照（key + revision） |
| v35 | `intel_ios_bindings` | `intel_ios_bindings` | SwiftUI 字段绑定 |
| v36 | `env` | `env_requirements`, `env_services` | 环境依赖项与探测/供给状态 |
| v37 | `intel_ai_rules` | `intel_ai_rules` | 全局 AI 建议规则 |
| v38 | `env_devices` | `env_devices` | adb 设备登记与项目绑定 |
| v39 | `remote_nodes` | `remote_nodes` | SSH 远程执行节点 |
| v40 | `intel_overrides` | `intel_overrides` | 人工/LLM 覆写（pending → applied/rejected） |
| v41 | `intel_feature_chats` | `intel_feature_chats` | 功能点问答记录 |
| v42 | `remote_nodes_work_dir` | `remote_nodes.work_dir` | 节点工作目录 |
| v43 | `project_modules_summary` | `project_modules.summary` | 模块 AI 摘要 |
| v44 | `projects_description` | `projects.description` | 项目画像自由文本（问答时恒定注入） |
| v45 | `intel_project_sources` | `project_sources` | 一个项目关联的多端多仓库源码 |
| v46 | `test_runs_progress` | `test_runs.progress,output` | run 进度文案与输出 |
| v47 | `intel_analysis_status` | `projects.analysis_status` | 后台分析生命周期 |

### 4.3 任务状态机

```
queued ──claim──▶ running ──成功──▶ succeeded
   ▲                 │失败
   │                 ▼
   └──retry◀──(attempts ≤ maxRetries)── failed（终态）
       退避: 5s,10s,20s,... 封顶 120s
queued/running ──cancel──▶ canceled
```

- `ClaimNextTask` 用单条 `UPDATE ... RETURNING` 保证单节点原子性，claim 时 `attempts+1`、检查 `available_at ≤ now`。SQLite 写串行化即天然互斥；PostgreSQL 额外用 `FOR UPDATE SKIP LOCKED`，否则并发 worker 会在任一方加锁前读到同一行、把同一任务 claim 两次。
- 执行器是 worker 池（`--workers`，默认 4，`1` 即串行）：每个 goroutine 独立 claim。显式共享同一个上游 `sessionId` 的任务由进程内 `sessionGate` 串行，避免并发读回同一次会话的最后一条回复。
- 重试通过 `RetryTask(id, backoffSecs)` 把任务置回 `queued` 并排 `available_at` 到未来；SQLite 与 PG 用不同时间运算方言（`rebind` 外的 driver 分支）。默认 `maxRetries = 2`（即首试外再 2 次），退避 `5 * 2^(attempts-1)` 秒、封顶 120 秒。
- 依赖与调度是状态机外的两条额外入队路径：`dependsOn` 未完成时任务落 `pending`，前置成功 `PromotePendingDependents` 转 `queued`、前置失败/取消 `BlockDependents` 转 `blocked`（`POST /api/tasks/{id}` 可人工解阻）；`scheduled_at` / `cron` 任务由 `RunScheduler` 到点提升为 `queued`（周期模板会克隆出实例子任务）。

### 4.4 自动化规则

`rules(kind, schedule, directory, session_id, prompt, enabled)`，三种 kind 由 `internal/automation` 的 15 秒轮询循环与 webhook 入口分别驱动：

- `cron`：`schedule` 是 6 字段秒级表达式（秒 分 时 日 月 周，支持 `*`、`*/n` 与逗号列表），引擎按周期评估到点即 `Fire`。
- `git`：`schedule`（留空则回退 `directory`）指向被观察的仓库目录，引擎记录 HEAD 基线，**首次观察不触发**，之后 HEAD 哈希变化即 `Fire`。
- `http`：由 `POST /api/webhook?target=` 匹配触发；未配置 `--webhook-secret` 时该端点一律 403，密钥只走 `X-Webhook-Secret` 头。

触发后统一经 `Fire` 建任务、标记 `last_fired_at` 并写 `rule_executions`。

### 4.5 测试智能的数据模型要点

- **确定性优先**：`intel_entities` / `intel_endpoints` / `test_cases` 等事实表每次分析按项目**整批替换**（`ReplaceIntel*`），LLM 只写描述性列（`summary`、`description`）与 `intel_overviews` / `intel_impacts` 快照，不作为事实来源。
- **人工覆写层（`intel_overrides`）**：人工或 LLM 校正不直接改事实表，而是按**自然键**登记——`target` + `row_key` + `field`（接口用 `"METHOD path"`、模块用 `rel_path`、功能点用 feature id），读取时由 `apply*Overrides` 合并覆盖。状态 `pending`（待确认队列，LLM 草稿先进这里）→ `applied` / `rejected`；重扫只重建自动值，`applied` 的人工值永远赢。
- **分析状态与陈旧判定**：`projects.analysis_status` 取 `""`（从未跑）/ `running` / `ok` / `failed`，UI 据此显示「进行中 / 失败」而非猜测 `analyzed_at` 是否为空。`projects.snapshot_sha` 是工作树快照哈希（git 项目取 HEAD，非 git 本地项目取「相对路径 + size + mtime」的有序 SHA-256，跳过 `.git`/`node_modules`/`target`/`.gradle`/`dist`）；`GET /api/intel/projects/{id}` 发现不一致即后台触发全量分析，同项目 2 分钟防抖、同一项目串行（`intelAnalyzeMu`）。
- **执行并发**：测试执行有同项目互斥（`intelExecMu`，一个项目同时只跑一个 run，避免多进程写同一模块构建产物）与全局水位 `intelExecConcurrency = 2`（限制同时存在的测试进程数，`run-all` 并行时不打满 CPU/IO），`intelCancels` 供 `POST /api/intel/runs/{id}/cancel` 终止。
- **向量检索只在 PG 生效**：`intel_chunks.embedding` 在 SQLite 下是普通 TEXT，因此 `vectorCapable`（见 `/api/system`）要求 pgvector 扩展已装 + embedding 客户端已配置，否则知识库问答/索引视为未配置。

## 5. 关键流程

### 5.1 提交异步任务

```
POST /api/tasks {prompt, directory?, dependsOn?}
  → store.CreateTask(status=queued；有未完成前置则 pending)
  → Executor.Run 起 N 个 worker（--workers）并发 ClaimNextTask
  → createSession（无 sessionId 时）→ SetTaskSession
  → POST /api/session/{id}/prompt  （V2 admitted：id 须 msg_ 前缀 + x-opencode-directory）
  → 轮询 /session/status 直到非 busy
  → GET /session/{id}/message 取最后 assistant 文本
  → CompleteTask / FailTask → WS 广播 task.event(severity)
  → 依赖结算：前置成功 PromotePendingDependents（pending/blocked → queued，推送 queued）
              前置失败/取消 BlockDependents（pending → blocked + reason，推送 warning）
  → POST /api/tasks/{id} 手动解阻：UnblockTask（blocked → queued）
```

### 5.2 事件驱动自动化

```
cron 规则 ──Engine.pollCron──▶ Fire ──▶ CreateTask
webhook  ──/api/webhook?target=──▶ FireKind ──▶ CreateTask
                                          └──▶ MarkRuleFired
```

### 5.3 WebSocket 推送

```
APP ──WS /api/ws?token=──▶ Server
  ← subscribed
  ← task.event {id,status}        severity: info/warning/critical
  ← task.event {id,status,upstream,reason}  依赖被阻塞/解阻，severity=warning/info
  ← upstream.health {healthy}     每 30s
```

## 6. 数据库方言处理

SQL 统一用 SQLite 风格 `?` 占位符书写，通过 `internal/store/rebind.go` 的 `rebind(driver, query)` 在 PG 下转换为 `$N`。`sqlStore.q()` 统一包装。少量时间运算方言（如 `datetime('now', '+N seconds')` vs `CURRENT_TIMESTAMP + interval`）在 `RetryTask` 内按 driver 分支。

> ⚠️ 新增 SQL 必须走 `s.q(...)`，否则 PG 会语法错误（历史教训：v1 曾因裸 `?` 导致 PG 完全不可用）。

## 7. 离线构建

本机无外网（proxy.golang.org 不可达），依赖全部来自本地 module cache：

```bash
GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go build ./...
GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off go test ./internal/...
```

新增依赖时需在 go.mod 手动 pin 缓存里已有的版本。

## 8. 部署形态

- **开发机本机**：`scripts/install.sh` → systemd 服务，`/var/lib/starburst-backend` 存数据。
- **容器**：`Dockerfile`（多阶段、非 root）+ `docker-compose.yml`（可选 PG profile）。
- **CI**：GitHub Actions `test` job + 打 tag 触发 `release` job 交叉编译 6 平台单二进制。

## 9. 客户端对接要点

- 加服务器时新增"通过 Backend 连接"选项：填后端地址 + Token。
- 订阅 WS 即可收到任务状态/健康心跳，无需轮询。
- 完整契约见 [docs/API.md](API.md)。
