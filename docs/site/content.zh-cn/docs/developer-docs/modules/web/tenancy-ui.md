---
title: tenancy-ui
weight: 10
description: "租户切换控件——为什么 TenantSwitcher 是一个以 auth-core 会话为 prop 的完全受控组件、上下文变更必须出声且不排队、输掉的切换竞争绝不重发,以及错误文案为何除含义不同处逐字复制 auth-ui。"
---

# tenancy-ui

`@speed/tenancy-ui` 是基于 speed 的前端的租户切换控件:一个受控组
件 `TenantSwitcher`,把宿主的当前租户渲染为触发器、把宿主的租户清
单渲染为菜单,经 `session.switchTenant` 将会话切到所选租户。[用户
指南的 tenancy-ui 页](/zh-cn/docs/user-guide/modules/web/tenancy-ui/)讲怎么用;
本页讲为什么。

## 职责与边界

组件与 `auth-ui` 同层——刻意 auth-aware(驱动以 prop 传入的
`@speed/auth-core` 会话)但完全受控:它不消费 auth-core hooks,除
所驱动的单一操作外不读会话状态,不 attach、不持久化会话,不导航,
不直连网络。它周边的数据边界同样刻意:

- **当前租户是宿主的事实。** `currentTenantId` 是 prop,通常来自
  `useCurrentTenant`;组件存在的意义是改变这个值,并经 `onSwitched`
  上报——提交后恰好触发一次。切换后从不更新 prop 的宿主会看到陈
  旧触发器。
- **租户清单按契约是宿主数据。** 没有任何端点回答"这个 principal
  能在哪些租户之间切换",所以清单——以及每条目名字,原样渲染、无
  人翻译——都是宿主的。
- **提交后的一切都是宿主的**:重取该租户数据、清掉上一租户的查询
  缓存、重挂 `/me` 派生的权限清单。切换本身是会话操作——租户在请
  求体里,绝不在请求头;提交后的上下文活在新铸的访问令牌里。

## 为何这样成形

**上下文变更必须出声。** 切换租户会悄悄改变宿主显示的行,所以控件
经 `role="status"` 实时区自我播报:切换在飞时触发器惰性化——
`aria-disabled`,绝不用原生 `disabled` 属性,因为菜单在飞行开始时
就关回触发器上,MUI 在关闭时还给触发器的焦点只有触发器仍可聚焦才
落得回去——同时一条通知点出目的地;提交后同一实时区变成确认文案,
点出会话现在运行于哪个租户,一直留到下一次切换开始(无自动消失计
时器,寿命不与读屏器的处理竞速)。失败的切换在一个 `role="alert"`
横幅渲染码文本,本地什么都不改:store 保住令牌、触发器留在同一租
户可用,下一次点选重试。

**当前行永远不能再次触发切换。** 它渲染为禁用——辅助技术会播报并
跳过——点击守卫在会话操作可能开始前返回,连合成点击都改不了任何
东西。而且一次只切一个:飞行中触发器惰性,控件绝不把第二次切换排
在第一次后面。

**输掉的竞争就此输掉——且绝不重发。** 两个切换器实例(顶栏加抽屉
副本)竞争把 `switchTenant` 切向不同租户时,服务端两边都成功;
auth-core 拒绝在兄弟提交之后才落定的那个回答——超越由回答落定顺
序决定,绝不是发送顺序。被超越的调用是输掉的竞争而非失败:不渲染
任何东西、不触发 `onSwitched`——赢家操作的提交自己恰好触发了一次,
为会话真正运行于的那个租户。重发输掉的请求会积极犯错:先发的请求
最后落定时,重发等于重新提交用户已放弃的租户。收敛靠宿主的
`currentTenantId`,或在已记录的"按发送顺序落定"残余情形里靠下一
次静默刷新——它按服务端存储的当前租户铸令牌。

## 错误文案:可达码白名单

切换表面只给确实画得出的码配文案——十三个:切换端点自己的三个回
答(成员关系被拒;成员关系无法建立,未接线的 `MembershipReader`
失败即拒;账号状态 `authn.invalid_credentials`)、切换因带着认证
而画得出的两个令牌验证回答、五个会话生命周期码、三个 `client.*`
传输码。其余一律渲染 `errors.unknown` 兜底,裸码绝不上屏。

错误文案有一条有理由的复制纪律:切换回答与登录回答含义相同的码,
文案逐字复制 auth-ui——同层包不能互 import 对方的目录,同一服务
端码的两个版本不得在产品里漂移——套件把 auth-ui bundle 当测试数
据导入、双向钉死配对。三个码在此创作而非复制:
`authn.invalid_credentials`(切换表面的含义"账号不活跃"不是登录
表面的"密码错误")与两个令牌验证码——pre-auth 登录表面不可能被
它们回答,auth-ui 因此没有它们的文案。

## 对外稳定面

一个组件四个 prop(`session`、`tenants`、`currentTenantId`、
`onSwitched`)、`TenantOption` 形态与双语
`TENANCY_UI_NAMESPACE`/`tenancyUiResources` 对。依赖面是 auth-aware
层里最小的——`@speed/auth-core`(会话类型与超越守卫)加
`@speed/i18n`;`ui-kit` 的主题 provider 只出现在测试树里。usage
example 是 `authn_switchTenant` 操作的 in-form 消费证明:一次登录
加三次切换尝试跑在真实 `@speed/api-client` 上,按序钉死,逐次断言
bearer token 与 `{tenant_id}` body。

## 相关页

- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——包分层与"无租户请求头"规则(它让切换成为令牌操作)
- [auth-ui 设计](/zh-cn/docs/developer-docs/modules/web/auth-ui/)——先于本组件的登录面的同层家族;[product-shell 设计](/zh-cn/docs/developer-docs/modules/web/product-shell/)——`userMenu` 槽承载切换器的框架
- 用户指南:[tenancy-ui 模块](/zh-cn/docs/user-guide/modules/web/tenancy-ui/)、[authn 模块](/zh-cn/docs/user-guide/modules/identity/authn/)、[前端构建域](/zh-cn/docs/user-guide/domains/frontend-building/)
