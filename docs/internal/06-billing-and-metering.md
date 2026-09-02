# 计费、支付与计量

> Plan/Feature/Entitlement 领域模型、国内外双支付通道、用量计量管道的可靠性分级，以及与订阅并列的信用点模式。

## 领域模型：Plan / Feature / Entitlement

计费的一切都建立在这三个概念上，它们同时被功能开关（[11 横切能力](11-cross-cutting.md) 的"套餐权益"层）、AI 模型访问控制（[08 AI 网关](08-ai-gateway.md)）和前端定价页（[12 前端架构](12-frontend.md)）依赖，因此必须先定义清楚。

```go
type Feature struct {
    Key       string      // "seats" / "api_calls" / "ai_tokens" / "model:gpt-4o"
    Kind      FeatureKind // Boolean（有无） / Quota（有额度） / Unlimited
    Unit      string      // Quota 类的计量单位
}

type Plan struct {
    ID       string
    TenantID *string          // nil = 平台级公共套餐；有值 = 给大客户的定制套餐
    Name     string
    Price    Money
    Interval BillingInterval  // month / year / one_time
    Grants   []Grant          // 该套餐授予的权益
}

type Grant struct {
    FeatureKey  string
    Value       any          // bool / int64 / "unlimited"
    Period      ResetPeriod  // 额度的重置周期：不重置 / 每月 / 每计费周期
    OverageMode OverageMode  // Block / AllowAndBill / Notify
}

// 业务代码唯一需要关心的判定入口
type Entitlements interface {
    Check(ctx context.Context, featureKey string, requested int64) (Decision, error)
}

type Decision struct {
    Allowed   bool
    Remaining int64
    Reason    string   // "ok" / "feature_disabled" / "quota_exceeded" / "no_subscription"
}
```

**设计要点：**

- **Plan 支持租户级定制**：`TenantID` 非空即为给特定大客户的专属套餐。对外交付场景里"给某个客户单独议价"是常态，不预留这个字段后期改动成本极高。
- **`Check` 是唯一判定入口**：业务模块（含 AI 网关）只依赖这一个接口，不直接读订阅表、不自己算额度。Boolean 型 Feature 让"能不能用某个模型"和"额度够不够"复用同一套机制，不必另造开关。
- **额度判定读实时计数器**（见下文计量管道），不查汇总表——汇总有聚合延迟，用它判定会放行超额请求。
- **`Check` 与信用点是两条路径**：`Check` 回答"这个套餐允不允许"，信用点回答"余额够不够"。按次消费的业务通常两者都要过。
- **权益变更立即生效**：订阅升降级、套餐改动通过事件总线广播，各实例刷新权益缓存，与动态配置的热更新走同一机制。

## 计费与支付：国内 + 国际双模式
关键设计：**Subscription 是内部领域概念，支付渠道只是收款执行者**，二者解耦。

- 国际：Stripe 原生订阅（Subscription/Invoice/Metered Billing 直接可用）。
- 国内：支付宝/微信原生不支持周期扣款（周期扣款资质门槛高、能力受限），采用**一次性订单 / 预付费余额包 + 内部自建订阅周期**模式。这是务实取舍，必须写进文档避免业务方误用。
- 两种模式共享同一套 `Subscription`/`Invoice` 领域模型，UI 层无感知。
- Webhook 验签各家差异大（Stripe 签名头、支付宝 RSA2、微信证书+HMAC），一律封在各 Provider 内部，向上归一化成 `NormalizedEvent`。
- **回调必须幂等**（支付集成最经典的坑）：所有渠道都会重复投递同一事件（网络重试、对方超时重发），重复处理会导致重复发货或重复入账。以渠道事件 ID 作唯一键落 `payment_events` 表，**先insert去重、再处理**；处理逻辑本身也设计成可重入。
- **回调不可信**：回调内容只作为"去查一次"的触发信号，金额与状态一律以主动调用渠道查询接口的结果为准，不直接采信回调报文中的金额。
- **回调可能先于下单响应到达**，也可能永远不到达：必须有主动轮询兜底（走 `jobs` 定时任务扫描处于中间态的订单）。
- `billing-gateway` 拆成独立 module，业务方不接支付时不必被拉入三家 SDK 的依赖树。

