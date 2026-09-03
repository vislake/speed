# 前端架构

> npm 包分层、生成式 API 调用（禁止手写）、ui-kit 组件范围、主题与多品牌定制、状态管理选型。

## 包分层

```mermaid
graph BT
    tokens["@speed/tokens<br/>设计token,纯数据"]
    i18n["@speed/i18n<br/>react-i18next封装+MUI locale联动"]
    uikit["@speed/ui-kit<br/>MUI二次封装+主题工厂"]
    api["@speed/api-client<br/>HTTP基建/认证/刷新/错误归一化"]
    sdk["@speed/api-sdk<br/>由OpenAPI生成的类型与Query hooks"]
    authcore["@speed/auth-core<br/>认证/租户/权限 headless"]
    billcore["@speed/billing-core<br/>计费 headless"]
    authui["@speed/auth-ui"]
    accountui["@speed/account-ui<br/>账号管理UI"]
    tenantui["@speed/tenancy-ui"]
    billui["@speed/billing-ui"]
    notifcore["@speed/notification-core<br/>通知/未读/SSE headless"]
    notifui["@speed/notification-ui<br/>通知中心/偏好矩阵"]
    layout["@speed/layout-kit<br/>AppShell/RouteGuard"]
    pshell["@speed/product-shell<br/>面向租户客户"]
    ashell["@speed/admin-shell<br/>面向内部运营"]

    uikit --> tokens
    uikit --> i18n
    sdk --> api
    authcore --> sdk
    billcore --> sdk
    billcore --> authcore
    authui --> uikit
    authui --> authcore
    accountui --> sdk
    accountui --> uikit
    accountui --> authcore
    tenantui --> authcore
    billui --> uikit
    billui --> billcore
    notifui --> uikit
    notifui --> notifcore
    notifcore --> sdk
    layout --> uikit
    pshell --> authcore
    pshell --> authui
    pshell --> layout
    ashell --> layout
```

> **图校准注记（product-shell round，边按已落地依赖修正）**：`layout --> auth-core` 与 `tenantui --> ui-kit` 两条自设计图起从未兑现的边已删除——layout-kit 落地时是 auth-agnostic 的（依赖仅 `i18n`/`ui-kit`，`RouteGuard` 只吃 host 注入的 `status` 值，见该包 AGENTS.md）；tenancy-ui 的 `src/` 从不 import `ui-kit`（主题 provider 只出现在 `test-utils/` 与 devDependencies——测试树要渲染在真实 host 组合下），其依赖仅 `auth-core`（type-only）与 `i18n`。新增 `pshell --> auth-core` 与 `pshell --> auth-ui`：product-shell 直接依赖两者（`useAuthState` 读会话快照、`auth-ui` 的默认 `SessionEndedScreen`）。正文下段"两者共享 `layout-kit`/`ui-kit`/`auth-core`"按**直接依赖**读为：product-shell 依赖 `layout-kit`/`auth-core`/`auth-ui`（`ui-kit` 与 `i18n` 经这三者间接进入），admin-shell 落地时同理只取所需。billing/notification 与 admin-shell 等计划中节点及全部其他边保持不变。

**product-shell 与 admin-shell 拆成两个包、两个独立部署应用**：客户端权限是"租户内角色"，运营端是"内部员工角色"且能跨租户看数据，混在一起会造成权限逻辑纠缠和跨租户数据泄漏的审计风险。两者共享 `layout-kit`/`ui-kit`/`auth-core`，shell 本身只是薄的组装配置层，重复代码很少。

## API 调用：一律使用生成代码

**前端禁止手写任何后端调用**——包括 `fetch`、`axios`、以及自己封装的 request 函数。所有接口调用只能来自 `@speed/api-sdk`（由 OpenAPI 规范经 orval 生成的 TanStack Query hooks）。完整机制见 [21 API 契约](21-api-contract.md)。

