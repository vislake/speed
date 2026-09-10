---
title: "@speed/api-client:前端唯一的手写 HTTP 之家"
weight: 5
description: "为什么 api-client 是前端唯一的手写 HTTP 层:构造时捕获的可注入 fetch、内存专用访问令牌存储、仅带凭据的单飞 401 刷新、保守的瞬时重试、统一 ApiError 与机制保留的 client.* 命名空间,以及与 React 隔离的 config 钩子。"
---

# @speed/api-client:前端唯一的手写 HTTP 之家

前端发出的每一个请求——`@speed/api-sdk` 的生成调用、`@speed/auth-core`
会话的操作、宿主自己的 config 读取——都经由一次 `createClient` 调用
构建出的那一个请求函数。本页解释这条唯一接缝背后的设计决策,以及每
一项决策在回应什么威胁;日常怎么调用,看[用户指南的 api-client 使用页](/zh-cn/docs/user-guide/modules/web/api-client/)。

## 职责与边界

- **`speed/no-direct-http` 规则的唯一白名单。** 任何其它包 `src` 里
  的直接 `fetch`、`window.fetch` / `globalThis.fetch`、`new
  XMLHttpRequest`、或 axios/node-fetch 导入都是 ESLint 错误,而本包
  是该规则唯一的配置级白名单(规则与测试在 `web/eslint-rules/`)。
  只有一个家,意味着传输行为——认证、重试、超时、错误形态——只被
  决定一次、被每个调用方共享;生成 SDK 与会话层都从它上面调用下去,
  从不绕开它。
- **无 UI、无 i18n 资源、无 storage API。** 错误码由消费包自己的目录
  映射为双语文本——这里从不发出用户可见的文字。访问令牌存储按设计只
  住内存(见下)。
- **任何地方都没有租户头。** 租户上下文在访问令牌内部;客户端没有可
  附加的租户概念。
- **只走 JSON 文本**——上传、SSE 与原始字节响应不提供,各带理由
  记录在包 README。
- **config 钩子住在隔离的 `./react` 子路径**——镜像 `@speed/i18n`
  的 `./mui-locale`:主入口保持零依赖,react 只是该子路径的必需
  peer。

## 设计:为什么每个失败都是一个 `ApiError`

每个失败请求都拒绝为一个 `ApiError`,原样携带 API 信封的 `code` /
`traceId` / `params` / `details`——没有信封可读时则携带合成码。两个
决定让这个形态值得信赖。其一,**`code` 是唯一必需的线上字段**:后端只
发 `{code, params}`,要求更多就会把每一个真实模块码都丢进 `client.*`
回退;`traceId` 在后端发出时浮出用于关联。其二,**`client.` 前缀是按
机制保留,而非按约定**:服务端码按模块划域(`authn.*`、`notes.*`),
`client` 不属于任何模块,因此以 `client.` 开头的信封在解析期就被拒绝,
浮出为诚实的 `client.http.<status>`——行为不端的后端或中间层无法让
一个会话错误读起来像"请求超时"。`isTransportFailure` 用 `status` 0
回答"这个请求是否从未到达可用响应"——那是任何 HTTP 响应都不可能携带
的状态——消费方的白名单应当询问它,而不是裸匹配码。

## 设计:为什么访问令牌住在内存

`localStorage` 里的访问令牌,是 XSS 随手拿走的凭据。令牌存储是一个
朴素的双方法接缝(`get` / `set`);内存实现是本包提供的唯一实现,这里
根本不存在 storage API。两个推论随之而来:令牌在每次尝试前重读,所以
刷新后的重试带着新令牌;请求可以声明 `omitAccessToken` 即使在持有令
牌时也完全跳过存储——生成的会话刷新操作正是这样声明的(见
[api-sdk](/zh-cn/docs/developer-docs/modules/web/api-sdk/) 设计页)。

刷新令牌同样不是本包的业务:authn API 在签发令牌的响应体里返回它、
不设刷新 cookie,所以由会话层(`@speed/auth-core`)在闭包里持有并驱动
刷新操作。`api-client` 只定义缝:`refreshAccessToken?: () =>
Promise<boolean>`。

