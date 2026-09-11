---
title: dbkit
weight: 2
description: "双方言(PostgreSQL 与 SQLite)数据访问层——唯一合法的 Open、强制的租户级 Repository[T]、版本化迁移、带盲索引的字段级加密,以及从软删到受限硬删除的删除语义。"
---

# dbkit

speed 服务的双方言数据访问层:唯一合法取得 `*gorm.DB` 的途径
(`Open`)、业务仓库必须内嵌的租户级 `Repository[T]`、版本化 SQL
迁移、带 HMAC 盲索引的字段级加密,以及从软删到系统上下文门控的
物理删除的完整删除语义。

dbkit 所做的一切都服务于三层租户隔离防线:GORM 插件(`Open` 在每条
连接上安装)注入 `WHERE tenant_id = ?`,无租户即失败关闭;泛型
`Repository[T]` 自己先从上下文解析租户、逐行复验;`WithTenantSession`
让每次仓库调用都跑在显式事务里,并设置部署方预置的 RLS 策略所依赖
的 PostgreSQL 会话变量(`app.current_tenant`)。每一层即使下面的层
被绕过也独自成立。

## 何时选用

任何拥有数据库行的模块都用它——没有别的入口:`gorm.Open` 与方言
驱动在整个平台里从不被直接调用。租户数据与关联表实现 `TenantScoped`
(内嵌 `TenantModel`,或在需要 `(tenant_id, id)` 复合主键时自行声明
`TenantID` + `GetTenantID()`),经由 `Repository[T]` 访问;身份数据
与平台数据(`users` 表、平台级套餐定义)**不得**实现 `TenantScoped`,
因此直接用 `Open` 返回的裸 `*gorm.DB`——这是安全的,因为插件忽略
非租户级模型,并由 `tenancytest.AssertNotTenantScoped` 证明。

## 接线与最少使用

```go
import _ "github.com/vislake/speed/go/dbkit/dialect/sqlite" // 或 /dialect/postgres

type Subscription struct {
    ID       string `gorm:"primaryKey;size:26"`
    TenantID string `gorm:"primaryKey;size:26;not null"`
    PlanID   string `gorm:"size:64;not null"`
    Status   string `gorm:"size:32;not null"`
}

func (s Subscription) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

type Repository struct {
    *dbkit.Repository[Subscription]
}

func NewRepository(db *gorm.DB) *Repository {
    return &Repository{Repository: dbkit.NewRepository[Subscription](db)}
}
```

打开连接并应用迁移(每个模块自带 `postgres/*.sql` 与
`sqlite/*.sql` 两套;`Apply` 按 `DependsOn` 排序,每模块一个事务):

```go
db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: dsn})
// handle err

reg := dbkit.NewMigrationRegistry()
if err := reg.Register(billingModule); err != nil { // billingModule: 你的 the module contract
    // handle err
}
if err := reg.Apply(ctx, db, dbkit.DialectPostgres); err != nil {
    // handle err
}
```

仓库调用从上下文取租户——`tenancy.Middleware` 跑过之后,请求上下文
已携带租户;后台任务用 `pkgcore.WithTenant` 自行重建。`Create` 会覆写
结构体 `TenantID` 字段上已设的任何值;`FindByID`、`Update`、`Delete`、
`Restore` 与 `HardDelete` 对「无此 id」与「属于别的租户」一律应答
`dbkit.ErrRecordNotFound`——刻意不可区分。

## 核心概念与 API 要点

- **`Open`**——校验方言(空导入驱动子包即完成注册;否则
  `dbkit.invalid_dialect` 指名修复方式),套用固定连接池上限
  (25 开 / 5 闲),ping,然后才安装插件。DSN 从不进日志或错误。
  每条 SQLite 连接带固定 5 秒 `busy_timeout`;读后升级写锁仍会以
  `SQLITE_BUSY` 快速失败,那是调用方自己的重试问题。
- **`Repository[T]`**——刻意最小:`Create`、`FindByID`、`Update`
  (整行保存,绝不是局部补丁)、`Delete`、`List`(该租户全部行;无
  过滤、无分页——更丰富的查询建在插件保护的 `*gorm.DB` 上,以
  `db.Model(&TenantScoped{})` 锚定,过滤器仍然生效)。
- **删除语义**——携带 `SoftDeletable`(`DeletedAt`/`DeletedBy`
  字段)的模型,其 `Delete` 是标记删除,并有可用的 `Restore`;软删
  行对普通读隐藏,但明文仍在表里——不是合规级删除。`HardDelete`
  才是物理删除:不可逆,上下文不带系统上下文即被拒
  (`ErrHardDeleteRequiresSystemContext`;谁有权持有该上下文是调用方
  侧白名单——admin、compliance、jobs、authn——经 tenancy 的审计
  包装器进入)。即使过了门,租户仍然强制且绑定:它绝不过租户删除。
- **加密与查找**——`dbkit.Cipher` 是 AES-256-GCM 字段加密,引导时
  注册一次(`RegisterEncryptedSerializer`);加密字段不可查询,等值
  查找走单独明文 64-hex 列上的 `BlindIndexer`——`NewBlindIndexer(
  "email_index", key, dbkit.NormalizeEmail)`,写侧 `Index(raw)`,
  查侧 `Equal(raw)`,两侧做同样的规范化。密钥 32 字节,加密与索引
  绝不共用同一把,也可用 `dbkit.DeriveKey(root, "purpose.v1")` 从
  一把根密钥派生全部。
- **审计采集**——可选 `Options.AuditBus`(加 `AuditModels`)把针对
  `Auditable` 模型的每次增删改捕获为 `dbkit.write.captured`,只在
  写入事务真正提交后发布;`go/dbkit/audit` 提供只追加的
  `AuditEvent` 模型、迁移、`Emit` 路径与持久化 `Module`。
- **测试辅助**——`dbkit/dbtest` 的 `NewSQLite(t)`/`NewPostgres(t)`
  返回插件齐备的连接供模块测试;强制的隔离断言在 `tenancy` 的
  `tenancytest` 里。

## 边界与注意

- 业务仓库绝不手握裸 `*gorm.DB` 自己写查询;`Repository[T]` 是
  底座,三个绕过点(`db.Table`/`db.Model`/`db.Raw`)由 CI 的
  semgrep 规则检查。
- 全仓无 `AutoMigrate`:迁移是每模块一套、每方言一套的版本化 SQL。
  不用 PostgreSQL 独有特性(`gen_random_uuid()`、JSONB 运算符、
  原生数组、`NOW()`):ID 应用层生成,JSON 走 `datatypes.JSON`,
  时间戳用 gorm 的 `autoCreateTime`/`autoUpdateTime`。
- 无租户的上下文失败关闭(`pkgcore.ErrNoTenant`),worker 里也一样
  ——在任务内重建上下文。
- 盲索引密钥轮换没有退役密钥回退:每行索引必须作为批处理任务重算。
- `Repository[T]` 在结构上排除身份数据与平台数据——这正是要点;
  别把 `TenantScoped` 硬加到它们身上。

## Source

- [dbkit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md)
- [dbkit `example_test.go`](https://github.com/vislake/speed/blob/main/go/dbkit/example_test.go)
- [dbkit/audit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/audit/AGENTS.md)
