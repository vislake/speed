---
title: auth-ui
weight: 8
description: "登录组件族——SignInScreen 及其密码、短信码与注册频道、社交登录、登出动作与会话结束占位页,全部是以 auth-core 会话为 prop 的受控组件。"
---

# auth-ui

`@speed/auth-ui` 是基于 speed 的前端的登录组件族:密码、短信码与注
册频道,社交登录区块与完成其交换的回调处理器,登出动作与会话结束
占位页——由 `SignInScreen` 在频道页签条后组装起来。它是
[authn](/zh-cn/docs/user-guide/modules/identity/authn/) 模块登录面
的前端面孔:每个组件都是**受控组件**,驱动宿主以 prop 传入的
`@speed/auth-core` 会话。

## 它做什么

这个包渲染租户面向应用的登录之门:`SignInScreen`(连同
`SignInChannel` 与 `SocialSignInOptions` 形状)、频道表单
`PasswordSignInForm`、`SMSSignInForm` 与 `RegisterForm`、社交区块
(`SocialSignInSection`、`SocialCallbackHandler` 与
`SocialProvider`/`SocialProviderConfig` 类型)、`SignOutButton`、纯展
示的 `SessionEndedScreen`,以及 `AUTH_UI_NAMESPACE` /
`authUiResources` 资源对。定义这个家族的契约:

- **这里没有任何组件消费 auth-core hooks、读取或持久化会话状态、
  导航、或直接碰网络。** 每个请求都是会话操作,经宿主绑进共享接缝
  的客户端发出;登录成功恰好触发一次 `onSignedIn`——接下来的一切
  都是宿主的,而且宿主回调只在操作落定后运行。
- **一次失败的提交不改变会话上的任何东西**,只经整次尝试的
  `role="alert"` 横幅把错误码解析成当前语言文本;白名单之外的码渲染
  `errors.unknown` 回退,原始键绝不出现。
- **这里不交付**:注册即登录(`register` 不是会话操作)、账户绑定与
  step-up(那些在 [account-ui](/zh-cn/docs/user-guide/modules/web/account-ui/) 里),
  以及频道发现——家族只渲染宿主声明的频道。

## 何时选用

任何用户用账号登录的租户面向前端。家族假定宿主已经接好会话层
(`@speed/auth-core` 加一个 `@speed/api-client`)——它是宿主门存在的
理由,而不是它的消费者。宿主拥有包无法决定的产品决策:频道组合
(没有东西能投递验证码时绝不提供短信登录)、表单之上的页面内容,
以及登录成功后去向何方。

## 接线

```tsx
// 一个会话与一个客户端共享同一个内存访问令牌 store;静默刷新腿
// 就是会话自己的 refresh()。
const store = createMemoryAccessTokenStore()
const session = createAuthSession(store)
bindRequestFn(createClient({
  baseUrl: 'https://api.example.com',
  fetch: fetchImpl, // 宿主的 fetch 实现
  accessTokenStore: store,
  refreshAccessToken: () => session.refresh(),
}))

// 双语实例;两个命名空间各恰好注册一次(ui-kit 的也要,因为
// FormField 的校验文案住在那里)。
const i18n = createI18n({ supportedLanguages: ['zh-CN', 'en-US'], /* ... */ })
registerNamespace(i18n, AUTH_UI_NAMESPACE, authUiResources)
registerNamespace(i18n, UI_KIT_NAMESPACE, uiKitResources)
attachSession(session) // hooks 读取已挂接的会话

// 宿主门:已认证 → 应用;快照在曾持有认证的视图上变匿名 →
// <SessionEndedScreen />;首次认证之前 → <SignInScreen session={session} />。
```

中间那条门分支需要一段宿主记忆——应用曾经到达过——以组件状态保
存,正如 [product-shell](/zh-cn/docs/user-guide/modules/web/product-shell/) 视图机
打包它的方式。

## 核心概念与 API 要点

