# 部署模式与实现组装

> 这是贯穿全局的架构原则，不是某个模块的局部设计。任何新增的基础设施能力都必须落在本文描述的组装机制里。

## 两条正交的轴

这里有两件**互不决定**的事，早期版本的本文把它们焊成了一条开关，是设计错误：

- **部署模式**——这套系统被部署成几个副本、能依赖哪些外部设施。
- **实现组装**——每个基础设施 seam 具体选用哪一套实现。

典型反例说明二者为何正交：一个单进程部署完全可以对接真实 Stripe、真实 SMTP、真实 S3，这正是"一个二进制交付给小客户"的常规生产形态；反过来，一个分布式部署也完全可以在联调环境里挂 MailHog 与支付沙箱。**"用不用真实外部服务"是环境与凭证问题，与部署成几个副本无关。**

因此：**部署模式不选择实现，它只约束实现。** 给定一组组装，装配阶段校验它能否在所声明的部署模式下正确运行，不能则启动失败。

约束的方向是单向的——分布式（多副本）排除进程内实现；单进程**不排除任何东西**。

## 基础设施 seam 与可选实现

每个 seam 是 `pkgcore` 中的一个接口，其实现有 N 套（N ≥ 1），**不是固定两套**。下表是清单的当前形态，新增一套实现是加一行，不是改开关：

| seam | 接口 | 可选实现 |
|---|---|---|
| 数据库 | `dbkit` 方言层 | SQLite（`glebarez/sqlite`，纯 Go）/ PostgreSQL（+ 可选 TimescaleDB 扩展） |
| 缓存 / 计数器 | `KVStore` | 进程内 `sync.Map` + TTL / Redis |
| 事件总线 | `EventBus` | 进程内 channel / Redis Streams（消费者组）/ NATS（候选） |
| 任务队列 | `Queue` | 进程内 worker pool + SQLite 任务表 / Redis（`hibiken/asynq`） |
| 定时调度 | 调度器 | 进程内 goroutine + ticker（这个 seam 目前只有一套实现，小集群无需独立调度器；多副本下靠分布式锁防重复执行——N=1 同样合法） |
| 分布式锁 | 锁接口 | 进程内 `sync.Mutex` / Redis 锁 |
| 对象存储 | `ObjectStore` | 本地文件系统目录 / S3 兼容（MinIO、云 OSS） |
| 邮件发送 | `Mailer` | 打印到 stdout（`ConsoleMailer`）/ SMTP |
| 短信发送 | 短信接口 | 打印到 stdout（`ConsoleSMS`）/ 各短信网关 |
| 支付渠道 | `PaymentGateway` | `MockGateway`（立即成功 / 可模拟失败与延迟回调）/ Stripe / 支付宝 / 微信 |
| AI Provider | `ChatProvider` / `ImageProvider` | `EchoProvider` + 可录制回放的 fixture / OpenAI / Anthropic / 国内厂商 |
| 社交登录 | `SocialProvider` | `MockSocialProvider` / Google / GitHub / 微信 / 钉钉 / 飞书 |
| 遥测导出 | OTel exporter | stdout + 进程内 `/metrics` 端点 / OTLP → Collector |
| 计量缓冲 | 计量 flush 后端 | 内存 channel 聚合写汇总表 / Redis Streams → aggregator |
| 站内信实时推送 | 推送扇出 | 单进程直接推给 SSE 连接 / Redis Pub/Sub 扇出 |
| 全文检索 | 检索后端 | SQLite `LIKE` 降级 / PostgreSQL `tsvector` |

注意表中的 `MockGateway`、`EchoProvider`、`MockSocialProvider`、`ConsoleMailer`、`ConsoleSMS` 只是**该 seam 的一套实现**，不是"单进程模式的实现"。它们通常出现在开发与 CI 的组装里，但这是组装者的选择，不是部署模式强加的。

## 能力声明与组装校验

每套实现声明自己的能力，每种部署模式声明它要求的能力，装配时做集合比较。能力至少包含这三项：

| 能力 | 含义 | 不满足的例子 |
|---|---|---|
| `MultiReplicaSafe` | 多个副本共享同一份状态 | 进程内 channel 总线：每个副本各发各的；内存 `KVStore`：配额各算各的；本地目录 `ObjectStore`：每个副本一块私有盘 |
| `SurvivesRestart` | 进程重启后数据仍在 | 内存 `KVStore`、进程内事件总线、临时目录 `ObjectStore` |
| `Stateless` | 实现不承载任何跨调用状态——进程死亡不损失数据，横幅警告（约束 5）对它是空洞的 | 内存 `KVStore`：它持有键值状态，只是不跨重启；Redis 总线：状态在服务端，声明的是 `SurvivesRestart` |

