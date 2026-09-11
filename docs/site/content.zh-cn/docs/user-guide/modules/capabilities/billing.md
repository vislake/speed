---
title: billing
description: "商务:Plan/Feature/Entitlement 领域模型、渠道无关的订阅与账单生命周期、信用账本——外加可选的支付网关层与刻意只读的 HTTP 面。"
weight: 1
---

# billing

billing 是 speed 的商务模块:`Plan`/`Feature`/`Grant`/`Entitlements`
领域模型、渠道无关的 `Subscription`/`Invoice` 生命周期,以及信用账
本——订阅之外、按用付费产品使用的第二条并行计费模式。可选的支付
网关层(`PaymentGateway`、一个注册表、三个真实 provider 实现)与刻
意只读的 OpenAPI 片段构成完整表面。

## 它做什么

三半。**领域模型。** 平台级 `Plan` 目录(租户可用自定义行覆盖)、
`FeatureKindBoolean`/`FeatureKindUnlimited`/`FeatureKindQuota` 的授
予,以及 `Entitlements.Check(ctx, featureKey, requested)`——回答「这
个租户的当前订阅是否允许这个」的唯一判定入口:按租户的
`Subscription` 生命周期(`created`/`active`/`past_due`/`canceled`)
由普通 Go 调用驱动,每次变迁发布 `EventSubscriptionStatusChanged`。
**信用账本。** `CreditService.PreDeduct` → `Confirm`/`Refund` 是每个
「按用付费但可能失败」的业务操作都需要的先预留后结算形态;`Grant`
充值;`Expire`(带 key、重试安全)是定时过期清扫要用的至多一次写入;
`Balance`/`Transactions` 读回。账本只增不改,并发安全靠每次变更一
条由数据库仲裁的 `UPDATE`。**支付网关层。** `PaymentGatewayRegistry`
加三个真实 provider(`go/billing/gateway/{stripe,alipay,wechat}`)、
去重的 `PaymentEvent` 账本,以及 `PollingService`——渠道 webhook 迟
迟不来的主动轮询兜底。

它**不是**什么:不是 metering——这里不做用量采集,配额判定经窄小的
`UsageReader` 模块读 `go/metering` 的实时计数器,从不读汇总表(汇总
有聚合延迟)。不挂接收 webhook 的入站 HTTP 端点,因此没有活的
`PaymentEvent` 在驱动 `Subscription` 变迁。不交付定时积分过期——机
制在,清扫本身是产品策略加 `jobs` 宿主的工作。它的 HTTP 片段按决
策只读。

## 何时选用

你的产品收费:订阅、积分包或按用门禁。在付费操作前问
`Entitlements.Check`(参考应用对每次 AI 调用都过这道闸);在可能失
败的工作前 `PreDeduct`,在它结算时 `Confirm`/`Refund`。真要收真实
支付渠道的那天,再加网关层。数字来自用量就配
[metering](/zh-cn/docs/user-guide/modules/services/metering/);要运营
仪表盘就把结果配给 [admin](/zh-cn/docs/user-guide/modules/capabilities/admin/) 的用量汇总端点。

## 怎么接线

```go
b := billing.NewModule(db, nil) // nil UsageReader:配额授予会失败关闭
// 放进你的组合选出的组件集。然后:

// 「这个租户能不能用功能 X」——在付费工作之前:
d, err := b.Entitlements().Check(ctx, "model:chat:default", 1)
if err != nil || !d.Allowed { /* 拒绝 */ }
// d.Remaining == nil 即无上限;reason 词汇表:
// DecisionReasonOK/FeatureDisabled/QuotaExceeded/NoSubscription。

// 按用付费:先预留、后结算:
res, err := b.Credits().PreDeduct(ctx, billing.PreDeductInput{
    Amount:        10,
    IdempotencyKey: "smilesim:" + reqID, // 必填,会成为账本行的 ID
    Reason:        "ai_generation:job_123",
})
if err != nil { /* 余额不足等 */ }
// ... 做付费工作 ...
if _, err = b.Credits().Confirm(ctx, res.ID); err != nil { /* 改走 Refund */ }
```

