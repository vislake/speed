# 身份与访问

> 涵盖：租户内的角色权限（RBAC）、面向企业客户的 SSO、面向个人用户的社交登录、多因素认证、组织树、以及登录日志与会话撤销。
>
> 组织结构（多层级组织树）也在本文档中，因为它与权限判定紧密耦合。

## 认证与授权：Casbin 自建 RBAC + OIDC RP
不自建 IdP（范围蔓延+安全责任重），不强依赖 Auth0/Ory（给每个交付项目强加付费第三方依赖，且用户身份脱离自己库表，破坏 tenant 关联与审计一致性）。

> **实现落地更正**（rbac 轮次实现本节 RBAC 时确认）：**`rbac` 模块最终没有采用 Casbin**，而是在三张受 `dbkit` 托管的 `TenantScoped` 表（`rbac_roles` / `rbac_role_permissions` / `rbac_role_bindings`）上自建判定引擎。**本节约定的语义全部保留**——domain 即 tenant、`resource:action` 命名、`"system"` 伪租户承载平台运营授权、物化路径前缀匹配的子树范围、必备的策略缓存——变的只是存储与判定的实现载体。
>
> 换掉 Casbin 的直接原因是它与本仓库三条 CI 强制纪律无法调和：
>
> 1. `casbin_rule` 表**没有 `tenant_id` 列**，租户藏在策略的 `v0`（domain）值里。这意味着全产品最安全敏感的一张表，会同时退出[04 数据与多租户](04-data-and-tenancy.md)的**三层隔离**：GORM 插件注入不到过滤条件、用不了 `Repository[T]`、Postgres RLS 也无从下手（RLS 策略以 `tenant_id` 列为准）。`tenancytest.AssertIsolated` 对它根本跑不起来，隔离保证退化成"调用方传对了 domain 字符串"。
> 2. `gorm-adapter` 自己持有 `*gorm.DB` 并自行发查询，正是 §3.2 与 `tools/semgrep_rules/raw-gorm-bypass.yml` 要禁止的形态。
> 3. `gorm-adapter` 默认 `AutoMigrate`（有 `TurnOffAutoMigrate` 可关，这条尚可绕过，前两条不能）。
>
> 而 Casbin 真正的价值（可插拔 model.conf、ABAC、RESTful matcher）在这里全部用不上：判定链就是 `subject → bindings → role → permissions` 加一次物化路径前缀判断。在"以库的形式分发的模块化单体"这个前提下，为此往每个交付项目的 `go.sum` 里塞两个第三方依赖，不成立。
>
> 落地实现同时明确了本节几处留白，均记录在 `go/rbac/AGENTS.md` 的 deferral 清单里：
>
> - **不做角色到角色的继承**（Casbin 的 `g` 分组）。本节"权限沿树继承"说的是**组织树**继承——绑定在 `/g1` 的授权覆盖 `/g1/r2/s7`——这一条已实现；角色层级本节从未要求，不臆测实现。
> - **节点物化路径在判定时解析，绝不快照到绑定行上**。绑定表只存 `node_id`。[16 验证](16-verification.md)要求"成员在树中移动后权限即时随之变化"，一旦把路径反规范化到绑定行，恰恰在这个场景上失效。
> - **`rbac` 不 import `authn`，也不 import `org`**，这是模块的定义性属性。它对两个邻居的全部认知就是两个在 `rbac` 内声明、由宿主实现的接口：`SubtreeResolver`（node id → 物化路径，`org` 建成后由它实现）和中间件的 `WithSubjectResolver`（认证方组装 `Subject{TenantID, UserID}` 传入）。任一 node 解析不出来时**拒绝**该条绑定，绝不回退成租户级授权。
> - **权限通配符（`billing:*`）不做**。通配符语法是一个安全面，需要专门的设计决策，不是实现时随手猜一个。当前判定是 `resource:action` 精确匹配。
> - **`Repository[T].WithinSubtree(nodeID)`（本节末尾承诺的）延后到 `org` 落地后再进 `dbkit`**。真实查询形态在组织树存在之前是未知的，`dbkit` 明确反对无消费者的推测性泛型扩展。`rbac` 先提供 `DataScope` 与 `PathWithinSubtree`，由消费方自行套用。
> - **`rbac` 不暴露任何 HTTP 端点**（`OpenAPISpec()` 返回 nil）：角色管理属于 M3 管理后台，`/me` 的扁平权限列表属于 `authn`（由它调 `ListPermissions`）。`rbac` 对 HTTP 层的贡献只有[01 架构](01-architecture.md)固定中间件链里的那道权限闸门。
> - [17 风险](17-risks.md)与[16 验证](16-verification.md)要求的"千级节点前缀匹配压测"仍然有效，但**归属 `org` 轮次**：`rbac` 造不出千级节点的组织树。前缀匹配的**正确性**（含 `/g1/r2` 不得匹配 `/g1/r20` 这个经典陷阱）已在 `rbac` 单元测试中锁死。

