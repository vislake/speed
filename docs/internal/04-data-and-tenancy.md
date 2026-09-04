# 数据层与多租户隔离

> ORM 选型与双方言约束、数据分域（哪些表带 tenant_id）、租户上下文的信任边界、隔离三重防护、受控的系统上下文逃生舱，以及标记删除与彻底删除两层删除语义。

## ORM：GORM（不选 ent / sqlc）
- **ent 不适合**：期望单一 schema graph 做代码生成，跨独立 module 扩展模型极别扭，下游想给 `User` 加字段几乎做不到。
- **sqlc 不适合作核心库**：需为 PG/SQLite 各写一套 query 文件，且难以提供通用 `Repository[T]`。允许业务方在自己 app 内对报表类复杂查询局部使用，作为逃生舱。
- **GORM 选定**：纯 struct + tag，下游可直接嵌入组合，无代码生成流水线；Scopes/Callback 机制正好是实现"强制 tenant_id 过滤"的抓手。
- SQLite 驱动用 `glebarez/sqlite`（纯 Go，无 CGO），避免镜像交叉编译复杂化。

**双方言兼容的硬性约束**（写入 lint 规则与文档）：
- ID 在应用层生成（UUID/ULID），不用 `gen_random_uuid()`
- JSON 用 `datatypes.JSON`，禁止依赖 JSONB 操作符做过滤
- 禁用 PG 原生数组类型，改 JSON 或关联表
- 全文检索抽象成 `SearchProvider` 接口，PG 实现 tsvector，开发环境退化 LIKE
- 时间戳用 `autoCreateTime/autoUpdateTime` tag，不写 `NOW()`

**字段级加密与盲索引**由 `dbkit` 提供（GORM Serializer + HMAC 盲索引列），是数据层的基础能力而非合规模块的附属——加密必须在第一条敏感数据落库前就位。具体设计见 [10 合规与审计](10-compliance-and-audit.md)。

**迁移工具链**：GORM struct 为 schema 唯一真源 → `ariga.io/atlas-provider-gorm` 生成 PG/SQLite 各自方言的版本化 SQL → 各模块在自己目录维护 `migrations/{postgres,sqlite}/*.sql` 并 `embed.FS` 暴露 → 应用层通过 `dbkit.MigrationRegistry` 按依赖顺序聚合执行。**生产禁用 `AutoMigrate`**（不可审计、不可回滚）。

## 数据分域：哪些表带 tenant_id，哪些不带

**这是多租户设计里最容易被跳过、又最早卡住实现的一步。** "所有表都带 tenant_id"是错的——`users` 表天然跨租户（一个人可以属于多个组织，社交登录成功那一刻还没有任何租户），套餐定义是平台级的，审计日志既有租户级也有平台级。必须先分类，`TenantScoped` 接口、`AssertIsolated` 套件、RLS 策略才有明确的适用范围。

| 分域 | 定义 | 是否 `TenantScoped` | 示例 |
|---|---|---|---|
| **租户数据** | 归属某个租户，跨租户绝对不可见 | 是 | 组织节点、成员关系、订阅、用量、媒体文件、业务数据 |
| **身份数据** | 归属自然人，可关联多个租户 | 否 | `users`、`user_identities`（社交账号绑定）、`sessions`、登录日志 |
| **平台数据** | 全局共享，租户只读 | 否 | 平台级 Plan 定义、社交登录渠道配置、系统级配置、模块注册表 |
| **关联数据** | 连接身份与租户的桥梁 | 是（按 tenant_id） | `memberships`（user_id × tenant_id × 角色） |

> **实现状态注记（2026-09-03，M1 审计基础设施轮；2026-09 补充第四例）**：`go/dbkit/audit` 的 `audit_events` 表是第三个"带真实 `tenant_id` 列、但不实现 `TenantScoped`"的平台数据表，与 `go/jobs` 的 `jobRecord`、`go/config` 的 `row` 同一模式——平台级事件用空字符串 sentinel 而非 NULL 表示"无租户"，租户级事件则写入真实 tenant_id；两者共存于同一张表，靠应用层区分，不靠 GORM 插件过滤。这正是本节开头"审计日志既有租户级也有平台级"的落地方式：该表既不能整体划入"租户数据"（会拒绝平台级记录），也不能整体划入"平台数据"（会丢失按租户检索的能力），所以采用"平台数据 + 真实 tenant_id 列"这一组合，用 `TestAuditEvent_DoesNotImplementTenantScoped` 类测试断言"不实现 `TenantScoped`"这一半，用 `ListByTenant` 的真实查询断言"tenant_id 列真实可用"这一半。`go/ai-gateway` 的 `credentialRow`（`ai_gateway_credentials` 表）是第四例：主键 `(provider, scope, tenant_id)`，`tenant_id` 同样 `NOT NULL`、系统级行用空字符串 sentinel、租户级行写真实 tenant_id，同一组合、同一理由。