这条规则由 CI 的 ESLint 规则强制，不是靠 review。理由很直接：手写调用是前后端接口漂移的唯一入口，堵住它就从机制上消除了这类问题。

`@speed/api-client` 与 `@speed/api-sdk` 的分工：前者是手写的运行时基建（fetch 实例、认证头注入、401 静默刷新、错误归一化、重试），后者是纯生成物、禁止手改。分开是因为生成物每次发布都会被整体覆盖。

## ui-kit 组件清单

M0 的"核心组件"指下面第一组。组件全部受控、props 驱动、不含任何业务与租户语义，保证在任何项目、任何认证方案下可复用。

**第一组（M0 必须）**

| 组件 | 说明 |
|---|---|
| `AppThemeProvider` / `createAppTheme` | 主题工厂与三层 token 合并 |
| `DataTable` | 分页、排序、筛选、空态、加载态、行选择 |
| `FormField` / `FormLayout` | 基于 react-hook-form 的表单适配层，统一校验错误展示 |
| `EmptyState` | 空数据、无权限、出错三种语义 |
| `ConfirmDialog` | 危险操作二次确认 |
| `PageHeader` | 标题、面包屑、操作区 |

**第二组（随对应能力交付）**

`FileUploader`（M2，配合 storage）、`StatCard` / `Sparkline`（M2，用量展示）、`StatusBadge`、`SearchInput`、`ToastProvider`、`LoadingOverlay`、`JobProgress`（M1，配合 jobs 的进度展示）。

> **已落地**（file-uploader round）：本组首项 `FileUploader` 已提前于 M2 计划窗口交付（排期注见 [15 里程碑](15-roadmap.md)）：受控队列组件——pick 队列与每行传输状态（上传中/成功/失败，重试/取消/移除）作为 interaction-local 例外存于组件内（与 `ConfirmDialog` 的 armed 状态同类），每次队列变化经 `onQueueChange` 上报；**上传本身由 host 注入的 `execute(file, { signal, onProgress })` 逐文件执行，组件零 HTTP**——host 的传输就是组件的网络边界，大小/类型/数量预校验与并发上限都是 executor 的职责，该例外与「组件全部受控」一条的关系见 ui-kit AGENTS.md。host 的 executor 正是 storage 前端操作就位后要接的位置：api-sdk 的 storage 调用仍随 consumer-shell round 从 `go/storage/api/openapi.yaml` 生成，[21 API 契约](21-api-contract.md) 的该轮注记与 ui-kit AGENTS.md 的 deferral 条目同记该延期。

**表单方案**：react-hook-form + zod 校验。zod schema 优先从 OpenAPI 生成的类型推导，避免前后端校验规则各写一套。

## 跨领域 hooks 的归属

避免"这个 hook 该放哪个包"反复扯皮，明确归属规则：**hook 跟随它所属的领域包，而不是集中放在 api-client。**

| Hook | 归属 | 说明 |
|---|---|---|
| `useJob(jobId)` | `@speed/api-sdk` + `ui-kit` 的 `JobProgress` | 查询由 sdk 生成，进度 UI 在 ui-kit |
| `usePermission` / `useCurrentTenant` | `@speed/auth-core` | 权限判定是纯客户端集合查找 |
| `useFeature(api, key)` / `usePublicConfig(api)` | `@speed/api-client` | 公开配置在应用启动时拉取，早于任何领域包初始化 |
| `useUnreadCount` / `useNotificationStream` | `@speed/notification-core` | 含 SSE 长连接管理，是 OpenAPI 覆盖不到的部分 |

> **已落地**（config-web round）：上表 `useFeature` / `usePublicConfig` 归属 `@speed/api-client` 的决策已按原样落地，实现放在隔离的 `@speed/api-client/react` 子路径下（`src/react.ts`），而非包主入口——主入口保持零依赖，仅这一子路径引入 `react`（作为该子路径的 required peerDependency），做法与 `@speed/i18n` 的 `./mui-locale` 子路径一致。两个 hook 的实际签名都要求显式传入共享缓存所依据的 `RequestFn`：`usePublicConfig(api)` 与 `useFeature(api, key)`（而不是上表为省略 `api` 参数所写的历史设计形态），共享一份按 `RequestFn` 引用键控的缓存。详见 `web/packages/api-client/README.md` 与 `AGENTS.md`。

