# 31 发布对账清单（下一次发布的破坏面与升级指引）

## 1. 这份清单是什么

本文登记自 2026-09 以来、尚未随任何发布说明对外呈现的全部**破坏性变更**，每一条都给出出处提交（sha）与替代路径。用途有两个：

- 下一次发布（见 [02 仓库与发布](02-repo-and-release.md) 的发布机制一节）撰写 release note 时，直接以本文为底稿；
- 面向消费者（尤其 examples/reference-app 与 saasctl 生成骨架这两类**仓内消费者**，以及未来的仓外消费者）写升级指引时，按本文逐条核对。

前提事实（决定了本清单的紧迫性与写法）：

- **v0.0.1 首发已作废、已删除远端与本地 tag，当前没有任何已发布版本、也没有仓外消费者**（02 号文"同一版本的 tag 语义"小节记录了作废与删除）；因此本清单是**预登记**，不是对已发布契约的追溯说明。
- 破坏面全部落在 `github.com/vislake/speed/go/*` 的 Go 模块公共面与宿主（host）装配契约上；npm 侧不受影响。
- 本文只登记"消费者可观察到的面"（符号、签名、环境变量、组装契约）；工具内部的机械迁移（§6）与纯注释改写不计入。

## 2. A 组：既有三处（先于 29 工程本身）

### A1. R2：宿主手写 ticker 循环退役 + `TenantLister` 改造

- **面**：宿主定时任务从"宿主自己写 ticker 循环 + 自己遍历租户"改为"模块在 `Schedules` 座声明 `pkgcore.PeriodicTask`，宿主起一个 `jobs.Scheduler`（`jobs.WithSchedules` + `jobs.WithTenantLister`）统一驱动"。
- **消费者影响**：任何自己维护 ticker 的宿主删掉那段代码，改为声明 + 装配 scheduler；每租户任务的幂等键格式统一为 `<模块>.<任务>:<窗口>`。
- **替代路径**：`pkgcore.ComponentRegistry` 的 `Schedules` 座（`PeriodicTask`）；`go/jobs` 的 `Scheduler`、`TenantLister` seam；reference-app 的改造是本条的第一个消费者实例。
- **出处**：`4e7535cb`（`pkgcore`：Registry 的 periodic-schedule 座）+ `88b169f8`（`jobs`：portable periodic-task scheduler）+ `9b61449f`（`reference-app`：按 scheduler 排程）。
- **说明**：这三个 sha 是 R2 三件套的**实际落地提交**；早期侦察记录里的 `477ef65b`/`2d11410c` 是引擎/宿主组装轮（§3.4）的提交，不是 R2 本身。

### A2. R3：`app.ServeUntilShutdown` 退役

- **面**：`go/app` 删除 `ServeUntilShutdown` 预引擎 helper（宿主信号处理由引擎自身的生命周期接管）。
- **替代路径**：`app.Assemble` / `app.Shutdown` / `app.RunAssembly`（见 `go/app/AGENTS.md`）。
- **出处**：`e4cdb1b0`（`refactor(app)!: retire the pre-engine serve helper`，带 `!BREAKING` footer）。

### A3. 宿主环境变量重命名：六平台键平铺 → 派生双下划线

- **面**：reference-app 宿主面向操作者的平台键环境变量由扁平拼写改为 `HOST_KEY_<模块>__<键>` 形式的双下划线拼写（六平台键）。
- **替代路径**：`examples/reference-app/DEPLOY.md` 的对照表（`eb0bd7c5` 落地）。
- **出处**：`f55d1652`（键材料声明改写）+ `eb0bd7c5`（DEPLOY.md 对照表）。

## 3. B 组：29 工程累积（配置驱动的组件装配）

### B1. `app.Application.Kernel()` 退役

- **面**：`go/app` 的 `Application` 不再暴露 `Kernel()`。
- **替代路径**：宿主直接操作 `pkgcore.ComponentRegistry`（`app.Assemble` 驱动七阶段），不再经过内核对象。
- **出处**：`336e14fe`（`refactor(app)!: assemble the option surface through the component machinery`）。

### B2. `go/app` option 面整体删除（23 符号）

