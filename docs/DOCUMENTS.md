# StarBurst 文档能力设计（解析 / 生成 / 预览 / 迭代）

> 本文档描述在 StarBurst Backend 中新增的**文档子系统**（`internal/doc`），随 v2.1.0 发布，
> 覆盖四类文档（PDF / Word / Excel / PPT）的解析入库、会话内生成、会话内预览，以及**由聊天上下文
> 驱动的重新生成 / 迭代**。范围与版本化见 [`docs/ROADMAP.md`](ROADMAP.md)；知识库检索注入见
> [`docs/RAG_PROMPT.md`](RAG_PROMPT.md)。
>
> 设计原则：
> - **LLM 只出结构，渲染由确定性代码完成**：生成链路以「JSON 文档骨架」为唯一中间格式，LLM
>   产出的只是骨架与文案，数值 / 计算 / 布局由渲染器确定性地落到文件；表格中的数据要么来自用户
>   输入，要么来自知识库检索结果。遵循 `/workspaces/AGENTS.md` 的关键数值铁律。
> - **单二进制无外部服务**：预览不依赖后端转图 / LibreOffice；三端（Web / Android / iOS 未来）
>   复用同一套**纯前端渲染页**。渲染与转换库优先 Apache-2.0，规避传染性许可（见 §10 未决问题）。
> - **生成是可迭代对象**：每次生成落库 JSON 骨架并回投会话成为附件，用户可在会话里点「重新
>   生成」或「按意见修改」——regenerate 带着原骨架 + 新的自然语言意见 + 最近会话上下文回到 LLM。
>
> **阅读须知**：本文是**设计文档**，描述目标形态而非当前实现。文中标有 **「⚠️ 实现现状」** 的
> 引用块是与代码核对后的更正，凡与正文冲突以标注为准；未决问题汇总见 **§10**。

## 1. 背景与目标

### 1.1 要解决的问题

1. **文档能力缺失**：现在 App / Web 会话里，`.docx` / `.xlsx` / `.pptx` / `.pdf` 附件只能显示
   一个文件名卡片，无法预览内容；知识库也只有"测试智能"项目级 RAG，团队文档进不去。
2. **生成靠用户手动**：做 PPT / 排期表 / 商务文档要离开会话到桌面包做，无法复用会话上下文。
3. **不可迭代**：文档生成是一次性的，用户想要"结合讨论再改一版"没有通路。

### 1.2 目标形态

```
解析：上传 PDF / Word / Excel / PPT → internal/doc 解析 → 纯文本 / Markdown
      → kb ingest（分块 → embed（pgvector））→ RAG-in-Prompt 检索注入

生成：会话里发需求 → /api/documents/generate → LLM 出 JSON 骨架 → 渲染器出文件
      → 产物作为会话附件 part（携带 docId）→ 用户在卡面预览 / 下载

迭代：卡片「按意见修改」→ /api/documents/regenerate {docId, instruction}
      → 原骨架 + 意见 + 最近会话上下文 → 新骨架 → 重渲染 → 新附件回投会话
```

## 2. 能力总览与模块划分

```
internal/doc                # 新子系统：解析 + 生成
  parse/                    #     PDF / docx / xlsx / pptx → Markdown/纯文本
  generate/                 #     JSON 骨架 → .pptx / .docx / .xlsx（渲染器按 type 路由）
  skeleton.go               #     骨架模型（LLM 输出 schema）
internal/server/doc*.go     # /api/documents/* 路由 + 异步任务 + 审计（复用 auth/push）
internal/webui/static/doc/  # 纯前端预览渲染页（webshare 双端复用）
documents 表                # 生成产物 + JSON 骨架落库（存 docId）
```

- 生成走异步任务：复用 `internal/tasks`（worker + 失败重试）+ `internal/push`（`doc.event`
  WS 推送）+ 审计打点。
- 解析走同步（小文件）或任务（大文件）两种入径，喂同一 `kb_documents` / `kb_chunks`。

> **⚠️ 实现现状**：v2.1.0 的生成是**同步**的——`POST /api/documents/generate` 内联完成
> 「LLM 出骨架 → 渲染 → 落盘 → 返回」，**不**走 `internal/tasks` 异步任务、**不**推
> `doc.event`、返回 `{id,name,docType,downloadUrl}` 而非 `taskId`（见 §4.1 标注）。解析也走
> 同步 ingest（`POST /api/kb/ingest`）。

