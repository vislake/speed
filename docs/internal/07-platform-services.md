# 平台服务

> 五个几乎每个 SaaS 都需要、但常被漏掉的通用能力：异步任务、媒体存储、分享链接、通知、对外集成。
>
> 这批能力是在用真实业务需求（[14 示例应用](14-reference-app.md)）校验方案时暴露出来的缺口——它们的缺失会在第一个真实项目接入当天就变成阻塞。

## 异步任务队列（jobs）

**这是原方案最严重的缺口**。事件总线是 fire-and-forget 的通知机制，无法承载"AI 生图要跑 40 秒、可能失败、需要重试、用户要看进度"这类需求。几乎所有 SaaS 都需要一个真正的任务队列。

```go
type Queue interface {
    Enqueue(ctx context.Context, task Task, opts ...EnqueueOption) (JobID, error)
    Get(ctx context.Context, id JobID) (*Job, error)   // 状态/进度/结果/错误
    Cancel(ctx context.Context, id JobID) error
}
type Handler interface {
    Type() string
    Handle(ctx context.Context, job *Job, progress func(pct int, msg string)) (Result, error)
}
```

- **多套实现**：进程内 worker pool + SQLite 持久化任务表（重启后能恢复，不是纯内存——任务丢失比配额少算严重得多）；Redis（`hibiken/asynq`，成熟且自带重试/延迟/定时/Web UI，不必自造）。选哪套由装配决定，见 [03 部署模式与实现组装](03-deployment-modes.md)。
- 必备能力：**指数退避重试**、超时控制、**幂等键**（同一业务操作重复提交只跑一次）、优先级队列、延迟与定时任务（替代 cron）、死信队列、并发度限制（AI 类任务要按租户限并发，防单租户打满 worker）。
- **进度上报**：`progress()` 回调写入任务记录，前端 `useJob(jobId)` 轮询或 SSE 订阅。
- **任务必须带租户上下文**：worker 执行时重建 `tenantctx`，否则 Repository 的租户过滤会 fail-closed 报错——这是最容易踩的坑，要在 `AGENTS.md` 里明确。
- **失败补偿要业务显式声明**：AI 生成失败必须退还已扣的信用点。队列只提供 `OnFailure` 钩子，补偿逻辑属于业务，脚手架不替业务猜。

## 媒体存储与处理（storage）

```go
type Object struct {
    ID          string
    TenantID    string
    Key         string   // 存储路径，不直接暴露给客户端
    MIME        string   // 服务端探测结果，非客户端声明
    Size        int64
    Checksum    string
    Derivatives []Derivative  // 缩略图 / 转码等派生资源，删除时一并清理
    ExpiresAt   *time.Time    // 生命周期策略
}
```

- **上传走预签名直传**，文件不经过应用服务器（照片动辄数 MB，代理转发会打满带宽和内存）。后端只签发凭证并在回调中登记元数据。
- **安全校验不可省**：服务端二次校验真实 MIME（不信任扩展名与客户端声明）、尺寸与像素上限、**剥离 EXIF**（含 GPS，患者照片带地理位置是隐私事故）、可选病毒扫描钩子。
- **派生资源异步生成**：缩略图、格式转换（WebP）、水印，全部走 `jobs` 队列。
- **访问控制**：私有对象一律短时效预签名 URL 访问，禁止公开桶；公开分享走 `sharing` 模块签发的令牌，二者不混用。
- **多套实现**：本地文件系统 + 应用自身提供带鉴权的读取端点；S3 兼容（MinIO / 阿里云 OSS / AWS S3）。本地实现不满足 `MultiReplicaSafe`，装进多副本部署模式时装配校验会拒绝它。
- **生命周期**：支持按租户配置保留期与自动清理（配合 `compliance` 的数据保留策略），删除时同步清理派生资源，避免孤儿文件。
- 前端：`ui-kit` 提供 `FileUploader`（拖拽、进度、多文件、失败重试、预览），不再让每个项目重写一遍。

**实现状态（2026-09，storage 模块轮）**：本节是目标设计；`go/storage` 模块已作为独立模块轮提前于 M2 计划窗口落地（排期注见 [15 里程碑](15-roadmap.md)），reference-app 端到端接入是第一个消费者（`cmd/server/storage_flow_test.go`）。逐项对照如下；能力边界、已知限制与延期项以 `go/storage/AGENTS.md` 为准：