## 计量计费：同一管道，两套后端

采集接口在两种部署模式下完全一致，业务代码只调 `metering.Recorder.Record(ctx, UsageEvent{...})`：进程内有界 channel 缓冲 + 后台 goroutine 批量 flush，`IdempotencyKey` 防重试重复计量。差异只在 flush 之后：

| 环节 | 单进程部署模式 | 分布式部署模式 |
|---|---|---|
| 缓冲与投递 | 内存 channel 直接进聚合器 | Redis Streams（消费者组，至少一次投递） |
| 聚合器部署 | 同进程 goroutine | 同进程 goroutine（MVP）→ 量大后拆独立容器 |
| 实时配额计数 | 进程内 `sync.Map` 计数器 | Redis `INCRBYFLOAT usage:{tenant}:{feature}:{period}` |
| 汇总存储 | SQLite `usage_*_summary` 表 | Postgres `usage_*_summary` 表 |
| 原始明细 | 默认关闭 | 可选开启，TimescaleDB hypertable + 保留策略 |

- 不引入 Kafka：Compose 小集群下 Kafka 的运维复杂度与团队规模严重不匹配，而 Redis 在分布式部署模式下本就要用于 session/缓存/限流，复用它不新增基础设施种类。
- TimescaleDB 是 Postgres 扩展（换镜像即可），不新增数据库引擎；单进程部署模式下 SQLite 无此扩展，因此原始明细默认关闭，只保留汇总表——这正是"能力按部署模式降级"原则的体现。
- 实时配额不查汇总表（有聚合延迟），走计数器；分布式部署模式下定期用聚合结果对账修正计数器，防长期漂移。
- **超额策略**：Plan 的 `OverageMode` 决定 Block / Allow&Bill / Notify；阈值事件经事件总线发出，由通知模块订阅。

### 可靠性分级：不是所有计量都能 fail-open

"fail-open 不阻塞业务"对分析型计量是对的，对计费型计量是错的——**丢一条事件就是少收一次钱，而且用于对账修正计数器的基准数据也一起丢了**，事后无从发现。因此按用途分两级：

| 级别 | 用途 | 失败行为 | 落地方式 |
|---|---|---|---|
| **计费级** | 直接产生费用（AI 调用、API 调用量、存储容量） | **不允许静默丢弃** | 与业务操作同事务写入本地 outbox 表，再由后台投递到聚合管道；投递失败无限重试 + 告警 |
| **分析级** | 仪表盘、趋势、运营分析 | fail-open，丢失可接受 | 内存缓冲批量投递，缓冲满即丢弃并计数告警 |

计费级用 **outbox 模式**（与业务写操作在同一个数据库事务里落一条待投递记录）是关键：这样"业务成功但计量丢了"在物理上不可能发生。代价是每次计费级计量多一次本地写入——对于按次计费的低频高价值操作（AI 生成），这个代价完全可接受；高频低价值的计量应归入分析级。

**信用点扣减不走计量管道**：预扣/确认/退还是同步的事务性操作（见下节），与事后计量是两条独立路径。计量用于账单明细与用量展示，扣点用于实时余额控制，两者最终对账，不能互相替代。

## 信用点（credits）：与订阅并列的计费模式

- 与订阅并列的第二种计费模式，也是国内支付场景的落地方式：`credit_balance`（租户余额）、`credit_transaction`（充值/扣减/退还/过期的完整流水，可对账）。
- **预扣 → 确认/退还** 两阶段：发起 AI 任务时预扣，成功则确认，失败则自动退还。这是所有"按次消费 + 可能失败"业务的通用模式，必须内建，否则每个项目都要自己实现且很容易漏掉退还。
- 支持套餐赠送点数、点数过期策略、余额不足的阻断与提醒。