### 2.1 与 RAG / 知识库的关系

- 解析出口（纯文本 / Markdown）复用 `internal/embed` + pgvector 索引：与当前 `intel_chunks`
  泛化的通用 `kb_chunks` 同一链路，KB 检索见 `docs/RAG_PROMPT.md`。
- 生成产物可**反向入库**：生成的 PPT 骨架渲染出的正文可 ingest 进 KB，下次聊天能引用自己
  产出过的文档。

## 3. 文档解析

| 格式 | 库（优先 Apache-2.0） | 出口 |
|------|----------------------|------|
| `.pdf` | `pdfcpu`（Apache-2.0）| 纯文本（按页），保留页号 |
| `.docx` | unioffice / 模板解析（见 §10 许可）| Markdown（标题 / 段落 / 表格 / 列表） |
| `.xlsx` / `.csv` | `excelize/v2`（Apache-2.0）| 工作表 → 结构化表格 Markdown |
| `.pptx` | unioffice / 模板解析（见 §10 许可）| 逐页：标题 + 要点（作者备注可并入） |

- 解析统一出口为带少量结构的 Markdown，走同一「分块 → embed → 检索」链路，复用
  `internal/server/rag.go` 的分块与缓存模式（chunk 带 `source: 文档名 + 章节/页`）。
- 表格是重点：`.xlsx` 解析保留 `Sheet / 行 / 列`，喂给 LLM 时以 Markdown 表格呈现，避免
  纯文本丢失行列结构。

> **⚠️ 实现现状**：`internal/doc` 已在 v2.1.0 落地，四类解析器齐备：`.pdf` 用
> `github.com/ledongthuc/pdf`（MIT，非上表 pdfcpu）、`.xlsx`/`.csv` 用 `excelize/v2`、
> `.docx`/`.pptx` 用标准库 `archive/zip` + `encoding/xml` 自解析（规避 AGPL）。解析结果统一为
> Markdown 喂给 `kb_chunks` 分块 → embed → 检索链路。

## 4. 文档生成

### 4.1 流程

```
用户（App / Web）发自然语言需求
  → POST /api/documents/generate（异步任务，返回 taskId）
  → 编排 LLM（internal/llm）把需求 + 可配 prompt 提炼为 JSON 骨架
  → 骨架落库 documents 表（docId + schema version）
  → 渲染器（按骨架 type 路由）确定性生成 .pptx / .docx / .xlsx
  → 产物存存储（本地目录 / 可选 MinIO），WS 推 doc.event（完成 / 失败）
  → 客户端把产物作为会话附件 part 追加进聊天（文件名带 docId 约定）
```

> **⚠️ 实现现状**：v2.1.0 的 `POST /api/documents/generate` 是**同步**的（200 直接返回
> `{id,name,docType,downloadUrl}`，2min 上限，不是 `taskId`）；产物只落本地 `<docs-dir>`
> （可选 MinIO 未接）；WS `doc.event` 未推。上流「骨架落库」在落盘前完成，文档类型由请求方
> 决定、LLM 只产对应 body；会话附件回投是客户端行为，后端不自动追加 part。

### 4.2 JSON 骨架（唯一中间格式）

LLM 输出示例（生成 PPT）：

```json
{
  "type": "pptx",
  "title": "产品发布会",
  "theme": "tech",
  "pages": [
    { "layout": "cover", "title": "产品发布会", "subtitle": "2026 Q3" },
    { "layout": "bullets", "title": "技术亮点", "points": ["低延迟", "可扩展"] },
    { "layout": "table", "title": "参数对比",
      "headers": ["型号", "价格"], "rows": [["X", "¥99"], ["Y", "¥199"]] }
  ]
}
```

> **⚠️ 实现现状**：v2.1.0 的骨架 schema 更简（`internal/doc/render.go` 的 `Skeleton`）：
> `{type, title, sheets?[{name,rows:[[...]]}], paragraphs?[], slides?[{title,bullets:[]}]}`，
> 按类型取对应字段渲染；上例的 `theme` / `layout`（cover/bullets/table）尚未实现
> （PPT 渲染只出每页标题 + 要点行）。文案来自 LLM、数值由调用方/检索喂入的铁律保持不变。
> 骨架提示词目前是代码内常量（`internal/server/documents.go` 的 `docGenerateSystem`），
> 尚未外置到 `prompts/` 文件。

