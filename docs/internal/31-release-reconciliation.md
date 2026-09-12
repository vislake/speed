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

（后续演进：该白名单中的泛型 `SeamRegistry[T]`/`Registration[T]` 与两个哨兵、以及三族模块内目录注册表已由收编阶段 3 移除；`pkgcore.Config`/`ErrMissingSeamConfig` 保留并迁入 `flat_config.go`。见 §5.26（收编阶段 3）段。）

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
- **出处**：`abe5e5bc`（`refactor(dbkit): export the write-boundary column fit`）+ `a8c8023f`（`refactor(jobs): consolidate the column upgrades and the descriptive-text fit`）+ `1494bf2f` / `9ecb2423`（sharing / integration 改调共享实现）。

### 5.5. pkgcore 新增可选依赖读取 `GetOptional`（非破坏，新增面登记）

- **面**：`go/pkgcore` 新增导出函数 `GetOptional[T any](r *ComponentRegistry) (T, bool, error)`——by-type 上下文的可选依赖读取：无匹配 put 值返回 `(zero, false, nil)`（缺失是事实、不是错误），恰一个匹配返回值与 `true`，其余错误（`ErrAmbiguousProvider` 包装、单匹配的转换失败）原样带回。既有导出符号与 `Get` 语义均无变化。
- **消费者影响**：无破坏、无升级动作；可选依赖消费点可改调该读取（缺失走 `ok == false` 分支、其余错误按调用方自身契约处理），与既有"缺失即跳过、其余错误上抛"写法行为一致。本轮已收敛 rbac/org/config/billing/pki/authn 六个模块组件构造与 go/app 组合配置读取共 12 处；`err == nil` 形态的"任何错误都按缺失"探测点（含 examples 与 saasctl 模板）当时维持原样，其后经语义裁定完成迁移，行为收紧见 §5.8。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`578ca10e`（`refactor(pkgcore): add the optional-dependency reading of the by-type context`）+ `4784d92a`（`refactor(rbac,org,config,billing,pki,authn,app): read optional dependencies through GetOptional`）。

### 5.6. billing：PreDeduct 补上幂等键的类型校验（行为收紧）

- **面**：`go/billing` 的 `CreditService.PreDeduct`（导出方法）在 `IdempotencyKey` 已经命名一条非 `Deduct` 类型的 `credit_transaction` 行时，由"静默把该行当作自己的既有预留返回"改为拒绝，返回编码冲突 `billing.idempotency_key_collision`（`ErrIdempotencyKeyCollision`）；键命名自身先前 Deduct 行的重试行为不变。函数签名不变。
- **消费者影响**：把 PreDeduct 的幂等键与其他类型的行（Grant、Expire、无键行的生成 UUID）撞用的调用方——键构造本身就已出错——从此收到明确冲突，而不是拿回一条并非自己预留的行；键唯一的正常调用方无感知。`Expire` 一侧的同类校验早已存在，本轮只是把两侧统一到同一核心。
- **替代路径**：无（同签名行为修正）；核对自身幂等键不跨类型复用即可。
- **出处**：`0217045a`（`refactor(billing): share the keyed deduction core of PreDeduct and Expire`）。

### 5.7. billing/gateway：alipay 公钥解析接受 X.509 证书（行为放宽登记）

- **面**：`go/billing/gateway/alipay` 的导出函数 `ParsePublicKeyPEM` 与 `Config.AlipayPublicKeyPEM` 的接受面，由"仅 PKIX 公钥 PEM"放宽为"PKIX 公钥或 X.509 证书 PEM"（证书取其中携带的 RSA 公钥），与 `wechat.ParsePublicKeyPEM` 早已接受的两种形态一致。函数签名不变；PKIX 输入的行为逐字节不变。
- **消费者影响**：无破坏、无升级动作；此前把平台公钥证书的内容填进 `alipay_public_key_pem` 会在启动时解析失败，现在按证书内公钥正常启用（与 Alipay 证书模式下"支付宝公钥证书"即平台公钥的事实一致）。
- **登记理由**：宿主可见面（配置接受面）的行为放宽，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`0904e8e4`（`refactor(billing/gateway): share the provider helpers in the gateway root`）。

### 5.8. 可选依赖读取全面改走 `GetOptional` 错误传播 + 交付歧义在 plan 期拒绝（行为收紧）

- **面**：两处配套的行为收紧。
  1. **读取侧**：`err == nil` 形态的"任何错误都按缺失"探测点全部迁移为 `GetOptional` 三值读法——缺失（not-found）跳过不变，`ErrAmbiguousProvider` 等真错误改为构造期上抛（原样或按站点既有包装）。迁移面共 28 处：sharing/ai-gateway/notification/integration/admin/compliance/jobs 七模块组件构造 16 处、saasctl 四变体模板的 SMSSender 接缝 4 处、reference-app host_wiring 7 处与 notes 1 处（含两处双取站点：ai-gateway 与 host_wiring 的 queue+storage）。
  2. **装配校验侧**：pkgcore 在 plan 期新增"同一单值 token 被多个选中组件交付 → 拒绝"校验（29 号文 §6.1 歧义规则的全选择集形态，原实现只在某消费方 `Requires` 锚定该 token 时才触发），报 `ErrAmbiguousProvider` 并列两个组件名，提示取消其一。
- **消费者影响**：误装配（同一 token 被两个选中组件同时交付，或宿主对同一 token 重复 `Put`）此前被静默按缺失处理——可选依赖不接、pkgcore sugar 读法取 nil；现在**构造期**（读点）与 **plan 期**（声明面）分别显式失败，即为"误装配从静默按缺失变为装配期失败"。正确装配的组合零行为变化：缺失仍走各站点文档化默认、命中仍同值。宿主直接 `Put` 的重复值不在组件声明面内、plan 不可见，仍为读时条件；pkgcore 五个 sugar 读法（`KVStore()`/`Mailer()`/`ObjectStore()`/`Locales()`/`EventBus()`）的 nil-for-absent 公共契约不变。
- **替代路径**：无（行为收紧）；按错误信息取消其一（组件选择或宿主 Put）即可修复。
- **登记理由**：宿主可见的装配契约行为收紧（此前可启动的误装配组合现在拒绝启动），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律**双轨**登记（四个收紧提交均带 `!BREAKING` footer，先例同 §5.2）。
- **出处**：`453ba35e`（`fix(pkgcore)!: refuse a token delivered by two selected components`）+ `69651edb`（`refactor(sharing,ai-gateway,notification,integration,admin,compliance,jobs)!: propagate optional dependency read errors`）+ `760a73db`（`refactor(saasctl)!: propagate the optional SMS sender read in the templates`）+ `58ffd7e3`（`refactor(reference-app)!: propagate optional dependency read errors in host wiring`）。