- **面**：`go/app` 原 option 面（`WithEventBus`/`WithKVStore`/`WithMailer`/`WithObjectStore`/`WithPreset` 等）全部删除。
- **替代路径**：组件组合配置（selection 的 `components` 块）+ 各 seam 实现在 ByType 上下文中的值；模板工程的 `selection/*/server.go` 与 reference-app 已是新面样板。
- **出处**：`c02a1ad3`（`refactor(app)!: retire the transition assembly surface`，footer 列出 23 个符号）。

### B3. D2b：模块声明面换型（`Registrar` view）

- **面**：模块 `Register`/`Attach` 的参型由过渡期 `Registrar` view 改为 `*pkgcore.ComponentRegistry`。
- **替代路径**：座席访问器（`reg.RoutesSeat()`、`reg.ConfigSeat()`、`reg.EventsSeat()`…）；`go/pkgcore/componenttest` 提供测试侧窗口帮手。
- **出处**：`ad99ab2b`（`refactor(pkgcore)!: address the module declaration face through the Registrar view`）。

### B4. E 组宿主组装契约翻转：宿主步骤即组件

- **面**：宿主的组装单元由"宿主步骤描述符"翻转为"普通组件"（`pkgcore.Component`），七阶段驱动（Prepare/Construct/Verify/Init/Start/…）统一处理；宿主自身也通过 `ComponentRegistry` 注册路由/中间件/生命周期。
- **替代路径**：`examples/reference-app/internal/app/attach.go` 与 `saasctl` 五份 selection 模板是新面样板（`go/app` 的 `Assemble`/`RunAssembly`）。
- **出处**：`f44887b6` + `477ef65b` + `904d0e92` + `5191b044` + `33ae0308` + `c02a1ad3`。

### B5. 30 号文：`PlatformConfig` 宿主镜像结构删除

- **面**：宿主配置里的 `PlatformConfig` 镜像结构删除，声明式 bootstrap key 由 loader 直接解析，宿主不再维护镜像字段。
- **替代路径**：[30 BootstrapKey schema 合成](30-bootstrap-key-schema-synthesis.md)（:66 已要求登记）；reference-app 的 `ServerConfig`/`hostConfig` 是样板。
- **出处**：`0d378f62`（`feat(app)!: resolve declared bootstrap keys without a host mirror struct`）。

## 4. C 组：F 本体（内核/模块注册表/preset 机制退役）

**出处**：`11e9b11a`（`refactor(pkgcore)!: retire the kernel, module registry and preset mechanisms`）。

### 4.1 删除的类型与符号（pkgcore）

| 删除面 | 替代路径 |
|---|---|
| `pkgcore.Kernel`、`NewKernel`、`KernelOption`、`Kernel.Bootstrap`、`Kernel.Shutdown`、`WithDeploymentMode` | 宿主建 `pkgcore.ComponentRegistry`（`pkgcore.NewComponentRegistry`），由 `app.Assemble`/`app.RunAssembly` 驱动七阶段；拓扑声明走组件组合配置的 deployment 字段 |
| `pkgcore.Module`（接口）、`Registrar`、`NewRegistry`、`Registry` | "模块契约"（`Name`/`DependsOn`/`Migrations`/`Locales`/`OpenAPISpec`/`Register(*pkgcore.ComponentRegistry) error`）为结构性约定，不再有接口类型；注册表为 `pkgcore.ComponentRegistry` |
| `SeamPreset`、`Preset`、`PresetStandalone`、`PresetDistributed`、`WithPreset` | 组件组合配置（`ComponentConfig` 的 `components` 块 + 每个组件的配置块）；八种分布式实现各自以组件名注册（`eventbus.redis`、`kv.nats`、`objectstore.s3` …） |
| `WithEventBus`、`WithKVStore`、`WithMailer`、`WithObjectStore` | `reg.Put(value)` 放入 ByType 上下文（由提供该 seam 的组件或宿主完成），能力位在组件描述符的 `Capabilities` 上声明 |
| `EventBusRegistry`、`KVStoreRegistry`、`MailerRegistry`、`ObjectStoreRegistry` | 各实现以组件描述符自注册（`pkgcore.MustRegister`，见各子包 `component.go`） |
| `ErrDuplicateModuleName`、`ErrMissingDependency` | 组件注册/依赖校验的对应错误（`pkgcore` 的组件组装阶段错误；见 `component_assembly.go`） |
| `ErrInvalidBootstrapKey`、`ErrDuplicateBootstrapKey`、`BootstrapRegistrar` | bootstrap key 声明改到组件描述符 `BootstrapKeys`（[30 号文](30-bootstrap-key-schema-synthesis.md)） |

