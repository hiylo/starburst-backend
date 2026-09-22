# RAG-in-Prompt 知识库检索注入设计

> 本文档描述在 StarBurst Backend 中新增的 **RAG-in-Prompt** 能力：让 OpenCode 聊天自动携带
> 本地知识库上下文。用户在会话里发消息时，后端在镜像代理层对 `prompt_async` 请求做知识库检索
> （`/api/kb/search`），**命中相关片段就拼进 prompt 再转发，没命中就原样转发**——对 OpenCode
> 上游与客户端均零侵入。
>
> 设计原则：
> - **单一规则、后端拼装**：判断「找得到 / 找不到」只在 Go 后端做一次，App / Web 各自只有一条
>   发送路径，不因后端有无而分裂成两套行为。
> - **RAG 是锦上添花，绝不阻塞发送**：检索失败、超时、无知识库都不得影响原始消息投递；
>   用户消息永远是「找到就拼、没有就原样走」。
> - **不改上游**：OpenCode 服务端与聊天协议不动，上下文以独立文本 part 注入，来源带
>   provenance（文档名 + 章节 + 相关度），可追溯。
>
> **阅读须知**：本文是**设计文档**，描述目标形态而非当前实现。文中标有 **「⚠️ 实现现状」**
> 的引用块是与代码核对后的更正，凡与正文冲突以标注为准。v2.1.0 起知识库子系统（`/api/kb/*`）
> 与代理层 RAG-in-Prompt 拼装已在 Go 后端落地，正文中原来的「计划中」描述均已实现，
> 详见 [`docs/API.md`](API.md) 与 [`docs/ROADMAP.md`](ROADMAP.md)。

## 1. 背景与目标

### 1.1 要解决的问题

1. **知识库概念缺失**：OpenCode 会话只感知当前项目目录，无法自动引用团队沉淀的文档
   （需求 / 报价单 / 管理制度 / 排期表等 PDF、Word、Excel）。
2. **手动搬运成本高**：用户要引用资料先得自己打开文件、复制内容、贴进输入框，附件超限还会
   被上游拒收。
3. **防幻觉**：模型回答业务问题时，若有相关知识片段兜底（带来源），引用可直接落到具体文档，
   而非编造。

### 1.2 目标形态

```
用户在会话里发消息（App 或 Web，路径一致）
  → App 照常 POST /api/opencode/session/{id}/prompt_async（fire-and-forget）
  → Go 镜像代理拦截 prompt 请求体
  → 取用户最新文本 → /api/kb/search（embedding + pgvector Top-K）
      命中且过阈值  → 把知识库上下文块拼进 parts 最前，再转发上游
      未命中/降级    → 转发原始请求体，一字不改
  → OpenCode 收到并处理（上下文与提问同属一条用户消息）
```

### 1.3 为什么放在 StarBurst Backend

后端已具备：连接本机 OpenCode 的镜像代理（`/api/opencode/*`）、可插拔 embeddings 客户端
（`internal/embed`，OpenAI 兼容）、pgvector 向量库（`intel_chunks`）、LLM 编排、审计与推送。本能力
只需在代理层加一个检索+拼装步骤，不引入外部服务、不改上游。

## 2. 决策：Go 端拼装（单一规则）

### 2.1 对比方案：安卓端拼装

| 环节 | 安卓端拼装 | Go 端拼装（选此） |
|------|-----------|------------------|
| 规则数量 | 两条：有后端时先 `/api/kb/search` 再拼；无后端时跳过 | **一条**：收到请求 → 检索 → 找到就拼、没找到就原样转发 |
| 双端一致 | App 与 Web 各写一遍拼装 | 两端零改动，一起受益 |
| 与后端可用性耦合 | 依赖 App 侧网关判定（`OpenCodeGateway.resolve`） | 与后端可用性天然同源，无额外分支 |
| 检索往返 | 发送前 App 多发一次请求（可感知延迟） | 在代理内完成，App 无感 |

选择 Go 端拼装，理由：**单一规则**是首要考量——判断「有没有命中」只在一个地方发生，App / Web
各只有一条发送链路，不会出现「一个环境会检索、另一个不会」的分叉行为。App 现在后端可用时本来就
走 `/api/opencode/*` 代理（`OpenCodeGateway.resolve`），拼装位于代理正好覆盖该路径。

> **⚠️ 实现现状**：`/api/kb/search` 与 §5–§7 描述的代理层拼接逻辑已在 v2.1.0 落地
> （`internal/server/kb.go` + `kb_rag.go`），检索后端经通用 `kb_collections / kb_documents /
> kb_chunks`（§8），不再依赖项目级 RAG。

