# 快速上手

5 分钟跑通：安装 → 登录配置 → 生成 Token → 提交任务 → 收到推送。

## 1. 启动

> **⚠️ 部署安全默认值（公网/共享网络暴露前必读）**
> 本服务有意提供"开箱即用"的宽松默认，直接暴露到公网是危险的：
>
> 1. **默认管理密码是 `admin`**。`internal/config/config.go` 中
>    `--default-admin-password` 的默认值为 `envOr("STARBURST_ADMIN_PASSWORD", "admin")`，
>    **仅首次初始化**生效：`auth.Initialize` 见 `admin.password_hash` 已存在就直接返回，
>    已初始化过的库再改这个 flag 无效。且首启只 `log` 一条 "initialized admin password is
>    weak" 警告、**不阻止弱口令启动**（`mustHash` 绕过强度校验）；强度规则（≥8 位且不在常见
>    弱口令表内）只在你后续通过配置页/`/api/web/password` 改密码时由 `SetPassword` 强制。
> 2. **监听是纯 HTTP，不带 TLS**。`--listen` 默认 `:18880`（`STARBURST_LISTEN`）绑定所有网卡，
>    登录口令、Token、`/api/ws` 推送全部明文传输。
>
> 暴露到任何非本机网络前，按顺序做完这两件事：
>
> - **改密码**：首次启动就用环境变量传强口令（`STARBURST_ADMIN_PASSWORD='<强口令>'`，
>   不要把口令写进命令行/脚本/文档），或启动后立即在配置页改；已用默认口令初始化过的库，
>   走配置页「修改管理密码」（`/api/web/password`）改掉。
> - **加 TLS**：`--listen 127.0.0.1:18880` 只绑回环，由 Nginx/Caddy 一类反向代理终结 TLS 后再
>   转发（证书与域名按部署环境配置，例：`https://<host>:<port>`）。同时按需收紧 Token
>   （`/api/tokens` 撤销失陷设备）与 `--webhook-secret`。
>
> 说明：APP 端是否允许明文 HTTP 属于移动客户端仓库的配置，不在本仓范围内；本仓要守的就是
> 上面这两条。

```bash
go build -o starburst-backend ./cmd/starburst-backend

# 后续步骤要让手机/其它设备连上来（默认 :18880 = 绑定所有网卡）：
#   口令走环境变量，不写进命令行；对外暴露必须先套 TLS 反代
STARBURST_ADMIN_PASSWORD='<强口令>' ./starburst-backend --listen :18880

# 只在同一台机器上自测时，收紧到回环更安全：
#   STARBURST_ADMIN_PASSWORD='<强口令>' ./starburst-backend --listen 127.0.0.1:18880

# 两个变量都不传也能起（--listen :18880 + 初始口令 admin），但请先读上面那块警告
```

> **默认存储是 SQLite 单文件**（`--db` 缺省 `sqlite`，落在 `--sqlite-path`，默认
> `starburst-backend.db`），零外部依赖即可跑通本指南的全部步骤。**唯一不含在内的是测试智能**
> （`/api/intel/**`）：它要求 `/api/system` 上报 `vectorCapable=true`，而这需要同时满足两条 ——
> ① 换到 PostgreSQL，且该库能 `CREATE EXTENSION vector`（`--db postgres --pg-dsn '...'` 或
> `STARBURST_PG_DSN`；扩展装不上整个迁移就失败，不留半可用状态）；② 配好 embedding 服务
> （`--embed-url` / `--embed-key` / `--embed-model`，如 `bge-m3`，向量维度固定 1024）。
> SQLite 部署下配置页会直接隐藏测试智能入口，绕过页面调 `/api/intel/ask` 得到的是 **503**，
> 不是残缺可用的功能。

确认运行：`curl http://localhost:18880/api/health`
```json
{"status":"ok","upstream":true,"upstreamError":"","time":"..."}
```
`upstream:true` 表示已连上本机 OpenCode（默认 127.0.0.1:4096）。

## 2. 打开配置页

浏览器访问 `http://<主机>:18880/`，用你在 §1 设置的口令登录（若一直用默认 `admin`，登录后**第一件事**就是改密码）。

