# 测试智能体系（Test Intelligence）设计

> 本文档描述在 StarBurst Backend 中新增的**通用测试智能体系**。它是一个与具体业务产品
> 无关的通用测试能力，可关联任意技术栈、任意仓库。覆盖：仓库理解与契约提取、测试资产发现、
> Git 变更驱动的增量测试、API/客户端字段级校验、被测环境供给（本机）、安全与合规审计、
> 失败归因与修复应用，并提供配置页执行入口。
>
> 设计原则：
> - **技术栈无关、类型可插拔**：项目类型以"Profile 注册表"形式扩展（现装现有体系：
>   Java/Maven、Go、Android、iOS、Web(Vue/React)、BFF(GraphQL)、Node…；Python 等后续
>   追加即注册新 Profile），不绑定任何具体产品。
> - **识别可人工校正**：自动识别带置信度分级，所有结果都可在界面上人工修正（表格直改 +
>   提示词批量辅助），修正以覆写层形式保存、可追溯。
> - **不主动改被测代码**：系统只读分析；发现问题可产出"修复建议"，由用户人工核验后在页面
>   点击「应用」才写入被测项目（唯一写路径，可审计）。
> - 遵循 `/workspaces/AGENTS.md`：**关键事实（表、字段、必填、数值）必须由确定性代码提取，
>   LLM 只做解释、补全与归因**；知识全部带 provenance（来源文件:行号），可审计。
>
> **阅读须知**：本文是**设计文档**，描述目标形态而非当前实现。文中标有 **「⚠️ 实现现状」**
> 的引用块是与代码核对后的更正，凡与正文冲突以标注为准；全部偏差汇总见 **§11**，需要架构组
> 评审的技术栈偏离（Go 而非 Spring、pgvector/HNSW、SQLite/PG 双方言、正则式静态抽取）见
> [`docs/TECH_DEVIATION.md`](TECH_DEVIATION.md)。设计内容本身**不因其未实现而删除**。

## 1. 背景与目标

### 1.1 要解决的问题

1. **事前理解**：选一个项目，先得扫仓库搞清楚它有什么实体、什么接口、接口返回哪些字段。
2. **契约校验**：测列表接口时，要知道它查的是哪张表、该返回哪些字段、哪些字段是必填的。
3. **客户端联动**：测 Android 时，要知道哪些字段界面必须展示，后端漏填/返回 NULL 时界面
   必然出问题；报错时能自动归因（后端 / 数据库 / 客户端绑定）。
4. **多项目组织**：后端、安卓、iOS、多个 Web、同端多个客户端由多个仓库组成，统一组织、
   自动识别每个仓库是什么、是给谁用的。
5. **测试资产复用**：项目里已有的 JUnit / Playwright / XCTest 等测试要能被识别、分类、
   存库，并在页面上按范围批量/后台运行。
6. **避免全量重测**：每次跑测试都全量扫描/全量执行成本高；Git 项目应基于"上次已验证版本到
   当前版本的变更 + 上次未解决的问题"做增量测试，问题解决即跳过。
7. **环境供给**：项目依赖中间件（MySQL/Redis/Nacos…）与构建工具链（JDK/Android SDK/Node…）
   与 Android 设备。测试前自动检测本机；缺失项**逐条列出、逐项一键安装**（不批量装所有）；
   中间件可容器拉起或手动配外部环境（落库可改）；Android 设备支持 USB / 无线 ADB，手动选择
   并绑定（存库，除非手动更改）。平台不支持的能力（如 Linux 跑 iOS）明确告知，并可**添加远程
   执行节点**（SSH 登录远机执行并拉回结果）补足。漏洞库公网在线拉取为主。
8. **安全与合规审计**：自动分析项目本身以及它引用的组件/代码是否存在漏洞、问题或不符合规范
   的地方（依赖漏洞、代码缺陷、合规规则、密钥硬编码等）。
9. **功能点组织**：程序中的漏洞/bug（区别于 §3.7 静态/合规 findings）按**功能点**组织管理
   （如"首页 Banner"、"实名认证"）。自动识别出一批功能点（顺序无序），可在页面**拖动排序**
   并随测演进；每个功能点标注**涉及的端**（Java 端/Android/iOS/H5/Web…）；在功能点详情页可
   做**单测**（接口是否通顺、调用后是否返回期望数据）。
10. **测试后 AI 对话**：单测/回归完成后，能带着"实测结果 + 相关契约/代码片段"的上下文向 LLM
    提问并归因（如"库里配了 4 个 Banner，为什么只给我 3 个"），系统自动组织提示词发送分析。

### 1.2 目标形态

```
添加项目（本地路径 / Git URL）→ 自动识别 type/role（可人工校正）
系统：分析 → 产出可审计的测试情报（表/API/契约/Android 必展示清单/测试资产）
      → 按 Profile 生成该项目命令白名单（详情页审核可改）

触发一轮测试（页面上点按钮 / Git Push / cron）
系统：环境门禁（就绪→执行；缺失→报错给解决途径逐项安装；平台不支持→明确告知）
      → Git delta（last_tested_sha→当前 HEAD）推导"受影响测试范围" ∪ 上次未解决问题
      → 后台执行（测试 Worker 并发默认 1）→ 逐字段/逐用例断言 → 结果入库并推送
失败：自动归因 → 产出修复建议（草稿）→ 用户核验后点「应用」写回被测项目
新一轮提交时：解决过的跳过，未解决的跟随重跑

功能点：接口聚簇识别 → 拖动排序/标注涉及端 → 功能点单测（连通性+期望数据）
        → 集成测试 bug 挂功能点 → 测后 AI 对话（自带实测上下文归因）
智能分级：敏感字段明文返回（密码/密钥/身份证…）→ SECURITY_WARNING 安全警告（非普通 bug）
AI 建议：提示词规则列表配置 → AI 润色 → 逐条扫描风险/性能 → findings 进闭环

环境：中间件/工具链/设备/远程节点四块，逐项检测；缺失逐项一键安装；设备绑定存库手动才变
审计：依赖漏洞扫描 + 代码静态/lint 采集 + 合规规则 + LLM 评审 → findings 入库进闭环
```

### 1.3 为什么放在 StarBurst Backend

它已具备连接本机 OpenCode 的完整能力（读文件、搜代码、驱动 agent、会话编排）与异步任务、
LLM 结构化输出、推送、自动化、审计、内嵌配置页全部底座；本功能只是在其上新增业务子系统，
不引入外部服务。

## 2. 现状能力盘点（可复用部分）

| 能力 | 现成实现 | 位置 | 本功能的用法 |
|------|----------|------|--------------|
| 读文件 / 列目录 / 搜文件 | `ReadFile`/`ListDirectory`/`FindFiles`/`SearchText` | `internal/opencode/client.go` | 仓库结构化扫描 |
| 列项目 / 项目目录 | `ListProjects`/`GetProjectDirectories` | 同上 | "选择项目" |
| 驱动 OpenCode agent 深入分析 | `PromptV2` / `tasks` 执行器 | `internal/tasks/executor.go` | 复杂理解、生成校验脚本/修复建议 |
| LLM 结构化抽取 | `llm.CompleteJSON`/`DecodeJSON` | `internal/llm/client.go` | 静态锚点覆盖不到的语义补全 |
| 异步长任务 | 任务队列：workers/重试/依赖/`workflow_id`/超时 | `internal/tasks` | 分析/执行多步编排 |
| 后台命令执行 | 可扩 `tasks` 新 kind（os/exec 跑 mvn/npm/gradle） | `internal/tasks` | 跑 JUnit / Playwright 等 |
| 实时推送 | WS Hub + severity 分级 | `internal/push` | 测试进度与结果 |
| 定时 / 事件触发 | cron + webhook 规则引擎 | `internal/automation` | 周期回归、GitLab push/tag 触发 |
| 知识落库 / 审计 / 统计 | 版本化迁移 SQLite/PG + audit + stats | `internal/store` | 持久化契约、测试资产、结果 |
| 批量下发 | `/api/batch` | `internal/server/batch.go` | 多项目并行分析 |
| 配置页 | `internal/webui`（go:embed SPA） | `internal/webui` | 新增"测试"执行页 |

## 3. 总体架构

新增独立子系统 `internal/intel`，与既有编排子系统平级，复用 store / push / opencode / llm /
tasks / webui。

```
┌──────────────────────────────────────────────────────────────┐
│ 项目列表（单用户、扁平）projects                                │
│   source: local path | git url(+branch/tag)  type/role 自动识别 │
│   commands_json（按 Profile 生成、详情页审核可改）              │
├──────────────────────────────────────────────────────────────┤
│ ⑤ 环境层 intel/env       被测环境供给（仅本机）：              │
│    中间件/工具链/设备/远程节点 逐项检测 → 缺失逐项一键安装  │
│    设备绑定存库(USB/无线ADB,手动选择)  平台不支持→明确告知   │
├──────────────────────────────────────────────────────────────┤
│ ① 情报层 intel/scanner   扫描项目仓库（确定性为主）            │
│    实体/表/接口契约 + Android 必展示清单 + 测试资产发现       │
│ ② 变更解析 intel/delta   扫描范围决策：                         │
│    本地非 git → 全量；git → last_tested_sha..HEAD diff 影响面  │
│    测试范围 = 变更影响面 ∪ 上次未解决问题                      │
│ ③ 执行层 intel/runner    契约校验（HTTP）+ 测试命令执行（os/exec）│
│    断言 + 报告解析（surefire XML / playwright json…）→ 结果入库 │
│ ④ 归因层 intel/rootcause 失败样本 + 代码片段 → 归因 + 修复建议(草稿)│
│ ⑥ 审计层 intel/audit     安全与合规：                           │
│    依赖漏洞 + 代码静态/lint 采集 + 合规规则 + LLM 评审 → findings│
└──────────────────────────────────────────────────────────────┘
          ▲ 页面触发（webui「测试」页）/ API / webhook / cron
```

### 3.1 项目模型（单用户、扁平）

单用户场景，**不设产品组/主项目两级**——就是一个扁平的**项目列表**，每个项目是一个真实仓库，
独立管理、独立编排。