- **一个 type 对应一个渲染器注册项**：新文档类型 = 加渲染器 + 骨架 schema，不动编排链路。
- LLM 产出 JSON 前做 schema 强约束（json_schema / 强 prompt），拼进 `prompts/` 文件
  （版本化、可测试，遵循 AGENTS AI 规范）。
- **数值不来自 LLM**：骨架里的数字要么来自用户输入，要么来自知识库检索，LLM 只填文案与结构。

### 4.3 渲染器（确定性）

| 产物 | 库 | 状态 |
|------|-----|------|
| `.pptx` | unioffice（AGPL）或 模板渲染（自研 zip+xml，规避许可） | 见 §10 |
| `.docx` | unioffice（AGPL）或 模板渲染（自研） | 见 §10 |
| `.xlsx` | `excelize/v2`（Apache-2.0）| ✅ 可直接用 |
| `.pdf` | 暂不生成（Backlog） | — |

> **⚠️ 实现现状**：三类渲染已在 v2.1.0 落地（`internal/doc/render*.go`）：`.xlsx` →
> `excelize/v2`（数据网格，**未含图表**）；`.docx` / `.pptx` → 标准库 `archive/zip` +
> `encoding/xml` 模板式自渲染（§10 的 AGPL 风险已按该方向锁定）。`.pdf` 仍不生成。

## 5. 会话内文档预览（客户端渲染）

### 5.1 决策：客户端渲染 vs 服务端转图

| | 客户端渲染（选此） | 服务端转图 |
|--|------------------|-----------|
| 后端依赖 | 无（零外部工具） | LibreOffice（破单二进制约束） |
| 双端复用 | 一套渲染页两端共用 | 图片直出但依赖工具 | 
| 弹性 | 点开才渲染，首屏略慢 | 缩略图可内联但成本高 |

### 5.2 渲染页（webui static，go:embed）

`internal/webui/static/doc/preview.html?src=<fileUrl>`，内置库：

| 格式 | 渲染器 | 能力 |
|------|--------|------|
| `.pdf` | pdf.js | Canvas 翻页 / 缩放 |
| `.docx` | mammoth.js | 转 HTML 段落样式 |
| `.xlsx` | SheetJS | 表格视图 |
| `.pptx` | pptxjs | 逐页渲染 |

> **⚠️ 实现现状**：v2.1.0 的渲染页已在 `internal/webui/static/doc/` 落地，渲染器与上表略异：
> `.pptx` 实际用 **JSZip 自渲染**（解析 `ppt/slides/slide<N>.xml` 逐页出标题+要点，未用
> pptxjs）；`.pdf` 支持 Canvas 渲染 + 翻页但**无缩放**；`.xlsx`/`.xls`/`.csv` 都走 SheetJS
> 每工作表一张表格。页顶提供同源下载。

- 文件来源：`src` 指向会话语义内的文件 url（App 本地缓存路径 / Web 会话文件地址）；小而关键
  的渲染页与库放 static，不引 CDN（离线可用）。

### 5.3 接入点

- **Web**（`internal/webui/static/assets/chat.js`）：附件卡片新增「预览」→ iframe / 弹窗加载
  渲染页。**⚠️ 实现现状**：v2.1.0 附件卡片已带「预览」按钮（`data-doc-preview`，仅
  pdf/docx/xlsx/xls/csv/pptx），点击用 `window.open` **整页打开**渲染页（webui
  `X-Frame-Options: DENY`，不能 iframe）。
- **Android**（`app/.../ui/screens/chat/ChatFileAndImageCards.kt`）：`Part.File` 卡片区分
  「图片内联 + 全屏预览」（已存在）与「文档 → WebView 弹层加载同一渲染页」。**⚠️ 实现现状**：
  目前只有图片路径；文档卡片 + WebView 预览为新增。

## 6. 聊天上下文驱动的重新生成 / 迭代

### 6.1 会话中的文档身份（docId 约定）

生成产物作为会话附件 part 追加，文件名携带 docId 以跨端找回映射：

```
生产规划-@doc-d7f2a1.pptx
```

- 解析规则：`-@doc-{id}.{ext}`。App / Web 据此把「附件 ↔ documents 表产物」对齐，点操作时
  把 docId 传给 regenerate。