**由此推导出的硬性规则：**

1. **`users` 表不含 `tenant_id`**。用户与租户的关系由 `memberships` 表承载，这是一张标准的多对多桥表。任何"当前用户的租户"都要经 membership 解析，不能从 user 记录直接读。
2. **身份数据的隔离靠权限而非 tenant_id**：用户只能读写自己的身份数据；管理员读取他人身份数据是一条审计事件。
3. **平台数据的写入需要系统上下文**（见下节），普通租户上下文只读。
4. **跨分域引用只存 ID，不建数据库外键**。`memberships.user_id` 指向 `users.id`，但不建 FK 约束——因为模块独立发布、迁移各自管理，跨模块 FK 会让迁移顺序与级联删除变成噩梦。完整性由应用层与定期一致性巡检保证，这个取舍必须写进模块文档。
5. **`AssertIsolated` 只对租户数据与关联数据强制**；身份数据与平台数据用另一套测试套件 `tenancytest.AssertNotTenantScoped` 断言它们**不会**被 GORM 插件误加租户过滤（反向断言同样重要——一个本该全局可见的表被错误地加上租户过滤，表现为"数据莫名其妙查不到"，排查成本极高）。

## 租户上下文的来源与信任边界

隔离防护再严密，如果"当前租户是谁"这个输入本身可被伪造，一切都白费。**必须先锁死租户上下文的来源。**

| 请求类型 | 唯一权威来源 | 说明 |
|---|---|---|
| 已认证请求 | **access token 的 claims** | 令牌由服务端签名，客户端无法篡改 |
| 未认证请求 | 自定义域名 → 子域名 → 默认租户 | 仅用于决定登录页品牌与可用登录方式，**不授予任何数据访问** |
| 内部服务间调用 | 签名的内部头 | 本项目是 modular monolith，进程内调用为主；该路径预留但默认关闭 |

**铁律：服务端永不接受来自请求头、查询参数或请求体的 `tenant_id`。** 这是水平越权最常见的入口——只要接受了，攻击者改一个 header 就能读别的租户数据，前面所有防护都形同虚设。

**切换租户**走专门接口：服务端校验该用户在目标租户的 membership 与权限后，签发携带新租户的 access token（复用同一 session 与 refresh token，见 [05 身份与访问](05-identity-and-access.md)）。客户端本地存的"当前租户"只用于 UI 展示与前端缓存命名空间，不参与鉴权。

```go
type Resolver interface {
    Resolve(r *http.Request) (TenantID, error)
}
// JWTResolver      —— 已认证请求的默认实现，从签名令牌读取
// DomainResolver   —— 未认证场景（登录页、公开配置）
// 组合时 JWT 优先；两者都拿不到且路由不在白名单（注册/健康检查/公开配置）则 403
```

## 多租户隔离：三重防护
单靠开发者自觉必然出事，"忘记加 tenant_id 过滤"是 SaaS 最常见的严重漏洞。

> **实现落地更正**（Round 1 实现 `pkgcore` 时确认）：本节下面的 `tenancy.FromContext`/`tenancy.WithSystemContext` 等函数，其**原语实际落在 `pkgcore` 包**，而不是 `tenancy` 模块——因为 `dbkit`（本节的 GORM 插件与 Repository 都在这里）需要直接调用它们，而 `dbkit` 不能 import `tenancy`（`tenancy` 本身依赖 `dbkit` 做 GORM 插件，反向 import 会成环）。`tenancy` 模块建成后，会在这些原语之上包一层面向业务代码的、带审计发布的便捷封装（`tenancy.WithSystemContext` 到时会调用 `pkgcore.WithSystemContext` 再发一条审计事件），但 `dbkit` 内部、以及任何不想引入 `tenancy` 依赖的底层代码，一律直接用 `pkgcore` 版本。下面的代码示例按 `pkgcore` 现状书写。

