# 技术选型偏离说明（starburst-backend）

> **依据**：`/workspaces/AGENTS.md`「核心规范 → 技术选型」条 —— "偏离标准技术栈（如 pgvector、
> MongoDB）须在项目 `docs/` 提供**《技术选型偏离说明》**，经架构组评审后实施，并同步更新本规范"。
>
> **状态**：**待架构组评审**（评审通过后请按规范末句同步更新 `/workspaces/AGENTS.md`）。
> **对象**：`/workspaces/opencode-backend`，Go module `github.com/hiylo/starburst-backend`
> （`go 1.25.0`，Dockerfile 构建镜像 `golang:1.25-alpine`）。
> **核对方式**：本文所有事实均由读码得出（`internal/` 全量 + `go.mod` + 路由注册表），不接受
> "文档自述"；文中不写行号（易漂移），一律以"文件 + 符号名"定位。

---

## 0. 一页速览

| 维度 | 工作区标准 | 本项目实际 | 偏离项 |
|------|-----------|-----------|--------|
| 语言 / 框架 | Java + Spring Boot / Spring Cloud | Go 1.25 标准库 `net/http`（无 Web 框架） | ① |
| 持久层 | Spring Data JPA + Hibernate + **Flyway** | `database/sql` + 手写 SQL + 自研版本化迁移（47 步） | ① |
| 数据库 | **MySQL 8** + Redis/Redisson + Druid | SQLite（`modernc.org/sqlite`，CGO-free）**或** PostgreSQL 16（`pgx`）双方言 | ③ |
| 向量检索 | （标准栈未定义，属点名偏离项） | PostgreSQL + **pgvector**（`vector(1024)` + **HNSW** `vector_cosine_ops`） | ② |
| RPC | OpenFeign | 直接 `net/http` 调本机 OpenCode 上游（`internal/opencode/client.go`） | ① |
| 缓存 | Redis / Redisson | 进程内有界 LRU-ish（`internal/server/ragcache.go`）+ 固定窗口限流（`ratelimit.go`） | ① |
| 静态分析 / 代码抽取 | AST（JavaParser 一类） | Java 侧 **21 条正则**；Go 侧真 AST（`go/ast`+`go/parser`） | ④ |
| 工具库 | Lombok / Jackson / JUnit 5 + Mockito | `encoding/json` + Go `testing`（无 Lombok 等价物，全部手写） | ① |
| 前端 | Vue 3 + Element Plus，`dist/` 进 `resources/static` | 手写原生 JS 单页（`internal/webui/static/`），`go:embed` 打进**同一个二进制** | ⑤ |
| 构建 | `mvn install -T 1C` | `go build ./...`（编译天然并行，无 `-T` 概念） | ① |

本仓 `.java` 源文件数为 **0**，因此工作区规范中仅对 Java 生效的条款（文件头 Copyright 模板、
Javadoc 措辞、Lombok 构造器注入、Knife4j/Swagger 禁用、SpotBugs 零排除、`@ExtendWith(MockitoExtension.class)`）
在本仓**没有作用对象**；仍在本仓生效并被遵守的 Java 侧规范是「安全红线」「单文件行数」
（后者见 ⑤ 的例外说明）与文档落 `docs/` 的约定。

---

## 1. 分层对应关系：Spring 约定 ↔ 本仓 Go 包

`/workspaces/AGENTS.md`「代码结构」规定
`controller/ service/ service/impl/ repository/ entity/ dto/ vo/ config/ exception/ constants/ converter/`，
命名要求 `{Module}Controller` / `{Module}Service` / `{Entity}Repository` / `{Entity}Entity`。
**该目录与命名约定对本仓不适用**（它是 Spring 容器 + JPA 注解驱动的产物），本仓的实际分层与
语义对应如下，评审时请以此表为映射基准：