部署模式声明所需能力：

| 部署模式 | 要求 |
|---|---|
| 单进程（单副本） | 无额外能力要求 |
| 分布式（多副本） | 所有承载共享状态的 seam，其实现必须 `MultiReplicaSafe` |

**校验失败即启动失败**，错误信息点名"哪个 seam 的哪套实现不满足哪条能力"。这取代了早期按模式硬编码的那批错误（`ErrMissingDistributedEventBus` 之类）——在 N 套实现下，"缺少分布式实现"这个说法本身就不成立。

能力表是可扩展的，而且这个扩展点已经实践过——`Stateless` 位（见上表）是改造落地后追加的第三位：横幅警告（约束 5）对 `mailer.console` 这类无状态实现是空洞的，于是加一个「无状态」位来豁免，警告只对有状态却不持久的实现打出。将来若需要区分投递顺序保证、消费者组语义等维度，照旧是往表里加行，不是加开关。

## 组装的表达：preset 与逐项覆盖

三层，后者覆盖前者：

```
内置 preset  <  配置文件逐项覆盖  <  代码注入（WithEventBus(...) 等）
```

- **preset** 是引导配置里的一组命名映射（seam → 实现名 + 该实现的参数）。它必须在**引导配置层**（koanf）解析完成，不能放进 `config` 模块的动态配置表——`config` 模块自身依赖数据库 seam，那是循环依赖。
- **框架不预设"生产""测试"这类 preset。** 哪一组组装算生产、生产环境该不该出现 mock 实现，是应用组装者的判断，脚手架不替它决定，也不做"检测到 mock 就拒绝启动"这类策略。框架内置的 preset 仅按部署模式命名，作为起点。
- **代码注入**保留，用于配置文件表达不了的情况：宿主自带一套 `pkgcore` 不认识的实现。

## 实现注册表

`名字 → 构造函数` 的注册表，键形如 `eventbus.redis`。内置实现开箱注册，宿主可追加自己的实现（比如接公司内部总线）。

### 编译期引入哪些实现，是应用组装者的决定

**每套实现独立成包，应用 import 哪些，就在编译期承担哪些。** 这条与本文开头"部署模式不选择实现，它只约束实现"是同一个立场在依赖层面的延伸，也与上一节"框架不预设生产/测试这类 preset"同源：哪些后端进这个应用，是组装者的判断，脚手架不替它决定。

三种情形，覆盖全部需求：

| 应用需要什么 | 它怎么做 | 它承担什么 |
|---|---|---|
| 确定只用 SQLite | 只 import SQLite 驱动包 | 只有 SQLite 的依赖 |
| 要能在运行期于 SQLite 与 PostgreSQL 间切换 | 两个驱动包都 import，用配置文件选 | 两套依赖——这是它自己选的能力，代价合理 |
| 自带 `pkgcore` 不认识的后端 | import 自己的实现包 | 自己的依赖 |

第二行说明"同一个二进制能跑任意组合、切换组装不必重新编译"这个能力**一分不少**：需要它的应用把相关实现全部 import 进来即可。

> **一处已被修正的表述。** 本节早先写的是"所有内置实现都链接进同一个二进制，不做按部署形态裁剪的编译变体。这是明确的取舍：换来'同一个二进制能跑任意组合'，代价是全部后端 SDK 都落进消费方的 `go.sum`"。
>
> **它不是取舍，因为它没有换来任何东西。** 上表第二行证明：运行期可切换这个属性，在实现分包之后依然可得——想要的应用 import 全部实现就有了，代价一分不少。分包只是把这个属性从**强制**变成**可选**。所以旧形态相对于分包形态是纯损失：它替所有应用做了"我需要全部后端"这个决定，而这个决定本该由应用自己做。参照物是 `database/sql`——import 两个驱动就能按 DSN 在运行期切换，只 import 一个就只支持一个，选择权始终在应用手里。

这条原则之所以需要单独写下来，是因为**"必须遵守的约束"第 1 条覆盖不到它**：Go 的依赖解析按**包**而非按符号进行，所以一个模块可以完全遵守"业务代码只依赖接口，永远不 import 具体实现"，却仍然因为接口与实现同包而背上后端 SDK。真实例子是 `go/ratelimit`——建立在 `KVStore` 接口之上的滑动窗口计数器，非测试代码里零第三方 import，消费者却要为它承担 24 个 indirect 依赖，其中包含 S3 SDK。它没有做错任何事，是它依赖的包替它做了决定。

