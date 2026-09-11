# 部署模式与实现组装

> 这是贯穿全局的架构原则，不是某个模块的局部设计。任何新增的基础设施能力都必须落在本文描述的组装机制里。

## 两条正交的轴

这里有两件**互不决定**的事；把它们焊成一条开关是设计错误：

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
| 缓存 / 计数器 | `KVStore` | 进程内 `sync.Map` + TTL / Redis / PostgreSQL（单表 + TTL 列，`kv/postgres`） / Memcached（`go/pkgcore/kv/memcached`——**无持久化**：Memcached 是纯内存缓存，进程重启、滚动升级或普通内存压力下的 LRU 淘汰都会无声丢光全部数据，`SurvivesRestart` 诚实地不声明；仅适合可从权威数据源重建的一次性状态，不适合会话状态或功能开关覆盖） / NATS JetStream KV（`go/pkgcore/kv/nats`） |
| 事件总线 | `EventBus` | 进程内 channel / Redis Streams（消费者组）/ PostgreSQL `LISTEN`/`NOTIFY`（配合持久化 outbox 表，见下方实现现状小节）/ NATS JetStream（`go/pkgcore/eventbus/nats`，每副本一个唯一命名的持久 pull consumer，同一投递语义：广播到每个副本，而不是消费者组式的负载均衡） |
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
| `KeyNeverLeavesBoundary` | 该实现保护的私钥从不以明文形式存在于本进程内存 | `go/pki` 的 `LocalSigner`：签名时把私钥解密进本进程内存；同一套 KMS/Vault 实现在信封模式下（真实密钥在外部加密、本地解密后签名）同样不满足，只有直签模式（密钥在外部服务内生成且从不离开）才满足 |

部署模式声明所需能力：

| 部署模式 | 要求 |
|---|---|
| 单进程（单副本） | 无额外能力要求 |
| 分布式（多副本） | 所有承载共享状态的 seam，其实现必须 `MultiReplicaSafe` |

**校验失败即启动失败**，错误信息点名"哪个 seam 的哪套实现不满足哪条能力"。错误集合里没有按模式硬编码的缺失类错误（`ErrMissingDistributedEventBus` 之类）——在 N 套实现下，"缺少分布式实现"这个说法本身就不成立。

能力表是可扩展的，扩展点已经实践过：`Stateless` 位（见上表）——横幅警告（约束 5）对 `mailer.console` 这类无状态实现是空洞的，于是加一个「无状态」位来豁免，警告只对有状态却不持久的实现打出。`KeyNeverLeavesBoundary`（见上表）的形状不太一样：它不描述四个内置 seam 中任何一个的状态，而是由 `go/pki` 自己的 `Signer` 实现通过同一套 `pkgcore.SeamRegistry[Signer]` 机制声明；装配**不**对它做任何校验——装配的能力校验只跑在被选中的组件描述符所声明的能力位上，`pki.SignerRegistry` 是模块内目录注册表，不在其列，`pki.Module.WithSigner` 没有能力位要求参数，`SignerRegistry.Build` 只是把注册时声明的能力位原样交还，从不比对——所以一个宿主误装了一个不满足这个位的 `Signer` 到本该要求它的场景，不会报错（详见 `go/pki/AGENTS.md` 的 Known limitations）。将来若需要区分投递顺序保证、消费者组语义等维度，照旧是往表里加行，不是加开关。

## 组装的表达：组合配置与逐项覆盖

三层，后者覆盖前者：

```
内置组合  <  配置文件逐项覆盖  <  代码注入（Put(...) 等）
```

- **组合配置**是引导配置里选中的一组组件（组件名 → 该组件的配置块）。它必须在**引导配置层**（koanf）解析完成，不能放进 `config` 模块的动态配置表——`config` 模块自身依赖数据库 seam，那是循环依赖。
- **框架不预设"生产""测试"这类组合。** 哪一组组装算生产、生产环境该不该出现 mock 实现，是应用组装者的判断，脚手架不替它决定，也不做"检测到 mock 就拒绝启动"这类策略。框架内置的组合仅按部署模式命名，作为起点。
- **逐项覆盖**＝在宿主自己的组合配置里只写要覆盖的组件与键，绝不写穿共享的内置组合——这正是宿主配置层与内置组合之间的那一层。

三条路径各司其职，不互相替代：

| 路径 | 适用 | 形状 |
|---|---|---|
| **纯配置通道** | 该 seam 的设置可用扁平字符串表达（地址、凭据、桶名等），每 seam 独立连接，零装配代码 | 组合配置里该组件的配置块；组件的 `New` 从配置自建资源并由装配持有、关闭 |
| **宿主自建组件** | 需要 typed/复杂配置（TLS、Sentinel/Cluster、凭据链）或要共享既有资源 | 宿主构建对象 → 自写组件描述符包住它（或放进 by-type 上下文）→ 在组合配置里选中；对象所有权留在宿主 |
| **裸注入**（`Put(...)` 等） | 测试 double、一次性对象、共享资源（如 KV 与 EventBus 共用一个 `*redis.Client`）、以及不想经名字体系的情形 | 直接注入；注入恒优先于组合选择（按类型），装配不关闭宿主给的对象。`eventbus/redis`、`kv/redis`、`objectstore/s3` 各出一站式构造器（`FromAddr`/`FromConfig`）：把宿主原先手搓 driver client 的那一步收进实现包，一次调用返回「实例 + 该包自声明的 `Capabilities`」；两个 redis 包返回带 `Close() error` 的 owned 值（释放构造器自建的 client，宿主停机时自行关闭），s3 无 client 可关、返回裸 store；空地址/不可用配置返回错误而非 panic |

**宿主自建组件的落地形态**：每个分布式实现包导出自己的一步构造器（`eventbus/redis.NewEventBus`、`kv/nats.NewKVStore`、`s3.NewObjectStore` 等）与该实现声明的 `Capabilities` 常量，宿主自建对象（TLS 客户端、Sentinel/Cluster 拓扑、凭据链解析结果、跨 seam 共享的资源）后，把它放进 by-type 上下文，或自写组件描述符包住对象——描述符带同一个 `Capabilities` 常量，声明值与装配校验值不可能漂移。部署切换由组合配置承担：换部署只改选中的组件（及各自的配置块），typed 对象始终留在宿主手里。根包内建的 memory/console/local/smtp 组件的全部设置都可由扁平字符串表达，无需宿主自建。

