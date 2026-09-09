---
title: "@speed/api-client"
weight: 5
description: "前端唯一手写 HTTP 的家——createClient:可注入 fetch、纯内存的访问令牌存储、静默单飞 401 刷新、保守的瞬时重试、ApiError 归一化与 Reporter 接缝。"
---

# @speed/api-client

`speed` 前端发出的每一个 HTTP 请求,都在 `@speed/api-client` 里定义
并执行。一次 `createClient` 调用把传输层的各项决策——可注入的
`fetch`、纯内存的访问令牌存储、每请求超时、静默的单飞 401 刷新、
保守的瞬时重试预算与结构化 reporter——接成前端其余部分调用的
那一个带类型的请求函数(`RequestFn`)。

它刻意是*唯一*手写 HTTP 的包:`speed/no-direct-http` ESLint 规则把
直接的 `fetch`/`window.fetch`/`globalThis.fetch` 调用、`new
XMLHttpRequest`、或任何其它包 `src` 里的 `axios`/`node-fetch` import
都判为错误,而这个包是这条规则唯一的白名单。它是后端那条"前端只能
用生成的面"纪律的前端镜像。本包不带 UI、不带 i18n 资源、也不带
storage API:错误 `code` 在消费方自己的目录里映射成双语文本;访问
令牌只活在内存里(`localStorage` 里的访问令牌就是 XSS 随手可拿走的
凭据)。

它不是什么:它不是会话层。刷新令牌从不进入本包——authn API 在
签发令牌的响应体里返回它、也不设刷新 cookie——所以像
[@speed/auth-core](/zh-cn/docs/user-guide/modules/web/auth-core/) 这样
的会话层把它握在闭包里,驱动刷新操作走本包只负责定义的那个接缝。

## 何时使用

每个与 speed 后端对话的主机都用本包,而且只用在一处:它的 bootstrap
构建一个客户端,再用 `bindRequestFn` 把它接进生成的面
([@speed/api-sdk](/zh-cn/docs/user-guide/modules/web/api-sdk/))。
你很少直接调客户端——生成的操作替你调;要改传输行为,改的是这一个
客户端,绝不是生成代码。

只有不存在 spec fragment 的地方才动用本包自己的函数:`go/config`
的两个免认证端点是手工维护的(见
[config](/zh-cn/docs/user-guide/modules/core/config/)),由主入口的
`fetchPublicConfig`/`fetchSystemFeatures` 提供;渲染 React 时也可用
`@speed/api-client/react` 子路径的 `usePublicConfig`/`useFeature`
两个 hook。

## 安装与接线

```ts
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession } from '@speed/auth-core'

const accessTokenStore = createMemoryAccessTokenStore()
const session = createAuthSession(accessTokenStore)   // 会话层,见下

bindRequestFn(
  createClient({
    baseUrl: '/api/v1',                        // 或 scheme + host + 前缀
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),   // 静默 401 刷新
    timeoutMs: 10_000,
  }),
)
```

`fetch` 可注入(测试传入确定性的替身);缺省时在构造时刻捕获环境的
全局 `fetch`,而不是每次调用时才查。store 初始为空,登录填入之前,
请求都不带 `Authorization`。客户端只构建一次并持有同一引用——下面
的共享 hook 以它的身份做缓存键,`bindRequestFn` 也是后绑定的单次
绑定。

## 核心 API 与使用要点

- **一个请求函数,一种错误类型。** `RequestFn` 是
  `<T>(path, options?) => Promise<T>`。任何失败都 reject 一个
  `ApiError`,携带 `status`(没有响应到达时为 0)、`code`、可选的
  `traceId`/`params`/`details`、`attempts`,以及 `auth`——只在 HTTP
  401 时为 true。带 API envelope 的响应原样呈现其 `code`;其它一切
  (代理错误页、断网、超时)得到一个保留的 `client.*` 码——
  `client.network`、`client.timeout`、`client.protocol`、
  `client.http.<status>`。前缀是机制层面保留的,不只是约定:`client`
  不属于任何模块的域,所以借用该前缀的 envelope 在解析时就被拒绝,
  如实以 `client.http.<status>` 呈现。用 `isApiError` 区分错误;
  `isTransportFailure` 回答"请求是否从未完成"(`status` 为 0——这是
  任何 HTTP 响应都带不来的状态,答案无法伪造)。
- **不带 storage API 的 Bearer 认证。** `AccessTokenStore` 接缝只有
  两个同步方法(`get`/`set`);本包只提供内存实现。令牌在每次尝试前
  重新读取,所以重试的请求带着新令牌。请求可以声明
  `omitAccessToken`,不带凭据出行、不被 store 触碰。任何地方都没有
  tenant 头:租户上下文在访问令牌内部旅行。
- **静默 401 刷新,仅限 bearer。** 当携带 bearer 令牌的请求答 401、
  且配置了 `refreshAccessToken` 时,只跑一次刷新——并发的 401 共享
  同一个在飞刷新,所以一批过期会话请求恰好触发一次——被拒的请求
  再重试恰好一次,任意方法,不占瞬时重试预算。刷新失败把原来的 401
  以 auth `ApiError` reject,并经 reporter 上报 `access token refresh
  failed`。不带凭据的请求收到 401 时原样呈现——这条规则是承重的:
  会话刷新操作本身就以无凭据出行(`omitAccessToken`,由生成的面
  设置),所以被拒的刷新令牌就此终结,而不是再次进入刷新路径等着
  自己。
- **保守的瞬时重试。** 只有幂等方法(GET/HEAD/OPTIONS)会重试,只在
  429(尊重 `Retry-After`,上限 `maxDelayMs`)、502/503/504、网络失败
  与超时时重试,预算为 `DEFAULT_RETRY_POLICY`(3 次尝试、初始
  200 ms、上限 4 s),退避为指数全抖动。调用方取消从不重试、也从不
  包装:中止你的 signal 会 reject 原始的 `AbortError`,查询层因此
  保持标准的取消语义。每尝试的超时覆盖响应体,退避睡眠也与你的
  signal 赛跑。
- **结构化上报。** `Reporter` 接缝收到常量的英文消息加 snake_case
  属性;默认 sink 写到 `console.error`/`console.warn`(浏览器没有
  结构化日志后端),主机通过 `ClientOptions.reporter` 替换它。
- **config hooks** 位于隔离的 `./react` 子路径,让 React 不进入主
  入口的 import 图。`usePublicConfig(api)` 返回 `{ data, error,
  isLoading, refresh }`,由使用同一 `api` 引用的所有组件共享——一次
  fetch,不是每组件一次——`useFeature(api, key)` 在同一缓存上组合,
  返回纯 `boolean`,加载中与出错时都为 `false`,从不抛出。两个端点
  都在服务端按请求 host 解析租户,所以两个 hook 都不接受 tenant
  参数。

## 边界与注意

- 客户端只构建一次,处处共享同一引用;每次渲染都新建 `createClient`
  会破坏 hooks 的共享缓存、每次重新拉取。
- 上传、SSE 与原始字节体未提供:本传输只读写 JSON 文本。
- 客户端不是会话。它从不为让被拒刷新呈现而清空 store;刷新令牌
  本身是会话层的事([@speed/auth-core](/zh-cn/docs/user-guide/modules/web/auth-core/))。
- 这里没有任何用户会读到的文本:错误 code 在消费方自己的目录里
  映射成双语文本。

## Source

- [api-client AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-client/AGENTS.md)——包的权威契约,包括 config hooks 的缓存契约。
