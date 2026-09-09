---
title: billing-ui
weight: 12
description: "账单单据只读面——为什么家族只渲染 billing 模块提供的读操作、为什么单一最新优先窗口是刻意的、为什么单据按 UTC 日历渲染、为什么行展开成一次全新的单文档读取。"
---

# billing-ui

`@speed/billing-ui` 是基于 speed 的前端的账单单据组件族:一个区块
`InvoicesSection`,经生成的 billing 读操作渲染调用方租户的发票——
最新优先,每行可展开进单张单据的明细。它是
[billing 模块](/zh-cn/docs/developer-docs/modules/capabilities/billing/)渠道无关
Invoice 模型的读面。[用户指南的 billing-ui
页](/zh-cn/docs/user-guide/modules/web/billing-ui/)讲怎么用;本页讲
为什么。

## 职责与边界

区块刻意是区块而非路由页:拥有它的页面、其上的标题与包外的一切表
面(订阅管理、支付)都是宿主内容。边界与同层家族同一纪律:

- **读面就是全部表面。** billing HTTP 片段只提供两个读操作——列
  表与单读,无写——家族因此只渲染模块确实提供的读面。这里不清
  算、不作废、不创建发票,也没有 mutation 后失效查询这回事:别处
  变了状态的单据经宿主自己的重取策略收敛。
- **无 prop、无处有租户值。** billing 操作不带租户概念:读取返回
  谁的发票由调用方的访问令牌决定,所以区块零 prop,租户绝不是
  prop 或请求头。
- **没有自己的会话层。** store 装着宿主登录流程预先种下的 bearer
  token;有会话层的宿主经客户端的 `refreshAccessToken` 接缝接入刷
  新。被拒的读原样浮出自己的码。
- **只用生成 hooks。** 读走生成进 `@speed/api-sdk` 的 react-query
  hooks,经宿主的 QueryClient——本包宿主在主题树之外多供的唯
  provider。区块从不自建客户端、从不手写 query key。

## 为什么这样成形

**单一最新优先窗口,刻意不分页。** 区块读最新五十张单据——冻结在
spec 1..100 上限之内,尺寸按"不用滚动机制渲染近期单据"选。spec
不提供 keyset 游标,整段历史的分页读不属于这个表面。

**行展开成一次全新读取,因为列表是快照。** 行与单据是同一个
`BillingInvoice` 形态,但展开一行会经 `billing_getInvoice` 重读单
张单据、渲染 get 自己的新鲜回答:列表取出后才落定的状态在单据里显
示现值,而折叠的行保留列表快照;单据还渲染行不渲染的字段——两个周
期边界、最后更新时间、两个 id。明细是有名区域,`aria-expanded`/
`aria-controls` 接线齐备,加载有播报,被拒的读渲染成带重试的码级横
幅。

**单据按服务端签发所用的 UTC 日历渲染。** 模型存 UTC 时刻,7 月
31 日签发的发票对任何时区的观看者都还是 7 月 31 日——单据日期对
每个观看者是同一个日期,这正是账单单据需要的性质,本地时区读法会
打破它。金额、日期与周期经 `Intl` 以表面当前语言渲染,绝不用手工
格式化;周期边界落在同一日历月内读作该月,跨月则渲染完整日期范
围;解析不了的周期回退到单据 id——它永远为真——绝不进入 `Intl`。

**状态词汇是 spec 的闭集。** 三个生命周期状态(open、paid、void)
渲染为标签即状态自身文案的 chip——颜色绝不单独承载含义——集合外
的状态值,回答里已经带上的未来状态,不渲染 chip、绝不显示裸值。

**加载、空态与错态尊重标题层级。** 未落定的加载——首次加载在飞,
或被 react-query 的离线模式停住——保持加载分支、含标题;只有落定
的查询才离开它。真正零发票的回答隐藏区块标题、渲染 `ui-kit`
`EmptyState` 空态;失败的加载渲染带重试的错态。每个落定态里
`EmptyState` 标题在隐藏的 `h2` 自己的层级上顶替它,标题层级绝不跳
级。

## 错误文案:可达码白名单

白名单有九个码:`billing.invoice_not_found`(单读自己的 404——一
个指不到调用方租户任何发票的 id)、受保护读在调用方会话中途死亡时
会答的五个会话生命周期码、三个 `client.*` 传输码。billing 400 对
按构造不可达——列表 hook 永远发冻结的范围内 limit——500 信封、未
来的 billing 码、`client.http.<status>` 回答与非 `ApiError` 抛出
一律渲染 `errors.unknown` 兜底,绝不显示裸 key。本包与登录家族共
享文案的每个码都是 auth-ui bundle 文案的逐字复制——同一服务端回
答在每个表面读起来相同。

## 对外稳定面

`InvoicesSection`(零 prop)、双语
`BILLING_UI_NAMESPACE`/`billingUiResources` 对,以及它消费的两个
生成 hooks。依赖是生成面(`@speed/api-sdk`,运行时依赖)、i18n 与
ui-kit 的 `EmptyState`;`@speed/api-client` 留在 devDependencies,
只有测试 rig 绑定。usage example——两个请求按序钉死、逐请求断言
authorization 头——是读面的 in-form 消费证明;尚无工作区消费壳渲
染本家族,因此该面只在包级被证明、没有浏览器加真实服务器腿,照实
记录。

## Source

- [billing 模块设计](/zh-cn/docs/developer-docs/modules/capabilities/billing/)——表面背后的 Invoice 模型与只读片段

## 相关页

- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——包分层与包如何对应后端模块
- [account-ui 设计](/zh-cn/docs/developer-docs/modules/web/account-ui/)——确立生成 hooks 层的同层家族;[billing 设计](/zh-cn/docs/developer-docs/modules/capabilities/billing/)——本家族渲染其读面的模块
- 用户指南:[billing-ui 模块](/zh-cn/docs/user-guide/modules/web/billing-ui/)、[billing 模块](/zh-cn/docs/user-guide/modules/capabilities/billing/)、[前端构建域](/zh-cn/docs/user-guide/domains/frontend-building/)
