---
title: tenancy-ui
weight: 10
description: "租户切换控件——一个以 auth-core 会话驱动的受控 TenantSwitcher,渲染宿主的当前租户与租户列表,并经会话提交切换。"
---

# tenancy-ui

`@speed/tenancy-ui` 是基于 speed 的前端的租户切换控件:一个受控组
件 `TenantSwitcher`,把宿主的当前租户渲染成触发器、把宿主的租户列表
渲染成菜单,并经 `session.switchTenant` 把会话切到所选租户——租户放
在切换请求体里,绝不在请求头,因为租户上下文乘在访问令牌内。

## 它做什么

包交付 `TenantSwitcher`(连同 `TenantSwitcherProps` 与 `TenantOption`
形状)以及 `TENANCY_UI_NAMESPACE` / `tenancyUiResources` 资源对。它
与 `@speed/ui-kit`、`@speed/auth-ui` 同层——刻意感知认证(驱动以
prop 传入的 `@speed/auth-core` 会话)但完全受控:

- **它从不消费 auth-core hooks,除所驱动的那个操作外从不读会话状
  态,从不挂接或持久化会话,从不导航,从不直接碰网络** —— 每个请求
  都是会话自己的生成切换操作,经宿主绑定的客户端发出。
- **租户列表是宿主数据。** 已登录用户能在哪些租户之间切换是宿主才
  知道的事(已交付表面没有名册端点),名字原样渲染,无人翻译。
- **成功的切换被宣告,绝不沉默**:它恰好触发一次 `onSwitched`(在提
  交之后),并经 `role="status"` 活动区宣告自己——一个会悄然改变宿
  主所显示行集合的语境变化必须说出口。

提交之后的一切都是宿主的:取回新租户的数据、移除前一租户的查询缓
存、重新挂接 `/me` 派生的权限列表(切换提交会按 auth-core 自己的生
存规则丢弃租户域权限集)。

## 何时选用

一个租户面向前端,其单个登录用户可属于多个租户且需要在它们之间切
换——典型在应用外框里,[product-shell](/zh-cn/docs/user-guide/modules/web/product-shell/) 的
`userMenu` 槽就是自然归宿。切换器绝不是独立表面:它假定宿主的登录
已运行、会话持有可供切换 *离开* 的当前租户。

## 接线

```tsx
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))
attachSession(session)
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, TENANCY_UI_NAMESPACE, tenancyUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources) // 宿主外框

const tenants: TenantOption[] = [
  { id: 'tenant-1', name: 'Sunshine Dental' },
  // ...宿主自己的名册
]
// 宿主门的已认证分支:
//   const currentTenant = useCurrentTenant()
//   <TenantSwitcher session={session} tenants={tenants}
//     currentTenantId={currentTenant?.tenantId ?? null}
//     onSwitched={(tenantId) => { /* 宿主的切换后数据移动 */ }} />
```

`useCurrentTenant` 读取已提交的快照,成功的切换因此把触发器重新渲染
到新租户上。

## 核心概念与 API 要点

`TenantSwitcher` props:`session`(必填)、`tenants`
(`readonly TenantOption[]`,必填)、`currentTenantId`(`string | null`,
必填——宿主的事实,典型来自 `useCurrentTenant()`)、`onSwitched`
(可选;每个已提交切换恰好触发一次,失败从不触发)。值得知道的行
为:

- **触发器显示当前租户** —— 匹配的 `tenants` 行的 `name`;没有匹配
  或 `currentTenantId` 为 null 时触发器禁用并显示
  `tenantSwitcher.noCurrentTenant` 文本。菜单里的当前行禁用,永不可
  能再次触发切换。
- **在途时触发器惰性但可聚焦** —— `aria-disabled` 加拒绝打开的处
  理器,绝不用原生 `disabled` 属性,那会把菜单关闭时的焦点恢复困在
  `document.body` 上。一条 `role="status"` 通知点名目的地
  (`tenantSwitcher.switchingTo`),随后在同一活动区变成 `switchedTo`
  确认,保持到下一次切换开始。
- **被拒的切换**把应答的码文本渲染进一个 `role="alert"` 横幅,本地
  什么都不变:store 保留令牌,触发器停在原租户上可用,下一次点选重
  试。
- **在同一会话上输掉竞争的切换保持输掉。** 两个实例竞争
  `switchTenant` 到不同租户,服务端两者都成功;auth-core 以
  `OperationSupersededError` 拒绝在兄弟提交之后才落定的那个应答。被
  取代的调用是输掉的竞争,不是失败:什么都不渲染、不触发
  `onSwitched`,触发器经宿主的 `currentTenantId` 收敛。重发输掉的请
  求会重新提交用户已经放弃的租户。
- **错误文案只覆盖可达应答** —— 切换端点的三个应答
  (`authn.tenant_membership_required`、`authn.tenant_membership_unavailable`、
  账号状态的 `authn.invalid_credentials`)、令牌校验应答
  (`authn.authentication_required`、`authn.token_invalid`)、会话生命
  周期族与三个 `client.*` 传输码;其余一切渲染 `errors.unknown` 回
  退。切换应答与登录应答含义相同时,文案逐字复制 auth-ui 包的——
  同层包不能互导目录。

## 边界与注意

- **当前租户是宿主的事实** —— 组件显示 `currentTenantId` 的行并上
  报变化;切换后从不更新 prop 的宿主会看到陈旧的触发器。`useCurrentTenant`
  流程是预期形状。
- **权限集与查询缓存由宿主搬动** —— `onSwitched` 是宿主移除前一租
  户查询缓存并重新挂接权限列表的地方;这里两者都不做。
- **触发器标签与列表条目是宿主文本** —— 内置文案只覆盖组件自己的
  状态。
- **没有包内租户名册** —— 列表按契约是宿主数据。
- **会话状态活不过页面加载** —— auth-core 的既有局限:重载即匿名。

## 相关页面

- 前端分层:[搭建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
- 后端表面:[authn](/zh-cn/docs/user-guide/modules/identity/authn/) 模块页(切换操作住在那里);错误码见[错误码索引(English)](/docs/user-guide/error-codes/#authn)
- 相关包页:[product-shell](/zh-cn/docs/user-guide/modules/web/product-shell/)(本组件的 `userMenu` 之家)、[auth-ui](/zh-cn/docs/user-guide/modules/web/auth-ui/)
- 同级包 `@speed/auth-core`、`@speed/i18n` 与 `@speed/ui-kit` 各在本组的页面
