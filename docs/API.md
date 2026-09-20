# StarBurst Backend API 契约

> 供客户端（Android / Web）与后端并行开发的接口规范。Base URL 形如 `http://<host>:18880`。
> 路由全部由标准库 `net/http.ServeMux` 注册（`internal/server/server.go` 的 `Routes`，共 98 条注册项），没有路由框架也没有中间件链，鉴权在每个 handler 内部完成。

## 鉴权模型

后端有两套凭据，**互不通用**：

| 凭据 | 用途 | 传递方式 |
|------|------|---------|
| **Web Session** | 浏览器/配置页登录管理 | Header `X-Web-Session: <sid>` |
| **APP Token** | Android/Web 客户端调编排 API | Header `Authorization: Bearer <token>` |

- 首次启动生成默认管理密码（`--default-admin-password`，默认 `admin`），登录后可修改。
- Session id 为 16 字节随机十六进制，落库持久化（重启后旧 sid 仍有效），24h 有效。
- Token 由 Web 配置页生成，带 `ocb_` 前缀，**仅显示一次**，按设备命名、可撤销；库中只存 sha256，接口永不回显。
- 错误统一返回 `{"error":"描述"}`，非 2xx 状态码。

### 鉴权类别速查

全文每个端点标注的「鉴权」取下面 5 类之一（与 `docs/ARCHITECTURE.md` 措辞一致）：

| 类别 | 代码判定 | 失败返回 | 覆盖端点 |
|------|---------|---------|---------|
| **免鉴权** | handler 不做校验 | — | `GET /api/health`、`GET /`（提示页/配置页）、`POST /api/web/session`（登录本身）、`POST /api/webhook`（另走共享密钥） |
| **共享密钥** | `X-Webhook-Secret` 与 `--webhook-secret` 定时比对 | 未配置密钥 → 403；缺失/不匹配 → 401 | `POST /api/webhook` |
| **仅 APP Token** | `requireToken`：只认 `Authorization: Bearer` | 401 | `POST /api/batch`、`POST /api/llm/generate`、`POST /api/llm/complete`、`POST /api/tasks/generate`、整棵 `/api/stt/**` |
| **仅 Web Session** | `requireWeb`：`X-Web-Session` 查库 | 401 | `DELETE /api/web/session`、`POST /api/web/password`、`GET/POST /api/llm`、`GET/POST /api/embed` |
| **双通道** | `if !requireWeb(r) { requireToken(r) 不通过才 401 }` | 401 | 其余全部，含整棵 `/api/intel/**`、`/api/tasks/**`、`/api/projects/**`、`/api/archives/**`、`/api/rules*`、`/api/audit`、`/api/stats`、`/api/tokens*`、`/api/opencode/**` |

两处需要特别注意的例外：

- **`?token=` 只在 WS/SSE 两条上被接受**：`requireToken` 只读 `Authorization` 头；`tokenFromRequest` 在头缺失时回落 `?token=<token>`，但**仅限** `/api/ws` 与 `/api/stream` —— 浏览器在 WebSocket / EventSource 握手里确实设不了自定义 header。其它任何路径（含 `GET /api/system`）带上 `?token=` 也照样 401：凭据进 URL 就会落进访问日志、代理与浏览器历史。Web 会话的 `?session=` 同样只在这两条上被接受。
- **镜像代理的高危路径**：`/api/opencode/**` 里 `/auth*`、`/pty*`、`/global/dispose*`、`/global/config*`、`/config*` 仅 Web Session 可用，APP Token 单独调用返回 403（详见「OpenCode 镜像代理」）。唯一例外是 `GET /config/providers`：两端模型下拉框都依赖它，对 APP Token 放行，凭据由回程剥离置空。
- **项目管理的写操作**：`POST /api/intel/projects`、`PUT /api/intel/projects/{id}`、`DELETE /api/intel/projects/{id}`、`PUT /api/intel/projects/{id}/sources` 需要 Web Session，APP Token 调用返回 **403**（完全无凭据仍是 401）。理由：`PUT`/`sources` 决定后端去读哪个目录，`DELETE` 不可逆。同一项目的 `GET` 仍双通道开放。
- **`directory` 是调用方给的工作目录**（`POST /api/tasks`、`POST /api/batch` 的每个 target、`POST /api/workflow` 及其每个 step、`POST /api/rules`）：任务执行时它成为上游 OpenCode agent 的 cwd，`kind=git` 的规则还会拿它本机跑 `git -C`。因此这几个入口共用一套路径校验（`internal/server/paths.go`）：留空放行（用默认目录），相对路径放行（由上游解析），但拒绝 `/`、`/boot` `/dev` `/etc` `/proc` `/root` `/sys` **及其子树**、`..` 越出当前目录的写法，以及**软链接解析后**落在上述目录里的路径 —— 命中返回 **400** `invalid directory: <原因>`。顶层单段目录（`/w`、`/data`）不拦，挂载盘当工作目录是常见用法。`kind=git` 规则的仓库目录实际取 `schedule` 字段（`directory` 只是兜底），所以 git 类型下 `schedule` 也按同一规则校验（400 `invalid schedule (仓库目录): …`）。**注意**：`/api/opencode/*` 镜像代理仍原样转发 `?directory=` 与 `x-opencode-directory`（镜像语义），这条校验只覆盖本后端自己的编排入口。

### 审计范围

- 审计中间件只记录 **token 认证通过**的 `/api/*` 调用（`Authorization` 头，或 WS/SSE 两条握手上的 `?token=`）；纯 Web Session 调用与免鉴权端点不入库。
- `/api/stt**` 子树整体跳过审计（一次录音几十条分片会淹没审计表）。
- 写入走异步队列（队列长度 512，每 500ms 批量落库、单批最多 256 条），不阻塞请求。

## 端点一览