1. **GORM 插件自动注入**：对实现 `TenantScoped` 标记接口的 model，在 query/update/delete 回调自动拼 `WHERE tenant_id = ?`，值取自 `pkgcore.TenantFromContext(ctx)`。
2. **强制泛型 Repository 基类**（核心手段）：业务模块的仓储一律**组合** `dbkit.Repository[T]` 而不是直接持有 `*gorm.DB`。`Create` 自动回填 tenant_id（调用方无法伪造），读取时拿不到 tenant 直接 fail-closed 报错。CI 加静态检查，禁止业务模块内出现绕过 Repository 的 `db.Table/db.Model/db.Raw`。
3. **Postgres RLS 纵深防御**：生产环境对租户表开启 Row-Level Security，事务内 `SET LOCAL app.current_tenant`。即便 Go 层被绕过，数据库也不返回跨租户数据。SQLite 不支持 RLS，这一层仅生产生效（开发环境无真实客户数据，可接受）。

**配套**：`tenancy` 提供 `tenancytest.AssertIsolated(t, repo)` 测试套件，自动造两租户同构数据并断言跨租户查询为空，**所有模块的 Repository 必须跑这个套件**。

**索引**：`tenant_id` 作为所有复合索引最左列；建议主键设为 `(tenant_id, id)`，为将来按租户分区预留空间。

## 系统上下文：受控的隔离逃生舱

`Repository[T]` 在拿不到租户时 fail-closed，这是安全的默认值。但有三类合法操作确实没有租户上下文，**没有逃生舱的话 admin 模块根本无法实现**：

- 平台运营跨租户查询（客服要查任意租户的工单）
- 系统定时任务（清理过期分享链接、聚合用量、归档审计）
- 注册与登录流程（此刻还不知道用户属于哪个租户）

设计原则是：**让逃生舱存在，但让它显眼、受限、留痕**——而不是让开发者用裸 `*gorm.DB` 绕过去（那样才真的失控）。

```go
// 唯一合法的绕过方式，函数名刻意冗长且醒目
// pkgcore 提供的是原语（无审计发布）；tenancy 建成后，业务代码应优先用
// tenancy.WithSystemContext（同签名，内部多发一条审计事件后委托给这个原语）。
ctx, err = pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
    Actor:   actor,            // 谁发起的：平台管理员 / 具名的系统任务
    Purpose: "admin.tenant_search", // 必须先用 pkgcore.RegisterSystemPurpose 注册过，否则报错
    Ticket:  "SUP-1234",       // 可选：工单号等外部依据
})
```

约束：

1. **只能由白名单模块调用**：`admin`、`compliance`、`jobs` 的系统任务、`authn` 的注册登录路径，以及 `tenancy` 模块自身的审计封装 `tenancy.WithSystemContext`（业务代码应调用它而非直接调用这个原语）。白名单由 code review / CODEOWNERS（`go/pkgcore`、`go/tenancy`）加两个函数的文档注释把关，**不是** depguard——depguard 只能按整个 import path 粒度放行/拒绝文件，做不到只挡 `WithSystemContext` 这一个符号：`pkgcore` 根包同时还装着 `TenantID`/`WithTenant`/`apperr`，`go/dbkit`（真实代码、不在白名单上）合法地 import 它们，把「仅白名单可 import `pkgcore`」接成 depguard 规则会连带拦下 dbkit 23 处无关导入，草稿规则因此未合入（完整推演见 `.golangci.yml` 的 depguard 注释；要让这条纪律可静态检查，需要先把 `WithSystemContext` 迁到独立子包，那是公开 API 决策，不属于 lint 配置的副作用）。
2. **必须携带原因**：`Purpose` 是必填枚举，不接受自由文本，防止"随便填一个"。
3. **每次使用都是审计事件**：进入系统上下文本身就要落审计，含 Actor、Purpose、影响的记录数。
4. **系统上下文下的查询依然受 RBAC 约束**：绕过的是租户过滤，不是权限判定。平台管理员没有 `tenant:read_any` 权限一样不能查。
5. **生产 PG 的 RLS 用独立数据库角色实现**：系统上下文切换到一个有 `BYPASSRLS` 的角色，普通请求用受 RLS 约束的角色。这样即便 Go 层的白名单被绕过，数据库层依然是第二道闸。
6. **禁止在 HTTP 中间件层"顺手"注入系统上下文**——只允许在具体的 handler 或任务函数内部、尽可能小的作用域里开启。

