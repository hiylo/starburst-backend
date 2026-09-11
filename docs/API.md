# OpenCode Backend API 契约

> 供客户端（Android / Web）与后端并行开发的接口规范。Base URL 形如 `http://<host>:8080`。

## 鉴权模型

后端有两套凭据，**互不通用**：

| 凭据 | 用途 | 传递方式 |
|------|------|---------|
| **Web Session** | 浏览器/配置页登录管理 | Header `X-Web-Session: <sid>` |
| **APP Token** | Android/Web 客户端调编排 API | Header `Authorization: Bearer <token>` 或 WS 的 `?token=` |

- 首次启动生成默认管理密码（`--default-admin-password`，默认 `admin`），登录后可修改。
- Token 由 Web 配置页生成，带 `ocb_` 前缀，**仅显示一次**，按设备命名、可撤销。
- 错误统一返回 `{"error":"描述"}`，非 2xx 状态码。

## 公共端点

### GET /api/health（免鉴权）
```json
{ "status": "ok", "upstream": true, "upstreamError": "", "time": "..." }
```
- `upstream`：后端能否连到本机 OpenCode。

### GET /api/system（需 Token 或 X-Web-Session）
返回内部地址，因此需要鉴权（APP Token 或 Web Session 二选一）：
```json
{ "backend": "opencode-backend", "opencodeURL": "http://127.0.0.1:4096", "opencodeVersion": "v1.2.3", "db": "sqlite" }
```

### GET /（免鉴权）
无头模式提示页；配置页资产嵌入时返回 index.html。

## Web 配置（Web Session）

### POST /api/web/session（免鉴权）
请求：`{"password":"admin"}`
- 200 → `{"session":"<sid>"}`（存 localStorage，24h 有效）
- 401 → `{"error":"invalid password"}`
- 429 → `{"error":"too many login attempts, try again later"}`：同一 IP 5 分钟内失败 5 次后限流，登录成功即重置

### DELETE /api/web/session（需 X-Web-Session）
退出登录。→ `{"ok":true}`

### POST /api/web/password（需 X-Web-Session）
请求：`{"newPassword":"新密码"}`（≥4 位）
- 200 → `{"ok":true}`；400 → 密码太短

### GET /api/tokens（需 X-Web-Session）
```json
[{ "id":"...", "name":"我的手机", "tokenHash":"...", "createdAt":"...", "revokedAt":null, "lastUsed":"..." }]
```

### POST /api/tokens（需 X-Web-Session）
请求：`{"name":"设备名"}`
- 201 → `{"token":"ocb_..."}`（仅显示一次）

### DELETE /api/tokens/{id}（需 X-Web-Session）
撤销。→ `{"ok":true}`；404 → 不存在

## 编排 API（APP Token）

### GET /api/projects（需 Token）
本机 OpenCode 会话按**工作目录**分组的列表：
```json
{ "projects": [ { "id":"/path/to/repo", "directory":"/path/to/repo", "sessionCount":3 } ] }
```

### GET /api/projects/{dir}（需 Token）
返回工作目录等于 `{dir}` 的会话明细（真实过滤，不是重复全量列表）。`{dir}` 是路径段，含 `/` 的目录需 URL 编码（如 `/w` → `%2Fw`）：
```json
{ "sessions": [ {
  "id":"ses_...", "slug":"...", "title":"...", "directory":"/w",
  "agent":"build", "model":"m1", "busy":true,
  "inputTokens":100, "outputTokens":50, "reasoningTokens":0, "cost":0,
  "createdMs":1700000000000, "updatedMs":1700000100000
} ] }
```
- 无匹配 → `{"sessions":[]}`；缺 `{dir}` → 400

### GET /api/tasks?status=queued（需 Token）
任务列表（最新在前，最多 50 条），`status` 可选过滤：
```json
{ "tasks": [ {
  "id":"task_...", "sessionId":"ses_...", "directory":"/path",
  "prompt":"...", "dependsOn":"task_...", "status":"queued|running|succeeded|failed|canceled|pending|blocked",
  "error":"", "result":"", "progress":"", "aiSummary":"", "attempts":0,
  "createdAt":"...", "updatedAt":"...", "startedAt":null, "finishedAt":null
} ] }
```