**保留面白名单**（未删，且是本轮收口的核验对象）：泛型 `pkgcore.SeamRegistry[T]`、`pkgcore.Registration[T]`、`pkgcore.Config`、`ErrDuplicateImplementation`、`ErrUnknownImplementation`、`ErrMissingSeamConfig`、`Capability` 能力位常量及各子包导出的 `Capabilities`、各子包构造器（`kv/nats.NewKVStore` 等）、`objectstore/s3` 的 `FromConfig`、pkgcore 内存实现（memory event bus / KV / console mailer / local object store）、`componenttest` 帮手、`chain.RouteSource`，以及模块内目录注册表 helpers（pki `SignerRegistry`、ai-gateway `ChatProviderRegistry`/`ImageProviderRegistry`、billing 的 gateway 注册表——`SeamRegistry[T]` 现在的活消费者）。

### 4.2 八个分布式子包的注册面退役

- **面**：`eventbus/{redis,postgres,nats}`、`kv/{redis,postgres,nats,memcached}`、`objectstore/s3` 的 `Registration` 工厂与 `FromAddr`/（s3 除外的）`FromConfig` 一步构造器退役。
- **替代路径**：各子包构造器（如 `eventbus/redis.NewEventBus`、`kv/redis.NewKVStore`）+ 组件描述符（`component.go`，`init` 里 `pkgcore.MustRegister`）；宿主要自定义组合时按模板写自己的组件。
- **出处**：`11e9b11a` 的同一提交。

### 4.3 声明面换型（模块与 dbkit/saasctl 入参）

- **面**：模块 `Register`/`Attach` 参型 `Registrar` → `*pkgcore.ComponentRegistry`（同 §3.3，F 完成全仓替换）；`dbkit.MigrationRegistry` 与 `saasctl` 的 `migrationSet` 入参面同步换型为携带 `Migrations()` 的结构性类型。
- **替代路径**：`dbkit.NewMigrationRegistry()` + `Register(module)` 保持不变；迁移集来源面以 `Migrations() embed.FS` 结构约定。
- **出处**：`11e9b11a`。

## 5. 独立行为修正

### 5.1. `i18n.Negotiate` 改按 Accept-Language 的 q 权重排序

- **面**：`pkgcore/i18n.Negotiate`（公开导出函数）的选语言规则由"回答 header 中第一个书写且受支持的语言"改为"按 q 权重降序回答权重最高者"：缺省 q 按语法默认权重 1，权重相同保持书写顺序，`q=0` 的语言即使排在最前也被丢弃。函数签名不变。
- **消费者影响**：依赖旧"书写顺序优先"语义的调用方行为变化——`en-US;q=0.7, zh-CN;q=1` 与 `en-US;q=0.5, zh-CN` 在旧语义下都回答 en-US，现在都回答 zh-CN；调用方代码无需改动，按旧语义写死的断言需要核对。
- **替代路径**：无（同签名的行为修正）；继续调用 `i18n.Negotiate` 即得权重语义。
- **出处**：`02e148c5`（`fix(pkgcore): honor Accept-Language quality weights`）。

### 5.2. `dbkit` 迁移账本改为按组件实现的模块名存键

- **面**：装配 boot 的迁移（`dbkit.ApplyMigrations`）写入 `schema_migrations` 的账本键由承载组件的组件名改为组件实现的模块名（`Asset.Module`），与 `MigrationRegistry.Apply` 的注册键一致；该提交带 `!BREAKING` footer。
- **消费者影响**：由 boot（而非 `saasctl db migrate`）迁移过的既有库，账本行键是旧组件名（如 `reference-app.pki`、`__APP_NAME__.pki`）：新版本 boot 认不出这些行，会把 DDL 重放到既有表上。升级前删库重建，或按模块名改写账本行（出处提交正文给出 `UPDATE schema_migrations SET module='pki' WHERE module='__APP_NAME__.pki'` 示例，逐模块执行）。
- **替代路径**：无（库内键修正）；升级动作即删库重建或改写账本行。
- **出处**：`4c9464e2`（`fix(dbkit)!: key the migration ledger by the module a component implements`）。