- **数据模型**：目标设计的 `Object` 拆为两张双方言迁移的 `TenantScoped` 表——`objects` 与 `object_derivatives`；`Derivatives []Derivative` 不是内嵌字段，而是按 `(tenant, object_id, kind)` 唯一索引的独立行（`kind` 当前仅 `thumbnail`）。存储路径即 `ObjectKey(tenant, objectID)` / `DerivativeKey(tenant, objectID, kind)`，由模块自身推导，永不暴露给客户端。
- **上传链路**：已落地为**服务端中转流式上传**而非本节的预签名直传——`Create` 开启上传窗口（依声明的大小/类型/校验和，可附请求保留期，宿主以 `WithMaxObjectLifetime` 设上限），`Upload` 将请求体流式写入 `pkgcore.ObjectStore` seam（字节不进数据库），`Complete` 收口。**预签名直传未落地**：模块不引入任何 presigner，直传凭据与短时效预签名 URL 一并留待分布式形态轮。
- **安全校验**：已落地于 `Complete` 的再校验管线——以对存储字节的实际探测为准，不信任客户端声明：真实大小与 MIME、字节上限与像素上限、**结构化元数据剥离**（JPEG APP 段、PNG eXIf，含 GPS；结构无法验证的文件直接拒绝而非放行），随后才落定 checksum 与尺寸。可选病毒扫描钩子未落地。
- **派生资源**：缩略图异步派生已落地——`Complete` 把任务入队到模块 `Register` 必需的 `jobs.Queue`，worker 侧经 `DeriveService` 写入 `object_derivatives`。WebP 转换与水印未落地。
- **访问控制**：已落地形态为私有对象经 API 鉴权访问——内容经 `OpenContent` 服务端流式返回，租户取自请求上下文，绝不来自请求参数；**短时效预签名 URL 未落地**；对外分享属 `sharing` 模块（M3），与本节「内部预签名、外部分享令牌、二者不混用」的划分一致。
- **多套实现**：字节所在的 `ObjectStore` seam 的本地 FS 与 S3 兼容双实现已由 `pkgcore` 落地（见 [03 部署模式](03-deployment-modes.md) 与 `go/pkgcore` 的 census 条目），storage 自身只依赖 seam 接口；本模块的集成层以 PostgreSQL + MinIO 两条腿验证（`go test -tags=integration`）。
- **生命周期**：核心已落地——`LifecycleService.Delete` 崩溃收敛删除协议（行标记 `completed`→`deleting` → 删原字节 → 按确定顺序删各派生行的字节 → 单事务删全部行；任一步中断由下一次运行收敛而非重复执行），删除同步清理派生资源；宿主经 `EnqueueExpirySweep` 按租户排程的 `Sweep` 恢复中断删除、回收上传窗口已关闭的 `uploading` 行、删除保留期已到的 `completed` 对象。差异：保留期由调用方逐对象请求、宿主设上限，**按租户的运行时保留策略配置、以及与 `compliance` 数据保留策略的联动，仍属 M4**。
- 前端 `ui-kit` 的 `FileUploader` 已随其组件轮（2026-09-04）提前于 M2 计划窗口落地，排期注见 [15 里程碑](15-roadmap.md)：完全受控的队列组件——队列就是 host 自己的 `rows` 状态，每行状态与进度按 props 原样渲染，每次 pick/取消/重试/移除经 `onSelectFiles`/`onCancel`/`onRetry`/`onRemove` 回调上报，上传传输（预校验、并发与网络调用）是 host 自己的代码，**组件零网络**，不持有 File 超过接收它的 event handler；本条目标设计点名的拖拽、多文件、进度与失败重试均已交付，**预览不在组件内**。storage 的前端调用（api-sdk 生成的 hooks）等合并文档扩展时落地——orval 只跑合并文档（notes + authn）里的片段，storage 片段要等下一次扩展（org 片段先排队的 org-web 轮，storage 搭同一班再生，见 `go/storage/AGENTS.md` 的 deferral 表）；wire 契约见 `go/storage/api/openapi.yaml`。

## 分享链接（sharing）

面向"把一份内部资源安全地给外部人看"的通用需求：患者查看自己的效果图、客户查看报告、匿名访问一次性结果页。它与 `storage` 的预签名 URL 是两套机制——预签名是给已认证用户的内部访问，分享链接是给未认证外部访问者的受控入口。