### 5.9. notification：联系人 SMS 的身份 locale 改为渲染实际 locale（行为修正）

- **面**：`go/notification` 的两条 contact 系 SMS 发送——投递路径（`deliverContactSMS`）与联系人验证码（`sendCode` 经 `renderContactCode`）——此前正文渲染 locale 与 seam 携带的 `SMS.Locale` 是两个来源：正文按档 locale（dispatch 捕获的请求语言 / 验证码创建与重发请求的协商语言）渲染，`SMS.Locale` 恒为平台默认（`en-US`）。现两者同源：`SMS.Locale` 即正文实际渲染 locale（生产者未捕获语言时同为平台默认 tier）。用户投递路径（`deliverUserSMS`）与 authn 验证码（`renderSMSCode` 回报的 `usedLocale`）本已同源，不变。
- **消费者影响**：模板型适配器（`pkgcore/sms/aliyun`、`pkgcore/sms/tencent`）按 `(locale, message-id)` 选择已审批模板。档 locale 非平台默认时，联系人与联系人验证码消息的模板选择从"平台默认语言模板"变为"该 locale 语言模板"——例如 zh-CN 档此前正文中文而模板取 en-US（或在该 pair 无映射时被拒）。只在平台默认语言注册过这些 message-id 模板的宿主，需为其实流量携带的其余 locale 补注册，否则消息在适配器侧发送前拒绝（fail-closed 无兜底，即既有适配器契约，本轮未新增行为）。自由文本传输（console/HTTP 网关/Twilio）忽略身份字段，不受影响。
- **替代路径**：无（同签名行为修正）；按流量语言补注册 `(locale, message-id)` 模板即可。
- **登记理由**：宿主可观测面（发送消息的模板语言与可达性）的行为修正，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（公共签名面未变，不带 footer）。
- **出处**：`f29618b9`（`fix(notification): select contact SMS templates in the locale the copy rendered in`）。

### 5.10. pkgcore：typed-nil 产品与 Put 值按缺失拒绝（行为收紧）

- **面**：组件装配的三个边界统一 nil 规则。`New` 回调返回 typed-nil 指针（`(*T)(nil)`）此前穿过 `instance == nil` 检查被当作产品收下，现与返回 untyped nil 一致按"未交付产品"在构造期拒绝（`Construct` 报 `ErrComponentFailed`、`Build` 同报错）；`reg.Put((*T)(nil))` 此前静默入上下文，现与 `Put(nil)` 一致 panic。导出符号与签名不变。
- **消费者影响**：此前 New 返回 typed-nil 的组件"构造成功"，nil 指针进入按类型上下文，依赖方向的 `Get` 返回一个"存在但为 nil"的值（首个方法调用即 panic，远离装配错误现场）；现在构造期即失败并点名组件。以 typed-nil 表达"服务不可用"的误用形态从"启动后首用崩"变为"启动期拒绝"；正确返回产品的组件零行为变化。
- **替代路径**：无（行为收紧）；让 `New` 返回真实产品或 `(nil, err)`。
- **登记理由**：宿主可见的装配契约行为收紧（此前可启动的形态现在构造期拒绝），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（签名不变、非 API 破坏，不带 footer）。
- **出处**：`25e1a77d`（`fix(pkgcore): treat a typed nil product as the absence it is`）。

### 5.11. pkgcore：整数目标字段拒收越界浮点（行为收紧）

- **面**：`ComponentConfig` 的 `Decode` / `Value[T]` 在目标为整数、配置值为浮点时，越界值此前先经 Go 的浮点→整数转换（越界结果由平台实现定义——arm64 饱和为边界值、amd64 为不定值）再对转换结果做 range check，于是 1e30 这样的值被静默接受为垃圾整数；现改为先对浮点本身做范围检查（int64 界精确用 -2^63 / 2^63、uint64 界用 2^64 的 float64 常量），越界即报 `does not fit` 并点名配置键。可表示边界照常转换（-2^63、2^62 入 int64，2^63 入 uint64），`Value`/`Decode` 文档承诺的"越界值为错误"不变、本次是让实现兑现它。
- **消费者影响**：把整数键写成越界浮点（如 `retries: 1e30`）的宿主此前以垃圾整数启动，现在 `Load`/`Decode` 失败并点名键；范围内数值配置零行为变化。
- **替代路径**：无（行为收紧）；把配置值改回目标整数范围内。
- **登记理由**：宿主可见的配置接受面行为收紧，同 §5.10（签名不变，不带 footer）。
- **出处**：`6e20f187`（`fix(pkgcore): refuse out-of-range floats before converting to integers`）。

### 5.12. admin 新增账本全量读取 `TenantService.ListAllRows`（非破坏，新增面登记）

- **面**：`go/admin` 新增导出方法 `TenantService.ListAllRows(ctx) ([]Tenant, error)`——租户账本的全量读取：分页走完 `TenantRepository.List` 的全部游标页，而不是发一次调用（单次调用在上限处静默截断）。既有导出符号无变化：分页读取本身自 `ListAllIDs`（§5.13）起就在单处，散在各跨租户读取内的是各调用方手写的 per-tenant 授权循环（未导出面），收敛进统一 walk 不构成消费者可见破坏。
- **消费者影响**：无破坏、无升级动作；本变更带来的是逐页走账的拼装收敛与用量看板 `Summary` 的重读消除——用量看板、成员资格组合与发送记录检索此前各自手写同样的 per-tenant 授权循环（逐租户进入已审计的 system-context、首个失败即中止），现收敛进 `TenantService.forEachLedgerTenant` 统一 walk（未导出面）；`Summary` 改从同一份账本清单直读每行的 display name，逐行的单行重读就此消除——单行读失败不再可能把行的名字置空。各跨租户读取的完整性（全量账本）自 2026-09-05 起即已由该日引入的分页读取（`afd4997f` 的 `ListAllIDs`，见 §5.13）保证，本变更未触及；授权、顺序与错误传播语义均不变。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`68bd691b`（`refactor(admin): fan cross-tenant ledger reads out through one walk`）。