## 运营后台的权限模型

`admin-shell` 面向平台内部员工，权限 domain 是 `system` 而非某个租户。`auth-core` 的 `usePermission` 对两者用同一套接口，差别只在 `/me` 返回的权限集来自哪个 domain——前端不需要两套权限逻辑，但**必须在 UI 上明确区分当前处于"平台视角"还是"租户视角"**，尤其在模拟登录期间要有持续可见的醒目标识，防止误操作。

## 状态管理与数据请求选型
- **服务端状态 TanStack Query + 客户端状态 Zustand**（不引入 Redux）。
- Query key 强制按租户命名空间：`['tenant', tenantId, resource]`。切换租户后 key 天然变化，旧数据自动失效；`switchTenant` 成功后显式 `removeQueries(['tenant', oldId])` 清理缓存，避免运营后台频繁切租户导致内存堆积。
- `currentTenantId` 放 Zustand 而非 Context：高频读低频写，selector 按需订阅避免大范围 re-render；且**可在组件树外读取**（如构造 query key 时）。
- **注意：前端不把 `tenantId` 作为请求头发送。** 租户上下文由 access token 携带，服务端只信任令牌（见 [04 数据层与多租户](04-data-and-tenancy.md) 的信任边界）。前端这份 `currentTenantId` 只用于三件事：query key 命名空间、UI 展示、调用切换租户接口时的入参。切换租户成功后拿到新令牌，随后所有请求自动带上新租户。
- Token 存储：refresh token 走 httpOnly+Secure+SameSite Cookie，access token 只存内存，不落 localStorage。
- **主题三层覆盖**：`defaultTokens`（包内置）→ `projectTokens`（业务项目 `theme/tokens.ts`，构建期）→ `tenantOverrides`（运行时从后端拉取，支持白标 SaaS 按租户换 Logo/主色）。业务项目只写差异部分，深合并回退默认值。品牌资产放业务项目 `public/`，包内不打包任何具体品牌资产。
- **计费 UI 配置驱动**：`Plan`/`Feature` 数据结构由后端 `/billing/plans` 下发，前端不硬编码套餐名与价格，同一套 UI 组件适配不同项目的定价模型。


> **已落地（auth-core round）：本表 `usePermission` / `useCurrentTenant` 归属 `@speed/auth-core` 的决策已按原样落地。** `web/packages/auth-core` 交付 `createAuthSession(store)`（内存态会话状态机：匿名/已认证快照、密码与短信登入、登出、切租户、step-up、刷新）与 `useAuthState` / `useCurrentTenant` / `usePermission(domain, permission)`（`attachSession` 绑定一个会话，last-bind-wins）；`usePermission` 的 domain 参数即下节"运营后台的权限模型"所分的 `tenant` / `system` 两域，实现是纯客户端集合查找（UX 便利而非安全边界，服务端独立授权）。
>
> **同轮机制注记：上节"/me 返回的权限集来自哪个 domain"所依赖的机制未被落地 API 支撑，需要修正**——shipped 的 `/api/v1/authn/me`（`authn_getMe`）只返回 `AuthnPrincipal`（身份：user_id / tenant_id / 会话信息），authn spec 中没有任何 permissions 字段；rbac 模块又刻意不挂 HTTP 路由，权限数据当前没有服务端下发端点。因此权限集只能由 **host 侧 attach**：`session.setPermissionSet('tenant' | 'system', string[] | null)`（`null` 清空该域），会话层执行存活规则——静默刷新与 step-up 保留两域、切租户丢弃 tenant 域并保留 system 域、换用户或登出清空两域、失败的操作不改动任何状态；权限数据的真实获取流程（含 `/me` 派生列表）留给 consumer-shell round 的设计注记。本表归属行不受影响：hook 归 `auth-core` 的决策按原样落地，`tenant` / `system` 的 domain 语义也未变。
>
> **token 传输现实注记（同一轮）**：上文"Token 存储：refresh token 走 httpOnly+Secure+SameSite Cookie，access token 只存内存"是设计目标；shipped 的 authn API 走的是另一条现实：token 签发响应把 refresh token 放在**响应体**（`AuthnTokenPair.refresh_token`，在 tenant-switch 与 step-up 响应中缺席——两者轮换既有 token 家族），**不设置 refresh cookie**——authn 设置的唯一 HttpOnly cookie 是 social 绑定预授权路径那个（`Path /api/v1/authn/social`）；refresh 端点在请求体里读取调用方持有的 token。`@speed/auth-core` 按此现实设计：access token 只进内存 store（`@speed/api-client` 每次发送前重读），refresh token 只存在于会话闭包、永不写入任何存储，无 `restore`——刷新页面即回到匿名，需重新登录（见该包 README 的 Known limitations）。是否引入 refresh cookie 或持久化层，留待 M4 发布基线与安全评审。