### POST /api/tasks（需 Token）
请求：
```json
{ "prompt":"给所有 controller 加日志", "sessionId":"ses_...(可选)", "directory":"/path(可选,新会话用)", "dependsOn":"task_...(可选,前置任务)" }
```
- 201 → 完整 Task 对象（含新生成的 id 与初始 `status`）
- 依赖语义：
  - `dependsOn` 指向 queued/running/pending 任务 → 本任务以 `pending` 入队，前置成功后自动转为 `queued`
  - `dependsOn` 指向已 `succeeded` 的任务 → 本任务直接 `queued` 执行
  - `dependsOn` 不存在或指向 failed/canceled/blocked → 400（快速失败，避免留下永远 pending 的任务）
- 400 → `prompt` 为空、`dependsOn` 任务不存在、或前置任务已终态失败

### GET /api/tasks/{id}（需 Token）
单个任务详情。

### DELETE /api/tasks/{id}（需 Token）
取消任务（queued/running/pending）。
- 200 → `{"ok":true}`；若该任务有 pending 下游，返回 `{"ok":true,"blocked":<n>}` 并把下游标记为 `blocked`
- 409 → 已结束（succeeded/failed/canceled/blocked）

### POST /api/tasks/{id}（需 Token）
手动解阻：把 `blocked` 任务重新排队执行（人工已在前置任务之外处理好问题，
无需重跑前置）。
- 200 → `{"ok":true}`，并通过 `/api/ws` 推送 `task.event`（`status=queued`，`reason="manual unblock"`）
- 409 → 任务不是 `blocked`（含不存在的 id）

### POST /api/batch（需 Token）
一条指令对多个 target 批量建任务：
```json
{ "prompt":"给所有模块加日志", "targets":[ {"directory":"/a"}, {"sessionId":"ses_..."} ] }
```
- 201 → `{"created":["task_..."],"count":2}`

### POST /api/archives（需 Token）
把远端会话归档到后端存储。请求：`{"sessionId":"...", "format":"markdown"|"json"}`（format 默认 markdown）
- 201 → `{"id":"arch_...","size":123,"format":"markdown"}`

### GET /api/archives（需 Token）
归档元数据列表（不含内容），`?limit=`。→ `{"archives":[...]}`

### GET /api/archives/{id}（需 Token）
完整归档（含 `content`）。

### DELETE /api/archives/{id}（需 Token）
删除归档。→ `{"ok":true}`；404 → 不存在

## 自动化规则（Web Session）

### GET /api/rules（需 X-Web-Session）
规则列表。→ `{"rules":[...]}`

### POST /api/rules（需 X-Web-Session）
```json
{ "name":"每晚测试", "kind":"cron|git|http", "schedule":"5m 或 cron 或 target", "directory":"/path", "prompt":"指令", "enabled":true }
```
- 201 → 完整 Rule 对象（含新生成的 id）

### POST /api/rules/generate（需 X-Web-Session）
自然语言自动生成一条规则草稿（不落库，客户端确认后再走 `POST /api/rules`）。请求：
```json
{ "description":"每天早上 8 点运行测试" }
```
- 200 → 草稿（字段与 Rule 一致，`enabled` 固定为 true）：
```json
{ "draft": { "name":"...", "kind":"cron|git|http", "schedule":"...", "directory":"", "prompt":"...", "enabled":true } }
```
- 400 → `description` 为空；502 → 模型生成失败（含无效 kind/空 prompt）；503 → 未配置编排大模型

### DELETE /api/rules/{id}（需 X-Web-Session）
删除规则。→ `{"ok":true}`；404 → 不存在

### GET /api/rules/{id}/executions（需 X-Web-Session）
规则执行历史（触发时间 + 产生的任务）：
```json
{ "executions": [ { "id":1, "ruleId":"rule_...", "taskId":"task_...", "triggeredAt":"..." } ],
  "total": 3 }
```

### POST /api/webhook?target=xxx
触发匹配的 http 规则（无需鉴权，由调用方如 git webhook 使用）。→ `{"fired":true}`；404 → 无匹配规则
- 若配置了 `--webhook-secret`，需带 `X-Webhook-Secret: <secret>`（或 `?secret=`），否则 401

## 审计与统计（Web Session）

