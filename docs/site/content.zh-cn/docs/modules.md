---
title: 模块索引
weight: 3
---

# 模块索引

speed 是一系列独立发布的 Go module 与 npm 包，不是一个应用程序。业务方
项目按需接入其中的一部分。下表每一行都链接到该模块自己在仓库里的
`AGENTS.md` 或 `README.md`——本站不保留模块文档的副本，所以链接始终
指向最新的那一份。

> [!NOTE]
> 这个页面列出的是"存在什么"，不是"什么已经被同等程度证明可靠"。凡事
> 自己核实仍然是规则：仓库根 `CLAUDE.md` 的 Repository Status 一节（见
> [面向 AI Agent](../ai-agents/)）才是"哪些模块今天真的能在 CI 里通过
> `go build` / `go vet` / `golangci-lint` / `go test -race`"的唯一权威
> 来源——按 `fast-check.yml` 自己的 `go-modules` 矩阵，全部 21 个都在内，
> `go/admin` 也不例外。

## Go module（`go/`，共 21 个）

`go.work` 里每一条 `use` 对应下表一行。依赖关系严格自底向上——粗略的
顺序见[面向 AI Agent](../ai-agents/)。

| 模块 | 是什么 | 文档 |
|---|---|---|
| `pkgcore` | 依赖的最底层：`Module`/`Registry`/`Kernel` 装配契约、租户上下文原语、`apperr`、seam registry 机制（`EventBus`/`KVStore`/`Mailer`/`ObjectStore`）、部署模式 preset，以及合并后的 i18n 消息目录。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/pkgcore/AGENTS.md) |
| `dbkit` | 双方言（SQLite/PostgreSQL）的 `*gorm.DB` 封装：强制使用的泛型 `Repository[T]`、带版本的迁移、字段级加密、支持精确匹配查询的盲索引、软删除/硬删除，以及审计捕获写插件。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/dbkit/AGENTS.md) |
| `tenancy` | 租户解析中间件、经过审计的 `WithSystemContext` 逃生通道，以及所有其他模块的 repository 都必须跑的 `AssertIsolated` / `AssertNotTenantScoped` 测试套件。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/tenancy/AGENTS.md) |
| `observability` | 两种部署模式下的 OpenTelemetry 初始化、默认开启 PII/密钥脱敏的上下文感知结构化日志器，以及带基数上限的请求指标 HTTP 中间件。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/observability/AGENTS.md) |
| `config` | 动态配置：每个模块在注册时声明自己的配置 schema；配置值按 system→tenant 分层作用域、带缓存，Sensitive 项在静态存储时加密。提供登录页所需的两个免认证端点。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/config/AGENTS.md) |
| `jobs` | 两种部署模式共用的 `Queue`/`Task`/`Job`/`Handler` 契约：`StandaloneQueue`（基于 SQLite 的进程内实现）与面向分布式模式的 Redis 实现，后者拆到独立子包，让只用 `StandaloneQueue` 的消费者不必为它付出依赖代价。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/jobs/AGENTS.md) |
| `ratelimit` | 一个基于 `KVStore` 的限流器，被 `authn`、`integration` 等需要限制某件事发生频率的模块共用。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/ratelimit/AGENTS.md) |
| `pki` | 签名密钥与 X.509 证书生命周期：`Signer` seam、内部 CA 签发、吊销、CRL 生成与 JWKS 导出。`authn` 的 `KeySource` 是它真实、API 已冻结的消费方；X.509/CA 那一层还没有真实消费方，这是一个有记录的已知例外。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/pki/AGENTS.md) |
| `authn` | 身份认证——回答"调用者是谁"，从不回答"可以做什么"。argon2id 密码、Ed25519 签名的访问令牌、一次性轮换的刷新令牌、TOTP 多因素认证，以及社交/企业单点登录（Google、GitHub、微信、钉钉、飞书、OIDC）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/authn/AGENTS.md) |
| `rbac` | 基于角色的访问控制，默认拒绝、精确匹配 `resource:action`。刻意从不 import `authn`——它看到的永远只是认证方组装好的 `Subject{TenantID, UserID}`。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/rbac/AGENTS.md) |
| `org` | 一个租户的组织树（邻接表加物化路径）、绑定在其上的成员关系，以及创建这些成员关系的邀请流程。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/org/AGENTS.md) |
| `storage` | 租户对象存储：基于 `ObjectStore` seam 的上传/完成/派生生命周期、结构化元数据剥离、缩略图派生，以及可崩溃收敛的删除加过期清扫。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/storage/AGENTS.md) |
| `notification` | 平台的消息通知层：业务模块声明进去的实时通知类型注册表、按用户的偏好设置、带 SSE 推送的站内信箱，以及经过同意验证的外部渠道投递（邮件/短信）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/notification/AGENTS.md) |
| `billing` | Plan/Feature/Grant/Entitlements、渠道无关的 Subscription/Invoice 生命周期、并发安全的信用点账本，以及真实的支付网关接入（Stripe、支付宝、微信支付）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md) |
| `metering` | 带两档可靠性的用量记录——失败即放行的分析级，以及基于 outbox、不会静默丢事件的计费级——两者最终汇聚到实时聚合计数器上。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/metering/AGENTS.md) |
| `sharing` | 公开分享链接：256 位 token、强制的默认过期时间、下一次访问检查即刻生效的吊销、完整的访问日志，以及无论拒绝原因是什么、对外表现完全一致的拒绝应答。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md) |
| `integration` | 租户对外的 API 接入层：API key 签发与分层限流，以及带 HMAC 签名和二次 SSRF/DNS 重绑定防护的出站 webhook。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/integration/AGENTS.md) |
| `ai-gateway` | 与厂商无关的 LLM 对话与图像生成网关（`ChatProvider`/`ImageProvider` seam、零依赖的 OpenAI 兼容默认实现、按作用域分层加密存储的 BYOK 凭据）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md) |
| `compliance` | 按租户的保留期清扫、带已证明跨租户不擦除他人数据性质的即时"被遗忘权"擦除、经由 `sharing` 的导出投递，以及只读的审计查询。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md) |
| `admin` | 运维控制台后端：租户台账、带双重身份审计轨迹的完整模拟登录流程、跨租户用户搜索，以及只读的审计查询外壳。第二轮工作——角色管理、用量看板、按租户强制执行——尚未落地。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md) |
| `saasctl` | 面向业务方的 CLI——它的四个命令见[快速开始](../quickstart/)。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/go/saasctl/AGENTS.md) |