## 实现注册表

`名字 → 构造函数` 的注册表，键形如 `eventbus.redis`。内置实现开箱注册，宿主可追加自己的实现（比如接公司内部总线）。

### 编译期引入哪些实现，是应用组装者的决定

**每套实现独立成包，应用 import 哪些，就在编译期承担哪些。** 这条与本文开头"部署模式不选择实现，它只约束实现"是同一个立场在依赖层面的延伸，也与上一节"框架不预设生产/测试这类组合"同源：哪些后端进这个应用，是组装者的判断，脚手架不替它决定。

三种情形，覆盖全部需求：

| 应用需要什么 | 它怎么做 | 它承担什么 |
|---|---|---|
| 确定只用 SQLite | 只 import SQLite 驱动包 | 只有 SQLite 的依赖 |
| 要能在运行期于 SQLite 与 PostgreSQL 间切换 | 两个驱动包都 import，用配置文件选 | 两套依赖——这是它自己选的能力，代价合理 |
| 自带 `pkgcore` 不认识的后端 | import 自己的实现包 | 自己的依赖 |

第二行说明"同一个二进制能跑任意组合、切换组装不必重新编译"这个能力**一分不少**：需要它的应用把相关实现全部 import 进来即可。

> **捆绑所有内置实现不是取舍，因为它没有换来任何东西。** 把全部内置实现链接进同一个二进制、不做按部署形态裁剪的编译变体，并不比"实现分包"多换来"同一个二进制能跑任意组合"之外的任何东西——上表第二行证明：运行期可切换这个属性，在实现分包之后依然可得——想要的应用 import 全部实现就有了，代价一分不少。分包只是把这个属性从**强制**变成**可选**。相对分包形态，捆绑是纯损失：它替所有应用做了"我需要全部后端"这个决定，而这个决定本该由应用自己做。参照物是 `database/sql`——import 两个驱动就能按 DSN 在运行期切换，只 import 一个就只支持一个，选择权始终在应用手里。

这条原则之所以需要单独写下来，是因为**"必须遵守的约束"第 1 条覆盖不到它**：Go 的依赖解析按**包**而非按符号进行，所以一个模块可以完全遵守"业务代码只依赖接口，永远不 import 具体实现"，却仍然因为接口与实现同包而背上后端 SDK。真实例子是 `go/ratelimit`——建立在 `KVStore` 接口之上的滑动窗口计数器，非测试代码里零第三方 import，消费者却要替它承担间接依赖——它没有做错任何事，是它依赖的包替它做了决定。

代价可以精确测量：新建空模块、`require` 目标模块、`go mod tidy`（`GOWORK=off`），数 `// indirect` 条目。**向内置清单添加实现的 PR 必须附上这个数字**，因为代价随依赖图向上累积——底座模块内联一套实现传染全部模块，叶子模块只影响自己的消费者，因此越靠近 `pkgcore`，门槛越高。

**分包优先用子包，不是新开模块。** 子包已经足够：Go 按包解析依赖，隔离一路穿透到 `go.sum` 与 MVS 版本选择。同一模块内的实测——只 import `pkgcore` 根包的消费者，`go.mod` 与 `go.sum` 里都不出现 `koanf` 的任何条目（它只被 `pkgcore/config` 子包使用），而 import 该子包的消费者 `go.sum` 里会出现它的条目。既然隔离效果相同，就不该为它多开模块：**模块是发布单元，按领域内聚性划分，不该被打包机制的需求扯变形。** lockstep 下每多一个模块要多一份 `go.work` 条目、CI 矩阵行、`AGENTS.md`、changesets 固定版本组条目与版本标签，子包这些全都不要。只有当某套实现需要独立的发布节奏、或消费者会绕开主模块单独使用它时才值得升格——lockstep 版本策略下这两种情况都不成立。

分包要接受一处代价：它把"忘了提供实现"从编译期错误变成启动期错误（`database/sql` 的 `unknown driver` 是同一个交易）。因此选择失败的报错必须点名补救方式——"组合选中了 `eventbus.redis`，但没有包注册过这个组件，请 import ×××"，而不是一句"组件未找到"。

现存的违反实例、实测数字与修复方案在 issue 跟踪中，不在本文展开——本文只确立原则。其中 `dbkit` 的方言驱动拆分已落地：`dbkit` 的两个 SQL 方言驱动拆分为 `dbkit/dialect/sqlite`、`dbkit/dialect/postgres` 子包，`dbkit.Open` 通过一个模仿 `database/sql` 的注册表（`RegisterDialect`）按方言名查找驱动，未 blank-import 对应子包时报错并点名补救方式；只 import `dbkit` 根包的消费者所背的 indirect 依赖随之大幅下降，细节见 `go/dbkit/AGENTS.md`。其余违反站点仍在跟踪中。

## 契约测试

N 套实现最大的风险是语义漂移，而且漂移面是 N² 而非 N。唯一的防线是**每个 seam 一套契约测试套件**，所有实现——内置的与宿主自带的——一律必须通过，做法参照 `tenancy` 的 `AssertIsolated`：

```
pkgcore/eventbustest.AssertConforms(t, factory)
```

CI 的矩阵因此不再是"同一组用例跑两遍部署模式"，而是：**每个 seam 的契约测试 × 该 seam 的每套实现**，外加若干条有代表性的整机组装冒烟。