### 5.13. admin 新增账本全量 ID 读取 `TenantService.ListAllIDs`（非破坏，新增面登记）

- **面**：`go/admin` 新增导出方法 `TenantService.ListAllIDs(ctx) ([]string, error)`——租户账本全部租户 ID 的全量读取：分页走完 `TenantRepository.List` 的全部游标页，而不是发一次调用（单次调用在上限处静默截断）。引入时 `SearchService.MembershipsOf` 与 `AuditService.Query` 的候选租户列表正取自被上限截断的单次 `List` 调用（账本超过一页即静默丢弃其后各行），两处随之改用本方法取得全量。其后 `68bd691b` 把实现改为 `ListAllRows` 的 ID 投影（§5.12），签名与语义不变。
- **消费者影响**：无破坏、无升级动作；需要"平台已知全部租户"ID 面作为候选列表的跨租户读取可直接调用本方法，一次调用即得全量。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（纯新增面，不带 footer）。
- **出处**：`afd4997f`（`fix(admin): refuse system-domain impersonation targets and page the full tenant ledger`）+ `68bd691b`（`refactor(admin): fan cross-tenant ledger reads out through one walk`；改为 `ListAllRows` 投影）。

### 5.14. Mailer 地址结构预校验：`Send` 拒绝不可寻址的地址（行为收紧）

- **面**：`pkgcore` 的共享校验 `validateMail`（`mailer_validation.go`，console 与 SMTP 两实现共用）在既有"空值/控制字符"规则之外新增结构规则：From、每个 To 项与可选 ReplyTo 必须各自经 `net/mail.ParseAddress` 解析为**一个**地址，否则以既有 `ErrInvalidMail` 拒绝（错误只点名字段，不回显地址，也不携带解析器的错误文本——`net/mail` 的解析错误会引用输入片段）。
- **消费者影响**：此前不是地址形态的取值（裸字符串、缺 `@`、空本地部/域名、含空白、一个字段塞两个地址、点位异常如 `a..b@example.com`）会经 `Send` 直达传输层——SMTP 面上原样进入 `MAIL FROM`/`RCPT TO` 与原始头，现改为**拨号/打印前**失败（`ErrInvalidMail`）。接受面不变：展示名形式（`Ada <ada@example.com>`）、加号标签、非 ASCII/IDN 本地部与域名、无点域名等一切 `net/mail.ParseAddress` 接受的形式继续放行；org 邀请与 notification 外部联系人各自的入口门（ASCII、域名含点等）不受影响，但两者都放行而解析器拒绝的点位异常形态（如 `a..b@example.com`）改为本地失败而非交给中继。`mailertest.AssertConforms` 同步要求每个 Mailer 实现（含宿主自带实现）满足该规则——此前通过套件的宿主实现需一并收紧。
- **替代路径**：无（行为收紧）；调用方修正地址本身即可。
- **登记理由**：宿主/API 调用面可见的契约行为收紧（`Mailer.Send` 的接受面变化 + 契约套件新增必过用例），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律**双轨**登记（提交带 `!BREAKING` footer，先例同 §5.2/§5.8）。
- **出处**：`1267ce1c`（`fix(pkgcore)!: structurally validate addresses before Send`）。

### 5.15. authn + metering：已声明的动态配置行从死声明变为生效开关（非破坏，宿主可见面登记）

- **面**：`go/authn` 与 `go/metering` 在 `ConfigSeat` 上声明的动态配置项由"仅入 schema、无任何读点"变为运行期生效；两模块各新增导出缝 `SettingsReader` / `WithSettingsReader`，`go/config` 的 `Handle` 新增类型化惰性读 `Duration`/`Int`/`String`（均为新增面，无导出符号删除）。authn 的 19 项——`authn.password_min_length` / `password_max_length`、`access_token_ttl` / `refresh_token_ttl` / `session_ttl` / `oauth_state_ttl` / `sms_code_ttl` / `sms_code_max_attempts`、`social.trusted_providers`、五个社交渠道的 `client_id` / `client_secret`——在各自消费操作处读取（签入、改密、铸造、发码、OAuth 流程）；metering 的 `metering.period_bucket_size` 与 `metering.default_overage_threshold` 在每次折叠处读取。读规则：显式行优先；未设行 / 未接缝 / 读失败回落构造期选项，故无 config 模块的组合与历史行为一致。两处例外：`authn.social.trusted_providers` 读失败 fail-closed 回落到空列表（不回落构造值，避免复活已被显式关闭的自动关联），显式空行即"不信任任何渠道"；`authn.social.<channel>.client_id` / `client_secret` 须成对显式设置，否则该渠道保持构造期凭据。装配面：两模块的组件描述符在 config 组件在场时自接线其 `Handle`（可选依赖，authn/metering 的 `go.mod` 因此直接 require `go/config`），宿主手接仍走 `WithSettingsReader(configModule.Handle())`；仓内两宿主已完成手接——reference-app 的 authn 覆盖组件，与 saasctl 四份含 authn 的选择变体模板的 authn 覆盖组件（新增 pki 侧同形的构造依赖边，保证读 config 产品有序），宿主级 pin 为「装配后经真实 `Set` 路径写入的 `authn.password_min_length` 系统行决定 Register 的密码策略」。访问令牌 TTL 是唯一一处"首次使用时解析并冻结"（它决定签名密钥生命周期），其余项随操作即时生效。
- **消费者影响**：对已写入这些配置行的宿主，行的语义从"记录在案"变为"控制行为"——如已设的 `authn.password_min_length` 会开始拒绝更短的改密请求、`authn.social.trusted_providers` 会开始决定自动关联白名单、`metering.period_bucket_size` 会改变实时计数与摘要行的分桶。宿主代码无需改动；带 config 组件的装配自动生效，不带则完全无感（回落构造期值）。既有导出符号与签名不变。
- **替代路径**：无（行为修正）；要保持构造期行为时不写这些键的显式行（未设行即回落），或不装配 config 组件。
- **登记理由**：宿主可见面（已设配置行由死声明变为生效开关）的行为修正，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`87f31b0d`（`feat(config): add typed lazy reads to the read handle`）+ `2fdb9c9d`（`feat(authn): make the declared dynamic config items effective at runtime`）+ `f3b8051b`（`feat(metering): read the declared config items through a settings seam`）+ `e1679e70`（`feat(reference-app): wire the config handle as authn's settings reader`）+ `a326e95c`（`feat(saasctl): wire the config handle as authn's settings reader in the templates`）。

