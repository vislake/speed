---
title: metering
description: "用量记录:一个 Recorder 接口、两级可靠性——容错的分析级与不可静默丢的计费级 outbox 投递——聚合进按租户的用量汇总。"
weight: 5
---

# metering

metering 是 speed 的用量计量模块:业务代码经一个 `Recorder` 接口
上报用量,与「哪个后端存它、聚合它」完全解耦。metering 负责测量与
发信号;`go/billing` 负责决定——本模块不带自己的套餐、配额或权益
模型。

## 它做什么

业务模块每单位用量报一个 `UsageEvent`——租户、`Feature`、
`IdempotencyKey`、`Quantity`、可选元数据。两级可靠性记录这些事件:

- **分析级**(`m.Recorder()`,即 `AnalyticsRecorder`):容错开放。有
  界内存通道加后台冲刷;缓冲满就丢事件并计数(`Dropped()`),重试
  的 record 不去重。给没有计费权重的用量用。
- **计费级**(包级 `Enqueue` 加一个 `Dispatcher`):不许静默丢、也
  不许静默双计。`Enqueue` **在调用方自己的事务里**写一行 outbox——
  行与你业务写的提交严格同步——由 UNIQUE `(tenant_id,
  idempotency_key)` 索引去重;`Dispatcher` 轮询待处理行并把每行送
  进聚合器,无限重试(固定延迟),超过声明的地平线后把永久失败行
  升级为 Error 级告警,并在与汇总写同一事务里落一行持久幂等收
  据——重投的行永远不可能双计。

两级汇聚到同一个进程内 `Aggregator`:按 (租户、feature、周期桶) 的
实时计数器、数据库背书的汇总行(`metering_usage_summaries`,租户
作用域),以及总线上边沿触发的超阈值事件——每次穿越恰好发布一
次。**一个 feature 只属于一个可靠性级**:同一个 `Feature` 经两条路
径记录,会把可丢与不可丢的数据混进无人能归属的一行。

它**不是**什么:无 Plan/Feature/Entitlement 模型、无积分、无配额
强制(那是 `go/billing` 的地盘——billing 的 `UsageReader` 接缝由
`*metering.Aggregator` 结构化满足,供配额判定);无 HTTP 面或
OpenAPI 片段(它是业务模块进程内调用的 Go 级 API);无分布式聚合
后端——进程内后端就是已交付的那个。

## 何时选用

任何用量必须被数清的产品面:功能采用、按租户仪表盘、将来的配额
判定。在使用发生的瞬间记录——一次完成的 AI 调用、一个 API 请求、
一份处理完的文档——按数字的含义选级:分析级给负载下可丢一事件的
计数器,计费级(在被计量之事落库的那笔事务内)给算钱的数字。数字
需要套餐或积分形状的决策,就配 billing;需要运营仪表盘,就把汇总
配给 admin 的用量汇总端点。

## 怎么接线

```go
m := metering.NewModule(db)          // 放进你的 Kernel.Bootstrap 模块集
m.Start(ctx)                         // 启动分析冲刷与 dispatcher 循环
defer m.Stop()

// 分析级——从请求路径发起,容错开放:
if err := m.Recorder().Record(ctx, metering.UsageEvent{
    TenantID:       tenantID,
    Feature:        "ai.chat_tokens",
    IdempotencyKey: "chat:" + callID,
    Quantity:       float64(tokens),
}); err != nil {
    // 处理 err
}

// 计费级——在调用方自己的事务内:
if _, err := metering.Enqueue(ctx, tx, metering.UsageEvent{
    TenantID:       tenantID,
    Feature:        "image.credits",
    IdempotencyKey: "img:" + jobID,
    Quantity:       1,
}); err != nil {
    // 处理 err
}
```

宿主选项设定周期桶(`WithPeriodBucket`,日或月)、超阈值
(`WithOverageThresholds`)、分析缓冲与 dispatcher 的间隔、批大小、
重试延迟、升级地平线与 outbox 保留期。`m.Summaries()` 读按租户的
汇总行;`m.Aggregator()` 服务实时计数器与计费级摄取。

## 核心概念与 API 面

- **outbox 是有意为之的平台数据。** `metering_outbox_records` 刻
  意不做租户作用域——后台 dispatcher 必须看见每个租户的待处理
  行,租户作用域的仓库做不到。
- **去重由数据库仲裁。** `Enqueue` 的唯一索引与收据的
  `ON CONFLICT DO NOTHING` 让两级都幂等,且从不中止调用方事务——
  该行为在真实 PostgreSQL 上被证明,那里的普通「捕获违规后重试」
  会毒化事务。
- **重启安全的计数器。** 实时计数器在首次触碰时从已提交的汇总行
  播种,进程重启既不丢东西,也不会双发超阈值穿越。
- **公平重试、诚实升级。** 认领顺序只依重试日程——从未失败的行饿
  不死失败过一次的行;永久失败的行不到终态却一定告警。
- **结构化错误码**(`metering.missing_tenant_id`、
  `metering.invalid_quantity`、`metering.field_too_long`、
  `metering.usage_summaries_unconfigured` 等)——见[错误码索
  引(English)](/docs/user-guide/error-codes/#metering)。刻意没有
  `metering.unknown_feature`:metering 没有 feature 目录——那属于
  billing。

## 已知限制与链接

- `Dispatcher` 假定同一时刻只有一个进程对着一套数据库:它的认领
  是读、不是原子认领-加锁,两个并发 dispatcher 会浪费工作(绝不双
  计——收据仲裁那件事),直到真正的认领步骤出现。
- `AnalyticsRecorder` 不去重重试的 record,按设计;汇总写由进程级
  互斥锁串行,原子 upsert 关掉跨进程竞态;重试没有退避曲线。
- 模块的依赖地板刻意很窄——pkgcore、dbkit、observability,加一个
  第三方包(`google/uuid`)。

### 出处

- [go/metering/AGENTS.md](https://github.com/vislake/speed/blob/main/go/metering/AGENTS.md)——权威文档(可靠性级、outbox 语义、聚合、已知限制)
- 相关页面:[平台服务](../)、域指南[计量与账单](../../../domains/billing-metering/)