**令牌模型**

```go
type Share struct {
    Token       string     // 高熵随机串，不可枚举（≥128 bit）
    ResourceRef string     // 资源引用，不暴露内部 ID
    ExpiresAt   *time.Time // 可选过期时间
    MaxViews    *int       // 可选访问次数上限
    PasswordHash *string   // 可选访问密码
    RevokedAt   *time.Time // 撤销标记
}
```

**必须遵守的规则**

1. **令牌必须高熵且不可枚举**：禁止用自增 ID 或可预测的哈希；泄漏一个链接不能推导出其他链接。
2. **默认有期限**：创建时若未指定过期时间，采用租户配置的默认值（默认 30 天），不允许创建永不过期的链接——这是最常见的数据泄漏来源。
3. **撤销立即生效**：撤销后所有访问立即失败，不依赖缓存过期。因此分享页面与其承载的资源**必须声明 `Cache-Control: no-store`，且不得置于 CDN 缓存之后**——CDN 缓存会让"已撤销"的内容继续对外可访问，这是撤销机制最常见的失效方式。
4. **访问不需要登录，但必须留痕**：每次访问记录时间、IP、UA、来源，供资源所有者查看"谁看过、看了几次"。
5. **分享页面不得携带任何租户内部信息**：不暴露租户名、内部 ID、其他资源的存在性。
6. **敏感资源的分享需要二次确认**：涉及个人敏感信息的资源，创建分享链接本身是一条审计事件（见 [10 合规与审计](10-compliance-and-audit.md)）。

**与其他模块的关系**：分享的资源内容通过 `storage` 取得；访问统计经事件总线流入 `compliance` 审计；到期清理走 `jobs` 定时任务。

## 通知系统（notification）：站内信 + 按类型选渠道

**核心模型是一张"通知类型 × 渠道"的偏好矩阵**，而不是一个全局开关。业务代码只声明"发生了什么"，具体走哪些渠道由用户偏好决定：

```go
notifier.Notify(ctx, Notification{
    Type:      "billing.quota_warning",     // 通知类型，非渠道
    To:        notify.User(userID),         // 或 notify.External(...)，见下
    Params:    map[string]any{"used": 900, "limit": 1000},
})
// 渠道由 用户偏好 → 租户策略 → 类型默认值 逐级解析得出
```

### 两类收件人

系统用户不是唯一的收件对象——`reference-app` 要给**患者**发短信，患者并不是系统用户。因此收件人有两类，走完全不同的准入规则：

```go
notify.User(userID)                       // 系统用户：走偏好矩阵
notify.External(contactID)                // 外部联系人：必须先完成登记与验证
```

| | **系统用户** | **外部联系人** |
|---|---|---|
| 准入 | 已注册即可 | **必须先登记为已验证联系人** |
| 渠道选择 | 用户偏好矩阵 | 登记时确定的渠道，且仅该渠道 |
| 退订 | 偏好设置页 | 每条消息附退订入口，退订即拉黑 |
| 频率 | 按类型限频 | 更严格的独立限频 |

### 外部联系人：先验证，后发送

**不允许向任意手机号/邮箱直接发送**——这既是防止系统被当作垃圾信息发送器（一旦被滥用，短信通道和邮件域名的信誉会被整体拉黑，影响所有租户），也是国内个保法与反垃圾邮件法规的硬要求。

外部联系人必须先进入 `verified_contacts` 表，且状态为已验证：

```go
type VerifiedContact struct {
    ID          string
    TenantID    string          // 同意是对具体租户的，不跨租户复用
    Channel     Channel         // sms / email
    Address     string          // 加密存储 + 盲索引（见 10 合规文档）
    Status      ContactStatus   // pending / verified / unsubscribed / bounced
    ConsentBy   ConsentSource   // double_opt_in / business_attested
    ConsentAt   time.Time
    ConsentRef  string          // 业务方声明时的凭据引用（如授权书编号、签署记录 ID）
    VerifiedAt  *time.Time
}
```

**两条合法的取得同意路径：**

