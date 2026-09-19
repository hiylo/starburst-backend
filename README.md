# StarBurst Backend

StarBurst（App）的轻量独立后端（Go 单二进制）。运行在开发机上连接本机 OpenCode，为 StarBurst App 提供**编排、自动化、推送与测试智能**能力。App 可直连 OpenCode，也可先连本后端再访问 OpenCode。

> 当前 HTTP 面共注册 98 条路由，其中 `/api/intel/*` 占 56 条：测试智能（仓库理解、契约提取、用例执行与归因）已与编排、推送并列成为第三块主能力，设计见 [docs/TEST_INTELLIGENCE.md](docs/TEST_INTELLIGENCE.md)。

## 特性

### 核心
- **单二进制无头服务**：一个进程，`--listen` 指定端口；支持 `--version`、`--health-check`
- **双数据库**：SQLite 默认（纯 Go 驱动，WAL 模式），PostgreSQL 可选（同一版本化迁移）
- **双凭据**：Web Session 管配置页，APP Token 管编排 API，互不通用；Token 多设备可撤销

### 编排
- **会话编排**：`/api/projects` 聚合本机 OpenCode 所有会话
- **流式对话中继**：`/api/stream` 把 OpenCode 全局 SSE 事件流原样转发（自动重连），APP 一条稳定连接即可流畅收流式输出
- **异步任务队列**：提交即返回，后台调度 agent 执行；进度/结果/重试（指数退避）、可取消
- **任务依赖**：`dependsOn` 声明前置任务，前置成功才执行（`pending`→`queued`）；前置失败/取消则标记 `blocked` 并记录原因，可在前置重试成功后自动解阻，也可手动解阻后继续执行
- **批量执行**：`/api/batch` 一条指令对多个目录/会话批量下发
- **事件驱动自动化**：cron 定时 + HTTP webhook 触发规则，命中即自动建任务
- **会话归档**：`/api/archives` 把远端会话导出为 Markdown/JSON 存档
- **智能编排决策层**：可插拔大模型（OpenAI 兼容，走 LiteLLM 网关）驱动三类能力——自然语言转规则草稿、任务结果摘要、失败自愈；不配置时自动退回纯规则引擎

### 测试智能
- **项目与仓库理解**：登记本机目录或 Git 仓库，自动识别技术栈（Java/Go/Android/iOS/Web）与子模块，首次添加即后台跑全量分析；本地项目源码变更时自动重扫（同项目串行、2 分钟防抖）
- **契约与数据映射**：静态提取接口契约（方法/路径/响应字段）与实体↔表↔字段映射，附带网关暴露路径；也支持粘贴响应体做单接口契约核对，或对着一个 `baseUrl` 一键批量核对全项目接口
- **功能点与客户端必显字段**：把接口聚成候选功能点（可人工改名、排序、增删端），并从 Android DataBinding 布局、Vue 模板、SwiftUI 视图提取「页面 → 字段」绑定；字段返回 null 即客户端必坏，用于验收清单
- **测试资产与执行**：发现既有用例（JUnit / go test / Vitest / Jest / Playwright / XCTest / pytest），按计划串行执行「先构建后测试」或一键全模块回归；同项目一次只跑一个 run、全局另有并发上限，进度通过 WS 推送；失败用例做确定性归因（后端/数据库/客户端/环境/契约）
- **审计与修复**：依赖清单（Maven/Go/npm/Gradle）归一化并可导出 CycloneDX 1.5 SBOM；源码合规规则扫描、敏感字段安全告警、可配置 AI 规则扫描；结论落成 finding，可豁免/标误报，AI 修复建议以 diff 形式审阅后一键应用或回滚
- **人工覆写层**：自动分析出的字段可被人工或大模型校正，进「待确认队列」由人拍板；确认后按自然键持久化，重扫只重建自动值，人工值永远赢
- **环境与设备**：从构建/配置文件推导测试环境依赖（中间件与工具链），探测本机、一键起可丢弃的 Docker 容器、登记外部实例、跑库初始化脚本；adb 设备（含无线）登记并绑定到项目，测试命令也可下发到 SSH 远程节点执行
- **知识库问答**：契约与文档切块向量化（pgvector），支持多轮自然语言问答；完整能力需要 PostgreSQL + 向量模型，SQLite 轻量化部署下会自动隐藏该入口

