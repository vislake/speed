# 数据层与多租户隔离

> ORM 选型与双方言约束、数据分域（哪些表带 tenant_id）、租户上下文的信任边界、隔离三重防护，以及受控的系统上下文逃生舱。

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