### 5.16. pki：配置声明项接真实读点（行为变更登记）

- **面**：`go/pki` 在 `ConfigSeat` 上声明的 8 项动态配置由"仅入 schema、无读点"变为运行期生效；模块新增导出缝 `SettingsReader` / `WithSettingsReader`（新增面，无导出符号删除），组件描述符新增可选 `(*config.Module)` 依赖并在 config 组件在场时自接线其 `Handle`（`go/pki` 的 `go.mod` 因此直接 require `go/config` 并补 replace）。读点与语义：`pki.propagation_window` 与 `pki.renewal_lead_time` 在 `ScanExpiry`/`PromoteNow` 的解析链（调用参数 → 显式行 → 构造期值 → 包常量）；`pki.crl_validity` 在 `GenerateCRL` 自身 validity<=0 的回落处（`RegenerateAllCRLs` 内部传 0 的路径随之生效）；`pki.crl_distribution_point` 在 `CreateRootCA`/`CreateIntermediateCA` 写行前补齐 caller 的空值（仅影响新建 authority，存量行不重写）；`pki.ca_default_validity`/`pki.ca_max_validity` 与 `pki.certificate_default_validity`/`pki.certificate_max_validity` 在签发路径补齐两套保护语义——`NotAfter` 零值回落对应 default（不再视为残缺输入），超过对应 max 的请求**钳制到上限**（贴合声明原文 "regardless of what the caller requests"）。读规则与 authn/metering 两项同形：显式行优先；未设行 / 未接缝 / 读失败回落构造期值或包常量（包常量即 schema 默认值本身）。
- **消费者影响**：对已写入这些配置行的宿主，行的语义从"记录在案"变为"控制行为"——已设 `pki.crl_validity` 会开始改变生成 CRL 的有效窗口，已设 `pki.ca_max_validity`/`pki.certificate_max_validity` 会开始钳制超限的签发请求，已设 `pki.crl_distribution_point` 会开始填入新建 authority。宿主代码无需改动；带 config 组件的装配自动生效（reference-app 的组合选中 config 组件，无 pki 覆盖组件，无需任何宿主接线），不带 config 组件的组合对这些行完全无感。与行无关的签发语义收窄对**所有**宿主生效：零值 `NotAfter` 现在回落包默认（CA 10 年 / 端实体 1 年）而非直通一张无效证书，超限请求钳制到包默认上限（15 年 / 2 年）——一切显式且未超限的请求不变（reference-app 的 root 10 年、intermediate 5 年均在上限内，无行为变化）。
- **替代路径**：无（行为修正）；不装配 config 组件可让动态行无感，但零值回落与上限钳制是签发路径的包默认语义，无法关闭——需要更长上限的宿主须装配 config 组件并显式设置对应的 max 行。
- **登记理由**：宿主可见面（已设配置行由死声明变为生效开关 + 签发语义收窄）的行为变更，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`491d36bf`（`feat(pki): read the declared config items at their read points`）+ `5cdc071e`（`test(pki): pin the config-item read points end to end`）+ `2745abf5`（`docs(pki): describe the configuration read points in the module guide`）。

### 5.17. go/app：RunAssembly 增加可选 serve 回调（公共面新增登记）

- **面**：`go/app` 的 `RunAssembly` 新增第三个参数——可选 serve 回调 `ServeFunc`（新导出类型，`func(ctx context.Context, reg *pkgcore.ComponentRegistry) error`）：引擎在装配 Start 完成后、两相关停（Stop→Close）之前调用它一次，把信号叠加后的 context 与活注册表交给宿主；回调返回即结束 serve 阶段，引擎随后照常两拍关闭。`nil` 回调保持原语义——引擎自己等 context（无 serve 步骤的进程，即签名变更前的行为）；serve 返回错误不跳过关闭，错误与关闭结果 join 后一并带回调用方。调用形态由 `RunAssembly(ctx, spec, extra...)` 变为 `RunAssembly(ctx, spec, serve, extra...)`。
- **消费者影响**：既有调用方在 spec 之后补 `nil` 即保持原行为；有自身服务生命周期的宿主把 serve 步骤填入新参数，不再手抄 signal+两相关停循环。仓内两宿主已迁移：reference-app 的 `Run` 与 saasctl 五份 selection 模板——宿主各自在自己的注册表上读描述符构建组件集，连 serve 步骤一起交给引擎组装（`examples/reference-app/internal/app/server.go` 的 `Run`/`serveHost`，五份 `selection/*/server.go` 的 `runServer`/`serveHost`）。`tools/check_host_composition.py` 的 `signal.NotifyContext` 宿主允许点随之退役：信号叠加归引擎，该禁令现无允许点。
- **替代路径**：`app.RunAssembly`（nil 回调=原签名行为）；主动驱动注册表、自持 serve 的宿主继续用 `Assemble` + `Shutdown`。
- **登记理由**：公共符号面变更（`RunAssembly` 签名新增参数 + 新导出类型 `ServeFunc`），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律**双轨**登记（三个提交均带 `!` footer，先例同 §5.8）。
- **出处**：`aaea1cc7`（`feat(app)!: hand the host's serve step to RunAssembly`）+ `a25b73f9`（`refactor(saasctl)!: hand the skeleton's serve step to RunAssembly`）+ `c5f2b9ae`（`refactor(reference-app)!: run the app through RunAssembly's serve step`）。

### 5.18. pkgcore：新增 httpapi 响应编码帮手（非破坏，新增面登记）