| Spring 层 | 本仓位置 | 实际形态与命名 |
|-----------|----------|----------------|
| `controller/` | `internal/server/*.go` | 方法 `handleXxx(w, r)`，集中注册在 `server.go` 的 `Routes(mux)`（**共 98 条路由，其中 intel 侧 56 条**）。无 `{Module}Controller` 类型，路由前缀即模块边界 |
| `service/` + `service/impl/` | `internal/intel/**`、`internal/tasks/`、`internal/automation/`、`internal/auth/` | 以**包**为单位的服务：`intel` 下再按能力拆 **19** 个子包（`android`/`compliance`/`contract`/`delta`/`deps`/`enrich`/`envagent`/`envdetect`/`feature`/`fix`/`gateway`/`ios`/`report`/`rootcause`/`sbom`/`schemainit`/`security`/`testassets`/`web`，根包另有 `scan.go`/`java.go`/`go.go`/`profile.go`/`commands.go`）。多为纯函数 + 显式入参，不依赖 HTTP 类型，因此天然可单测 |
| `repository/` | `internal/store/` | 单一 `Store` 接口（`store.go`）+ `sqlStore` 实现，每个聚合一个文件（`task.go`/`rule.go`/`intel_exec.go`/`rag.go`…）。手写 SQL，经 `s.q(...)`（`rebind.go`）做 `?` → `$N` 方言适配 |
| `entity/` | 同 `store` 各文件内的结构体 | 如 `store.Task`、`store.TestRun`、`store.RagChunk`；带 `json` tag 直接对外序列化，**没有** `createdAt`/`updatedAt` 的 `@PrePersist`，由 SQL 侧 `CURRENT_TIMESTAMP` 写入 |
| `dto/` + `vo/` | handler 函数体内**匿名结构体** | 请求体就地声明（`struct{ ProjectID int64 \`json:"projectId"\` ... }`），响应复用 entity + `writeJSON`。刻意不建 `dto/`/`vo/` 包：单服务、无跨模块契约 |
| `converter/` | 包内私有函数 | 例如 `intel_exec.go` 的 `parseReport`、`store` 各文件的 `rows.Scan` 目标映射 |
| `config/` | `internal/config/config.go` | flag + `STARBURST_*` 环境变量，`envOr/envInt/envDuration` 三件套 |
| `exception/` | 返回 `error` + `writeErr(w, code, msg)` | Go 无 checked exception；`store.ErrNotFound` 一类哨兵错误承担"异常类型"职责，无全局异常处理器 |
| `constants/` | 各包内 `const` / `var` 表 | 如 `store/rule.go` 的 `TriggerCron/TriggerGit/TriggerHTTP`、`intel_runner.go` 的 `intelExecConcurrency` |
| 多租户 `tenant_id` / 乐观锁 `@Version` | **未实现** | 产品形态是**单用户自托管**（见 `docs/TEST_INTELLIGENCE.md` §9「单用户访问」），全仓 `grep tenant_id` 命中 0 处。属"规范条款不适用"而非"违反"，但需评审确认边界 |

---

## 2. 偏离项 ①：Go 而非 Java / Spring

**偏离了什么**
主语言与整套 Java 基建（Spring Boot/Cloud、JPA+Hibernate、Flyway、OpenFeign、Lombok、
JUnit5+Mockito、Maven `-T 1C`）全部未采用；`/workspaces/AGENTS.md`「文档与开发偏好」列出的
技术栈为 "Java / Python / Node.js / Android Kotlin"，**Go 不在其列**。

**为什么**
1. **部署形态是硬约束**：本服务的定位是"跑在开发者本机 / NAS 容器里的 OpenCode 伴生后端"，
   交付物必须是**单个免依赖二进制**（前端亦 `go:embed` 进包，见 ⑤）。JVM + Spring 上下文与该
   目标冲突。
2. **工作负载形态不匹配 Spring 的甜区**：核心负载是长连接 WebSocket 推送、进程内并发派发
   （`intelExecSem` 信号量 + 每项目 `sync.Mutex`）、大量 `os/exec` 子进程编排与流式收集。
   goroutine/channel + `context` 超时传播在这类"IO + 子进程"负载上比线程池模型直接。
3. **强制复用规则不可执行**：`components` 是 Java 组件库、`framework` 是 Spring Cloud 基础库
   （依 `/workspaces/AGENTS.md`「项目架构」），Go 侧无法依赖；本仓 `go.mod` 主 `require` 块的直接
   依赖只有 7 个（`gorilla/websocket`、`jackc/pgx/v5`、`jackc/pgservicefile`、
   `golang.org/x/crypto`、`golang.org/x/text`、`modernc.org/sqlite`、`gopkg.in/yaml.v3`），
   与被扫描/被编排的 Java 体系无功能重叠，
   "禁止引入与 `components` 重叠的第三方库"实质成立（不存在同名 Java 库可重叠）。