"短信发送"一行是真实代码，不是设计目标：`go/pkgcore/sms.go` 的 `SMSSender` 是共享 seam（go/authn 的短信登录码与 go/notification 的 sms 渠道共用；无内核座位，各模块经自己的 `WithSMSSender` 注入），`pkgcore.SMS` 同时携带同一事实的两种形态——自由文本渠道投递的已渲染 `Text`，以及模板型适配器路由所需的消息身份（`MessageID`、`Locale`、`Params`）。根包有两套实现——单进程用 `pkgcore.NewConsoleSMSSender`（写到注入的 `io.Writer`，`examples/reference-app` 默认接 `os.Stdout`），分布式用 `pkgcore.NewHTTPSMSSender`（通用 JSON 网关 POST，默认走 `pkgcore/safehttp` 的 SSRF 防护客户端），两者都是自由文本实现、忽略身份三字段；authn 在分布式部署模式下不传 `SMSSender` 会在其装配期直接失败（`ErrMissingDistributedSMSSender`），不会静默退化成打印到 stdout。真正的运营商适配器在 `pkgcore/sms/`（`aliyun`、`tencent`、`twilio` 三个子包，每个一个 `NewSender` 构造器返回 `pkgcore.SMSSender`，由宿主照常装配）：各自按官方 API 做真实签名与请求，全部标准库手写、零新增依赖。其中 `aliyun` 与 `tencent` 没有自由文本通道，是模板型适配器——`Config.Templates` 按 `"<locale>/<message-id>"` 映射到账户已审批模板及其声明的变量清单，映射未命中、`MessageID`/`Locale` 为空或声明变量缺值都会在发出请求前失败（fail-closed，无兜底模板、不回退自由文本）；`twilio` 保持自由文本。仓库自身从未在真实网关前跑过这三个适配器，对真实账号的验收以 `ALIYUN_SMS_*`/`TENCENT_SMS_*`/`TWILIO_SMS_*` 环境变量门控的集成 leg 形式存在（缺凭据时带说明自跳过，alipay 沙箱 leg 的先例），见 `go/pkgcore/AGENTS.md` 的 "SMS carrier adapters" 一节。

"认证"一行的"密码登录"半边也已落地，并且比这行原文写得更多：`go/authn` 除密码外还实现了手机号+短信验证码登录、五个社交登录渠道（Google/GitHub/微信开放平台/钉钉/飞书）、按租户配置的企业 OIDC 单点登录、TOTP 二次验证与恢复码、会话/设备自助管理。"内置种子账号"路径不存在——`Taskfile.yml` 的 `seed` 任务是未实现的 stub；参考 app 的演示账号由 `APP_DEMO_USERS_PASSWORD` 在启动时经真实 register 路由注册并授予演示租户的成员身份与授权（`cmd/server` 的 demo subject resolver 按 `X-Demo-User` 头解析调用者身份）。一个新注册账号在被显式授予租户成员身份之前无法登入任何租户——这是 `go/authn/service.go`'s `resolveTenant` 的既定 fail-closed 行为，不是缺陷（`go/authn/AGENTS.md` 的 Known limitations 有完整记录）。
## 必须遵守的约束

1. **业务代码只依赖接口，永远不 import 具体实现**。`billing` 依赖 `pkgcore.KVStore`，不 import `go-redis`。
2. **接口选型以能力最弱的一方为准**。锚点是"该 seam 所有已注册实现中最弱的那个"，不是"单进程实现"——`KVStore` 因此不暴露 Redis 特有的 Lua 脚本能力；确需原子操作时，接口层定义 `IncrByFloat`/`CompareAndSwap` 这类可被所有实现满足的语义。
3. **业务代码看不见部署模式，也看不见选了哪套实现**。不允许 `if mode == "standalone"` 散落在业务逻辑里——这类分支只允许存在于 Kernel 的装配代码中。这条纪律因此容易守：业务代码根本拿不到可供分支的全局模式。
4. **单副本部署的独占校验**：进程无法自知被部署了几个副本，因此靠**独占锁**来保证——启动时对 SQLite 数据文件取排他文件锁并写入实例标识，第二个进程会立即失败退出并打印明确原因。这比"检测副本数"可靠得多，也顺带防止了两个进程同时写坏同一个 SQLite 文件。
5. **不满足 `SurvivesRestart` 的实现必须在启动时显式声明**：装配了这类实现时打印醒目横幅，写明哪些数据不保证跨重启存活。注意这是**该实现**的性质，不是单进程部署的性质——计费级计量走 outbox、与业务写同一事务落库（见 [06 计费与计量](06-billing-and-metering.md)），这条路径在 SQLite 上同样持久，不因单进程而失效。
6. **每套实现独立成包（优先子包，而非新开模块）；编译期引入哪些实现由应用组装者决定，框架不代劳**。约束 1 管源码层面（业务代码不 import 实现），这一条管打包层面（实现的依赖会不会落到接口消费者头上）——两者不能互相替代，因为 Go 按包而非按符号解析依赖，同包内联的实现无法被消费者裁剪。需要运行期在多套实现间切换的应用，把它们全部 import 进来即可，该能力一分不减。添加实现的 PR 必须附上实测的依赖增量。理由与测法见"实现注册表"节。

## Compose 分层

按需组合，而非一个大文件。仓库里落地的编排在 `examples/reference-app/`：

```
docker-compose.yml             # standalone 拓扑：app + 数据库
docker-compose.distributed.yml # 分布式拓扑：app + redis + rustfs + mailpit
```

其余编排材料（observability 叠加、LGTM 栈、支付沙箱等 dev-tools 组合）尚未落地。

## 对可测试性的额外收益

进程内的那批实现（内存 `KVStore`、channel 总线、`ConsoleMailer`、本地目录 `ObjectStore`）同时就是单元测试的 test double，不需要为测试再造一套 mock，也让 CI 无需拉起 testcontainers 就能跑绝大多数测试（只有双方言 SQL 兼容性测试、以及需要真实 Redis / RustFS 的集成测试才需要容器）。这是这套接口抽象除"部署灵活"之外的第二个正收益，值得为它多付出抽象成本。

## 当前实现状态

**本文描述的设计已落地**。实现分布：