1. **双向确认（double opt-in）**：系统向该地址发一条**仅含验证链接/验证码**的消息，对方确认后状态转为 `verified`。验证消息本身是唯一允许发给未验证地址的消息类型，且有严格频率限制（同一地址每日上限、失败次数上限），防止被用作短信轰炸的跳板。
2. **业务方声明（business attested）**：业务场景中已取得书面/电子授权（如患者在诊所签署的知情同意书）。此时由业务方调用登记接口并**必须提供 `ConsentRef`**（授权凭据引用）。这条路径的责任归属明确落在业务方，脚手架侧记录来源与时间戳作为合规追溯证据，并在审计日志中留痕。

**其余硬性规则：**

- **同意是租户级的**：患者退订 A 诊所不影响 B 诊所；反过来，A 诊所取得的同意也不能被 B 诊所复用。
- **退订即永久拉黑**：状态转 `unsubscribed` 后除非重新走一次同意流程，否则任何消息（含事务性）都不再发送。每条外发消息必须带退订入口（短信回 TD、邮件退订链接）。
- **平台级黑名单**：投诉、硬退信（hard bounce）、无效号码进入平台级黑名单，跨租户生效——这类地址再发只会继续损害通道信誉。
- **地址加密存储**：手机号与邮箱属于个人信息，字段级加密 + 盲索引查询（见 [10 合规与审计](10-compliance-and-audit.md)）。
- **发送前二次校验**：投递任务执行的那一刻重新检查状态，而不是只在入队时检查——入队与投递之间可能已经退订。

**通知类型注册表**：每个模块声明自己的通知类型，包含 key、所属分组（安全 / 账务 / 协作 / 营销）、默认渠道、**是否可退订**、双语文案模板。这份注册表同时驱动三件事——偏好设置页面自动渲染（不用为每个通知写页面）、文档自动生成、发送时的合法性校验。

**站内信是一等渠道**（不是邮件的附属品）
- 独立的 `in_app_messages` 表，含未读状态、分组、跳转链接、过期时间。
- 提供未读计数、标记已读/全部已读、按分组筛选、批量清理。
- **实时推送**：SSE 通道（比 WebSocket 轻，且这里只需服务端单向推）。两套实现：单副本下直接推；多副本下经 Redis Pub/Sub 扇出到所有实例。不做实时的话前端只能轮询，体验和成本都差。
- 前端新增 `@speed/notification-ui`：通知中心（铃铛 + 未读角标 + 下拉列表 + 消息中心页）与偏好设置矩阵表格，开箱即用。

**偏好优先级**：用户个人设置 > 租户强制策略 > 类型默认值。
- 租户管理员可以**强制**某些通知必达（如安全告警、账单逾期），用户不能关闭——企业场景的实际需要。
- **安全类通知不可完全关闭**：改密码、新设备登录、异地登录这类，至少保留一个渠道，UI 上禁用取消。
- **营销类必须可一键退订**，且邮件正文带退订链接（合规硬要求）。

### 触发方式：事件驱动，不是直接调用

业务模块**不直接依赖 `notification`**。`authn` 检测到异地登录、`org` 发出邀请、`billing` 触发超额，都只发布领域事件；`notification` 订阅事件并按订阅表决定发什么通知。

这样做的理由是硬性的：如果每个业务模块都直接调 `notifier`，依赖图上会多出 authn/org/billing/storage/compliance → notification 一大把边，与"业务模块之间用 ID 引用 + 领域事件解耦"的架构纪律直接冲突，也会让"关闭通知模块"变成不可能。

`notification` 维护一张 `事件类型 → 通知类型` 的映射表（同样在模块注册时声明），业务方要改"什么事件发什么通知"只改映射，不改业务代码。

**唯一例外**：验证码类消息（登录短信、邮箱验证、外部联系人验证）是**同步且强一致**的——用户在等这条消息，发送失败必须立刻反馈错误，不能走异步事件。这类走直接调用，且 `authn` 对 `notification` 的这个依赖要在依赖图中显式标注。

**其余机制**
- 模板按 `template_key + locale` 存储，按**收件人 locale** 渲染（与 [11 横切能力](11-cross-cutting.md) 的国际化规则一致），运营后台可编辑与预览。
- 全部经 `jobs` 异步投递并重试，发送记录落库（渠道、状态、耗时、错误、供应商回执 ID），运营后台可查"这封邀请邮件到底发出去没有"。
- 同类通知的**聚合与限频**：短时间内的重复通知合并发送，避免把用户淹没（如批量导入失败不该发 500 封邮件）。
- **每个发送通道各有多套实现**：邮件是 `ConsoleMailer` / SMTP，短信是控制台输出 / 各短信网关，推送同理。选哪套由装配决定，与部署成几个副本无关——单进程部署照样可以走真实 SMTP。站内信落库不依赖任何外部服务，在任何组装下都完全可用。