4. **本项目同时是"Java 项目的分析器"**：`internal/intel` 需要理解 Spring/JPA 仓库，但不需要
   成为 Spring 应用；用 Go 写反编译器不会引入任何被测框架的依赖。

**代价与缓解**

| 代价 | 缓解 |
|------|------|
| 无 ORM：字段映射、方言、迁移全靠手写，样板代码多 | `store` 层每个聚合一个文件 + `s.q()` 重绑定 + `schema_migrations` 版本表（47 步，SQLite/PG 共用同一序号）；迁移必须保持"只追加不改旧" |
| 无 Bean 容器：依赖装配手写、循环依赖靠人工 | 依赖集中在 `Server` 结构体字段（`internal/server/server.go`），测试里用 `httptest` + 真 store 组装 |
| 无 JPA 审计注解：`created_at/updated_at` 容易漏 | 统一在 INSERT/UPDATE 语句里写 `CURRENT_TIMESTAMP`，不信任内存结构体（`tasks.go` 建任务后**回读**一行再响应，避免返回零值时间） |
| 规范里的 Lombok/`@Data`/`@Slf4j` 等约定无法比对 | 以 `go vet` + `gofmt` 为基线；评审时请按 Go 惯例而非 Java 条款打分 |
| 团队 Java 技能沉淀复用不上 | 文档与包边界按 Spring 分层做了 1:1 映射表（§1），降低跨栈阅读成本 |

**待评审确认点**
- 是否将 **Go** 正式写入 `/workspaces/AGENTS.md`「技术栈」清单（当前它不在列，属规范外语言）？
- 是否为本仓单独开一节"Go 项目规范"（错误处理、context 超时、goroutine 生命周期、
  `Store` 接口变更评审口径），替代无法套用的 Java 条款？
- 单用户形态（无 `tenant_id`、无 RBAC）是否接受为长期决策？

---

## 3. 偏离项 ②：pgvector + HNSW 做向量检索

**偏离了什么**
`/workspaces/AGENTS.md` 明确点名 pgvector 属"偏离标准技术栈"。本仓在 PostgreSQL 上启用
`vector` 扩展，`intel_chunks.embedding` 列为 `vector(1024)`，并建
`CREATE INDEX ... USING hnsw (embedding vector_cosine_ops)`（`internal/store/migrate.go` 的
`migrationIntelRag`）；检索侧用 `<=>` 余弦距离（`internal/store/rag.go` 的 `searchRagChunksPG`）。

**为什么**
1. **测试智能的召回需要语义检索**：`/api/intel/ask` 与功能点 AI 对话要把"实体/表摘要、接口契约、
   源码片段"混在一起按相关性召回（`askIntelProject` → `Embed(question)` → `SearchRagChunks`），
   MySQL 8 无原生向量类型/索引，关系型 + LIKE 无法胜任。
2. **避免再加一个存储组件**：本服务已经要连一个 PostgreSQL（见 ③），把向量放在同一个实例里，
   省掉 Milvus/ES-kNN/独立向量库这一整层运维与数据同步；向量行与它描述的 `project_id`/
   `module_id`/`source_file:line` 同表，天然一致。
3. **HNSW 与规模匹配**：单项目 `ReplaceProjectChunks` 全量重建，规模在万级 chunk 内，HNSW 的
   ANN 召回/延迟足够且无需调分片；`ef`/`M` 用扩展默认值。
4. **维度 1024 与模型绑定**：`store.EmbedDim = 1024`（默认 embedding 模型 bge-m3），列类型固定，
   换模型必须重建索引（代码注释亦要求调用方校验维度，见 `internal/embed`）。

**代价与缓解**