## 3. 消息流程与时序

```
用户点发送
  App: sendParts() → POST /api/opencode/session/{id}/prompt_async
       （fire-and-forget，返回 204，App 不等待）
  Go:  opencode_proxy.go 拦截该路径的 POST
       1) 解析请求体 parts，取最后一条用户文本（其余 part 原样保留）
       2) embedding 检索 Top-K（超时上限 ragQueryTimeout，见 §7）
       3) 命中且过阈值 → 重写请求体：前置知识库上下文 part
       4) 未命中/降级     → 原请求体转发
  → opencode.Client.Do() 转发上游 /session/{id}/prompt_async
```

> **⚠️ 实现现状**：该「检索 + 可选重写 body」分支已在 v2.1.0 落地
> （`internal/server/kb_rag.go` 的 `applyRagSpliceToProxy`）：仅对
> `POST /session/{id}/prompt_async` 生效，未命中/失败/超时一律原样转发，不影响其它代理路径。

## 4. 三态逻辑

Go 端拼装层对外只呈现三种结果：

| 输入 | 判定 | 行为 |
|------|------|------|
| 检索命中且片段 >= 阈值 | 找到 | 拼装知识库上下文块，重写 parts 后转发 |
| 未命中 / 知识库为空 / 能力缺失 / 超时 / 全部低于阈值 | 没找到 | **原样转发**，与今天行为完全一致 |
| 上游转发失败 | — | 沿用代理既有错误处理，与 RAG 无关 |

核心不变量：**消息发送的成功率与延迟不因 RAG 而变差**。`prompt_async` 是 fire-and-forget，
所以「没找到」时用户端无任何感知差异。

## 5. 拼装格式

命中时，在原始 parts 最前面追加一个独立 text part，用户自己的文本 part **一字不改**：

```
[RAG_CONTEXT_START]
以下是知识库检索到的参考资料，供回答本次问题使用。
请优先依据资料作答；资料未覆盖的部分请如实说明，不要编造。
引用结论时可标注对应来源，例如 [来源1]。

[来源1] 《仓储管理制度.docx》 · 第 4 章 库存（相关度 0.93）
安全库存 = 日均出库量 × 备货周期 × 1.2，每月 1 日重新测算。

[来源2] 《12月库存报表.xlsx》 · Sheet2 汇总（相关度 0.84）
本月甲类 SKU 安全库存 12,000 件，实际 9,800 件，处于短缺预警状态。

[RAG_CONTEXT_END]
```

格式要点：

- **独立 part + 明确的 `[RAG_CONTEXT_START]/[RAG_CONTEXT_END]` 边界**：把「资料区」与
  「提问区」区分开，模型不会把资料当指令执行；用户原始文本 part 不动，会话历史与回显保持一致。
- **`[来源N]` + 文档名 + 章节 + 相关度**：模型可标注出处、降低幻觉，来源可追溯、可点击跳原文。
- **按相关度降序注入**：优先喂最相关片段。
- 预算与截断规则（与 App 现有上下文预算估算口径对齐）：
  - 按相关度贪心装填，放不下的片段正文追加 `…（超出上下文预算，已截断）`；
  - 头部说明 + 来源标签占预算约 30%，正文最多分到余下部分；
  - 预算内一个片段都放不下时整体跳过注入（空上下文块同样是噪音）。

> **⚠️ 实现现状**：Go 侧拼装已在 v2.1.0 落地（`internal/server/kb_rag.go` 的
> `ragSpliceJSON` / `ragPromptBlock`），格式与参数与本节一致——独立的 text part、
> `[RAG_CONTEXT_START/END]` 边界、`[来源N]` 标签、按相关度贪心装填 + 超预算截断。Kotlin 侧
> 曾在讨论中实现过同款拼装函数（`buildRagContextText` / `injectRagContext`），已回滚；若未来
> App 要展示引用卡片，两边语义一致。

## 6. 降级路径（没找到时）

「没找到」是一个集合，全部落到同一条路——**原样转发**：

| 情况 | 处理 |
|------|------|
| 检索返回空 Top-K | 原样转发 |
| 全部片段低于相关度阈值 `ragMinScore` | 原样转发，低分噪音不如不拼 |
| 知识库为空（从未 ingest） | 原样转发 |
| embeddings 未配置 / 无 pgvector（SQLite 部署） | 原样转发，沿用后端既有能力降级 |
| 检索超时 / embedding 服务异常 | 原样转发，且不阻塞 prompt 发送 |
| 预算内放不下任何片段 | 原样转发 |