- `go/pkgcore` 新增的装配机制文件：`capability.go`（`Capability` 位掩码与 `Has`，三个能力位 `MultiReplicaSafe`/`SurvivesRestart`/`Stateless`——`mailer.console` 这类无状态实现没有可跨重启丢失的数据，`SurvivesRestart` 横幅对它们空洞，于是以 `Stateless` 位豁免，警告只对有状态却不持久的实现（内存 `KVStore` 等）打出）、`seam_registry.go`（`SeamRegistry[T]`/`Registration[T]`，镜像 `database/sql` 的驱动注册模式——模块内目录注册表的机制，pki 的 `SignerRegistry` 等在其上构建）、`component.go`/`component_registry.go`（组件描述符与组合配置：每个组件以组件名注册，配置块携带该组件自己的参数），以及根包内置实现的按 seam 注册文件（`eventbus_memory.go` 注册 `eventbus.memory`、`kv_memory.go` 注册 `kv.memory`——这两个文件的注册与各自的内存实现同文件；`mailer_registry.go` 注册 `mailer.console`/`smtp`、`objectstore_registry.go` 注册 `objectstore.local`；共享的 `mustRegister` 帮手与 `ErrMissingSeamConfig` 在 `registries.go`；Redis/PostgreSQL/Memcached/NATS 等分布式实现不在这些文件里，各自在自己的子包 `init()` 注册，见下）。`app.Assemble(ctx, reg, spec)` 不以部署模式为首位参数：组合配置的 deployment 字段只声明拓扑，选中哪些组件由宿主决定（或由配置驱动的 loader 从组合配置解析），宿主自建的值经 by-type 上下文注入。装配的 `Prepare` 阶段对每个选中组件做一次能力校验：组件描述符声明的能力位不满足所声明模式要求的 `RequiredCapabilities()` 即启动失败，错误 wrap `ErrCapabilityUnsatisfied` 并点名组件、缺失能力与模式（错误集合里没有 `ErrMissingDistributed*` 哨兵）；不满足 `SurvivesRestart` 的实现照常启动、但打印横幅警告（约束 5）。
- **`go/pkgcore` 的 Redis/S3 内置实现分居各自子包**（"实现注册表"节原则的落地）：`redis_kv.go`/`redis_eventbus.go`/`s3_objectstore.go` 从根包搬到 `go/pkgcore/kv/redis`、`go/pkgcore/eventbus/redis`、`go/pkgcore/objectstore/s3` 三个子包，`NewRedisKVStore`/`NewRedisEventBus`/`NewS3ObjectStore` 相应更名为各子包自己的 `NewKVStore`/`NewEventBus`/`NewObjectStore`（`S3Config` 随之更名 `s3.Config`），各自 `init()` 把 `kv.redis`/`eventbus.redis`/`objectstore.s3` 组件描述符自注册到 pkgcore 的全局组件注册——内置组合指向的组件名不变，但唯有宿主 import 了对应子包（哪怕只是 `import _ "..."`）才能选中，否则装配的 `Prepare` 阶段报未知组件，`database/sql`"忘记 import 驱动"式的编译期错误变运行期错误，正是"实现注册表"节接受的代价。只 import 根包 `pkgcore` 的消费者在 `go.mod`/`go.sum` 里不再出现 Redis、S3 两套 SDK 的依赖条目（量法见"实现注册表"节）——依赖闭包对根包消费者的成本归零。`.golangci.yml` 的 `redis-only-in-pkgcore-and-jobs`/`minio-only-in-pkgcore` 两条 depguard 豁免收窄到具体子包路径，使"实现不得与其他 seam 的实现共享一个包"这条纪律可被 lint 捕获。三个子包各自拥有独立的 `integration_test/`（Docker-backed），不共享 `go/pkgcore` 根包的集成层；`go/dbkit`/`go/observability`/`go/jobs` 的同类拆分见各自模块的条目。
- **`KVStore` seam 的 PostgreSQL 实现：`kv/postgres`**，单表 `pkgcore_kv_entries`（`key`/`value`/`expires_at` 三列）+ TTL 列，供已经在跑 PostgreSQL、不想为这一个 seam 再拉起 Redis 的部署使用，与 `eventbus/postgres` 的"零额外基础设施"理由一致（姊妹实现）。自身 `init()` 把 `"kv.postgres"` 组件自注册，与 `kv.redis` 并存，内置组合的 `"kv"` 模块不变（仍指向 `kv.redis`）——想用这套实现的宿主在自己的组合配置里把 `"kv"` 指向 `kv.postgres` 并给出 `dsn`（组件自行建池、由装配管理生命周期），或直接调 `kv/postgres.NewKVStore` 并把值放进 by-type 上下文，注入宿主持有的池。`IncrByFloat`/`CompareAndSwap` 各是一条数据库仲裁的单条 SQL 语句——前者是 `INSERT ... ON CONFLICT DO UPDATE` 的算术更新，后者是一个 `WITH` CTE：一条 `UPDATE`（键存在分支，含"过期视为不存在"的重置语义）配一条由 `NOT EXISTS` 守卫、`ON CONFLICT DO NOTHING` 兜底的 `INSERT`（键确实不存在分支）——而不是 Go 层的读-改-写，`integration_test/` 里两个专门的并发对抗测试（`TestKVStore_ConcurrentIncrementsLoseNoUpdates`、`TestKVStore_ConcurrentCompareAndSwapSetIfAbsent_ExactlyOneWinner`）分别拿 50 个 goroutine 打同一个 key，验证前者不丢更新、后者恰好一个赢家且输家从不报错，这正是 `go/billing` 的 `applyBalanceDelta` 已经确立的同一条纪律；这两条 SQL 里 `$2`/`$3` 每处出现都显式转 `::bytea`——PostgreSQL 的隐式类型推断会把 `octet_length($2)` 的参数解析成 `text`，导致后续 `value = $2` 与 `bytea` 列比较时报 `operator does not exist: bytea = text`；该行为由上述并发测试与全部 `CompareAndSwap` 单测/集成测试钉住。PostgreSQL 没有 Redis 式的主动过期，`Get`/`IncrByFloat`/`CompareAndSwap` 靠 `expires_at IS NULL OR expires_at > now()` 这道 WHERE 守卫把过期行当不存在处理（懒惰式，与 `pkgcore.memoryKVStore` 的既有约定一致），但不会主动物理删除——`Store.Sweep`（一次性物理 `DELETE`）补这一半，宿主自己按需调度，`NewKVStore` 因此返回具体类型 `*Store` 而非裸接口 `pkgcore.KVStore`，好让想用 `Sweep` 的宿主不必做类型断言。迁移文件 `migrations/postgres/0001_create_pkgcore_kv_entries.sql` 没有 `sqlite/` 对应物——standalone 模式从不跑这套实现（`pkgcore.NewMemoryKVStore` 已经覆盖，无需数据库）——这一先例由 `eventbus/postgres` 自己的迁移文件先立下；`schema.go` 的 `EnsureSchema` 因此是本包自成一体的迁移执行器（幂等的 `CREATE TABLE/INDEX IF NOT EXISTS`，没有 `schema_migrations` 台账），原因与 `eventbus/postgres.EnsureSchema` 完全相同：`go/pkgcore` 是依赖关系最底层的模块，不能反过来 import 位于它之上的 `go/dbkit`。依赖成本按"实现注册表"节的量化方法实测：由于 `go/dbkit` 的 PostgreSQL 方言本就依赖同一个 `jackc/pgx/v5`，对已经在用 `dbkit`+PostgreSQL 的宿主而言，这套依赖早已在其构建图内，净增量趋近于零。
- **`go/pkgcore/eventbus/postgres`：`EventBus` 的 PostgreSQL 实现，`LISTEN`/`NOTIFY` 配合持久化 outbox 表**——面向"已经在跑 PostgreSQL、不想单为这一个 seam 再拉一套 Redis/NATS"的部署，零额外基础设施：`NewEventBus(pool *pgxpool.Pool, replicaID string)` 与 `eventbus/redis.NewEventBus` 同源同构地拆在自己的子包里（同样的依赖隔离理由），自己 `init()` 把 `"eventbus.postgres"` 组件自注册；内置组合的 `"eventbus"` 一项不变，仍指向 `"eventbus.redis"`，想用这套实现的宿主在自己的组合配置里指向它，或直接注入。跨副本投递语义是**扇出**，与 `eventbus/redis` 完全一致（依据是 `eventbus/redis` 自己 doc comment 记的"每个副本一个 reader、每条事件送达每个副本恰好一次"，以及 `go/notification` 真实通过的 `TestRedisBus_DeliveredInbox_AnnouncesAcrossReplicas`），而不是把 `NOTIFY` 天然的广播语义硬拗成"恰好一个副本收到"的负载均衡契约——本子包自己的 `integration_test/` 用两个不同 `replicaID`、共享同一个 `pool` 的 `EventBus` 实例各自 Subscribe 同一 Type 后各发一次 Publish 验证了这一点。持久化机制：`Publish` 在同一个事务里先 `INSERT` 一行到 `pkgcore_eventbus_outbox`、再 `SELECT pg_notify(...)`，取"NOTIFY 前先落 outbox"的顺序——`NOTIFY` 本身对断线或尚未 `LISTEN` 的会话没有任何补发能力，outbox 才是真正的事实来源；每个副本自己的监听 goroutine 持有一条独立于 `pool`、专门用于 `LISTEN` 的 `*pgx.Conn`（从不向 `pool` 借用也从不归还，避免 `LISTEN` 会话污染常规查询连接池），在收到真实 NOTIFY 或每 2 秒一次的周期性超时（两者同一处理路径，无需区分）时，对每个本地已 Subscribe 的 Type 扫描 `pkgcore_eventbus_cursor` 表记录的、按 `(replicaID, eventType)` 持久化的水位线之后的 outbox 行并投递。与 `eventbus/redis` 相比这里真正多出的、也是本实现存在的全部理由：**`replicaID` 由宿主提供且必须跨该副本自身的重启保持稳定**，于是重连后的水位线读的是数据库里持久化的值,而不是 Redis 消费者组那种"新建组=从末尾重新起跳"，一次真实进程重启窗口(`TestEventBus_CatchUp_MissedNotifyIsDeliveredAfterReconnect`)与一次真实连接中途断开重连(`TestEventBus_CatchUp_ReconnectMidStream_DeliversWhatArrivedWhileDisconnected`，`pg_terminate_backend` 强制断线)都各有一条集成测试直接证明"NOTIFY 期间错过的事件在重连后被补上"，而不只是断言"活着的时候能收到"。首次 Subscribe 仍以 outbox 当前末尾为起点(与 `eventbus/redis` 的 `"$"` 语义一致)，因此 Subscribe 返回与监听 goroutine 真正跑起第一次扫描之间存在与 `eventbus/redis` 消费者组创建同源的竞态——本子包与 `go/notification` 对 `eventbus/redis` 的证明手法相同，用"反复重发直到观察到投递"的 `warmUp` helper 绕开而非试图消灭它。outbox 表没有 Redis Stream 那种自动裁剪，`postgres.PurgeOutboxBefore(ctx, pool, olderThan)` 是本包显式的、需宿主自行调度的对应物。迁移：`postgres.EnsureSchema(ctx, pool)` 应用本包 `migrations/postgres/` 下唯一一份版本化 SQL（幂等的 `CREATE TABLE IF NOT EXISTS`），且**没有 `migrations/sqlite/` 对应物**——这是刻意的、有文档记录的例外：机制本身是 PostgreSQL 专属，standalone 部署模式压根不会跑到这套实现，之所以没有平行的 SQLite 产物,先例是 `go/pki` 的 `signer/vault`/`signer/kmsaws` 两个子包(同样一份迁移都不带,理由相同);本包因此也绕开了 `dbkit.MigrationRegistry`(`go/pkgcore` 是依赖地板,不能反向 import `go/dbkit`),`EnsureSchema` 是自成一体的替代品。依赖成本按"实现注册表"节的量化方法实测：对一个已经依赖 `go/dbkit` PostgreSQL 方言（`go/dbkit/dialect/postgres`，同一份 `jackc/pgx/v5`）的消费者而言，这套依赖早已在其构建图里，即**新增第三方依赖为零**——这正是本实现相对 `eventbus/redis`（无论宿主是否已经在用 Redis 都得再拉一次 go-redis）在纯 PostgreSQL 部署下的实际优势。
- **`EventBus` seam 的 NATS JetStream 实现：`go/pkgcore/eventbus/nats`**：与 `eventbus/redis` 同构的一处新子包，组件名 `eventbus.nats`（不被任何内置组合指向——内置组合的 `eventbus` 模块仍是 `eventbus.redis`；宿主要用这套实现，在自己的组合配置里指向它并给出 `url`，或直接注入）。投递语义与 `eventbus.redis` 逐条对齐而非另立一套：每个事件类型一个 JetStream stream（`Publish` 首次发布时按需 `CreateOrUpdateStream`，弥补 JetStream 没有 Redis Streams 那种 `XADD` 自动建流的能力），每个 bus 实例在该 stream 上创建自己唯一命名的 durable pull consumer（消费者名嵌入随机实例 id）——JetStream 的负载均衡只发生在"同名消费者"之间，实例 id 保证互不同名，因此每个实例的 consumer 各自独立收到每条消息，形成与 Redis 消费者组方案完全相同的**广播到每个副本**而非**负载均衡到某一个副本**的语义。这条语义由 `integration_test/eventbus_test.go` 的 `TestEventBus_FanOutDeliversToEveryReplica`（两个独立 `*nats.Conn`，标记两个副本，一次 `Publish`，断言两个 handler 都触发）针对真实 NATS 容器直接验证，而不是从代码读出来就采信；同一目录另跑一遍 `eventbustest.AssertConforms` 共享契约套件，并把 `eventbus/redis` 集成层的其余覆盖（恰好一次的本地/远程投递、按类型隔离的 stream、非 JSON payload 发布前失败、远程 handler panic 不冲垮 reader、`Close` 停止投递并只清理自己创建的 consumer、晚订阅者不补历史、consumer 被外部删除后自愈）逐项搬了过来。能力位诚实取自 JetStream 自身：`MultiReplicaSafe|SurvivesRestart`（JetStream 默认 `FileStorage`，已提交的消息扛得住 broker 重启，与 Redis Streams 的 AOF/RDB 持久化是同一诚实性）。`Publish` 遇到发布失败时会清空本地缓存、强制重建 stream 后重试一次：一个只发布、从不订阅的实例在 stream 被另一个实例（作为"最后一个 consumer"）关闭删除后，会因自己 `ensureStream` 的本地缓存过期而发布失败（`no response from stream`）——Redis 因为 `XADD` 自动建流从不会有这个问题；`TestEventBus_Close_StopsPublishAndRemoteDelivery` 钉住该行为。依赖成本按本文件"必须遵守的约束"一节的量法单独测量：子包分包这条纪律本身保证只 import 根包 `pkgcore` 的消费者依然为零。
- **`KVStore` seam 的 Memcached 实现：`go/pkgcore/kv/memcached`**：与 `kv/redis` 同构的一处新子包，注册名 `kv.memcached`，客户端选用零依赖的 `github.com/bradfitz/gomemcache`（其 `Get` 内部即走 Memcached 的 `gets` 命令并回填 CAS token，配合原生 `cas`/`add` 已经具备本实现需要的一切原语）；本文件"必须遵守的约束"一节的量法测得：一个只 blank-import 它的抛弃式模块，`GOWORK=off go mod tidy` 后 `// indirect` 条目为 **0**——`gomemcache` 自身零传递依赖，对照 `kv/redis`、`kv/postgres` 与 `eventbus/nats` 都不是零。能力位诚实取自 Memcached 自身而非照抄 `kv.redis`：只声明 `MultiReplicaSafe`（多副本共享同一台 Memcached），**故意不声明** `SurvivesRestart`——Memcached 是纯内存缓存，没有任何持久化机制，进程重启、滚动升级或者服务器在内存压力下的正常 LRU 淘汰都会无声丢光这个 seam 持有的一切，且不向任何调用方报错；这与 `kv.redis`（AOF/RDB）或数据库、文件系统支撑的实现是本质不同的风险画像，包文档与 `AGENTS.md` 都用大白话写明，不是只标一个能力位就算交代过去。也正因为这条诚实声明，`kv.memcached` **不被任何内置组合指向**（内置组合的 `kv` 模块仍是 `kv.redis`）——宿主要用它必须在自己的组合配置里显式指向它（`addrs`）或直接注入，不像 `kv.redis` 是内置分布式组合的零配置默认。
  实现上最棘手的一点是 TTL 粒度错配：Memcached 协议的 `exptime` 字段只到秒级（30 天以内是"从现在起 N 秒"，超过 30 天则被服务器重新解释为绝对 Unix 时间戳，本实现验证并正确处理了这个协议阈值），而 `kvstoretest.AssertConforms` 共享契约套件故意用几十毫秒的 TTL 来保持整套一致性测试的速度（`kv/redis` 靠 `PEXPIRE` 的毫秒粒度原样通过同一套件）。把亚秒级 ttl 直接向上取整到 Memcached 能表达的最小单位——一秒——会让契约套件在 125ms 后回读时发现键还物理存在，静默违反"短 ttl 到期即消失"这半条契约。解法是这套实现在 Memcached 粗粒度的物理过期之上，自己维护一层逻辑过期"信封"（8 字节大端绝对到期时间戳，全零表示永不过期，后跟调用方原始字节）：每次写入都把这层信封存进去，Memcached 自身的物理 `exptime` 仍然照常设置（取逻辑到期时间与至少 1 秒之间的较大者，作为"即使再没人读它也终会被服务器物理回收"的兜底，从不是"这个键还在不在"的裁决者），每次读取都先按本地墙钟核对信封里的绝对时间戳，物理上还没被淘汰但逻辑上已过期的行会被这次读取顺手删除并报告"不存在"，与其余三套实现对"过期键视为缺失"的处理完全一致。`IncrByFloat` 与 `CompareAndSwap` 同样建在这层信封之上：Memcached 的原生 `incr`/`decr` 只支持 64 位无符号整数，协议层完全没有浮点原语，也没有任意字节值的原子比较，两者都实现成对 Memcached 原生 `gets`/`cas`/`add` 的比较-交换重试循环——键不存在或逻辑已过期时走 `add`（Memcached 原生的"仅当不存在时写入"），键存在时走 `cas`（借这次 `gets` 读到的 token 做条件写），输掉竞态（`ErrCASConflict`/`ErrNotStored`/`ErrCacheMiss`）不是错误，从新的 `gets` 重新来过，有限次数加短退避封顶，真正病态的持续竞态才会诚实报错而不是无限自旋。这两条循环各自配一条真实并发攻击性测试佐证：多协程同时对同一个键 `IncrByFloat`，断言精确的总和且没有更新丢失；多协程同时对同一个键跑 `CompareAndSwap` 的"仅当不存在时写入"形态，断言"恰好一个赢"。集成层跑在真实 Docker Memcached 容器上（`go/pkgcore/kv/memcached/integration_test/`），既跑通了上述两条攻击性测试，也把 `kvstoretest.AssertConforms` 共享契约套件原样跑了一遍并通过——证明这套信封机制确实弥合了协议粒度的落差，而不是回避了它。