- **`SignInScreen`** —— `session`(必填)、`channels`
  (`readonly ('password' | 'sms')[]`,默认两者都提供)、`social`、
  `defaultChannel`、`onSignedIn`。切换频道会卸载前一个表单,半填的
  状态与整次尝试的错误随之重置;只声明一个频道时直接渲染表单,没有
  页签条,屏幕本身也不渲染自己的标题。
- **`PasswordSignInForm`** —— 一个标识字段(邮箱或手机号,由后端
  决定是哪个)加密码字段,驱动 `session.loginWithPassword`。
- **`SMSSignInForm`** —— 两步流程:手机号步骤请求验证码
  (`session.requestSMSCode`),其 202 接受就是该步骤的终态;验证码步
  骤经 `session.loginWithSMSCode` 完成登录。新验证码让验证码字段从
  空开始,过期码绝不会搭车进入新的验证码会话。在并发登录竞争中央败
  的提交会得到 `OperationSupersededError`——无横幅、无 `onSignedIn`
  ——而短信渠道还会渲染"验证码已使用"提示,因为落败的提交花掉了它
  的一次性验证码。
- **`RegisterForm`** —— 注册绝不登录:`'@'` 启发式把单个标识字段拆
  成规范里分离的邮箱/手机号槽,语言在提交时读取,创建的用户交给
  `onRegistered`(其类型是生成的 `AuthnUser`)——没有回调则渲染成功
  面板。
- **社交登录** —— `SocialSignInSection` 为每个配置的提供商渲染一个
  描边按钮;点击向会话要该渠道的授权 URL——纯请求,经
  `onAuthorizeUrl` 向上报告,绝不是导航。`SocialCallbackHandler` 在
  宿主回调路由完成交换,effect 以 `(code, state)` 对为键,StrictMode
  恰好只启动一次交换;失败的交换对同一对参数可重试。
- **`SignOutButton`** —— 驱动 `session.logout()`;失败的登出渲染应答
  的码文本并可重试,成功者刻意安静(宿主的 hooks 观察到翻转)。
- **`SessionEndedScreen`** —— 纯展示,没有会话 prop:视图的认证快照
  刚变匿名时的占位。它用 ui-kit 的 `EmptyState` `noPermission` 变体
  渲染,所有文本槽都从本包命名空间覆盖;`headingLevel` 默认 `h1`,
  即不承接任何层级的标题级别。

## 边界与注意

- **注册不是登录** —— `RegisterForm` 从不登录(规范与 auth-core 契
  约皆然);新账号之后经登录面登录。
- **会话状态活不过一次页面加载** —— auth-core 的既有局限,被继承:
  刷新令牌只在会话闭包里,重载即匿名。
- **没有频道发现** —— 服务端不回答"该租户可用哪些频道";宿主组合
  频道清单,`SignInScreen` 没有 `social` prop 就不渲染社交区块。
- **账户绑定与 step-up 不在这里** —— 已登录调用者绑定新渠道或执行
  step-up 门控操作属于 [account-ui](/zh-cn/docs/user-guide/modules/web/account-ui/);
  企业 SSO 的发现是按租户的服务端配置,没有区块渲染它。
- **组件不向宿主路由器发信号** —— 门观察快照并做决定。

## Source

- 包 README:
  [web/packages/auth-ui/README.md](https://github.com/vislake/speed/blob/main/web/packages/auth-ui/README.md) —— 权威文档(导出、props 表、错误码白名单、可访问性、测试装置)
- 前端分层:[搭建前端](/zh-cn/docs/user-guide/domains/frontend-building/)
- 后端表面:[authn](/zh-cn/docs/user-guide/modules/identity/authn/) 模块页;错误码见 [authn](/zh-cn/docs/user-guide/error-codes/#authn)
- 相关包页:[account-ui](/zh-cn/docs/user-guide/modules/web/account-ui/)、[product-shell](/zh-cn/docs/user-guide/modules/web/product-shell/)
- 同级包 `@speed/auth-core`、`@speed/ui-kit` 与 `@speed/layout-kit` 各在本组的页面
