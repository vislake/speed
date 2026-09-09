---
title: "@speed/auth-core"
weight: 7
description: "浏览器会话生命周期——生成 authn 面上的无头、纯内存状态机:访问令牌在 store、刷新令牌在会话闭包、单飞静默刷新、代际守卫的竞态,以及只读的 React hooks。"
---

# @speed/auth-core

`@speed/auth-core` 把浏览器会话生命周期做成一台无头、纯内存的状态
机,跑在 [@speed/api-sdk](/zh-cn/docs/user-guide/modules/web/api-sdk/)
的生成 authn 面上。`createAuthSession(store)` 把生成的操作——密码与
SMS 登录、登出、切换租户、step-up、刷新,外加喂给注册与社交登录
流程的 SMS 发码、register 与社交操作——接成一个可观测的会话。
访问令牌住在调用方提供的 store 里;刷新令牌只住在会话闭包内部,
从不写往任何地方。主机用同一个 store、以
`refreshAccessToken: () => session.refresh()` 构建客户端,就免费得到
静默刷新:任何请求上过期令牌的 401 只跑一次刷新,重试的请求带着
新令牌。

没有 UI、没有 storage 写入,也刻意没有 `restore`:authn API 在签发
令牌的响应体里返回刷新令牌、不设刷新 cookie,所以会话闭包之外的
一切都不活过这一页——刷新页面就从匿名开始,用户重新登录。会话层
绝不让任何凭据进 `localStorage`。

## 何时使用

任何通过 authn API 让用户登录的主机都用它。React 主机还要把会话
绑到只读 hooks 上,并在其上组合组件族——登录组件族、账号页、租户
切换器与组装好的 product shell,都驱动这同一个会话契约。非 React
主机同样可以用会话:`subscribe`/`getSnapshot` 就是全部的可观测面,
操作是普通的 promise。

会话编码的规则是服务器自己的规则(见
[authn](/zh-cn/docs/user-guide/modules/identity/authn/)):authn API
把同一刷新令牌的并行呈现视为盗用、旋转整个令牌族——这正是为什么
会话自己串行化刷新,而不是让调用方去赛跑。

## 安装与接线

```ts
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createAuthSession, attachSession } from '@speed/auth-core'

const accessTokenStore = createMemoryAccessTokenStore()
const session = createAuthSession(accessTokenStore)

bindRequestFn(
  createClient({
    baseUrl: 'https://api.example.com',
    accessTokenStore,
    refreshAccessToken: () => session.refresh(),   // 静默刷新,见下
  }),
)

// 非 React 主机在 bootstrap 订阅一次:
session.subscribe((snapshot) => render(snapshot))

// React 主机在 bootstrap 把 hooks 绑到会话上一次:
attachSession(session)
```

组件里,`useAuthState()` 返回当前快照,`useCurrentTenant()` 返回
当前的 `{ tenantId }`——匿名时为 `null`——`usePermission('tenant',
'notes:write')` 返回布尔。三者都在每次会话变迁时重渲染。

## 核心 API 与使用要点

- **可观测的会话。** `getSnapshot()` 读取当前状态
  (`{ state: 'anonymous' | ..., principal }`);`subscribe(listener)`
  返回退订函数。用户操作(`loginWithPassword`、
  `loginWithSMSCode`、`completeSocialLogin`、`logout`、
  `switchTenant`、`verifyStepUp`)以新快照 resolve。会话前操作
  (`requestSMSCode`——无论手机号是否属于某账号都一律以 202
  resolve;`register`、`socialAuthorizeUrl`)成功也不改变任何状态——
  注册不是登录,创建出的用户交给主机自己的后续动作。
  `socialAuthorizeUrl` 是纯请求:会话从不导航。
- **失败契约。** 每个操作都以原始的 `ApiError` reject(用
  [@speed/api-client](/zh-cn/docs/user-guide/modules/web/api-client/)
  的 `isApiError` 区分),失败的操作不改变任何东西:store、持有的
  刷新令牌与快照,与尝试前分毫不差。当并发的兄弟操作(第二次登录、
  切换、step-up)在本请求在飞期间已提交,本请求自己的 2xx 就不再
  描述会话——它什么都不被应用,操作以 `OperationSupersededError`
  reject(用 `isOperationSuperseded` 区分),其 `snapshot` 字段携带
  胜者的状态。违反契约的签发令牌 2xx(缺令牌或缺 principal)在
  任何状态改变之前以 `client.protocol` reject。
- **`refresh()` 是静默路径。** 存下新的一对时 resolve `true`;无事
  可刷、服务器拒绝持有的令牌(会话已终结——它在本地登出,服务器
  早已终止该令牌族)、或刷新在飞期间一次租户切换胜出时,resolve
  `false`。传输失败或服务端错误把原始 `ApiError` 重新抛出,store
  与持有的令牌原封不动。携带同一刷新令牌的并发 `refresh()` 调用
  共享一个在飞请求(并行呈现服务端会视为盗用),切换或 step-up
  之后的调用仍携带同一持有令牌,所以同样共享这一次飞行。
- **代际守卫。** 完成的登出胜过在其后 resolve 的刷新;已提交的
  登录/切换/step-up 胜过陈旧的刷新:败者那一对的访问令牌与快照
  从不盖到胜者之上——只有一个例外,即旋转后的刷新令牌本身:当胜出
  操作保留了持有令牌时采纳它。结果如实反映谁赢了:step-up 保留了
  同一 principal,刷新 resolve `true`;租户切换换了 principal,刷新
  resolve `false`,而那个以 401 引发刷新的请求就此失败,不会在它
  从未请求过的租户下重放。
- **hooks 是只读的。** `attachSession` 绑定一个会话(后绑定者胜;
  前一会话的变迁不再到达 hooks)。attach 之前与登出之后,每个 hook
  都失败闭合:匿名快照、`null` 租户、每个权限都是 `false`。hooks
  从不驱动会话——登录与登出在事件处理器里调用,不在 effect 里。
- **权限检查只是集合查找。** 主机按域附加列表——
  `session.setPermissionSet('tenant', [...])` 与 `('system', [...])`,
  传 `null` 清除——`usePermission(domain, permission)` 回答"这个
  字符串在不在那个列表里"。这里没有任何抓取或求值,域列表缺席读
  作 `false`。principal 变更提交时会话应用存活规则:静默刷新与
  step-up 保留两个列表,租户切换丢弃租户列表、保留系统列表,登录
  或登出清空两者。这些检查是 UX 便利,绝不是安全边界——授权在
  服务端;登录或切换之后,重新抓取各域的列表。

## 边界与注意

- **跨页面加载不持久**——纯内存、没有 `restore`,是设计使然。
  刷新页面即匿名。
- **刷新令牌在内存里对 JavaScript 可见。** authn API 不设刷新
  cookie 来藏它,所以会话必须持有刷新端点从请求体里接收的那个
  令牌。这里刻意不存在 storage API。
- **绑定流程不在这个面上。** authn 回调端点对已认证调用者身兼二
  职、答以绑定结果,`completeSocialLogin` 刻意以 `client.protocol`
  拒绝那种绑定形状的响应:本会话的回调面是登录面。绑定是对调用者
  自己访问令牌的一次普通生成调用,绝不是会话操作。
- 切换租户与 step-up 不铸新刷新令牌——它们旋转调用者已有的那枚,
  依 authn spec;对 principal 没有 membership 的租户 `switchTenant`
  会被服务端拒绝,本地不改变任何东西。

## Source

- [auth-core AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/auth-core/AGENTS.md)——包的权威契约。