代价可以精确测量：新建空模块、`require` 目标模块、`go mod tidy`（`GOWORK=off`），数 `// indirect` 条目。**向内置清单添加实现的 PR 必须附上这个数字**，因为代价随依赖图向上累积——底座模块内联一套实现传染全部模块，叶子模块只影响自己的消费者，因此越靠近 `pkgcore`，门槛越高。

**分包优先用子包，不是新开模块。** 子包已经足够：Go 按包解析依赖，隔离一路穿透到 `go.sum` 与 MVS 版本选择。同一模块内的实测——只 import `pkgcore` 根包的消费者，`go.mod` 与 `go.sum` 里都不出现 `koanf` 的任何条目（它只被 `pkgcore/config` 子包使用），而 import 该子包的消费者 `go.sum` 里有 10 条。既然隔离效果相同，就不该为它多开模块：**模块是发布单元，按领域内聚性划分，不该被打包机制的需求扯变形。** lockstep 下每多一个模块要多一份 `go.work` 条目、CI 矩阵行、`AGENTS.md`、changesets 固定版本组条目与版本标签，子包这些全都不要。只有当某套实现需要独立的发布节奏、或消费者会绕开主模块单独使用它时才值得升格——lockstep 版本策略下这两种情况都不成立。

分包要接受一处代价：它把"忘了提供实现"从编译期错误变成启动期错误（`database/sql` 的 `unknown driver` 是同一个交易）。因此解析失败的报错必须点名补救方式——"preset 需要 `eventbus.redis`，但没有任何包注册过它，请 import ×××"，而不是一句"实现未找到"。

现存的违反实例、实测数字与修复方案在 issue 中跟踪，不在本文展开——本文只确立原则。**issue #1 的四个站点中，站点二已落地**：`dbkit` 的两个 SQL 方言驱动拆分为 `dbkit/dialect/sqlite`、`dbkit/dialect/postgres` 子包，`dbkit.Open` 改为通过一个模仿 `database/sql` 的注册表（`RegisterDialect`）按方言名查找驱动，未 blank-import 对应子包时报错并点名补救方式；只 import `dbkit` 根包的消费者实测少背 14 个 indirect 依赖（41 降到 27），细节见 `go/dbkit/AGENTS.md`。其余三个站点仍在 issue 中跟踪。

## 契约测试

N 套实现最大的风险是语义漂移，而且漂移面是 N² 而非 N。唯一的防线是**每个 seam 一套契约测试套件**，所有实现——内置的与宿主自带的——一律必须通过，做法参照 `tenancy` 的 `AssertIsolated`：

```
pkgcore/eventbustest.AssertConforms(t, factory)
```

CI 的矩阵因此不再是"同一组用例跑两遍部署模式"，而是：**每个 seam 的契约测试 × 该 seam 的每套实现**，外加若干条有代表性的整机组装冒烟。

> **实现状态注记（2026-09-03，authn 轮）——本注记不是设计正文，设计正文保持原样。**
>
> "短信发送"一行已经是真实代码，不再是设计目标：`go/authn/sms.go` 的 `SMSSender` 接口有两套实现——单进程用 `NewConsoleSMSSender`（写到注入的 `io.Writer`，`examples/reference-app` 默认接 `os.Stdout`），分布式用 `NewHTTPSMSSender`（通用 JSON 网关 POST，默认走 `internal/safehttp` 的 SSRF 防护客户端）；分布式部署模式下不传 `SMSSender` 会在装配期直接失败（`ErrMissingDistributedSMSSender`），不会静默退化成打印到 stdout。真正的运营商适配器（阿里云/腾讯云/Twilio）仍未接入，属于 M2 `notification` 轮的工作（`go/authn/AGENTS.md` 的 Known limitations）。
>
> "认证"一行的"密码登录"半边也已落地，并且比这行原文写得更多：`go/authn` 除密码外还实现了手机号+短信验证码登录、五个社交登录渠道（Google/GitHub/微信开放平台/钉钉/飞书）、按租户配置的企业 OIDC 单点登录、TOTP 二次验证与恢复码、会话/设备自助管理。"内置种子账号"这半边仍不存在——`Taskfile.yml` 的 `seed` 任务仍是一个等待 `org`+`billing` 的占位 stub，`examples/reference-app` 的 `demoMemberships`（`cmd/server/server.go`）在生产装配路径下从空状态启动：一个新注册账号在被显式授予租户成员身份之前无法登入任何租户——这是 `go/authn/service.go`'s `resolveTenant` 的既定 fail-closed 行为，不是缺陷（`go/authn/AGENTS.md` 的 Known limitations 有完整记录）。当前实现状态以根目录 CLAUDE.md 的 Repository Status 为准。