> **已落地（auth-ui round）：登录组件家族落地，上一注记留给 consumer-shell 的"运行时端到端消费"在形态层面兑现（真实 api-client + fetch 替身 seam，见 21-api-contract.md 的同一轮注记）。** `web/packages/auth-ui` 交付受控的登录组件家族，组件全部以 session prop 驱动、零 hooks 消费、成功后只触发一次回调、从不导航或直连网络：`SignInScreen` 以 tab 条组装通道（密码为默认通道，切换即卸载前一表单——刻意重置，通道错误不跨表面残留，社交块只在给了 `social` prop 时出现），`PasswordSignInForm` 单 identifier 字段（邮箱或手机，由后端决定），`SMSSignInForm` 两步 phone→code（请求步唯一的 code 形态失败是 `authn.rate_limited`），`RegisterForm` 以 `'@'` 启发式把 identifier 拆进 spec 的 email/phone 分离形态、trim 可选 display name、locale 在提交时读取；社交一半是 `SocialSignInSection`（每 provider 一个按钮，点击只请求该通道的 authorize URL——纯请求经 `onAuthorizeUrl` 上报，包从不导航）加 host 回调路由上的 `SocialCallbackHandler`（effect 以 `(code, state)` 对为键，StrictMode 双调用只发起一次交换）；会话出口是 `SignOutButton`（成功后刻意静默、失败可重试、渲染不区分是否已认证）与 `SessionEndedScreen`（`ui-kit` `EmptyState` 的 noPermission 变体、全部文本槽从 auth-ui namespace 覆盖——无 session prop、无 hooks、无网络）。全部内置文案来自双语 `auth-ui` namespace（每语言 59 键）；错误答案经 27-code 可达子集白名单解析（13 个登录/注册与 identifier code——含后来补上的 `invalid_email`/`invalid_phone`、6 个社交端点 code、5 个会话生命周期 code、3 个 client 传输 code），白名单外一律落 `unknown` 兜底，包内永远不渲染裸 key。浏览器 + 真服务器 leg 与跨路由门禁仍留待 reference-app shell / e2e。
>
> **同轮机制注记一（组件零 hooks 消费是刻意契约，host 侧会话观察是快照驱动）**：auth-ui 的组件契约是 auth-core round 的 host-gate 形态倒推的——session 以 prop 进组件、成功登录只发一次 `onSignedIn`，之后 host 用自己的 `useAuthState` 快照翻转决定渲染什么；`src/usage-example.test.tsx` 的 SessionGate fixture 就是 host-router 的迷你模型：认证快照 → app 视图；曾在 app 视图而快照转匿名 → `SessionEndedScreen`；首次认证前 → 登录面。会话结束是**可观察的而非可命令的**：无恢复、无 refresh cookie，服务端会话死亡经 api-client 的 401-刷新腿收敛——`refresh()` 解析 `false` 即本地登出、快照转匿名（api-client 的 Reporter seam 以 `access token refresh failed` 上报那次被拒的刷新），`SessionEndedScreen` 的 action 经 `onSignIn` 把用户交还 host 的登录面。跨路由门禁（layout-kit `RouteGuard`、shell 真实路由）不是本轮的形状，属 consumer-shell。
>
> **同轮机制注记二（permission-attach 缺口原样保留并指向 shell）**：auth-core round 注记记录的"权限集只能由 host 侧 attach、无服务端下发端点"这一缺口在 auth-ui round **没有变化**——登录组件家族只消费身份（会话快照与 `/me` 的 `AuthnPrincipal`），auth-ui 无任何权限集合参数；`usePermission` 的集合仍由 host 在 `attachSession` 后 set（auth-core 的 `setPermissionSet`），路由级门禁（layout-kit `RouteGuard` 的 `status` prop）仍是 consumer-shell round 的事。家族刻意停在身份层，不碰授权。
>
> **同轮机制注记三（register ≠ login；绑定 / MFA / 企业 SSO 表面未交付；`client.protocol` 拒绝 bound-identity 形态；channels 全是 props）**：注册成功不建立会话（spec 的 register 不是会话操作）——`RegisterForm` 靠 `onRegistered`（携带生成的 `AuthnUser`）或成功面板把新账号交还 host 的登录面再登录；绑定 UI（已认证用户给账号加通道）不在本家族——`SocialCallbackHandler` 只按登录面处理回调路由，交换答回 bound-identity 无 token 形态（服务端对已认证调用方的回答）时按 auth-core 的 `completeSocialLogin` 契约以 `client.protocol` 拒绝，绑定流程留给账号管理 UI；step-up 门槛操作与每租户配置的企业 OIDC 在本家族也没有表面。服务端不存在通道发现端点（`go/authn` 侧无此类 operation），家族只渲染 host 组合进 props 的通道与 provider——页面上方的品牌、标题与 register 链接同理都是 host 内容，不入包。

