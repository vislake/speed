---
title: app
weight: 1
description: "go/app 的设计——唯一没有业务域的模块为何存在、三个包如何按依赖成本拆分,装载器为何不带隐式默认,以及中间件次序为何如此固定。"
---

# app

`go/app` 是 speed 的**应用装配层**:每个应用的启动代码所基于的结
构,也是仓库里唯一没有业务域的模块。[app 使用
页](/zh-cn/docs/user-guide/modules/app/)展示调用;本页讲模块为何把
边界画在这里。

## 职责与边界

模块守住的分工是**结构归这里,策略归宿主**:

- 归此处:装载器(配置装载与 composition 计划)、阶段驱动器、关停
  时序、引擎与其消费者共享的 HTTP 帮手(`AuthnAPIPath`、服务时
  限、`PreAuthAllowlist`)、固定中间件链与缝桥接。
- 归宿主:组装哪些组件、用什么取值、写哪些种子、宿主自己的配置
  target 声明哪些键、自己的路由与规则。这些全部经组件描述符、
  load spec、代码覆盖或 `chain.Config` 字段进入。

三条禁令把边界磨得更利:

- **无隐式默认。** 装载器指名它要装载的 target,缺了就装配失败;宿
  主没供给的值保持未设。唯一有记录在案的例外是内置 composition
  层——standalone 部署默认与默认参与的 observability 组件——两者都
  可被更高来源取消。
- **引擎核心里没有 HTTP 组装、没有监听。** `driver.go`、`loader.go`
  与 `component_observability.go` 两者皆无;路由由宿主自己的应用组
  件从声明座席组装,listener 由它自持。
- **绝不构造基础设施实现。** 一个进程跑哪个
  EventBus/KVStore/Mailer/ObjectStore 是组装应用的决定,二进制带哪
  些 SQL 方言包是宿主的 blank import。这由机制强制、不止于承诺:
  app 位于 `go/` 之下,仓库的具体基础设施 depguard 规则(redis、
  minio、asynq 与方言驱动)对它与对任何业务模块一视同仁。而宿主自
  己的模块绝不依赖 app——业务模块回到它的 import 边就是该停下的
  地方。

## 无域模块为何存在

模块纪律按领域内聚划分模块,而 app 没有领域:它不注册路由、配置
schema、功能旗标、权限、任务 handler 或审计动作,也不实现
`pkgcore.Module`。它是这条纪律**唯一有记录的例外**,理由是一笔交
换:装配层之所以成立,恰恰因为替代品是同一份胶水——配置装载、启
动次序、组件驱动、关停时序——在每个消费方手里手写维护、逐宿主漂
移。两个消费者让这个论证具体化:参考应用(完整组装)与 `saasctl
new` 物化出的项目骨架(最小选集)都经本模块的面组装。

## 三个包,按依赖成本拆分

包布局由实测成本决定,不是口味。根包的裸消费者——用一个一次性模
块在 `GOWORK=off go mod tidy` 下测量——要付 36 条 `// indirect`:
根 import 的同仓模块(config、observability、tenancy)加上它们拖进
的 koanf、go-i18n、OTel 与 GORM 栈。这份测量驱动了拆分:

| 包 | 闭包 | 后果 |
|---|---|---|
| 根 | pkgcore(及其 config 子包)、config、observability、tenancy——经 config 还有 dbkit 及其 GORM | 每个组装都背着它;它绝不 import 链或桥接的参与者,也不 import 任何业务模块 |
| `chain` | 根 + authn + rbac + tenancy + pkgcore——以链自身参与者为界 | impersonation 装饰器保持为宿主构造的普通 `func(http.Handler) http.Handler`,admin 前缀以字符串经选项到达,因此不需要 import admin |
| `bridges` | ai-gateway、billing、metering、org、sharing、authn、config | 只有接这些模块的宿主才付费 |

