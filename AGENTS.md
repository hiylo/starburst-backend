<!--
Copyright(c) 2016 - Present, Clouds Studio Holding Limited. All rights reserved.
-->

# AGENTS.md — opencode-backend（StarBurst Backend）项目规约

## 项目概述

StarBurst App 的轻量独立后端（Go 单二进制），为 App/Web 提供编排、自动化、推送、测试智能、知识库/文档生成。App 可直连 OpenCode 也可走本后端镜像。默认监听 `:18880`。

- **module**：`github.com/hiylo/starburst-backend`
- **Version**：2.1.0（`internal/config.Version`）
- **路由**：98 条，`/api/intel/*` 占 56 条

## 技术栈

| 层 | 选型 |
|---|---|
| 语言 | Go 1.25.0 |
| Web 框架 | **标准库 `net/http`**（无框架，偏离项见 `docs/TECH_DEVIATION.md`） |
| WebSocket | gorilla/websocket v1.5.3 |
| 数据库 | SQLite（modernc.org/sqlite，CGO-free）/ PostgreSQL 16 + pgvector（可选） |
| 迁移 | 自研版本化迁移 47 步（`internal/store/migrate.go`），无 Flyway |
| 文档解析 | ledongthuc/pdf、xuri/excelize/v2、docx/pptx 自写渲染 |
| 前端 | **手写原生 JS 单页**（`go:embed` 打进同一二进制），无 package.json |
| LLM | OpenAI 兼容，走 LiteLLM 网关；prompt 在 `internal/server/prompts/*.prompt` |
| 部署 | Docker（golang:1.25-alpine → alpine:3.21，非 root user 10001） |

> **技术选型偏离声明**：本项目使用 Go 标准库 `net/http` 而非 Gin/Echo，自研迁移替代 Flyway，SQLite+pgvector 替代 MySQL。详见 `docs/TECH_DEVIATION.md`（待架构组评审）。

> **注意**：全局 Java 编码规范（Copyright 头、Javadoc、Lombok、SpotBugs、JUnit 5+Mockito）在本项目**无作用对象**。Go 代码遵循 Go 官方惯例。

## 架构 / 包结构

256 个 .go 文件 / 96 个 `*_test.go`。

```
cmd/starburst-backend/main.go        # 364 行，唯一入口
internal/
├── server/    (106) 路由+handler：handlers.go、tasks.go、workflow.go、stream.go、
│                events.go、intel_*.go（40+）、prompts/
├── store/     (47)  data access + migrate.go
├── intel/     (29)  测试智能核心（20 个子包）
├── doc/       (10)  pdf/ooxml/xlsx/docx/pptx 渲染
├── alerts/(6) automation/(4) config/(3) push/(3) tasks/(3) webui/(3)
├── auth/(2)  embed/(2)  llm/(2)  netguard/(2)  opencode/(2)
```

凭据模型双轨：Web Session（`X-Web-Session`，24h）与 APP Token（`Bearer ocb_*`，只存哈希）。

## 安全红线（提交 / 推送前必须自查）

1. **禁止提交以下内容**（写入 commit 视为事故，需重写历史）：
   - 内网/私有 IP（`10.x`、`172.16-31.x`、`192.168.x`）及内网域名
   - 明文口令、API Key、token（GitLab PAT、GitHub PAT、OpenAI `sk-`、AWS `AKIA` 等）
   - 私钥（`*.pem`、`id_rsa`、keystore、`.jks`）、`.env`、连接串内嵌凭据（`user:pass@host`）
2. **地址占位统一用 RFC 5737 文档网段**：`192.0.2.x` / `198.51.100.x` / `203.0.113.x`，或 `<host>:<port>` 变量形式
3. **git remote 禁止在 URL 内嵌 token**，改用 SSH 或 git credential helper
4. **本地提交**：已提供 pre-commit 钩子，新 clone 后执行一次 `git config core.hooksPath githooks`，之后每次 commit 自动运行 `scripts/check-secrets.sh`
5. **推送 GitHub 前**：手动跑 `scripts/check-secrets.sh --all` 复查全仓；CI 另有 `.github/workflows/secret-scan.yml`（gitleaks）兜底
6. 确认为误报需放行时：先整改为合规写法；实在无法避免才 `git commit --no-verify`，并在提交说明注明原因

## 构建与测试

```bash
# 构建
go build ./...                                          # CI 全量
go build -o starburst-backend ./cmd/starburst-backend   # 本地

# Release 交叉编译（6 平台）
go build -trimpath -buildvcs=false \
  -ldflags "-s -w -X github.com/hiylo/starburst-backend/internal/config.Version=$version" \
  -o "dist/starburst-backend-${GOOS}-${GOARCH}" ./cmd/starburst-backend

# 测试
go test ./...                                           # SQLite 全量
go vet ./...                                            # CI
go test -race ./internal/...                            # 并发回归
go test -tags pgtest -race -run Postgres -v ./internal/store/   # PG 专项（需 STARBURST_PG_DSN）

# 脚本检查
bash -n scripts/install.sh                              # 安装脚本语法
docker compose config                                   # compose YAML
```

### CI（GitHub Actions）
- `test`：vet + race + build
- `postgres`：pgvector 真实跑 + **显式拒绝 skip**（防静默跳过）
- `install-script`：bash -n + 沙箱安装
- `release`：6 平台矩阵 + sha256sum + `gh release create`
- `secret-scan`：gitleaks

## Git 规范

- 提交消息：`{type}({scope}): {subject}`，type：`feat`/`fix`/`refactor`/`docs`/`chore`/`test`
- 远端：`github`（SSH）、`gitlab`（内网，URL 中不含 token）
  > 安全红线中「禁止内网 IP」针对**版本库内容**（代码/配置/文档），本地 git remote 配置不在版本库中，可使用真实内网地址。

## 文档索引

| 文件 | 说明 |
|------|------|
| `docs/API.md` | API 文档（846 行） |
| `docs/TEST_INTELLIGENCE.md` | 测试智能（880 行） |
| `docs/TECH_DEVIATION.md` | 技术选型偏离说明（待架构组评审） |
| `docs/ARCHITECTURE.md` | 架构文档 |
| `docs/DOCUMENTS.md` | 文档生成说明 |
| `docs/RAG_PROMPT.md` | RAG Prompt 设计 |
| `docs/QUICKSTART.md` | 快速开始 |
| `docs/ROADMAP.md` | 路线图 |
