# 横切能力：国际化、配置管理、功能开关、限流

> 这四项都是贯穿多个模块的横切关注点。前三项必须从第一天就位——事后补的代价远高于前置投入；限流则是另一种典型的横切模式：多个业务模块各自独立需要同一种能力，与其各自重新发明、日后再整合，不如提前抽成共享原语一次做对。

## 国际化：默认支持中文与英文

**这是必须从 M0 就位的横切能力，不能后补。** 多个 UI 包一旦硬编码文案发布出去，回头补 i18n 是全量返工，且业务方已经基于硬编码文案做了定制，改动会破坏他们的项目。

**语言协商链**（优先级从高到低）：URL 参数 / 用户手动切换（存 localStorage）→ 用户 profile 的 `locale` 字段 → `Accept-Language` 请求头 → 默认 `zh-CN`。前后端使用同一套解析结果，避免出现"界面中文、邮件英文"。

**后端**
- 新增 `pkgcore/i18n` 子包，选 `nicksnyder/go-i18n`（支持复数形式与嵌套消息，生态成熟）。
- **API 响应默认不返回翻译后的文案，而是返回结构化错误码 + 参数**（如 `{"code":"billing.quota_exceeded","params":{"limit":1000}}`），由前端翻译。这样同一个后端能同时服务不同语言的客户端，也便于业务方覆盖文案。
- **后端自己生成的内容必须后端翻译**：邮件（邀请、密码重置、超额提醒）、导出的账单/发票、Webhook 通知文本、运营后台的审计日志描述。这类内容用收件人的 `locale` 而非请求方的语言渲染——给英文用户发的邀请邮件不能因为操作者是中文界面就变成中文。
- 每个 Go module 在自己目录维护 `locales/{zh-CN,en-US}.toml` 并 `embed.FS` 暴露，由 Kernel 装配时合并注册（与 migrations 的聚合机制一致）。
- 时区同样按用户偏好处理：数据库统一存 UTC，展示层转换。

**前端**
- 新增 `@speed/i18n` 包（位于 `api-client` 同层），封装 `react-i18next` 实例创建、语言检测、懒加载与 MUI locale 联动（切到英文时 MUI 组件的内置文案也要跟着变，`@mui/material/locale` 的 `zhCN`/`enUS`）。
- **每个 UI 包自带自己的 `locales/{zh-CN,en-US}.json`，用包名作为 namespace**（`auth`、`billing`、`tenancy`…），注册到宿主应用的同一个 i18n 实例。业务项目可通过覆盖同名 key 定制任意文案，不需要 fork 组件。
- 硬性规则：UI 包内**禁止出现中英文字面量文案**，CI 加 lint 规则扫描 JSX 中的裸文本节点。
- 日期、数字、货币格式一律用 `Intl.DateTimeFormat` / `Intl.NumberFormat`，不手写格式化；货币展示要同时正确处理 CNY 与 USD 的符号位置和小数位。
- 布局注意：德语/英语文案普遍比中文长 30%~50%，`ui-kit` 组件不得依赖固定宽度容纳文案，Storybook 里为每个组件提供中英双语 story 以便及早发现截断。

**范围边界**：v1.0 只保证 `zh-CN` 与 `en-US` 两种语言的完整覆盖，但架构上不写死为双语——新增语言只需补一份资源文件，不改代码。RTL（阿拉伯语等）方向支持不在 v1.0 范围，但 MUI 本身具备 RTL 能力，后续可增量启用。

## 配置管理：引导配置与动态配置分离

**必须先切开两类配置，否则会陷入"数据库连接串存在数据库里"的鸡生蛋死结。**

| | **引导配置（bootstrap）** | **动态配置（dynamic）** |
|---|---|---|
| 内容 | 数据库连接、Redis 地址、部署模式、**实现组装（preset 与逐项覆盖）**、监听端口、加密主密钥、日志级别 | 功能开关、套餐默认限额、SMTP 参数、AI 模型默认参数、品牌信息、可用支付渠道 |
| 来源 | 命令行 flag → 环境变量 → 配置文件 → 内置默认值 | 数据库（可经运营后台修改） |
| 时机 | 进程启动时确定，**运行期不可变** | 运行期可改，热生效 |
| 归属 | `pkgcore/config`（零依赖） | 独立 `config` 模块（依赖 dbkit + tenancy + eventbus） |

