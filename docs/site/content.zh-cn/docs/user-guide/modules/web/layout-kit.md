---
title: "@speed/layout-kit"
weight: 4
description: "共享应用骨架层——AppShell(响应式页头、导航抽屉与 main 内容区)与 RouteGuard(由宿主注入的 status 驱动的内容门),两者都受控、与认证无关,与 @speed/ui-kit 同层。"
---

# @speed/layout-kit

给任何建在 speed 上的项目用的共享应用骨架:`AppShell`——响应式页
头、导航抽屉与内容区——以及 `RouteGuard`,一道完全由宿主注入的
status 驱动的路由/内容门。两个组件都受控、都靠 props 驱动,与
`@speed/ui-kit` 同层:这里没有任何业务、租户或认证机制语义。
`RouteGuard` 尤其如此——放行/拒绝/等待的决定以普通 `status` 值进
来,不是它去调的回调,更不是某个具体认证或路由包的 import——所以
面向租户的外壳与面向平台运维的控制台可以共用这一个包,各自接不同
的授权来源。

## 它做什么

- **`AppShell`**——应用骨架:固定 `AppBar`(`header`/banner
  landmark)、导航 `Drawer`(带标签的 `nav` landmark——`md` 及以上
  常驻,以下临时浮层,读环境主题自己的断点,不引入新断点)、`main`
  内容 landmark。一个视觉隐藏的跳到内容链接是骨架里第一个可聚焦元
  素,直接聚焦 `main`(绝不用片段跳转——那会改写 hash 路由宿主当前
  的 route)。
- **`RouteGuard`**——按宿主算好的 `RouteGuardStatus` 门控
  `children`:`'allowed'` 渲染子内容,`'pending'` 渲染
  `pendingFallback` 或默认的带标签转圈,`'denied'` 渲染
  `deniedFallback` 或 ui-kit 的 `EmptyState variant="noPermission"`
  ——本包唯一一处具体的 ui-kit 耦合。三种状态按构造互斥——不会有
  一个单独的 loading 旗标跟 allowed 打架。`onDenied` 在每次*进入*
  `'denied'` 时恰好触发一次——给宿主路由重定向或遥测用的回调,与
  渲染解耦。

不是路由器、不是认证门、不是导航系统。AppShell 不做任何路径匹配:
`navItems` 是宿主全量算好的 `readonly AppShellNavItem[]`,哪一项
`selected` 也由宿主算。

## 何时选用

需要标准骨架——页头、可收合的导航抽屉、内容区——以及一道在已登
录界面与调用者可见范围之间的门,又不想把骨架绑死在自己的路由器或
授权来源上时。会话族包不决定骨架的任何事,骨架也不决定会话的任何
事。`@speed/product-shell` 包把 `AppShell` 当自己的登录后骨架来组
合,并以宿主算出的 status 演练 `RouteGuard` 的形状。

## 接线与最少使用

```tsx
import { createI18n, I18nextProvider, registerNamespace } from '@speed/i18n'
import { AppThemeProvider, UI_KIT_NAMESPACE, uiKitResources } from '@speed/ui-kit'
import {
  AppShell,
  LAYOUT_KIT_NAMESPACE,
  layoutKitResources,
  RouteGuard,
  type RouteGuardStatus,
} from '@speed/layout-kit'
import { useState } from 'react'

const i18n = createI18n()
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
registerNamespace(i18n, LAYOUT_KIT_NAMESPACE, layoutKitResources)

function AppContent() {
  // 这个值由宿主真实的授权来源算出来——
  // RouteGuard 从不 import 任何认证包。
  const [status] = useState<RouteGuardStatus>('allowed')
  return (
    <AppShell
      navItems={[{ id: 'home', label: 'Home', href: '/', selected: true }]}
      header="My App"
    >
      <RouteGuard status={status}>{/* 受门控的页面内容 */}</RouteGuard>
    </AppShell>
  )
}
```

`ui-kit` 的命名空间必须与 `layout-kit` 的一起注册——默认的拒绝回退
复用它的 `emptyState.noPermission.*` 资源束。注册每个 i18n 实例只
跑一次,在启动时。

## 核心 API 与使用要点

- **`AppShell` 的 props**——`navItems`(必填;每项
  `{ id, label, icon?, href?, onClick?, selected? }`,有 `href` 渲
  染成链接,`onClick` 无论有无 `href` 都触发)、`header`、
  `headerActions`、`userMenu` 是 AppBar 的槽,传什么渲染什么(没传
  的槽渲染为空,绝无占位)、`children` 渲染在 `main` landmark 里、
  `sidebarWidth` 默认 280px、`mobileOpen`/`onMobileOpenChange` 是移
  动端抽屉的可选受控对——两个都不传时由 AppShell 自己管开关,这是
  全族唯一一处交互局部例外。
- **窄视口防护纯 CSS**——移动端抽屉纸面宽封顶
  `min(sidebarWidth, 85vw)`,AppBar 行在真实溢出压力下换行,让内容
  避开固定页头的三个占位块的高度来自实测页头(ResizeObserver)。没
  有 props 变化。
- **`RouteGuard` 的 props**——`status`、`children`、
  `pendingFallback`、`deniedFallback`、`headingLevel`、`onDenied`。
  `headingLevel`(ui-kit 的 `h1`..`h6` 并集,默认 `'h1'`)只作用于
  默认拒绝组合:门替换了它守护的页面内容,所以回退就是页面自己的
  标题——页面标题在门之前的宿主,传能续上页面顺序的级别(usage
  example 在自己的 `h1` 下面以 `headingLevel="h2"` 接门)。
- **内置文案**来自 `layout-kit` 命名空间:`appShell.skipToContent`、
  `appShell.navLabel`、`appShell.openNav`/`appShell.closeNav` 与
  `routeGuard.pending`——拒绝回退按设计不带自己的文案。想改措辞
  的宿主在启动时以 `LAYOUT_KIT_NAMESPACE` 注册自己键结构一致的双
  语资源对——绝不改组件文本。

## 边界与注意

- **没有认证、没有路由、没有数据。**本包只依赖 `@speed/i18n` 与
  `@speed/ui-kit`——任何地方都不出现 `auth-core`、`api-client`、
  `api-sdk` 或具体认证/路由包,也不直接依赖 `@speed/tokens`:
  `AppShell` 经宿主 `AppThemeProvider` 已经建好的环境 MUI 主题读断
  点与 z-index。
- **status 归宿主算。**别指望 RouteGuard 去取权限或认得会话;从你
  自己的授权来源推导 `status`(一次权限拉取,或一个由服务端作答的
  查询——被拒的读取失败关闭成 `denied`)。
- 别给没有 `href` 的导航项指望导航——没有 `href` 的项除了自己的
  `onClick` 外是惰性的。
- 移动端抽屉在 `md` 以下是浮层,不是推入式;需要其它响应式形状的
  宿主自己组装骨架。

## Source

- [web/packages/layout-kit/AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/layout-kit/AGENTS.md)——包规则与记录在案的决定
- 相关:它踩着的主题与 `EmptyState` 在 [ui-kit](/zh-cn/docs/user-guide/modules/web/ui-kit/);两者渲染所经的 i18n 实例在 [i18n](/zh-cn/docs/user-guide/modules/web/i18n/);领域叙事见[构建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