### 可观测
- **实时推送**：WebSocket 长连 + 通知分级（info/warning/critical），任务状态、上游健康心跳、测试 run 进度、硬件阈值告警
- **OpenCode 镜像代理**：`/api/opencode/<path>` 原样转发本机 OpenCode 的任意接口（方法不变，SSE 逐块 flush 透传），App 只换 Base URL 即可沿用原有路径；写 provider key、开 PTY、全局 dispose 等高危上游操作只允许 Web Session 调用（模型下拉框读的 `GET /config/providers` 是唯一放行给 APP token 的例外）。唯一改写回程的是 `/config`、`/provider` 一族的响应——上游回传的明文 API Key 会在代理层按字段名置空后下发，剥不动的整体失败关闭（502）而绝不透传
- **会话事件看板**：后端常驻订阅上游全局事件流并落库，`/api/events` 供 App/Web 拉取最近会话动态，`/api/unread` 做跨端未读标记（任一端读过即全员清除）
- **阈值告警**：`internal/alerts` 周期采样本机 CPU/内存/磁盘，越线即推 `alert.hardware`；默认关闭，阈值经 `GET/POST /api/alerts` 读写（配置页暂无该表单）
- **审计日志**：记录每个 Token 的 API 调用（谁、何时、做了什么），异步批量落库、保留 30 天
- **用量统计**：任务状态聚合、按 Token 调用排行、归档数

### 运维
- **Web 配置页**：默认密码登录（改密码需验旧密码，成功后其它设备的登录会话全部失效），左栏 11 个入口分三组——控制台（AI 工作台 / 任务 / 编排 / 实时流）、开发（项目与会话 / 自动化规则 / 会话归档）、系统（智能测试 / 审计日志 / Token 管理 / 设置）；顶栏可切换外观主题（跟随系统 / 浅色 / 深色 / 纯黑 / 柔和，取值与安卓端一致）
- **AI 工作台**：Web 端与安卓端共用同一套上游接口，覆盖会话列表与新建、流式对话（推理 / 工具调用 / 代码 diff / 文件 part 逐块渲染）、权限授权与待答问题的远程批复、模型选择与 @ 提及、附件与斜杠命令、停止与重发
- **测试智能工作台**：项目详情含模块 / 接口契约 / 实体字段 / 用例 / 功能点 / 执行记录 / 问题 / 审计结论 / 修复建议 / 依赖与环境概览 / 增量影响 / 待确认覆写 / 客户端绑定各页签
- **一键安装**：`scripts/install.sh`（systemd 托管）；Docker 与 docker-compose 提供
- **CI**：GitHub Actions 自动测试 + 6 平台交叉编译 release

## 构建

```bash
go build -o starburst-backend ./cmd/starburst-backend
```

## 运行

```bash
# SQLite（默认）——口令走环境变量：命令行参数会出现在 ps / shell history 里
STARBURST_ADMIN_PASSWORD='<强口令>' ./starburst-backend --listen :18880

# PostgreSQL（DSN 内不嵌口令，用 .pgpass 或 PGPASSWORD 提供）
STARBURST_ADMIN_PASSWORD='<强口令>' ./starburst-backend --listen :18880 \
    --db postgres --pg-dsn "postgres://starburst@192.0.2.10:5432/starburst?sslmode=require"
```

配置项支持命令行 flag 与环境变量，**环境变量只是该项的默认值，显式给出的 flag 覆盖环境变量**（只想用环境变量时，别把 flag 一起写在命令行上）：