指导规则:根是每个组装都背的地板,所以任何会加宽它闭包的东西都
移到"其自身参与者需要它"的那个包里。

## 装载器:五个来源、每个键一个声明点

装载器跑在第一阶段之前,因为它解析的东西正是装配所计划的——它没
跑之前什么都选不了,所以没有哪个 composition 能取消它。它按序做
三件事:用 `pkgcore/config` Loader 装载宿主的配置 target(与声明键
相同的选项和来源,于是宿主键与声明键在同一次装配里绝不会解析出不
同结果);从五个来源(内置默认、项目文件、环境、命令行、宿主代码
覆盖)解析 composition 配置,连配置 target 一起发布进注册表;并
在同一条链上解析每个已注册组件声明的 `BootstrapKeys`。

第三步正是省掉一个结构体的设计:声明键**背后没有宿主结构体字段**
——声明在哪里做出,就在哪里解析——所以没有什么去绑定它,也没有什
么会绑定失败。没有来源供给的声明键解析为空,需要它的消费者自行上
报缺失的材料。真正会让装载失败、且在构造任何东西之前失败的,是
无法作为一个 schema 解析的声明集:畸形键路径、闭集之外
(string、int、bool、hexkey)的格式、没有 Description 的 Sensitive
键,或两个组件对同一键路径声明不一——一个键路径每次装配只有一个
解析,哪份声明胜出将不可判定。每个问题都点名阶段、声明组件、原因
与补救办法。

声明方文档化的开发默认(`ConfigDevDefaults`)站在结构体默认过去所
在的位置:低于 root-key 派生与显式来源——所以从密钥库取密钥材料的
部署不传它,声明键在有来源供给之前解析为空。

## 链:次序为何是这条次序

`chain.Chain` 是固定中间件次序唯一的家:`authn.Middleware(verifier)`
最外——这是**唯一只验证一次 token 的次序**。`tenancy.Resolver` 的签
名无法把已验证 JWT 的 claim 交给下游,先跑 tenancy 会迫使每条
token 在两个可能漂移的代码路径上验两遍——而最终做决定的那条,正是
什么都没认证的那条。验证恰一次之后,挂着
`authn.NewPrincipalResolver()` 的 `tenancy.Middleware` 从请求
context 读出已验证的 Principal,并且始终是设置租户上下文唯一的地
方。authn 中间件是*可选*认证(无 token 则无 Principal 继续;无效
token 立即 401),这正是为什么 tenancy 的 fail-closed 默认——拒绝
任何 (method, path) 不在 allowlist **且** resolver 失败的请求
——能让每条挂载路由无需逐路由包壳就受到保护。

两个分支从 authn 的输出**按结构**(挂载路径,而非 allowlist 条目)
分发,各有 allowlist 无法表达的理由:

- **authn 自己的子树**:authn 操作按 Principal 自身 claim 逐操作解
  析租户,且企业 SSO 的动态 `oidc:<tenant>` 登录起始路径根本无法枚
  举为精确 (method, path) allowlist。
- **admin 的路由**:它的权限在 `rbac.SystemDomain` 里按调用者自己
  的、未被替换的 Principal 评估,所以租户解析与 impersonation 替换
  都不得先于它;impersonation 装饰器本身卡在 authn 与 tenancy 之间
  ——它唯一正确的位置——因为它读真实、已验证的 Principal,且仅在请
  求携带有效 impersonation 授权 id 时,为下游全部替换成目标
  Principal。

`chain.Standard` 从已 bootstrap 的注册表推导整份组装,而不接受预先
切好的部件——它的 `RouteSource` 参数由两种注册表形态同时满足(模块
Registry 与组件装配的 `ComponentRegistry`)——业务半边由宿主经选
项供给。`chain.Chain` 是路由布局自定义的宿主的直通路径;不含
authn 模块的选集完全不走链。用哪个入口是宿主的决定,而这份弹性是
刻意的:注册表路由集就是面子时用 `Standard`,布局是宿主自己的时
用 `Chain`。