> **已落地（product-shell round）：consumer-shell 的一半以 `@speed/tenancy-ui` 与 `@speed/product-shell` 两个包落地；auth-ui 轮注记一/二/三中"跨路由门禁属 consumer-shell round"的声称由本注记部分取代——门禁作为 host 组合的形态已由套件证明，shell 自持门禁与权限真实获取仍未落地。** 两个新包把上文的 host 义务收进可交付形态：
>
> - **tenancy-ui 交付受控的租户切换控件**：`TenantSwitcher`（session 以 prop 进组件、从不自己 attach/观察/驱动会话、当前租户来自 host 的 `currentTenantId`、切换经 `session.switchTenant`、成功只发一次 `onSwitched`、当前租户行禁用、拒绝的切换渲染白名单码文本且可重试）——它是 `authn.switchTenant` 端点的 in-form 运行时消费证明：`src/usage-example.test.tsx` 用真实 api-client + 以真 `Response` 作答的 fetch 替身钉死一次登录加三次切换尝试（成功切换断言 store 持有新令牌、请求携带 `authorization` 与 `{tenant_id}` body；拒绝尝试断言码文本呈现、会话状态不变）。文案 12 叶/语言、9-code 白名单（六 `authn.*` 会话生命周期 + 三 `client.*` 传输）+ `unknown` 兜底，与 auth-ui 共享码的文案逐字同源（同层包互不 import 目录，两版文案以套件双向钉死配对）。
> - **product-shell 把 SessionGate 形态做成 shipped 三分支视图机**：`ProductShell` 只读 `useAuthState` 快照、从不驱动会话——认证快照 → `layout-kit` `AppShell` 框架包 children；匿名且本 mount 到达过 app → host 的 `sessionEnded` 槽或默认 `auth-ui` `SessionEndedScreen`（仅默认屏内连"回登录视图"action）；匿名且从未到达 → host 的 `signIn` 槽或空（刻意不设默认登录面，通道组合是 host 产品决策）。分支二先于分支三判定：登出过的用户绝不回落到新访客登录面——这就是 auth-ui 轮注记一里 SessionGate fixture 的 shipped 形态。壳本身零文案（无 namespace、无 locale、无错误白名单），全部 chrome props 原样透传给 `AppShell`。`src/usage-example.test.tsx` 编译并执行 README 组合：四 namespace 引导（ui-kit/layout-kit/auth-ui/tenancy-ui）+ `attachSession` + userMenu（`TenantSwitcher` 与 `SignOutButton` 并列），旅程走完整圈——登录 → 框架 → 切租户 → 登出 → 默认会话结束屏 → 再登录回框架，请求与 body 钉死。
> - **门禁与权限 attach：host 组合形态已证明，shell 自持仍延期**。auth-ui 轮注记二 deferral 的"路由级门禁（layout-kit `RouteGuard` 的 `status` prop）仍是 consumer-shell round 的事"现部分兑现为 `src/gated-journey.test.tsx` 的 fixture：`children` 里的 view-id mini-router 每个目的地挂 `RouteGuard`，status 由 `usePermission` 在 host attach 的列表上派生，切租户 commit 后按 auth-core 存活规则 re-attach（fixture 还以 role-load 替身扮演权限下发，因为真实获取流程——auth-core 轮机制注记 deferral 到 consumer-shell 的 /me 派生列表或 rbac 端点——仍无服务端形态）。旅程覆盖 pending→allowed 的列表重载、denial 期间刷新保持门禁稳定（`onDenied` 恰好一次）、被拒切换与会话死亡收敛。即：**门禁与 re-attach 是 host 在 `children` 里的组合，不是任何包代码**——product-shell 的 deferral 表明文不消费 `RouteGuard`/`usePermission`/`setPermissionSet`，auth-ui 轮注记一 deferral 的"浏览器 leg"（reference-app shell 真实路由与真服务器）保持原样。

