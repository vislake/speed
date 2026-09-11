---
title: pkgcore
weight: 1
description: "依赖底座——模块/组件组装契约、租户上下文、基础设施接缝接口、结构化错误与合并后的消息目录。"
---

# pkgcore

speed 服务的依赖底座:每个其他 Go 模块都导入它,而它不导入其中
任何一个。

pkgcore 只拥有七样东西:模块/组件组装契约——每个模块实现随附一个
`Component` 描述符,其 `Init` 回调执行一次
`Register(reg *pkgcore.ComponentRegistry)` 声明体;租户上下文与裸的系统
上下文标记;基础设施接缝接口 `KVStore`、`EventBus`、`Mailer`、
`ObjectStore` 及各自的进程内实现(内存存储与总线、控制台发信器、
本地对象存储,同时充当测试替身);装配借以解析并校验组合的
组件注册表/能力机制;合并后的后端消息目录;`DeploymentMode` 枚举;
以及每条接缝的每个实现都必须通过的契约套件(`eventbustest`、
`kvstoretest`、`mailertest`、`objectstoretest`)。它的子包承载各有
归属的部件:`apperr`(每个模块返回的结构化错误)、`config`(启动参数
的加载器——只处理进程启动值)与 `i18n`(以接收方语言渲染后端生成的
内容)。数据库访问、SQL 层租户强制、日志、运行时配置与任务执行被
刻意划在范围之外——那是它上面各模块的事。

## 何时选用

永远选用:平台每个模块都建在 pkgcore 上,所以每个 speed 二进制都
携带它;消费方在自己的接缝处也握着 pkgcore 的类型——租户来自
`pkgcore.TenantID` 上下文,被拒的操作以 `apperr` 码应答,
`ComponentRegistry` 的访问器交出已解析的总线与存储。在其中,你
扮演两种角色之一:

- **模块作者**——你的业务模块随附一个 `pkgcore.Component` 描述符
  (名字、七个生命周期回调与资产 embed),其 `Init` 回调执行一次
  `Register(reg *pkgcore.ComponentRegistry)` 声明体,贡献路由、
  配置项、功能开关、权限、任务处理器、通知类型、事件与审计动作。
  此后不要再扩张描述符的契约:在锁步版本化下那会同时打破所有
  模块——这正是横切机制改为落在 `*pkgcore.ComponentRegistry`
  声明面上的原因。
- **宿主**——你的二进制用 `pkgcore.NewComponentRegistry()` 组装
  组件,并用 `app.Assemble`(或 `app.RunAssembly` 糖)驱动七个
  阶段,在组合配置里声明它以哪种拓扑运行。组合配置的
  `deployment` 键默认 standalone——零外部服务的形态。

## 接线与最少使用

模块在 `Register` 里声明自己的全部表面——不做 I/O、不启动任何
服务;注册顺序从不重要,由 `Requires`/`Provides` 契约令牌与装配
的依赖排序决定:

```go
func (m *BillingModule) Register(reg *pkgcore.ComponentRegistry) error {
    reg.RoutesSeat().Mount("/api/v1/billing", m.router())
    if err := reg.PermissionsSeat().Add("billing:read", "billing:write"); err != nil {
        return err
    }
    if err := reg.EventsSeat().Publishes(pkgcore.EventDecl{
        Type: "billing.invoice.paid", PayloadType: "billing.InvoicePaid",
        Description: "An invoice was paid in full.",
    }); err != nil {
        return err
    }
    reg.EventsSeat().Subscribe("authn.user_created", m.openCreditLedger)
    return nil
}
```

宿主引导它——组件集是你自己的组合(`pkgcore` 不附带任何应用):

```go
// 宿主的引导目标结构体:每个进程启动键一个字段。
// go/app 的 loader 依次从旗标、环境变量、可选配置文件与
// 结构体自身默认值填充它;字段可用 config:"env=..." 钉住确切的变量名。
var boot struct {
    DatabaseDSN string
}
// 宿主自己的组件:它的业务模块描述符,加上它自行接线的
// config 与 jobs 组件。
reg := pkgcore.NewComponentRegistry()
for _, c := range hostComponents() {
    if err := reg.Register(c); err != nil {
        return err
    }
}
// 组合配置的代码覆盖层列出这个二进制选择的组件
// (nil 选中,false 剔除);deployment 键默认 standalone。
composition := pkgcore.ComponentConfig{}.With("components",
    pkgcore.ComponentConfig{}.
        With("eventbus.memory", nil).
        With("kv.memory", nil).
        With("billing", nil))
spec := app.LoadSpec{
    Host:      &boot,
    Options:   []app.ConfigOption{app.ConfigEnvPrefix("BILLING")},
    Overrides: &app.CompositionOverrides{Config: composition},
}
// 七阶段驱动;拒绝会点名组件与原因(能力不足报
// ErrCapabilityUnsatisfied,带组件、缺失的能力位与模式)。
if err := app.Assemble(ctx, reg, spec); err != nil {
    return err
}
```

