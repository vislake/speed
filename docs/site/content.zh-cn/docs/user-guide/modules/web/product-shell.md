---
title: product-shell
weight: 11
description: "租户面向的组装壳——ProductShell 把 AppShell 外框、auth-ui 登录家族与 auth-core hooks 组合成一个三分支视图机。"
---

# product-shell

`@speed/product-shell` 是基于 speed 的前端的租户面向组装壳。它把共
享应用外框(`@speed/layout-kit` 的 `AppShell`)、登录家族
([auth-ui](/zh-cn/docs/user-guide/modules/web/auth-ui/))与无头会话
hooks(`@speed/auth-core`)组合成一个可直接复制的租户面向业务应用前
门。同层的平台员工壳尚未构建。

## 它做什么

包导出 `ProductShell` 与其 props 类型,外加 `PRODUCT_SHELL_NAMESPACE`
(字面量 `'product-shell'`)与 `productShellResources` 资源对。
`ProductShell` 从认证快照渲染三个分支之一:

| 快照 | 分支 |
| --- | --- |
| 已认证 | `AppShell` 外框(banner + 导航 drawer + main)包住你的应用 `children` |
| 匿名,应用从未到达 | 宿主的 `signIn` 槽——与 auth-ui 的 `SignInScreen` 配对 |
| 匿名,应用曾到达 | `sessionEnded` 槽;没有则默认 auth-ui 的 `SessionEndedScreen` |

第三个分支正是壳以组件而非每个应用里两行 hooks 存在的理由:曾在应
用内部的登出用户绝不可回退到全新访客的登录面。壳以组件状态、按会话
记住应用曾经到达;会话本身留在 `@speed/auth-core`。

因为壳是整页切换,它也承担页面切换的可访问性职责:每次分支翻转都把
焦点移进分支自己的容器(一个可聚焦、非制表位的包装),每次翻进会话
结束视图都经 `role="status"` 区域宣告自己。那段宣告是壳**自己的唯一
一句文案**——`product-shell` 命名空间里双语的
`announcements.sessionEnded` 键;外框与默认结束屏上的其它文案都来自
宿主反正要注册的 layout-kit 与 auth-ui 命名空间。未注册
`product-shell` 命名空间的宿主仍保有焦点转移,只是不渲染原始键文
本。

## 何时选用

想要标准前门——登录、已认证外框、会话结束回退——而不想在每个应用
里重推视图机的租户面向前端。壳刻意不交付默认登录面:`signIn` 槽由
宿主提供,因为频道组合是产品决策。完全没有账号的应用更适合只用
`AppShell` 外框。

## 接线

五个命名空间、一次会话挂接、一个待填的槽:

```tsx
// 在宿主唯一一个 i18n 实例上各注册恰好一次:
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)            // 外框 chrome
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)    // 外框文案
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)          // 默认结束屏
registerNamespace(i18n, PRODUCT_SHELL_NAMESPACE, productShellResources)
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)    // userMenu 切换器
attachSession(session) // 宿主的 @speed/auth-core 会话,渲染之前

<ProductShell
  navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
  header="My App"
  signIn={<SignInScreen session={session} />}
  userMenu={<UserMenu session={session} />}   // 租户切换器 + 登出,
                                              // 宿主组合(见下)
>
  <MyRoutes />   {/* 已认证应用 */}
</ProductShell>
```

套件编译并运行这个组合——登录、外框、租户切换、登出、默认会话结束
屏,以及回到登录视图——跑在真实客户端上、每个请求都被钉住,快速开
始因此不会偏离 API。

## 核心概念与 API 要点

- **多租户 `userMenu`** —— 壳自己没有租户切换代码。宿主把
  [tenancy-ui](/zh-cn/docs/user-guide/modules/web/tenancy-ui/) 的
  `TenantSwitcher` 组合进 `userMenu` 槽,以 auth-core 的
  `useCurrentTenant` 喂其 `currentTenantId`,旁边是 auth-ui 的
  `SignOutButton`。点选租户驱动 `session.switchTenant(id)`;切换铸造
  一枚访问令牌、没有刷新令牌,中途因此不会出现刷新腿。`onSwitched`
  是宿主搬动租户域状态的时刻:auth-core 的生存规则已丢弃前一租户的
  权限列表,宿主在那里重新挂接 `/me` 派生的列表并丢弃前一租户的查
  询缓存。
- **任何 `AppShell` 外框 prop 都直通** —— `navItems`、`header`、
  `headerActions`、`userMenu` 等是 layout-kit 的表面;`navItems` 必须
  宿主计算好、含 `selected`,因为这里没有任何东西按 URL 做路径匹配。
- **`signIn` 槽实际上必填** —— 没有它,应用之前的匿名分支渲染空白
  页,这是刻意选择。默认结束屏的动作把访客送回 `signIn` 视图,因此
  没有 `signIn` 槽、会话中途死亡的宿主停在结束屏上,不会重置进一个
  渲染空白的分支。
- **`sessionEnded` 槽可选** —— 没有则渲染 auth-ui 的
  `SessionEndedScreen`;自定义节点原样渲染,自己找出路。
- **别与焦点契约作对** —— 每次分支翻转把焦点移进分支自己的容器,
  会话结束翻转宣告自己;传入的内容留在边界之内。

## 边界与注意

- **没有自己的权限门控** —— 壳从不消费 layout-kit 的 `RouteGuard`,
  从不调用 `usePermission`,从不挂接权限列表。路由级授权是
  `children` 里的宿主组合,喂给它宿主从自己挂接(来自 `/me`)并在租
  户切换时重新挂接的列表推导出的状态;列表挂接之前 hooks 失败关闭,
  因此什么都别在客户端门控,一律信赖服务端。
- **没有自己的租户切换器、没有路径匹配、没有网络** —— 切换器只经
  `userMenu` 里的宿主组合出现;导航选中态宿主计算;组合视图发出的每
  个请求都是经宿主绑定客户端的会话操作。
- **不要求路由、状态或查询库** —— `children` 自带。
- **会话状态活不过页面加载** —— auth-core 的既有局限:重载即匿名,
  壳重新显示登录分支(应用曾到达的记忆随页面重置)。

## Source

- 包 README:
  [web/packages/product-shell/README.md](https://github.com/vislake/speed/blob/main/web/packages/product-shell/README.md) —— 权威文档(宿主清单、分支、i18n、测试套件)
- 前端分层:[搭建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
- 相关包页:[auth-ui](/zh-cn/docs/user-guide/modules/web/auth-ui/)(登录家族与默认结束屏)、[tenancy-ui](/zh-cn/docs/user-guide/modules/web/tenancy-ui/)(`userMenu` 切换器)
- 同级包 `@speed/layout-kit`(`AppShell` 外框与 `RouteGuard`)、`@speed/auth-core`(会话 hooks)与 `@speed/ui-kit`(主题)各在本组的页面