- **RBAC**：Casbin `RBAC with domains` 模型，domain = tenant_id，天然支持"同一用户在租户 A 是 admin、在租户 B 是普通成员"。策略存储用 `casbin/gorm-adapter`。权限命名统一 `resource:action`（`billing:manage`、`org:invite_member`）。平台运营权限复用同一引擎，用 `domain="system"` 伪租户表示，不为运营后台另造一套鉴权。
- **SSO**：`coreos/go-oidc` + `oauth2` 实现标准 OIDC Relying Party，覆盖 Okta/Azure AD/Google Workspace。每租户一条 `TenantSSOConfig`（issuer/client_id/secret/allowed_domains）存库，客户可自助配置 SSO 无需改代码；回调后 claims → Principal 映射，支持 JIT 建用户。
- **SAML** 作为可选子包延后，OIDC 已覆盖多数企业场景，不把 SAML 依赖强加给所有消费方。
- **面向个人用户的社交登录（Google/GitHub/微信/钉钉/飞书等）是另一套机制，见本文档「第三方账号登录与注册」**—— 配置层级和协议差异都很大，不要与企业 SSO 复用同一套配置。
**核心类型**（被架构纪律直接引用，必须先定死）：

```go
// authn 产出：认证结果，刻意不含角色信息，保持 authn / rbac 解耦
type Principal struct {
    UserID    string
    TenantID  pkgcore.TenantID  // 当前租户，来自令牌 claims
    SessionID string
    Email     string
    AMR       []string          // 认证方式：password / oidc / social:google / mfa:totp
}

// rbac 输入：授权只认这两个字段，由认证方拼装
type Subject struct {
    TenantID pkgcore.TenantID
    UserID   string
}

type Authorizer interface {
    Can(ctx context.Context, sub Subject, action, resource string) (bool, error)
    ListPermissions(ctx context.Context, sub Subject) ([]string, error)  // 供 /me 下发前端
    AssignRole(ctx context.Context, sub Subject, role string, scope Scope) error
}
```

`AMR`（认证方法引用）记录本次会话是怎么登录的，step-up 重新验证与"要求 MFA 的操作"都依赖它判断当前会话强度是否足够。

- **密码存储用 argon2id**（不用 bcrypt：argon2id 是当前 OWASP 首选，抗 GPU 破解更强），参数随硬件可配；密码策略走动态配置（最小长度、复杂度、常见弱口令字典校验），默认值遵循 NIST 建议——长度优先于强制符号组合。
- **前后端契约**：`rbac` 必须提供 `ListPermissions(ctx, subject) []string`，在 `/me` 接口返回**扁平化有效权限列表**。前端不重新实现策略引擎，`usePermission` 只做集合查找。切换租户时重新拉取 `/me` 获得新权限集。

## 第三方账号登录与注册

**先区分两个容易混淆的能力**——它们的配置层级、目标用户、接入方式都不同：

| | **社交登录**（本节） | **企业 SSO**（见上一节） |
|---|---|---|
| 面向 | 个人用户自助注册登录 | 企业客户的员工统一登录 |
| 渠道 | Google、GitHub、微信、企业微信、钉钉、飞书、QQ | 客户自有 IdP（Okta / Azure AD / Keycloak） |
| 配置层级 | **平台级**（动态配置里开关 + 填凭证） | **租户级**（每个客户配自己的 issuer） |
| 协议 | OAuth2 及其各家变体 | 标准 OIDC / SAML |

**Provider 抽象**（不能直接套标准 OIDC 库——国内渠道普遍是 OAuth2 变体，微信用 `appid`/`secret` 而非 `client_id`/`client_secret`，返回结构也各不相同）：

```go
type SocialProvider interface {
    Name() string                    // "google" / "github" / "wechat" / "dingtalk" / "feishu"
    AuthorizeURL(state, redirectURI string) string
    Exchange(ctx context.Context, code string) (*ExternalIdentity, error)
}

type ExternalIdentity struct {
    Provider      string
    ExternalID    string  // 该 provider 下的稳定唯一标识
    Email         string
    EmailVerified bool    // 决定能否自动关联已有账号，见下方安全规则
    Name, Avatar  string
    Raw           json.RawMessage
}
```

**v1.0 内置渠道**：Google、GitHub（标准 OAuth2，覆盖海外与开发者场景）+ 微信开放平台、企业微信、钉钉、飞书（覆盖国内 to C 与 to B 场景）。QQ、微博、支付宝作为二期按需补充。**手机号 + 短信验证码**在国内几乎是刚需，作为独立的 `Authenticator` 实现（不是 social provider）一并纳入，短信网关同样做接口抽象（单进程部署模式下打印到控制台）。