**引导配置**
- 选 `knadh/koanf` 而非 Viper：无全局单例、依赖树干净、Provider 可插拔。脚手架会被大量项目引入，Viper 的全局状态和臃肿依赖在这种场景下是负担。
- 优先级链固定为：**命令行 flag > 环境变量 > 配置文件 > 内置默认值**，写进文档不允许各模块自行发挥。
- 环境变量统一 `SPEED_` 前缀，嵌套用双下划线（`SPEED_DB__DSN`）。
- **强类型 + 启动时 fail-fast**：配置绑定到结构体，必填缺失或格式错误直接退出并打印清晰的错误（缺哪个键、从哪些来源找过），绝不允许带着空配置启动到一半才崩。
  > **实现落地更正**（Round 1）：`pkgcore/config` 最终没有引入 `go-playground/validator`，而是用 `config:"required"` 结构体标签 + 反射做必填校验——范围/格式/枚举这类更复杂的校验规则暂不支持。理由是目前还没有任何 bootstrap 配置结构体需要这类校验，先不引入这个依赖；等第一个真的需要范围/枚举校验的场景出现时，再评估是加 `go-playground/validator` 还是继续扩展手写标签系统。
- 每个模块声明自己的配置结构体片段，由 Kernel 聚合装配——与 migrations、i18n 资源的聚合机制保持一致。
- `saasctl config print` 打印最终生效配置及**每个值的来源**（来自 flag / env / 文件 / 默认值），敏感值自动脱敏。这是排查"为什么我改了配置文件没生效"的关键工具。

**动态配置**
- 存储为 `configs` 表，**三层作用域**：`system`（平台全局）→ `tenant`（租户覆盖）→ 未来可扩展到 `user`。读取时按作用域从具体到宽泛回退，这是多租户 SaaS 的刚需（每个租户可以有自己的功能开关和限额）。
- 接口：
  ```go
  type Store interface {
      Get(ctx context.Context, key string) (Value, error)        // 自动按当前租户作用域解析
      GetTyped[T any](ctx context.Context, key string) (T, error)
      Set(ctx context.Context, scope Scope, key string, v Value, by Actor) error
      Watch(key string, fn func(Value))                          // 变更回调
  }
  ```
