---
title: notification
description: "外发消息:经已声明通知类型的应用内收件箱、邮件与短信投递,外部收件人需经同意验证,带逐类型通道偏好。"
weight: 2
---

# notification

notification 是 speed 的外发消息模块:把租户的通知投递给它所服务的
人——用户走应用内收件箱、邮件与短信,外部联系人只有先完成同意验
证才会收到消息。

## 它做什么

你产品发出的每条消息都走一个**已声明的通知类型**,类型由发出它的
业务模块声明——在自己的 `Register` 期间 `reg.Notifications.Add`——
绝不在这里存成模板。声明携带该类型的偏好组、默认通道、可否退订
(验证码是事务性的,不可退订)与收件人可见参数;文案住在声明模块
自己的双语 locale 包里,按 `<type_key>.<channel>.<part>` id 惯例,
投递时按收件人 locale 渲染。模块自己的表不持任何身份数据:用户是
一个不透明 id,用户收件人的外发地址在发送时经宿主的
`UserAddressResolver` 接缝读取。

投递天生异步。`Dispatch` 只校验并入队——每位收件人每通道一个
job;worker 重建租户上下文,在**发送时**重查偏好、同意与地址——
入队与投递之间可能变化的任何东西都不冻结进 payload——渲染文案,
投递(收件箱行、邮件、短信),并为每个尝试过的通道结算一行
`send_records`。应用内收件箱是一等通道:零外部依赖,经 SSE 流
(`GET /api/v1/notifications/stream`)实时宣告,先行后事件顺序。

它**不是**什么:不是你产品通知类型的拥有者(那是你业务模块的声
明),不是模板编辑台,不是邮件服务商——它经你接好的传输投递,并且
永不 import `authn`、`rbac` 或 `org`。

## 何时选用

你发任何种类的消息——事务性的(验证码、告警)、事件驱动的(租户
里发生了某事)、或偏好路由的(逐类型 × 逐通道的选择,缺省取声明默
认)。你需要带未读计数的应用内收件箱;你需要触达**不是**任何租户
用户的联系人(带双重确认或业务证明的同意账本);你需要每次尝试都
可审计的投递结果。如果你的「消息」其实是发往客户系统的外发 HTTP,
那是 integration 的 webhook 半边,不是本模块。

## 怎么接线

六个选项必选——缺任何一个,`Register` 都以各自具名的 `Err*Required`
拒绝:

```go
m := notification.NewModule(db,
    notification.WithSMSSender(pkgcore.NewConsoleSMSSender()), // pkgcore 的 SMS 接缝——console、HTTP 网关或运营商适配器
    notification.WithMailFrom("no-reply@example.com"),
    notification.WithContactEmailIndexer(emailIndexer),   // dbkit.NewBlindIndexer,over notification.AddressIndexColumn
    notification.WithContactPhoneIndexer(phoneIndexer),   // 索引键必须与加密键不同
    notification.WithDeliveryQueue(queue),                // 投递 handler 排空的 jobs.Queue
    notification.WithUserAddressResolver(myResolver),     // 读取用户的已验证外发地址
    // 可选:WithSubjectResolver——HTTP 面的调用者身份
)
```

宿主还把业务事件接到 `Deliveries().Dispatch` 上(宿主胶水——模块除
自己的事件外什么都不订阅)。服务访问器是 `Preferences()`、
`Contacts()` 与 `Deliveries()`;HTTP 面是 `/api/v1/notifications`
下的十一个操作(收件箱读取与已读标记、未读数、类型目录、偏好读
写、联系人名册创建/验证/重发)外加手工挂载的流。

## 核心概念与 API 面

- **偏好矩阵。** 逐类型 × 逐通道的行(`notification_preferences`);
  缺行按类型的声明默认解析。写入对着实时类型注册表校验——未知类
  型或通道直接拒绝,绝不存成将来不可达。
- **外部联系人的同意。** `VerifiedContact` 是一个通道(邮件或短
  信——联系人永不在应用内)上一个受同意门控的地址,静态加密、盲
  索引列。同意经双重确认(6 位码,5 分钟有效,以 SHA-256 哈希住在
  行上,`VerifyCode` 是一次比较-交换)或业务证明(`ConsentRef`)到
  达。`unsubscribed` 与 `bounced` 是每次投递都拒绝的终态;逐类型
  退订把一位已验证联系人从一种类型中窄化出去,其余照收。验证尝试
  在查验码之前先扣每地址预算。
- **一切在发送时重查。** 偏好、同意与地址由投递 job 现读,绝不采
  信 payload;永久传输失败(`ErrTransportPermanent`)把租户自己的联
  系人标成 `bounced`。
- **结果日志。** 每个尝试过的通道一行 `send_records`,收在
  UNIQUE `(tenant_id, idempotency_key)` 索引下——`succeeded` 只在
  传输接受之后写,`failed` 带受限分类(绝不存原始传输文本),
  `skipped` 带短理由。至多一次只跨单次投递的重试成立;传输与记
  录之间的崩溃窗口被如实记录,而非设计掉。
- **locale 纪律。** 用户投递携带收件人必填的 locale;模板缺失或
  locale 未知是结构化内部错误,绝不回退到另一种语言。(外部联系
  人按平台默认 locale 渲染。)

## 已知限制与链接

- 无平台黑名单写入方、无租户强制偏好层、无同类型聚合或投递限
  流、无管理端模板编辑、无 `@speed/notification-ui` 前端——每条都
  带理由记在模块的 `AGENTS.md` 里。
- 收件箱流不发心跳;用户收件人半边按契约把一项安全义务委托给宿
  主(resolver 必须返回已验证地址——模块无法察觉提供未验证地址的
  宿主)。
- 结构化错误码:见[错误码索引(English)](/docs/user-guide/error-codes/#notification)——
  偏好组、联系人组(码、同意、退信、限流拒绝)、派发与收件箱组,
  以及六个 `Err*Required` 接线哨兵。

### 出处

- [go/notification/AGENTS.md](https://github.com/vislake/speed/blob/main/go/notification/AGENTS.md)——权威文档(投递流水线、同意状态机、宿主接缝、规则、未实现清单)
- 设计依据:[docs/internal/07-platform-services.md](https://github.com/vislake/speed/blob/main/docs/internal/07-platform-services.md)
- 相关页面:[平台服务](../)、域指南[任务与通知](../../../domains/jobs-and-notifications/)