配置页共 11 个入口（左栏分三组）：
- **控制台**：AI 工作台（会话列表 + 对话/工具/授权/待答问题，与安卓端同一套上游接口）、任务、编排、实时流
- **开发**：项目 / 会话、自动化规则、会话归档
- **系统**：智能测试、审计日志、Token 管理、设置（含修改管理密码）

顶栏右侧可切换外观：跟随系统 / 浅色 / 深色 / 纯黑 AMOLED / 柔和 Dim，选择记在浏览器本地，取值与安卓端主题模式一致。

> 生产环境务必的事项见 §1 的「⚠️ 部署安全默认值」：改默认密码、`--listen` 绑回环或加防火墙、
> 初始口令走 `STARBURST_ADMIN_PASSWORD` 而非命令行明文、对外一律经反向代理加 TLS。

## 3. 用 Token 调 API

```bash
TOKEN=ocb_xxxxxx

# 查看本机会话
curl http://localhost:18880/api/projects -H "Authorization: Bearer $TOKEN"

# 提交一个后台任务（熄屏也会跑）
curl -X POST http://localhost:18880/api/tasks \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"prompt":"检查当前 git 仓库状态并总结","directory":"/workspaces/opencode"}'

# 查看任务
curl "http://localhost:18880/api/tasks?status=running" -H "Authorization: Bearer $TOKEN"
```

## 4. 接收推送

任何 WebSocket 客户端订阅 `ws://<主机>:18880/api/ws?token=<TOKEN>` 即可实时收到：

```json
{"type":"subscribed"}
{"type":"task.event","payload":{"id":"task_...","status":"running"},"severity":"info"}
{"type":"task.event","payload":{"id":"task_...","status":"succeeded"},"severity":"info"}
{"type":"task.event","payload":{"id":"task_...","status":"blocked","upstream":"task_...","reason":"前置任务失败"},"severity":"warning"}
{"type":"upstream.health","payload":{"healthy":true,"time":"..."}}
```

Python 快速试：
```bash
pip install websockets
python3 -c "
import asyncio, websockets, sys
async def m():
    async with websockets.connect('ws://localhost:18880/api/ws?token=$TOKEN') as ws:
        async for msg in ws:
            print(msg)
asyncio.run(m())
"
```

## 5. 配置自动化规则

配置页以管理员登录后（或调用 `/api/rules`）：

```bash
# 每晚 23:00 跑一次构建检查（cron 是 6 字段、秒在最前：秒 分 时 日 月 周）
curl -X POST http://localhost:18880/api/rules \
  -H "X-Web-Session: <sid>" -H "Content-Type: application/json" \
  -d '{"name":"每晚构建","kind":"cron","schedule":"0 0 23 * * *","directory":"/workspaces/opencode","prompt":"跑构建并汇报结果","enabled":true}'
```

> 写错字段数或把 `0 23 * * * *` 当成「23:00」是最常见的坑：那样其实是**每小时的第 23 分**跑一次。
> 日和周同时给值时是 **AND**（`0 0 0 13 * 5` 只在黑色星期五触发），不支持经典 cron 的「或」语义。

## 6. 批量执行

```bash
curl -X POST http://localhost:18880/api/batch \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"prompt":"为所有 handler 添加日志","targets":[{"directory":"/repo-a"},{"directory":"/repo-b"}]}'
# → {"created":["task_..."],"count":2}
```

## 7. 排查

| 现象 | 处理 |
|------|------|
| `upstream:false` | 本机 OpenCode 没起，或 `--opencode-url` 不对 |
| 401 invalid token | Token 错/已撤销；确认 `Authorization: Bearer ` 前缀 |
| 任务一直 queued | 检查日志；上游 agent 忙会排队，任务有自动重试 |
| PG 报语法错 | 确保代码里 SQL 走 `s.q(...)`（`?`→`$N`） |
| 忘记管理密码 | 停服务 → 删掉 `settings` 表里 `key='admin.password_hash'` 那一行 → 带 `STARBURST_ADMIN_PASSWORD='<新口令>'` 重启，`auth.Initialize` 会重新写入。注意 `--health-check` **只做连通性自检然后退出，不会删库也不会重置口令** |