**实现状态（2026-09-04，notification 模块轮）**：本节是目标设计；`go/notification` 已作为独立模块轮提前于 M2 计划窗口落地（排期注见 [15 里程碑](15-roadmap.md)），reference-app 端到端接入是第一个消费者（`cmd/server/notification_flow_test.go`）。逐项对照如下；能力边界、已知限制与延期项以 `go/notification/AGENTS.md` 为准：

- **偏好矩阵**：核心机制已落地——`notification_preferences` 按类型 × 渠道存用户选择，`Set` 只接受活声明的类型与渠道（未知声明直接拒绝），`ResolveForDelivery` 对未设置的行缺省为类型默认值；安全类不可退订由声明的类型标记（声明不可退订的类型拒绝全关）保证。**"租户强制策略"中间层未落地**——没有管理员面，偏好优先级实际是两级（用户行 → 类型默认值）。
- **通知类型注册表**：声明方在 `Register` 时经 `reg.Notifications.Add`（key、默认渠道、是否可退订）声明，taxonomy 是**活的注册表**、解析时读取而非冻结快照——发送前的合法性校验与偏好设置的校验共用同一份声明。文案模板不经通知模块存储：类型 key `<module>.<entity>.<action>` 的文案由**声明模块自己的双语资源**按 `<type_key>.<channel>.<part>` 约定提供（站内信 title/body、邮件 subject/body_text、短信 text），投递时按收件人 locale 从 host 合并的 i18n 目录渲染——与本节"注册表驱动偏好页自动渲染 / 文档自动生成"对应的前端与文档生成未落地，运营后台模板编辑亦未落地。
- **两类收件人与外部联系人台账**：已落地且与设计一致——用户收件人的渠道地址是身份数据，由 host 经 `UserAddressResolver` seam 在投递时解析（模块无用户表）；外部联系人的地址与同意落在租户自己的 `verified_contacts`（加密落库 + 盲索引查询），两条同意路径齐备：`double_opt_in` 经验证码（代码哈希存于行上、验证消息是唯一允许发给未验证地址的消息、发送前于行上盖章再同步发出，`VerifyCode` 单赢家收敛在集成层以 PostgreSQL 腿证明——该比较并交换语义是 SQLite 串行化无法置于风险下的）、`business_attested` 要求 `ConsentRef`。`unsubscribed` 与 `bounced` 是终态，投递任务执行时二次校验状态。**平台级黑名单**：`platform_blacklist` 表与读路径（`IsBlacklisted`）已落地，**写者（投诉回调与投递硬失败腿）与恢复流程延期**；**类型级退订**（"这个类型不发"）、按联系人的 locale 协商亦在延期清单。
- **站内信一等渠道**：已落地——`in_app_messages` 表（跳转链接、过期时间字段随模型）与未读计数、单个/全部标记已读、按类型筛选读取的 HTTP 面；实时推送是 SSE `GET /api/v1/notifications/stream`（行先落库、后发 `notification.inbox.created` 事件，订阅者可读回而不与写者竞争；模块 `Register` 订阅自己的该事件扇出给本副本的连接，多副本经总线扇出由集成层 Redis 腿证明）。前端 `@speed/notification-ui`（铃铛、未读角标、偏好矩阵页面）未交付，属 M2 的 web round。
- **触发方式**：事件驱动原则照落地，但**接线在 host 而不是映射表订阅**——业务模块只声明类型、发领域事件；host 订阅自己的事件并调 `Deliveries().Dispatch`（notification 的 `Register` 只订阅自身的 inbox-created 事件），通知模块保持依赖图叶子位置，host 也因此能决定"什么事件发什么通知"而不改业务代码。**验证码例外**按设计只留给外部联系人验证消息（notification 自己同步发出）；`authn` 未依赖 notification——登录验证码走 authn 自己的短信发送器，设计正文"authn 对 notification 的依赖显式标注"未按原文落地。
- **异步投递与发送记录**：已落地——一次 `Dispatch` 每个收件人每渠道一个 `jobs` 队列任务，渲染后投递；`send_records` 记录每次尝试（渠道、状态、耗时、错误、供应商回执 id——`provider_receipt_id` 列已留），投递键由业务事件 + 收件人 + 渠道派生，`UNIQUE (tenant_id, idempotency_key)` 限定记录集，尽力而至多一次的收敛以"先查记录、再尝试、再落账"实现。同类通知**聚合未落地**（与限频同属延期清单或后续轮，以 AGENTS.md 为准）。
- **每通道多套实现**：站内信落库零外部依赖 ✓（任何组装可用）；邮件走 pkgcore `Mailer` seam（host 注入，`WithMailFrom` 必填）；短信是包内 `SMSSender` seam 的 `NewConsoleSMSSender`（单进程装配与测试双用）——短信升格为 pkgcore 级 seam（连同 authn 的 HTTP 发送器进 seam registry）在延期清单。