`UsageReader` 实参在你要判定 quota 类授予时是 `*metering.Aggregator`;
`NewModule(db, nil)` 合法,只是这种授予会答 `billing.usage_reader_unconfigured`
(失败关闭)——Boolean 授予从不问它。网关接线全部可选:
`billing.WithQueue(queue)` 武装轮询兜底,`billing.WithGateways(map)`
或注册表的 `Build`(先 blank-import provider 子包)提供渠道。

## 核心概念与 API 面

- **`Plan` 是双域表,不是 `TenantScoped`。** `tenant_id` 空串哨兵即
  平台级(`go/config` 的答案);`PlanStore.Resolve` 先查租户自定义
  行、回落到平台目录。隔离证明是 `AssertNotTenantScoped`,带作用域
  的 `Get`/`Update` 只够得着点名的作用域。
- **信用账本只增不改。** `CreditTransaction` 用复合主键
  `(id, tenant_id)`——`PreDeduct` 行的 ID *就是* 调用方的幂等
  key——它的仓库只暴露 `Insert` 与读,绝无 `Update`/`Delete`(反射
  测试钉死)。所有变更汇入一条带守卫的 `UPDATE`
  (`applyBalanceDelta`),WHERE 子句保证两个桶都不为负,并发扣减无
  法透支。
- **重试由首跑自己的行作答。** 重试的 `PreDeduct` 在同一笔事务里读
  回自己早先的预留(`ON CONFLICT DO NOTHING`,绝不抛出毒化事务的违
  规——这个分歧只有真实 PostgreSQL 层能显示,这正是 billing 带一
  个的原因)。带 key 的 `Expire` 撞上别类行时答
  `billing.idempotency_key_collision`。
- **五个改状态的积分方法在提交后发出各自声明的审计动作**(`Grant`/
  `PreDeduct`/`Confirm`/`Refund`/`Expire`),记录账本行与变更后的余
  额。
- **模块访问器:** `Plans()`、`Subscriptions()`、`Invoices()`、
  `Credits()`、`Entitlements()`、`PaymentEvents()`、`Polling()`。
- **结构化错误码**(`billing.insufficient_credits`、
  `billing.plan_not_found`、`billing.webhook_signature_invalid` 等)
  ——见[错误码索引(English)](/docs/user-guide/error-codes/#billing)。

## 已知限制与链接

- 没有调用方移动真钱:provider 包签真请求、验真签名,但没有任何参
  考应用或其它调用方对真实 Stripe/Alipay/WeChat 账户调
  `CreateCharge`。
- `SubscriptionService.Active` 假定每租户至多一个 active 订阅;数据
  库层也没有任何强制。
- 无 key 的 `Expire`(运营一次性形态)刻意在重试下不幂等——带 key
  的才是清扫该用的。清扫本身是宿主的 `jobs` 排程加产品策略决策。
- `Invoice` 与 Quota/`UsageReader` 判定路径有真实、经过测试的 API,
  但除编译型示例外没有工作区内调用方;`go/billing/AGENTS.md` 记录
  了精确的消费状态。
- HTTP 面(`GET /api/v1/billing/credits/balance`、
  `.../credits/transactions`)只读;退款以扣减行状态变为 `refunded`
  被观察到,绝不是一次静默的余额变化。

### 出处

- [go/billing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md)——权威文档(领域模型、账本语义、网关层、限制)
- 相关页面:[metering](/zh-cn/docs/user-guide/modules/services/metering/)、[ai-gateway](/zh-cn/docs/user-guide/modules/capabilities/ai-gateway/)、域指南[计量与账单](/zh-cn/docs/user-guide/domains/billing-metering/)