| flag | 环境变量 | 默认 | 说明 |
|------|---------|------|------|
| `--listen` | `STARBURST_LISTEN` | `:18880` | HTTP 监听地址 |
| `--opencode-url` | `STARBURST_OPENCODE_URL` | `http://127.0.0.1:4096` | 本机 OpenCode 地址 |
| `--db` | `STARBURST_DB` | `sqlite` | `sqlite` 或 `postgres` |
| `--sqlite-path` | `STARBURST_SQLITE_PATH` | `starburst-backend.db` | SQLite 数据库文件 |
| `--pg-dsn` | `STARBURST_PG_DSN` | — | PostgreSQL 连接串 |
| `--default-admin-password` | `STARBURST_ADMIN_PASSWORD` | `admin` | 首次初始化密码（可后改） |
| `--default-token` | `STARBURST_DEFAULT_TOKEN` | — | 首次运行时预置的 API token（只落哈希，日志里只打掩码前缀，明文以你配置的这个值为准） |
| `--webhook-secret` | `STARBURST_WEBHOOK_SECRET` | — | `POST /api/webhook` 的共享密钥，走 `X-Webhook-Secret` 头；**不设置则 webhook 一律 403** |
| `--llm-url` | `STARBURST_LLM_URL` | — | OpenAI 兼容编排大模型地址（如 LiteLLM 网关），空 = 关闭智能编排 |
| `--llm-key` | `STARBURST_LLM_KEY` | — | `--llm-url` 的 API key |
| `--llm-model` | `STARBURST_LLM_MODEL` | — | 编排决策使用的模型名 |
| `--embed-url` | `STARBURST_EMBED_URL` | — | OpenAI 兼容向量模型地址（`/v1/embeddings`），空 = 关闭知识库向量检索 |
| `--embed-key` | `STARBURST_EMBED_KEY` | — | `--embed-url` 的 API key |
| `--embed-model` | `STARBURST_EMBED_MODEL` | — | 向量模型名（如 `bge-m3`，向量维度固定 1024） |
| `--workers` | `STARBURST_WORKERS` | `4` | 并发执行的任务数（`1` = 串行） |
| `--max-concurrency` | `STARBURST_MAX_CONCURRENCY` | `0` | 全局运行中任务上限（`0` = 仅受 `--workers` 限制） |
| `--task-retention` | `STARBURST_TASK_RETENTION` | — | 已完成任务保留时长（Go duration，如 `168h0m`），不设置 = 永久保留 |
| `--stt-url` | `STARBURST_STT_URL` | — | 流式语音识别引擎地址（如 `http://192.0.2.150:18090`），空 = 关闭 `/api/stt` |
| `--stt-timeout` | `STARBURST_STT_TIMEOUT` | `30s` | 单次引擎往返超时 |
| `--stt-max-chunk-bytes` | `STARBURST_STT_MAX_CHUNK_BYTES` | `2097152` | 单个音频分片上限（字节） |
| `--version` | — | — | 打印版本号退出 |
| `--health-check` | — | — | 检查数据库/上游连通性后退出 |

> 任务默认并发执行（`--workers 4`），适合批量下发；需要严格串行时设 `--workers 1`。显式指定同一个上游 `sessionId` 的多个任务会在后端自动排队，避免并发读回同一次会话的最后一条回复。

> 已完成（succeeded/failed/canceled）的任务默认永久保留。设 `--task-retention 168h0m` 后每小时清一次超期的；仍被依赖引用的前置任务不会被删（否则依赖方会指向不存在的任务），等依赖方自己被清掉后下一轮再删。

> 智能编排大模型也可在运行时不重启配置：Web 配置页调用 `GET/POST /api/llm` 修改地址/密钥/模型（见 [docs/API.md](docs/API.md)），后台配置持久化后**优先于**启动 flag/环境变量。向量模型同理走 `GET/POST /api/embed`，两套配置彼此独立；两者的密钥都不会被读回，`GET` 只告知是否已设置。知识库相关能力要求「向量模型已配置 + PostgreSQL 装好 pgvector 扩展」同时满足，SQLite 轻量化部署下 `/api/system` 的 `vectorCapable` 为 `false`，配置页隐藏整个测试智能入口，`POST /api/intel/index` 与 `/api/intel/ask` 直接返回 503（不静默建立一份无人可读的索引）。

> 服务端语音识别：`--stt-url` 指向跑着流式识别引擎的主机（本地方案：NAS 上用 sherpa-onnx 的 zipformer 中英双语 int8 流式模型，`docker` 常驻，模型约 197MB、加载 8s、4 线程推理）。后端只做鉴权与协议透传，不自己解码音频；手机端在端侧 MNN 模型不可用时自动回退到它，端侧可用则继续本地识别。未配置时 `/api/stt` 返回 503，App 不会显示服务端语音入口。接口细节见 [docs/API.md](docs/API.md) 的「语音识别」一节。识别引擎容器化部署见 `scripts/stt-server/Dockerfile` 与 `docker-compose.yml` 的 `stt-server` 服务（`--profile stt`）。

## 测试

```bash
go test ./...            # SQLite 全量
go test -race ./...      # 并发回归（推送中枢等）

# PostgreSQL 方言回归（可选，默认跳过）
# 自建 ocb_test_<pid> 临时库，测试结束自动删除
STARBURST_PG_DSN='postgres://starburst@192.0.2.10:5432/ocb_test?sslmode=disable' \
    go test -tags pgtest -run Postgres ./internal/store/
```