不额外向模型注入「知识库里没有资料」之类的提示——模型不需要知道存在一个知识库。

## 7. 关键参数（Go 侧定义）

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `ragQueryTimeout` | 800ms | 检索上限，超时即按「没找到」原样转发，杜绝拖慢发送 |
| `ragMinScore` | 0.5 | Top-K 相关度阈值，低于即弃 |
| `ragTopK` | 5 | 单次检索片段数上限 |
| `ragBudgetTokens` | 1500 | 拼装块预算，超预算截断/丢弃 |

参数全部收敛在后端常量/配置里，App 与 Web 不感知。

> **⚠️ 实现现状**：上表参数已在 v2.1.0 收敛为 `internal/server/kb_rag.go` 的四个常量
> （`ragQueryTimeout`/`ragMinScore`/`ragTopK`/`ragBudgetTokens`），App 与 Web 不感知。

## 8. 与现有 RAG 底座的关系

- 复用：`internal/embed`（OpenAI 兼容 embeddings 客户端，可缺省短路）、pgvector 索引、
  `internal/server/rag.go` 的分块/检索/缓存模式（`ragRetrievalCache`、重建后失效）。
- 计划：把当前**项目级** `intel_chunks` 索引泛化为通用 `kb_chunks`（
  `kb_collections / kb_documents / kb_chunks`），新增 `/api/kb/*`：
  `ingest`（上传 PDF/Word/Excel/TXT/MD → 解析 → 分块 → embed）、`search`（Top-K）、`list/delete`
  。SQLite 无 pgvector时沿现有 `ErrRagUnsupported` → 503 提示，检索端按「能力缺失」降级原样转发。
- 文档解析边界：解析统一出口为纯文本 / Markdown，保证 PDF / Word / Excel 都能进同一条
  分块 → embed → 检索链路。

> **⚠️ 实现现状**：上段「计划」已在 v2.1.0 落地：`kb_collections / kb_documents / kb_chunks`
> 三表与 `/api/kb/*`（ingest / search / list / delete）已实现（`internal/store/kb.go` +
> `internal/server/kb.go`）；SQLite 无 pgvector 沿 `ErrRagUnsupported` → 503。解析器由
> `internal/doc`（pdf / xlsx / xls / csv / docx / pptx → Markdown）提供。

## 9. 可观测

- 代理响应头 `X-Rag-Spliced: 0|1`，供客户端（如后续要在 App 提示「未检索到相关资料」）零成本
  识别；不要时忽略。
- 日志/计数打点：命中数、未命中数、降级原因（空 / 低分 / 无向量库 / 超时 / 预算不足），
  便于评估知识库有效性。
- 命中场景计入审计链路，来源带 provenance（文档名 + 章节 + 相关度）。

> **⚠️ 实现现状**：`X-Rag-Spliced: 0|1` 已在 v2.1.0 落地（仅 `prompt_async` 响应携带）；
> 降级原因只落服务端日志并计入 `/api/kb/stats` 计数器（spliced / skipNoEmbedding /
> skipNoVector / skipTimeout / skipNoResult / skipBelowThreshold），Web 知识库页
> `kb.html` 有可视化面板；检索结果在进程内缓存（`kbSearchCache`，按查询+集合范围+topK+
> 阈值，KB 写操作全量失效）。命中场景的审计 provenance 未接入。

## 10. 未决问题

- [x] 检索与拼装是否按 collection 隔离：`--rag-collection-ids <逗号分隔>` 已落地
  （空 = 全库，见 `docs/ROADMAP.md`）；按会话目录自动绑定集合仍开放。
- [ ] `X-Rag-Spliced` 是否值得透传到 App UI 展示「已带入 N 条资料」。
- [ ] 知识库文档管理与图库 upload 走 Web 配置页还是仅 API。
- [ ] 多端知识库一致性：Web 已 ingest、App 路由本机时索引是否需先构建。

## 参考

- [`internal/server/opencode_proxy.go`](../internal/server/opencode_proxy.go) — 镜像代理与拦截点
- [`internal/server/rag.go`](../internal/server/rag.go) — 现有项目级 RAG 检索
- [`internal/embed`](../internal/embed) — 可插拔 embeddings 客户端
- [`docs/TEST_INTELLIGENCE.md`](TEST_INTELLIGENCE.md) — 测试智能 RAG 底座来源