- **`KVStore` seam 的 NATS JetStream KV 实现 `kv.nats`（`go/pkgcore/kv/nats`）**：`NewKVStore(ctx, nc, bucket)` 用一个已连接的 `*nats.Conn` 与一个 bucket 名构造，构造期用 `CreateOrUpdateKeyValue` 幂等地把 bucket 建好（这一点与 `kv/redis` 不同——`redis.NewKVStore` 不拨号，JetStream 的 bucket 是服务端必须先认可的 schema，不是客户端可以凭空假设存在的裸命名空间，所以这个构造函数要带 `ctx` 且可能失败）。JetStream KV 只有按版本号比较的 `Update(key, value, revision)`，没有原生按值比较、也没有原生原子自增，`CompareAndSwap`/`IncrByFloat` 因此都建在这一个版本号校验之上：读到的版本号只决定「接下来该发 `Create` 还是发 `Update`」，真正的原子性来自写入那一步服务端做的版本号校验——`Create` 撞见 `ErrKeyExists`、`Update` 撞见 `ErrKeyRevisionMismatch` 都被当作「swap 没发生」（`false, nil`），从不当错误；`IncrByFloat` 把同样的判断包进一个有限重试的 CAS 循环（上限 200 次、指数退避封顶 20ms）。过期时间是这层实现自己在被存值前加的信封（8 字节大端 Unix 纳秒头 + 原始值），从不使用 bucket 级 TTL——bucket TTL 是全 bucket 一个值，表达不了"自增在续命现有 key 的过期时间上创建新 key 时却不带过期时间"这条 `KVStore` 接口硬性要求的语义。JetStream 的 key 字符集不含冒号，而 `KVStore` 的 key 是不做限制的不透明字符串（`kvstoretest`/`kv/redis` 自己的测试就大量使用 `billing:invoice:1042` 这样的 key），所以这层实现把每个 key 十六进制编码后再落到 JetStream 上——无损、单射，对任何输入都合法。声明的能力位与 `kv.redis` 相同（`MultiReplicaSafe|SurvivesRestart`，默认 `FileStorage`），但 **`kv.nats` 没有被接进内置分布式组合**——它只让该组件可被选中，不改变任何既有组合的默认选择；一个想用 NATS 而非 Redis 或 PostgreSQL 的宿主在自己的组合配置里把 `kv` 指向它（`url`，可选 `bucket`），或注入一个已连接的 `*nats.Conn`。`go/pkgcore/kv/nats/integration_test/`（Docker-backed，JetStream-enabled 的真实 NATS 容器）跑通共享的 `kvstoretest.AssertConforms`，外加两个必需的对抗性并发证明——40 个 goroutine 各自 15 次 `IncrByFloat` 竞争同一个 key，最终和精确等于 600，没有一次触及重试上限；30 个 goroutine 竞争同一个从未写过的 key 的 `CompareAndSwap` set-if-absent，最终只有且恰好一个赢家——以及一个真实的服务器重启存活证明（同一容器 stop/start，客户端凭 `nats.go` 默认的自动重连恢复操作，且重启前写入的数据仍在，证实 `SurvivesRestart` 不只是声明）。**依赖成本（净增口径）**：`kv/nats` 对本仓库的净新增第三方依赖为零——`nats.go` 连同其间接依赖已是根 `go.mod` 的既有直接依赖（由 `eventbus/nats` 引入，两实现 pin 同一个 `github.com/nats-io/nats.go`），它复用的是同一份依赖闭包，而不是第二次单独付费。`.golangci.yml` 的 `nats-only-in-pkgcore` depguard 规则同时豁免 `eventbus/nats` 与 `kv/nats` 两个子包。
- **组装的「参数通道」与「逐项覆盖」已按正文落地**：组合配置把每个选中组件映射到自己的配置块（组件名 + 该组件的参数），配置块由组件的 `New` 通过 `ComponentConfig.Decode` 解出：各内置实现文档化的配置键（redis 系的 `addr`/`password`/`db`、nats 系的 `url`/`token`/`user`/`password`/`bucket`、`mailer.smtp` 的 `host`/`port`/`username`/`password`/`tls_mode`/`insecure_skip_verify`/`reply_to`、`objectstore.s3` 的 `endpoint`/`bucket`/`access_key`/`secret_key`/`region`/`use_ssl`/`bucket_lookup`、memcached 的 `addrs`、postgres 系的 `dsn`/`replica_id`）在组合配置里全部可达。逐项覆盖层的落地形态是在宿主自己的组合配置里逐键写：从不写穿共享的内置组合，就地赋值会改写同一进程内所有后续装配的默认。`pkgcore` 自身仍从不读配置文件或环境：宿主自己的 koanf/环境层在上，把解析好的组合配置喂进来（与 `Config.OTLPEndpoint` 的喂法相同，见 `pkgcore/component.go` 的 doc comment）。内置组合的组件配置块本身不带凭据——那是「命名组件，不携带部署」的诚实形状，pkgcore 不发布任何凭据——所以裸的内置分布式组合下两个 Redis 组件仍落到裸默认地址（`localhost:6379`，无鉴权）、SMTP/S3 组件仍以 `ErrMissingSeamConfig` 构造失败；区别在于需要真实凭证的宿主不再被迫走代码注入：在组合配置里给 `mailer.smtp` 组件一个带 `host` 的配置块即为「配置文件逐项覆盖」的落地形态（`examples/reference-app` 的 SMTP 与 S3 两个组件走的正是这条路径），需要共享既有资源或 typed 配置（TLS、池对象、共享 client）的宿主自写组件描述符包住自己的对象，或直接注入 by-type 上下文——注入恒优先于组合选择（按类型）。参数通道的端到端证明在 `go/app` 与各子包的组件测试里：组件的配置块携带真实容器的映射地址（自由端口），构造出的实现拨到该地址、装配关闭时归还全部连接。装配校验、横幅警告与能力位已按正文落地。
- 「契约测试」节的目标形态已是仓库标准：`go/pkgcore/eventbustest`、`kvstoretest`、`mailertest`、`objectstoretest` 四个 `AssertConforms(t, factory)` 套件，每套内置实现各跑一遍——真实 Redis/RustFS 上的实现跑在各自子包自己的 Docker 集成层（`go/pkgcore/eventbus/redis`、`kv/redis`、`objectstore/s3` 各自的 `integration_test/`），SMTP 走进程内 fake relay，留在 `go/pkgcore` 根包自己的单元测试层（矩阵形态见 [16 验证](16-verification.md) §2 与 [18 CI/CD](18-cicd.md)）。
- `go/observability` 的导出器分包：OTLP 与 Prometheus 导出器在 `exporter/otlp`、`exporter/prometheus` 两个子包，各自 `init()` 调用根包的 `RegisterOTLPExporters`/`RegisterLocalMetricsReader` 单槽注册（约束 6 的同一模式，槽位而非按名注册表，因为每类只有一套内置实现）。`Init(ctx, opts...)` 不接收部署模式，导出器选择只取决于是否提供 `WithOTLPEndpoint`——单进程组装可以把遥测送往真实 Collector（错误集合里没有 `ErrMissingOTLPEndpoint`）。未 blank-import `exporter/otlp` 时 `WithOTLPEndpoint` 直接报错点名该 import；未 blank-import `exporter/prometheus` 时 `MetricsHandler` 的本地拉取端点默认 404、需显式选用——`examples/reference-app` 与 `go/saasctl` 的生成项目模板（`cmd/server/main.go`，其中 `exporter/otlp` 的导入服务于 `APP_OTLP_ENDPOINT` 接线）均已 blank-import 以保住其 `/metrics` 路由（闭：`21a656c5`，2026-09-10；记录见 `go/observability/AGENTS.md`）。只 require 根包的消费者不再背 gRPC/protobuf 与两套 Prometheus 包的依赖（量法见"实现注册表"节）。
- `go/jobs`：`StandaloneQueue` 与 `asynq.Queue` 由宿主直接选择、本身不读部署模式，形态与本文一致。打包隔离：`asynq.Queue` 连同 `hibiken/asynq`/`redis/go-redis/v9` 一并搬在 `go/jobs/queue/asynq` 子包，`StandaloneQueue` 留在模块根包，其非测试代码的第三方 import 限于模块既有的 `gorm.io/gorm`/`go.opentelemetry.io/otel`/`github.com/google/uuid`，`hibiken/asynq` 与 `redis/go-redis/v9` 只在子包——只是包位置的收敛，措施与 `go/dbkit` 方言驱动、`go/pkgcore` 的 Redis/S3 实现拆分子包同源（量化对比见 `go/jobs/AGENTS.md`「Packaging: why asynq.Queue is a subpackage」一节）。Queue seam 尚未组件化/能力声明化——未纳入 `pkgcore.Component`/装配能力校验/组合配置。
- `examples/reference-app`：入口接受任何部署模式（`internal/app/server.go` 读 `APP_DEPLOYMENT_MODE`，默认 standalone）；`APP_REDIS_ADDR` 把「eventbus」**和**「kv」两个 seam 一起换成同一个真实 Redis 客户端支撑的实现（`kv/redis.NewKVStore` 与 `eventbus/redis.NewEventBus` 签名相同，共享一个 `*redis.Client`）——两条路径分工的示范：共享 typed 资源（一个 `*redis.Client`）走裸注入，`APP_S3_*`（`objectstore.s3`）与 `APP_SMTP_*`（`mailer.smtp`）这类可完全用扁平字符串表达的组装走组合配置（在组合配置里给对应组件的配置块，宿主不预建任何对象），「SMS sender」这一个共享选项 seam（`APP_SMS_GATEWAY_URL`，`pkgcore.NewHTTPSMSSender`——分布式模式下这个变量留空会让 `authn.NewModule` 自己的装配期校验直接拒绝，`ErrMissingDistributedSMSSender`）无内核座位、照旧条件注入；standalone 拓扑 + `MultiReplicaSafe` 实现的组装正是两条轴正交的演示，四个 `pkgcore` kernel seam 加 SMS 这一个共享但刻意无内核座位的 seam 都可以被指向真正满足 `MultiReplicaSafe` 的实现。**部署模式轴的证明**：`examples/reference-app/integration_test/distributed_mode_test.go` 启动两个真实子进程，都声明 `APP_DEPLOYMENT_MODE=distributed`，都指向同一个真实 Redis、同一个真实 RustFS 桶、同一个真实 SMTP 收信容器——replica B 以 `APP_DISABLE_QUEUE_WORKER=true` 启动，使其自身永远不会执行投递任务，测试在 replica A 创建笔记之前先打开 replica B 自己的 SSE 收件箱流（`GET /api/v1/notifications/stream`），断言 `EventInboxCreated` 的通知帧仍然送达 B——由于两个副本共享同一个 SQLite 文件，这个结果只有真实的、基于 Redis 的 EventBus 确实跨了进程边界才能解释，共享的 jobs 表本身无法解释；另一条子断言分别对同一账号在两个副本上各发起一次错误密码登录，验证 replica B 能读到 replica A 那次尝试写入的账号锁定状态，证明 KVStore seam 同样跨了进程边界，而不是一个只在单进程内部生效的 in-process channel；对偶地，一次装配不完整的分布式启动，通过真实二进制的进程退出码而不只是 `flowtests/server_guards_test.go` 的进程内断言，同样 fail closed。记录在案的偏离：两个副本仍共享同一个 SQLite 文件（`examples/reference-app` 的数据库方言硬编码为 SQLite，见该测试文件自己的包注释），所以这不是「部署模式 × 数据库方言」的组合矩阵证明，而是部署模式这一条轴独立于方言的证明。其 Docker-backed 集成测试（`examples/reference-app/integration_test/`，随 full-check 的 reference-app job 运行）以真实子进程启动整机，跨进程断言事件经真实 Redis 送达。

破坏面（`app.Assemble` 签名、`ErrMissingDistributed*` 哨兵、`observability.Init` 签名）集中在 `go/pkgcore` 与 `go/observability` 两个模块；`DeploymentMode` 类型与常量、`ParseDeploymentMode`、`Kernel.DeploymentMode()` 保留——模式仍是公开 API，只是不再选择实现。