## Web 包（`web/packages/`，共 11 个）加消费方壳应用

每一行是一个 `@speed/` 作用域下的 npm 包，全部运行在同一个根目录为
`web/` 的 pnpm workspace 里。

| 包 | 是什么 | 文档 |
|---|---|---|
| `@speed/tokens` | 设计 token 树，作为零依赖的纯数据——间距、断点、排版比例、z-index——其中与 MUI 对齐的行由测试钉住。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tokens/AGENTS.md) |
| `@speed/i18n` | react-i18next 封装，是 `pkgcore/i18n` 在前端的对应实现：语言协商，以及会在 `zh-CN` 与 `en-US` 语料 key 集合不一致时直接拒绝的命名空间注册机制。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/i18n/AGENTS.md) |
| `@speed/ui-kit` | 基于 token 树的 MUI v9 主题工厂（`createAppTheme`），加上七个受控组件（`PageHeader`、`EmptyState`、`ConfirmDialog`、`FormField`、`FormLayout`、`DataTable`、`FileUploader`）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/ui-kit/AGENTS.md) |
| `@speed/api-client` | 前端唯一手写 HTTP 调用的地方：可注入的 fetch、仅存于内存的访问令牌存储、单飞的 401 静默刷新、保守的瞬时错误重试，以及统一归一化的 `ApiError` 形态。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md) |
| `@speed/api-sdk` | 合并后 OpenAPI 文档生成出的类型化调用面（orval 产出）——从不手改，每一次调用都经过 `api-client` 唯一的绑定 seam。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md) |
| `@speed/layout-kit` | 共享的应用外壳：`AppShell`（应用栏、响应式导航抽屉）与只依赖宿主注入状态的 `RouteGuard`——不依赖任何认证或路由包。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/layout-kit/AGENTS.md) |
| `@speed/auth-core` | 无界面的会话层：`createAuthSession` 把生成的 authn 操作变成一台可观察、仅存于内存的状态机（登录、刷新、切换租户、二次验证）。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-core/AGENTS.md) |
| `@speed/auth-ui` | 基于 auth-core 会话的登录组件族：密码/短信/社交渠道、注册，以及登出/会话结束视图。每个组件都是受控组件。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-ui/AGENTS.md) |
| `@speed/tenancy-ui` | 一个受控的 `TenantSwitcher` 组件——会话、租户列表与当前租户全部由宿主通过 props 提供。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/tenancy-ui/AGENTS.md) |
| `@speed/product-shell` | 面向租户的组装外壳：`ProductShell` 把 `AppShell`、auth-ui 登录组件族与 auth-core hooks 组合成一台三分支视图状态机。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/product-shell/AGENTS.md) |
| `@speed/account-ui` | 登录后的账户管理组件族：会话列表、登录历史、社交身份绑定，以及经由生成的 react-query hooks 驱动的多因素认证注册/二次验证。 | [AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/account-ui/AGENTS.md) |
| `examples/reference-app/web`（消费方壳应用） | 不是一个 `@speed/*` 包——reference app 自己的 web host，作为同一个 pnpm workspace 的外部成员，把上面每一个包组合成一个真实的浏览器端应用。 | [README.md](https://github.com/vislake/speed/blob/main/examples/reference-app/web/README.md) |

> [!NOTE]
> 完整的后端加前端全貌见 [reference app 自己的 README](https://github.com/vislake/speed/blob/main/examples/reference-app/README.md)——
> 它是每一个模块的**强制性首个消费方**（一个 reference app 从未真正使用
> 过的模块 API，不算完成）。
