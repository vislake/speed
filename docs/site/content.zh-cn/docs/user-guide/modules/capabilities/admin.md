---
title: admin
description: "平台员工运营控制台:租户账本与停用、冒充、跨租户搜索、审计查询与导出、角色管理与用量仪表盘——全部搭建在其它每个模块已提供的能力之上。"
weight: 5
---

# admin

admin 是运营控制台后端:面向平台员工的操作面,搭在其它每个模块已
有的能力之上。它坐在模块依赖图最顶端,是唯一被明确允许直接 import
其下每个模块具体包的模块——它是运营者的座位,由 `authn`、`org`、
`rbac`、`tenancy`、`compliance`、`notification`、`metering` 与
`billing` 组合而成,绝不是其中任何一个的并行实现。

## 它做什么

七个面,每个都是对下层模块的薄而真实的组合:

- **租户账本与停用。** `admin_tenants`(平台数据)记录平台认为存在
  的租户——由 `org` 的 `org.node.created` 事件加手动 CRUD 填充——
  是运营便利,绝不是权威来源。把租户状态 PATCH 成 `suspended` 有真
  牙齿:`TenantService` 结构化实现 `tenancy.TenantStatusResolver`,
  宿主一旦把它接进自己的 `tenancy.Middleware`,触碰被停用租户的请
  求在紧接着的下一次调用即答 `tenancy.tenant_suspended`,覆盖所有
  非 allowlist 路由。
- **冒充。** 短命(30 分钟)、可显式撤销的 `ImpersonationGrant`——
  绝不是给目标用户铸造的真 token。`ImpersonationMiddleware` 插在
  `authn.Middleware` 与 `tenancy.Middleware` 之间,在请求的剩余部分
  里替换成目标的身份,同时管理员自己仍经验证的 token 照旧;该请求
  产生的每条审计记录都带双重身份(`Actor` = 被冒充用户,
  `OnBehalfOf` = 管理员),被冒充用户还会收到一封必达、不可退订、
  文案不泄漏运营者任何信息的安保通知。
- **跨租户用户搜索。** 身份查找用 `authn.Service.SearchUsers`,成员
  关系按候选租户逐个 `org.MemberService.Get`——每次跨租户读都走在
  命名运营者的 `tenancy.WithSystemContext` 审计包装之下。
- **审计查询与导出。** 包在 `compliance.AuditQuery` 上的薄 HTTP 壳
  (带 `onBehalfOf` 过滤维度),加一条异步导出腿:入队一个 `jobs` 任
  务,worker 跑 `compliance.ExportService.Export`,完成的导出以
  `go/sharing` 单次分享链接投递——其一次性 token 刻意绝不落进持久
  的 job 记录。
- **角色管理。** 包在 `rbac.Service` 上的薄壳(`DefineRole`/
  `AssignRole`/`RevokeRole`/`RestoreRole`),经装配后的
  `AttachRBAC` 接线;凡是命名租户的写都直接拒 `rbac.SystemDomain`
  伪租户,只握 `admin:roles_manage` 的调用方无法把平台运营权委托给
  自己。
- **用量/账单仪表盘。** `GET /api/v1/admin/usage-summary`——metering
  汇总与 billing 余额、订阅的按租户拼接,自身无表。
- **发送记录检索。** notification 的投递记录,按租户过滤或跨账本查。

HTTP 面是模块自己的片段,挂在 `/api/v1/admin` 下;每条路由由宿主以
`rbac.SystemDomain` 权限(`admin:impersonate`、`admin:audit_read`
等)上门禁——模块自己不判授权,它的路由也从不坐在普通租户解析之
后:它们是*关于*租户的,不是局限于一个租户的。

## 何时选用

你运营平台,不止运营一个租户:员工需要租户账本、停用某个租户的权
力、在完整审计下查看并扮演某个用户、跨租户搜索、读取并导出审计
轨迹、管理角色、瞄一眼跨租户用量。每个面都是可选接线——admin 组
合你给它的东西,对它声明表面所必需的部分缺失时,装配直接拒绝
绝。