- **项目（projects）**：一个仓库，含以下属性——
  - 来源：本地路径（含 `.git` 时同样走增量）或 Git URL；Git URL 项目由后端 clone/pull 到
    **全局设置的临时仓库缓存路径**（默认 `~/.local/share/starburst-backend/intel-repos`，
    可在测试页「全局设置」中修改），之后统一按"本地文件 + git 数据"分析（扫描器只有一套
    实现，Git 只是数据源）。
  - **缓存路径变更确认**：修改 `intel.repos_dir` 时页面**弹出提示询问是否重建**——① 不重建：
    原缓存目录及已 clone 仓库保留不动（原路径数据继续可用）；② 重建：先删除原缓存目录下
    全部已 clone 仓库（破坏性操作，二次确认 + 审计留痕）再按新路径重新 clone；重建后相关
         项目自动标为待重新分析。

    > **⚠️ 实现现状：两阶段重建未落地**。`internal/server/server.go` 注册的 **98 条路由**里
    > 找不到对应 handler——§5 设计的 `POST /api/intel/settings/probe-repos`（预检）与
    > `POST /api/intel/settings/repos-rebuild`（确认+删除+重建）**都不存在**，也没有**任何针对
    > repos_dir 变更**的清理代码（唯一的删除动作在 `ensureGitClone`：origin URL 变了就
    > `os.RemoveAll` 单个项目的 clone 再重拉，与缓存根目录无关）。现状：`intel.repos_dir` 只是一
    > 个普通 `settings` KV，改了值后旧目录下已 clone 的仓库**原地不动**，新 clone 才落到新路径。
    > 另：本条所称"默认 `~/.local/share/starburst-backend/intel-repos`"也不成立——代码里**没有
    > 默认值**，`projectRoot` 读不到该 setting 时直接返回
    > `intel.repos_dir not configured for git projects`。
  - **Git 克隆认证与 ref 语义**：Git URL 项目 clone 需要认证时（如私有 GitLab），在项目或
    全局设置配置 **HTTP token / SSH 私钥**（加密落库，仅在 clone/pull 时注入）；`git_ref` 为
    钉住的分支或 tag（默认远端默认分支），**ref 变化 → 该仓库回退全量重扫**；检测到
    `last_tested_sha` 已不在新 HEAD 祖先链上（force push / 重置）同样回退全量，不做增量。
    大仓库可配置 shallow clone（仅首次，pull 时自动加深）。
  - **类型自动识别（type）— 可插拔 Profile 注册表**：`build.gradle.kts`/`settings.gradle.kts`
    → Android；`*.xcodeproj`/`*.xcworkspace` → iOS；`pom.xml` → Java/Maven；`package.json`
    且含 `vue/react` → Web；含 `schema.graphqls` → BFF(GraphQL)；`go.mod` → Go；`package.json`
    其他 → Node。每个 Profile 打包自己的：识别锚点 / 结构化扫描器 / 测试命令与报告解析 /
    依赖漏洞工具 / 合规规则 / 默认命令白名单，注册即用。
  - **子项目（modules）— 支持混合/多类型仓库**：一个仓库可含多种类型（如某仓库内 Java 后端
    + Node 服务 + Android 客户端 + iOS 客户端共存）。识别按"仓库 → 子项目"分层：探测
    Maven 多模块（`<modules>`）、Gradle `settings.gradle` 的 `include`、目录形态（`clients/`、
    `services/`、`app/` 等平台目录）与各子目录自己的构建锚点，**对每个子项目独立识别
    type/role**，产出 `project_modules`（rel_path + type + role + 构建工具）。契约、测试资产、
    环境依赖、命令白名单按子项目组织；分析/测试可精确到子项目层面，平台类型也以子项目为准
    （e.g. 同仓库的 Android 与 iOS 子项目分别归属对应 profile）。
  - **角色自动识别（role）**：业务定位（如"某项目·安卓用户端"）。确定性骨架（目录名/应用名/
    README/DESIGN.md/PROJECT_OVERVIEW.md）优先；模糊处由 LLM 结合文档与命名（类名/方法名/
    变量名）补全，产物带 provenance 与置信度，**一律可人工校正（见 §3.8）**。
  - **命令白名单（commands）— 每项目一份**：按 Profile 识别时**自动生成该技术栈全量候选
    命令集**（build/test/package/清理等全部可能用到的命令模板），**默认全量列出、由用户手动
    筛选**需要放行的子集后生效；用户可逐条增删改，保存即生效。远程节点执行与本地执行共用
    同一份白名单。
  - 每个项目记录：`last_tested_sha`（上次测试时的 commit）、知识快照版本、关联 env。

### 3.2 知识缓存与"用前必校验"

- 契约 / 测试资产 / 测试记录全部落 DB：下次直接复用。
- **复用前陈旧度校验**：`当前 HEAD sha != 快照 sha` 或项目文件哈希变化 → 缓存失效，
  重新分析后再用。本地非 git 目录用目录扫描哈希。

### 3.3 扫描策略：全量 vs 增量

```
首次 / 版本跳变 / 缓存失效 / 本地非 git → 全量扫描（顺带完整测试资产发现）
增量触发（git 项目）：
  git log/diff（last_tested_sha → 当前 HEAD）
  → 变更文件集 → 影响面推断：改了 Entity/Repository → 涉及哪些接口契约；
      改了下单页面 → 涉及哪些 Android 界面；改了测试文件 → 该测试直接入范围
  → 测试范围 = 变更影响面 ∪ 上次未解决问题
```

- **变更文件分类规则**（决定影响面放大/收窄）：
  1. **构建/依赖/基础设施文件**（`pom.xml`/`build.gradle`/`go.mod`/`package-lock.json`/
     `Dockerfile`/CI 配置）：难以局部定位 → **放宽为对应子项目全量**（共享程度高则整仓全量）。
  2. **配置/资源文件**（`application.yml`/`bootstrap.yml`/Flyway `*.sql` 迁移/SQL 脚本）：
     影响同模块**全部集成测试与契约校验**（SQL 迁移尤其要先合入再测）。
  3. **测试文件**：该测试直接入范围。
  4. **业务源码**：按已提取的契约 provenance（file:line → 端点/实体/页面）反查受影响契约与
     用例；公共代码位置（shared util）按引用关系**模糊匹配各模块，宁可放大不漏测**。

### 3.4 问题闭环与修复应用（变更随历史演进）

```
提交 A → 触发测试 → 发现问题 P（intel_issues 落库，未解决）
提交 B → 触发测试：
  ├─ P 未解决 → 把 P 的定向用例并入本轮范围（验证 B 是否顺带修复/影响它）
  ├─ 重跑 P 转绿 → P 标记 resolved，后续跳过（进入回归清单）
  └─ 同时验证 B 自身变更的新增影响面
```

- "是否已解决"以**重跑对应失败用例转绿为准**（commit message 提及仅作辅助线索）。
- **集成测试 bug 必须关联功能点（§3.9）**：integration 类测试产生的 issue 必须有
  `feature_id`（落到涉及的功能点）；自动归属不了的进「待分配」队列由人工挂载，挂上才算完整记录。
- **智能分级：安全警告 ≠ 普通 bug（§3.9）**：问题分类不按死规则——命中安全敏感特征
  （响应返回明文密码/密钥/完整身份证/银行卡/手机号、未脱敏金额等）自动升为 `SECURITY_WARNING`
  安全告警（severity 高，与 findings 同闭环），普通字段缺陷才是普通 bug。
- **结果随重测自动演进（分类型处置）**：每次版本提交触发的重测结束后，对受影响范围内的
  findings/issues 做状态更新——
  1. **漏洞类（intel_findings，含 CVE/依赖/安全）**：重新扫描确认已修复（如依赖升级、代码
     修复）→ 从活跃清单**移除**（`removed`），历史保留可查（记录 `removed_at/commit`）。
  2. **Bug/警告类（intel_issues，业务缺陷/代码告警）**：对应用例重跑转绿 → 标记**已处理**
     （`resolved`），保留记录但不阻塞后续；可随时回滚查看。
  3. 重测仍失败 → 保持 `open` 并更新 `last_check_at/commit_seen`，继续跟随下一轮。
  4. **安全警告类（SECURITY_WARNING）**：复测确认敏感字段已脱敏/移除 → 置 `resolved`；
     未处理保持 `open`，排序权重高于普通 bug（优先跟随每一轮）。
  - 处理粒度以"该问题关联的测试/扫描项"为准；状态变化全部写审计留痕。
- **修复应用（不自动改代码，人工点「应用」）**：
  - 归因/审计发现问题若可修（如 Maven 执行报错、缺必填字段、违反合规规则），系统产出**修复
    建议（patch 草稿）**：含改动文件、行位置、新内容，带 provenance 与置信度。
  - **系统绝不自动写入被测项目**。修复建议在页面展示 diff，由用户逐条核验，核验无误后点
    「应用」才写回本地工作区；**写回形态为可配置项**（每项目设置或每次应用时选择：`直接写
    文件` / `生成补丁文件` / `仅复制到剪贴板让用户自操作`），记为一次写审计。
  - **git 项目写回可选项**：额外可选**`建分支 + 提交`**——变更提交到临时分支不污染主干，
    由用户自决是否 push；与直接写工作区二选一。
  - **备份与回滚**：每次「应用」前自动备份被改文件原文（或记录 git diff 基线）；应用后提供
    **一键回滚**入口（恢复备份 / `git checkout` 基线），回滚同样经审计留痕，可反复应用/回滚。
  - 应用后自动复测该项；仍未解决则保持打开继续跟随，已解决的标记 fixed。

### 3.5 测试资产发现与执行

- **发现（scanner 一部分）**：按框架识别测试文件并分类入库——Java `src/test` 的
  `@Test`/`@SpringBootTest`（unit vs integration），Web `*.spec.ts`/`playwright.config`（vitest vs
  playwright），Android `src/androidTest`+`src/test`（instrumentation vs local unit），iOS
  `*Tests.swift`（XCTest），Python `test_*.py`，Go `*_test.go`。维度：`kind`（unit/integration/
  api/e2e-ui/android-ui/ios-ui）+ `framework` + `module`（Maven 模块/页面目录）+ `class/method` +
  路径 + 最近状态/耗时/flaky 次数。
- **执行（runner）**：跑测试是确定性命令，由 `tasks` 新增 kind 直接 os/exec，命令必须落在**该
  项目命令白名单**内：Java `mvn -pl <module> test -Dtest=<Class>#<method>`、Playwright
  `npx playwright test <file>`、Android `./gradlew test`/`connectedDebugAndroidTest`、Go
  `go test ./...`。捕获退出码 + 解析报告（surefire `TEST-*.xml` / playwright json / go test -json）
  → 逐用例结果回写。全程异步、可批量、可取消，进度/结果走 WS 推送。

> **⚠️ 实现现状（本节"由 `tasks` 新增 kind"未落地）**
> 代码里**没有** `kind=test-run` 这种 task kind：`internal/store/task.go` 的 `Task` 结构体**无
> `Kind` 字段**（只有 prompt/dependsOn/priority/workflowId/retry 相关列）。测试执行走
> `internal/server/intel_runner.go` 的**自有派发通道**：`enqueueIntelRun` → goroutine →
> 全局信号量 `intelExecSem`（`const intelExecConcurrency = 2`）+ 每项目互斥
> `intelExecMutex` + 取消表 `intelCancels`，结果落在独立的 `test_runs`/`test_results` 行，进度经
> `intel.run.event`（`pushIntelRunEvent`）推送。**后果见 §7 的同名标注**（拿不到重试/依赖/优先级/
> worker 池/崩溃恢复/janitor）。os/exec 跑命令 + 命令白名单约束 + 异步/可取消 + WS 推送**已实现**。
> - **但报告解析只接通两种 kind**：`parseReport`（`internal/server/intel_exec.go`）仅分派
>   `go`（`go test -json`）与 `surefire`（`target/surefire-reports/TEST-*.xml`，Maven/Gradle 共用）。
>   **Playwright JSON 解析器虽已实现且有单测（`internal/intel/report` 的
>   `ParsePlaywrightJSON`）却没有任何调用方**，故本节与 §8 M3 承诺的"playwright 报告解析"
>   在当前代码里**取不到逐用例结果**；`npm` 报告类型不解析，只合成"整轮一条"通过/失败。
>   本节列出的 iOS XCTest、Python pytest 报告解析亦**未实现**。

- **Flaky 治理**：同一用例连续 N 次（默认 3，可配）出现"同一版本先失败、重跑转绿"→ 自动判定
  flaky：标记 `flaky_quarantined`、**从"受影响范围/增量回归"自动剔除**，不再拖累整轮结果；页面
  单独告警区展示被隔离用例与历史 `flaky_count`；用户可**手动解除隔离**或**标记为真实 bug**入
  问题闭环。

### 3.6 环境管理（intel/env）