## 设计:为什么 401 刷新只认带凭据的请求、单飞、且只一次

当携带 bearer 令牌的请求答 401 且钩子已配置时,客户端跑一次刷新——
并发的 401 共享同一个在飞刷新 promise,所以一阵过期会话请求只触发一
次刷新——然后把原请求恰好重试一次,任何方法,在瞬时重试预算之外。
刷新失败把原 401 拒绝为 `auth: true` 的 `ApiError` 并报告 `access
token refresh failed`。

只认带凭据的规则是承重墙。无凭据请求上的 401 意味着端点要求认证,刷
新无从提供,所以它原样浮出。会话自己的刷新请求*靠声明*不带凭据,所
以一个被拒的刷新令牌原样浮出,而不是重入刷新路径等待它自己。刷新回
合还是独立交换:`timeoutMs` 界定每次 HTTP 交换,从不界定花在刷新钩子
里的时间,所以一次缓慢的刷新无法把被拒 401 自己的信封——它的 code 与
trace id——降级成合成的 `client.http.401`。

## 设计:为什么瞬时重试如此保守

只有幂等方法(GET/HEAD/OPTIONS)会被重试,只在 429(尊重
`Retry-After`,有上限)、502/503/504、网络失败与超时上重试,延迟按
指数全抖动,预算为冻结的 `DEFAULT_RETRY_POLICY`:3 次尝试 / 200 ms
起 / 4 秒上限。瞬时预算与 401 刷新回合从不重叠:刷新重试不做瞬时重
试、也从不消耗一次。取消从不被重试、从不被包装——abort 你的
`signal` 拒绝原始 `AbortError`,于是 TanStack Query 这类查询层保持标
准取消语义。每次尝试的计时器覆盖整个交换、含响应体,所以一个先答头
再卡住体的服务器以 `client.timeout` 拒绝,而不是把请求吊在半开响应
上。为重试而丢弃的响应先取消其 body,释放连接。

## 对外稳定面

`createClient(options)` 返回 `RequestFn` 类型;`ApiError` 类(无响
应到达时 `status` 为 0,`auth` 恰好只在 HTTP 401 时为 true)与
`isApiError` 守卫、`isTransportFailure` 谓词;保留的
`ERROR_CODE_NETWORK` / `ERROR_CODE_TIMEOUT` / `ERROR_CODE_PROTOCOL`
常量与 `httpErrorCode(status)`;`AccessTokenStore` 类型与
`createMemoryAccessTokenStore()`;`RetryPolicy` 类型、冻结的
`DEFAULT_RETRY_POLICY` 与纯函数 `retryDelayMs` / `retryAfterDelayMs`;
带 console 默认实现的 `Reporter` 缝;两个 pre-auth config 抓取器
(`fetchPublicConfig` / `fetchSystemFeatures`,走
`CONFIG_PUBLIC_PATH` / `SYSTEM_FEATURES_PATH`,与 `go/config` 及其
OpenAPI 片段手工保持同步——片段生成的操作为主消费面,这两个封装是
其下的逐键映射层);以及 `./react` 子路径的
`usePublicConfig` / `useFeature`——每个 `RequestFn` 身份共享一次抓
取,`useFeature` 在同一缓存上合成、加载中或出错时默认 `false`,从不
抛出。

## Source

- 包契约与决策:[AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md)
- 执行规则的实现:[web/eslint-rules/](https://github.com/vislake/speed/tree/main/web/eslint-rules)(`speed/no-direct-http`)

## 相关页

- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——本页挂靠的分层综述;"唯一的手写 HTTP 之家"一节就是本包
- 怎么用:[用户指南的 api-client 页](/zh-cn/docs/user-guide/modules/web/api-client/)
- web HTTP 组其余设计页:[api-sdk](/zh-cn/docs/developer-docs/modules/web/api-sdk/)——经由这条缝调用的生成面;[auth-core](/zh-cn/docs/developer-docs/modules/web/auth-core/)——填充刷新缝的会话层
- 两个 config 抓取器的 Go 侧:[config](/zh-cn/docs/developer-docs/modules/core/config/)