### 5.3. jobs 新增空载荷周期任务适配器（非破坏，新增面登记）

- **面**：`go/jobs` 新增导出函数 `NewEmptyPayloadHandler(taskType, run)`——空载荷周期任务（poll/sweep/scan）的 `Handler` 适配器：拒绝非空 payload 并转调 `run`。既有导出符号无变化：各模块此前的窗口/键私有推导与私有空载荷适配器都是未导出面，删除不构成消费者可见破坏。
- **消费者影响**：无破坏、无升级动作；替换的是"拒绝非空 payload + 转调"的逐模块样板（各模块 Enqueue* 的窗口/键推导改经 `jobs.ScheduleWindowStart`/`ScheduleIdempotencyKey`/`SchedulePlatformIdempotencyKey`，键格式与窗口语义逐字节不变）。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`b8cbfd05`（`feat(jobs): add the shared empty-payload periodic handler adapter`）+ `867a58bc`（六模块改为调用共享推导与共享适配器）。

### 5.4. dbkit 新增列拟合助手 `FitColumnValue`（非破坏，新增面登记）

- **面**：`go/dbkit` 新增导出函数 `FitColumnValue(v, maxRunes) (fitted, cut)`——写入边界的列拟合：先把非法 UTF-8 连续段净化为 U+FFFD，再按 rune 截断到列宽，并报告是否发生截断。既有导出符号无变化：被替换的私有实现（dbkit/audit、jobs、sharing、integration 各自的截断副本）均为未导出面，删除不构成消费者可见破坏。
- **消费者影响**：无破坏、无升级动作；自带写入边界截断的消费者可改调该共享实现（rune 界）。按字节界的截断（metering 的 `truncateError`）是不同契约，保持模块自有；authn 的 `truncateToColumnWidth`（宽度内不做清化）同样保持原语义。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`9be64217`（`refactor(dbkit): export the write-boundary column fit`）+ `8c69f8e2`（`refactor(jobs): consolidate the column upgrades and the descriptive-text fit`）+ `0ad8822e` / `29148005`（sharing / integration 改调共享实现）。

### 5.5. pkgcore 新增可选依赖读取 `GetOptional`（非破坏，新增面登记）

- **面**：`go/pkgcore` 新增导出函数 `GetOptional[T any](r *ComponentRegistry) (T, bool, error)`——by-type 上下文的可选依赖读取：无匹配 put 值返回 `(zero, false, nil)`（缺失是事实、不是错误），恰一个匹配返回值与 `true`，其余错误（`ErrAmbiguousProvider` 包装、单匹配的转换失败）原样带回。既有导出符号与 `Get` 语义均无变化。
- **消费者影响**：无破坏、无升级动作；可选依赖消费点可改调该读取（缺失走 `ok == false` 分支、其余错误按调用方自身契约处理），与既有"缺失即跳过、其余错误上抛"写法行为一致。本轮已收敛 rbac/org/config/billing/pki/authn 六个模块组件构造与 go/app 组合配置读取共 12 处；`err == nil` 形态的"任何错误都按缺失"探测点（含 examples 与 saasctl 模板）当时维持原样，其后经语义裁定完成迁移，行为收紧见 §5.8。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`0d9d3070`（`refactor(pkgcore): add the optional-dependency reading of the by-type context`）+ `0d8d1d6e`（`refactor(rbac,org,config,billing,pki,authn,app): read optional dependencies through GetOptional`）。

### 5.6. billing：PreDeduct 补上幂等键的类型校验（行为收紧）

- **面**：`go/billing` 的 `CreditService.PreDeduct`（导出方法）在 `IdempotencyKey` 已经命名一条非 `Deduct` 类型的 `credit_transaction` 行时，由"静默把该行当作自己的既有预留返回"改为拒绝，返回编码冲突 `billing.idempotency_key_collision`（`ErrIdempotencyKeyCollision`）；键命名自身先前 Deduct 行的重试行为不变。函数签名不变。
- **消费者影响**：把 PreDeduct 的幂等键与其他类型的行（Grant、Expire、无键行的生成 UUID）撞用的调用方——键构造本身就已出错——从此收到明确冲突，而不是拿回一条并非自己预留的行；键唯一的正常调用方无感知。`Expire` 一侧的同类校验早已存在，本轮只是把两侧统一到同一核心。
- **替代路径**：无（同签名行为修正）；核对自身幂等键不跨类型复用即可。
- **出处**：`4a446745`（`refactor(billing): share the keyed deduction core of PreDeduct and Expire`）。