- 备选：客户端本地映射表（跨端丢失）；**选文件名约定**：随会话走、不落客户端状态。

### 6.2 重新生成 / 迭代

```
卡片「重新生成」 → POST /api/documents/regenerate {docId}                （无 instruction = 原 prompt 重跑）
卡片「按意见修改」 → POST /api/documents/regenerate {docId, instruction:"把第3页改成对比图"}（迭代）

后端 regenerate：
  取 documents 表原 JSON 骨架 + 新 instruction（可空）
  + 最近 N 条会话消息（internal/opencode 拉取，作为聊天上下文）
  → LLM：骨架(修订) = f(原骨架, instruction, 会话上下文)
  → 新骨架落库（documents 表 + version++）→ 重渲染 → 新附件回投会话
  → 旧附件保留 + 新附件追加（聊天记录是事实日志，不删除）
```

- **会话上下文取数**：取生成该文档前后最近 N 条 turn（含用户多轮意见与 AI 累积结论），
  一并喂 LLM，实现「结合聊天的上下文」。
- 失败与超时：复用生成任务的失败重试与用户可见错误（卡片「重新生成失败」）。

## 7. 关键参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `docGenTaskTimeout` | 3min | 生成任务超时 |
| `docSkeletonVersion` | 1 | 骨架 schema 版本，向后兼容旧渲染 |
| `docContextTurns` | 20 | regenerate 时取最近 N 条会话 turn |
| 解析分块 | 复用 rag 现有默认 | 与 RAG_PROMPT §7 一致 |

## 8. 可观测与审计

- 生成 / 迭代 / 解析全部经审计链路（`logMiddleware` APP token 路径），带 docId 溯源。
- WS 事件 `doc.event`（生成完成 / 失败 / 迭代完成）推 App / Web。
- 打点：生成成功率、每次骨架版本、渲染耗时、预览页打开次数。

## 9. 客户端配套（App / Web 联动）

- Android：文档附件卡片 → 预览（WebView 渲染页）、下载、重新生成 / 按意见修改操作菜单。
  对应 App 仓库 ROADMAP 3.x。
- Web：同能力在 chat.js 附件卡片落地。
- 两端各自持有 `sentParts`（App `ChatViewModelOps.sendParts` / Web `chat.js`）的附件追加逻辑，
  生成完成后回投会话。

## 10. 未决问题

- [x] **OOXML 渲染/解析库许可**：unioffice 为 AGPL，与仓库（MIT）存在传染风险。方向：
  ① xlsx 用 excelize（Apache-2.0）不动；② docx/pptx 评估模板式自渲染（贪婪注册：
  go 标准库 `archive/zip` + `encoding/xml`，按 Office OpenXML 模板填内容），确定后再锁渲染器。
  **⚠️ 实现现状**：v2.1.0 已按方向②锁定——`.docx`/`.pptx` 解析与渲染全部用标准库
  `archive/zip` + `encoding/xml` 自实现（`internal/doc/ooxml.go`、`render_docx.go`、
  `render_pptx.go`）；`.xlsx` 解析/渲染用 `excelize/v2`；`.pdf` 解析用
  `github.com/ledongthuc/pdf`（MIT）。
- [ ] 产物存储介质：本地目录 or 接入 MinIO（`SCRM_STORAGE_PROVIDER` 已有双后端抽象可参考）。
- [ ] docId 文件名约定是否会与鉴权 / 下载路径冲突（`validateIntelLocalPath` 同款路径清洗）。
- [ ] 预览页是否支持「编辑骨架 → 重渲染」（Web 端进阶，可能滑出 v2.1.0）。
- [ ] 生成产物反向 ingest KB 的触发点（默认生成即入库？还是一次性任务？）。

## 参考

- [`docs/RAG_PROMPT.md`](RAG_PROMPT.md) — RAG-in-Prompt 检索注入
- [`docs/ROADMAP.md`](ROADMAP.md) — v2.1.0 范围与版本化
- [`internal/server/rag.go`](../internal/server/rag.go) — 分块 / 检索 / 缓存模式
- [`internal/tasks`](../internal/tasks)、[`internal/push`](../internal/push) — 异步任务 / WS 推送底座
- App：`ui/screens/chat/ChatFileAndImageCards.kt`（现有图片内联预览，文档卡片待扩展）