**账号关联模型**：`user_identities` 表（`user_id`, `provider`, `external_id`, `unique(provider, external_id)`），一个用户可绑定多个外部身份，也可同时保留密码登录。

**必须遵守的安全规则（社交登录最经典的漏洞就在这里）**：
1. **绝不能仅凭 email 相同就自动合并账号**。攻击者可以在第三方平台注册一个与受害者同邮箱的账号来劫持。只有当 provider 明确返回 `EmailVerified=true` **且**该 provider 在平台信任列表内时，才允许自动关联到同邮箱的已有用户；否则一律要求"先用原方式登录，再在设置页绑定"。
2. **state 参数强制校验**（防 CSRF），并绑定到会话；OAuth 回调地址走白名单。
3. **微信的 openid 是 per-app 的**，同一个人在不同应用下 openid 不同。若未来有多应用，必须用 unionid 作为 `ExternalID`，否则同一用户会在各应用里变成不同账号。这个坑要在接入文档里显式写明。
4. **解绑保护**：解绑前校验用户至少还保留一种可用登录方式，否则拒绝，避免把自己锁在门外。

**与多租户的衔接**：社交登录成功且是新用户时，进入引导流程——创建自己的组织，或接受已有的邀请（邀请链接里携带 tenant 上下文）。社交登录本身不决定租户归属，租户归属由组织创建/邀请流程决定。

**前端**：`@speed/auth-ui` 的 `SocialLoginButtons` 组件根据 `/api/config/public` 下发的**已启用渠道列表**动态渲染，业务项目不需要改代码就能增减渠道；各渠道图标与品牌规范（Google 对按钮样式有强制要求）内置。

## 组织模型：多层级组织树

```go
type OrgNode struct {
    ID       string
    TenantID pkgcore.TenantID
    ParentID *string
    Path     string   // 物化路径 "/group1/region2/store7"，权限前缀匹配依据
    Depth    int
    Name     string
    Kind     string   // 业务自定义："group" / "region" / "store"
}

// 用户与租户的多对多桥接，用户表本身不含 tenant_id（见 04 数据分域）
type Membership struct {
    UserID   string
    TenantID pkgcore.TenantID
    NodeID   *string  // 绑定到组织树的某个节点；nil = 租户根
    Roles    []string
    Status   string   // active / invited / suspended
}
```

- 从"Organization → Workspace"两级扩展为**任意层级的组织树**（如 集团 → 区域 → 门店），用 `parent_id` + 物化路径（materialized path）实现，兼顾查询效率与实现复杂度。
- **权限沿树继承**：上级角色可下探管辖下级。以下按当初的 Casbin 方案描述（实际实现见本文档开头的「实现落地更正」：语义一致，载体换成了自建引擎）——domain 仍是租户根，**被管辖节点的物化路径作为策略的一个维度**参与判定：

  ```
  p, role:store_manager, tenant_42, /group1/region2/store7/*, order:read
  ```

  用自定义 matcher 做路径前缀匹配（`pathMatch(r.node, p.node)`），角色绑定在树节点上而非整个租户上。这个 matcher 与物化路径的组合是本模块的实现难点，需要在 M1 早期就打样并压测——前缀匹配的策略数量会随节点数增长，必须配合策略缓存，不能每次请求都全量加载。
- 数据可见范围按节点子树计算，`Repository[T]` 提供 `WithinSubtree(nodeID)` 作用域，避免每个业务自己拼递归查询。

## 多因素认证（MFA）

- **TOTP 为默认第二因素**（RFC 6238，兼容 Google Authenticator / 1Password 等所有主流应用），不依赖短信通道，也没有跨境短信送达问题。
- 短信验证码作为**可选**第二因素：国内用户接受度高，但 SIM 劫持风险与通道成本都高于 TOTP，因此不作默认。
- **恢复码**：开启 MFA 时一次性生成 10 个一次性恢复码，仅展示一次、哈希存储。没有恢复码机制的 MFA 会直接制造大量"永久锁死账号"的客服工单。
- **强制策略分层**：平台可要求特定角色（如平台运营、租户 Owner）强制启用；租户管理员可对本租户成员强制启用。策略走动态配置的租户级覆盖。
- **重新验证（step-up）**：改密码、改 MFA 设置、删除组织、导出数据这类高风险操作，要求重新输入第二因素，而不是仅凭已有会话放行。
- WebAuthn / Passkey 不在 v1.0 范围，但 `SecondFactor` 接口预留实现位。

## 会话与租户的关系

一个用户可以属于多个租户，因此必须明确：**会话属于用户，不属于租户。**