### 5.7. billing/gateway：alipay 公钥解析接受 X.509 证书（行为放宽登记）

- **面**：`go/billing/gateway/alipay` 的导出函数 `ParsePublicKeyPEM` 与 `Config.AlipayPublicKeyPEM` 的接受面，由"仅 PKIX 公钥 PEM"放宽为"PKIX 公钥或 X.509 证书 PEM"（证书取其中携带的 RSA 公钥），与 `wechat.ParsePublicKeyPEM` 早已接受的两种形态一致。函数签名不变；PKIX 输入的行为逐字节不变。
- **消费者影响**：无破坏、无升级动作；此前把平台公钥证书的内容填进 `alipay_public_key_pem` 会在启动时解析失败，现在按证书内公钥正常启用（与 Alipay 证书模式下"支付宝公钥证书"即平台公钥的事实一致）。
- **登记理由**：宿主可见面（配置接受面）的行为放宽，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`52c96731`（`refactor(billing/gateway): share the provider helpers in the gateway root`）。

### 5.8. 可选依赖读取全面改走 `GetOptional` 错误传播 + 交付歧义在 plan 期拒绝（行为收紧）

- **面**：两处配套的行为收紧。
  1. **读取侧**：`err == nil` 形态的"任何错误都按缺失"探测点全部迁移为 `GetOptional` 三值读法——缺失（not-found）跳过不变，`ErrAmbiguousProvider` 等真错误改为构造期上抛（原样或按站点既有包装）。迁移面共 28 处：sharing/ai-gateway/notification/integration/admin/compliance/jobs 七模块组件构造 16 处、saasctl 四变体模板的 SMSSender 接缝 4 处、reference-app host_wiring 7 处与 notes 1 处（含两处双取站点：ai-gateway 与 host_wiring 的 queue+storage）。
  2. **装配校验侧**：pkgcore 在 plan 期新增"同一单值 token 被多个选中组件交付 → 拒绝"校验（29 号文 §6.1 歧义规则的全选择集形态，原实现只在某消费方 `Requires` 锚定该 token 时才触发），报 `ErrAmbiguousProvider` 并列两个组件名，提示取消其一。
- **消费者影响**：误装配（同一 token 被两个选中组件同时交付，或宿主对同一 token 重复 `Put`）此前被静默按缺失处理——可选依赖不接、pkgcore sugar 读法取 nil；现在**构造期**（读点）与 **plan 期**（声明面）分别显式失败，即为"误装配从静默按缺失变为装配期失败"。正确装配的组合零行为变化：缺失仍走各站点文档化默认、命中仍同值。宿主直接 `Put` 的重复值不在组件声明面内、plan 不可见，仍为读时条件；pkgcore 五个 sugar 读法（`KVStore()`/`Mailer()`/`ObjectStore()`/`Locales()`/`EventBus()`）的 nil-for-absent 公共契约不变。
- **替代路径**：无（行为收紧）；按错误信息取消其一（组件选择或宿主 Put）即可修复。
- **登记理由**：宿主可见的装配契约行为收紧（此前可启动的误装配组合现在拒绝启动），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律**双轨**登记（四个收紧提交均带 `!BREAKING` footer，先例同 §5.2）。
- **出处**：`da8d6c62`（`fix(pkgcore)!: refuse a token delivered by two selected components`）+ `2ae8a125`（`refactor(sharing,ai-gateway,notification,integration,admin,compliance,jobs)!: propagate optional dependency read errors`）+ `cfa6f7e1`（`refactor(saasctl)!: propagate the optional SMS sender read in the templates`）+ `3b719310`（`refactor(reference-app)!: propagate optional dependency read errors in host wiring`）。

### 5.9. notification：联系人 SMS 的身份 locale 改为渲染实际 locale（行为修正）