- **Schema 先行**：每个配置项必须先注册元数据（key、类型、默认值、取值范围、是否敏感、说明文案、所属分组）。带来三个好处：运营后台可以**根据 schema 自动渲染配置表单**而不必为每个配置写页面；写入时能做类型与范围校验；文档可从 schema 自动生成。
- **热更新**：变更后经事件总线广播 `config.changed`，各实例刷新本地缓存；同时带 TTL 兜底轮询（防止事件丢失）。用的就是装配好的那套 `EventBus`：进程内实现天然即时生效，Redis Streams 一类实现跨副本广播——复用已有机制，不引入新组件。
- **敏感项加密存储**（复用 AI 网关的 AES-GCM + 环境变量主密钥方案），读取时解密，日志与 API 响应中一律脱敏，导出配置时屏蔽。
- **变更审计**：每次修改写审计日志（谁、何时、改了什么、旧值→新值），经事件总线流入 `compliance` 模块。配置误改是生产事故的常见来源，没有审计追溯排查会非常痛苦。
- **未登录时如何知道是哪个租户的配置**：白标场景下品牌、可用登录方式都是租户级的，但此刻还没有登录态。解析顺序为 **自定义域名 → 子域名 → 默认租户**（单租户部署时即平台默认值）。这份映射由 `tenancy` 的 `Resolver` 统一提供，登录前后用的是同一套解析逻辑，避免出现"登录页是 A 品牌、登录后变 B 品牌"。未匹配到任何租户时返回平台默认品牌，绝不报错——登录页打不开是最糟糕的失败模式。
- **前端运行时配置**：提供 `/api/config/public` 下发前端可见的公开配置（品牌信息、功能开关、可用支付渠道、可选语言），前端在启动时拉取。这样改品牌色、开关功能不需要重新构建前端。`@speed/api-client` 提供 `usePublicConfig()` hook。

  > **实现落地更正**（实现 `config` 模块时确认）：本节动态配置部分已按下列实际形态落地，个别表述与设计不同：
  > - **声明与冻结分离**：各模块在 `Register` 阶段只通过 `pkgcore` 的 `ConfigSchemaRegistrar`/`FeatureRegistrar` **声明**自己的配置项与功能开关；运行期 schema 要到 `Bootstrap` 走完、所有模块注册完毕才完整，因此宿主在 `Bootstrap` 返回后调用一次 `config.NewModule(...)` 的 `Attach(reg)` 冻结 schema 并取回 `Service`。声明与冻结之间的请求窗口返回 `ErrServiceNotAttached` 而不是带病服务；含 `Sensitive` 项却未注入 cipher 时拒绝启动（`ErrCipherRequired`）；重复 `Attach` 报 `ErrAlreadyAttached`。
  > - **事件名与防丢**：变更事件名实现为 `config.item.changed`（本节写的是 `config.changed`），经共享总线广播让各实例失效本地缓存；防丢兜底是后台轮询器定期重读近期更新的行（不是 TTL）。
  > - **敏感项**：加密存储、读取时解密、日志与响应脱敏均按设计落地；变更事件同样不携带明文——payload 的两个取值槽位都放 `[redacted]` 标记，明文不跨出模块。
  > - **作用域**：`system` 与 `tenant` 两层已落地（system 行以空字符串 `tenant_id` 为哨兵），读取从租户覆盖回退到 system 行再到 schema 默认值；`user` 层按"未来可扩展"预留但刻意未实现，任何写入返回 `ErrUserScopeUnavailable`。
  > - **审计**："变更审计" bullet 的落地形态是 `configs` 表行级 `updated_by`/`updated_at` 留痕 + 经共享总线发布的变更事件；专门的审计记录与 `compliance` 消费者随 `compliance` 模块的 round 落地（届时订阅 `config.item.changed` 即可，本模块不依赖审计方）。
  > - **端点**：`/api/config/public`（公开项生效值 + 依赖解析后的启用功能开关列表）与 `/api/system/features`（启用功能开关列表）都已上线：未登录可访问、只接受 GET/HEAD（其它方法 405 + `Allow: GET, HEAD`），租户经宿主注入的 `tenancy.Resolver` 逐请求解析，未匹配时回退平台默认值、绝不报错——与上面"登录页" bullet 的规则一致。响应里的"可用支付渠道、可选语言"等条目还要等对应模块注册相应公开配置项后才会出现。
  >
  >   **已落地**（config-web round）：`usePublicConfig(api)` / `useFeature(api, key)` hook 已在 `@speed/api-client` 的隔离子路径 `@speed/api-client/react` 落地（主入口保持零依赖，React 只出现在这个子路径，做法与 `@speed/i18n` 的 `./mui-locale` 一致）。两个 hook 都要求显式传入共享缓存所依据的 `RequestFn`：同一个 `api` 的多个消费者只触发一次请求，`useFeature` 直接复用 `usePublicConfig` 的缓存而不单独打 `/api/system/features`，未决或出错时返回 `false`、从不抛出。配置管理 UI 仍未交付。详见 `web/packages/api-client/README.md`（"Config hooks"一节）与 `AGENTS.md`。

**分层缓存**：动态配置读取路径在热路径上（每次权限判断、每次计量都可能读），必须走进程内缓存 + 变更失效，不能每次查库。

## 功能开关：三个层次，不要混为一谈

"允许禁用"在不同层次上含义完全不同，混在一起会造成"我明明关了为什么还在"的困惑：

| 层次 | 机制 | 回答的问题 | 变更方式 |
|---|---|---|---|
| **模块开关** | `saasctl new --with=billing,ai`，决定引入哪些 Go module / npm 包 | 这个产品**有没有**这个能力 | 构建期，改依赖 |
| **功能开关** | 动态配置里的 feature flag，支持全局与租户级 | 运营**是否开放**这个功能 | 运行期，后台可改 |
| **套餐权益** | `billing.Entitlements` | 这个租户**买没买** | 随订阅变化 |