## 必须遵守的约束

1. **业务代码只依赖接口，永远不 import 具体实现**。`billing` 依赖 `pkgcore.KVStore`，不 import `go-redis`。
2. **接口选型以能力最弱的一方为准**。锚点是"该 seam 所有已注册实现中最弱的那个"，不是"单进程实现"——`KVStore` 因此不暴露 Redis 特有的 Lua 脚本能力；确需原子操作时，接口层定义 `IncrByFloat`/`CompareAndSwap` 这类可被所有实现满足的语义。
3. **业务代码看不见部署模式，也看不见选了哪套实现**。不允许 `if mode == "standalone"` 散落在业务逻辑里——这类分支只允许存在于 Kernel 的装配代码中。新设计下这条纪律更容易守：业务代码根本拿不到可供分支的全局模式。
4. **单副本部署的独占校验**：进程无法自知被部署了几个副本，因此靠**独占锁**来保证——启动时对 SQLite 数据文件取排他文件锁并写入实例标识，第二个进程会立即失败退出并打印明确原因。这比"检测副本数"可靠得多，也顺带防止了两个进程同时写坏同一个 SQLite 文件。
5. **不满足 `SurvivesRestart` 的实现必须在启动时显式声明**：装配了这类实现时打印醒目横幅，写明哪些数据不保证跨重启存活。注意这是**该实现**的性质，不是单进程部署的性质——计费级计量走 outbox、与业务写同一事务落库（见 [06 计费与计量](06-billing-and-metering.md)），这条路径在 SQLite 上同样持久，不因单进程而失效。
6. **每套实现独立成包（优先子包，而非新开模块）；编译期引入哪些实现由应用组装者决定，框架不代劳**。约束 1 管源码层面（业务代码不 import 实现），这一条管打包层面（实现的依赖会不会落到接口消费者头上）——两者不能互相替代，因为 Go 按包而非按符号解析依赖，同包内联的实现无法被消费者裁剪。需要运行期在多套实现间切换的应用，把它们全部 import 进来即可，该能力一分不减。添加实现的 PR 必须附上实测的依赖增量。理由与测法见"实现注册表"节。

## Compose 分层

按需组合，而非一个大文件：

```
docker-compose.standalone.yml    # 仅 app 一个容器 + SQLite 卷
docker-compose.yml               # app + postgres + redis
docker-compose.observability.yml # 叠加 otel-collector/prometheus/tempo/loki/grafana
docker-compose.dev-tools.yml     # 可选：MinIO、MailHog、支付沙箱代理
```

## 对可测试性的额外收益

进程内的那批实现（内存 `KVStore`、channel 总线、`ConsoleMailer`、本地目录 `ObjectStore`）同时就是单元测试的 test double，不需要为测试再造一套 mock，也让 CI 无需拉起 testcontainers 就能跑绝大多数测试（只有双方言 SQL 兼容性测试、以及需要真实 Redis / MinIO 的集成测试才需要容器）。这是这套接口抽象除"部署灵活"之外的第二个正收益，值得为它多付出抽象成本。

## 当前实现状态

**本文描述的设计已落地**（2026-09，deployment-composition 改造轮）——preset 的「参数通道」一项除外，见下方专列 bullet。实现分布：

