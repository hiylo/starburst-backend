# OpenCode Backend

OpenCode 客户端的轻量独立后端（Go 单二进制）。运行在开发机上连接本机 OpenCode，为 APP 提供**编排、自动化与推送**能力。APP 可直连 OpenCode，也可先连本后端再访问 OpenCode。

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

### 可观测
- **实时推送**：WebSocket 长连 + 通知分级（info/warning/critical），任务状态、上游健康心跳
- **审计日志**：记录每个 Token 的 API 调用（谁、何时、做了什么）
- **用量统计**：任务状态聚合、按 Token 调用排行、归档数

### 运维
- **Web 配置页**：默认密码登录（可修改），Token 管理、任务列表、编排项目、系统状态
- **一键安装**：`scripts/install.sh`（systemd 托管）；Docker 与 docker-compose 提供
- **CI**：GitHub Actions 自动测试 + 6 平台交叉编译 release

## 构建

```bash
go build -o startburst-backend ./cmd/startburst-backend
```

## 运行

```bash
# SQLite（默认）
./startburst-backend --listen :18880 --default-admin-password admin

# PostgreSQL
./startburst-backend --listen :18880 --db postgres --pg-dsn "postgres://user:pass@host/db"
```

配置项支持命令行 flag 与环境变量（环境变量优先）：

| flag | 环境变量 | 默认 | 说明 |
|------|---------|------|------|
| `--listen` | `OCB_LISTEN` | `:18880` | HTTP 监听地址 |
| `--opencode-url` | `OCB_OPENCODE_URL` | `http://127.0.0.1:4096` | 本机 OpenCode 地址 |
| `--db` | `OCB_DB` | `sqlite` | `sqlite` 或 `postgres` |
| `--sqlite-path` | `OCB_SQLITE_PATH` | `startburst-backend.db` | SQLite 数据库文件 |
| `--pg-dsn` | `OCB_PG_DSN` | — | PostgreSQL 连接串 |
| `--default-admin-password` | `OCB_ADMIN_PASSWORD` | `admin` | 首次初始化密码（可后改） |
| `--default-token` | `OCB_DEFAULT_TOKEN` | — | 首次运行时预置的 API token（只落哈希，明文只在日志里打一次） |
| `--llm-url` | `OCB_LLM_URL` | — | OpenAI 兼容编排大模型地址（如 LiteLLM 网关），空 = 关闭智能编排 |
| `--llm-key` | `OCB_LLM_KEY` | — | `--llm-url` 的 API key |
| `--llm-model` | `OCB_LLM_MODEL` | — | 编排决策使用的模型名 |
| `--workers` | `OCB_WORKERS` | `4` | 并发执行的任务数（`1` = 串行） |
| `--task-retention` | `OCB_TASK_RETENTION` | — | 已完成任务保留时长（Go duration，如 `168h0m`），不设置 = 永久保留 |
| `--stt-url` | `OCB_STT_URL` | — | 流式语音识别引擎地址（如 `http://192.0.2.150:18090`），空 = 关闭 `/api/stt` |
| `--stt-timeout` | `OCB_STT_TIMEOUT` | `30s` | 单次引擎往返超时 |
| `--stt-max-chunk-bytes` | `OCB_STT_MAX_CHUNK_BYTES` | `2097152` | 单个音频分片上限（字节） |
| `--version` | — | — | 打印版本号退出 |
| `--health-check` | — | — | 检查数据库/上游连通性后退出 |

> 任务默认并发执行（`--workers 4`），适合批量下发；需要严格串行时设 `--workers 1`。显式指定同一个上游 `sessionId` 的多个任务会在后端自动排队，避免并发读回同一次会话的最后一条回复。

> 已完成（succeeded/failed/canceled）的任务默认永久保留。设 `--task-retention 168h0m` 后每小时清一次超期的；仍被依赖引用的前置任务不会被删（否则依赖方会指向不存在的任务），等依赖方自己被清掉后下一轮再删。

