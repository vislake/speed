---
title: tenancy
weight: 3
description: "多租户隔离的输入端——决定请求携带哪个租户的 resolver 与中间件,以及在少数正当理由下带审计地走出租户过滤的系统上下文包装器。"
---

# tenancy

speed 多租户隔离故事的输入端:当 `dbkit` 在上下文已经携带租户之后
强制隔离时,`tenancy` 决定这个上下文最初携带哪个租户——每个 HTTP
入口都跑在它后面的 `net/http` 中间件——并给业务代码一条带审计的
路,在少数正当理由下刻意走出租户过滤。它在依赖图里紧贴 `dbkit`
之上,除测试支撑子包外只导入 `pkgcore`。

中间件从不信任请求给出的租户。`Resolver` 是每个请求只被咨询一次、
并被完全信任的契约;每个实现都必须从服务器自己掌控的来源推导租户
——已验证令牌的声明、数据库查找——绝不从客户端附在请求上的头、
查询参数或体里取。这是模块的硬规则,不是可以配置掉的默认值。

## 何时选用

每个承载请求的服务都用 `Middleware`;问题只是用哪个 resolver。
**未认证**入口(登录页、公共品牌页)用内置 `DomainResolver`,其
`lookup` 函数把 `(*http.Request).Host` 映射到租户,查不到时回落到
默认租户——绝不报错——页面总能渲染。**已认证**请求从已验证访问
令牌的声明里解析租户;那个 resolver 是 `authn` 自己实现 `Resolver`
的类型——这里刻意没有 `JWTResolver`,因为验签是 `authn` 的事,而
依赖方向是 `authn -> tenancy`,不是反过来。

## 接线与最少使用

```go
resolver := tenancy.NewDomainResolver(lookupTenantByHost, "public") // 默认租户

mux := http.NewServeMux()
mux.HandleFunc("/login", loginPageHandler)
mux.HandleFunc("/healthz", healthCheckHandler)

protected := tenancy.Middleware(resolver, tenancy.WithAllowlist(http.MethodGet, "/healthz"))(mux)
```

下游处理器读取中间件已解析的租户:

```go
tenant, ok := pkgcore.TenantFromContext(r.Context())
// 只有解析失败且被 allowlist 豁免的请求,ok 才会是 false。
```

Allowlist 是精确匹配:`WithAllowlist(method, paths...)` 只豁免该
精确 (method, path) 对——豁免的是解析失败本会应答的 403,绝不豁免
解析成功时的租户注入本身;也没有前缀、通配或 GET 隐含 HEAD 的
便利。被豁免请求在解析失败时以**上下文中无租户**继续,这正是让
登录这类先于租户存在的路由保持可用的原因。失败应答
`tenancy.tenant_unresolved`(403),是结构化 `apperr`;resolver 侧的
细节永不进入响应体。

可选且默认关闭:`WithTenantStatusResolver` 接入 `TenantStatusResolver`
接缝,让已解析租户的停用状态真正拒绝请求(`tenancy.tenant_suspended`);
`Status` 调用本身失败则关闭拒绝(`tenancy.tenant_status_unavailable`)
——够不到的状态源是故障,绝不是「没有消息就是好消息」。该接缝是
结构化类型;`admin` 的租户台账是它的第一个真实实现。

## 带审计的逃生口

跨租户工作走 `pkgcore.WithSystemContext`——但能导入 `tenancy` 的
代码应改调 `tenancy.WithSystemContext`:它在返回提升后的上下文之前
发布一条 `tenancy.system_context.entered` 审计事件,发布失败即关闭
失败:授予了逃生口却没有相应审计记录,正是这个包装器要堵的洞,所以
总线坏了就返回未提升的原上下文加 `ErrAuditPublishFailed`。原因必须
预先声明(`RegisterSystemPurpose`),并携带 actor 与可选 ticket。

```go
ctx, err := tenancy.WithSystemContext(ctx, bus, pkgcore.SystemReason{
    Actor:   "authn.registration",
    Purpose: purposeNewAccountProvisioning, // 已用 pkgcore.RegisterSystemPurpose 声明
    Ticket:  "",
})
if err != nil {
    return err // 什么都没授予:原因被拒,或审计发布失败
}
```

这个包装器**不**做什么:它不扩宽 `dbkit.Repository[T]` 能看见的
范围。仓库绝不因系统上下文在场而扩宽(唯一例外是 `HardDelete`,
它把系统上下文当作*必须*在场的门),系统上下文也绝不顶替租户——
无租户调用照样以 `pkgcore.ErrNoTenant` 失败关闭。

## 核心概念与 API 要点

- **`Resolver`**(`Resolve(r *http.Request) (pkgcore.TenantID, error)`)
  与 `DomainResolver`;`Middleware(resolver, opts...)` 经
  `pkgcore.WithTenant` 注入。
- **`WithSystemContext`** + `EventSystemContextEntered` +
  `SystemContextEnteredEvent{Actor, Purpose, Ticket, EnteredAt}`——
  上文所述的审计包装器。
- **`tenancytest`**——`AssertIsolated[T]`(租户/关联数据走
  `dbkit.Repository[T]`:跨租户读被拒、伪造 tenant_id 被覆写、无租户
  上下文失败关闭)与 `AssertNotTenantScoped`(身份/平台数据:可见性
  被证明与任何租户上下文无关)。有仓库的每个模块必跑其一——跑哪个
  由数据域决定,绝不凭口味——PostgreSQL 形态在 `-tags=integration`
  后面。

## 边界与注意

- GORM 隔离插件与 `Repository[T]` 在 `dbkit` 里,不在这里——
  `tenancy` 既不安装也不包装它们。
- 别把 `DomainResolver` 回落默认值的行为抄进已认证 resolver:那个
  例外只为登录页还能渲染而存在;其他 resolver 宁可返回非 nil 错误
  也不发明一个租户。
- 别把 `Resolver` 答 `("", nil)` 当成功的零租户解析——中间件把它
  与解析失败同等对待。
- 停用强制恰好覆盖宿主未 allowlist 的那些路由;因为什么都不缓存,
  停用在下一个未豁免请求上立即生效。
- 能导入 `tenancy` 的代码绕过包装器直接调裸 `pkgcore` 原语,等于
  跳过审计记录——包装器才是正道。

## Source

- [tenancy AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md)
- 设计:[docs/internal/04-data-and-tenancy.md](https://github.com/vislake/speed/blob/main/docs/internal/04-data-and-tenancy.md)
