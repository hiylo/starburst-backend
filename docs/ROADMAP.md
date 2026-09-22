# StarBurst Backend Roadmap

> 本文件是方向性规划，**非承诺**。优先级与时间可能调整。
> 状态标记：✅ 已发布 · 🚧 进行中 · ⏳ 计划中

## 版本历史

- **v2.0.1**（已发布）：webui 专注模式、系统通知、markdown/事件列细节打磨；与 App
  `REQUIRED_BACKEND_VERSION=2.0.1` 对齐。
- **v2.0.0**（已发布）：Test Intelligence 成为第三主能力（`/api/intel/*` M1–M8）。
- **v1.1.0**（已发布）：多设备同步 `/api/sync`、硬件告警 `/api/alerts`。
- **v1.0.0**（已发布）：任务 / 规则 / 归档 / 审计 / 统计底座。

## 2.1.0（进行中 🚧）— 知识库与文档能力

> 主题：文档相关能力整体上线——知识库、文档解析、RAG-in-Prompt 检索注入、文档生成。
> 对应设计文档：[`docs/RAG_PROMPT.md`](RAG_PROMPT.md)（检索注入）与
> [`docs/DOCUMENTS.md`](DOCUMENTS.md)（解析 / 生成 / 预览 / 迭代）。

### 知识库子系统 `/api/kb/*`

- [x] `kb_collections` / `kb_documents` / `kb_chunks` 表（复用 `internal/embed` + pgvector，
      把 intel 项目级索引泛化为通用知识库）
- [x] `ingest`：上传 PDF / Word / Excel / Markdown / TXT → 解析 → 分块 → embed
  （支持 `replace=true`：同名文档替换入库，避免重复）
- [x] `search`：Top-K 检索（复用 `internal/server/rag.go` 的检索与缓存模式）
- [x] `list` / `delete`（集合与文档管理，文档列表支持 `limit`/`offset` 分页 + `total`）
- [x] `PATCH /api/kb/collections/{id}` 重命名/改描述（重名 409、改名失效检索缓存）
- [x] KB 页检索测试支持按集合过滤（`collectionIds` 暴露到 UI）
- [x] SQLite 无 pgvector 时沿既有 `ErrRagUnsupported` → 503 提示降级
- [x] KB 检索结果进程内缓存（按查询+集合范围+topK+阈值），写操作全量失效

### 文档解析 `internal/doc`（四类全入 v2.1.0）

- [x] PDF 解析
- [x] Word（`.docx`）解析
- [x] Excel（`.xlsx` / `.csv`）解析
- [x] PPT（`.pptx`）解析
- [x] 统一出口：纯文本 / Markdown（喂给 kb 分块 → embed → 检索引擎链路）

### RAG-in-Prompt（Go 端拼装）

- [x] 代理层拦截 `POST /api/opencode/session/{id}/prompt_async`
- [x] 三态逻辑：命中过阈值 → 拼上下文再转发；没找到/超时/能力缺失 → 原样转发；转发失败 →
      沿用代理既有错误处理（`docs/RAG_PROMPT.md` §4–§6）
- [x] 拼装格式：`[RAG_CONTEXT_START/END]` + `[来源N]` + 预算截断（`docs/RAG_PROMPT.md` §5）
- [x] 关键参数后端化：`ragQueryTimeout=800ms`、`ragMinScore=0.5`、`ragTopK=5`、
      `ragBudgetTokens=1500`（`--rag-timeout` 可配）
- [x] 集合范围：`--rag-collection-ids <逗号分隔>` 限定自动注入的集合（空 = 全库）
- [x] 可观测：`X-Rag-Spliced: 0|1` + 命中/未命中/降级原因打点（`/api/kb/stats`，KB 页有可视化）

### 文档生成 `internal/doc`（LLM 出结构 + 确定性渲染）

- [x] 自然语言需求 → 编排 LLM 产出结构化 JSON 文档骨架（prompt 放 `prompts/` 可版本化）
- [x] 骨架与产物落库（`documents` 表 + JSON 骨架），供会话内重新生成复用
- [x] PPT 渲染（unioffice）→ `.pptx`
- [x] Word 渲染（unioffice）→ `.docx`
- [x] Excel 渲染（excelize/v2）→ `.xlsx`（含图表）
- [ ] 生成任务异步化 + 结果下载 / WS 推送（复用 `internal/tasks` + `internal/push`）

### 会话内文档预览（客户端渲染，双端复用同一套渲染页）

- [x] 预览渲染页（webui static）：`.docx`→mammoth.js、`.xlsx`→SheetJS、`.pptx`→pptxjs、
      `.pdf`→pdf.js，纯前端、无需后端转图
- [x] Web 工作台：聊天里文档附件卡片 → 预览弹窗 / iframe 内嵌
- [ ] Android：聊天里文档附件卡片 → WebView 弹层加载同一渲染页（指到文件 url）
- [x] 预览页支持翻页 / 放大 / 下载（各渲染器能力内）

### 聊天上下文驱动的重新生成 / 迭代

- [x] 生成文档作为会话附件 part 出现，携带 `docId` 映射（`-@doc<id>` 文件名约定 +
      `POST /api/documents/{id}/attach` 复制进工作目录 + Web 聊天发 `{type:"file",path}` part）
- [x] 文件卡片操作：「重新生成」（原 prompt 重跑）/「按意见修改」（LLM 按新意见修订）——
      Web 聊天对含 `-@doc<id>` 的附件卡片显示操作按钮，结果卡一键「发送到本会话」
- [x] `POST /api/documents/regenerate {docId, instruction?}`：原骨架 + 意见 + 最近会话上下文
      → LLM 产出新骨架 → 重渲染 → 以新消息附件替换 / 追加
- [x] 会话上下文取数复用 opencode 会话消息拉取（最近 N 条），随任务入审计

### 客户端

- [x] Web 工作台：知识库管理（ingest / list / delete / replace / 分页）+ 文档生成（含集合选择
      反向入库）+ 会话内预览 + 重新生成/按意见修改 + 发送到本会话
- [ ] App 侧（StarBurst）：知识库引用展示、文档生成、会话内预览（WebView 渲染页）与
      重新生成，见 starburst（App）仓库 ROADMAP 3.x

## Later — Backlog

- [ ] PDF 生成（Markdown → PDF，评审渲染方案：原生 Go 库 / 外部转换器）
- [x] 文档生成模板库（内置 PPT 发布会/周报/立项汇报、xlsx 报价单/漏斗/排期、docx 纪要/周报/汇报，
      KB 页「内置模板」下拉一键填充类型与需求描述）
- [x] 生成参考知识库：generate/regenerate 支持 `kbCollectionIds`，先检索知识库再出骨架，
      产物数字/表述贴合库内资料（未指定集合回退 `--rag-collection-ids`）
- [ ] 生成任务异步化 + WS 推送（复用 `internal/tasks` + `internal/push`；`executor.go`/`task.go`
      在并发 agent 未提交改动集内，等收口后做）
- [ ] 知识库集合与项目/会话目录的自动绑定（`docs/RAG_PROMPT.md` §10）
- [ ] 多端知识库一致性（Web ingest 后本机索引同步策略）