## 对外集成：API 开放与外发 Webhook（integration）

**API Key 与限流**
```go
type APIKey struct {
    ID          string
    TenantID    string    // 归属租户，不归属个人
    Prefix      string    // 明文前缀，用于识别与检索（如 "sk_live_a1b2"）
    Hash        string    // 密钥本体只存哈希
    Scopes      []string  // 权限范围，创建时从创建者权限中选取的子集
    CreatedBy   string
    ExpiresAt   time.Time // 强制到期，默认上限 1 年
    LastUsedAt  *time.Time
    RevokedAt   *time.Time
}
```

- 租户可自助创建 API Key（前缀可识别 + 仅创建时明文展示一次 + 存哈希）、按 Key 设置权限范围（复用 RBAC 的 permission 集合）与到期时间、支持轮换与吊销。
- **Key 归属租户，不归属个人**：创建者只是记录在案的责任人，成员离职删除账号时 Key 不会随之失效——集成会因为某人离职而中断是不可接受的。但离职流程必须提醒接管，且 Key 列表显示"创建者已离职"标记。
- **Key 的权限是创建者权限的子集且不随之变化**：创建时从创建者当前权限中选取，之后创建者权限变化不影响已发出的 Key（否则权限会在无人察觉时悄悄扩大或失效）。需要变更只能重新签发。
- 强制**到期时间上限**（默认 1 年），到期前通过通知提醒轮换；长期不过期的 Key 是最常见的凭证泄漏面。
- **限流三层，基于 `go/ratelimit`**：全局、租户级、Key 级三层，分别对应一次 `go/ratelimit.Allow(ctx, key, limit)`（单 key 判定）调用，由 `integration` 自行组合三层、任意一层拒绝即整体拒绝——多维度组合是调用方职责，不是共享原语内置的能力；滑动窗口算法本身与 `KVStore` 契约细节见 [11 横切能力](11-cross-cutting.md) 的"限流"一节，不在此重复。
- **HTTP 语义翻译是 `integration` 自己的职责**：`go/ratelimit` 返回的 `Decision` 是协议无关的纯数据，由 `integration` 的 handler 层负责把被拒绝的 `Decision` 翻译成 `429` 响应，带 `Retry-After` 与配额响应头。
- API 调用同样接入 `metering`，可作为计费维度。

**外发 Webhook**
- 租户可订阅事件类型并配置接收地址。事件源来自领域事件总线，业务模块无需额外写代码。
- **但内部领域事件不能直接外发**：内部事件带着内部字段结构，一旦外发就成了事实上的公开 API，此后任何内部重构都会破坏客户的接收端。必须有一层**内部事件 → 公开事件 schema 的显式映射**，只暴露刻意选择的字段。
- **公开事件独立版本化**：负载 schema 用 `event.type` + `event.version` 标识（如 `billing.subscription.created` / `v1`），schema 定义随版本发布（见 [21 API 契约](21-api-contract.md) 的覆盖范围说明）。破坏性变更走新版本号并保留旧版本一段时间，而不是原地改。
- 必备：**HMAC 签名 + 时间戳**（接收方可验真、防重放）、指数退避重试（走 `jobs`）、死信与手动重投、投递日志、**出站地址 SSRF 防护**（禁止内网地址段，这是外发 Webhook 最常见的安全漏洞）。