- **面**：`pkgcore/httpapi` 新增导出 `WriteJSON(w, status, v)` 与常量 `JSONContentType`（原包内未导出的 JSON media type 常量转正）：`WriteJSON` 把一次 JSON 成功响应写入收敛为一处——共享 Content-Type、调用方给定的状态码、`json.Encoder` 编码（带结尾换行）；编码失败在状态行已写出后丢弃（断连客户端，非响应缺陷）。包内 `WriteError` 改用同一导出常量。既有导出符号与签名不变。
- **消费者影响**：无破坏、无升级动作；仓内各模块（admin、ai-gateway、authn、billing、config、integration、notification、org、pki、sharing、storage 与 reference-app 的 notes 面）此前的本地 `writeJSON` 帮手与 `jsonContentType` 常量已删除、调用点改走 `httpapi.WriteJSON`，线上字节逐字不变；新模块脚手架的生成物也改走 `httpapi.WriteJSON` / `httpapi.WriteError`（响应字节等价）。宿主自带处理器可直接调用新导出面。
- **登记理由**：公共符号面新增，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（本轮为纯新增，不带 footer）。
- **出处**：`16ccace8`（`feat(pkgcore): add the shared JSON response writer to httpapi`）+ `c2b79705`（`refactor: write the modules' JSON responses through pkgcore/httpapi`）+ `2a0ca108`（`refactor(reference-app): write the notes responses through httpapi.WriteJSON`）+ `db6fedcf`（`refactor(tools): emit the shared JSON helpers from the new-module scaffold`）。

### 5.19. authn：SMS 正文渲染改经宿主合并目录（组装边界登记）

- **面**：`go/authn` 的手机验证码短信正文由"模块自带 TOML 目录（私有解析 + `BurntSushi/toml` 直接依赖）"改为经宿主合并目录渲染（`pkgcore/i18n` 机制；`Module.Register` 自动把注册表挂为服务的宿主缝，调用期读取）。渲染结果等价：同 key 同文案，占位符仍由 code/minutes 两个参数插值，落单语言仍回落 `DefaultLocale` 并在短信 seam 上报告实际渲染 locale（承接 §5.9 的 seam 语义，不变）。导出符号与签名无变化；`go/authn` 的 `go.mod` 中 `BurntSushi/toml` 由直接依赖降为 `// indirect`（经 `pkgcore`，`pkgcore/i18n` 才是同一批文件的解析者）。
- **消费者影响**：凡经 `NewModule` + `Register` 装配的宿主（含 reference-app 与 saasctl 各含 authn 的模板工程）零行为变化。边界：直接以 `NewService` 组装、不经注册表挂载宿主缝、又自行服务短信发码路径的宿主，渲染失败与其它渲染失败同路（记日志、按成功应答），不再从模块自带文件兜底——此类宿主照 `Module.Register` 的做法挂载注册表即可恢复（`go/authn/AGENTS.md` 的 `NewService` 行已写明该要求）。
- **替代路径**：无（行为修正）；走 `Module.Register` 即自动生效。
- **登记理由**：组装契约边界（直接构造路径新增宿主缝挂载要求）的登记，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（装配宿主零行为变化，不带 footer）。
- **出处**：`4688506d`（`refactor(authn): render the SMS body from the host's merged i18n catalog`）+ `b5724ade`（`test(authn): pin the catalog render and run the locale census through the catalog`）。

### 5.20. （收编阶段 1）成员组件化与组件面能力位读法（非破坏，新增面登记）

- **面**：四族既有包级 `pkgcore.SeamRegistry` 的实现目录新增组件面（描述符均为纯新增，旧 SeamRegistry 注册与解析路径原样保留）。`pkgcore` 新增导出读法 `ComponentCapabilities(reg *ComponentRegistry, name string) (Capability, error)`：按装配计划读取**被选中**组件描述符声明的 `Capabilities`——未选中 / 未注册分别以 `ErrUnknownComponent` 报出（与 `Build` 的两个错误分支同文），零声明读作零值；`Build[T]` 签名不变，该读法即"组件面拿能力位、`Build` 只出产品"的配对读法。`go/billing/gateway/{stripe,alipay,wechat}` 各新增组件描述符，名字与注册名逐字一致（`gateway.stripe` / `gateway.alipay` / `gateway.wechat`，模块 `gateway`），组合块键集与扁平 `pkgcore.Config` 相同，两面共用同一构造路径（`gatewayFromConfig` → `NewGateway`）。`go/ai-gateway` 新增 `chat.openai-compatible` / `image.openai-compatible` 组件（模块 `chat` / `image`）：route 的 provider 逻辑名即组件名（与凭据解析键、注册键同一字符串），"route 逻辑名 → 组件名"的解析为模块内恒等并由名字与能力位的对照测试钉住——宿主既有 route 字符串与凭据行零迁移；`Build[T]` 的 override 参数形态即"每次请求按已解析凭据构造 provider"的组件面形状。`go/pki/signer/{vault,kmsaws}` 各新增两个组件（`signer.vault` / `signer.vault-direct`、`signer.aws-kms` / `signer.aws-kms-direct`），与既有 `signer.local` 并列；能力位与注册声明逐一对齐（envelope 模式与 `signer.local` 声明 0，两个 `-direct` 声明 `pkgcore.KeyNeverLeavesBoundary`）。
- **消费者影响**：无破坏、无升级动作。新名字默认不选中，不选入组合即零影响；选入时按普通组件参与装配（含部署模式的能力位校验）。双轨并存由各族对照测试钉住：同一名字的两面构造同一实现、声明同一能力位、以同一方式拒绝缺配置；pki 侧另钉住组件面能力位读法与 `BuildSignerRequiring` 的注册面比较结论一致。`ComponentCapabilities` 为纯新增导出符号，`Build[T]` 与各 `SeamRegistry` 方法签名均不变。
- **替代路径**：无（纯新增面）。
- **登记理由**：宿主可见面新增（新的可选组件名 + pkgcore 导出读法），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（纯新增面，不带 footer）。
- **出处**：`c3915cf7`（`feat(pkgcore): read a selected component's capabilities without building it`）+ `a2d1873f`（`feat(billing): add component descriptors for the three payment gateways`）+ `684f6ba3`（`feat(ai-gateway): add component descriptors for the default chat and image providers`）+ `0dadae09`（`feat(pki): add component descriptors for the vault and kmsaws signers`）。