> 智能编排大模型也可在运行时不重启配置：Web 配置页调用 `GET/POST /api/llm` 修改地址/密钥/模型（见 [docs/API.md](docs/API.md)），后台配置持久化后**优先于**启动 flag/环境变量。

> 服务端语音识别：`--stt-url` 指向跑着流式识别引擎的主机（本地方案：NAS 上用 sherpa-onnx 的 zipformer 中英双语 int8 流式模型，`docker` 常驻，模型约 197MB、加载 8s、4 线程推理）。后端只做鉴权与协议透传，不自己解码音频；手机端在端侧 MNN 模型不可用时自动回退到它，端侧可用则继续本地识别。未配置时 `/api/stt` 返回 503，App 不会显示服务端语音入口。接口细节见 [docs/API.md](docs/API.md) 的「语音识别」一节。识别引擎容器化部署见 `scripts/stt-server/Dockerfile` 与 `docker-compose.yml` 的 `stt-server` 服务（`--profile stt`）。

## 测试

```bash
go test ./...            # SQLite 全量
go test -race ./...      # 并发回归（推送中枢等）

# PostgreSQL 方言回归（可选，默认跳过）
# 自建 ocb_test_<pid> 临时库，测试结束自动删除
OCB_PG_DSN='postgres://user:pass@host/db?sslmode=disable' \
    go test -tags pgtest -run Postgres ./internal/store/
```

## 一键安装

```bash
curl -fsSL https://<host>/install.sh | bash
```

或本地 `bash scripts/install.sh [--port 8080] [--db sqlite|postgres] [--pg-dsn "..."] [--admin-password "..."] [--workers 4]`。
安装为 systemd 服务，数据存 `/var/lib/startburst-backend`。

加 `--default-token <值>`（或 `OCB_DEFAULT_TOKEN`）可在首次启动时预置一个固定 API token，客户端直接拿它调 API/WS/SSE，不用再登网页建 token。该值只在首次运行生效一次（仅保存哈希，日志里打印一次明文），之后改配置不重建；在配置页删除它则彻底取消。

只验证脚本、不触碰真实系统时加 `--prefix <目录>`（沙箱模式）：二进制、配置、unit 全部写到该目录下并跳过 systemctl，配合 `OCB_BIN_URL=file:///本地二进制` 可跳过下载：

```bash
OCB_BIN_URL="file://$(pwd)/startburst-backend" bash scripts/install.sh --prefix /tmp/sandbox --db postgres --pg-dsn "$OCB_PG_DSN"
```

## Docker

```bash
docker compose up -d              # 仅后端（SQLite）
docker compose --profile postgres up -d   # 附带 PG，可用 OCB_PG_DSN 切换
```

## API

见 [docs/API.md](docs/API.md)。配置页为 `/`（`index.html`）。

## 文档

- **[docs/QUICKSTART.md](docs/QUICKSTART.md)** — 5 分钟快速上手（启动→配置→任务→推送）
- **[docs/API.md](docs/API.md)** — 完整 API 契约（供客户端并行开发）
- **[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** — 架构设计、数据模型、关键流程

## 目录结构

```
cmd/startburst-backend  入口（--version / --health-check / serve）
internal/
  config     flag/env 配置
  store      SQLite/Postgres 存储抽象 + 迁移（v1-v10）
  auth       Web 密码 + APP Token
  server     HTTP/WS 路由与 handler
  opencode   本机 OpenCode 客户端（会话/消息/导出）
  llm        可插拔编排大模型客户端（OpenAI 兼容，未配置时禁用）
  tasks      异步任务调度器（重试/退避 + LLM 摘要/自愈）
  automation cron/webhook 自动化规则引擎
  push       WS 推送 Hub（severity 分级）
  webui      嵌入的配置页
scripts/install.sh  一键安装
docs/API.md         API 契约
.github/workflows   CI / release 交叉编译
Dockerfile / docker-compose.yml / .dockerignore
```