| 代价 | 缓解 |
|------|------|
| **部署依赖扩展可用性**：目标 PG 必须能 `CREATE EXTENSION vector`（云托管/受限实例常不给） | 迁移里 `CREATE EXTENSION IF NOT EXISTS vector` 失败即整个迁移失败（快速暴露，不留半可用状态）；`/api/system` 上报 `pgvector` 与 `vectorCapable` 供页面判定 |
| **能力探测把 SQLite 部署整体判为"不具备"**：`PGVectorInstalled` 对非 PostgreSQL **无条件 `return false, nil`**（`internal/store/rag.go`），`/api/system` 据此上报 `vectorCapable=false`，Web 控制台隐藏「测试」入口 | 这是刻意选择：轻量化部署不给残缺功能，见下方"已拍板" |
| 向量列在 SQLite 上退化为 TEXT（`[1,2,3]` 文本），只写不读 | `SearchRagChunks` 在非 PostgreSQL 驱动下**直接返回 `store.ErrRagUnsupported`**（曾经的进程内 cosine 回退已删除，见下方"已拍板"）；`handleIntelAsk` 把它映射为 **HTTP 503**，不做静默降级 |
| ANN 是近似算法，召回不是 100% | 检索结果**只作为 LLM 上下文**（带 `similarity` 与 `source_file:line` provenance），不参与任何硬断言；关键事实仍由确定性扫描器给出（`docs/TEST_INTELLIGENCE.md` §9「确定性优先」） |
| 无向量库的过滤/命名空间/版本管理能力 | 用表内 `project_id`/`module_id` 做硬过滤条件；重建走"删全部 + 插入"，不维护部分更新 |
| embedding 服务不可用时的行为 | `internal/embed` 未配置即 `Enabled()=false`，`askIntelProject` 直接返回 `errEmbeddingDisabled`，不静默降级为关键词检索 |

**已拍板（2026-09-19）：选 2 —— 删除 SQLite 回退路径**

承认"测试智能必须有 PostgreSQL + pgvector"，不再保留一条"代码完整、有单测、但前端总开关
把它整体隐藏"的暗路径。落地内容：

1. `store.ErrRagUnsupported` 成为显式契约；`SearchRagChunks` 在非 PostgreSQL 驱动下立即返回它，
   `searchRagChunksSQLite` 及其 `parseVector`/`cosine` 辅助函数一并删除。
2. `handleIntelAsk` 将该哨兵错误映射为 **503**（而不是被 `ask failed` 兜底成 500），
   响应体直接说明"需要 PostgreSQL + pgvector"。
3. 项目画像注入逻辑抽成纯函数 `applyOverview`，因此它不再依赖向量检索也能被单测覆盖；
   向量检索本身只在 `pgtest` 构建标签下由 `internal/store/rag_pg_test.go` 覆盖。
4. `docs/QUICKSTART.md` §1 已写明：SQLite 单文件部署不含测试智能，前置条件是 pgvector +
   embedding 服务两项齐备（`vectorCapable`）。

其它：是否把 pgvector 与 Go 一起写入 `/workspaces/AGENTS.md` 的许可技术清单，避免每次新成员
都要重走一次"这算不算违规"。

---

## 4. 偏离项 ③：SQLite(modernc) / PostgreSQL 双方言，而非单一 MySQL 8

**偏离了什么**
标准栈的默认数据库是 MySQL 8（配 Druid 连接池 + Redis）。本仓**不支持 MySQL**：`--db` 仅接受
`sqlite` 与 `postgres`（`internal/config/config.go` 中非法值直接 `invalid db driver`），
驱动注册表只有两个（`internal/store/driver.go`：`sqlite` → `modernc.org/sqlite`、
`postgres` → `pgx`）。

**为什么**
1. **零依赖落地**：SQLite 让"下载一个二进制就能跑"成立，是本服务作为个人/内网工具的第一价值；
   而 `modernc.org/sqlite` 是**纯 Go 实现（无 CGO）**，交叉编译与 `golang:*-alpine` 镜像都简单。
2. **规模与并发画像**：单用户、低 QPS、以本地文件与子进程为主要开销，写冲突用 WAL +
   `busy_timeout(5000)`（`SQLiteDSN`）即可消化；引入 MySQL 只是增加一个必须先起来的容器。
3. **需要向量的场景才升 PG**：`docs/TEST_INTELLIGENCE.md` 的 pgvector 前置只在 PG 分支成立，
   双方言正好把"轻量化"和"完整能力"分给两类部署。