## 删除语义：标记删除（可恢复）与彻底删除（合规擦除）

> **现状（2026-09-03，dbkit 硬删除轮次）**：本节的两半都已落地为代码。标记删除（软删除）半部分随软删除轮次落地——`dbkit.SoftDeletable` 标记接口、`Repository[T].Delete` 按模型能力分流、`Repository[T].Restore`，以及查询回调专用的自动 scope 插件，均在 `go/dbkit`（`soft_delete.go`、`repository.go`）实现并测试。彻底删除（`HardDelete`）半部分于本轮（2026-09-03）在 `go/dbkit/hard_delete.go` 落地：真正物理 `DELETE`，对 `SoftDeletable` 模型同样适用（软删除行与存活行一样可擦除，自动 scope 只作用于查询）；门禁是**只查存在性**的系统上下文——普通租户上下文在触碰数据库之前即被拒绝（`ErrHardDeleteRequiresSystemContext`，`dbkit.hard_delete_requires_system_context`，机制级调用方错误、刻意不是 `Forbidden`，靠 `apperr.As` 的 Code 匹配，见设计要点 3 与 `go/dbkit/AGENTS.md` 的"Hard deletion"小节）；门禁通过后租户仍然强制且绑定——系统上下文绝不顶替租户（无租户照样 `pkgcore.ErrNoTenant` fail-closed），也绝不越出 ctx 租户自己的行，跨租户不可擦除性质由单测（`TestRepository_HardDelete_CrossTenant_SystemContextDoesNotEscapeTenantScope`）与 `go/dbkit/integration_test/postgres_hard_delete_rls_test.go`（真实受限角色 + RLS 策略）双边钉死。`HardDelete` 不涉及任何迁移或 DDL（物理 DELETE 本就是模式一直允许的）；谁有权持有系统上下文本身是调用侧白名单（`admin`/`compliance`/`jobs`/`authn`，生产授予走 tenancy 的审计封装 `tenancy.WithSystemContext`），不在 dbkit 内。参考应用的 notes 模块（`examples/reference-app/internal/notes`）是两半共同的第一个真实消费者，消费证明是服务级的（无 HTTP delete/restore 端点）。**保留期配置、`jobs` 定时清理与"被遗忘权"编排已经落地，不再是 M4 未建设想**——`go/compliance`（round 1 + round 2）的 `RetentionService.SweepTenant`/`SweepAllTenants`（按租户可配置的保留窗口，读 `RetentionService.RetentionWindow` 对 `config` 模块的租户级覆盖，未覆盖时退回默认值）、`EnqueueRetentionSweep` 的 `jobs` 定时任务、`ErasureService.Erase`（跳过保留窗口、按主体立即彻底删除的"被遗忘权"路径）都是真实、经测试的代码，均落在参与者注册表 `pkgcore.RetentionParticipant` 之上的应用层编排，最终调用的正是本节定义的 `dbkit.Repository[T].HardDelete`。仍然真正未建的，是 `go/compliance/AGENTS.md` 自己"Known limitations"记录的那几项：数据库角色/触发器层面的仅追加强制、可选哈希链、按分区归档，以及格式化的（CSV/JSON）审计报表导出——这几项，以及导出投递的跨模块级联清理由哪个业务模块自己负责等，才是本节"边界"与 [10 合规与审计](10-compliance-and-audit.md) 里仍要交给后续轮次的部分。本节下方的设计要点、交互清单与边界描述现在是已落地机制的权威说明，而不仅是设计意图；`go/dbkit/AGENTS.md` 的"Soft deletion"与"Hard deletion"两个小节是面向消费方的对应文档。