环境分三类：**中间件**（MySQL/Redis/Nacos/RabbitMQ/ES…）、**构建工具链**（JDK/Maven/
Android SDK/Node/Gradle/Go…）、**Android 设备 / 远程执行节点**（真机 / 模拟器 / 远程机器）。
原则：**只管理本机**（不连接已部署的被测应用环境）；检测、缺失项逐个列出、**逐项一键安装**，
不做"全装"；本机平台不支持的能力（如 Linux 跑 iOS）→ 明确告知，并支持**添加远程执行节点**补足。

- **依赖声明**：两种来源——① 自动推导：扫 `pom.xml`（mysql-connector/redis starter/
  nacos/rabbit/elasticsearch）、`bootstrap.yml`/`application.yml`、`package.json`、Android
  `build.gradle.kts`（compileSdk/minSdk/AGP 版本）得出中间件与工具链清单；② 人工 manifest：
  项目可维护 `intel-env.yaml` 显式声明 `{service, version, port}` 覆盖或补充自动结果。
- **检测本机（逐项）**：中间件探活（Docker socket 可用、镜像在私有 registry/本地容器在跑、
  目标端口通、外部地址连通）；工具链验证（`java -version`/`mvn -v`/`node -v`/`sdkmanager
  --list_installed` 对齐版本要求）；设备检测（`adb devices` 列出连接设备）。**缺失项逐个列出
  到页面**，每一项独立显示"缺少什么、期望版本、当前无"。
- **供给模型**：
  1. **中间件——容器自动拉起**：`docker run`/`docker compose` 启动（MySQL 8/Redis/Nacos/
     RabbitMQ/ES），分配空闲端口、生成随机口令、健康检查（MySQL `select 1`/Redis `ping`/
     Nacos `/nacos`）、等待就绪后把连接信息写入运行配置。**数据卷持久化**（容器重建数据不丢）；
     提供容器/卷的启停、重置、**过期清理**（长期未用可一键清理，审计留痕）；**自动拉起生成的
     口令与外部配置一致加密落库**（`env_services`），服务重启后可直接复用连接，页面遮蔽显示。
  2. **中间件——外部配置**：用户填 IP/端口/账号/密码 → 探活校验连通性，通过后沿用；
     凭证加密落库，**配置持久化可再次修改**（除非环境变更否则不重建）。
  3. **工具链——逐项一键安装**：每个缺失项对应一个「安装」按钮，单独安装单一环境
     （如 JDK 下载包、Android SDK 用 `sdkmanager` 装对应 platform/build-tools、Node 装
     指定版本），安装后复检并落库，**不批量安装所有缺失项**。安装**幂等**（目标版本已装则跳过）；
     下载源固定并做**校验和/签名校验**；对已装项提供**卸载**入口（恢复主机整洁，卸载同样审计）。
- **Android 设备**（Linux 主机，无 iOS 环境，见 §10）：
  - **连接方式二选一，需手动选择并绑定**（不自动抓取现有设备，配置入口提供"选择已有 /
     新增连接"两种）：
    - **选择已有**：连接页列出已上线设备/已建模拟器，点选即作为测试绑定设备。
    - **新增 · 无线 ADB**：填写手机 IP + 端口 → 点「连接」→ 手机上弹出授权窗口需手动确认 →
      确认后 `adb connect` 建立连接并保存绑定。
    - **新增 · USB 直连**：设备通过数据线接主机 → 手动选择该设备为绑定设备。
  - **环境绑定**：绑定关系存 DB（`env_devices`），**除非手动更改否则保持不变**；测试运行
    前检查绑定的设备/模拟器是否在线，离线则提示重新连接，不静默换设备。
  - 模拟器：可用 `emulator` 启动已装 AVD（作为"安装完成的设备"进入同一绑定模型）。
- **远程执行节点（remote nodes）**：本机平台缺失/不支持时（如公网 Linux 主机跑 iOS、需要
  macOS 的 Xcode 测试），**添加一台机器为远程节点**：
  - **配置**：填主机 IP/端口、登录用户、口令或密钥（加密落库）；可加 SSH key 指纹校验
    （host key 校验，防中间人）。
  - **能力标签**：节点声明自己能干什么（`ios-xcode` / `android-sdk` / `linux-docker`...），
    项目测试按能力标签路由到对应节点执行。
  - **执行**：runner 通过 SSH 登录远程，执行该项目命令白名单内的命令（`xcodebuild test`/
    `gradlew`/`mvn` 等），**流式回传输出**、拉回报告/产物（scp），结果按既有管道解析落库。
  - **生命周期**：节点可达性检查（SSH 连通+能力自检），离线标灰提示；绑定/节点选择存 DB，
    除非手动更改否则不变。

> **⚠️ 实现现状（远程执行与上述四点均有出入，头号能力"去 macOS 跑 xcodebuild"实际不可用）**
> - **未使用 `golang.org/x/crypto/ssh`**：全仓**没有任何 `crypto/ssh` import**，`x/crypto` 依赖仅被
>   `internal/auth/auth.go` 用于 bcrypt。真实实现是 `internal/server/intel_exec.go` 组 argv 后调
>   `envagent.RunSSH`（`internal/intel/envagent/envagent.go`），后者 `exec.CommandContext(ctx, "ssh", …)`
>   起**系统 `ssh` 二进制**（`BatchMode=yes`、`ConnectTimeout=10`、`StrictHostKeyChecking=accept-new`、
>   `IdentitiesOnly=yes`）。文档所称"已在依赖中"不准确——是**间接的 Go 模块依赖**，不是被引用的库。
> - **无流式回传**：`cmd.CombinedOutput()` 一次性收集，远端输出**不会**边跑边推给前端。
> - **无 scp、无产物回拉**：全仓无 scp 调用，远程只回传 stdout；这也是下一条限制的直接原因。
> - **仅 `go` 报告可远程执行**：`intel_exec.go` 对 `reportKind != "go"` 直接返回
>   「远程执行目前仅支持 go 报告（stdout 自包含）」。**因此本节和 §10 反复承诺的"添加 macOS
>   节点跑 XCUITest / 装 Android SDK 跑 gradlew"目前跑不通**——xcodebuild / Gradle / Maven 依赖
>   落盘报告（xcresult / surefire XML / JUnit XML），拿不回本机就无法解析落库。
> - **capabilities 有表无逻辑**：`remote_nodes.capabilities` 列存在、CRUD 与页面可填，但执行链路
>   **没有任何按能力标签路由**的代码——只有调用方显式传 `node` id 才会走远程；`intel_exec.go`
>   注释里"routed runs rely on the node's capability labels instead"所指的机制并不存在。
> - 已实现部分：节点 CRUD/可达性探测、host key 指纹落库、远程命令 shell 元字符拒绝、白名单约束。
- **执行链路集成**：一次测试运行前走 `ensure-ready`——逐依赖 + 工具链 + 绑定设备/节点逐个
  检查，缺失/离线则列出并提示（可跳转去逐项安装/连接），到位才进执行；结果回写 env 状态。
- **库环境初始化（schema/data）**：中间件就绪后、跑集成测试/契约校验前，先做**表结构与数据
  初始化**——优先执行项目自带迁移（Java Flyway/Liquibase、`*.sql` 脚本），或 `intel-env.yaml`
  声明的 `init_scripts: [{service, script}]` 列表；初始化完成（含迁移版本校验）才进入执行，
  避免在空库上校验全部失败/全部空值。初始化动作同样落在命令白名单内、记录审计。
- **生命周期**：中间件可手动启动/停止/重置（删容器清数据重拉），容器挂掉自动重启或标记
  异常推送；工具链与设备绑定跨重启持久化。
- **运行前门禁（gate）与缺失报错**：点击「运行」先做环境门禁，结果分三类响应——
  1. **就绪** → 直接进入执行；
  2. **缺失且本机可修复**（中间件未起 / 工具链版本不符 / 设备未连）→ **报错中断**并给出
     解决途径：提示缺什么、期望版本或连接步骤，链接到对应「安装」/「连接」入口；设备/模拟器
     给"选择已有或新增"配置面板，不自动绕过；
  3. **平台不支持**（如 Linux 主机跑 iOS：无 Xcode/模拟器）→ **明确告知"当前平台不支持该
     环境/测试"**并给出解决途径（添加带对应能力标签的远程执行节点、指定可执行该项的节点）；
     页面标注该能力及支持平台，避免用户误以为配置缺失而反复排查；此类门禁项记 `unsupported`
     但**可经远程节点路由后变为可执行**。
  - 门禁错误进入归因链路记 `ENV_ISSUE`（区分 missing/unsupported/noderedirected），不误报到
    业务代码。

### 3.7 安全与合规审计（intel/audit）

对项目做漏洞与规范扫描，产物统一进 findings，进入问题闭环（§3.4），可修复项产出修复建议
（§3.4 修复应用）。

- **依赖漏洞**：按技术栈走确定性工具——Maven `dependency-check`/OWASP、npm `audit`、
  Go `govulncheck`、通用 SBOM + Trivy（`--scanners vuln`）。**当前环境公网可达，在线拉取为
  主**：Trivy 直接从 ghcr.io 拉取 `trivy-db`；OSV（`osv-scanner`）走 `api.osv.dev` 支持
  Maven/npm/Go/PyPI 多生态；  NVD 反爬 + API Key 要求导致 `nvd.nist.gov` 直连不稳（403），
  Maven 扫描优先 Trivy/OSV 而非 dependency-check 直连 NVD。公网不可达时才回退离线缓存（本地
  Trivy db / NVD JSON 目录）。
- **扫描去重与快照缓存**：findings 按 **CVE/规则号 + 依赖坐标去重**（同一漏洞跨子项目/跨版本
  只留一条，聚合受影响位置）；扫描结果按**依赖清单快照缓存**（lock 文件/`pom.xml` 未变 → 复用
  上次结果，不重复在线拉取），随重测自动失效，避免每轮全量重扫。
- **SBOM 导出**：Trivy/`osv-scanner` 生成 SBOM（CycloneDX）一并落库，提供**导出入口**（`POST
  /api/intel/sbom` 或页面按钮），供对外交付与审计核对。
- **代码缺陷与 lint**：采集项目已有的静态分析结果（如 Java 项目构建本就跑 SpotBugs/PMD/
  Checkstyle）——扫描 `target/` 报告或主动执行一次；外加自研规则（密钥硬编码、SQL 注入、
  越权、弱口令等黑名单/正则锚点，确定性检测）。
- **合规规则**：对照 `/workspaces/AGENTS.md` 硬规范的可配置规则集（Copyright 头缺失、
  缺 Javadoc、Swagger 注解残留、单文件超行数、import 未清理、行尾空格等），规则可开关、
  可配阈值；每个命中记录 `rule_id + source_file:line`。
- **LLM 评审**：对高风险/复杂区（依赖树引入的公共漏洞、鉴权/支付/隐私相关代码路径）做
  理解式审查，产出带 provenance 的建议；只做提示，不参与硬断言（AGENTS.md：LLM 不生成
  关键结论）。
- **产物与状态演进**：`intel_findings`（detector/severity/类型/位置/状态），severity 分级
  critical→info，可按项目/子项目/severity 过滤，未修复项进问题闭环随测；每次重测后自动重估
  ——已修复漏洞置 `removed`（历史保留）、代码类置 `resolved`、仍存在保持 `open`（§3.4）。
- **误报/豁免通道**：规则与漏洞扫描必有误报与"接受风险"项——每条 finding 支持人工标记
  `false-positive`（误报）或 `waived`（接受风险），**必须填理由并审计留痕**，可随时取消豁免
  重新打开；被豁免项不进问题闭环、不阻塞后续。

### 3.8 自动识别、置信度与人工校正

自动识别（尤其 role、接口/字段语义）可能因文档缺失、代码无注释而偏差，**必须可人工纠正**。
核心思想：自动值只占"底稿"，人工修正以**覆写层**叠加，二者并存且全部可追溯。