- **面**：`go/notification` 的两条 contact 系 SMS 发送——投递路径（`deliverContactSMS`）与联系人验证码（`sendCode` 经 `renderContactCode`）——此前正文渲染 locale 与 seam 携带的 `SMS.Locale` 是两个来源：正文按档 locale（dispatch 捕获的请求语言 / 验证码创建与重发请求的协商语言）渲染，`SMS.Locale` 恒为平台默认（`en-US`）。现两者同源：`SMS.Locale` 即正文实际渲染 locale（生产者未捕获语言时同为平台默认 tier）。用户投递路径（`deliverUserSMS`）与 authn 验证码（`renderSMSCode` 回报的 `usedLocale`）本已同源，不变。
- **消费者影响**：模板型适配器（`pkgcore/sms/aliyun`、`pkgcore/sms/tencent`）按 `(locale, message-id)` 选择已审批模板。档 locale 非平台默认时，联系人与联系人验证码消息的模板选择从"平台默认语言模板"变为"该 locale 语言模板"——例如 zh-CN 档此前正文中文而模板取 en-US（或在该 pair 无映射时被拒）。只在平台默认语言注册过这些 message-id 模板的宿主，需为其实流量携带的其余 locale 补注册，否则消息在适配器侧发送前拒绝（fail-closed 无兜底，即既有适配器契约，本轮未新增行为）。自由文本传输（console/HTTP 网关/Twilio）忽略身份字段，不受影响。
- **替代路径**：无（同签名行为修正）；按流量语言补注册 `(locale, message-id)` 模板即可。
- **登记理由**：宿主可观测面（发送消息的模板语言与可达性）的行为修正，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（公共签名面未变，不带 footer）。
- **出处**：`51da1649`（`fix(notification): select contact SMS templates in the locale the copy rendered in`）。

### 5.10. pkgcore：typed-nil 产品与 Put 值按缺失拒绝（行为收紧）

- **面**：组件装配的三个边界统一 nil 规则。`New` 回调返回 typed-nil 指针（`(*T)(nil)`）此前穿过 `instance == nil` 检查被当作产品收下，现与返回 untyped nil 一致按"未交付产品"在构造期拒绝（`Construct` 报 `ErrComponentFailed`、`Build` 同报错）；`reg.Put((*T)(nil))` 此前静默入上下文，现与 `Put(nil)` 一致 panic。导出符号与签名不变。
- **消费者影响**：此前 New 返回 typed-nil 的组件"构造成功"，nil 指针进入按类型上下文，依赖方向的 `Get` 返回一个"存在但为 nil"的值（首个方法调用即 panic，远离装配错误现场）；现在构造期即失败并点名组件。以 typed-nil 表达"服务不可用"的误用形态从"启动后首用崩"变为"启动期拒绝"；正确返回产品的组件零行为变化。
- **替代路径**：无（行为收紧）；让 `New` 返回真实产品或 `(nil, err)`。
- **登记理由**：宿主可见的装配契约行为收紧（此前可启动的形态现在构造期拒绝），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（签名不变、非 API 破坏，不带 footer）。
- **出处**：`82823591`（`fix(pkgcore): treat a typed nil product as the absence it is`）。

### 5.11. pkgcore：整数目标字段拒收越界浮点（行为收紧）

- **面**：`ComponentConfig` 的 `Decode` / `Value[T]` 在目标为整数、配置值为浮点时，越界值此前先经 Go 的浮点→整数转换（越界结果由平台实现定义——arm64 饱和为边界值、amd64 为不定值）再对转换结果做 range check，于是 1e30 这样的值被静默接受为垃圾整数；现改为先对浮点本身做范围检查（int64 界精确用 -2^63 / 2^63、uint64 界用 2^64 的 float64 常量），越界即报 `does not fit` 并点名配置键。可表示边界照常转换（-2^63、2^62 入 int64，2^63 入 uint64），`Value`/`Decode` 文档承诺的"越界值为错误"不变、本次是让实现兑现它。
- **消费者影响**：把整数键写成越界浮点（如 `retries: 1e30`）的宿主此前以垃圾整数启动，现在 `Load`/`Decode` 失败并点名键；范围内数值配置零行为变化。
- **替代路径**：无（行为收紧）；把配置值改回目标整数范围内。
- **登记理由**：宿主可见的配置接受面行为收紧，同 §5.10（签名不变，不带 footer）。
- **出处**：`c663f610`（`fix(pkgcore): refuse out-of-range floats before converting to integers`）。