## 怎么接线

```go
a := admin.NewModule(db,
    admin.WithAuthn(authnModule),           // 必填:每缺一个 Option,
    admin.WithOrg(orgModule),               //   装配都以各自
    admin.WithCompliance(complianceModule), //   的命名错误失败
    admin.WithNotification(notificationModule),
    admin.WithQueue(queue),                 // 审计导出绝不能在请求内同步跑
    admin.WithMetering(meteringModule),     // 可选:用量汇总维度
    admin.WithBilling(billingModule),       // 可选,与 metering 相互独立
)
// 放进你的组合选出的组件集。组件自己的 Start 回合会替你绑定已发布的
// rbac Service;手工接线的模块则在装配返回后自己调用 AttachRBAC——
// rbac.Service 是在装配的 Init 窗口内发布的:
a.AttachRBAC(rbacService)
```

停用与冒充经你自己的管线构造生效,不是模块路由——冒充坐在
`authn` 与 `tenancy` 之间,租户状态解析器骑在 tenancy 层上:

```go
handler := authn.Middleware(verifier)(
    admin.ImpersonationMiddleware(a.Impersonation())(
        tenancy.Middleware(authn.NewPrincipalResolver(),
            tenancy.WithTenantStatusResolver(a.Tenants()),
        )(mux),
    ),
)
```

不带 `X-Admin-Impersonation` 头的请求原样通过;有效授权在那一请求
的剩余部分替换目标的 `Principal`,并把双重身份盖到它的上下文上。

## 核心概念与 API 面

- **模块访问器:** `Tenants()`、`Impersonation()`、`Search()`、
  `Roles()`、`Usage()`、`Export()`——加 post-assembly 的
  `AttachRBAC(svc)`。`Roles()` 与 `Impersonation()` 在 `AttachRBAC`
  跑之前以 `ErrRBACServiceRequired` 失败关闭——必须能切断授权的机
  器还没就位时,授权绝不诞生。
- **活授权在其管理员的权限被撤销那一刻即结束**——
  `ImpersonationService` 订阅 rbac 的撤销事件并重验 `Can` 后结束受
  影响授权;请求路径本身不带任何逐请求权限检查。
- **两张平台数据表**(`admin_tenants`、`admin_impersonation_grants`),
  绝不做租户作用域,写都走带守卫的 compare-and-set:停用撞恢复、或
  两次并发的授权结束——第二次被拒,绝无静默的陈旧覆盖。
- **用量汇总的 GET 带一笔文档化的写**——billing 的先读先物化契约
  会给账本里每个还没有余额行的租户建一行零余额;其它表一概不碰。
- **结构化错误码**——其中 `admin.roles_system_domain_forbidden`——
  索引在[错误码索引(English)](/docs/user-guide/error-codes/#admin)。

## 已知限制与链接

- 账本刻意不权威:租户创建不靠它强制,缺行读作 `active`,绝不误
  停。
- 无双人复核工作流,也没有对 admin 动作(冒充在内)的限流或异常检
  测——如实记录的排除项。
- 冒充 TTL 是固定 30 分钟常量;审计查询与跨租户发送记录的分页继承
  底下 compliance 查询的 Go 侧切片。
- `RestoreRole`/`EnsureBuiltinRoles` 只在 `RoleService` 层,没有
  HTTP 路由。
- admin 自己没有 Docker 集成层;组合接线由它对真实下游模块的单测
  套件与参考应用的端到端流测试证明。

### 出处

- [go/admin/AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md)——权威文档(各面、接线契约、冒充管线、限制)
- 相关页面:[compliance](/zh-cn/docs/user-guide/modules/capabilities/compliance/)、[tenancy](/zh-cn/docs/user-guide/modules/core/tenancy/)、[notification](/zh-cn/docs/user-guide/modules/services/notification/)、[metering](/zh-cn/docs/user-guide/modules/services/metering/)、[billing](/zh-cn/docs/user-guide/modules/capabilities/billing/)