4. **同一套 SQL 双方言成本可控**：所有查询经 `s.q(...)` 重绑定（`?` → `$N`），
   `isPostgres(driver)` 集中决定自增列写法、`vector` 列类型、`ON CONFLICT`/`RETURNING` 差异；
   迁移用同一 `schema_migrations` 版本序号，两后端从同一版本线出发。

**代价与缓解**

| 代价 | 缓解 |
|------|------|
| 双方言 = **每个 SQL 改动要过两条路径**，单测只跑 SQLite 时 PG 侧回归会漏 | 迁移本身两驱动都跑；PG 专属行为集中在 `isPostgres` 分支内，不散落；CI 侧另有 `STARBURST_PG_TEST_DSN` 门控的 PG 回归（属并行的另一项整改，本文不展开） |
| SQLite 并发写与类型宽松（`INTEGER` 无强约束、无 `DECIMAL`）可能掩盖问题 | 金额/时间等在应用层转换；时间统一 UTC 且由 DB `CURRENT_TIMESTAMP` 落库；不使用 SQLite 的隐式类型转换做业务判断 |
| 无 Flyway：迁移是自研的 `migrations` 切片 + 版本表 | **只追加、不改旧迁移**（`docs/TEST_INTELLIGENCE.md` §4 亦作此约定），`migrate.go` 顶部有版本表说明；新表必须同时给出两方言可用 DDL |
| 无 Druid/Redis：热点数据没有分布式缓存 | 进程内有界缓存（`ragcache.go`）+ 单实例假设；如未来多实例部署，缓存一致性与 SQLite 写并发会**同时**成为阻塞项，需回到评审 |
| 标准栈的 MySQL 生态（DBA 规范、备份、审计）不复用 | 数据落 `--sqlite-path` 单文件或既有 PG 实例，备份 = 文件快照 / `pg_dump`；`docker-compose.yml` 已给出卷挂载形态 |

**待评审确认点**
- 是否接受"MySQL 8 在本仓**永不支持**"，并把它写成规范例外条款（避免误判为"未接入标准库"）？
- 双方言是否设边界：例如"新增能力只允许在 PG 分支实现"时必须在本文档登记（pgvector 就是首例），
  以免 SQLite 分支长期只作为"能启动"存在而语义上残缺。
- 若未来多实例部署：需要引入的中心化缓存/锁（对应标准的 Redis/Redisson）此刻是否就要立项。

---

## 5. 偏离项 ④：Java 静态抽取用 21 条正则，而非 AST 解析

**偏离了什么**
`docs/TEST_INTELLIGENCE.md` 的确定性优先原则要求"表↔列↔nullable、必填"从代码静态提取。
本仓两条腿的做法并不一致：
- **Go 侧是真 AST**：`internal/intel/go.go` 使用 `go/parser` + `go/ast`
  （`parser.ParseFile`、`ast.Inspect`、`ast.IsExported`、`*ast.StructType`/`*ast.FuncDecl`
  /`*ast.SelectorExpr` 类型断言），拿到的是语法树而非文本。
- **Java 侧是行级正则**：`internal/intel/java.go` 顶部集中 **21 条** `regexp.MustCompile`
  （`reEntity`/`reTable`/`reClass`/`reInterface`/`reImplements`/`reImport`/`rePackage`/
  `reColumn`/`reColumnName`/`reColumnNullable`/`reField`/`reController`/`reRequestMapping`/
  `reMethodValue`/`reReturnType`/`reRequestBody`/`rePathVariable`/`reRequestParam`/
  `reRequestRequired`/`reGraphQLMapping`/`reGraphQLArgument`），逐行匹配 `*.java`。

**为什么（这是有意取舍，不是偷懒）**
1. **零依赖代价**：仓库里没有 Java 编译器/JDT/JavaParser（且按 ① 不能引 Java 库），要 AST 就得
   外挂 JVM 或引入第三方 Java 前端 —— 与"单二进制"部署目标直接冲突。
2. **锚点足够规整**：抽取目标只有 JPA/Spring 注解族（`@Entity`/`@Table(name=)`/`@Column(nullable=)`/
   `@RestController`/`@*Mapping`/`@RequestBody`/`@PathVariable`/`@RequestParam`/GraphQL
   `@*Mapping`+`@Argument`），这些在真实工程里形态高度收敛，正则命中率与 AST 差距有限。