分布式宿主通过在组合配置里选择实现组件换入真实实现——
`eventbus.redis`、`kv.redis`、`mailer.smtp` 与 `objectstore.s3` 就是
内置的 Redis/SMTP/S3 组合,二进制导入对应子包后即可解析,各自声明
自己的能力位。业务代码绝不按模式分支——它只经注册表的访问器看到
已解析的值。

错误走 `apperr` 契约:构造器如
`apperr.NotFound("billing.subscription_not_found")`,配合
`WithParam`/`WithCause`(两者都派生新错误,包级值可共享)。码是稳定、
机器可读的 API 契约;人类可读文本绝不住进码里。参数会原样序列化进
响应体——只用已知安全的标量,绝不放秘密;客户端不该看到的东西走
`WithSensitiveParam`。

## 核心概念与 API 要点

- **`ComponentRegistry`**——每个机制一个座席字段(`Routes`、
  `Config`、`Features`、`Permissions`、`Jobs`、`Notifications`、
  `Events`、`AuditActions`、`Retention`、`Schedules`),用
  `pkgcore.NewComponentRegistry()` 从包级组件注册加宿主自有的组件
  创建;座席仅在 Init 阶段接受写入。它的访问器按类型上下文读出
  已组装的值——`EventBus()`、`KVStore()`、`Mailer()`、
  `ObjectStore()` 与合并后的 `Locales()` 目录,装配不携带时各自
  为 nil。`EventBus()` 就是注册器背后的总线,宿主发布进模块订阅
  的地方。
- **接缝与能力**——每个接缝接口按已注册实现里最弱的那一个设计
  (`KVStore` 上没有服务端脚本);实现声明自己的能力位:四个基础设施
  接缝上是 `MultiReplicaSafe`/`SurvivesRestart`/`Stateless`(由装配
  校验),go/pki 的 `Signer` 实现另有 `KeyNeverLeavesBoundary`——经
  Signer 自己的注册表声明,装配并不把它与任何要求作比较(该缺口由
  go/pki 自己的文档记录)。装配的 Prepare 阶段让无法满足所声明模式
  能力的组合启动失败;缺 `SurvivesRestart` 只是启动警告,
  `Stateless` 实现连警告都免。
- **租户上下文**——`WithTenant`/`TenantFromContext`/
  `MustTenantFromContext`(失败关闭:`ErrNoTenant`,绝无「所有租户」
  语义),以及本身不压制任何东西的 `WithSystemContext` 标记——谁
  能用、如何审计,是 `tenancy` 包装器的职责。
- **Actor**——`WithActor`/`WithOnBehalfOf` 各自独立铺层,让被
  冒充操作的审计记录能同时携带两个身份。
- **目录**——后端处理器绝不翻译响应(它们返回码);`pkgcore/i18n`
  渲染后端自己生成的内容——邮件、发票、通知文案——用接收方语言,
  缺消息是错误,绝不回落到另一语言。
- **子包实现**——Redis、PostgreSQL、NATS、Memcached 与 S3 实现
  住在各自子包,由 `init()` 自注册;组合只有在宿主空导入其子包后
  才能选中对应组件——`database/sql` 式的公认代价(拒绝会点名该
  组件并列出已注册的组件)。

## 边界与注意

- 新的横切机制落在声明面上(`ComponentRegistry` 上的一个座席
  访问器加其背后的注册器),绝不作为新的描述符回调。
- `Register` 不得做 I/O;注册器错误绝不是合并——跨模块重复键会让
  注册失败。
- 不要为接缝写 mock:`NewMemoryKVStore`、`NewMemoryEventBus`、
  `NewConsoleMailer`、`NewLocalObjectStore` 就是测试替身;四条
  契约套件是任何自注册实现的强制检查。
- 启动 `config.Loader` 只处理启动值——运行时与租户可覆盖的设置
  属于 `config` 模块。
- `SystemPurpose` 必须先经 `RegisterSystemPurpose` 声明,`WithSystemContext`
  才会接受;系统上下文也不是授权绕过。
- 根包零第三方依赖;分布式实现需要的 SDK 都被关在各自子包里。
  加依赖前先测量:它会落进每个消费方的 `go.sum`。

## Source

- [pkgcore AGENTS.md](https://github.com/vislake/speed/blob/main/go/pkgcore/AGENTS.md)
- [pkgcore `example_test.go`](https://github.com/vislake/speed/blob/main/go/pkgcore/example_test.go)