### GET /api/audit?tokenId=&limit=（需 X-Web-Session）
最近审计记录（token 认证的 API 调用）。→ `{"audit":[...]}`

### GET /api/stats（需 X-Web-Session）
用量统计：
```json
{ "tasks":{"queued":0,"running":0,"succeeded":5,"failed":1,"canceled":0,"pending":0,"blocked":0,"retried":1,"total":7},
  "tokenUsage":[{"tokenId":"...","tokenName":"我的手机","calls":12}],
  "archives":3 }
```

## 推送通道（WebSocket）

### GET /api/ws?token=xxx（需 Token，query 参数）
升级 WebSocket 长连，后端向客户端实时推送事件。连接建立后先收到 `subscribed`。

事件帧（JSON）：
```json
{ "type":"task.event", "payload":{"id":"task_...","status":"queued|running|succeeded|failed|canceled|blocked"}, "severity":"info" }
{ "type":"task.event", "payload":{"id":"task_...","status":"blocked","upstream":"task_...","reason":"前置任务失败"}, "severity":"warning" }
{ "type":"upstream.health", "payload":{"healthy":true,"time":"..."} }
```

`upstream`/`reason` 仅在依赖任务被阻塞或重新排队时出现，人工介入靠这两字段定位原因。

| type | payload | 说明 | severity |
|------|---------|------|----------|
| `subscribed` | — | 订阅成功 | info |
| `task.event` | `{id,status,upstream?,reason?}` | 任务状态变更（含依赖被阻塞/解阻） | queued/running/succeeded/canceled=info, retrying/blocked=warning, failed=critical |
| `upstream.health` | `{healthy,time}` | 本机 OpenCode 可达性心跳（30s） | info |

## 流式对话（SSE 中继）

### GET /api/stream（需 Token，Authorization: Bearer）
把上游 OpenCode 的**全局 SSE 事件流**（`/global/event`）原样中继给客户端。APP 通过它维持单一稳定连接即可实时收到对话流式输出，无需直连 OpenCode。

响应 `Content-Type: text/event-stream`，先发握手再逐事件转发：
```
event: connected
data: {}

data: {"directory":"/workspaces/opencode","payload":{"id":"evt_...","type":"message.part.delta","properties":{...}}}
data: {"payload":{"type":"message.part.updated","properties":{...}}}
```

- 上游断连时后端**自动重连**（指数退避，客户端无需感知）
- 事件逐条 `data:` 原样透传，字段结构与直连一致
- 主要事件类型：`server.connected`、`message.part.delta`、`message.part.updated`、`session.*`、`turn.completed`

## 错误码汇总

| 状态码 | 含义 |
|--------|------|
| 400 | 参数错误 / 密码太短 / dependsOn 无效 |
| 401 | 未授权（Web Session 或 Token 无效/已撤销） |
| 404 | 资源不存在 |
| 409 | 任务已结束无法取消 |
| 429 | 登录尝试过多（同 IP 5 分钟内失败 5 次） |
| 500 | 服务内部错误 |

## 状态码表（任务）

`pending` → `queued` → `running` → `succeeded` / `failed`；
`queued`/`running`/`pending` → `canceled`；
`pending` → `blocked`（前置任务 failed/canceled）；
`blocked` → `queued`（两条路径：前置任务重试后成功自动解阻，或 `POST /api/tasks/{id}` 手动解阻）；
`retrying` = 失败后排队重试（带退避）。

| 状态 | 含义 | 终态 |
|------|------|------|
| `pending` | 等待 `dependsOn` 前置任务成功 | 否 |
| `queued` | 已入队等待执行 | 否 |
| `running` | 正在执行 | 否 |
| `succeeded` | 执行成功 | 是 |
| `failed` | 重试耗尽后失败 | 是 |
| `canceled` | 被用户取消 | 是 |
| `blocked` | 前置任务未成功，暂不可执行（`error` 记录原因）；前置重试成功后自动解阻，也可手动解阻 | 否 |

依赖任务的每一次状态变更（被阻塞 / 重新排队）都会通过 `/api/ws` 推送
`task.event`，payload 为 `{id, status, upstream, reason}`；被阻塞为 `severity=warning`，
其余为 `info`。人工介入依赖这条通知。