3. **容错取向不同**：分析对象是**别人的、可能编译不过的**仓库；正则逐行独立、单点失败只丢一行，
   而 JavaParser/`javac` 遇到不完整源码（缺依赖、语法半截、非 UTF-8）会整体罢工。
4. **成本可控且可观测**：整文件读入 + 一次 `FindAllStringSubmatch`，复杂度线性；Java 子项目
   平均扫描成本是本项目"能在开发者笔记本上跑"的前提之一。
5. **有兜底层**：正则拿不准的一律降置信度并进「待确认」队列，由 §3.8 的覆写层（人工修正 +
   `intel_overrides`）与 `llm-proxy` 标注的 LLM 辅助补全，**绝不把猜测当事实**。

**代价与缓解**

| 代价（正则固有） | 现状与缓解 |
|------------------|------------|
| **不理解作用域/嵌套**：内部类、匿名类、方法内局部变量可能被判成字段；DTO 字段为另一 DTO 时**不递归展开**，只归约成 `object`/`array` | 已在 `extractDtoFields` 的文档注释里**显式写明该限制**（单级别、`resolveResponseFields` 不递归）；`reClass`/`reInterface` 只取顶层声明名 |
| **不理解类型系统**：`List<FriendPageDto>` 靠 `innermostType` 取最内层泛型参数；跨文件同名类靠 `buildClassIndex`（全仓 class → 文件路径）+ `imports`/`package` 解析 FQCN，**同名类会撞** | classIndex 按 root 缓存（`classIndexFor`），解析不到就返回空字段而不是猜；撞名场景交由置信度降级 + 人工覆写 |
| **不解析 nullability/校验注解**：`required` 恒为 `false`（未识别 `@NotNull`/`@NotBlank`/`@Valid`），路径/查询参数则靠 `@RequestParam(required=false)` 反推 | 函数注释明确 `required` 默认 false 且原因；`@Column(nullable=)` 是**唯一**被解析的 nullability 来源（实体列），DTO 字段必填靠人工/LLM 补 |
| **对格式敏感**：注解换行、块注释包裹、`record`/Lombok 生成方法、`@GetMapping` 用 `path=` 之外的冷门写法会漏 | 正则集合覆盖 `value=`/`path=`/裸串三种；漏抽的后果是"契约缺失"（可被 `intel_overrides` 与待确认队列兜住），**不会**被误报成错误契约 |
| 无跨方法数据流：`Controller→Service→Repository→表` 精确链还原不了 | 已在 `docs/TEST_INTELLIGENCE.md` §10 作为已知遗留（M1 只报"模块内实体集合 + 端点字段契约"） |
| **非 Java 类型连正则腿都没有**：Android/iOS/Web/Node 子项目只有类型探测（`internal/intel/profile.go` 的 7 个 Profile 锚点），`ScanModule` 对它们返回空契约 | 已在 `internal/intel/scan.go` 的 `ScanModule` 注释与 `docs/TEST_INTELLIGENCE.md` §11 偏差表登记，避免"现装 7 套 Profile"被误读为"7 套扫描器" |

**待评审确认点**
- 契约抽取的**准确率门槛**由谁定：是否接受"正则 + 人工覆写层"作为长期方案，还是要求在某规模
  项目上达到可量化准确率（例如端点召回 ≥95%）后必须升级为 AST？
- 若升级为 AST，允许哪条路：(a) 引纯 Go Java 语法库（破坏"无重叠第三方库"审计，需登记）；
  (b) 外挂一个可选的 JVM/JavaParser sidecar（破坏"单二进制"，需登记）；(c) 用 `javaparser` 的
  非 JVM 移植；(d) 维持现状并明确写"Java 契约为启发式，非权威"。**本仓当前选择 (d)。**
- 是否要求把 `go.go`（真 AST）的做法作为"新 Profile 扫描器的默认姿势"写进规范，Java 正则作为
  历史例外记录在案。

---

## 6. 偏离项 ⑤：内嵌原生 JS 单页，而非 Vue 3 + Element Plus