### 5.21. pkgcore：组件关闭阶段直接释放带 `Close` 的产品（描述符可省适配回调）（非破坏，组装契约登记）

- **面**：`ComponentRegistry` 第七阶段（Close）的释放规则放宽。描述符未声明 `Close` 回调的组件，其构造产品若实现 `Close() error`（标准 `io.Closer`），由注册表直接调用释放（`closeEntries` 的 `io.Closer` 探测）；描述符声明了 `Close` 回调的组件行为完全不变（回调独占释放，产品自身的 `Close` 不被叠加）；产品无 `Close` 方法则跳过。回滚集合（`rolled back:` 名单）与关闭失败聚合的错误文本对两类释放一视同仁，未变。同时八个内置分布式实现（`eventbus/{redis,postgres,nats}`、`kv/{redis,postgres,nats,memcached}`、`objectstore.local`）描述符上逐字复制的适配回调删除，每个可关闭产品类型旁改以编译期 `var _ io.Closer = (*T)(nil)` 断言钉住声明。
- **消费者影响**：宿主代码无需改动。对"产品实现 `Close() error` 但描述符未声明 `Close` 回调"的宿主组件，其产品现在会在装配关闭（含构造后失败的回滚）时被释放——此前不会；这正是 `go/pkgcore/AGENTS.md` 一直记载的契约（"声明所有权的方式是在 `New` 返回的值上实现 `Close() error`"），本次是机制对齐文档。内置实现的产品释放行为与删除适配回调前逐项等价（同一 `Close` 方法、同一逆序、同一失败聚合与 `rolled back` 名单）。唯一的语义约束：产品类型上的 `Close() error` 即"装配关闭时释放我"的声明，语义不符的产品不应以该形态暴露（`io.Closer` 的标准含义即释放所拥有的资源）。
- **替代路径**：无（行为放宽）；需要 context / registry / 自定义拆解的释放继续声明描述符 `Close` 回调（优先级不变）。
- **登记理由**：宿主可见面（组装契约：关闭阶段多释放一类产品）的行为变更，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`5fd9c801`（`feat(pkgcore): release constructed products through their own Close() error`）+ `b671031c`（`refactor(pkgcore): drop the duplicated close adapters from the built-in descriptors`）+ `7af0a87f`（`test(pkgcore): keep the kv.redis release probe inside its implementing package`）。

### 5.22. org：`org.invitation_email` 的声明默认改随宿主接线（行为修正）

- **面**：`go/org` 的 `org.invitation_email` 声明默认此前硬编码为 true，与 `WithInvitationEmailDisabled`（语义="该宿主未配置邮件发送设施"）相悖。现 `Register` 按模块自身的 `emailEnabled` 状态构造声明（`featureFlagDecls(emailEnabled)`，`go/org/module.go`）：调用该 option 的宿主声明默认即为 false，与模块无 gate 时的内部回落、以及 `Register` 对"邮件开而发件地址/链接未配"的启动期拒绝三者自此同源。未调该 option 的宿主声明默认保持 true，无任何变化。
- **消费者影响**：同时调用 `WithInvitationEmailDisabled` 且经 `org.WithFeatureGate` 接线 config 读取缝的宿主（saasctl 两份含 org 的选择模板即此形态），在租户无覆盖行时：config 的 features 查询不再把 `org.invitation_email` 报为开，Invite 以成功、静默不发信完成（接线前骨架行为；接线缺陷下该默认臂曾以 `org.invitation_mail_required`（Internal 类）硬失败，正是本轮修复的对象）。操作员显式写 `org.invitation_email=true`（系统行或租户行）时，无 from/link 的宿主按 org 真实语义拒绝该次邀请（`org.invitation_mail_required`）并撤销刚落库的邀请——与启动期"邮件开即要求传输配齐"的契约同语义，不静默丢信；有真实传输的宿主（reference-app 形态，不调该 option）显式开臂照常送达。既有导出符号与签名不变。
- **替代路径**：无（行为修正）；要由 org 自行送达的宿主接 `WithMailFrom` 与 `WithInvitationLinkBuilder` 并去掉该 option；交由通知模块送达的宿主维持现状。
- **登记理由**：宿主可见面（已声明特性开关的默认值随宿主接线改变，features 查询的报告随之改变）的行为修正，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（签名不变，不带 footer）。
- **出处**：`01bf3405`（`fix(org): declare the email flag off for a host without a mail transport`）+ `1bc68db1`（`test(saasctl): pin the three invitation arms a gated generated project produces`）。

### 5.23. （收编阶段 2）pki：模块的签名者改由组合选中的 signer 成员绑定（行为登记）

- **面**：`go/pki` 的模块组件不再无条件使用自身内置的 `LocalSigner` 默认：其描述符新增一条**可选** `(*Signer)(nil)` 依赖，组合中恰好选中一个 `signer.*` 成员时，该成员的构造排到模块自身之前，模块以**该成员的实例**为准签名（成员名的**最后一段**即模块记录的签名者名——`signer.local` 读作 `local`，与其行内一直记录的签名者名一致；宿主按 `capabilityComponent` 形状克隆注册名的改名形态——宿主前缀 + 同一注册名，如 `reference-app.signer.local` / `__APP_NAME__.signer.local`，即 reference-app 与 saasctl 模板实际选中的形状——同样读作 `local`，该形状已入钉；`signer.vault` / `signer.aws-kms` / `signer.vault-direct` 等按各自后缀记录）；选中两个成员在计划期即以歧义提供者失败（绑定式模块由选择强制）。不选任何 signer 成员时模块保持原默认，行为与既有组合一致。导出符号零变化（`WithSigner` 保留）。
- **消费者影响**：对既选 `signer.local`（或任一 `signer.*` 成员）又选 `pki` 的组合，**实现来源变化而结果等价**：`signer.local` 成员与模块默认都构造同一 `LocalSigner`（类型、共享连接、签名者名 `local` 逐项相同），行内容不变；差别只在从此"组合选中什么就签什么"——此前该选中对模块无效果，宿主若想换实现只能走 `SignerRegistry`/`BuildSignerRequiring` 的名字路径 + `WithSigner` 注入。选 `signer.vault` 等成员而不注入的自定义组合，从"静默用本地签名者"改为"用选中的实现"（这正是绑定式组件的语义）。未选 signer 成员的组合零影响。
- **替代路径**：无（行为修正）；要维持内置默认即不选任何 signer 成员；要指定实现即在组合中选中对应 `signer.*` 成员（或在组装之外自行构造并经 `WithSigner` 注入，二者保留）。
- **登记理由**：宿主可见面（组合选中的语义由"装饰"变为"生效"、绑定式模块的选中约束）的行为变更，按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏——既有组合结果等价，不带 footer）。
- **出处**：`b18d9c01`（`feat(pki): bind the module's signer to the composition-selected member`）+ `dac5a202`（`fix(pki): read the signer identity from the member name's last segment`——改名克隆形状的推导修正与入钉）。