> **已落地（account-ui round）：账号管理组件家族交付，auth-ui 轮注记三留给"账号管理 UI"的绑定流程在此兑现。** `web/packages/account-ui` 交付四个账号面区块 + 一个回调组件：`SessionsSection`（会话列表：服务端答 `is_current` 标记当前设备、行内单设备下线、双确认的一键下线其他设备、`revoked_count` 以 role=status 播报；revoked 会话留在列表灰显，当前会话不可从列表下线）、`LoginHistorySection`（最新 20 条登录历史，method/result/failure_reason 裸 token 走组件内已知清单渲染，清单外渲染通用文案）、`SocialBindingsSection`（已绑定身份列表 + 行内解绑 + 每 provider 一个按钮的 add 区，点击只请求该通道 authorize URL 并经 `onAuthorizeUrl` 上报、包从不导航）+ `BindingCallbackHandler`（host 回调路由上完成绑定：effect 以 `(code, state)` 对为键防 StrictMode 双交换，按应答形状分派——绑定形答 `{bound, identity}` → 刷新绑定列表 + `onBound` 一次；登录形答带 tokens → 渲染"已在别处登录"面板且不回调，auth-ui 轮注记三里 `SocialCallbackHandler` 以 `client.protocol` 拒绝的 bound-identity 形态在本包是正常路径）、`MfaSection`（step-up 门控的 TOTP 注册/更换 + 恢复码再生成，见机制注记二）。家族全部文案来自双语 `account-ui` namespace（每语言 106 键）；错误答案经白名单解析（会话生命周期 5 + 社交绑定 8 + 双因子 4 + 限流 1 + client 传输 3，共 21 code + `unknown` 兜底），其中 8 个与登录面同义的 code 逐字复用 auth-ui bundle 文案。runtime 端到端消费在形态层面兑现：`src/usage-example.test.tsx` 编译并执行 README quick start（真实 api-client、scripted fetch 答真实 `Response`、18 个请求按序 pinned、逐请求断言 authorization 头），浏览器 + 真服务器 leg 与 reference-app shell 仍留待 shell / e2e。
>
> **同轮机制注记一（读走生成 react-query hooks 是与 auth-ui 零-hooks 契约的刻意对照；host 多一个 QueryClientProvider）**：账号面读的是**可缓存的列表状态**（会话、登录历史、绑定身份），每次写后要失效重取——这正是 sdk 生成 hooks 层（shared-QueryClient 契约，见 [21 API 契约](21-api-contract.md)）的适用面；而登录表单的答案是 one-shot、不是缓存，所以 auth-ui 组件零 hooks 消费的契约不被破坏，两层各自成立。包内只消费生成 hooks 与导出的 query-key 构造器（`getAuthnListIdentitiesQueryKey` 等），失效从不手写 query key，组件也不自建 QueryClient（宿主拥有）；hooks 消费意味着 host 树在 auth-ui 所需 provider 之外**多一个 `QueryClientProvider`**（test-utils 的 `renderWithProviders` 同步多这一层）。连带效应：`@speed/api-sdk` 在本包从 auth-ui 的 type-only dependency 升级为**运行时 dependency**（hooks、query-key 构造器、`authnSocialCallback` 直呼都在这里执行）——包成为 sdk 生成面继 auth-core（compile consumer）之后第二个 in-workspace 消费证明，且是第一个把 hooks 真正渲染进组件树的包。
>
> **同轮机制注记二（MFA 面没有"状态"只有"行为"；step-up 验证是包内 dialog 而非页面路由；无 factor-status/disable op）**：authn spec 没有 factor-status operation 也没有 disable operation，`MfaSection` 因此永不声明"已启用/未启用"——状态经行为发现：注册请求 200 ⇒ 无激活因子、挂起向导（secret + provisioning URI 纯文本，包不引入 QR/剪贴板依赖）；403 `authn.step_up_required` ⇒ 存在激活因子，包内 step-up dialog 打开、经 `session.verifyStepUp(code)` 验证成功后再重跑被门控的操作（更换向导因此只在 step-up 后显示替换警告；恢复码再生成无条件走 step-up，重试后 200 ⇒ 面板、404 `authn.mfa_not_enrolled` ⇒ 引导文案指向注册入口）。验证成功只把新因子落进新 access token 的 `amr`，dialog 不承诺 token 轮换后不再询问——"step-up 只活在单个 access token 寿命内"在组件层原样呈现。恢复码只显示一次、离开即弃、包内永不缓存或再取。
>
> **同轮机制注记三（auth-ui 不 import；provider 词汇 copy-sync；session prop 的边界是 session operation 的边界）**：绑定面与登录面共享同一批 provider（authn spec 的 5 通道），但同层包互不 import 的规则成立——`SocialProvider`/`SocialProviderConfig` 在本包为自有定义、与 auth-ui 的副本逐字一致并由 spec 保证同轮同步；社交绑定回调与登录回调是同一 spec 端点的两种调用方形状，不需要 auth-ui 的任何类型。session prop 的边界守则：**有 session operation 才给 prop**——add 区的 authorize URL 请求（`session.socialAuthorizeUrl`）与 step-up 验证（`session.verifyStepUp`）是生成面表达不了的两个点，其余表面无 prop（读经绑定 client 的 access token 身份）。家族四区块是 section 不是页面：空/错态隐藏区块标题、渲染 ui-kit `EmptyState`，标题层级不跳级；导航、登入登出、密码修改（spec 无 change-password op，见 05 文档的 account-ui 轮注记）都不是本包形状。