**偏离了什么**
规范「前端与全栈规范」要求 Web 统一 Vue 3 + Element Plus，前端产物通常输出到
`src/main/resources/static` 单包部署。本仓的前端在 `internal/webui/static/`
（`index.html` + `assets/app.js` + `assets/style.css`，无构建步骤、无 `package.json`、
无框架依赖），经 `internal/webui/webui.go` 的 `//go:embed static` 打进 Go 二进制。

**为什么**
- 与 ① 同源：交付物是单文件，任何需要 `npm install && vite build` 的资产都会把构建链拖进
  发布流程；配置页的功能是"表格 + 表单 + 少量实时状态"，原生 DOM 足以承担。
- 无独立前端团队维护：一个 4.5k 行的 `app.js` 由后端同僚随手改，框架引入的收益低于成本。

**代价与缓解**

| 代价 | 缓解 |
|------|------|
| 无组件库 = 样式与交互全靠手写，重复代码多 | 统一走 `style.css` 的 class 约定（`card`/`badge`/`row`）与 `app.js` 内的 `show`/`escapeHtml`/`api()` 小工具；所有插值过 `escapeHtml` 防 XSS |
| 无构建 = 无类型检查、无 tree-shaking，`app.js` 体积 228 KB | 属规范「单文件嵌入式前端」豁免范围（`/workspaces/AGENTS.md` 单文件行数条款明确豁免"内嵌 index.html/css/js"），因此不计入 1500 行硬上限；评审若不接受该豁免，需登记拆分计划 |
| 与工作区前端技能栈不共用 | 后端 API 为唯一事实源（`docs/API.md`），同一套 `/api/*` 已被移动 APP 复用，Web 页只是其中一个消费者 |

**待评审确认点**：是否把"伴生工具类内嵌页面"列为 Vue 3 + Element Plus 条款的正式例外场景；
以及 `app.js` 是否需要在某个体积阈值后强制拆分。

---

## 7. 连带影响清单（评审时一并过）

1. **Go 侧文件行数**：`/workspaces/AGENTS.md` 的 1000 行目标 / 1500 行硬上限，非测试代码已**无超标**
   —— 原 `internal/server/intel.go`（2085 行）按子域拆为
   `intel_projects.go`（项目 CRUD 与来源仓库，453 行）、`intel_inventory.go`（读侧清单 API 与人工校正
   的读取期合并，522 行）、`intel_analyze.go`（分析引擎：模块检测与全量/增量分析，438 行）、
   `intel_persist.go`（分析产出的落库与 LLM 增强，524 行）、`intel_workspace.go`（源码根解析、git
   clone、快照哈希，235 行），`intel.go` 只留各 handler 共用的 project id 解析助手（57 行）；
   沿用 `intel_exec.go`/`intel_audit.go`/`intel_plan.go`/`intel_runner.go` 的先例命名。
   当前最长的非测试文件是 `internal/store/migrate.go`（1262 行，配置型迁移表）与
   `internal/opencode/client.go`（1040 行）。测试侧的硬超标已消除：原
   `internal/server/intel_test.go`（3452 行、55 个顶层声明）按子域整函数搬移为 9 个文件，
   行数分别是 `intel_env_test.go` 667（环境门禁/工具链/设备/远程节点：`TestIntelEnvEnsureAndInstall`、
   `TestIntelDevices`、`TestIntelRemoteNodes` 等）、`intel_analyze_test.go` 633（`TestIntelFlow`、
   `TestIntelAnalyzeIncremental` 等）、`intel_exec_test.go` 627（`TestIntelRunAll`、`TestFlakyRetry`、
   `TestIntelRunCancel` 等）、`intel_inventory_test.go` 521（`TestIntelModuleDetail`、
   `TestIntelAndroidBindings` 等）、`intel_contract_test.go` 355（`TestIntelContractCheck`、
   `TestIntelOverridesPending` 等）、`intel_projects_test.go` 290（`TestIntelProjectSourcesCRUD`、
   `TestIntelGitProjectClone` 等）、`intel_feature_test.go` 251（`TestIntelFeatureManagement`、
   `TestIntelFeatureChat`、`TestIntelAIRulesCRUD`）、`intel_audit_test.go` 100（`TestIntelFixApplyAndRollback`、
   `TestSplitLocation`），`intel_test.go` 收敛到 109 行，只留跨子域共用 helper（`loginWeb`、
   `writeTestFile`、`waitIntelRunFinished`、`waitIntelCondition`、`gitRun`、`jsonInt`、`itoa2`、
   `stringsContains`）。拆分产物全部低于 1000 行目标；搬移为逐函数整体移动，逐行多重集守恒校验通过
   （除新增的 `package server`/import 行外零增删），`go vet` 与 `go test ./internal/server/` 均通过。
   测试侧最后一处超标同批拆完：原 `internal/server/server_test.go`（1447 行、29 个顶层声明）拆为
   `server_tasks_test.go`（360，任务 CRUD/依赖/分页/批量）、`server_auth_test.go`（289，凭据边界与
   健康/系统端点）、`server_data_test.go`（275，项目分组、会话归档、审计与统计）、
   `server_events_test.go`（242，WS 推送与 SSE 中继）、`server_rules_test.go`（224，定时/HTTP 触发
   规则），`server_test.go` 收敛到 121 行只留跨子域共用的 `newTestServer` 与 `(*Server).do`；
   `waitForAuditEntries`/`auditEntryView` 随唯一使用者 `server_data_test.go` 走。同样为整函数搬移 +
   逐行多重集守恒（missing=0）。至此 `internal/server` 包内测试文件最长 667 行。
   仅剩 `internal/store/store_test.go`（1003 行）高于 1000 行目标、未超硬上限，暂不处理。