**模块开关**
- CLI 生成骨架时按需引入，未选择的模块不进依赖、不进二进制。这是最彻底的禁用，也让不需要计费的项目不必背上三家支付 SDK 的依赖树。
- 模块间有依赖关系（`billing` 依赖 `metering`、`admin` 依赖 `rbac`），CLI 需要做依赖闭包解析并在选择冲突时明确报错。

**模块的可选性分级**（避免"我能不能不要 jobs"这类问题反复出现）：

| 级别 | 模块 | 说明 |
|---|---|---|
| **必需** | `pkgcore`、`dbkit`、`tenancy`、`config`、`observability` | 所有模块的底座，不提供关闭选项 |
| **底座型**（可关，但会连带关闭依赖方） | `jobs`、`storage`、`notification`、`authn`、`rbac`、`org` | 例如关闭 `jobs` 意味着 `storage`/`notification`/`billing`/`ai-gateway`/`integration`/`compliance` 全部不可用——CLI 必须在关闭前明确列出连带影响并要求确认 |
| **业务能力型**（自由关闭） | `billing`、`billing-gateway`、`metering`、`ai-gateway`、`sharing`、`integration`、`compliance`、`admin` | 关掉不影响其他模块，只是少一块能力 |

典型的"最小可用组合"是：必需五件 + `authn` + `rbac` + `org`——即一个有多租户、认证与组织管理但不收费的应用。

**功能开关**
- 每个模块向注册表声明自己提供的开关：key、默认值、说明、依赖的其他开关。开关清单与配置清单一样**从注册表自动生成文档**。
- 粒度示例：关闭自助注册（仅邀请制）、关闭某个社交登录渠道、关闭 AI 功能、关闭某支付渠道、关闭用量超额自动扣费。
- **启动时校验开关依赖图并 fail-fast**：例如启用了"用量超额计费"却禁用了 `metering`，直接拒绝启动并说明原因，而不是运行到一半才出现诡异行为。
- **禁用只跳过路由注册与后台任务，不跳过数据库迁移**。表结构始终保持最新，这样开关可以随时来回切换而不需要做数据迁移——这个取舍很关键，反过来做会让"临时关一下"变成一次运维事故。
- 被禁用功能的接口返回 `404` 而非 `403`（不暴露"存在但被关闭"的信息），但在 `/api/system/features` 里可查询当前启用状态，方便排查。
- 前端通过 `/api/config/public` 拿到启用列表，`useFeature('billing')` hook 控制菜单与路由的显隐；`layout-kit` 的 `NavItem` 支持 `requiredFeature` 字段，与已有的 `requiredPermission` 并列。

**组合爆炸的应对**：N 个开关有 2^N 种组合，不可能全测。策略是——依赖图校验保证非法组合根本无法启动；CI 只测三种典型组合：**最小可用组合**（上表所列）、**全开**、**典型交付组合**（最小可用组合 + billing + 一个支付渠道 + storage + notification）。

## 限流：独立模块，单一维度，不做业务语义

**独立于 `pkgcore` 之外的共享原语**：`authn` 的登录/注册/密码重置防暴力、`integration` 现有的 API Key 三层限流、`sharing` 的公开分享链接防滥用、`ai-gateway` 独立于信用点成本限额之外的按租户请求频率限流——四个业务模块各自需要限流能力，与其各自实现一遍，不如抽成一个共享的 `go/ratelimit` 模块。但**不并入 `pkgcore`**：`pkgcore` 是所有模块共同的依赖底座，只收纳 `KVStore`/`EventBus`/`tenantctx` 这类每个模块都需要的通用原语；限流是"部分消费者需要"的能力而非通用原语，塞进 `pkgcore` 只会让底座变重。`go/ratelimit` 因此是与 `dbkit`/`observability`/`tenancy` 同一层级、只依赖 `pkgcore` 的独立模块——依赖图见 [01 整体架构](01-architecture.md)——而且是一个**纯库**：不实现 `pkgcore.Module`，不注册路由、配置 schema 或功能开关。

