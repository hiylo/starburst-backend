# 快速上手

5 分钟跑通：安装 → 登录配置 → 生成 Token → 提交任务 → 收到推送。

## 1. 启动

```bash
go build -o startburst-backend ./cmd/startburst-backend
./startburst-backend --listen :18880 --default-admin-password admin
```

确认运行：`curl http://localhost:18880/api/health`
```json
{"status":"ok","upstream":true,"upstreamError":"","time":"..."}
```
`upstream:true` 表示已连上本机 OpenCode（默认 127.0.0.1:4096）。

## 2. 打开配置页

浏览器访问 `http://<主机IP>:18880/`，用默认密码 `admin` 登录。

配置页支持：
- **修改管理密码**
- **生成 APP Token**（给每台手机/设备生成一个，起个名字，Token 只显示一次，请保存）
- **任务列表**（填入 APP Token 后可查看/加载任务）
- **编排项目**（本机 OpenCode 的会话聚合）

> 生产环境务必：① 改默认密码；② 用 `--listen` 绑定到非 0.0.0.0 或加防火墙；③ 通过 `OCB_ADMIN_PASSWORD` 设置初始密码而非明文 flag。

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
# 每晚 23:00 跑一次构建检查（cron 秒级：分 时 日 月 周）
curl -X POST http://localhost:18880/api/rules \
  -H "X-Web-Session: <sid>" -H "Content-Type: application/json" \
  -d '{"name":"每晚构建","kind":"cron","schedule":"0 23 * * * *","directory":"/workspaces/opencode","prompt":"跑构建并汇报结果","enabled":true}'
```

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
| 忘记管理密码 | 停服务后 `--health-check` 删库重建，或改 DB `settings.admin.password_hash` |