### 5.24. （收编阶段 2）ai-gateway：provider 按调用解析优先走组件面（行为登记，双轨并存）

- **面**：`go/ai-gateway` 的每请求 provider 解析（`Gateway.Chat/ChatStream` 与图像任务处理器共用的 `resolveProvider`）在装配注册表在场时优先按组件面构造——`pkgcore.Build[T](reg, route.Provider, override{base_url, api_key})`，即阶段 1 登记的"每次请求按已解析凭据构造 provider"的组件面形状；名字未被选中（或 Gateway 在组装之外直接构造）时回落到包级 `ChatProviderRegistry` / `ImageProviderRegistry`，行为不变。route 的 provider 逻辑名与组件名逐字同一（阶段 1 的缺口 C 恒等），宿主既有 route 字符串与凭据行零迁移；选中 `chat.openai-compatible` / `image.openai-compatible` 的组合，每请求构造改走组件面，与注册面同实现、同能力位、同拒绝（缺 base_url/api_key 的拒绝同码）。装配接线在 `Module.Register`（Gateway 既有的 host-seams 步骤）把注册表挂到 Gateway 上。
- **消费者影响**：组合选中 provider 组件的宿主，请求路径经组件面构造（结果等价，无升级动作）；未选中的宿主逐项维持原行为（回落注册表）。`WithChatProviderRegistry` / `WithImageProviderRegistry` 的文档语义收窄为"装配选中缺名时的回落面"，签名与实现保留至本体退役轮。仓内首个消费者 reference-app 已实落该组合：当 boot 配置了对应平台凭据（与其平台凭据写入同一条件，未配置则该成员不选中，名字维持注册面回落）时，其组合选中 `chat.openai-compatible` / `image.openai-compatible` 成员，组合块携带与凭据行相同的 base_url/api_key 对，每请求构造即走组件面。
- **替代路径**：不选 provider 组件即维持注册表面；要固定实现即在组合中选中对应 provider 组件。
- **登记理由**：每请求解析的来源选择变化（组件面优先、注册表回落；既有组合结果等价），按"宿主可见面变更须带 `!BREAKING` footer 或登记本清单"的纪律登记（非破坏，不带 footer）。
- **出处**：`68c714d9`（`feat(ai-gateway): resolve providers through the assembly's selected members first`）。

### 5.25. （收编阶段 2）billing：三个支付网关组件暂无仓内消费者（状态登记）

- **状态**：`gateway.stripe` / `gateway.alipay` / `gateway.wechat` 组件（阶段 1 登记）已注册、经各包套件与双面对照测试钉住，但**没有任何仓内宿主消费其产品**：`billing.Module` 的支付网关表按设计由宿主注入（`WithGateways`，channel 集合不是配置），reference-app 无支付收单与 webhook 生命周期、亦不空导入任何 provider 子包，saasctl 各模板不含 billing。
- **披露**：如实登记"暂不接"——不以虚构消费者（为示例宿主补一条无业务意义的收单流程）凑齐机制；将来宿主接入（组合选中 `gateway.*` 成员并经 `WithGateways` 注入）时另行登记。组件面本身已可用：选中即按普通组件参与装配的能力位校验。
- **登记理由**：披露性状态登记，非破坏面。

### 5.26. （收编阶段 3）pkgcore：泛型 SeamRegistry[T]/Registration[T] 与三族目录注册表本体退役（破坏面登记）

