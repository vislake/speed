---
title: auth-ui
weight: 8
description: "登录组件族——以 auth-core 会话为 prop 的受控组件:为什么切换频道以卸载重置、注册绝不登入、社交回调扛得住 StrictMode,以及错误文案为何经可达码白名单解析。"
---

# auth-ui

`@speed/auth-ui` 是基于 speed 的前端的登录组件族:密码、短信码与注
册频道,社交登录区块与完成其交换的回调处理器,登出动作与会话结束
占位页——由 `SignInScreen` 在频道页签条后组装起来。它是
[authn 模块](/zh-cn/docs/developer-docs/modules/identity/authn/)登录
面在 `@speed/auth-core` 无头会话之上的前端脸面。本页解释组件族形态
背后的"为什么";[用户指南的 auth-ui 页](/zh-cn/docs/user-guide/modules/web/auth-ui/)讲怎么用。

## 职责与边界

每一件组件都是受控组件,驱动宿主以 prop 传入的 `@speed/auth-core`
会话;边界在每一侧都相同:

- **本家族不消费 auth-core hooks,不 attach、不读会话状态,不导航,
  不碰网络。** 每个请求都是经宿主绑定客户端的会话操作。消费 hooks
  的组件反正必须挂在宿主的 `attachSession` 之下——本家族是宿主门
  禁存在的原因,不是它的消费者。
- **登录成功恰好触发一次宿主的 `onSignedIn`;之后发生的一切都是宿
  主的事。** 宿主回调只在操作判定落定后运行;抛异常的回调也被包住
  ——它不可能重新归类结果,因为提交已经发生。
- **家族只渲染宿主组合进来的频道。** 服务端没有频道发现端点可查;
  页面装饰——品牌、标题、注册链接——都是宿主内容。
- **注册不是登录,绑定不属于本家族。** `register` 不是会话操作,所
  以 `RegisterForm` 把新用户交给宿主的 `onRegistered`,或显示成功
  面板。给已登录账号绑定渠道与 step-up 门控的双因子属
  `@speed/account-ui`;企业 SSO 按租户配置,两边都没有区块。刷新页
  面即回到匿名——会话状态按 auth-core 契约只存内存,可经快照观察,
  不可命令。

## 各部件为何如此成形

**页签条以卸载来重置。** 切换频道会卸载前一表单,它的半填状态与整
次尝试的错误随之消失——频道错误不得跨表面残留,这是刻意的重置。可
供频道由宿主声明:部署绝不能提供一个完不成的频道(短信登录配一个
发不出码的通道,是任何手机都兑现不了的承诺);只声明一个频道时表单
直接渲染、没有页签条——无可切换之物的 tablist 不是选择,ARIA 接
线也会悬空。

**输掉的竞争不是失败。** 提交的登录输掉并发竞争时,auth-core 答
`OperationSupersededError`,表单按输掉的竞争对待:无错误横幅、无
`onSignedIn`——赢家的调用自己恰好触发一次。短信频道还要听见竞争
的代价:手机登录码服务端一次性,输掉的提交花掉的正是它自己验证过
的码,不得重提;只有新码能登录。

**注册绝不登入。** 按 spec 与 auth-core 契约,`register` 不是会话操
作——新账号经登录面登入。`'@'` 启发式把单一标识符字段拆进 spec
分离的 email/phone 形态,而不是把含混的值交给后端猜。

**社交登录是纯请求,由宿主完成。** 点击 provider 只向会话要该渠道
的授权 URL,经 `onAuthorizeUrl` 上报——会导航的包无法活在另一个宿
主的路由、弹窗或新页签流程里。`SocialCallbackHandler` 在宿主回调
路由完成交换,effect 以 `(code, state)` 对为键,StrictMode 的双重
effect 调用只发起一次交换。挂载时发现会话已认证,是完成交换后的重
入:一次性码已消费,不再发起第二次交换——处理器再触发一次
`onSignedIn`,pending 提示一直亮到宿主反应为止。pending 与已发送态
经 `role="status"` 播报;失败一律落进一个 `role="alert"` 横幅,绝
不做逐字段文案。

**会话结束是纯占位。** `SessionEndedScreen` 渲染 `ui-kit`
`EmptyState` 的 `noPermission` 变体——内容重新上锁,直到用户再登
录——每个文本槽都从 auth-ui namespace 覆盖,无 session prop、无
hooks、无网络。标题默认 `h1`,因为它整页替换已认证页面:整页占位
不延续任何层级。

## 错误文案:可达码白名单

每条提交路径把失败归一到单个错误码,经同一个整次尝试横幅渲染。解
析器只把本表面可达的回答——authn 的登录、注册、社交与会话生命周
期族,加 `client.*` 传输码——映射到专属文案;白名单外的一切,未来
的 authn 码也在内,都渲染 `errors.unknown` 兜底:包内永不显示裸
key,缺失翻译也绝不会漏出另一种语言。分类器把非 `ApiError` 形态
的抛出塌缩到刻意不在白名单的码上,兜底正是它们的落点——会抛的操
作总有一个码可显示。

## 对外稳定面

宿主所对的表面小而稳:八个导出组件及其 prop 表(凡有操作处
`session` 必填)、镜像 authn spec 渠道清单的
`SocialProvider`/`SocialProviderConfig` 类型,以及双语的
`AUTH_UI_NAMESPACE`/`authUiResources` 对——宿主重写文案的方式是
在 bootstrap 注册自己同键的 bundle 对,绝不是改组件文本。
`@speed/api-sdk` 只是 type-only 依赖;`src/` 里没有任何直连 HTTP,
由工作区的 `no-direct-http` 规则强制。

## 相关页

- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——包分层与受控组件契约
- [account-ui 设计](/zh-cn/docs/developer-docs/modules/web/account-ui/)——账号故事的已登录一半;[tenancy-ui 设计](/zh-cn/docs/developer-docs/modules/web/tenancy-ui/)——同层邻居;[product-shell 设计](/zh-cn/docs/developer-docs/modules/web/product-shell/)——本家族填充其登录分支的视图机
- 用户指南:[auth-ui 模块](/zh-cn/docs/user-guide/modules/web/auth-ui/)、[authn 模块](/zh-cn/docs/user-guide/modules/identity/authn/)、[身份与访问域](/zh-cn/docs/user-guide/domains/identity-access/)、[前端构建域](/zh-cn/docs/user-guide/domains/frontend-building/)