这里要分开的是两个都成立、但互相冲突的真实需求：终端用户手滑删错数据后要能自己找回；合规要求某些删除必须是不可逆的物理擦除，"删了但其实还在"不能算数。这不是同一个操作的两个开关，而是**同一份数据生命周期里两个先后发生、职责不同的阶段**——用一个 `Delete` 方法同时承担两种语义只会两头不讨好：默认物理删除，用户误删无法挽回；默认软删除，合规意义上的"已删除"又变得不可信。

| | 标记删除（软删除） | 彻底删除（硬删除） |
|---|---|---|
| 回答的问题 | 用户手滑删错了，能不能找回？ | 个人数据必须不可恢复地清除，能不能证明？ |
| 触发方 | 数据所有者的正常删除操作 | 保留期到期的定时任务 / "被遗忘权"请求 / 平台运营的强制清除 |
| 效果 | 行仍在库里，打上 `deleted_at`/`deleted_by`，从正常查询中隐藏 | 行从库中物理消失 |
| 可逆性 | 保留窗口内可 `Restore` | 不可逆 |
| 谁能触发 | 数据的正常所有者，受 RBAC 约束 | 系统上下文白名单（`compliance`、`jobs` 系统任务），见上节 |
| 审计事件语义 | Update 语义（改的是 `deleted_at` 字段） | Delete 语义（行消失，自动采集的事件只携带 Resource 的类型与 ID、`After` 为 nil——捕获事件本身从不带可读名称；擦除后若还要把这次删除读作"曾删掉名为 X 的行"，X 只能来自该行更早的 create/update 捕获事件或显式 `audit.Emit` 的 `Resource.DisplayName`，见 [10 合规与审计](10-compliance-and-audit.md)） |

**设计要点：**

1. **软删除是按模型显式声明的可选能力，不是 `TenantScoped` 之外新增的隐式默认行为。** 新增标记接口 `dbkit.SoftDeletable`（要求模型携带 `DeletedAt *time.Time`、`DeletedBy string`），模型实现它才进入软删除路径；未实现的模型走今天既有的物理删除，行为不变、向后兼容。不是所有租户数据都该软删除——例如高频写入的用量流水表软删除没有意义，逐表决定，不做成全局开关。

2. **`Repository[T].Delete` 按模型能力分流，不新增参数改变既有签名的含义**：目标类型实现 `SoftDeletable` 时落地为一次 `UPDATE ... SET deleted_at = ?, deleted_by = ?`；未实现时保持今天的物理 `DELETE` 不变。新增 `Repository[T].Restore(ctx, id)`，在保留窗口内把 `deleted_at` 清空。GORM 插件对实现了 `SoftDeletable` 的模型自动在查询回调追加 `deleted_at IS NULL`，与 `TenantScoped` 的 `tenant_id = ?` 是两个独立正交的 scope、同时生效；未显式要求"含已删除"的调用方看不到软删除的行。不直接复用 GORM 内建的 `gorm.DeletedAt`，因为内建类型没有 `deleted_by`，也没有跟 `tenant_id` scope 的编排顺序做过约定。

3. **彻底删除是一条独立、受限的入口，不是给 `Delete` 加个参数。** 新增 `Repository[T].HardDelete(ctx, id)`（命名刻意冗长醒目，呼应 `WithSystemContext` 的命名原则），语义等同今天的物理 `DELETE`，但要求调用方已处于系统上下文——对普通租户上下文直接拒绝，防止业务代码手滑把"删除"接成"彻底删除"。两个使用方：
   - **保留期到期清理**：`jobs` 定时任务扫描 `deleted_at` 早于租户保留期配置的行，调用 `HardDelete`，与 [10 合规与审计](10-compliance-and-audit.md) 的"数据保留与删除"策略是同一件事的两半——保留期配置从此有了明确含义：**软删除行在保留窗口内可恢复，窗口一过自动彻底删除**。
   - **"被遗忘权"请求**：按主体立即彻底删除，跳过保留窗口等待，级联清理关联媒体与派生资源；这条路径本就需要 `compliance` 在系统上下文下操作，见上节白名单。

