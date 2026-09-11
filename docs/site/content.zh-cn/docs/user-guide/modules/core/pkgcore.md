---
title: pkgcore
weight: 1
description: "依赖底座——Module/Registry/Kernel 组装契约、租户上下文、基础设施接缝接口、结构化错误与合并后的消息目录。"
---

# pkgcore

speed 服务的依赖底座:每个其他 Go 模块都导入它,而它不导入其中
任何一个。

pkgcore 只拥有七样东西:`Module`/`Registry`/`Kernel` 组装契约——
每个模块一次 `Register(reg Registrar)` 调用;租户上下文与裸的系统
上下文标记;基础设施接缝接口 `KVStore`、`EventBus`、`Mailer`、
`ObjectStore` 及各自的进程内实现(内存存储与总线、控制台发信器、
本地对象存储,同时充当测试替身);`Bootstrap` 借以解析并校验装配的
注册表/能力/预设机制;合并后的后端消息目录;`DeploymentMode` 枚举;
以及每条接缝的每个实现都必须通过的契约套件(`eventbustest`、
`kvstoretest`、`mailertest`、`objectstoretest`)。它的子包承载各有
归属的部件:`apperr`(每个模块返回的结构化错误)、`config`(启动参数
的加载器——只处理进程启动值)与 `i18n`(以接收方语言渲染后端生成的
内容)。数据库访问、SQL 层租户强制、日志、运行时配置与任务执行被
刻意划在范围之外——那是它上面各模块的事。

## 何时选用

永远选用:平台每个模块都建在 pkgcore 上,所以每个 speed 二进制都
携带它;消费方在自己的接缝处也握着 pkgcore 的类型——租户来自
`pkgcore.TenantID` 上下文,被拒的操作以 `apperr` 码应答,`Registry`
交出已解析的总线与存储。在其中,你扮演两种角色之一:

- **模块作者**——你的业务模块实现 `pkgcore.Module`,通过一次
  `Register` 调用贡献路由、配置项、功能开关、权限、任务处理器、
  通知类型、事件与审计动作。此后不要再给 `Module` 加方法:在
  锁步版本化下那会同时打破所有模块——这正是横切机制改为落在
  `Registrar` 声明面上的原因。
- **宿主**——你的二进制用 `Kernel.Bootstrap` 组装模块,并声明自己
  以哪种拓扑运行。裸 `NewKernel()` 默认即 standalone:全部进程内
  接缝、零配置、无外部服务。

## 接线与最少使用

模块在 `Register` 里声明自己的全部表面——不做 I/O、不启动任何
服务;注册顺序从不重要,由 `DependsOn` 与 `Bootstrap` 的排序决定:

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

宿主引导它——模块集是你自己的组装(`pkgcore` 不附带任何应用):

```go
// 宿主的引导目标结构体:每个进程启动键一个字段。
// go/pkgcore/config 的 loader 依次从旗标、环境变量、可选配置文件与
// 结构体自身默认值填充它;字段可用 config:"env=..." 钉住确切的变量名。
var boot struct {
    DeploymentMode string
}
if err := config.New().Load(&boot); err != nil {
    return err // loader 会点名该键以及它查过的每个来源
}
mode, err := pkgcore.ParseDeploymentMode(boot.DeploymentMode)
if err != nil {
    return err
}
// 宿主自己的模块值:它的业务模块实现的 pkgcore.Module,
// 加上它自行接线的 config 与 jobs 模块。
reg, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(mode)).
    Bootstrap(ctx, billingModule, orgModule)
if err != nil {
    return err // Prepare 阶段点名了组件、缺失能力与模式(ErrCapabilityUnsatisfied)
}
```

分布式宿主用真实实现替换预设默认:`WithEventBus(bus,
pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart)`、`WithKVStore`、
`WithMailer`、`WithObjectStore` 注入一个实现并声明其能力位;或者
`WithPreset(pkgcore.PresetDistributed)` 指名内置的 Redis/SMTP/S3
组合。业务代码绝不按模式分支——它只看到 `Registry` 上已解析的接缝。

错误走 `apperr` 契约:构造器如
`apperr.NotFound("billing.subscription_not_found")`,配合
`WithParam`/`WithCause`(两者都派生新错误,包级值可共享)。码是稳定、
机器可读的 API 契约;人类可读文本绝不住进码里。参数会原样序列化进
响应体——只用已知安全的标量,绝不放秘密;客户端不该看到的东西走
`WithSensitiveParam`。

## 核心概念与 API 要点

- **`Registry`**——每个机制一个字段(`Routes`、`Config`、`Features`、
  `Permissions`、`Jobs`、`Notifications`、`Events`、`AuditActions`、
  `Retention`、`Schedules`),
  由三参 `NewRegistry(bus, kv, mailer)` 构建,或由 `Bootstrap`
  安装——它同时解析 `ObjectStore()` 与合并后的 `Locales()` 目录。
  `Registry.EventBus()` 就是注册器背后的总线,宿主发布进模块订阅
  的地方。
- **接缝与能力**——每个接缝接口按已注册实现里最弱的那一个设计
  (`KVStore` 上没有服务端脚本);实现声明自己的能力位:四个基础设施
  接缝上是 `MultiReplicaSafe`/`SurvivesRestart`/`Stateless`(由装配
  校验),go/pki 的 `Signer` 实现另有 `KeyNeverLeavesBoundary`——经
  Signer 自己的注册表声明,装配并不把它与任何要求作比较(该缺口由
  go/pki 自己的文档记录)。`Bootstrap` 让无法满足所声明模式能力的组
  合启动失败;缺 `SurvivesRestart` 只是启动警告,`Stateless` 实现连
  警告都免。
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
  住在各自子包,由 `init()` 自注册;指名某个实现的预设只有在宿主
  空导入其子包后才能解析——`database/sql` 式的公认代价
  (`ErrUnknownImplementation` 会点名缺失的导入)。

## 边界与注意

- 新的横切机制落在声明面上(一个 `Registrar` 访问器加其背后的
  `Registry` 字段),绝不作为新的 `Module` 方法。
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