## 一键安装

```bash
curl -fsSL https://<host>/install.sh | bash
```

或本地 `bash scripts/install.sh [--port 8080] [--db sqlite|postgres] [--pg-dsn "..."] [--admin-password "..."] [--workers 4]`。
安装为 systemd 服务，数据存 `/var/lib/starburst-backend`。

加 `--default-token <值>`（或 `STARBURST_DEFAULT_TOKEN`）可在首次启动时预置一个固定 API token，客户端直接拿它调 API/WS/SSE，不用再登网页建 token。该值只在首次运行生效一次（库里只存哈希，日志只打掩码前缀，明文就是你配置的这一个）；之后改配置不重建，在配置页删除它则彻底取消。

只验证脚本、不触碰真实系统时加 `--prefix <目录>`（沙箱模式）：二进制、配置、unit 全部写到该目录下并跳过 systemctl，配合 `STARBURST_BIN_URL=file:///本地二进制` 可跳过下载：

```bash
STARBURST_BIN_URL="file://$(pwd)/starburst-backend" bash scripts/install.sh --prefix /tmp/sandbox --db postgres --pg-dsn "$STARBURST_PG_DSN"
```

## Docker

```bash
docker compose up -d              # 仅后端（SQLite）
docker compose --profile postgres up -d   # 附带 PG，可用 STARBURST_PG_DSN 切换
```

## API

见 [docs/API.md](docs/API.md)。配置页为 `/`（`index.html`）。

## 文档

- **[docs/QUICKSTART.md](docs/QUICKSTART.md)** — 5 分钟快速上手（启动→配置→任务→推送）
- **[docs/API.md](docs/API.md)** — 完整 API 契约（供客户端并行开发）
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — 架构设计、数据模型、关键流程
- **[docs/TEST_INTELLIGENCE.md](docs/TEST_INTELLIGENCE.md)** — 测试智能子系统的设计文档（子包划分、扫描与归因规则、门禁与人工覆写模型）

## 目录结构

```
cmd/starburst-backend  入口（flag/env → store → auth → 各后台 goroutine → HTTP 服务）
internal/
  config     flag/env 配置、--version / --health-check
  store      SQLite/Postgres 存储抽象 + 版本化迁移（47 步，v1 initial … v47 intel_analysis_status）
  auth       Web 密码（bcrypt）+ APP Token（哈希、多设备、可撤销）
  server     HTTP/WS 路由与 handler（intel_*.go 承载测试智能端点）
  intel      测试智能子系统（19 个子包）：仓库理解与契约提取（java/go/scan）、
             功能点（feature）、测试资产发现（testassets）、报告解析（report）、
             失败归因（rootcause）、Git 增量影响（delta）、依赖与 SBOM（deps/sbom）、
             合规与安全（compliance/security）、契约校验（contract）、网关路由（gateway）、
             客户端绑定（android/ios/web）、环境与修复（envdetect/envagent/schemainit/fix）、
             文档增强（enrich）
  opencode   本机 OpenCode 客户端（健康、会话、消息、导出、镜像代理上游调用）
  llm        可插拔编排大模型客户端（OpenAI 兼容 chat completions，未配置时禁用）
  embed      可插拔向量模型客户端（OpenAI 兼容 embeddings，供知识库检索，未配置时禁用）
  tasks      异步任务执行器（worker 池、claim/重试退避 + LLM 摘要/自愈、保留期清理）
  automation cron（秒级 6 字段）/ webhook 规则引擎
  alerts     本机 CPU/内存/磁盘阈值采样 → alert.hardware 推送
  push       WS 推送 Hub（severity 分级、单连接写超时）
  webui      go:embed 的配置页静态资源
scripts/install.sh        一键安装（systemd 托管，支持 --prefix 沙箱模式）
scripts/check-secrets.sh  提交/推送前的敏感信息自查（githooks 与 CI 复用）
scripts/stt-server/       服务端语音识别引擎镜像（docker-compose --profile stt）
githooks/pre-commit       本地提交钩子（调 scripts/check-secrets.sh）
docs/                     API.md / ARCHITECTURE.md / QUICKSTART.md / TEST_INTELLIGENCE.md
.github/workflows         CI（测试 + secret 扫描）/ release 交叉编译
Dockerfile / docker-compose.yml / .dockerignore
```