| 消费方 | 限流场景 |
|---|---|
| `authn` | 登录/注册/密码重置防暴力，均按 IP，另视端点按账号或目标邮箱/手机号（细节见 [05 身份与访问](05-identity-and-access.md) 的"登录日志"一节） |
| `integration` | 现有 API Key 三层限流：全局 + 租户 + Key |
| `sharing` | 公开分享链接防滥用 |
| `ai-gateway` | 按租户的请求频率限流，独立于按信用点计的成本限额 |

**算法：滑动窗口计数器近似，而不是滑动窗口日志。** 这个选择由 `KVStore` 的实际契约决定，不是随意挑的：`KVStore` 故意不提供服务端脚本、pipeline，也不提供超出"不透明字节值"之外的数据类型——这样内存 map、Redis 以及将来任何一套 `KVStore` 实现才能同等地实现它。滑动窗口日志需要为每次请求单独记一条时间戳、再按窗口范围查询，这需要有序集合一类的结构，`KVStore` 的契约给不了；滑动窗口计数器只需要"计数 + 一个界定窗口的 TTL"，`KVStore` 现有的两个原语正好够用：
- `Set` 创建一个窗口的计数器，用 `ttl` 参数天然界定窗口边界；
- `IncrByFloat` 在窗口内递增计数——它"递增但不延长已有 key 的 TTL"这条语义（见 `go/pkgcore/kv.go`）正是窗口型计数器要的：一个窗口内的突发请求不会把窗口边界向后推。

当前窗口的计数按已经过去的时间比例，与前一个相邻窗口的计数加权，得到近似的滑动效果——这是"滑动窗口计数器"与"滑动窗口日志"的本质区别，也是它不需要存储单条请求记录的原因。

**不需要修改 `pkgcore.KVStore` 接口**：`Set` + `IncrByFloat` 已经够用；`CompareAndSwap` 也已经在接口里，留给未来如果有消费者需要 token-bucket 语义时使用。`go/ratelimit` 完全建立在现有契约之上。

接口形状如下（示意，字段以实现时为准）：
```go
type Limiter interface {
    Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}

type Limit struct {
    Rate int           // 窗口内允许的请求数
    Per  time.Duration // 窗口长度
}

type Decision struct {
    Allowed    bool
    Remaining  int           // 当前窗口剩余可用次数
    ResetAfter time.Duration // 距窗口重置的时间
}
```

**刻意只做单一维度，不内置多层复合**：`go/ratelimit` 不像 `integration` 现有设计那样把"全局+租户+Key"三层复合限流内置进原语。多维度限流由调用方对不同 `key` 分别调用 `Allow` 自行组合——每层/每个维度一次调用，任意一次拒绝就整体拒绝。这样这个原语的形状不会被最先落地的某个消费者的具体结构固化下来：`authn` 的"账号+IP"双维度、未来任何新维度，用的都是同一套组合方式。

**刻意与 HTTP 无关**：`Decision` 是纯数据，不涉及任何协议细节。把一次被拒绝的 `Decision` 翻译成带 `Retry-After` 和配额响应头的 HTTP 429，是调用方 handler 层自己的职责，不是 `go/ratelimit` 要做的事——这样它在 HTTP 之外的场景（例如限制一个后台任务的派发速率）也能直接用。

**刻意不理解"渐进式/升级式"语义**：`authn` 现有的防暴力设计——失败次数越多延迟越长、直至锁定，分账号和 IP 两个维度——是建立在 `go/ratelimit` 报告的计数之上的业务逻辑，不是 `go/ratelimit` 本身理解的一种模式或概念。`go/ratelimit` 的职责到"这个 key 在配置的窗口、配置的速率下是否超限、还剩多少余量"为止；拿到重复拒绝之后是锁定、是升级延迟、还是直接拒绝，完全由调用方决定。

**没有自己的配置 schema**：阈值（`Limit` 的取值）由每个调用点在代码里直接传入，不是本文"配置管理"一节所说的、需要注册 schema、经运营后台调整的动态配置项。

**测试**：`go/ratelimit` 只依赖 `KVStore` 接口，单元测试直接用 `pkgcore.NewMemoryKVStore()`，不需要 testcontainers；也不设专属的集成测试层——真正的分布式 `KVStore` 后端（Redis 等）自身是否正确，是该后端自己的测试责任，不是 `go/ratelimit` 需要重新验证的东西。
