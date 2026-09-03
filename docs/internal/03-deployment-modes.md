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

每套实现声明自己的能力，每种部署模式声明它要求的能力，装配时做集合比较。能力至少包含这两项：

| 能力 | 含义 | 不满足的例子 |
|---|---|---|
| `MultiReplicaSafe` | 多个副本共享同一份状态 | 进程内 channel 总线：每个副本各发各的；内存 `KVStore`：配额各算各的；本地目录 `ObjectStore`：每个副本一块私有盘 |
| `SurvivesRestart` | 进程重启后数据仍在 | 内存 `KVStore`、进程内事件总线、临时目录 `ObjectStore` |

部署模式声明所需能力：

| 部署模式 | 要求 |
|---|---|
| 单进程（单副本） | 无额外能力要求 |
| 分布式（多副本） | 所有承载共享状态的 seam，其实现必须 `MultiReplicaSafe` |

**校验失败即启动失败**，错误信息点名"哪个 seam 的哪套实现不满足哪条能力"。这取代了早期按模式硬编码的那批错误（`ErrMissingDistributedEventBus` 之类）——在 N 套实现下，"缺少分布式实现"这个说法本身就不成立。

能力表是可扩展的：将来若需要区分投递顺序保证、消费者组语义等维度，是往表里加行，不是加开关。

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

**所有内置实现都链接进同一个二进制**，不做按部署形态裁剪的编译变体。这是明确的取舍：换来"同一个二进制能跑任意组合、切换组装不必重新编译"，代价是全部后端 SDK 都落进消费方的 `go.sum`。因此**哪些实现进内置清单需要逐个论证**——脚手架维护一份克制的内置集合，冷门后端走宿主自行注册的路径。

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

**本文描述的是目标设计，代码尚未跟进。** 截至本次修订，已实现的部分仍是早期的二元开关形态：

- `go/pkgcore`：`NewKernel(mode, opts...)` 以部署模式为首位参数，并用它**选择**默认实现（`registry.go` 的 `kvStore`/`resolveMailer`/`resolveObjectStore`）；四个 seam 各有一个 `ErrMissingDistributed*` 错误。宿主可用 `WithEventBus`/`WithKVStore`/`WithMailer`/`WithObjectStore` 覆盖，因此自由组装在**注入层面**已经可行，缺的是注册表、preset、能力声明与校验。
- `go/observability`：`Init` 接受部署模式，且 `OTLPEndpoint` 对单进程模式**被忽略**（`init.go`）——这是唯一一处连注入都绕不过的硬绑定，单进程部署目前无法把遥测送往真实 Collector。
- `go/jobs`：`StandaloneQueue` 与 `AsynqQueue` 由宿主选择，本身不读部署模式，形态已经接近目标设计。
- `examples/reference-app`：入口拒绝 standalone 以外的一切部署模式，分布式路径从未真正启动过。

改造的破坏面（`DeploymentMode` 类型与常量、`ParseDeploymentMode`、`NewKernel` 签名、`Kernel.DeploymentMode()`、四个 `ErrMissingDistributed*`）全部集中在 `go/pkgcore` 与 `go/observability` 两个模块，v1.0 未发布，是改造成本最低的时刻。