- **置信度分级**：
  - `high`：确定性锚点（构建文件/工程结构/注解/自动推导的表列），无需人工确认。
  - `medium`：启发式识别（目录名/类名/方法名/变量名约定，如 `submitRealName` → "实名提交"）。
  - `low`：LLM 结合文档/命名推断的语义（业务角色、接口用途），仅作建议。
- **人工校正方式（两种并用）**：
  1. **表格直改**：识别结果在页面以表格列出（type/role、接口、字段、Android 必展示
     清单等），行内可直接修改/补备注；修改即录入覆写层（见下）。
  2. **提示词批量辅助**：`POST /api/intel/overrides/suggest` 用一句自然语言生成覆写草稿
     （"所有以 /admin 开头的接口都属于管理端"、"字段 level 在界面叫会员等级"），LLM 转成
     结构化草稿，**先以表格形式展示、逐条确认后应用**——保留"LLM 不出最终结论"的底线，人拍板。
- **覆写层（overrides）**：自动值保留（含溯源）为 `auto_value`，人工/提示词确认为
  `manual_value`（生效值，`overridden=true`）。再次扫描时按锚点策略处理（见下），不静默覆盖。
- **锚点变更策略**：重新分析时——对应代码锚点（file:line / commit）未变 → 保留人工覆写；
  锚点已变 → 该项进入「待复核」，由用户决定保留或清除，**绝不自动吞掉人工修正**。
- **待确认队列**：`medium/low` 置信度项进「待确认」，页面红点提示逐条确认/修正，确认后固化
  （`provenance='manual'`）。

### 3.9 功能点组织、单测与 AI 对话（intel/feature）

程序中的漏洞/bug（区别于 §3.7 静态/合规 findings）按**功能点（Feature）**组织——业务功能单元
如"首页 Banner"、"实名认证"、"下单"。功能点是集成测试问题、单测、AI 对话的共同挂载点。

- **识别（自动聚簇 + 人工校正）**：scanner 在端点契约基础上按 Controller/路径前缀/关联实体/
  页面绑定聚簇出**候选功能点**（如 `/banner/*` 接口 + BannerPage 绑定 → "首页 Banner"）；命名
  默认取聚合锚点，LLM 辅助语义命名（low 置信度）；全部可人工改名/合并/拆分。
- **顺序（人工优先，可拖动）**：自动识别顺序按扫描序（无序、可能乱排）；页面**拖动排序**，
  `sort_order` 持久化；重扫**不覆盖人工顺序**，新识别功能点追加到未排序区。
- **涉及端（ends）**：每个功能点列出涉及的端（java / android / ios / h5 / web / bff），按关联
  接口归属子项目类型 + 页面绑定来源推导，可人工勾选校正。如"Banner 获取"= java + android
  （可能含 h5）。
- **功能点单测**：功能点详情页可发起单测——① 接口连通性（HTTP 可达、状态码、超时）；
  ② 期望数据（调用后返回字段匹配契约、关键字段值符合期望，如"列表非空、条数正确"）。复用
  runner/env 门禁/命令白名单，结果落 test_runs 并关联 feature_id；单测可只对某涉及端执行
  （如只测 android 端字段绑定）。
- **集成测试 bug 必须挂功能点**：integration 类测试发现的 bug 必须落到功能点（feature_id）；
  自动归属不了的进「待分配」队列人工挂载，挂上才算完整记录（§3.4）。
- **AI 对话（自带上下文）**：功能点详情页内置对话区。用户提问时，系统**自动组织上下文提示词**
  = 用户问题 + 该功能点最近单测结果/实测数据 + 相关契约/代码片段（带 provenance）→ 发送 LLM
  归因分析，回复存库可回看。例：后台配 5 个 Banner、按兴趣只推送给交友人群，实测只返回 3 个，
  用户问"数据库里有 4 个，为什么只给我 3 个？"——上下文带上实际返回、契约、过滤逻辑代码片段，
  LLM 给出归因。**LLM 只做解释/归因，不生成关键数值结论**（AGENTS.md）。

### 3.10 智能分级与 AI 建议规则（非死规则）

- **安全警告 ≠ 普通 bug（自动分级）**：命中安全敏感特征自动升为 `SECURITY_WARNING` 而非普通
  bug——如"获取用户信息接口返回用户密码/明文密钥/完整身份证/银行卡/手机号、未脱敏金额"。
  确定性特征（字段名 password/pwd/secret/token/idCard/bankCard/mobile、返回内容含密钥/密码
  格式）优先，LLM 语义补全（如接口虽不命中字段名但返回体含敏感明文）。安全警告 severity 高于
  普通 bug、排序优先、进 findings 同闭环（§3.4）。
- **自动风险判断**：对**金额、账号、鉴权、支付、隐私**等敏感域自动做风险评估（数据从静态
  关键路径提取 + LLM 评审），产出带 provenance 的风险建议。
- **AI 建议规则（提示词规则，可配置可逐条扫描）**：性能/风险类建议不靠死规则——提供
  **规则列表设置页**，每条规则 = 一段提示词（如"N+1 查询检测"、"事务边界过宽"、"越权校验缺失"
  、"金额精度/溢出"、"敏感字段明文返回"）+ 目标范围 + severity + 启停开关。扫描时按配置的规则
  **逐条逐个执行**（对变更影响面/全量范围应用规则提示词 → LLM 结构化输出命中项），产物统一进
  findings（detector=`ai-rule`），可豁免、可进修复建议。
- **提示词 AI 润色**：规则列表页每条提示词提供**「AI 润色」**按钮——把用户写的草稿发给 LLM
  优化（补全判据、约束输出 JSON 格式、增加正反例），返回润色版，**diff 对比确认后才覆盖保存**，
  保留人工最后拍板。

## 4. 数据模型（store 迁移 v17+）

沿用版本化迁移模式（追加，不改旧迁移）。

| 表 | 说明 | 关键列 |
|----|------|--------|
| `projects` | 项目（单用户、扁平） | id, name, source(local/git), local_path, git_url, git_ref, last_tested_sha, snapshot_sha, commands_json, env_name, analyzed_at |
| `project_modules` | 仓库内子项目（monorepo/混合仓库识别结果） | id, project_id, rel_path, kind_type, kind_role, build_tool, commands_json, last_tested_sha, analyzed_at |
| `env_requirements` | 依赖声明（推得或手写，可按子项目） | project_id, module_id, service(mysql/redis/jdk/android-sdk/...), category(middleware/toolchain), version, source(auto/manual), resolved_by |
| `env_services` | 中间件/工具链实例 | project_id, service, category, provider(container/external/installed), status(ready/missing/unsupported), host, port, endpoint, healthy, container_id, health_check_at |
| `env_devices` | Android 设备绑定（存库，手动才变） | project_id, name, method(usb/wireless/emulator), adb_host, adb_port, serial, avd, authorized, bound(bool), status(disconnected/connected), last_seen_at, created_at |
| `remote_nodes` | 远程执行节点（SSH 执行，能力标签路由） | id, name, host, port, user, auth(加密), host_key_fp, capabilities(ios-xcode/android-sdk/linux-docker), reachable, last_check_at, note |
| `intel_entities` | 实体↔表↔列映射（含 nullable） | project_id, module_id, entity, table, column, nullable, source_file, source_line |
| `intel_endpoints` | API 端点与字段契约 | project_id, module_id, method, path, response_type, request_json(参数名/类型/required/来源), fields_json(各字段 nullable/required/来源), source_file |
| `intel_android_bindings` | Android 必展示字段清单 | project_id, module_id, page, field_path, bound_ui, source_file, source_line |
| `intel_contract_snapshot` | 某次校验用的契约快照 | run_id, project_id, module_id, scope, schema_json |
| `test_cases` | 测试资产（自动发现，按子项目） | project_id, module_id, module, kind, framework, class, method, path, tags, last_status, last_duration_ms, flaky_count, last_run_at |
| `test_runs` | 一次测试执行 | id, project_id, module_id, scope(全量/module/class/affected), kind, command, status, started_at, finished_at, log_path |
| `test_results` | 单条执行结果 | run_id, project_id, module_id, case_id, kind, endpoint/page, passed, failures_json, rootcause_json |
| `test_case_results` | 远调用例级结果 | run_id, case_id, status, duration_ms, error_xml |
| `intel_issues` | 测试/业务问题闭环状态 | project_id, module_id, feature_id(归属功能点，integration bug 必填), key, kind(bug/security_warning), severity, location, commit_seen(第一个发现sha), commit_fixed(解决sha), status(open/resolved/regression/removed), resolved_at, last_check_at, detail_json |
| `intel_features` | 功能点（业务功能单元，单测/AI对话/问题挂载点） | id, project_id, name, summary, ends_json(java/android/ios/h5/web/bff), sort_order(拖动排序，人工优先), source(auto/manual), anchor(聚合锚点/关联接口列表), status, created_at, updated_at |
| `intel_chats` | 功能点 AI 对话记录 | id, feature_id, project_id, question, context_json(自动组织的上下文：单测结果/契约/代码片段+provenance), answer, created_at |
| `intel_ai_rules` | AI 建议规则（提示词规则，可配置可逐条扫描） | id, name, prompt, scope(all/affected), target(category), severity, enabled, order, polished_from(润色来源), updated_at |
| `intel_fixes` | 修复建议（草稿→人工应用） | project_id, issue_id, kind(ai-suggest/manual), title, diff_json(文件+行+新旧内容), status(proposed/applied/rejected/expired/rolled_back), applied_backup(原文备份), write_mode(direct/patch/clipboard/git-branch), applied_at, applied_by, rollback_at |
| `intel_findings` | 安全/合规审计问题 | project_id, module_id, detector(dep-check/trivy/spotbugs/npm-audit/rule/llm/ai-rule), severity, category, cve_or_rule_id, location(file:line), summary, status(open/resolved/removed/false_positive/waived), removed_at, waived_reason |
| `intel_overrides` | 人工校正覆写层 | project_id, module_id, target(表,row_key), field, auto_value_json(含溯源), manual_value, confidence, status(pending/applied/superseded/rejected), source(table/manual/llm-suggest), anchor(file:line/commit), created_at |
| `settings`（复用 v1） | 测试页全局设置 KV | `intel.repos_dir`、`intel.env.*`、`intel.workers`（测试 Worker 并发，默认 1）、`intel.device.*`（默认绑定设备） |

溯源约定：所有从代码提取的行带 `source_file + source_line`；人为设定（如"前端必展示"）标
`provenance='manual'`。`intel.repos_dir` 为 Git URL 项目的落地根目录；**外部环境凭证与中间件
自动拉起生成的口令一律加密落库**（页面遮蔽显示），命令模板按项目白名单约束、参数运行时注入。
**日志与产物（log_path）留存默认 30 天 / 单项目容量上限，可在全局设置调整，超限自动清理**
（audit 记录与 findings 历史不受影响）；旁路产物（bug 报告/子集）随 issue 保留。