## 6. D 组：configrefgen 工具内部迁移（工具面，非宿主面）

- **面**：`tools/configrefgen` 的内部实现从旧内核面迁到组件面（工具自身不在消费者依赖面内）。
- **登记理由**：它是仓内唯一以"生成物一致性"为门禁的工具（文档检查流水线 `--check` 四产物），其迁移已完成并全绿；消费者无需动作。
- **出处**：`23bbb3a9`（`refactor(tools): resolve the configuration reference through the component assembly`）。

## 7. E 组：e2e stub 缺口（状态登记）

- **状态**：e2e CI 腿已由 stub 改为真腿（`.github/workflows/e2e.yml`，含诚实门），原文"e2e 仍是 stub"的表述已清扫。
- **出处**：e2e 轮提交 `e5ec05b9`（及同轮 §2.1 的四句表述清扫）。
- **说明**：本轮只登记状态、不改其文件；若该轮先于本清单落地，此条即为已闭记录。

## 8. 附录：2026-09 以来全部 `!` 提交

以下为 `git log --grep '!:'`（2026-09-01 起，含本清单所在收口轮）的完整清单，供 release note 逐条改写使用：

| sha | 一句话 |
|---|---|
| `a92adfb6` | 三条 integration 腿与三处运行时文案迁到组件面（本轮） |
| `4c9464e2` | dbkit 迁移账本按组件实现的模块名存键（§5.2） |
| `11e9b11a` | 内核/module 注册表/preset 机制退役（F 本体，§4） |
| `c02a1ad3` | app 过渡组装面退役（23 符号，§3.2） |
| `0d378f62` | bootstrap key 声明不再需要宿主镜像结构（§3.5） |
| `ad99ab2b` | 模块声明面换型为 Registrar view（§3.3） |
| `336e14fe` | app option 面经组件机制组装（§3.1） |
| `e4cdb1b0` | app 预引擎 serve helper 退役（§2.2） |
| `1e6a846c` | 腾讯云 SMS 模板按 locale/message id 映射 |
| `2d95a45f` | 阿里云 SMS 模板按 locale/message id 映射 |
| `5252ec95` | SMS seam 携带 message identity/locale/params |
| `f18aa07e` | MountedRoute 上声明路由授权 |
| `c7aeafd9` | 配置经 preset 通道携带 |
| `5cb0ce68` | 预认证 config 端点加固与版本化 |
| `349f2881` | integration webhook SSRF override seam 导出 |
| `936580e0` | org 增加 TreeService.Restore / MemberService.Restore |
| `0c1a47bb` | compliance 数据导出经 go/sharing 交付 |
| `b215eb12` | authn 签名密钥经 pki.Service（KeySource） |
| `8297ee77` | observability OTLP/Prometheus 拆子包、均改为显式开启 |
| `5663f5cb` | observability.Init 去掉 deployment mode 参数 |
| `8820c349` | pkgcore 部署模式开关换成能力位校验 |
| `28c5f16d` | jobs.DemoQueue 改名 StandaloneQueue |
| `1432d1fd` | pkgcore.Profile 改名 DeploymentMode |

注：另有一个同题重写版提交 `6fb6785c`（提交消息与 `11e9b11a` 逐字相同；其补丁仅扫入一个 29MB 的 `tools/configrefgen/configrefgen` 构建产物二进制，该二进制其后已从树中移除、不再跟踪），不单列于此表。

## 9. 挂起项

- **29 §9 目录式组件收编**：把模块内目录注册表（pki signer、ai-gateway provider、billing gateway）统一收编进组件机制的设想**未落地**，保持开放项登记，不在本清单的破坏面内。
- **pkgcore 注释面死引用（残留登记）**：`register.go` 悬空引用 52 处/33 文件、flat adapter 6 处，与 F-2 轮登记的 129 处/47 文件属同一类别，合并为一个子项，输入 R18 清扫收尾批；纯注释面，不在本清单的破坏面内。
- **首个可用版本**：`task release:plan` 的离线计划（02 号文）在首个版本发布时以本清单生成 release note；发布后本清单转为历史记录，后续破坏面另起新篇。