2. **构建规范**：`mvn install -T 1C` 在本仓的等价命令是 `go build ./...`（Go 编译天然并行）；
   测试为 `go test ./...`，CI/本地另有 `GOFLAGS=-mod=mod GOPROXY=off GOSUMDB=off` 的离线口径。
3. **安全红线**：与语言无关，本仓继续遵守（`githooks/` + `scripts/check-secrets.sh` +
   `.github/workflows/secret-scan.yml`）。补充事实：仓库内**不存放任何真实口令/内网地址**，
   示例地址使用 RFC 5737 文档网段或 `<host>:<port>` 占位；口令类字段
   （`env_services` 中间件口令、`remote_nodes.auth` SSH 凭据）经 `internal/store/crypto.go`
   的 AES-256-GCM 加密落库，`Admin` 口令走 bcrypt（`internal/auth/auth.go`）。
   **注意**：Git clone 凭据（token / SSH 私钥）**尚未实现**，`docs/TEST_INTELLIGENCE.md`
   §11 第 10 条已登记，评审时不要把它当成已交付能力。
4. **规范同步动作**：本文评审通过后，需按 AGENTS.md 末句**同步更新** `/workspaces/AGENTS.md`
   ——至少把「Go + pgvector + SQLite/PG 双方言」写入许可清单，并说明本仓不适用 JPA/Flyway/
   MySQL/Lombok/JUnit5 各条。

---

## 8. 本文的验证口径（供复核）

- 语言与依赖：`go.mod`（主 `require` 块 7 个直接依赖）、`find . -name '*.java'` → 0 个、无 `pom.xml`/`build.gradle`。
- 分层与路由：`internal/server/server.go` 的 `Routes`（98 条 `mux.HandleFunc`，intel 56 条）。
- pgvector/HNSW：`internal/store/migrate.go` 的 `migrationIntelRag`、`internal/store/rag.go`
  的 `searchRagChunksPG`/`searchRagChunksSQLite`/`PGVectorInstalled`、
  `internal/server/handlers.go` 的 `handleSystem`（`vectorCapable`）、
  `internal/webui/static/assets/app.js` 的入口隐藏逻辑。
- 双方言：`internal/store/driver.go`、`internal/store/rebind.go`、`internal/config/config.go`。
- 正则 vs AST：`internal/intel/java.go`（21 条 `regexp.MustCompile`）、
  `internal/intel/go.go`（`go/ast` + `go/parser`）、`internal/intel/scan.go` 的 `ScanModule`。
- 里程碑与"文档 vs 代码"偏差：`docs/TEST_INTELLIGENCE.md` 各「⚠️ 实现现状」标注与 §11 清单。