> **⚠️ 实现现状：上表漏记了 RAG 向量表 `intel_chunks`**（迁移见
> `internal/store/migrate.go` 的 `migrationIntelRag`）：列
> `project_id/module_id/kind/ref_id/title/content/source_file/source_line/embedding`，
> 供 `/api/intel/ask` 与功能点 AI 对话做余弦召回。`embedding` 在 **PostgreSQL 上是
> `vector(1024)` + `vector_cosine_ops` 的 HNSW 索引**，在 **SQLite 上退化为 TEXT 存
> `[1,2,3]` 文本、检索走进程内 cosine 全量比较**（`internal/store/rag.go`）。固定 1024 维来自
> 默认 embedding 模型 bge-m3，换模型需重建索引。**为什么选 pgvector/HNSW、为什么做 SQLite/PG
> 双方言而非标准栈的 MySQL 8**，属技术栈偏离，评审材料见
> [`docs/TECH_DEVIATION.md`](TECH_DEVIATION.md) 的偏离项 ②③。
>
> 另：本段"日志产物默认留 30 天、超限自动清理"**未实现**——`test_runs`/`test_results` 只在
> 删除项目时随 `project_id` 一并删除，没有保留期回收（详见 §7 标注的 janitor 缺口）；
> `intel.workers` / `intel.env.*` / `intel.device.*` 也非代码内常量，而是页面写入的
> `settings` KV 命名约定，只有 `intel.repos_dir` 真正被读取。

## 5. API 设计（新增端点，Token 鉴权，风格对齐现有编排 API）

```
POST /api/intel/projects                     创建/注册项目（本地路径或 Git URL）
GET  /api/intel/projects                     项目列表（含检测出的 type/role/命令白名单）
POST /api/intel/analyze   {"projectId"}      异步分析：扫描+契约+测试资产发现+命令白名单生成
GET  /api/intel/projects/{id}                项目详情（type/role/provenance/commands_json）
GET  /api/intel/projects/{id}/modules        仓库内子项目清单（monorepo 识别结果，含 type/role）
PUT  /api/intel/projects/{id}/commands       审核/修改命令白名单（逐条增删改，保存即生效）
POST /api/intel/settings/probe-repos {"newPath"}     修改 repos_dir 前预检：返回受影响项目数、
                                                     待删除缓存大小、原路径数据保留量（供确认弹窗显示）
POST /api/intel/settings/repos-rebuild {"confirm":true, "newPath"}
                                             变更 repos_dir 后的整仓重建（两阶段：先 probe 再确认+审计）
PUT  /api/intel/projects/{id}/commands-modules  按子项目分别调整命令白名单/角色
GET  /api/intel/{project}/endpoints          接口契约清单
GET  /api/intel/{project}/entities           表/列/nullable + provenance
GET  /api/intel/{project}/test-cases         测试资产清单（按 kind/framework/module 过滤）
POST /api/intel/run       {"projectId","scope","kind[]","module[]","caseIds[]"}
                                              异步执行（scope=all|module|class|affected|manual）
GET  /api/intel/runs/{id}                    执行状态 + 进度 + 日志
GET  /api/intel/runs/{id}/results            逐项结果
GET  /api/intel/issues                       未解决问题清单（open 跟随随测）
POST /api/intel/issues/{id}/ack              人工标记/复核
GET  /api/intel/results/{id}/rootcause       归因报告
POST /api/intel/sync/scan   {"projectId"}    增量重扫（只取变更文件 + 失效缓存）
POST /api/intel/env/ensure  {"projectId"}    检测依赖/工具链/设备→逐项列出缺失（异步）
GET  /api/intel/env/status                   本机环境逐项状态（中间件/工具链/设备，含 unsupported）
POST /api/intel/env/install {"service","version"}   单个缺失环境一键安装（逐项，不批量）
POST /api/intel/env/devices/connect {method,ip,port}  设备连接（无线 ADB 需手机授权）
PUT  /api/intel/env/devices/{id}/bind        选择已有/新增 → 绑定，存库（手动才变）
DELETE /api/intel/env/devices/{id}           解绑
POST /api/intel/nodes                        新增远程执行节点（IP/端口/用户/口令或密钥、能力标签）
GET  /api/intel/nodes                        节点列表 + 可达性
PUT  /api/intel/nodes/{id}/check             手动触发节点连通性/能力自检
DELETE /api/intel/nodes/{id}                 删除节点
POST /api/intel/run 附加 {"node"}            指定本轮执行路由（缺省本机，按能力标签自动匹配）
POST /api/intel/scan/findings {"projectId","detectors[]"}   依赖漏洞+代码/合规扫描（异步）
GET  /api/intel/findings                     审计问题清单（按 severity/status/detector 过滤）
GET  /api/intel/findings/{id}                单条审计问题（含 CVE 或规则详情）
POST /api/intel/findings/{id}/waive          误报/接受风险（false-positive/waived，填理由+审计，可取消）
POST /api/intel/sbom {"projectId"}           生成/导出项目 SBOM（CycloneDX）
GET  /api/intel/fixes                       修复建议清单（proposed 待核验）
POST /api/intel/fixes/{id}/apply             人工核验后应用修复（写回被测项目，审计留痕）
POST /api/intel/fixes/{id}/reject            驳回修复建议
POST /api/intel/fixes/{id}/rollback          回滚已应用的修复（恢复备份/diff基线，审计留痕）
POST /api/intel/overrides/suggest {"projectId","instruction"}   提示词→覆写草稿（表格预览，不生效）
POST /api/intel/overrides                    批量应用确认后的覆写（specific / rule）
GET  /api/intel/pending                      待确认队列（medium/low 置信度 + 锚点变更待复核）
POST /api/intel/pending/{id}/confirm         确认/修正后固化
GET  /api/intel/features {"projectId"}      功能点清单（含 ends/sort_order/最近单测状态）
PUT  /api/intel/features/order {"projectId","order[]"}   拖动排序保存（人工优先，重扫不覆盖）
POST /api/intel/features {"projectId"}      新建/合并/拆分功能点（人工校正）
PUT  /api/intel/features/{id}               改名/调涉及端（ends）/聚合锚点调整
GET  /api/intel/features/{id}               功能点详情（单测记录/挂载问题/AI对话）
POST /api/intel/features/{id}/test {"scope"}   功能点单测：接口连通性+期望数据校验（异步，可指定涉及端）
GET  /api/intel/features/{id}/chats          功能点 AI 对话历史（含自动组织的上下文）
POST /api/intel/features/{id}/chat {"question"}   提问→自动组织上下文（实测数据+契约/代码片段）→ LLM 归因
POST /api/intel/issues/{id}/link-feature {"featureId"}  为未分配 bug 挂功能点（integration bug 必挂）
PUT  /api/intel/features/{id}/issues         调整功能点挂载的问题集合
GET  /api/intel/ai-rules                     规则列表（提示词规则，含启停/severity/顺序）
POST /api/intel/ai-rules                     新增规则（提示词 + 范围 + 目标类别 + severity）
PUT  /api/intel/ai-rules/{id}                编辑/启停/调整顺序
POST /api/intel/ai-rules/{id}/polish         AI 润色提示词（返回润色版，diff 对比确认后覆盖）
POST /api/intel/scan/rules {"projectId","ruleIds[]"}  按配置规则逐条扫描（AI 建议/风险/性能，异步）
```

> **⚠️ 实现现状（本节是设计清单，非已交付接口表）**。`internal/server/server.go` 共注册
> **98 条路由**，其中 intel 侧 56 条。与本节对不上的地方：
> - **设计中写了但完全没有 handler 的端点**：`/api/intel/settings/probe-repos`、
>   `/api/intel/settings/repos-rebuild`（见 §3.1 标注）、`/api/intel/sync/scan`（增量重扫只能再调
>   `/api/intel/analyze`）、`/api/intel/sbom`（SBOM 在 analyze 时生成，随 `intel_overview` 的
>   `sbom_json` 列存库并经 `/api/intel/overview` 返回，**无独立导出端点**）、
>   `/api/intel/scan/findings`（实际只有 `GET /api/intel/findings` 列表 + `POST
>   /api/intel/findings/{id}/waive`，**扫描动作没有独立触发端点**）、`/api/intel/issues/{id}/ack`、
>   `/api/intel/issues/{id}/link-feature`、`/api/intel/runs/{id}/results`（`handleIntelRunByID`
>   只识别 `GET /api/intel/runs/{id}` 与 `POST .../cancel`）。
> - **路径归属与表格不同**：设计中写在 `/api/intel/projects/{id}/…` 下的
>   `modules`、`commands`、`commands-modules` **都不是子路径**——实际端点是
>   `GET /api/intel/modules`、`GET|PUT /api/intel/modules/`（`handleIntelModuleCommands`，按
>   moduleId 操作）；`/api/intel/projects/{id}` 只挂了 GET/PUT/DELETE 与 `sources` 子资源。
> - **`POST /api/intel/run` 的入参远少于设计**：结构体只有 `{projectId, moduleId, node, force}`；
>   没有 `scope`、`kind[]`、`caseIds[]`，因此 `scope=all|module|class|affected|manual` 与
>   "按能力标签自动匹配"未实现（`node` 为空即本机执行，见 §3.6 标注）。
> - **设计中没写、实际已存在的 intel 端点**（本节宜补录）：`/api/intel/index`、`/api/intel/ask`、
>   `/api/intel/chats(/{id})`、`/api/intel/overview`、`/api/intel/gateway-routes`、
>   `/api/intel/run-all`、`/api/intel/plan`、`/api/intel/impact`、`/api/intel/results/{id}/…`、
>   `/api/intel/fixes/generate`、`/api/intel/contracts/check(-batch)`、
>   `/api/intel/{android,web,ios}-bindings`、`/api/intel/env/{ensure,status,install,stop,external,
>   schema-init}`、`/api/intel/env/devices(/{id})`、`/api/intel/nodes(/{id})`、
>   `/api/intel/ai-rules(/{id})`、`/api/intel/overrides/{suggest,enqueue}`、
>   `/api/intel/pending(/{id}/confirm)`。
>
> 权威接口表以 `docs/API.md` 为准。

## 6. 配置页（webui）「测试」页

现有内嵌 SPA 新增「测试」功能，采用**两段式导航：项目列表 → 项目详情**。

- **列表页（项目列表）**：展示所有项目，行内显示 type/role、最近一次测试时间/通过率、
  环境状态摘要；提供「新建项目」「分析」「一键回归」入口。点进任一项目进入详情页。