- 一次登录 = 一个 session，用户在此 session 内可以自由切换租户，不需要重新登录。
- **access token 中携带"当前租户"**，切换租户签发新的 access token（复用同一 session、同一 refresh token）。这样每个请求的租户上下文来自令牌本身，服务端无需查库。
- 切换租户时服务端**必须重新校验该用户在目标租户的 membership 与权限**，不能信任客户端传来的租户 ID——这是水平越权最容易被忽略的入口。
- 设备下线针对的是 session（连带所有租户上下文），而不是某个租户的访问权。
- 从某个租户移除成员时，需**主动使该用户在该租户下的令牌失效**（在撤销列表中按 user × tenant 打标），否则被移出组织的人在 access token 过期前仍能访问。

## 登录日志与会话管理

### 登录日志
每一次登录**尝试**（成功与失败都记）落库：时间、IP、**IP 归属地**（本地 GeoIP 库解析，不调外部 API 泄漏用户信息；**注意 MaxMind GeoLite2 需注册账号且有使用条款限制，商业交付前须由 [20 质量与安全](20-quality-and-security.md) 的许可证扫描确认合规**，必要时替换为纯真库等国内数据源）、User-Agent 解析出的设备/浏览器/操作系统、登录方式（密码 / 社交渠道 / SSO / 手机号）、结果与失败原因。

用途不只是展示：
- 连续失败触发**渐进式延迟与锁定**（按账号 + 按 IP 双维度，防撞库）：延迟曲线怎么走、第几次失败触发锁定，是 `authn` 自己的业务逻辑；底层的失败计数建立在共享的 `go/ratelimit` 模块之上（设计见 [11 横切能力](11-cross-cutting.md) 的"限流"一节）——`go/ratelimit` 只按窗口报告计数是否超限，不理解"渐进式"这类语义。
- **注册与密码重置请求端点同样接入 `go/ratelimit`**，针对的是另外两类常见滥用：批量注册占号、以及把密码重置请求当撞库探测已注册地址的手段。注册端点按 IP 维度限流；密码重置请求按 IP 与目标邮箱/手机号两个维度限流。三者用的是同一套共享原语，区别只在 key 的取法与阈值。
- **异常登录检测**：新设备、新地区、不可能的位移（10 分钟前在上海、现在在纽约）→ 触发安全通知（见 [07 平台服务](07-platform-services.md) 中不可关闭的通知类型）
- 用户可在账号安全页查看自己的完整登录历史

### 会话管理与撤销：JWT 无状态与"立即下线"的冲突

**这是必须正面回答的设计冲突**——纯 JWT 是无状态的，签发后在过期前无法收回，而"取消指定客户端登录权限"要求的恰恰是撤销能力。方案是**有状态会话 + 短时效 access token**：

- 每次登录创建一条 `sessions` 记录：session_id、用户、设备指纹、IP、归属地、创建时间、最后活跃时间、状态。
- **access token 短时效（默认 15 分钟）且携带 session_id**；**refresh token 长时效、绑定 session_id、存哈希**。
- 撤销会话 = 标记 session 为 revoked → refresh 立即失败 → 现有 access token 最长 15 分钟后自然失效。

**"最长 15 分钟"对多数场景可接受，但对安全敏感项目不够**，因此提供一个开关：

| 模式 | 机制 | 代价 | 适用 |
|---|---|---|---|
| **自然过期**（默认） | 仅撤销 refresh，access token 等待过期 | 零额外开销 | 多数 SaaS |
| **立即失效**（可开启） | 撤销列表放 `KVStore`，中间件校验 session_id | 每请求一次 KV 查询（可本地缓存 + 短 TTL 削减） | 医疗、金融等敏感场景 |

撤销列表只存"已撤销且尚未自然过期"的 session_id，数据量极小，TTL 到期自动清理——不会无限膨胀。

**refresh token 轮换与重放检测**（OAuth 2.0 安全最佳实践，必须实现）：每次刷新都作废旧 token 并签发新的；**若检测到已作废的 refresh token 被再次使用，说明令牌已泄漏，立即撤销整个会话族并通知用户**。这是能自动发现令牌被盗的少数机制之一。

**用户可见的设备管理**（`@speed/auth-ui` 提供页面）：
- 已登录设备列表：设备类型、浏览器、IP 与归属地、最后活跃时间，**当前设备明确标记**
- 单个设备下线 / **一键下线其他所有设备**（保留当前）
- 修改密码时询问"是否同时下线所有其他设备"，默认勾选
- 所有下线操作都进审计日志，并向被下线设备的所有者发安全通知

**边界**：API Key（见 [07 平台服务](07-platform-services.md)）与用户会话是两套独立体系，吊销互不影响，但都纳入审计。

---
