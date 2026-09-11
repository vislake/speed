---
title: billing-ui
weight: 12
description: "账单文档只读面——一个 InvoicesSection 渲染调用方租户的发票(最新在前),每行可展开进单张文档的详情。"
---

# billing-ui

`@speed/billing-ui` 是基于 speed 的前端的账单文档组件家族:后端
[billing](/zh-cn/docs/user-guide/modules/capabilities/billing/) 模块与
渠道无关的 `Invoice` 模型的只读面,经生成的账单读操作渲染。一个区块
——`InvoicesSection`——组成宿主的账单页:调用方租户的发票最新在
前,每行可展开进单张文档的详情。

## 它做什么

包交付 `InvoicesSection` 以及 `BILLING_UI_NAMESPACE` /
`billingUiResources` 资源对。区块刻意是**区块,不是路由页**:拥有它
的页面、它上方的标题、本包之外的一切表面(订阅管理、支付)都是宿主
内容。

层级是 api-sdk 契约的生成 hooks 层:读经 `@tanstack/react-query`
生成进 `@speed/api-sdk` 的 hooks(`useBillingListInvoices`、
`useBillingGetInvoice`)跑在宿主的 QueryClient 上。账单操作不带租户
概念——读返回谁的发票由调用方的访问令牌决定——因此**区块完全无
prop**,租户值永远不是 prop 或请求头。这里也没有会话 prop、没有会话
操作:表面按规范只读,账单 HTTP 片段恰好交付四个读操作。这里没有任
何东西读存储、导航或直接碰网络;内置文案从双语 `billing-ui` 命名空
间渲染,落定的空/错状态组合 ui-kit 的 `EmptyState`。

## 何时选用

已登录的租户面向前端需要展示自己的账单文档:近期发票的生命周期状
态、每张覆盖什么、金额多少。家族假定宿主的登录先运行、内存 store
持有活令牌,读按其租户解析。片段没有写操作,必须结算或作废发票的宿
主在这里没有可用的表面。

## 接线

```tsx
const store = createMemoryAccessTokenStore()  // 登录种下的令牌
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  // refreshAccessToken: () => session.refresh()  // 有会话层的宿主传入自己的
}))
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, BILLING_UI_NAMESPACE, billingUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 0 } } })
// <QueryClientProvider client={queryClient}> 包住账单页:
//   <main><h1>Billing</h1><InvoicesSection /></main>
```

react-query 的重试与缓存策略是宿主自己的。套件编译并运行这个组
合——真实客户端上按序钉住两个请求并断言授权头,脚本化 fetch 应答真
正的 `Response` 对象——文档化的用法因此不会偏离 API。

## 核心概念与 API 要点

`InvoicesSection` 渲染服务端应答的最新一页,页大小固定在 50(在规范
的 1..100 上限内)且从不分页:账单读面只服务一个最新在前的窗口,遍
历全部历史的游标不是规范的一部分。行显示:

- **生命周期封闭词汇里的状态** —— open(待支付)、paid(已结清)、
  void(支付前作废)——以徽章呈现,标签即状态自己的文案;颜色绝不单
  独承载含义,集合之外的状态值不渲染徽章,绝不渲染原始值。
- **账单周期** —— 落在一个自然月内的周期读作该月("July 2026");
  跨月的边界渲染为完整日期区间。
- **以自身货币计的金额与开票日期** —— 以表面当前语言经 `Intl` 格
  式化,绝不手写格式;渲染在服务端开票所依据的 UTC 历法里,因此文档
  日期对每个观看者都是同一天。

行可展开:行尾控件挂载一个命名详情区,**重新读取单张文档**
(`useBillingGetInvoice`)并渲染新鲜应答——状态、金额、两个周期边
界、开票与最后更新时间(仅当行在开票后被触碰过)以及两个 id。列表
行保持列表查询的快照,文档则是 get 自己的应答,因此列表取回后才落
定的状态在文档里显示当前值。详情区宣告自己的加载,把被拒的读渲染
成带重试的码级横幅,随行收起。

未落定的加载(首次加载在途,或因 react-query 默认的
`networkMode: 'online'` 在离线时停驻)保持加载分支,含标题;只有落
定的查询才离开它——零发票隐藏标题并渲染空 `EmptyState`,失败的加
载渲染带重试按钮的错误变体,两者都在 `headingLevel="h2"`,标题层级
因此从不跳级。

错误文案只覆盖可达应答——文档读自己的 404
(`billing.invoice_not_found`)、会话中途死亡时受保护读会应答的五个会
话生命周期码,以及三个 `client.*` 传输码;其余一切(账单 400 对——
构造上不可达,因为列表 hook 总是发送冻结的界内上限;500 信封;未来
的码)渲染 `errors.unknown` 回退。共享的码逐字复制 auth-ui 包的文
案。

## 边界与注意

- **读面就是全部表面** —— 账单片段没有写操作,因此没有东西在 mutation
  后使查询失效;别处变化状态的文档经宿主自己的重新取回策略在列表上
  收敛。
- **一个最新在前窗口,无分页** —— 最新五十张文档,页大小钉在组件
  里,prop 不可配置。
- **文档日期渲染在 UTC 历法里** —— 模型存 UTC 时刻,家族按 UTC 渲
  染;想看自己历法读数的时间戳的观看者在这里找不到。
- **解析失败的周期以发票 id 自称** —— 畸形应答绝不进 `Intl`;周期
  标签回退到文档 id,它总是真的。
- **只有白名单错误文案** —— 想为某个码定制文案的宿主在命名空间下
  注册自己的资源对。

## 相关页面

- 前端分层:[搭建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
- 后端表面:[billing](/zh-cn/docs/user-guide/modules/capabilities/billing/) 模块页与域指南[计费与计量](/zh-cn/docs/user-guide/domains/billing-metering/);错误码见[错误码索引(English)](/docs/user-guide/error-codes/#billing)
- 同级包 `@speed/api-sdk`(生成 hooks)、`@speed/ui-kit` 与 `@speed/i18n` 各在本组的页面