- **详情页（项目详情 = 编排与操作主界面）**：
  - **子项目（modules）**：仓库级详情顶部展示子项目清单（monorepo 识别出的各类型，如 Java/
    Node/Android/iOS），每个子项目可独立进入其 type/role、命令白名单、契约与测试资产视图。
  - **功能点（features）**：项目详情进出功能点列表——卡片/表格展示，行内显示名称、涉及的端
    badge（java/android/ios/h5/web）、最近单测状态、挂载问题数（open 高亮）；**支持拖动排序**
    （人工优先，重扫不覆盖）；行内可改名、增删涉及端。点进功能点详情页：
    - **单测区**：一键发起功能点单测（接口连通性 + 期望数据校验，可指定只测某涉及端），
      结果实时回显（可达性/状态码/字段匹配/期望值断言），失败可跳归因。
    - **AI 对话区**：内置对话，提问自动带上该功能点的实测数据 + 契约/代码片段上下文，LLM
      归因回复；对话历史可回看。
    - **挂载问题**：展示归属该功能点的 open/resolved 问题，可把未分配 bug 手动挂到此功能点。
  - **智能规则（AI 建议）**：**规则列表设置页**——每条规则展示提示词、目标范围、severity、
    启停开关；可新增（写草稿提示词 → 「AI 润色」优化 → diff 对比确认保存）；「扫描」按配置的
    规则**逐条逐个执行**，命中项进 findings（detector=ai-rule）并可豁免/转修复建议。
  - **识别与校正**：识别结果表格行内可编辑（子项目 type/role、接口、字段、Android 必展示
    清单）；「待确认」队列入口（红点），逐条确认/修正；「批量修正」输入框 → 提示词转覆写
    草稿表格 → 勾选应用。
  - **命令白名单**：按 Profile 自动生成的命令清单（多类型仓库按子项目分列），默认全量列出、
    手动勾选放行，行内可增删改，保存即生效；展示每条命令允许的参数约束。
  - **测试资产**：按项目/子项目/kind/framework/module 过滤的用例清单；行内显示最近状态/耗时/
    flaky；**flaky 隔离区**单独展示被自动隔离用例（连续 N 次先败后转绿），支持手动解除隔离或
    标记为真实 bug。
  - **执行按钮**：「全量运行」「运行选中」（checkbox 多选用例/模块/子项目）「受影响运行」
    （默认 Git delta 影响面 + 未解决问题）「重跑失败」——全部后台异步执行，进度走 WS；执行前
    先过环境门禁，缺什么/不支持什么先在页面明示。
  - **结果**：本轮结果汇总（通过率/失败清单）+ 逐失败用例归因摘要 + 日志入口；「未解决
    问题」标签页（open 跟随随测）。重测后自动演进的：**漏洞类从活跃清单移除（历史可查）、
    Bug·警告类标记已处理**，页面提供「已处理/已移除/全部」过滤视图。
  - **修复建议**：失败/审计发现的可修项列 patch 草稿（diff 预览），逐条核验后点「应用」写回
    被测项目（应用即审计），或「驳回」；写回前可选形态（直接写文件/补丁/剪贴板/git 分支+提交）；
    **应用后可一键回滚**（恢复备份/diff 基线）；应用后自动复测该项。
  - **环境（env）**：中间件 / 工具链 / 设备 / 远程节点四块状态列表；缺失项逐个显示 + 各自
    「安装」按钮（不批量装）；已装工具链提供**卸载**入口；中间件容器支持启停/重置/**卷清理**；
    设备连接面板（选择已有 / 无线 ADB IP+端口 → 连接 → 手机授权 /
    USB 直连 → 绑定）；平台不支持项（如 Linux 下 iOS）标注"当前平台不支持"并引导「添加远程
    节点」（填 IP/端口/用户/口令，选能力标签如 ios-xcode）；外部中间件配置表单（IP/端口/
    账号/口令，落库可改）；**库初始化**区展示/触发 schema 迁移与数据初始化脚本。
  - **安全与合规（findings）**：按 severity/detector/状态过滤的审计问题清单；行内显示 CVE/
    规则号、位置（file:line）、状态（open/resolved/removed/false_positive/waived）；
    「扫描」按钮触发依赖漏洞+代码/合规审计；每条可**豁免/标记误报**（填理由，可取消，审计留痕）；
    提供**SBOM 导出**按钮。
- **全局设置**：独立入口（或详情页侧栏），配置 Git 临时仓库缓存路径（`intel.repos_dir`）、
  默认被测环境、**测试 Worker 并发数（默认 1，串行）**、默认绑定设备、Git clone 认证（token/
  SSH key，加密落库）、日志产物保留期/容量上限等。**修改 repos_dir 为两阶段**：① 提交新值 →
  预检返回确认项（原缓存下已 clone 项目数、将受影响项目、待删除缓存大小、原路径数据保留量）；
  ② 弹窗二选一「不重建」（保留原缓存目录与已 clone 仓库，数据继续可用）/「重建」（确认删除
  原缓存目录并重新 clone，相关项目标为待重新分析），全程审计留痕。

> **⚠️ 实现现状（本节多数控件已落地，以下几项没有）**
> - **repos_dir 两阶段修改**：无预检/确认端点与前端弹窗（见 §3.1 标注）。
> - **测试 Worker 并发数**：后端已支持可调（2026-09-20：`intelSem` 动态信号量 + `intel.workers`
>   setting，`GET/POST /api/intel/settings` 即时生效），**但页面无该设置项**；默认仍编译期
>   常量 `intelExecConcurrency = 2`（**不是**本节所述的"默认 1"）。
> - **Git clone 认证（token/SSH key 加密落库）**：**完全没有实现**——`IntelProject` 无
>   `git_token`/`ssh_key` 之类字段，`cloneGitRepo`/`ensureGitClone` 也不注入任何凭据，
>   私有仓库只能依赖主机自身的 git 凭据（§9 承诺的"git 私钥/token 加密落库"同此）。
>   （中间件口令与远程节点 SSH auth 的加密落库**已实现**，见 `internal/store/crypto.go`。）
> - **flaky 隔离区**：核心状态机已实现（2026-09-20：test_cases 加 `quarantined` 列，flaky_count
>   累计达阈值 3 次即隔离；`flakyRetry`/`recordRunIssues` 跳过已隔离用例；`POST /api/intel/test-cases`
>   endpoint 解除隔离），但**前端隔离区视图**未做；`flakyRetry` 仍仅 go 报告生效。
> - **工具链卸载入口 / 中间件卷清理**：`/api/intel/env/install` 只有装没有卸；env 路由里没有
>   卷清理端点（`/api/intel/env/stop` 只做容器停止）。
> - ✅ 已实现且与描述一致：两段式导航、功能点列表/详情/拖动排序/涉及端、单测区与 AI 对话区、
>   命令白名单、待确认队列与批量覆写、findings 过滤与豁免/误报、SBOM 导出按钮（前端
>   `downloadIntelSbom()` 拉 `/api/intel/overview` 的 `sbomJson` 落盘，**不经后端导出端点**）、
>   设备连接/绑定、环境四块状态与逐项安装。

### 6.1 Star Burst APP 端对应变动

本功能是 Star Burst 的后端能力（Web 已有「测试」页），Star Burst 移动 APP 作为同一账号体系的
客户端，需要**能力对等**：APP 内也要能看功能点、发起单测、AI 对话、接收测试结果推送。

- **入口与导航**：APP 首页或工作台新增「测试智能」入口，进入后复用现有导航结构（项目列表 →
  项目详情 → 功能点列表 → 功能点详情）。
- **功能点列表**：与 Web 一致的列表展示（名称/涉及端 badge/最近单测状态/未解决问题数）；
  **拖动排序、改名、调整涉及端**能力对齐 Web，操作立即同步后端（同一接口）。移动端交互以长按
  拖动排序、滑动单测结果、卡片化展示为主。
- **功能点详情（单测 + AI 对话）**：发起单测（接口连通性/期望数据，可指定涉及端）；结果入
  报告页；**AI 对话区**与 Web 同接口，提问自动带上功能点实测上下文，回复展示在对话流。
- **结果与通知**：测试运行进度/结果通过既有 APP 推送通道（push WS 桥接 APNs/FCM 或走后端
  push 事件）推送到 APP：单测完成、发现问题、安全警告、需人工挂功能点的未分配 bug。
- **发现与处理闭环**：问题清单（含 SECURITY_WARNING 高亮置顶）、修复建议 diff 预览、豁免/标记
  误报均可在 APP 完成；复杂重活（环境管理、命令白名单、全局设置、规则列表编辑）仍在 Web，
  APP 聚焦"看结果 + 单测 + 对话归因 + 问题盯办"。
- **鉴权**：沿用现有 APP 登录/Token（同一账号体系），不新增权限模型；接口复用 `/api/intel/*`
  同一套端点，APP 与 Web 共享一套数据，无独立实现。

## 7. 与现有能力集成

- **任务化**：analyze/run 复用任务状态机/重试/`workflow_id`；步骤间用 `dependsOn`（analyze →
  run → rootcause → fix-apply 后复测）。Git 仓库的 clone/pull 在同目录 sessionGate 串行。
- **测试 Worker 并发**：独立于 OpenCode 编排 worker，数量在**页面全局设置「测试并发」**中配置，
  **默认 1（串行）**，可放宽为并行；改动即时生效，不重启。（存量 `--workers` flag 只管编排
  任务，与测试执行互不影响。）

> **⚠️ 实现现状（本条"任务化"与"并发可配"均未兑现——本文档最主要的两处承诺未兑现）**
> - **intel 完全没有接入任务状态机**。`internal/store/task.go` 的 `Task` 结构体**没有 `Kind`
>   字段**，`tasks` 表也没有 kind 列；全仓创建任务的**生产**调用点只有 5 处
>   （`CreateTask`：`internal/server/batch.go:61`、`internal/server/scheduler.go:96`、
>   `internal/automation/engine.go:146`；`CreateTaskWithStatus`：`internal/server/tasks.go:249`、
>   `internal/server/workflow.go:82`），**没有一处来自 intel**（`internal/server/intel*.go` 内
>   既不出现 `store.Task` 也不出现 `CreateTask`）。§3.6 的 env 操作、§3.4 的 fix-apply 同样
>   **不是** task kind，只是各自 handler 里的同步/异步处理。
> - **直接后果（intel 运行拿不到的能力）**：因为不进 `tasks` 表、不进 `internal/tasks` 执行器，
>   intel run **没有** —— ① **自动重试与退避**（`attempts`/`available_at` 只对 tasks 生效）；
>   ② **`dependsOn` 依赖编排**（无法表达 analyze → run → rootcause → fix-apply → 复测）；
>   ③ **优先级排序**（`priority` 列不参与 intel 派发）；④ **worker 池**（`--workers` /
>   `--max-concurrency` 只管编排任务）；⑤ **启动崩溃恢复**——`RecoverStaleRunning`
>   （`internal/store/task.go`）只在 `internal/tasks/executor.go` 的 `Run()` 里调用，
>   **`test_runs` 没有等价物**：后端重启后遗留的 `running`/`queued` run 会永久停留在该状态，
>   既不重置也不标失败；⑥ **janitor 清理**——`internal/tasks/janitor.go` 的保留期回收
>   （`--task-retention`）只扫 `tasks`，`test_runs`/`test_results` 及其输出会无限增长（§4
>   所称"日志产物默认留 30 天、超限自动清理"亦无实现）。
> - **实际形态**：`internal/server/intel_runner.go` 的自有派发 —— `enqueueIntelRun` 建行后
>   `go runIntelJob(...)`，靠 `intelExecSem`（`const intelExecConcurrency = 2`，**编译期常量、
>   不可热改**）限制全局进程数，靠 `intelExecMutex(projectID)` 做同项目串行，靠 `intelCancels`
>   + `POST /api/intel/runs/{id}/cancel` 做取消；进度走独立事件 `intel.run.event`（非
>   `task.event`），单次超时 `intelRunTimeout = 10 分钟`、run 内输出截断 `intelOutputLimit =
>   256 KiB`。§4 的 `intel.workers` 设置项**全仓无人读取**。
> - **仍有价值**：本节设计（把执行下沉为 task kind）是**待实施的规划**，不是对现状的描述；
>   实现前请以本标注为准。
- **执行 kind**：`tasks` 新增 `kind=test-run`（os/exec 跑确定性命令）、`kind=env`（容器拉取/
  启停/工具链安装）、`kind=audit`（漏洞/静态扫描）、`kind=fix-apply`（应用修复，唯一写路径）；
  与现有 prompt kind 并存；worker 池、取消、超时复用；命令必须落在**项目命令白名单**内。

  > **⚠️ 实现现状：`kind=test-run` / `env` / `audit` / `fix-apply` 四种 task kind 全部未落地**，
  > `Task` 结构体与 `tasks` 表都**没有 kind 列**。四类工作分别由
  > `internal/server/intel_runner.go`（测试执行）、`internal/server/env.go`（环境供给）、
  > `internal/server/intel_audit.go` + `internal/intel/{security,compliance,deps,sbom}`（审计扫描）、
  > `internal/server/intel_audit.go` 的 `handleIntelFixAction`（修复 apply/reject/rollback）
  > **各自实现**，互不共享 worker 池/重试/依赖，
  > 也**不"与现有 prompt kind 并存"**（因为它们根本不在 tasks 体系内）。详见本节上方标注与文末
  > §11 偏差清单。已实现的约束只有"命令必须落在项目命令白名单内"。
- **执行 transport（本机 / 远程节点）**：runner 抽象为可插拔 executor——本机 `os/exec` 与
  远程 `SSH`（golang.org/x/crypto/ssh，已在依赖中）两套实现；远程执行流式回传输出、scp 拉回
  报告，解析与落库走同一管道；节点按能力标签路由，所有远程命令同样受命令白名单约束。

  > **⚠️ 实现现状：与括号内所述不符**（完整逐条对照见 §3.6「远程执行节点」下的标注）。要点：
  > 全仓**无 `golang.org/x/crypto/ssh` import**，远程走 `envagent.RunSSH` 起**系统 `ssh` 二进制**；
  > 输出用 `CombinedOutput()` **一次性回传，非流式**；**无 scp、无产物回拉**；因此
  > **仅 `go` 报告类型允许远程执行**，`xcodebuild`/`gradlew`/`mvn` 会被硬拒；**无按能力标签路由**
  > （`node` 参数为空即本机）。"可插拔 executor"抽象也未出现——远程分支是 `intel_exec.go` 里的
  > 一段 `if remoteNode != nil` 内联逻辑。
- **执行前置**：一次 run 前先 `ensure-ready`（env 门禁），分三类响应（就绪 / 缺失可修给途径 /
  平台不支持明确告知），环境故障归因 `ENV_ISSUE`（区分 missing/unsupported），不误报到业务代码。
- **推送**：新增 `intel.ready` / `intel.progress` / `intel.done` / `test.result` / `env.health` /
  `audit.finding` / `fix.suggest` / `feature.test.done`（功能点单测完成）/
  `feature.chat.answer`（AI 对话回复就绪）事件；APP 与 Web 走同一事件源，路由到对应端展示。

  > **⚠️ 实现现状：上述 9 个事件名在代码里一个都不存在。**全仓 `hub.Broadcast` 的 intel 侧只有
  > **单一事件类型 `intel.run.event`**（`internal/server/intel_runner.go` 的
  > `pushIntelRunEvent`，载荷是整条 `test_runs` 行），靠 `run.status`
  > （queued/running/passed/failed/canceled）+ `run.progress` 表达全部语义；env 门禁结果、
  > 审计 findings、修复建议、功能点对话**均无独立事件**，前端只能靠拉取。编排侧仍只有
  > `task.event` / `session.event` / `subscribed`。
- **自动化**：`rules.kind` 扩展 `intel-run`，支持 GitLab push/tag webhook + cron 周期回归；
  触发时自动带 last_tested_sha 做增量。✅ 复用 `/api/webhook` 基础设施。
- **审计**：analyze/run/执行命令/env 操作/命令白名单修改/覆写修改/修复应用全部走既有 API
  审计中间件；命令与实际参数逐条记录；写操作（修复应用）单独高亮记录。
- **批量**：`/api/batch` 支持对多个项目并行下发分析/执行。

  > **⚠️ 实现现状（自动化与批量两条均未兑现）**
  > - `rules.kind` 的取值只有 `cron` / `git` / `http` 三种（`internal/store/rule.go` 的
  >   `TriggerCron/TriggerGit/TriggerHTTP`，`internal/server/rules.go` 按此校验），**不存在
  >   `intel-run` kind**；`internal/automation/engine.go` 命中规则后调 `store.CreateTask`
  >   下发的是 **OpenCode prompt 任务**，**不会**触发 analyze/run 或带 `last_tested_sha` 的增量回归。
  >   （✅ 部分成立：`/api/webhook` 基础设施确实存在并被 `rules.go` 复用为 `http` 触发入口。）
  > - `/api/batch` 的入参是 `{prompt, targets[]}`，为每个 target 建一条 `store.Task`
  >   （`internal/server/batch.go`），**只支持批量下发编排 prompt**，**没有**"对多个项目并行
  >   下发分析/执行"的能力。

## 8. 分阶段实施

> **⚠️ 实现现状**：下表是**规划表**，不是进度表。当前状态：M1/M2/M3/M5/M6/M8 的能力**大体已
> 落地**，但 M3 的"`test-run` kind"与"flaky 自动隔离"、M5 的"可卸载/卷清理/能力标签路由"、
> M7 的"intel-run 规则 + SBOM 导出端点"、M4 的写回形态选项与 §7 全部集成假设**均未按表中措辞
> 实现**（M9 属移动端仓库，本仓无从核对）。逐条以文中各「⚠️ 实现现状」标注和 §11 清单为准。

| 阶段 | 内容 | 验收 |
|------|------|------|
| M1 项目+扫描器 | projects 扁平模型 + type/role 自动识别（含 monorepo 子项目 modules）+ scanner（JPA/Flyway/Controller/DTO → entities+endpoints，带 provenance） | 注册一个混合仓库，能拆出 Java/Node/Android 等子项目并正确识别类型；Java 子项目可查出"列表接口"底层实体表、返回字段、nullable |
| M2 Git delta | 故障缓存校验 + git log/diff 影响面推导（含变更文件分类规则：构建/依赖/配置/源码）+ ref/force-push 回退全量 + 问题闭环（intel_issues） | 提交 A→发现问题→提交 B→只重测影响面+未解决问题，解决后跳过；改 pom.xml/application.yml 分别放宽到全模块/全部集成测试；force push 后正确回退全量；findings/issues 随重测自动演进（漏洞移除/Bug标记已处理） |
| M3 测试资产+执行 | 测试发现分类入库 + `test-run` kind + 命令白名单生成/审核 + surefire/playwright 报告解析 + webui「测试」页 + flaky 自动隔离 | 页面上能选某 Java 项目的 JUnit 批量/范围运行，结果回写展示；命令白名单可审核修改；连续失败的用例被隔离并告警、可解除 |
| M4 修复应用 | rootcause 产出修复建议 + diff 预览 + 「应用/驳回」手动写回 + 备用/回滚 + git 分支选项 + 审计 | 一个失败项能生成修复建议，人工应用（可选写文件/补丁/剪贴板/git分支）后复测转绿；应用后能一键回滚 |
| M5 环境管理 | 中间件/工具链/设备/远程节点四块检测 + 缺失逐项一键安装（幂等+校验和+可卸载）+ 中间件容器（数据卷持久化/口令加密落库/卷清理）/外部配置 + 库环境初始化 + 设备绑定(USB/无线ADB) + 远程节点(SSH/能力标签) + unsupported 提示 + 门禁 | 缺 MySQL/构建环境时逐项引导安装；安装可卸载；Linux 下 iOS 明确提示不支持并可添加远程节点；中间件就绪后 schema/数据初始化完成才执行测试 |
| M6 Android 校验 | 必展示清单 + 契约↔界面差集 + 空值检查 | 后端某必展示字段返回 null 能报 CLIENT_MISSING_FIELD |
| M7 审计+自动化 | findings 扫描（去重+快照缓存）+ 误报/豁免通道 + SBOM 导出 + intel-run 规则（push/tag/cron）+ 统计接入 | 一条 cron 周期回归 + GitLab push 触发，失败实时推送；重复漏洞不重复入库；可豁免误报并留痕；能导出 SBOM |
| M8 功能点+AI | 功能点聚簇识别 + 拖动排序/涉及端 + 功能点单测 + 问题挂功能点 + AI 对话（自带上下文）+ 智能分级（SECURITY_WARNING）+ AI 建议规则（提示词可配+润色+逐条扫描） | 识别出"首页 Banner"等功能点并可拖拽排序、标注涉及端；功能点单测通断+期望数据校验；集成测试 bug 自动挂功能点；"获取用户信息返回密码"能报安全警告而非普通 bug；配一条 N+1 规则扫描出命中并润色提示词 |
| M9 Star Burst APP | APP 端"测试智能"入口 + 功能点列表/详情 + 单测发起/结果 + AI 对话 + 推送 | APP 上能打开功能点列表、发起单测并收到结果推送、在功能点内提问 AI 并看到带上下文的归因回复 |

## 9. 边界与安全

- **确定性优先**：表↔列↔nullable、必填推断、测试资产分类从代码静态提取；LLM 不得生成表结构
  或必填结论，只做语义补全与归因。LLM 产物标记 `llm-proxy`，默认不参与硬断言。
- **命令白名单（每项目一份）**：执行只允许该项目详情页审核过的命令模板（Profile 生成默认 +
  人工增删改），不支持任意 shell（防注入也防误操作）。
- **不主动改被测代码**：本功能默认只读被测仓库。唯一写路径 = 修复建议经用户核验后在页面点
  「应用」（git 项目可选生成补丁文件）；每次应用记为一次写审计。
- **凭证**：外部环境 token、DB 连接串、git 私钥/token 不落库明文——**一律加密落库或运行时
  注入**；中间件自动拉起生成的随机口令与外部配置一致加密落库并页面遮蔽（服务重启直接复用）；
  **远程节点 SSH 口令/密钥同样加密落库**，启用 host key 指纹校验（SSH-FP）默认证入，防止中间人；
  巡检日志对口令脱敏，明文凭证不出现在 API 响应与日志。

  > **⚠️ 实现现状**：中间件口令（`env_services`）与远程节点 SSH auth（`remote_nodes.auth`）
  > 确实走 AES-256-GCM 加密落库（`internal/store/crypto.go`），host key 指纹字段亦已落库；
  > 但 **"git 私钥/token 加密落库" 未实现**（无任何凭据字段与注入路径），见 §11 第 10 条。
- **单用户访问**：只有本人通过 VPN 使用，**不设细粒度权限/RBAC**；沿用既有基础鉴权
  （Web Session 加 APP Token），不向第三方开放。
- **成本**：全量 LLM 分析按文件量节流；增量模式下 LLM 仅处理变更影响面与未解决问题。
- **AI 对话/建议规则边界**：功能点 AI 对话、AI 建议规则均为**只读分析 + 归因/建议**，不改被测
  代码、不写入项目；上下文中的实测数据（单测结果/返回体）与代码片段带 provenance，LLM 不得
  生成表结构/必填/数值等关键结论；AI 建议规则扫描按用户配置的提示词执行，产物进 findings 走
  人工豁免/修复流程；提示词润色仅改写规则提示词，不接触被测仓库。
- **范围**：契约/API 校验默认对被测环境只读；需要写（建数据/下单做穿透）时以命令白名单内
  的测试命令推进，不走任意 HTTP 写。

## 10. 风险与遗留

- **跨文件调用链解析**：Controller→Service→Repository→Entity 精确还原在纯静态下复杂度高；
  M1 先报"模块内实体集合 + 端点字段契约"（够支撑字段级校验），跨链精确到表属 M2 增强。
- **无 OpenAPI 前提**：多数项目没有现成 OpenAPI、部分禁 Swagger，契约全靠代码反推——
  确定性扫描器正是为此设计。
- **平台能力（iOS）**：Linux 主机无 Xcode/模拟器，iOS 相关测试在门禁中标记 `unsupported`
  明确告知，并可**添加远程 macOS 节点**（能力标签 `ios-xcode`）经 SSH 执行 XCUITest 并拉回
  报告；无 macOS 节点时该项保持不可执行。

  > **⚠️ 实现现状：只有前半句成立**。门禁把 iOS 相关项标 `unsupported` 已实现；但"添加 macOS
  > 节点 → 经 SSH 跑 XCUITest → 拉回报告"整条链路**当前不可用**：远程执行硬性只放行 `go`
  > 报告、无产物回拉、无按能力标签路由（详见 §3.6 标注）。也就是说 iOS/Android 真机这类
  > "必须去别的平台跑"的场景，现在事实上**仍不可执行**，与"无 macOS 节点时不可执行"没有区别。
- **漏洞库在线可达（公网）**：当前环境公网可达（github.com/ghcr.io/osv.dev 通，nvd.nist.gov
  反爬 403、proxy.golang.org 超时）——漏洞扫描在线拉取为主，个别域名受限时回退镜像/离线缓存
  （见 §3.7）。⚠️ **Nexus(192.0.2.150:8081) 是 OSS 制品仓库，不含漏洞数据**（漏洞防护属
  商业 Nexus Lifecycle/IQ），依赖漏洞库依赖公网或本地缓存，不能指望 Nexus。
- **被测环境依赖**：穿透校验需要被测服务在本机或远程节点可访问；与 GitLab/TeamCity 对接属
  后续独立工作。
- **多项目并发**：同一项目 repo 的分析/执行需按目录串行（复用 sessionGate 思路），避免两轮
  git pull 竞争；不同项目可并行（受测试 Worker 并发数控制）。

## 11. 文档与实现的偏差清单（集中收口）

本节把上文各「⚠️ 实现现状」标注的结论一次列全，便于评审与排期。核对方式：逐条读
`internal/server/`、`internal/intel/`、`internal/store/`、`internal/tasks/` 源码与
`internal/server/server.go` 的路由注册表（共 **98 条路由**，其中 intel 侧 56 条），**不采信
文档自述**。文中所有行号会随提交漂移，故一律以"文件 + 符号名"定位。

| # | 小节 | 文档声称 | 代码实际 | 影响 / 后果 | 处置建议 |
|---|------|----------|----------|-------------|----------|
| 1 | §7 任务化、§3.5 执行、§8 M3 | analyze/run 复用任务状态机，`tasks` 新增 `kind=test-run/env/audit/fix-apply` | `store.Task` **无 `Kind` 字段**、`tasks` 表无 kind 列；5 处生产建任务调用点无一处来自 intel；intel 走 `intel_runner.go` 自有派发 + `intelExecSem` + `intelExecMutex` + `intelCancels` | intel run **拿不到重试、`dependsOn`、优先级、worker 池、`RecoverStaleRunning` 崩溃恢复、janitor 清理**；`test_runs` 里重启前 `running`/`queued` 的行永久悬挂 | 保留设计为待办；短期给 `test_runs` 补启动期"孤儿 run 置 failed"，长期再下沉为 task kind |
| 2 | §3.6、§7 执行 transport、§10 iOS | 远程执行用 `golang.org/x/crypto/ssh`（"已在依赖中"）、流式回传、scp 拉回产物、按能力标签路由 | 全仓**无 `crypto/ssh` import**（x/crypto 仅 bcrypt）；实际 `envagent.RunSSH` 起**系统 `ssh` 二进制** + `CombinedOutput()` 一次性收集；**无 scp/产物回拉**；`reportKind != "go"` 直接拒绝；`capabilities` 列有表无路由逻辑 | 去 macOS 跑 `xcodebuild`、去远端跑 Gradle/Maven **整条路走不通**，而"补 macOS 节点"正是文中引入远程执行的头号理由；`intel_exec.go` 内"capability labels"注释属误导 | 先补产物回拉（scp/tar over ssh）再放开非 go 报告；capabilities 要么实现自动匹配要么从文档/字段语义降级为"人工选节点" |
| 3 | §6 全局设置、§3.1 缓存路径、§5 | `intel.repos_dir` 两阶段修改（预检 → 确认 → 破坏性删除重建），默认路径 `~/.local/share/starburst-backend/intel-repos` | `probe-repos` / `repos-rebuild` **两个 handler 都不存在**；无 repos_dir 变更清理逻辑；**无默认值**（读不到即报 `intel.repos_dir not configured`） | 改路径后旧缓存既不清理也不重建，磁盘只增不减；未配置时 Git 项目完全无法分析 | 二选一：实现两阶段端点，或把本节降级为"仅提示"并手动清理 |
| 4 | §6/§7 测试并发、§4 `intel.workers` | 并发数页面可配、默认 1（串行）、热生效不重启 | **已实现（2026-09-20）**：`intelSem` 动态信号量（`intel_runner.go`）替代固定容量 channel；`intel.workers` setting 驱动，`GET/POST /api/intel/settings` 可调、启动时恢复；cap 变化即时生效 | 默认仍 `intelExecConcurrency=2` 并行（与文档"默认 1"不同，已改口径）；页面设置项待补 | ✅ 并发可配已兑现（后端 + API）；前端全局设置项未做 |
| 5 | §7 推送 | 9 个新事件类型（`intel.ready`…`feature.chat.answer`） | intel 侧只有 **`intel.run.event`** 一种，语义靠 `run.status`/`run.progress` 表达 | 前端无法区分"环境就绪/审计发现/修复建议/对话回复"等语义，只能轮询 | 事件名以代码为准改写本节，或按语义补事件类型 |
| 6 | §7 自动化、§7 批量 | `rules.kind` 扩展 `intel-run`；`/api/batch` 可并行下发分析/执行 | 规则 kind 只有 `cron/git/http`，命中后建的是 **prompt 任务**；`/api/batch` 同样只建 prompt 任务 | 周期回归、push/tag 触发回归**当前无法自动跑起来**（§8 M7 验收不成立） | 新增 `intel-run` 触发类型并把 target 指向 intel 队列 |
| 7 | §3.5/§6 flaky | 连续 N 次先败后绿 → `flaky_quarantined`、自动剔除出增量范围、页面隔离区可解除 | **核心已实现（2026-09-20）**：test_cases 加 `quarantined` 列（迁移 `intel_flaky_quarantine`），`UpdateIntelTestCaseOutcome` 在 flaky_count 累计达阈值（3 次）时置隔离；`flakyRetry`/`recordRunIssues` 跳过已隔离用例（不再重跑/建 issue）；`POST /api/intel/test-cases`（endpoint）解除隔离。`flakyRetry` 仍仅 go 报告、同轮重跑预算 1 | 不稳定用例达 3 次后被隔离并告警、可解除（issue 不重复生成）；跨报告类型（JUnit/playwright）flaky 判定与页面隔离区视图仍缺 | ✅ 核心隔离状态机已兑现；页面隔离区 UI + 跨类型 flaky 判定待补 |
| 8 | §5、§3.7 SBOM 与扫描触发 | `POST /api/intel/sbom`、`POST /api/intel/scan/findings`、`/api/intel/sync/scan`、`/api/intel/issues/{id}/ack`、`/api/intel/issues/{id}/link-feature`、`/api/intel/runs/{id}/results` | 上述端点**均未注册**；SBOM 随 `intel_overview.sbom_json` 由 analyze 产出、前端 `downloadIntelSbom()` 下载；findings 扫描随 analyze 触发；issue 只有列表 | 依赖本节接口表的调用方会拿到 404/405 | 以 `docs/API.md` 为权威，本节改为"设计提案" |
| 9 | §3.4/§6 修复写回形态 | 写回可选 直接写文件 / 补丁文件 / 剪贴板 / 建分支+提交，四选一 | **已实现（2026-09-20）**：`POST /api/intel/fixes/{id}/apply` 接受 body `writeMode`；`applyIntelFix` 分派 `file`（直接写+备份）/`patch`（`fix.GeneratePatch` 写 `intel-fix-<id>.patch`，不改源码）/`branch`（git 分支 `intel-fix-<id>` 提交，非 git 退避 file）；备份 + `rollback` 保持 | 主干污染可用 patch/branch 规避；「剪贴板」由前端取 patch 文本实现 | ✅ file/patch/branch 三形态已实现；剪贴板形态由前端承接 |
| 10 | §3.1/§6/§9 Git 凭据 | clone 认证 HTTP token / SSH 私钥加密落库、运行时注入 | `IntelProject` 无凭据字段，`cloneGitRepo`/`ensureGitClone` **不注入任何凭据**（§9 该条对 git 不成立；中间件口令与节点 SSH auth 确实已 AES-256-GCM 加密落库） | 私有 GitLab 仓库只能在主机侧预置凭据，页面配置项是空头承诺 | 补凭据字段 + `GIT_ASKPASS`/ssh-agent 注入，或删掉该承诺 |
| 11 | §3.6 依赖声明 | 人工 manifest `intel-env.yaml`（含 `init_scripts`） | **无解析实现**（仅 `internal/store/env.go` 注释里提过一次）；依赖只靠 `envdetect` 自动推导 | 自动推导覆盖不到的项目无法人工补声明 | 标注为未实现或落地解析器 |
| 12 | §5 路径归属 | `GET /api/intel/projects/{id}/modules`、`PUT …/commands`、`…/commands-modules` | 实际是 `GET /api/intel/modules` 与 `GET\|PUT /api/intel/modules/`（按 moduleId），projects 子路径只挂了 `sources` | 与 §5 写法不一致，易误判为缺失 | 改写 §5 路径 |
| 13 | §3.5、§8 M3 报告解析 | 解析 surefire / playwright json / go test -json（§3.5 另列 XCTest、pytest） | **已实现（2026-09-20）**：`parseReport` 加 `playwright` 分派（stdout 容错提取 JSON 段）；`detectPlaywright` 检出 web/node 项目（playwright.config 或 @playwright/test）且无人工指定命令时，默认 `npx playwright test --reporter=json`；surefire/go 不变 | Web/E2E 项目逐用例结果可解析；XCTest/pytest 仍缺 | ✅ Playwright 已接线；XCTest/pytest 待补 |
| 14 | 文首设计原则、§3.1 Profile 注册表 | "现装现有体系：Java/Maven、Go、Android、iOS、Web、BFF(GraphQL)、Node" | 类型**探测**（`internal/intel/profile.go` 的 7 个 Profile 锚点）确实全都有；但"每个 Profile 打包自己的结构化扫描器"只有 **Java（`java.go`）与 Go（`go.go`）**两套，其余类型 `ScanModule` 返回空结果 | 混合仓库里 Android/iOS/Web 子项目能被识别出 type/role，却拿不到实体/端点契约，字段级校验与影响面推导对这些子项目不成立 | 见 `internal/intel/scan.go` 的 `ScanModule` 注释；补 Android/iOS/Web 扫描器前，文中"现装"应限定为"探测现装、扫描器仅 Java/Go" |

**另需知悉（不算"文档说谎"，但会影响判断）**

- **env 门禁（2026-09-20 已修复）**：原 `internal/server/env.go` 的 `envGate` 在 `ensureEnv` 返回错误时
  `log … (放行)` 并 `return nil`，即"环境探测本身坏了"等同于"环境检查通过"，与 §3.6/§7 承诺的
  硬门禁语义相反。现改为**遇错即拦**并暴露探测错误（硬门禁）；本地 run 只有探测正常时才被真正拦住，
  `force`/远程 run 仍跳过门禁。
- **轻量化部署看不到 intel**：`/api/system` 的 `vectorCapable`（= pgvector 已装 **且**
  embedding 已配）为 false 时，`internal/webui/static/assets/app.js` **直接隐藏「测试」入口**。
  因此默认 SQLite 部署下，整套 intel 功能"后端可用、页面无入口"。这也是
  [TECH_DEVIATION.md](TECH_DEVIATION.md) 偏离项 ② 把"SQLite 进程内 cosine 回退路径是否放行"
  列为待评审确认点的原因。

> 本节只描述"文档 vs 代码"的差异，不构成实施优先级承诺；技术栈层面的偏离理由与评审材料
> 见 [`docs/TECH_DEVIATION.md`](TECH_DEVIATION.md)。