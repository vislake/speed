---
title: 多租户与组织
weight: 2
description: 一个请求如何获得它的租户,以及如何用 org 模块塑造你产品的组织树、成员与邀请。
---

# 多租户与组织

speed 产品里的每个请求都以*某个*租户的身份运行——这就是隔离模型,
它有三层防护(GORM 插件自动注入租户过滤、强制的 `dbkit.Repository[T]`
基座、分布式部署下的 PostgreSQL 行级安全)。`tenancy` 模块解析一个
请求属于哪个租户;`org` 模块给每个租户提供组织树、绑在树上的成员,
以及创建成员的邀请。

```mermaid
flowchart TD
    Req[进入的请求] --> Res[tenancy 解析器\n自定义域名,再子域名,\n最后平台默认]
    Res -->|租户 id| MW[tenancy.Middleware]
    MW -->|租户上下文| H[你的处理器]
    H --> R[dbkit.Repository[T] 查询\n过滤到 ctx 租户]
    Org[org 模块] -->|节点、成员、邀请| ODB[(租户级行)]
    MW -.->|白名单预认证路径跳过| Pub[登录页、公共配置]
```

解析器是宿主提供的函数:`tenancy.NewDomainResolver` 按主机把请求映
射到租户;参考应用先自定义域名、再子域名、最后平台默认——解析失败
对登录页端点仍以 200 提供平台默认值,绝不出错。请求的租户从哪来是
框架的事:你的处理器用 `pkgcore.TenantFromContext(ctx)` 从上下文里
读,绝不接受来自头、参数或请求体的租户。

## 最少集成步骤

1. **挂中间件。** `tenancy.Middleware(resolver, opts...)` 包住你的
   mux;预认证路径(登录页、`/api/config/public`)进白名单,其余一切
   在租户解析失败时失败关闭。
2. **排在认证之后。** 组合链里租户层在 `authn.Middleware` 下游,把
   验证过的 principal 变成租户上下文——完整顺序见身份与访问领域页。
3. **接 org 模块。** `org.NewModule(db, opts...)` 启动时需要两个必选
   接线(邮箱盲索引器与邀请链接构造器;不发邮件的宿主可用
   `WithInvitationEmailDisabled`);`Tree()`、`Members()`、
   `Invitations()` 是三个运行时。
4. **塑形组织树。** 在父节点下创建节点;每个节点带物化路径与深度。
   一次移动操作更新全部后代的路径——`org.node.moved` 的订阅者
   (rbac 的子树授权也在其中)靠事件收敛。
5. **用邀请加人。** 邀请是租户自己的流程:原始令牌从不落库(只存
   哈希),受邀者地址在盲索引下加密存储,投递按租户与按收件人双重
   限流。接受邀请即创建成员;成员可以按子树列出或移除。
6. **在自己的模块里证明隔离。** 每个租户数据仓库都必须跑
   `tenancytest.AssertIsolated`——正是会抓住缺失过滤器的套件。身份
   与平台表改跑 `AssertNotTenantScoped`。

## 值得知道的边界

- `users` 刻意**不**租户级:一个人可以属于多个租户;`memberships`
  是链接表。设计任何表之前,先把它归入四个数据域之一(租户/身份/
  平台/链接)。
- 唯一合法的跨租户通道是带审计的 `WithSystemContext` 逃生门,其使用
  被限制在平台自身的扩权目的(admin、compliance、jobs、authn)——
  业务代码绝不扩权。
- 软删节点不会挡住被删同级节点的名字:唯一索引是部分索引
  (`WHERE deleted_at IS NULL`),所以被删兄弟的名字与被移除成员的
  席位都可以复用。

## 下一步

完整 API 见模块参考中的 `tenancy` 与 `org` 页面;数据与配置领域页
讲租户级模型怎么声明与迁移。

## Source

- [tenancy AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md)
- [org AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md)
- [dbkit AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md)
