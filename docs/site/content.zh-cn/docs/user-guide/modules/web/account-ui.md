---
title: account-ui
weight: 9
description: "已登录账号管理家族——带逐会话与批量撤销的会话列表、登录历史、带绑定回调的社交绑定,以及 step-up 门控的 TOTP 与恢复码设置。"
---

# account-ui

`@speed/account-ui` 是基于 speed 的前端的账号管理组件家族:[auth-ui](/zh-cn/docs/user-guide/modules/web/auth-ui/) 家族结束之
处的账号故事已登录一半。四个表面组成宿主的账号页——会话列表(逐会
话与批量撤销)、登录历史、社交绑定表面(连同在宿主回调路由完成绑定
的回调处理器),以及 step-up 门控的 TOTP/恢复码设置——全部渲染后端
[authn](/zh-cn/docs/user-guide/modules/identity/authn/) 模块。

## 它做什么

包交付 `SessionsSection`、`LoginHistorySection`、
`SocialBindingsSection`(带 `SocialProvider` / `SocialProviderConfig`)、
`BindingCallbackHandler`、`MfaSection`,以及 `ACCOUNT_UI_NAMESPACE` /
`accountUiResources` 对。这些表面刻意是**区块,不是路由页**:拥有它
们的页面、它们上方的标题、以及本包之外的一切表面(资料字段、密码设
置——authn 规范都没交付)都是宿主内容。

层级比 auth-ui 高一层,站在 api-sdk 契约的生成 hooks 一侧:**读经
`@tanstack/react-query` 生成进 `@speed/api-sdk` 的 hooks,跑在宿主的
QueryClient 上**;写经同一 `bindRequestFn` 接缝上的生成 mutation——
这里没有任何东西读存储、挂接会话、导航或直接碰网络。两个区块以
`@speed/auth-core` 会话为 prop,各自只为恰好一个会话操作
(`SocialBindingsSection` 用于添加区的授权 URL 请求,`MfaSection` 用
于 step-up 挑战的 `verifyStepUp`);另外两个完全无 prop。内置文案都
从双语 `account-ui` 命名空间渲染;危险对话框与空/错状态组合 ui-kit
的 `ConfirmDialog` / `EmptyState`。

## 何时选用

已登录账号页需要一个安全区块:哪些会话在活、如何撤销它们、登录历
史显示什么、绑定了哪些社交身份、以及双因素设置。家族假定宿主的登
录面先运行过、内存 store 持有活令牌;从不登录用户的宿主没有东西可
渲染。

## 接线

```tsx
const store = createMemoryAccessTokenStore()   // 登录种下的令牌
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl,
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, ACCOUNT_UI_NAMESPACE, accountUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: 0 } } })
// <QueryClientProvider client={queryClient}> 包住账号页,页面在宿主
// 自己的标题下组合四个区块。
```

react-query 的重试与缓存策略是宿主自己的,绑定回合也是:添加区经
`onAuthorizeUrl` 上报授权 URL,宿主把提供商重定向路由到该 URL 的
`redirect_uri` 所指的回调路由,`BindingCallbackHandler` 在那里完成交
换,其 `onBound` 导航返回。

## 核心概念与 API 要点

- **`SessionsSection`**(无 prop)——authn 模块为该账号持有的每个会
  话,请求自己的那个带当前徽章。既非当前也未撤销的会话有行尾登出按
  钮,无需二次确认;区块顶部的*登出其它设备*在 ui-kit 双重确认的危
  险 `ConfirmDialog` 之后,服务端的 `revoked_count` 应答在成功通知中
  呈现。每次成功撤销都让列表查询失效。
- **`LoginHistorySection`**(无 prop)——服务端登录尝试列表的最新一
  页,固定在 20 行、从不分页;方法与失败原因 token 只在组件的已知
  token 清单上才渲染。
- **`SocialBindingsSection`** —— `session`、`providers` 与
  `onAuthorizeUrl` props。每个已绑身份列出提供商与邮箱;解绑动作在
  危险 `ConfirmDialog` 之后,被拒的解绑(如 `authn.last_login_method`)
  带码文本留在页上。添加区为每个尚未绑定的已配置提供商渲染一个按
  钮。提供商词汇刻意复制而非从 auth-ui 导入——同层包永不互相
  import;authn 规范是共享的事实源。
- **`BindingCallbackHandler`** —— 在宿主回调路由经一次普通生成调用
  完成绑定:绑定是往调用者自己的账号加身份,不登录任何人。绑定形状
  的应答经导出的查询键构造器使身份列表失效并恰好触发一次 `onBound`;
  登录形状的应答(调用者的登录已死,交换变成了登录)渲染"已在别处登
  录"面板,不触发任何回调;失败的交换对同一 `(code, state)` 对可重
  试。
- **`MfaSection`** —— `session` prop 只为挑战对话框而存在;注册、确
  认与再生成都是普通生成 mutation。规范没有因子状态、没有禁用操作,
  区块因此从不声明已启用或已禁用——状态经动作发现,每个被服务端门
  控的动作在访问令牌没有新鲜二因子证明时都以 403
  `authn.step_up_required` 应答,于是打开驱动 `session.verifyStepUp`
  的 step-up 对话框:成功落定一枚 `amr` 携带该因子的新访问令牌,并准
  确重试被门的那个操作。确认应答的恢复码打开一次性展示面板——恢复
  码唯一出现的地方;这里没有任何缓存或重新取回。

每条失败路径都经可达码白名单——会话生命周期族、社交绑定应答、MFA
与 step-up 应答、`authn.rate_limited` 与 `client.*` 传输码——解析成
一个 `role="alert"` 横幅,`errors.unknown` 回退保证原始键绝不出现;
失败语境与登录面相同的码逐字复用 auth-ui 包的文案。

## 边界与注意

- **无因子状态、无禁用、无登录门控** —— 只有规范交付的动作存在:设
  置或替换验证器、确认它、再生成恢复码。
- **恢复码恰好出现一次**,明文呈现;离开一次性面板即丢弃,再看只能
  再生成。注册是手动录入——没有二维码或剪贴板依赖。
- **会话列表是服务端的列表** —— 已撤销会话保留在列(置灰),当前会
  话永不可从列表撤销;登出眼前这台设备是宿主的登出动作。
- **step-up 提升只活一枚访问令牌的生命期**,会话状态也活不过页面加
  载——auth-core 的既有局限,与会话家族的每个包一样。
- **账号页的其它一半是宿主内容** —— authn 规范没有改密或资料操作,
  没有区块覆盖它们。

## Source

- 包 README:
  [web/packages/account-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/account-ui/README.md) —— 权威文档(导出、props 表、错误码白名单、可访问性、测试装置)
- 前端分层:[搭建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
- 后端表面:[authn](/zh-cn/docs/user-guide/modules/identity/authn/) 模块页;错误码见 [authn](/zh-cn/docs/user-guide/error-codes/#authn)
- 相关包页:[auth-ui](/zh-cn/docs/user-guide/modules/web/auth-ui/)、[tenancy-ui](/zh-cn/docs/user-guide/modules/web/tenancy-ui/)
- 同级包 `@speed/auth-core`、`@speed/ui-kit` 与 `@speed/api-sdk` 各在本组的页面