| 分组 | 前缀 | 鉴权 | 章节 |
|------|------|------|------|
| 公共 | `/api/health`、`/api/system`、`/` | 免鉴权 / 双通道 | [公共端点](#公共端点) |
| Web 配置 | `/api/web/**`、`/api/tokens*` | Web Session / 双通道 | [Web 配置](#web-配置web-session--双通道) |
| 编排 | `/api/projects*`、`/api/tasks*`、`/api/batch`、`/api/workflow*`、`/api/archives*`、`/api/sync`、`/api/events`、`/api/unread*` | 双通道（`/api/batch` 仅 Token） | [编排 API](#编排-api双通道) |
| LLM 能力 | `/api/llm*`、`/api/embed` | Web Session / 仅 Token | [LLM 与嵌入配置](#llm-与嵌入配置) |
| 语音识别 | `/api/stt**` | 仅 Token | [语音识别](#语音识别仅-app-token) |
| 自动化规则 | `/api/rules*`、`/api/webhook` | 双通道 / 共享密钥 | [自动化规则](#自动化规则双通道) |
| 审计统计 | `/api/audit`、`/api/stats`、`/api/alerts` | 双通道 | [审计与统计](#审计与统计双通道) |
| 推送 | `/api/ws`、`/api/stream` | Token / WS·SSE 专用凭据 | [推送通道](#推送通道websocket)、[流式对话](#流式对话sse-中继) |
| 镜像代理 | `/api/opencode/**` | 双通道（高危仅 Web Session） | [OpenCode 镜像代理](#opencode-镜像代理apiopencode) |
| 测试智能 | `/api/intel/**`（56 条注册项） | 双通道 | [测试智能 API](#测试智能-apiapiintel) |

## 公共端点

### GET /api/health（免鉴权）
```json
{ "status": "ok", "upstream": true, "upstreamError": "", "time": "..." }
```
- `upstream`：后端能否连到本机 OpenCode（每次调用实时 ping，5s 上限）。
- 仅支持 GET，其余方法 405。

### GET /api/system（双通道）
返回内部地址，因此需要鉴权（`Authorization: Bearer` 或 Web Session 任一；本端点**不接受** `?token=`）：
```json
{ "backend": "starburst-backend", "version": "0.0.0-dev",
  "opencodeURL": "http://127.0.0.1:4096", "opencodeVersion": "v1.2.3",
  "db": "sqlite", "pgvector": false, "vectorCapable": false }
```
- `db`：`sqlite` 或 `postgres`。
- `pgvector`：PostgreSQL 上 pgvector 扩展是否已安装（SQLite 恒 false）。
- `vectorCapable`：`pgvector` 且嵌入模型已配置 —— APP 只在 true 时展示智能测试（RAG 问答/向量检索）入口。

### GET /（免鉴权）
无头模式提示页；配置页资产嵌入时返回 index.html。

## Web 配置（Web Session / 双通道）

### POST /api/web/session（免鉴权）
请求：`{"password":"admin"}`
- 200 → `{"session":"<sid>"}`（存 localStorage，24h 有效）
- 401 → `{"error":"invalid password"}`
- 429 → `{"error":"too many login attempts, try again later"}`：同一来源 5 分钟内失败 5 次后限流，登录成功即重置。来源取自 `RemoteAddr`；**仅当连接来自回环**（即部署在本机反代之后的常见形态）才改信 `X-Forwarded-For` 的第一跳，所以反代后面仍是按客户端限流，而不是全后端共享一个计数器 —— 反代必须覆写而不是追加 XFF，否则该计数可被伪造来源绕开

### DELETE /api/web/session（仅 Web Session）
退出登录，只认 `X-Web-Session` 头 —— 这里是 `requireWeb` 硬校验，APP Token 单独调用返回 401（`handleWebSession` 的 DELETE 分支没有 Token 兜底）。→ `{"ok":true}`

### POST /api/web/password（需 X-Web-Session）
请求：`{"currentPassword":"当前密码","newPassword":"新密码"}`
- 200 → `{"ok":true}`；**调用方自己的会话保留，其它 Web Session 全部吊销**（防止被盗用的会话在改密后继续有效，其它设备需重新登录）
- 400 → 新密码不足 8 位，或命中弱口令黑名单（`admin`、`12345678`、`admin123` 等）
- 401 → 缺少/错误 `X-Web-Session`，或 `currentPassword` 不匹配（改密必须验旧密码，只有会话不够）

### GET /api/tokens（双通道）
Token 元数据列表（`tokenHash` 永不序列化给客户端）：
```json
[{ "id":"...", "name":"我的手机", "createdAt":"...", "revokedAt":null, "lastUsed":null }]
```

### POST /api/tokens（双通道）
请求：`{"name":"设备名"}`
- 201 → `{"token":"ocb_xxxxxxxxxxxxxxxxxxxx"}`（明文只出现在这一次响应里）

### DELETE /api/tokens/{id}（双通道）
撤销。→ `{"ok":true}`；404 → 不存在

## 编排 API（双通道）

### GET /api/projects（双通道）
本机 OpenCode 会话按**工作目录**分组的列表（取 `/experimental/session` 的真实目录，再合并 `/project` 里无会话的 worktree，所以空项目也会出现；有会话的目录排前面）：
```json
{ "projects": [ { "id":"/path/to/repo", "directory":"/path/to/repo", "sessionCount":3 } ] }
```
- 上游不可达 → 502（`{"error":"opencode: ..."}`）。

### GET /api/projects/{dir}（双通道）
返回工作目录等于 `{dir}` 的会话明细（真实过滤，不是重复全量列表）。`{dir}` 是路径段，含 `/` 的目录需 URL 编码（如 `/w` → `%2Fw`）：
```json
{ "directory": "/w", "sessions": [ {
  "id":"ses_...", "slug":"...", "title":"...", "directory":"/w", "path":"/w",
  "agent":"build", "model":"m1", "busy":true,
  "inputTokens":100, "outputTokens":50, "reasoningTokens":0, "cost":0,
  "createdMs":1700000000000, "updatedMs":1700000100000
} ] }
```
- 顶层 `directory` 回显解析后的目录；无匹配 → `{"directory":"/w","sessions":[]}`；缺 `{dir}` → 400；上游不可达 → 502
- `slug`/`title`/`directory`/`path`/`agent`/`model` 为空时整个字段省略（`omitempty`），`busy` 与 token 计数恒返回

### GET /api/tasks?status=queued&limit=50&offset=0（双通道）
任务列表（最新在前）。`status` 可选过滤；`limit` 默认 50、最大 500（超出或 ≤0 回退到 50）；`offset` 默认 0（负数回退到 0）。`total` 是符合 `status` 过滤的总数（不受分页影响），用于判断还有没有下一页：
```json
{ "total": 137, "limit": 50, "offset": 0, "tasks": [ {
  "id":"task_...", "sessionId":"ses_...", "directory":"/path", "name":"加日志",
  "prompt":"...", "dependsOn":"task_...", "status":"queued",
  "error":"", "result":"", "progress":"", "aiSummary":"", "attempts":0,
  "priority":0, "timeoutSeconds":0, "workflowId":"",
  "createdAt":"...", "updatedAt":"...", "startedAt":null, "finishedAt":null,
  "availableAt":"...", "scheduledAt":null, "cron":"", "lastFiredAt":null
} ] }
```
- `status` 取值见文末状态表（8 个，含 `scheduled`）；`availableAt` 是允许被领取的最早时间（重试退避会推后它）。

### POST /api/tasks（双通道）
只有 `prompt` 必填：
```json
{ "name":"给所有 controller 加日志", "prompt":"给所有 controller 加日志",
  "sessionId":"ses_...(可选)", "directory":"/path(可选,新会话用)",
  "dependsOn":"task_...(可选,前置任务)", "priority":0, "timeoutSeconds":0,
  "workflowId":"", "scheduledAt":"2026-01-02T08:00:00Z", "cron":"0 0 8 * * *" }
```
- 201 → 完整 Task 对象（含新生成的 id 与初始 `status`）
- `sessionId` 会拼进 executor 的上游 URL，必须匹配 `^[A-Za-z0-9_-]{1,128}$`，否则 400
- `priority` 越大越先被 worker 领取（领取顺序 `priority DESC, created_at ASC`，并受 `available_at <= now` 约束）；不传即 0。`timeoutSeconds` 0/缺省 = 单次执行不额外限时
- `cron` 为 **6 字段秒级**表达式，创建时用 `automation.ParseCron` 校验，非法 → 400。带 `cron` 的行是周期模板：自身不执行，到点由调度器克隆一条实体任务（6 字段解析与校验细节见 `docs/ARCHITECTURE.md`）
- `scheduledAt` 必须是 RFC3339，否则 400
- 依赖与排期语义（**排期优先于依赖**）：

  | 条件 | 初始 `status` |
  |------|--------------|
  | `cron` 非空 | `scheduled` |
  | `scheduledAt` 是未来时间 | `scheduled` |
  | 无 `dependsOn`，或前置已 `succeeded` | `queued` |
  | `dependsOn` 指向 queued/running/pending/scheduled | `pending`（前置成功后自动转 `queued`） |
  | `dependsOn` 不存在，或指向 failed/canceled/blocked | 400（快速失败，避免留下永远 pending 的任务） |

### GET /api/tasks/{id}（双通道）
单个任务详情（完整 Task 对象）。404 → 不存在。

### DELETE /api/tasks/{id}（双通道）
取消任务（queued/running/pending，以及 scheduled 走排期专用路径）。
- 200 → `{"ok":true}`；若该任务有 pending 下游，返回 `{"ok":true,"blocked":<n>}` 并把下游标记为 `blocked`
- 409 → 已结束（succeeded/failed/canceled/blocked）

### POST /api/tasks/{id}（双通道）
手动解阻：把 `blocked` 任务重新排队执行（人工已在前置任务之外处理好问题，
无需重跑前置）。
- 200 → `{"ok":true}`，并通过 `/api/ws` 推送 `task.event`（`status=queued`，`reason="manual unblock"`）
- 409 → 任务不是 `blocked`（含不存在的 id）

### POST /api/tasks/{id}/retry（双通道）
手动重试（`RequeueTask`）处于可重试终态的任务。
- 200 → `{"ok":true}`，推送 `task.event`（`status=queued`，`reason="manual retry"`）
- 409 → 当前状态不可重试（含不存在的 id）

### GET /api/tasks/{id}/dependents（双通道）
以本任务为 `dependsOn` 前置的下游任务。→ `{"dependents":[Task, ...]}`（无下游时为空数组，不是 null）

### POST /api/tasks/action（双通道）
批量操作：`{"ids":["task_..."],"action":"cancel"|"retry"}`
- cancel → `{"ok":true,"affected":<n>}`；逐条推送 `task.event`（`reason="batch cancel"`），并把下游级联置 `blocked`
- retry → `{"ok":true,"affected":<n>}`；逐条推送 `task.event`（`status=queued`，`reason="batch retry"`）
- 400 → `ids` 为空，或 `action` 不是 cancel/retry

### DELETE /api/tasks（双通道）
清理终态任务（succeeded/failed/canceled）。`?olderThan=<秒>`，默认 0 = 清理全部终态；仍被非终态任务引用的行会保留。→ `{"deleted":<n>,"kept":<m>}`

### GET /api/tasks/stats?days=7（双通道）
滚动窗口聚合。`days` 默认 7，仅接受 1-90，越界回退 7：
```json
{ "statusCounts": { "queued":1, "succeeded":5, "failed":1 },
  "total":7, "completed":6, "succeeded":5, "failed":1,
  "successRate":0.833, "avgDurationSec":42.5,
  "trend": [ { "day":"2026-01-02", "created":3, "succeeded":2, "failed":1 } ] }
```

### POST /api/batch（仅 APP Token）
一条指令对多个 target 批量建任务：
```json
{ "prompt":"给所有模块加日志", "targets":[ {"directory":"/a"}, {"sessionId":"ses_..."} ] }
```
- 201 → `{"created":["task_..."],"count":2}`
- 400 → `prompt` 为空 / `targets` 为空 / 单请求超过 100 个 target

## 工作流（多步编排，双通道）

一条工作流 = 一组首尾相接的任务：`steps[i].dependsOn = steps[i-1].id`，全部共享同一个 `workflowId`，因此严格串行执行。

### POST /api/workflow（双通道）
```json
{ "name":"发版前检查", "directory":"/path",
  "steps":[ { "name":"构建", "prompt":"跑一遍构建", "directory":"(可空,回落到工作流 directory)", "timeoutSeconds":0, "priority":0 } ] }
```
- 200 → `{"workflowId":"task_...","tasks":["task_...","task_..."]}`（注意是 200，不是 201）
- 400 → `steps` 为空，或某步 `prompt` 为空（错误信息带步号：`step 2 has empty prompt`）
- 首步以 `queued` 入队，其余 `pending`；创建成功后推送 `task.event`，payload 为 `{"workflowId":"...","tasks":["task_..."],"name":"..."}`（severity=info）

### GET /api/workflows（双通道）
最近 50 条编排的汇总，无数据时返回空数组：
```json
{ "workflows": [ { "workflowId":"task_...", "name":"发版前检查", "steps":3,
                   "succeeded":2, "failed":0, "running":1, "createdAt":"..." } ] }
```
- `running` 统计的是「未完成」（queued/running/pending/retrying）

### GET /api/workflow/{id}（双通道）
该编排的逐步任务明细（含实时状态）。→ `{"steps":[Task, ...]}`

### POST /api/workflow/{id}/cancel（双通道）
取消整条编排的未完成任务。→ `{"ok":true,"canceled":<n>}`

### POST /api/workflow/{id}/rerun（双通道）
从第一个未完成步骤重跑。→ `{"ok":true,"reset":<n>}`

## 归档 · 同步 · 事件 · 未读（双通道）

### POST /api/archives（双通道）
把远端会话归档到后端存储。请求：`{"sessionId":"...", "format":"markdown"|"json"}`（format 默认 markdown）
- 201 → `{"id":"arch_...","size":123,"format":"markdown"}`
- 400 → `sessionId` 缺失或不符合 `^[A-Za-z0-9_-]{1,128}$`、`format` 非 markdown/json；502 → 上游拉取消息失败

### GET /api/archives（双通道）
归档元数据列表（`content`/`rawMessages` 恒为空串），`?limit=`（默认 50，最大 500）。→ `{"archives":[...]}`

### GET /api/archives/{id}（双通道）
完整归档（含 `content`、`rawMessages`）。404 → 不存在。

### DELETE /api/archives/{id}（双通道）
删除归档。→ `{"ok":true}`；404 → 不存在

### GET /api/sync?key=xxx（双通道）
读取某个 key 的最新配置快照（设备/团队同步，last-write-wins）。404 → 该 key 从未推送过；400 → 缺 `key`。

### PUT /api/sync（双通道）
推送新快照：`{"key":"...","payload":"<任意字符串,通常是 JSON 文本>"}` → `{"key":"...","revision":<n>}`

### GET /api/events?sessionId=&since=&limit=（双通道）
采集器记录的上游会话事件（APP 首页与 Web 工作台共用）。`limit` 默认 200、最大 1000；`since` 接受 RFC3339 或 unix 毫秒，非法 → 400。
```json
{ "events": [ { "id":1, "sessionId":"ses_...", "eventType":"message.part.updated",
                "payload":{ }, "createdAt":"..." } ] }
```
- 高频事件（`*.delta`、含 `progress`、`message.part.updated/removed`、`heartbeat`）不落库，只走 WS 推送

### GET /api/unread（双通道）
有未读活动的会话集合。注意外层有 `unread` 包一层，不是裸 map：
```json
{ "unread": { "ses_...": true } }
```

### POST /api/unread/{sessionId}（双通道）
清除某会话的未读标记（任一端读过即全端清除）。→ `{"ok":true}`；400 → 路径缺 id 或 id 含 `/`

## 语音识别（仅 APP Token）

把音频分片代理到部署在另一台主机上的流式识别引擎（`--stt-url`）。后端不做解码，只负责鉴权、分片体积校验与协议透传；未配置 `--stt-url` 时本节端点一律返回 503，客户端应回退到端侧识别。

音频格式：**16kHz / 单声道 / PCM16LE 裸字节**（无 WAV 头），`Content-Type: application/octet-stream`。

鉴权统一为 `requireToken`（只认 `Authorization: Bearer`，**不**接受 `?token=`）。

### GET /api/stt（仅 Token）
引擎健康与能力探测，供客户端决定是否启用服务端识别。
- 200 → `{"enabled":true,"maxChunkBytes":2097152,"engine":{"status":"ok","model":"...","sample_rate":16000,"sample_width":2,"channels":1,"sessions":1,"max_sessions":16}}`
- 503 → 未配置引擎；502 → 引擎不可达或不健康

### POST /api/stt/sessions（仅 Token）
新建识别会话。
- 201 → `{"session_id":"3028de8b55454b27","sample_rate":16000,"sample_width":2,"channels":1}`
- 503 → 引擎已达并发上限

### POST /api/stt/sessions/{id}/chunks（仅 Token）
上传一个音频分片，**响应体即当前累积文本**（每次请求都会重新解码，所以边说边出字）。
- 200 → `{"session_id":"...","text":"昨天是 MONDAY","final":false,"bytes":6400,"seconds":0.4}`
- 400 → 空分片 / 样本数非法；413 → 超过 `--stt-max-chunk-bytes`；404 → 会话不存在（或已超时回收）

### POST /api/stt/sessions/{id}/finish（仅 Token）
结束输入，引擎冲刷尾部后返回最终文本。
- 200 → `{"session_id":"...","text":"昨天是 MONDAY TODAY IS LIBR","final":true}`

### DELETE /api/stt/sessions/{id}（仅 Token）
丢弃会话（如用户取消录音）。→ `{"deleted":true,"session_id":"..."}`；404 → 不存在

### POST /api/stt/refine（仅 Token）
用编排大模型校对一段流式识别文本（补标点、去重复字、修同音字）。**永不失败**：模型缺失/失败/输出不可信时，返回本地规则（错字词典 + 去重 + 确定性补标点）的结果并在 `reason` 说明原因。请求：`{"text":"昨天是是周一以经开完会了"}`
- 200 → `{"text":"昨天是周一，已经开完会了","changed":true}`；未改动或走本地兜底时 → `{"text":"...","changed":false,"reason":"llm not configured"|"llm failed"|"rejected"}`
- 400 → `text` 为空 / body 非法；413 → 文本超过 16 KiB
- 超过 200 字的长文本按子句分块多次调用模型，单轮总预算 90s

> 会话空闲超过 120s 由引擎自行回收；单分片上限 2 MiB。`/api/stt/**` 的调用不写审计表，避免一条 10 秒录音留下几十行噪音。

## LLM 与嵌入配置

### GET /api/llm（仅 Web Session）
编排大模型运行时配置。**密钥永不回显**，只报告是否已设置：
```json
{ "url":"https://<llm-host>/v1", "model":"<model-name>", "keySet":true, "enabled":true }
```

### POST /api/llm（仅 Web Session）
请求：`{"url":"...","key":"...","model":"..."}`。`url`/`model` 原样覆盖（空串=清空）；**`key` 传空表示保留当前密钥**。落库后热生效，响应体同 GET。503 → 未初始化 LLM 客户端。

### GET /api/embed（仅 Web Session）
嵌入模型配置，字段与 `/api/llm` 一致（`url`/`model`/`keySet`/`enabled`）。RAG 检索与智能测试的向量能力依赖它。

### POST /api/embed（仅 Web Session）
同 `POST /api/llm` 语义（空 key 保留原值）。503 → 未初始化嵌入客户端。

### POST /api/llm/generate（仅 APP Token）
把对话上下文转成 3 条下一步建议（结果不入库）。请求：`{"system":"(可空,用内置提示词)","user":"..."}`
- 200 → `{"suggestions":["...","...","..."]}`
- 400 → `user` 为空；502 → 模型调用失败或返回空数组；503 → 未配置编排大模型；429 → 单 token 超过 20 次/分钟

### POST /api/llm/complete（仅 APP Token）
任意文本补全（如生成 AGENTS.md 草稿），返回散文而非 JSON 数组。请求体与 `/api/llm/generate` 相同，120s 上限，同样限流。
- 200 → `{"text":"..."}`；400 / 502 / 503 / 429 同上

### POST /api/tasks/generate（仅 APP Token，支持 `?stream=1`）
自然语言 → 任务计划草稿（**不落库**，客户端确认后走 `POST /api/tasks`）。两种模式：
- 新建：`{"description":"每天早上 8 点跑测试并汇总"}`
- 细化：`{"draft":{...},"instruction":"再加一步清理构建产物"}`

草稿结构：
```json
{ "draft": { "name":"", "directory":"", "steps":[ { "name":"", "prompt":"", "directory":"" } ],
             "schedule": { "type":"immediate|delay|at|cron", "minutes":0, "cron":"", "at":"" } } }
```
- 200 → `{"draft":{...}}`（`steps` 为空、某步 `prompt` 为空、`schedule.type` 非法 → 502）；400 → 两种模式参数都不满足；503 → 未配置编排大模型；429 → 单 token 超过 20 次/分钟
- `?stream=1` 时改为 SSE：若干 `data: {"type":"delta","text":"..."}`，末尾一条 `data: {"type":"draft","draft":{...}}`，失败发 `data: {"type":"error","message":"..."}`

## 自动化规则（双通道）

### GET /api/rules（双通道）
规则列表。→ `{"rules":[...]}`

### POST /api/rules（双通道）
```json
{ "name":"每晚测试", "kind":"cron|git|http", "schedule":"5m 或 cron 或 target", "directory":"/path", "prompt":"指令", "intelProjectId":1, "enabled":true }
```
- 201 → 完整 Rule 对象（含新生成的 id）
- `intelProjectId` 非 0 时：触发时改跑**测试智能 run-all 回归**（而非 prompt 任务），`prompt` 可留空；`intelProjectId=0`（默认）时必须有 `prompt`

### POST /api/rules/generate（双通道）
自然语言自动生成一条规则草稿（不落库，客户端确认后再走 `POST /api/rules`）。请求：
```json
{ "description":"每天早上 8 点运行测试" }
```
- 200 → 草稿（字段与 Rule 一致，`enabled` 固定为 true）：
```json
{ "draft": { "name":"...", "kind":"cron|git|http", "schedule":"...", "directory":"", "prompt":"...", "enabled":true } }
```
- 400 → `description` 为空；502 → 模型生成失败（含无效 kind/空 prompt）；503 → 未配置编排大模型

### DELETE /api/rules/{id}（双通道）
删除规则。→ `{"ok":true}`；404 → 不存在

### GET /api/rules/{id}/executions（双通道）
规则执行历史（触发时间 + 产生的任务）：
```json
{ "executions": [ { "id":1, "ruleId":"rule_...", "taskId":"task_...", "triggeredAt":"..." } ],
  "total": 3 }
```

### POST /api/webhook?target=xxx
触发匹配的 http 规则（供 git 等外部系统调用，不走 Token / Web Session）。→ `{"fired":true}`；404 → 无匹配规则；405 → 非 POST。
- 凭据是**共享密钥**，只认 `X-Webhook-Secret: <secret>` 头（**不支持 `?secret=`**）：未配置 `--webhook-secret` 时本端点整体关闭并返回 **403 `webhook disabled (no secret configured)`**，配了但缺失/不符 → 401。

## 审计与统计（双通道）

### GET /api/audit?tokenId=&limit=（双通道）
最近审计记录（token 认证的 API 调用）。→ `{"audit":[...]}`

### GET /api/stats（双通道）
用量统计：
```json
{ "tasks":{"queued":0,"running":0,"succeeded":5,"failed":1,"canceled":0,"pending":0,"blocked":0,"retried":1,"total":7},
  "tokenUsage":[{"tokenId":"...","tokenName":"我的手机","calls":12}],
  "archives":3 }
```

### GET /api/alerts（双通道）
资源水位与告警判定快照 → `{"enabled":bool,"thresholds":{…},"metrics":{…},"alerts":{"cpu":false,"mem":true,…}}`；未启用采集器时返回**空快照（200）**而不是 404。

### POST /api/alerts（双通道）
`{"enabled":true,"cpuPct":85,"memPct":90,"diskPct":88}` 局部更新阈值（字段可省略，省略即不改）→ 更新后的同一份快照；越界/非法值 → 400；其余方法 405。

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

### GET /api/stream（需 Token 或 Web Session，接受 `?token=` / `?session=`）
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

## OpenCode 镜像代理（/api/opencode/**）

整棵 `/api/opencode/` 子树注册到同一个 handler（`handleOpenCodeProxy`）：**任意方法、任意上游路径**都剥掉前缀后转发给本机 OpenCode，上游的状态码/响应头原样回传（只滤掉逐跳头），响应体除下文「凭据剥离」一族外也是逐字节透传。它不是 REST 接口，没有逐端点清单，下文只写代理层自身的语义。

- **入口鉴权**：`requireToken` **先判**，再 `requireWeb`（与本章其它端点「Web Session 优先」的顺序相反，效果同为双通道）→ 都不通过 401 `web session or APP token required`。
- **高危上游操作仅管理员**：非 Web Session（即只用 APP Token）调用下列上游路径 → **403 `operation requires admin web session`**：`/auth`、`/auth/**`、`/pty`、`/pty/**`、`/global/dispose`、`/global/dispose/**`、`/global/config`、`/global/config/**`、`/config`、`/config/**`。理由是设备 token 一旦泄露不等于 OpenCode 全权——写 provider key、开 PTY、全局 dispose 都必须由登录后的配置页触发。读取与 permission reply（APP 远程批准）保留可用。
  - **唯一放行例外**：`GET /config/providers`。App 与 Web 的模型下拉框都以它为数据源，挡成 admin-only 会让安卓端列不出任何模型；它的响应体改由下面的凭据剥离兜底。
- **凭据剥离**：`/config*` 与 `/provider*` 的响应（上游会在每个 provider 的 `key` 字段里原样回传明文 API Key）会在回程被整体解析并把凭据形态的字段置空——判定是**字段名后缀**匹配（去 `_`/`-`/空格 + 小写后以 `key`、`token`、`secret`、`password`、`credential`、`authorization` 等结尾），所以 `ANTHROPIC_API_KEY`、`aws_secret_access_key` 这类写法同样被抹掉；只置空非空**字符串**，`models`、`limit.context`、`capabilities`、`options.baseURL` 等结构与非字符串字段保持原样。**不看响应的 `Content-Type`**：按类型放行等于上游漏带类型就能把密钥透出去。因此这两族的响应**不再是逐字节透传**：重编码后长度变化，`Content-Length` / `Content-Encoding` 一并丢弃。空响应（204/304）只回状态码；无法解析为 JSON、或超过 16 MiB 时**宁可不转发**，直接 502 `upstream response could not be sanitized`——失败兜底走透传等于没做剥离。
- **请求体上限**：代理转发的请求体最大 64 MiB（附件以 base64 内嵌，10 MiB 文件约 13.4 MiB JSON，留足余量）。`Content-Length` 超限直接 413 `request body too large`，分块传输由 `http.MaxBytesReader` 兜底。
- **凭据替换**：客户端的 `Authorization` 头**不转发**（它属于本后端），上游收到的是 `opencode.Client` 自身配置的凭据。目录作用域头（`x-starburst-directory` / `x-opencode-directory`）与查询参数原样透传。
- **一处本地增强**：`GET /session/status` 不再纯代理，而是合并上游快照与采集器的事件聚合结果（上游快照偶发漏掉 busy 会话），其余路径仍是纯转发。
- **SSE 透传**：`GET /global/event` 这类流式响应逐块读、每读一次 flush 一次，事件实时到达 APP。
- 上游请求失败 → **502 `upstream opencode request failed`**；客户端断开时上游连接随之取消。

## 测试智能 API（/api/intel/**）

测试智能（intel）子系统对**被测项目的源码**做静态分析（模块、接口契约、实体表、网关路由、依赖与 SBOM），在此之上提供知识库问答、测试计划与执行、缺陷归因与修复建议、测试环境供给。路由表共 56 条注册项、44 个去重路径。

### 能力前置条件

向量检索只在 **PostgreSQL + pgvector** 上提供，能力位由 `GET /api/system` 报告（`vectorCapable` = pgvector 已装且嵌入模型已配置），APP/控制台在 false 时整体隐藏智能测试入口。逐条依赖：

| 依赖 | 受影响端点 | 不满足时的返回 |
|------|-----------|--------------|
| pgvector（向量检索） | `POST /api/intel/ask`（唯一做检索的端点）、`POST /api/intel/index`（索引只有被检索这一条出路，因此一并拒绝） | SQLite → `store.ErrRagUnsupported`，两个 handler 都映射为 **503**，不静默降级 |
| 嵌入模型（`POST /api/embed`） | `POST /api/intel/index`、`POST /api/intel/ask` | **500**（`embeddings not configured`）。判定顺序是先嵌入后 pgvector：SQLite 且未配嵌入 → 500，SQLite 且已配嵌入 → 503（不再白烧一次 embedding 调用去写无人可读的 `intel_chunks`） |
| 编排 LLM（`POST /api/llm`） | `overrides/suggest`、`scan/rules`、`ai-rules/{id}/polish` → **400**；`fixes/generate` → **500**；`features/{id}/chat` → 200 但 `mode="no-llm"`；模块摘要 / 接口 summary / 网关建议 → 静默跳过 |
| Docker | `env/ensure`、`env/status`（响应里 `dockerReady=false` + `dockerNotice`）、`env/install` → **500** |
| git / 构建工具链 / adb | `analyze`、`run*`、`env/devices/connect` | 分析失败 → 500；执行类失败写进 run 行 `status=failed`+`progress`，**不是** HTTP 错误 |

其余端点（项目、清单、计划、缺陷、覆盖度、环境、节点）只依赖关系表，SQLite 部署同样可用。

### 通用约定

- **鉴权**：`/api/intel/**` 逐条都是 `if !requireWeb(r) { requireToken(r) }` 的**双通道**形态，两者都不通过 → `401 {"error":"web session or APP token required"}`；`requireWeb` 先判，Web Session 永远优先。唯一例外是项目管理的四个写操作（`POST /api/intel/projects`、`PUT`/`DELETE /api/intel/projects/{id}`、`PUT /api/intel/projects/{id}/sources`）额外要求 Web Session，APP Token → **403**。本章下文不再逐个标注。
- **`projectId` 必填**：清单类用查询串 `?projectId=<正整数>`（缺失/非正整数 → 400），动作类放 body。`?moduleId=` 普遍可选（0 = 不限模块）。
- **路径 id 三种解析风格**：`intelPathID`（`projects/`、`chats/`、`runs/`、`findings/`）非法 id → 400，且文案**恒为 `invalid project id`**，即使在 `runs/`、`findings/` 上解析的其实是 run / finding id；它只取第一段，所以 `{id}` 后面的任意后缀都被忽略。`intelIDFromPath`（`modules/`、`endpoints/`、`features/`）非法 id → 直接返回**空响应体（200）**，不报错。`fixes/{id}/{action}`、`pending/{id}`、`ai-rules/{id}`、`results/{id}` 各自内联 `strconv.ParseInt`，同为 400 但文案分别是 `invalid fix id` / `invalid override id` / `invalid rule id` / `invalid result id`。
- **没注册的路径不返回 404**：`/api/intel/overrides/1`、`/api/intel/findings/1/notes` 这类形态没有子树注册，会落到 catch-all `/` → `handleIndex`，返回 **200 的控制台页面/纯文本**而不是 JSON。写 SDK 时按「404 = 不存在」假设会误判。
- **请求体**：JSON，上限 4 MiB；解析失败 → 400 `invalid request body`。项目管理的四个写接口走 `readBody`，超限时按全局约定返回 413 `request body too large`，其余 intel 端点把超限与非法并列为 400。
- **同步 vs 异步**：`analyze`(5min)、`index`(15min)、`contracts/check-batch`(5min)、`env/install`(5min)、`env/schema-init`(10min)、`features/test`(2min)、`scan/rules`(5min) 是同步阻塞的；`run` / `run-all` / `plan` POST 是异步入队，立刻返回 run 行。
- **实时进度**：run 的每次状态变更通过 `/api/ws` 广播 `{"type":"intel.run.event","payload":{"run":{…}}}`（推送通道章节的事件表未列这一族）。全局并发上限 2（`intelExecConcurrency`），单模块 10min、聚合运行 30min，`output` 只保留末尾 256 KiB。「同项目串行」指的是**不同 run 之间**抢同一把项目锁；`run-all` / `plan` 聚合内部各模块是**并发**跑的（同样受全局 2 限制），只是聚合自身持锁期间别的 run 进不来。拿不到执行槽位时等到 `intelRunTimeout` 才失败（`排队超时，未获得执行槽位`）。
- 分析、索引、增量分析共用同一把**项目级互斥锁**，并发调用排队而不是报错。

### 项目与分析

- `GET /api/intel/projects` → `{"projects":[IntelProject]}`（最新注册在前，无分页）。
- `POST /api/intel/projects` → **200**（不是 201）+ 完整 IntelProject：
```json
{ "name":"order-center", "source":"local|git", "localPath":"/srv/code/order",
  "gitUrl":"", "gitRef":"", "description":"", "commandsJson":"[\"go test ./...\"]",
  "envName":"", "snapshotSha":"", "analysisStatus":"running", "analyzedAt":null }
```
  `source` 缺省 `local`；`local` 必须给 `localPath`、`git` 必须给 `gitUrl`，否则 400；`name` 留空则从路径/URL 末段推导。创建即后台跑一次全量分析（不阻塞响应），进度看 `analysisStatus`（`""|running|ok|failed`）。**APP Token → 403。**
- `GET /api/intel/projects/{id}` → `{"project":…,"modules":[…]}`；404 → 不存在。**副作用**：已分析过的 local 项目若目录快照 SHA 变了（源码有改动）会后台自动全量分析，同项目 2 分钟防抖。尾随子路径被忽略，`/api/intel/projects/{id}/modules` 响应同本详情。
- `PUT /api/intel/projects/{id}` → 200 + project。**APP Token → 403。** `name`/`description`/`envName`/`gitRef` 空值不覆盖；`source` 只接受 `local|git`（其他 400，缺省表示不动来源），`local` 按下面的目录校验通过后清空 `gitUrl`（`git` 反之）；`commandsJson` 必须是 **JSON 字符串数组**，逐条 trim 丢空后落库为项目级命令白名单（运行期按 argv 拆分、**不经 shell 执行**）。来源四字段（`source`/`localPath`/`gitUrl`/`gitRef`）任一变化 → 后台**增量**分析（只扫新增模块、删掉已移除模块的派生数据）；仅 `description` 变化 → 只重建知识库索引。
- `DELETE /api/intel/projects/{id}` → `{"ok":true}`（连带清派生数据与向量 chunk）。**APP Token → 403。**
- **本地源码目录校验（`localPath`，创建/`PUT`/`sources` 共用）**：必须是非空的**绝对路径**，`filepath.Clean` 后落库；软链接按 `EvalSymlinks` 解析后的真实位置判定，因此「项目目录里一个指向 `/etc` 的链接」同样被拒。拒绝清单：`/` 本身、只有一段的顶层目录（`/tmp`、`/home` 这类整棵目录不可能是仓库）、以及 `/boot` `/dev` `/etc` `/proc` `/root` `/sys` **及其子树**。刻意**不**限制 `/run` `/tmp` `/var` `/usr` `/home` 下的多层目录——被测仓库完全可能放在那里。不通过一律 400，报错文案直接回显原因。
- `GET /api/intel/projects/{id}/sources` → `{"sources":[IntelProjectSource]}`（多端多仓库的关联源码）；`PUT` 同路径**整体替换**（字段 `endName|source|localPath|gitUrl|gitRef`，校验同创建：`local` 走上面的目录校验、`git` 要求 URL、`endName` 不得重复，均 400）→ `{"ok":true,"sources":[…]}`；`PUT` **APP Token → 403**；列表实际变化才触发后台增量分析。关联仓库的模块 `relPath` 带 `@<端名>/` 前缀。
- `POST /api/intel/analyze` `{"projectId":3}` → `{"ok":true}`。`projectId` 必填正整数；一次全量分析产出模块清单、接口契约、实体列、测试资产、功能点聚类、三端客户端绑定、网关路由、依赖/环境需求/CycloneDX SBOM、增量影响快照、合规与安全 finding，成功后后台重建向量索引。失败 → 500 `analyze failed: <原因>` 且 `analysisStatus=failed`。
- `GET /api/intel/overview?projectId=3` → `{"overview":{"depsJson","envJson","sbomJson"}}`；**未分析过 → `{"overview":null}`（200，不是 404）**。三个字段都是 JSON 文本，客户端需二次解析。
- `GET /api/intel/impact?projectId=3` → `{"impact":{"baseSha","headSha","impactJson"}}`，同样可能为 `null`。仅 git 项目生成；上一快照与新 HEAD 非祖先关系（force-push / reset）时记为全量重扫。

### 清单：模块 · 实体 · 接口 · 绑定

- `GET /api/intel/endpoints?projectId=&moduleId=` → `{"endpoints":[IntelEndpoint]}`（按 `path, method` 排序）。`requestJson`/`fieldsJson` 是 JSON 文本；`summary` 读到的是人工/LLM 校正后的值；`gatewayRoutes`（`omitempty`）是命中的公网网关路径。
- `PUT /api/intel/endpoints/{id}` `{"summary":"分页查询用户"}` → `{"ok":true,"key":"GET /users"}`。`summary` 去空必填（400）；404 → 接口不存在。写入的是校正层（`target=endpoint`、`rowKey="METHOD path"`、`field=summary`、`status=applied`），**重扫不丢**。
- `GET /api/intel/entities?projectId=&moduleId=` → `{"entities":[IntelEntity]}`，**一行一列**（`entity`/`table`/`column`/`fieldType`/`nullable`/`isPrimary`），按 `table, column` 排序。
- `GET /api/intel/modules/{moduleId}` → `{"module":…,"stats":{"endpoints":n,"entities":n,"cases":n}}`；404 → 模块不存在。`summary` 为空时**懒生成**（调编排 LLM 并缓存落库，未配 LLM 则保持空串）。
- `POST /api/intel/modules/{moduleId}/resummarize` → 清空并立刻重算 → `{"module":…,"summary":"…"}`（60s 上限）。
- `PUT /api/intel/modules/{moduleId}/overrides` `{"role":"业务后端","summary":"…"}` → `{"module":…}`。两个字段都是指针、可只传一个，但**空串等同不传**（无法把覆写改回自动值）；按自然键 `relPath` upsert 成 applied 校正（`kind_role` / `summary`），读取时覆盖自动值。两条写库调用的错误都被丢弃（`_ =`），落库失败也返回 200 + 改过的模块对象。
- `GET /api/intel/gateway-routes?projectId=` → `{"gatewayRoutes":[{service,uri,pathsJson,source,sourceLine}]}`。`source` 为配置文件/Nacos 来源的是权威结果，`source=llm` 是分析时模型建议补齐的路径。
- `GET /api/intel/android-bindings?projectId=` / `GET /api/intel/web-bindings?projectId=` / `GET /api/intel/ios-bindings?projectId=` → 三者同形 `{"bindings":[…]}`，分别是 DataBinding 布局 / Vue 模板 / SwiftUI 视图抽出的「页面 → 字段路径」必展示清单（Android 用 `widget`，Web/iOS 用 `slot`，均带 `sourceFile`/`sourceLine`）。
- `GET /api/intel/test-cases?projectId=&moduleId=` → `{"testCases":[TestCase]}`（按 `path, class, method` 排序）。`lastStatus`/`lastDurationMs`/`flakyCount`/`quarantined` 由每次运行回写（`quarantined` 在 flaky_count 累计达 3 次时自动置 1）。`POST /api/intel/test-cases` body `{"projectId":1,"moduleId":2,"endpoint":"Class.method"}` → 解除该用例的 flaky 隔离（`quarantined=0`、`flaky_count=0`）。
- `GET /api/intel/settings` → `{"workers":N}`（测试执行并发，默认 2）。`POST /api/intel/settings` body `{"workers":3}`（1-16）→ 即时生效并持久化（`intel.workers`），重启恢复。
- 测试报告逐用例解析：`go`（-json）、`surefire`（target/surefire-reports）、`playwright`（web/node 检出 playwright.config 时默认 `npx playwright test --reporter=json`）、`xctest`（xcode 模块，命令靠白名单/计划，报告从 stdout 或 junit*.xml）、`pytest`（python 模块，`pytest --junitxml=junit.xml`）。
- `GET /api/intel/modules?projectId=3` → `{"modules":[IntelModule]}`（按 `relPath` 升序，人工改过的 `kindRole`/`summary` 会被 applied 校正覆盖后返回；**`kindType` 没有覆写通道**，读取侧只认 `kind_role`（含旧写法 `role`）与 `summary` 两个 field，别的 field 存进去也不生效）。`projectId` 缺失或非正整数 → 400；仅 GET，其余 405。这与 `GET /api/intel/projects/{id}` 响应里的 `modules` 同源，区别只在不要求先取项目详情。（此路由曾按 `/api/intel/projects/` 前缀解析路径 id，永不命中 → 200 空体。）

### 知识库与问答

- `POST /api/intel/index` `{"projectId":3}` → `{"ok":true,"chunks":<n>}`。片段来源：每张表一段（列/主键/可空）、每个接口一段、项目 Markdown 文档（单文件截 64 KiB、每块 ≤1500 rune）、三端绑定（按页面聚合）、项目概览段；内容完全相同的片段去重后入库。嵌入维度必须与列维度（1024）一致，否则 500 报模型不匹配。**SQLite 部署 → 503**（这份索引唯一的读者是 `ask`，写它没有接收方）。分析成功后会自动后台重建，通常不必手工调用。
- `POST /api/intel/ask` `{"projectId":3,"question":"用户表有哪些字段","chatId":0,"limit":10}` →

```json
{ "chatId":7, "question":"用户表有哪些字段", "answer":"……",
  "sources":[ { "title":"user 表", "kind":"entity", "content":"……",
                "sourceFile":"user.java", "sourceLine":12, "similarity":0.71 } ], "count":1 }
```

  - `projectId`+`question` 必填（400）；`limit` 默认 10、**上限 20**（超出截到 20，不是报错）。
  - `chatId` 省略/0 = 新会话（标题取问题前 40 rune）；传值 = 续聊，带最近 20 条消息；会话不属于该项目 → 500。
  - 召回有进程内缓存（同项目+同问题+同 limit 命中即跳过 embedding），索引重建时按项目失效。
  - 召回里没有 `overview` 片段时，项目 `description` 会作为「项目画像」插到 `sources` 首位，用来兜住全项目级提问。
  - 未配编排 LLM → `answer` 为空串但仍 200 + `sources`。**SQLite 部署下本端点返回 503**（见前置条件表）。
- `GET /api/intel/chats?projectId=3` → `{"chats":[{id,projectId,title,createdAt,updatedAt}]}`（`updatedAt` 倒序）。
- `GET /api/intel/chats/{id}` → `{"chat":…,"messages":[…]}`，**只回最近 200 条**（长对话不无界增长）；404 → 不存在。`DELETE /api/intel/chats/{id}` → `{"ok":true}`。

### 功能点与契约核对

- `GET /api/intel/features?projectId=3` → `{"features":[IntelFeature]}`（`sortOrder, name` 排序；`endsJson` 是「METHOD path」数组文本；`anchor` 是聚类锚点；`source` = `auto|manual`；`name` 会被 applied 校正覆盖）。
- `POST /api/intel/features` `{"name":"下单","ends":["POST /orders"],"anchor":""}` → 200 `{"feature":…}`；`name` 去空必填，`source` 固定 `manual`（重扫不覆盖）、`status=active`。
- `PUT /api/intel/features/order` `{"projectId":3,"order":[5,2,9]}` → `{"ok":true}`。`order` 非空必填（400），逐个把 `sortOrder` 写成数组下标；不属于本项目的 id 静默跳过。
- `PUT /api/intel/features/{id}` `{"name","summary","ends":[],"status"}` → `{"feature":…}`；只支持 PUT（其余方法 405），空字段不覆盖；404 → 不存在或不属于该项目。
- `POST /api/intel/features/test` `{"projectId":3,"featureId":5,"baseUrl":"http://192.0.2.150:18090"}` →

```json
{ "feature":…, "baseUrl":"…", "endpoints":[IntelEndpoint],
  "results":[ { "method":"GET", "path":"/users/{id}", "url":"…/users/1",
                "status":200, "ok":true,
                "contract":{ "passed":8, "failed":0, "results":[…] } } ], "count":1 }
```

  三字段必填（400），`baseUrl` 必须是合法 http(s)（400）。按 `anchor` 匹配接口，anchor 失配时回退 `endsJson` 精确匹配（也兼容只填 path）。路径/查询参数按类型填占位值（名字含 `id` 或数值类型 → `1`，boolean → `true`，其余 → `test`）；POST/PUT/PATCH 带 `{}` 体；单请求 15s、响应体最多读 1 MiB；`ok` = 2xx/3xx；接口无字段契约则不带 `contract`。整体 2min 预算。

- `GET /api/intel/features/{id}/chats?projectId=3` → `{"chats":[IntelFeatureChat]}`（id 倒序）。
- `POST /api/intel/features/{id}/chat` `{"projectId":3,"question":"这个功能为什么失败"}` → 200 `{"chat":…,"mode":"ai"|"no-llm"}`。**不走向量检索**：上下文由 `buildFeatureChatContext` 确定性拼装（关联接口契约 + 最近一次运行实测结果 + 挂载问题），所以 SQLite / 未配 embedding 也可用；未配 LLM 时 `answer` 是占位文案但记录仍入库。`projectId`+`question` 必填，功能点不属于该项目 → 404。
- `POST /api/intel/contracts/check` `{"endpointId":11,"responseJson":"{\"id\":1}"}` → 逐字段核对真实响应 → `{"results":[…],"passed":8,"failed":0}`。两字段必填（400）；404 → 接口不存在；接口无字段契约 → **先**返回 200 `{"results":[],"passed":0,"failed":0,"note":"该接口暂无响应字段契约"}`（这条短路在前，所以无契约时 `responseJson` 是否合法都不会被校验）；有契约而 `responseJson` 非法 JSON → 400；库里的 `fieldsJson` 本身坏了 → 500。
- `POST /api/intel/contracts/check-batch` `{"projectId":3,"baseUrl":"http://192.0.2.150:18090"}` → 对**全部**接口逐个探测并核对（5min 同步）→ `{"baseUrl","endpoints":[…],"results":[…],"total","contractReady","reachable","passed","failed","graphqlSkipped"}`。GraphQL 操作（`QUERY`/`MUTATION`/`SUBSCRIPTION`）无法按 REST 探测，只计数跳过。

> 上面两个 `baseUrl` 端点是**服务端发起**的出网请求，目标地址由客户端给出，因此过一遍出网校验（见下）：非 http(s) 方案、或解析后命中被禁地址 → **400** `baseUrl target not allowed: …`，在发出任何请求之前拒绝。

### 出网目标校验（`internal/netguard`）

被测环境是本机/内网服务，所以**私网段与回环刻意放行**（`127.0.0.1`、`192.168.x` 都是合法被测目标）。被禁的是「拿到设备 token 的人不该借后端摸到」的地址：

| 规则 | 覆盖 |
|------|------|
| 链路本地 | `169.254.0.0/16`（AWS/GCP/OpenStack 元数据）、`fe80::/10` |
| 未指定 / 组播 / 保留 | `0.0.0.0`、`::`、`224.0.0.0/4`、`ff00::/8` |
| 各家云元数据常量 | `100.100.100.200`（阿里云）、`fd00:ec2::254`（AWS IPv6） |

两条不可省的实现约束：**先解析域名再判**（否则一条指向 `169.254.169.254` 的 A 记录、或 `metadata.google.internal` 这类名字就能绕过字符串黑名单，域名解析出的**每个**地址都必须通过），以及**按已校验的 IP 拨号**（`netguard.Dial` 固定目标，消掉校验与连接之间的 DNS rebinding TOCTOU）+ **不跟随任何重定向**（`CheckRedirect` 返回 `http.ErrUseLastResponse`，一跳 302 也不能把合法目标换成被禁目标）。接入点：`/api/intel/features/test`、`/api/intel/contracts/check-batch`（`intelCheckBaseURL`）、测试环境探测与 `adb connect`（`internal/intel/envagent`）、远程构建节点可达性（`/api/intel/nodes`）。

### 测试计划与执行

- `GET /api/intel/plan?projectId=3` → `{"plan":[{"moduleId","relPath","kindType","kindRole","buildCommand","testCommand","priority","reason"}]}`（只读）。排序权重 `priority = 角色权重×100 + 该模块最近失败用例数`，角色权重 backend/bff=3、web/app/android=2、ios=1；解析不出测试命令的模块不入计划。命令优先取项目白名单里的同工具命令；**计划里的 Go 测试命令缺 `-count=1` 就整条覆写成 `go test -json -count=1 ./...`**（不保留白名单原文，否则命中缓存或无逐用例输出）。构建命令默认 `go build ./...` / `mvn package` / `./gradlew build`（Android `assembleDebug`）/ `npm run build`。
- `POST /api/intel/plan` `{"projectId":3}` → `{"run":{…},"plan":[…]}`：建一条 `scope=plan` 聚合 run 立即返回，每模块各一条 `scope=plan` 子 run；每模块先构建后测试，构建失败即标该模块失败并跳过测试。同项目串行、全局并发 2、整轮 30min。
- `POST /api/intel/run` `{"projectId":3,"moduleId":0,"node":0,"force":false}` → 200 `{"run":{…}}`（`status=queued`、`scope=module`）。`moduleId` 省略/0 = 根模块（`relPath="."`，没有则取第一个）。`node>0` 经 SSH 把命令发到远程执行节点，此时要求该节点 `reachable` 且**只支持 go 报告**，命令或工作目录含 shell 元字符直接拒绝。`force=true` 跳过环境门禁；两处的门禁条件都是 `node == 0 && !force` —— **走远程节点的 run 一律不评估门禁**（本机探测到的 ready 对远端无意义，改依赖节点 `reachable`），不只是 force 才跳过。门禁实际评估**两次**：入队前同步一次（缺项 → 500 `run failed: 环境门禁未通过，缺失项：…`，不建 run 行），异步执行前 `runIntelTests` 再评估一次（排队期间环境退化 → 该 run 标 `failed`，原因写进 `progress`，同样不是 HTTP 错误）。
- `POST /api/intel/run-all` `{"projectId":3,"force":false}` → `{"run":{…}}`，`scope=all` 聚合 run + 每模块一条 `scope=module` run。
- `GET /api/intel/runs?projectId=3` → `{"runs":[TestRun]}`（`createdAt` 倒序，含排队中的；`progress`/`output` 为 `omitempty`）。
- `GET /api/intel/runs/{id}` → `{"run":…,"results":[TestResult]}`；404 → 不存在。`POST /api/intel/runs/{id}/cancel` → `{"ok":true}`；仅仍在执行的 run 可取消，否则 404 `run not running or already finished`。
- `GET /api/intel/results/{id}`（也接受 `…/{id}/rootcause`）→ `{"result":…,"rootcause":…}`；通过或没跑归因时 `rootcause` 为 `null`；404 → 不存在。

执行侧固定行为：命令按构建工具推导（默认 `go test -json -count=1 ./...` / `mvn test` / `./gradlew test`（Android 为 `testDebugUnitTest`）/ `npm test`），argv 直跑**不经 shell**；项目命令白名单里有同工具的条目时**整条替换**默认命令（Go 单模块 run 不再补 `-count=1`，白名单条目自己漏写就会命中 Go 缓存 —— 只有计划路径 `plan` 强制补齐）。go 报告优先按 `-json` 事件流解析，解析不到时回落经典文本的 `--- PASS|FAIL|SKIP: 名 (0.12s)` 行；Java 读 surefire XML，但只在 `<模块目录>/target/surefire-reports/TEST-*.xml` 找（Maven 布局）—— Gradle/Android 写在 `build/test-results/`，因此这类模块跑完**解析不到逐用例**，只剩退出码决定的 run 状态；`npm` 无逐用例报告时合成一条整体结果（退出码非零时该条记为失败）。**测试框架用退出码表达失败**，所以非零退出不再中断解析——此前它会让最需要逐用例的失败运行反而一条都没有。但若退出码非零而一个失败用例都没解析出来（编译失败、命令本身报错、崩在报告之前），运行仍标 `failed` 而不是 `passed`。Go 失败用例**固定重跑 1 次**（重跑命令恒带 `-count=1`），重跑通过即标 `flaky`（仍算通过、不建缺陷）；失败结果自动转成 `kind=bug`、`severity=medium` 的 issue。

聚合 run（`scope=all` / `scope=plan`）的收尾状态由各模块结果派生：有失败 → `failed`，只有中断/取消 → `error`，全通过或无可测模块 → `passed`；明细始终在 `progress`/`output` 的「N 通过 / N 失败 / N 错误」摘要里，逐模块状态看各自的 `scope=module`/`scope=plan` 子 run。

### 缺陷 · 审计 · 修复

- `GET /api/intel/issues?projectId=3&status=open` → `{"issues":[IntelIssue]}`；`status` 为可选精确过滤。
- `GET /api/intel/findings?projectId=3&status=&detector=` → `{"findings":[IntelFinding]}`。`detector` ∈ `rule`（合规）/ `security`（实体敏感字段）/ `ai-rule`；`status` ∈ open/false_positive/waived/fixed。每轮分析后**闭环**：同 detector 下本次未命中的 open finding 自动置 `fixed`（用户显式 waive 的不受影响）——`ai-rule` 族没有这条闭环，规则收紧后旧告警会长期残留。排序是 `severity DESC, created_at DESC`，而 `severity` 是**自由文本**，按字典序排：`medium` 排最前、`critical`/`high` 垫底，前端不能靠列表顺序做优先级展示；无分页无 limit。`moduleId` 只有 `security` 族填，`rule`/`ai-rule` 恒 0。
- `POST /api/intel/findings/{id}` `{"status":"waived","reason":"只读内网接口"}` → `{"ok":true}`。`status` 只接受 `false_positive|waived|open`（否则 400）；非 `open` 时 `reason` 必填（400）。**豁免可撤销**：没有独立 unwaive 端点，传 `{"status":"open"}` 即回到待处理（此时 `reason` 可空，但给了照样写进 `waivedReason`）。写库只更新 `status`/`waived_reason`（`UpdateIntelFinding` 还会把 `removed_at` 一起写回，而 handler 传的是新构造的结构体，所以恒为 NULL —— 该列全仓没有生产者，`removedAt` 永远是 `null`），**id 不存在时也返回 `{"ok":true}`（无 404）**；路径只取第一段，`/8`、`/8/waive`、`/8/任意后缀` 完全等价。
- `GET /api/intel/fixes?projectId=3&status=proposed` → `{"fixes":[IntelFix]}`。
- `POST /api/intel/fixes/generate` `{"projectId":3,"findingId":8}` → 200 `{"fix":{…,"kind":"ai-suggest","status":"proposed","diffJson":…}}`（2min）。读 `finding.location` 指向的源文件（截 100 KiB）让模型出最小改动，`oldText` 必须在文件中**唯一命中**、`newText` 截 4000 字，否则 500；未配 LLM 也是 500（`fixes/generate` 是本组唯一用 500 表达「没配 LLM」的端点）。finding 不存在、`location` 为空、源文件读不到同样是 **500 而非 404**。此步**只入库不落盘**，改代码要靠 apply；同一 finding 反复调用**不去重**，每次多一条 `proposed`。`diffJson` 是 `[{file,oldText,newText,line,confidence}]` 的 JSON 文本，需二次解析。
- `POST /api/intel/fixes/{id}/{action}`，`action` ∈ `apply|reject|rollback` → `{"ok":true,"status":"applied|rejected|rolled_back"}`。`apply` 要求当前是 `proposed`（否则 400），会把 `diffJson` 里每处 `oldText→newText` **真实写回被测项目工作树**（0644 覆写，逐条校验 `oldText` 与当前内容匹配，路径越出项目根/绝对路径直接拒绝），原文件内容整体存进 `appliedBackup` 供 `rollback` 使用（要求状态是 `applied`，无备份 → 500）；apply 成功后关联 finding 置 `fixed`。多文件中途失败**不会自动还原已写入的文件**。`reject` 只改状态、不碰文件也**不回退关联 finding**；`rollback` 还原文件但**不把 finding 从 `fixed` 改回 `open`**。路径不是恰好 `{id}/{action}` 两段 → 400；404 → fix 不存在（**动作校验在取行之后**，不存在的 id 配错动作会先吃 404）。三个动作都不触发索引重建、不重跑分析。

### 覆盖度校正与 AI 规则

校正层（override）是自动分析与人工拍板之间的差分表：`target` + `rowKey`（自动行的自然键）+ `field` 定位一行，`status` ∈ `pending|applied|superseded|rejected`，只有 `applied` 会在读取时覆盖自动值。**读取侧只认三种组合** —— `module` × `relPath` × `kind_role`（或旧写法 `role`）/ `summary`、`endpoint` × `"METHOD path"` × `summary`、`feature` × `<featureId>` × `name`；写入侧（`overrides`、`overrides/enqueue`、`pending/{id}/confirm`）对 `target`/`field` **不做白名单校验**，存别的组合只会静默不生效（`env_requirement` 之类根本没有读取方）。

- `GET /api/intel/pending?projectId=3` → `{"pending":[IntelOverride]}`（只含 `status=pending`，id 倒序）。**分析不会自动往里塞分歧行**：pending 只来自 `overrides/enqueue`。
- `POST /api/intel/pending/{id}/confirm` `{"manualValue":"订单服务","status":"applied"}` → `{"override":…}`。`/confirm` 后缀可选（`/api/intel/pending/5` 等价，`/5/reject` 这类其它后缀因解析失败 → 400 `invalid override id`）。`manualValue` 省略或 `null` 保留原值（显式 `""` 会清空）；`status` 只认 `applied|rejected`，其他值**包括完全不传**一律按 `applied`；404 → 不存在。**副作用只有 `UPDATE intel_overrides` 的三列**：不改模块/接口/功能点表、不改 findings、不重建索引——覆写是在**读取时**合并回自动值的，所以天然扛得住重扫。
- `POST /api/intel/overrides` `{"projectId":3,"overrides":[{moduleId,target,rowKey,field,manualValue,confidence,source}]}` → `{"applied":<n>}`。整批直接以 `status=applied` 落库（人工最后拍板，不进待确认队列）；`manualValue` 为空的条目跳过，单条写库失败只影响计数不报错、**也不回滚已成功项**；`confidence` 默认 `high`、`source` 默认 `manual`。走的是 INSERT 而非自然键 upsert，**同 `(projectId,target,rowKey,field)` 重复提交会累积多行**（与 `PUT /api/intel/modules/{id}/overrides` 的 upsert 语义不一致）。
- `POST /api/intel/overrides/suggest` `{"projectId":3,"instruction":"把 gateway 模块的角色改成网关"}` → `{"drafts":[{target,rowKey,field,autoValue,manualValue,reason}]}`，**不落库**。需要编排 LLM，且**可用性检查在解析 body 之前** → 未配置时任何请求都是 400 `orchestration LLM is not configured`（body 非法也吃这条）；模型调用失败是 500。确定性闸门丢弃 `target`/`field`/`manualValue` 为空的草稿并逐字段截断（`manualValue`≤200、`reason`≤300 字），**最多返回 20 条**。上下文只有项目模块清单（`relPath`/`kindRole`/`kindType`）。
- `POST /api/intel/overrides/enqueue` `{"projectId":3,"drafts":[{target,rowKey,field,autoValue,manualValue,confidence}]}` → `{"queued":<n>}`。以 `status=pending`、`source=llm-suggest`（**不可指定**）入待确认队列，`confidence` 默认 `medium`；与 suggest 同形但**不依赖 LLM**，可手工灌。
- **没有读「全量已生效覆写」的端点**：对外只有 `GET /api/intel/pending`（pending 子集），`applied` 行只能从被覆写的资源读回来。`status` 的 `superseded` 也没有生产者。
- `GET /api/intel/ai-rules?enabled=true` → `{"rules":[IntelAIRule]}`；只有字面 `enabled=true` 才过滤，其余值返回全量；排序 `enabled DESC, sort_order ASC, id ASC`。规则是**全局**的，不按 projectId 隔离。
- `POST /api/intel/ai-rules` `{"name","prompt","scope":"all|affected","target":"risk|performance|compliance","severity":"high|medium|low","enabled":true,"sortOrder":0}` → 200（不是 201）`{"rule":…}`；`name`+`prompt` 必填，其余有默认值。`enabled`/`sortOrder` 用指针区分「不传」，**想关掉规则必须显式传 `false`**。
- `PUT /api/intel/ai-rules/{id}` 同上传语义（空字段不覆盖，想清空只能换成新值）→ `{"rule":…}`；`DELETE /api/intel/ai-rules/{id}` → `{"ok":true}`，但**不会带走已经生成的 `detector=ai-rule` finding**（残留要自己去 findings 那族 waive）。该子树只注册 PUT/DELETE：`GET /api/intel/ai-rules/{id}` → 405，多余后缀被忽略（`/{id}/任意` 按 `/{id}` 处理）。
- `PUT /api/intel/ai-rules/{id}/polish` → `{"polished":"改写后的提示词"}`，**不落库**（`polishedFrom` 也不写），确认后由调用方自己 PUT 保存；**只有 PUT**（`POST …/polish` → 405）；未配 LLM → 400。校验顺序：非法 id 400 → 方法 405 → 未配 LLM 400 → 规则不存在 404 → 模型失败 500。
- `POST /api/intel/scan/rules` `{"projectId":3,"ruleIds":[1,2]}` → `{"created":<n>,"rules":<m>}`。`projectId` 校验（400）**先于** LLM 检查（未配 → 400）；`ruleIds` 省略 = 跑所有启用规则，显式传值取「启用 ∩ 传入」，指向未启用规则的 id 静默丢弃；无可执行规则 → `{"created":0,"rules":0}`（200 不报错）。**不依赖向量检索**：直接在内存里复用知识片段（每规则最多 60 段、单段截 2000 rune、规则×片段扁平化后 4 并发），5min 同步；要先有 analyze 结果，否则 `rules>0`、`created=0`。命中即写 `detector=ai-rule` 的 finding（按 项目+规则+位置 去重，**重复命中不更新已有文案**）。`created` 计的是「模型判命中且写库未报错」的次数，**命中已存在的同一 finding 也计入**，不等于新增行数。单段 LLM 失败只记服务端日志、不影响整体返回。`scope` 字段目前无消费方。

### 环境供给与设备

- `POST /api/intel/env/ensure` `{"projectId":3}` 与 `GET /api/intel/env/status?projectId=3` 返回同一份结构（差别只在传参方式），两者都会**重新探测并回写**状态（2min 上限）：

```json
{ "requirements":[ { "service":"mysql", "category":"middleware", "version":"8.0", "source":"auto" } ],
  "services":[ { "service":"mysql", "category":"middleware", "provider":"docker", "status":"ready",
                 "host":"127.0.0.1", "port":3306, "healthy":true, "containerName":"…",
                 "username":"root", "endpoint":"127.0.0.1:3306" } ],
  "dockerReady":true, "dockerNotice":"", "ready":3, "missing":1, "unsupported":0,
  "portHint":"中间件连接串以 127.0.0.1 配置的端口为准" }
```

  `category` ∈ `middleware|toolchain`，`status` ∈ `ready|missing|unsupported`；`services[].password` 标了 `json:"-"`，永不回显。`ensureEnv` 同时是 `run` 门禁的输入。

- `POST /api/intel/env/install` `{"projectId":3,"service":"mysql"}` → `{"service":…}`（5min）。中间件走容器供给，`service` 不在内置清单里 → 400 `该服务不支持容器自动供给`。工具链（`jdk|node|go|gradle|maven`）需要 root 或免密 sudo：拿不到 → 400 且文案末尾给出可复制的 `sudo …`；有 → 直接 argv 执行 `apt-get`（**不经 shell**，`service` 不在白名单时 400 `该工具链暂不支持自动安装（请手动安装）`），成功响应额外带 `elevated` 与 `command` 两个字段；供给失败 → 500。
- `POST /api/intel/env/stop` `{"projectId":3,"service":"mysql"}` → `{"ok":true}`，该项状态重置为 `missing`。
- `POST /api/intel/env/external` `{"projectId":3,"service":"redis","host":"198.51.100.20","port":6379,"username":"","password":""}` → `{"service":…}`（`provider=external`）。`projectId/service/host/port` 缺一 → 400；只探一次 TCP 可达性决定 `ready|missing`；外部实例之后的重探测不会被容器逻辑覆盖，凭据只入库不回显。
- `POST /api/intel/env/schema-init` `{"projectId":3,"service":"mysql"}` → `{"scripts":[{"rel","ok","error"}],"executed","total","container","dbName"}`（10min）。`service` 默认且仅支持 `mysql`（其他 400）；对应环境项必须已 `ready` 且有容器名，否则 400；脚本从 Flyway/Liquibase/`*.sql` 自动发现，发现不到时返回 200 `{"scripts":[],"executed":0,"note":"未发现 SQL 迁移脚本（Flyway/Liquibase/*.sql）"}`（此时**没有** `total/container/dbName` 三个字段）；有的话逐个经 `docker exec -i [-e MYSQL_PWD=…] <容器> mysql -uroot` 灌入，单脚本失败不中断（逐条报 `ok/error`，`executed` 只数成功的）。口令走 `MYSQL_PWD` 环境变量而非 `-p<口令>` argv（后者任何本机进程都能从 `ps`/`/proc` 读到）。用户固定 `root`，`services[].username` 在这里不生效。
- `GET /api/intel/env/devices?projectId=3` → `{"devices":[IntelDevice]}`（按 `serial` 升序）。`projectId` **可选**，省略或非法即返回全部设备；只读设备表，不做 adb 实时枚举。
- `POST /api/intel/env/devices/connect` `{"ip":"192.0.2.30","port":5555}` → `{"device":…}`（`method=wireless`、`bound=false`，落库后再查回 id）。`ip` 必填（400）、`port` 缺省 5555；adb 连接或枚举失败 → 500。
- `PUT /api/intel/env/devices/{id}/bind` `{"projectId":3}` → `{"device":…}`（`bound=true`，只有人工绑定才变）；`DELETE /api/intel/env/devices/{id}` → `{"ok":true}`；其余方法 405、非法 id 400。

### 远程执行节点

- `GET /api/intel/nodes` → `{"nodes":[RemoteNode]}`（id 倒序）。
- `POST /api/intel/nodes` `{"name","host","port":22,"user","auth","capabilities":"linux-docker","workDir","note"}` → `{"node":…}`。`name`+`host` 必填（400），`port` ≤0 回落 22；创建时做一次 3s TCP 探测写 `reachable`/`lastCheckAt`；`auth`（私钥/口令）`json:"-"`，只入库永不回显。
- `PUT /api/intel/nodes/{id}/check` → 重新探测并回写 → `{"node":…}`；`DELETE /api/intel/nodes/{id}` → `{"ok":true}`；其余方法 405（不支持 `GET /api/intel/nodes/{id}`）。
- 节点由 `POST /api/intel/run` 的 `node` 字段引用：SSH 执行目前只支持 go 报告（stdout 自包含），命令与工作目录含 shell 元字符一律拒绝（远端经 login shell 解释）。

## 错误码汇总

| 状态码 | 含义 |
|--------|------|
| 400 | 参数错误 / 密码太短 / dependsOn 无效 |
| 401 | 未授权（Web Session 或 Token 无效/已撤销） |
| 403 | Web 管理端点缺少/错误 `X-Web-Session`；APP Token 单独调用 `/api/opencode/**` 的高危上游路径；未配置 `--webhook-secret` 时 `POST /api/webhook` |
| 404 | 资源不存在 |
| 409 | 任务已结束无法取消 |
| 429 | 登录尝试过多（同 IP 5 分钟内失败 5 次） |
| 500 | 服务内部错误 |
| 502 | 镜像代理的上游 OpenCode 请求失败；`POST /api/tasks/generate` 模型生成失败 |
| 503 | 需要 PostgreSQL + pgvector 的 RAG 端点（`POST /api/intel/ask`、`POST /api/intel/index`）跑在 SQLite 部署上 |

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