## 七阶段驱动与其失败语义

驱动器只做编排。`Assemble` 跑装载器并把注册表走过 Prepare、
Construct、Verify、Init 与 Start;`Shutdown` 执行两相关停(非阻塞
的 Stop 通知,然后逆序 Close、聚合错误);`RunAssembly` 是糖——从
全局注册加 extra 建注册表、驱动、等 context、关停。失败语义是注
册表的:Prepare 失败没有可回滚的东西;Construct 起的失败在返回错
误前,把每个已构造组件按逆序关闭恰好一次——调用方绝不需要自行拆
卸。有一个洞被接受并记录在案:路由冲突在装配期 panic
(`pkgcore.MountRoutes` 的契约),拿到最响亮的报告,且 panic 时不
跑回滚。

## 强制:一份内核、两个宿主、一道门

共享内核这条性质由工具保护,不靠约定。
`tools/check_host_composition.py` 扫描两棵消费者树(参考应用与骨架
内嵌项目)并拒绝:宿主的代码里重声明内核标识符;重长出内核自有的
语句(服务循环、liveness 与 authn 路径字面量);以及重发引擎自有
的装配调用(`dbkit.Open`、`dbkit.NewMigrationRegistry`、
`http.NewServeMux`、`jobs.NewStandaloneQueue`/`jobs.Wire`、
`signal.NotifyContext`、`chain.Chain`、`obs.Init`、
`pkgcore.NewKernel`、`.Bootstrap(`、`pkgcore.NewComponentRegistry`
与组件各阶段)——每一项各有唯一指名文件的豁免,用于宿主组件合法拥
有该调用的地方。而 depguard 规则覆盖"不构造基础设施"的那一半,
对 app 与对每个模块同等。

这道门的设计点同时就是模块的采用测试:对引擎、链、allowlist 或服
务生命周期的任何改动,都必须让两个消费者继续工作;一个骨架最小选
集(无 authn,因而根本没有 `chain.Chain` 调用)无法采用的改动,是
"它不属于这里"的症状。

## 取舍与冻结面

- **无域模块胜过逐宿主胶水。** 代价就是这份例外:图里多一个模块、
  多一份要冻结的导出面,换来的启动是构造性共享,而不是靠模仿。
- **实测、拆开的闭包胜过顺手 import。** 根包 36 条的测量,就是链
  与桥接不在根包里的理由——尽管多数宿主最后三个包都会 import。
- **固定链序,逃生舱在链外。** 次序不做参数化:`chain.Chain` 构造
  即装 `authn.NewPrincipalResolver()` 与平台 pre-auth allowlist;
  链需要不同 resolver 或不同 pre-auth 集合的宿主应自行组装
  `tenancy.Middleware`,而不是去掰配置。目前没有消费者需要那么
  做;把这条写成已知限制,就是诚实的边界。

对消费者冻结的面:`Assemble`、`Shutdown`、`RunAssembly`、`Load`、
`LoadSpec`/`CompositionOverrides`、`Config*` 选项构造器、根帮手
(`AuthnAPIPath`、`ReadHeaderTimeout`、`ShutdownTimeout`、
`PreAuthAllowlist`),以及 `chain`/`bridges` 两包的入口。
observability 组件默认参与,像任何其他组件一样经 composition 块
(`service_name`、`otlp_endpoint`)配置。

## Source

- 模块纪律:[go/app/AGENTS.md](https://github.com/vislake/speed/blob/main/go/app/AGENTS.md)

## 相关页

- 使用:[用户指南的 app](/zh-cn/docs/user-guide/modules/app/)
- [总体架构](/zh-cn/docs/developer-docs/architecture/)——装配层在模
  块图之上的位置
- [pkgcore 设计](/zh-cn/docs/developer-docs/modules/core/pkgcore/)
  ——驱动器所走查的 `Component`/`ComponentRegistry` 契约