- `go/pkgcore` 新增四件套：`capability.go`（`Capability` 位掩码与 `Has`，三个能力位 `MultiReplicaSafe`/`SurvivesRestart`/`Stateless`——第三个是紧随其后补的：`mailer.console` 这类无状态实现没有可跨重启丢失的数据，`SurvivesRestart` 横幅对它们空洞，于是加 `Stateless` 位豁免，警告只对有状态却不持久的实现（内存 `KVStore` 等）打出）、`seam_registry.go`（`SeamRegistry[T]`/`Registration[T]`，镜像 `database/sql` 的驱动注册模式——实现按名字注册，`Build(name, cfg)` 按名解析并回报其能力位）、`preset.go`（`Preset` 是 seam 名 → 实现名的映射，`PresetStandalone`/`PresetDistributed` 两个预置）、`builtin_implementations.go`（四个 seam 的注册表集中预置八套内置实现：`eventbus.memory`/`redis`、`kv.memory`/`redis`、`mailer.console`/`smtp`、`objectstore.local`/`s3`）。`NewKernel(opts...)` 不再以部署模式为首位参数：`WithDeploymentMode` 只声明拓扑，`WithPreset` 换整张映射，`WithEventBus`/`WithKVStore`/`WithMailer`/`WithObjectStore` 各带能力位注入单套实现。`Bootstrap` 在四个 seam 解析完成后做一次能力校验：某 seam 解析出的实现不满足声明模式的 `RequiredCapabilities()` 即启动失败，错误 wrap `ErrCapabilityUnsatisfied` 并点名 seam、实现名、缺失能力与模式（四个 `ErrMissingDistributed*` 哨兵已删除）；不满足 `SurvivesRestart` 的实现照常启动、但打印横幅警告（约束 5）。
- **`go/pkgcore` 的 Redis/S3 内置实现已分包（2026-09-04，issue #1 site 1 轮，"实现注册表"节原则的落地）**：`redis_kv.go`/`redis_eventbus.go`/`s3_objectstore.go` 从根包搬到 `go/pkgcore/kv/redis`、`go/pkgcore/eventbus/redis`、`go/pkgcore/objectstore/s3` 三个子包，`NewRedisKVStore`/`NewRedisEventBus`/`NewS3ObjectStore` 相应更名为各子包自己的 `NewKVStore`/`NewEventBus`/`NewObjectStore`（`S3Config` 随之更名 `s3.Config`），各自 `init()` 把 `kv.redis`/`eventbus.redis`/`objectstore.s3` 注册到根包仍然导出的 `KVStoreRegistry`/`EventBusRegistry`/`ObjectStoreRegistry` 上——`PresetDistributed` 三个名字不变，但现在唯有宿主 import 了对应子包（哪怕只是 `import _ "..."`）才能解析成功，否则 `Bootstrap` 报 `ErrUnknownImplementation`，`database/sql`"忘记 import 驱动"式的编译期错误变运行期错误，正是"实现注册表"节接受的代价。实测：只 import 根包 `pkgcore` 的消费者，`go.mod`/`go.sum` 里的 `// indirect` 条目从 23 条降到 3 条，与只 import `pkgcore/i18n` 打平——Redis、S3 两套 SDK 依赖闭包对根包消费者的净成本归零。`.golangci.yml` 的 `redis-only-in-pkgcore-and-jobs`/`minio-only-in-pkgcore` 两条 depguard 豁免同步从整个 `go/pkgcore/**` 收窄到具体子包路径，使"实现不得与其他 seam 的实现共享一个包"这条纪律第一次真正可被 lint 捕获。三个子包各自拥有独立的 `integration_test/`（Docker-backed），不再共享 `go/pkgcore` 根包的集成层；`go/dbkit`/`go/observability`/`go/jobs` 的同类拆分是同一 issue 的另外三个站点，各自独立成轮，本文其余处仍以历史口径描述"八套内置实现集中在 `builtin_implementations.go`"，读者应以此 bullet 为准。
- **preset 的「参数通道」与「配置文件逐项覆盖」未随本轮落地，是明确的遗留**：正文「组装的表达」一节要求 preset 形如「seam → 实现名 + 该实现的参数」、在引导配置层（koanf）解析完成，并存在「内置 preset < 配置文件逐项覆盖 < 代码注入」三层。shipped 的 `Preset` 只是 seam 名 → 实现名的映射（`map[string]string`），解析路径上的参数通道是空的——`seam_registry.go` 的 `Build(name, cfg)` 签名预留了 `cfg`，但 preset 解析路径恒以空 `Config` 构建（`resolveKernelSeam`），`cfg` 尚无任何调用方填充；引导配置层与逐项覆盖通道同样不存在，`pkgcore` 自身从不读配置文件或环境，宿主自己的 koanf/环境层在上，把解析好的 `Preset` 喂进来（与 `Config.OTLPEndpoint` 今天的喂法相同，见 `preset.go` 的 doc comment）。这层缺失的可见后果：`PresetDistributed` 里两个 Redis seam 落到裸默认地址（`localhost:6379`，无鉴权），SMTP/S3 两个没有安全默认的 seam 在该 preset 下以 `ErrMissingSeamConfig` 解析失败——需要真实凭证的宿主走代码注入（`WithMailer`/`WithObjectStore`），注入恒优先于 preset（按 seam）。参数通道留给后续轮次；装配校验、横幅警告与能力位已按正文落地。
- 契约测试随模块落地，「契约测试」节的目标形态已是仓库标准：`go/pkgcore/eventbustest`、`kvstoretest`、`mailertest`、`objectstoretest` 四个 `AssertConforms(t, factory)` 套件，每套内置实现各跑一遍——真实 Redis/MinIO 上的实现跑在各自子包自己的 Docker 集成层（`go/pkgcore/eventbus/redis`、`kv/redis`、`objectstore/s3` 各自的 `integration_test/`，上一条 bullet 记录的分包结果），SMTP 走进程内 fake relay，留在 `go/pkgcore` 根包自己的单元测试层（矩阵形态见 [16 验证](16-verification.md) §2 与 [18 CI/CD](18-cicd.md)）。
- `go/observability`：`Init(ctx, opts...)` 不再接收部署模式，导出器选择只取决于是否提供 `WithOTLPEndpoint`——单进程组装如今可以把遥测送往真实 Collector（`ErrMissingOTLPEndpoint` 已删除）。旧形态里唯一连注入都绕不过的硬绑定就此消失。**（issue #1 site 3，2026-09 补齐）** 根包过去无条件内联 OTLP 的 `otlptracegrpc`/`otlpmetricgrpc` 与本地 Prometheus 的 `client_golang`/`otel/exporters/prometheus`，本轮拆成 `exporter/otlp`、`exporter/prometheus` 两个子包，各自 `init()` 调用根包新增的 `RegisterOTLPExporters`/`RegisterLocalMetricsReader` 单槽注册（约束 6 的同一模式，槽位而非按名注册表，因为每类只有一套内置实现）：未 blank-import `exporter/otlp` 时 `WithOTLPEndpoint` 直接报错点名该 import；未 blank-import `exporter/prometheus` 时 `MetricsHandler` 的本地拉取端点由「无条件可用」变成「默认 404、需显式选用」——这是一次真实的默认行为变更，`examples/reference-app` 已跟进 blank-import 以保住其 `/metrics` 路由，`go/saasctl` 的生成项目模板尚未跟进，记在 `go/observability/AGENTS.md` 里等后续轮次处理。实测：只 require 根包的消费者 `go.mod` 间接依赖从 55 条降到 36 条，gRPC/protobuf 全家桶与两套 Prometheus 包彻底消失。
- `go/jobs`：`StandaloneQueue` 与 `asynq.Queue`（issue #1 site 4 已完成的打包隔离：后者连同 `hibiken/asynq`/`redis/go-redis/v9` 一并搬进 `go/jobs/queue/asynq` 子包，`StandaloneQueue` 仍留在模块根包且自身非测试代码零第三方依赖——只是包位置的收敛，措施与 `go/dbkit` 方言驱动、`go/pkgcore` 的 Redis/S3 实现拆分子包同源，详见 `go/jobs/AGENTS.md`「Packaging: why asynq.Queue is a subpackage」一节的量化对比）由宿主直接选择、本身不读部署模式，形态与本文一致；Queue seam 真正的注册表化/能力声明化（纳入 `pkgcore.SeamRegistry`/`Kernel.Bootstrap`/`Preset`）仍留给 go/jobs 自己更大的后续轮次——本次改动不构成、也不冒充那一轮。
- `examples/reference-app`：入口不再拒绝任何部署模式（`cmd/server/server.go` 读 `SPEED_DEPLOYMENT_MODE`，默认 standalone）；`SPEED_REDIS_ADDR` 把「eventbus」seam 从 preset 换成真实 Redis Streams 总线——standalone 拓扑 + `MultiReplicaSafe` 实现的组装正是两条轴正交的演示。其 Docker-backed 集成测试（`examples/reference-app/integration_test/`，随 pr-full 的 reference-app job 与 `task test:full` 运行）以真实子进程启动整机，跨进程断言事件经真实 Redis 送达。

改造的破坏面（`NewKernel` 签名、四个 `ErrMissingDistributed*`、`observability.Init` 签名）集中在 `go/pkgcore` 与 `go/observability` 两个模块，已在 v1.0 之前以破坏性提交完成；`DeploymentMode` 类型与常量、`ParseDeploymentMode`、`Kernel.DeploymentMode()` 保留——模式仍是公开 API，只是不再选择实现。
