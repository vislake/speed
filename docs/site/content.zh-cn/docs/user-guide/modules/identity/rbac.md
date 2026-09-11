---
title: rbac
weight: 2
description: "基于角色的访问控制——Subject 能否对某资源执行某动作,以及能在组织树的哪一片里执行:默认拒绝、精确的 resource:action 授权、事件失效的决策缓存。"
---

# rbac

rbac 是 speed 的基于角色访问控制引擎:决定一个 subject 能否对某资源
执行某动作,以及能在租户组织树的哪一片里执行。它的定义性属性是它
**不**依赖什么——它永不 import `authn`、`org` 或 `config`。授权对身
份只知一件事:`Subject{TenantID, UserID}`,由认证请求的一方组装。

## 它做什么

三张租户级表——角色、每个角色授予的权限、把角色给到用户的绑定——
外加一份**冻结的内存目录**:宿主各模块声明的每个 `resource:action`
串。求值是 subject → 绑定 → 角色 → 权限,绑定限定到组织子树时再用
物化路径前缀测试收窄。

- **默认拒绝。** 没有匹配授权就是 `(false, nil)`——一次拒绝,不是错
  误。匹配精确:没有通配符,`notes:read` 不蕴含 `notes:write`。
  *未声明*的权限在检查时拒绝、在授予时被拒(`ErrUnknownPermission`)。
- **两个问题,刻意分开。** `Can` 回答粗门——「在自己租户内任意处持
  有」。`DataScope` 回答行过滤——「树的哪一片」——返回租户行的
  handler 必须套用它。解析不到的节点限定只收窄、绝不放宽:`Can` 可
  能仍为真,而 `DataScope` 拒绝。
- **节点路径现解析,从不存储。** 绑定只存节点 id;宿主
  `SubtreeResolver` 在决策时解析路径,成员一移动范围立刻跟着变。
- **决策按 subject 缓存**,由模块自己的事件失效,一个副本上的吊销经
  bus 让其他副本收敛;防丢 TTL(默认 30 秒)给丢失事件兜底。
- **内置角色**——`owner`(一切已声明权限)、`admin`(除
  `rbac:manage` 外一切)、`member`(无)——权限集取自冻结目录,绝不
  是字面清单;`EnsureBuiltinRoles` 启动时按租户调和。

它原生实现于 `dbkit` 之上而非 Casbin——一项已记录的偏离:`casbin_rule`
没有 `tenant_id` 列,会让最要害的安全表一次脱离全部三层隔离。它不是
认证、组织树或消息,也不挂任何 HTTP 路由(`Module.OpenAPISpec()` 返
回 nil):角色管理由运维控制台表面承接;它也不给自己的写操作授权——
`rbac:read`/`rbac:manage` 是调用方用来门控角色管理的词汇。

## 何时选用

当你的模块声明权限、路由必须门控在权限上时:每次操作的粗粒度检查、
每份列表的「哪些行他看得见」过滤、`"system"` 伪租户上的平台级授权
——它对每一层都是一个普通租户 id,绝不是进入客户数据的通配符。没有
组织树的产品照样租户全域使用:`SubtreeResolver` 可选,缺了它只会拒绝
节点限定的授权,不会放宽。只有什么都不门控的产品可以不用它。

## 接线与最少使用

rbac 分两相,因为模块按 bootstrap 顺序注册:`Register` 期间拍的权限快
照会是残缺的。

```go
rbacMod := rbac.NewModule(db) // 选项:WithSubtreeResolver、WithCacheTTL、WithQueue
// rbacMod 加入组合选出的组件集;Attach 恰好一次,在装配的 Init 窗口内:
az, err := rbacMod.Attach(reg) // 冻结每个模块声明的权限
if err != nil {
    return err // 第二次 Attach 失败:ErrAlreadyAttached
}
```

返回的 `*Service` 实现每个消费方都对着编程的 `Authorizer` 接口。先按
租户播种内置角色(`EnsureBuiltinRoles(ctx)`,幂等),再门控路由:

```go
mux.Handle("/api/v1/notes",
    rbac.RequirePermission(az, "notes:read")(notesHandler),
)
```

`RequirePermission`(以及给按请求派生权限的 `RequirePermissionFunc`)是
中间件链上认证之后的那道门。它失败关闭且不可区分:没有可用 subject、
解析不了的权限串、以及普通拒绝都答 `403 rbac.permission_denied`;只有
一次没能*执行*的检查不同(`500 rbac.storage_error`)。换一种方式认证
的宿主经门的 `WithSubjectResolver` 选项按请求供给 subject。授权是服务
调用:`AssignRole(ctx, sub, role, scope)` 幂等,`RevokeRole` 严格,空
`Scope` 即租户全域。

挂载的模块路由用一张表做一次性声明:`rbac.GuardRoutes` 接收宿主模块已挂载的
路由,外加每条路由一条 `rbac.RouteRule`——要么公开,要么点名它要求的权限,并可选
按路由指定 subject 解析器、豁免(处理器自行把关的请求)与门内层——表未点名的路由
拒绝对外服务,后加的路由不可能无决定地上线。`rbac.SplitPermission`(门自己的切分
器,在最后一个分隔符处切开,三段式 `integration:apikey:read` 因此可用)与
`rbac.WriteAuthzError`(门的拒绝信封)已导出,供自行组门控胶水的宿主使用。

## 核心概念与 API 要点

- **每个方法都显式收 `Subject`**——绝不从上下文取——读取用 subject
  自己的租户,上下文与令牌不一致也拐不动查找。
- **授权生命周期。** `DefineRole` 创建自定义角色(只创建);
  `RestoreRole` 撤销一次吊销的标记删除——绑定实现
  `dbkit.SoftDeletable`,部分唯一索引让已吊销的范围可复用。
- **决策缓存**以 `(tenant, user)` 键控,存的是已经过角色压平的授
  权;assign/revoke/restore 事件只丢一个受影响的 subject,角色变更丢
  整个租户。
- **范围来自树,却不 import 它。** `org` 移除成员或删除节点时绑定被
  reap,org 恢复它们时绑定复职——事件只按名字订阅、从不声明;宿主有
  queue 时经 `WithQueue` 入队。
- **每笔角色管理写都有审计**——`rbac.role.define`/`assign`/`revoke`,
  带执行操作者;模拟身份下带双身份对。事件流不是审计记录。

## 边界与注意

- 别 import `authn` 或 `org` 去打听用户或节点的事实——模块接口的存在正
  是为了让引擎能被任何认证方式不同的宿主复用。
- 没有通配符语法、没有角色继承角色、角色创建后不可编辑;缓存 TTL
  是选项,刻意不是动态配置项。
- 吊销在本副本立即生效,经事件让其他副本收敛;TTL 只给丢失事件兜
  底。
- 结构化错误码:见[错误码表(English)](/docs/user-guide/error-codes/#rbac)。

## Source

- [go/rbac/AGENTS.md](https://github.com/vislake/speed/blob/main/go/rbac/AGENTS.md)——权威文档(决策面、缓存、模块、规则)
- 相关:域指南[身份与访问](/zh-cn/docs/user-guide/domains/identity-access/),以及本组页面[authn](/zh-cn/docs/user-guide/modules/identity/authn/)与[org](/zh-cn/docs/user-guide/modules/identity/org/)