- **面**：泛型注册机制本体与它的最后三族消费者一并移除。`go/pkgcore`：`SeamRegistry[T]`（`Register`/`Build`）、`Registration[T]`、`NewSeamRegistry`、`ErrDuplicateImplementation`、`ErrUnknownImplementation` 删除（`seam_registry.go` 与其测试整文件删）；`pkgcore.Config` 与 `ErrMissingSeamConfig` **保留**，从 `seam_registry.go`/`registries.go` 迁入新文件 `flat_config.go`（内置实现共享构造路径的扁平设置面与缺配置哨兵，签名与语义不变），`registries.go` 因只含该哨兵而随迁删除。`go/pki`：`SignerRegistry` 与 `BuildSignerRequiring` 删除（`signer_registry.go` 及其测试整文件删）；`signer.local` 与四个 provider 名（`signer.vault`/`signer.vault-direct`/`signer.aws-kms`/`signer.aws-kms-direct`）保留为组件描述符，两个 provider 子包的 `register.go`（init 注册面）删除，适配器（`envelopeSignerFromConfig`/`directSignerFromConfig`/`configFromFlat`）折入各自 `component.go`。`go/ai-gateway`：`ChatProviderRegistry`、`ImageProviderRegistry`、`WithChatProviderRegistry`、`WithImageProviderRegistry` 删除（`registry.go`/`image_registry.go` 及测试整文件删）；每请求解析在没有装配注册表在场时直接拒绝（错误点名 provider 与装配要求），不再回落包级注册表；`ProviderOpenAICompatible`/`ProviderOpenAICompatibleImage` 常量与两份适配器移入 `openai_compatible.go`/`openai_compatible_image.go`。`go/billing`：`PaymentGatewayRegistry` 删除（`gateway.go` 内 var 删，根部随之不再 import pkgcore），三个 provider 子包的 `register.go` 删除、`gatewayFromConfig` 折入各自 `component.go`。`go/dbkit` 的 `RegisterDialect` 注释改引 `pkgcore.MustRegister`（原引 `registries.go` 的 mustRegister）。
- **消费者影响**：三处公共面移除，宿主升级动作——① 曾建 `pkgcore.NewSeamRegistry[T]()` 或调 `SeamRegistry.Register/Build` 者：自建实现改写成组件描述符（`pkgcore.Component`），在组合里选中，用 `pkgcore.Build[T]`/`Get` 读产品；② 曾走 `pki.SignerRegistry.Build`/`pki.BuildSignerRequiring` 名字路径解析签名者者：组合选中对应 `signer.*` 成员，能力位要求（`KeyNeverLeavesBoundary`）改经 `pkgcore.ComponentCapabilities` 读被选中成员的声明并自行比对（不再有解析点强制）；③ 曾注册/注入 ai-gateway provider 者：组合选中 `chat.openai-compatible`/`image.openai-compatible` 或自写描述符；`WithChatProviderRegistry`/`WithImageProviderRegistry` 的回落面不复存在，组装之外构造的 Gateway 不再解析任何 provider 名；④ 曾注册/构造 billing 支付网关者：组合选中 `gateway.*` 组件（或直接构造）后经 `billing.WithGateways` 注入。仓内消费者已随本批迁移完毕：reference-app 两个服务测试夹具经新叶子包 `internal/testutil/assemblytest` 走组件面，pki/billing/ai-gateway 的 Example 与测试面全量改组件面，文档面（CLAUDE.md、`.golangci.yml` 五条 depguard 描述、ADR 0003、docs/internal 03/06/08/15/22/27、双语文档站 pkgcore/ai-gateway/billing/pki 页、错误码索引生成物）同步改写。
- **替代路径**：组件面即唯一替代（组合选中 + `pkgcore.Build`/`Get`/`ComponentCapabilities`）；`pkgcore.Config` 与 `ErrMissingSeamConfig` 保留，扁平构造路径的既有用法无需迁移；`Module.WithSigner`/`Module.WithGateways` 的直接注入面保留。
- **登记理由**：破坏面登记（三处公共符号移除，加一处行为收紧——组装之外不再解析 provider 名）。本批四个破坏面提交（`16ec5d52`/`1bf726cf`/`5f47c462`/`b4afb1a0`）带 `!BREAKING` footer，随批的测试/文档提交（`629bc517`/`f5ead4cd`/`90c8ccee`）不带 footer（未触公共面）；§9 的「29 §9 目录式组件收编」挂起项随之闭合。
- **出处**：`16ec5d52`（`feat(pkgcore)!: remove the seam registry machinery`）+ `1bf726cf`（`refactor(ai-gateway)!: resolve providers through the component face only`）+ `5f47c462`（`refactor(pki)!: retire the signer registry`）+ `b4afb1a0`（`refactor(billing)!: retire the payment gateway registry`）+ `629bc517`（`test(reference-app): assemble provider fixtures through the component face`）+ `f5ead4cd`（`docs: narrate the resolution face as components, not registries`）。

## 6. D 组：configrefgen 工具内部迁移（工具面，非宿主面）

- **面**：`tools/configrefgen` 的内部实现从旧内核面迁到组件面（工具自身不在消费者依赖面内）。
- **登记理由**：它是仓内唯一以"生成物一致性"为门禁的工具（文档检查流水线 `--check` 四产物），其迁移已完成并全绿；消费者无需动作。
- **出处**：`23bbb3a9`（`refactor(tools): resolve the configuration reference through the component assembly`）。

## 7. E 组：e2e stub 缺口（状态登记）

- **状态**：e2e CI 腿已由 stub 改为真腿（`.github/workflows/e2e.yml`，含诚实门）；"e2e 仍是 stub" 类过时表述已清扫——四个工作流文件（fast-check、reusable-docker-build、docker-image-ci、reusable-npm-package-ci）随该轮改写，docs/internal 同族句一并对齐。
- **出处**：e2e 轮提交 `e5ec05b9`（及同轮 §2.1 的四句表述清扫）。
- **说明**：本轮只登记状态、不改其文件；若该轮先于本清单落地，此条即为已闭记录。

## 8. 附录：2026-09 以来全部 `!` 提交

以下为 `git log origin/main --grep '!:' --oneline`（2026-09-01 起）的完整清单，供 release note 逐条改写使用：

| sha | 一句话 |
|---|---|
| `58ffd7e3` | reference-app 宿主接线的可选依赖读取改错误传播（§5.8） |
| `760a73db` | saasctl 模板的 SMS sender 读取改错误传播（§5.8） |
| `69651edb` | 七个模块组件构造的可选依赖读取改错误传播（§5.8） |
| `453ba35e` | plan 期拒绝同一 token 被多组件交付（§5.8） |
| `0c7a61a8` | notification 永久传输失败哨兵改挂 pkgcore（ErrTransportPermanent 移除） |
| `0a62299d` | pki RootCAParams / IntermediateCAParams 合并为 CAParams |
| `09fffdc9` | billing ExpireInput 并入 PreDeductInput |
| `1267ce1c` | Mailer 地址结构预校验：`Send` 拒绝不可寻址的地址（§5.14） |
| `a92adfb6` | 三条 integration 腿与三处运行时文案迁到组件面（本轮） |
| `53a824b8` | reference-app 选用模块自身描述符（去宿主拷贝面） |
| `91ffde15` | config/rbac 快照服务改在 Start 补齐（不再发布 Provides token） |
| `12c1ef4f` | pkgcore 每条 Provides 声明钉到构造期交付 |
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

- **29 §9 目录式组件收编**：把模块内目录注册表（pki signer、ai-gateway provider、billing gateway）统一收编进组件机制的设想**已落地**——阶段 1 加组件面、阶段 2 切宿主面、阶段 3 本体退役，破坏面见 §5.26（收编阶段 3）段。此项闭合。
- **pkgcore 注释面死引用（残留登记）**：`register.go` 悬空引用 52 处/33 文件、flat adapter 6 处，与 F-2 轮登记的 129 处/47 文件属同一类别，合并为一个子项，输入 R18 清扫收尾批；纯注释面，不在本清单的破坏面内。
- **首个可用版本**：`task release:plan` 的离线计划（02 号文）在首个版本发布时以本清单生成 release note；发布后本清单转为历史记录，后续破坏面另起新篇。