4. **与已有机制的交互，设计阶段先记下来，免得实现时踩坑：**
   - **审计采集**：`audit_capture.go` 的自动写捕获插件按 GORM 的 Create/Update/Delete 回调分类；软删除底层是一次 `UPDATE`，天然捕获成 Update 语义的 diff（`deleted_at: nil → <time>`），不会被误记成 Delete 事件——该期望行为已由 `audit_capture_test.go` 的 `TestAuditCapturePlugin_SoftDelete_ClassifiesAsUpdateWithRealDiff` 显式断言钉死，避免被写成"在 `Delete` 里手动再发一条 Delete 审计事件"跟自动采集重复；`HardDelete` 是真正的物理 `DELETE`，走自动采集的 Delete 分支（`After` 为 nil、成功只发一个事件、调用方不得再手动补发），由 `TestAuditCapturePlugin_HardDelete_ClassifiesAsDelete` 与 `TestAuditCapturePlugin_HardDelete_NoMatchingRow_PublishesNothing` 钉死。**署名义务是调用方的，不是门禁的**：捕获插件从写入 context 读 `pkgcore.ActorFromContext`，而系统上下文本身不携带 Actor——`pkgcore.WithSystemContext` 只存 `SystemReason`（其 `Actor` 是无结构字符串，命名授权给谁，从不提升进结构化的 Actor 载体），tenancy 的审计封装返回的也正是这个 context——所以在系统上下文下执行 `HardDelete` 的调用方必须先叠加 `pkgcore.WithActor`（模拟操作下再加 `pkgcore.WithOnBehalfOf`，见双身份规则），否则擦除记录——主体行即将不复存在的那一条审计记录——将以零 Actor 落库。该行为由 `TestAuditCapturePlugin_HardDelete_SystemContextAlone_DoesNotAttribute` 显式钉死；`hard_delete.go` 的文档注释与 `go/dbkit/AGENTS.md` 的"Hard deletion"小节均明示该义务，参考应用 notes 消费方（`TestRepository_HardDelete_SoftDeletedNote_PhysicallyRemoved`）演示的就是合规形态（先在租户 context 上叠加 `pkgcore.WithActor` 再授予系统上下文）。
   - **唯一索引**：软删除行仍是一行真实数据，会继续占用唯一约束——例如 `go/org` 的 `UNIQUE(tenant_id, parent_id, name)`，删除一个节点后在保留窗口内用同名重建会被挡住。落地时需要在"把 `deleted_at` 并入唯一索引做局部索引（`WHERE deleted_at IS NULL`，PostgreSQL 与 SQLite 均支持，不违反双方言约束）"与"接受名字要等清理/彻底删除后才能复用"之间选一个，具体由模块决定，本节只要求不能被遗漏。**已有实践收敛成一条经验规则**：`go/org`、`go/rbac` 都选了前者——把已存在的完整唯一索引原地改写成局部索引；`go/integration` 的 `WebhookSubscription` 迁移则是第三种、本节未列出的真实情况——这张表压根没有会被软删除行占用的唯一索引（只有一个非唯一索引），迁移因此只加两列、不改任何索引。三个真实采用方目前是同一条规则："已有唯一索引就原地改成局部索引；没有就什么也不用做"，尚未出现真正采用"等清理后才能复用"这条被否决分支的模块。
   - **RLS**：软删除行对 PostgreSQL RLS 而言就是普通行，`tenant_id` 过滤照常生效；`HardDelete` 用的物理 `DELETE` 同样过 RLS，不需要额外设计。
   - **软删除只是隐藏，不是安全边界，更不是合规意义上的"已删除"**：软删除行仍是数据库里明文存在（加密字段除外）的一行，SQLite（standalone 模式默认方言）没有 RLS，隐藏能力完全靠 Go 层的 GORM scope 承担；不能把"用户在界面上删除了"等同于"数据已经不在了"。只有 `HardDelete` 之后，数据才算真正从库里消失——这正是合规场景要求彻底删除、软删除不能充数的原因。

**边界**：本节只定义 `dbkit.Repository[T]` 这一层两条删除入口的职责边界；保留期的按租户可配置值、"被遗忘权"请求的受理与导出编排本身，是 `compliance` 模块的治理层职责，且已经落地（见上方状态注记）；仍未落地、真正留给后续轮次的是 `go/compliance/AGENTS.md` 记录的"Known limitations"——仅追加强制、哈希链、分区归档、格式化报表导出——以及跨业务模块的媒体/派生资源级联清理各自由哪个业务模块自己负责这类编排细节，见 [10 合规与审计](10-compliance-and-audit.md)。
