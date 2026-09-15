# StarBurst Backend 架构文档

## 1. 定位与边界

StarBurst Backend 是运行在**开发机本机**的轻量编排层，位于 OpenCode 服务与客户端（APP / Web）之间。它**不是透传代理**——核心价值是对本机 OpenCode 做**编排**（调度任务、聚合会话、事件触发、归档）并为客户端提供**推送**。

```
┌──────────┐  直连(可选)   ┌──────────────────┐
│  APP/Web │ ────────────▶ │   OpenCode 服务   │
│          │               │  (127.0.0.1:4096) │
└────┬─────┘               └────────▲─────────┘
     │ Token + WS 推送              │  HTTP 编排调用
     ▼                              │
┌────────────────────────────────────┴───────────┐
│              starburst-backend（单二进制）        │
│  HTTP API + WebSocket 推送 + 任务调度 + 自动化   │
│  SQLite / PostgreSQL                            │
└─────────────────────────────────────────────────┘
```

## 2. 凭据模型

两套独立凭据，互不通用：

- **Web Session**（`X-Web-Session` header）：配置页管理登录，24h 有效期，内存态存储。
- **APP Token**（`Authorization: Bearer ocb_...`）：客户端调编排 API；DB 只存 sha256 哈希，多设备各一个、可撤销。

Web 端（`/api/web/*`、`/api/rules`、`/api/audit`、`/api/stats`、`/api/tokens`）要求 Web Session；编排端（`/api/projects`、`/api/tasks`、`/api/batch`、`/api/archives`、`/api/ws`）要求 APP Token。

## 3. 模块划分

| 包 | 职责 |
|----|------|
| `internal/config` | flag + 环境变量解析，`--version`/`--health-check` 动作 |
| `internal/store` | `database/sql` 抽象层：SQLite(modernc, WAL) / PostgreSQL(pgx)；版本化迁移 |
| `internal/auth` | bcrypt 管理密码 + `ocb_` Token（哈希存储、多设备、可撤销） |
| `internal/opencode` | OpenCode 客户端：健康、会话、消息拉取、Markdown 导出 |
| `internal/tasks` | 任务调度器：claim→执行→重试（指数退避）；severity 推送 |
| `internal/automation` | 规则引擎：cron（秒级解析）+ webhook 触发 |
| `internal/push` | WS Hub：连接注册/广播，severity 分级 |
| `internal/server` | HTTP/WS 路由与 handler；审计中间件 |
| `internal/webui` | `go:embed` 配置页（登录/Token/任务/项目/统计） |

## 4. 数据模型（迁移 v1-v6）

版本跟踪表 `schema_migrations(version, applied_at)`，两端迁移共用同一序列。

| 迁移 | 表 | 说明 |
|------|----|------|
| v1 | `settings`, `tokens` | KV 配置；Token 哈希表 |
| v2 | `tasks` | 异步任务（含 attempts/状态机） |
| v3 | `tasks.available_at` | 重试退避门控 |
| v4 | `rules` | 自动化规则（cron/git/http） |
| v5 | `audit_log` | API 审计 |
| v6 | `archives` | 会话归档 |

### 任务状态机

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
- 重试通过 `RetryTask(id, backoffSecs)` 把任务置回 `queued` 并排 `available_at` 到未来；SQLite 与 PG 用不同时间运算方言（`rebind` 外的 driver 分支）。

### 自动化规则

`rules(kind, schedule, prompt)`：`cron` 由引擎按轮询周期评估；`http` 由 `/api/webhook?target=` 触发匹配。触发后经 `Fire` 创建任务并标记 `last_fired_at`。

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
