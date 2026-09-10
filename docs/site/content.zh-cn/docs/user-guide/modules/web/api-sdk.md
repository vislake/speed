---
title: "@speed/api-sdk"
weight: 6
description: "合并 API 文档的生成类型面——orval 产物经唯一手写接缝(bindRequestFn)绑到主机的客户端,react-query hooks 跑在共享的 QueryClient 上,无租户概念、无 i18n 资源。"
---

# @speed/api-sdk

`speed` 前端规范先行纪律的一半,在 `@speed/api-sdk` 里:操作函数、
TanStack Query hooks 与响应模型,都由 orval——固定在 8.17.0,经
`pnpm dlx` 运行,所以 orval 从不进入 workspace 的 lockfile——从
`contracts/speed.yaml` 生成;这份合并文档由固定版本的 redocly
`join` 十一个平台模块 fragment(admin、ai-gateway、authn、billing、
config、integration、notification、org、pki、sharing、storage)而来。
`src/` 里除一个文件外全是生成器产物,盖着带固定 orval 版本的
DO-NOT-EDIT 头:生成器与已提交产物之间的工具漂移,会以头部本身的
diff 显现。

它不是什么:它自己不执行任何 HTTP。每个生成调用都经过本包唯一的
手写接缝——`src/runtime.ts`,以 `@speed/api-sdk/runtime` 子路径导出——
它把 orval 的 axios 形状调用适配到你主机在 bootstrap 时绑定过一次的
`@speed/api-client` 请求函数上。

## 何时使用

任何调用平台 API 操作的前端都用本包。生成的 hooks 与函数是唯一
被认可的路径——凡是 spec fragment 覆盖的东西,绝不手写调用
([@speed/api-client](/zh-cn/docs/user-guide/modules/web/api-client/)
的 `speed/no-direct-http` 规则强制执行这一点)。本包也是 workspace
内消费方的编译地板:[@speed/auth-core](/zh-cn/docs/user-guide/modules/web/auth-core/)
通过这个接缝驱动生成的 authn 会话面;账号组件族把生成的 hooks 渲染
进组件树。

你自己应用的 API 不在这里。参考应用的 notes、cases 与 smilesim
操作刻意不是 merge 成员;它们生成进应用自己的 app-owned SDK
(`examples/reference-app/web/src/app-api`,`task api:gen:app`
这一腿),由应用 web host 为自己的界面引入;平台操作继续乘本包。
交付的业务方遵循同一形状:平台操作走 `@speed/api-sdk`,你自己的
操作走你自己的 app-owned SDK,两者都走下面的同一个绑定。

## 安装与接线

peer 是 `react` 与 `@tanstack/react-query` v5:主机共享同一个
`QueryClient`/`QueryClientProvider`——本包从不自己创建。在
bootstrap 绑定一次你的客户端:

```ts
import { bindRequestFn } from '@speed/api-sdk/runtime'
import { createClient, createMemoryAccessTokenStore } from '@speed/api-client'

bindRequestFn(
  createClient({
    baseUrl: '/api/v1',
    accessTokenStore: createMemoryAccessTokenStore(),
    refreshAccessToken: () => session.refresh(),
  }),
)
```

`bindRequestFn` 后绑定者胜。runtime 还导出 `speedRequest`(生成代码
import 的 mutator)与 `speedRequestCredentialless`——把 authn 会话
刷新请求声明为无凭据(`omitAccessToken`)的逐操作覆盖,因此它不带
`Authorization` 头,它的 401 在客户端"仅限 bearer"的刷新规则下保持
终结,而不是再次进入刷新路径。app-owned SDK 自己的接缝 re-export
这个子路径,所以一次 `bindRequestFn` 调用同时服务两个生成面。

再生成在 `web/` 下依次运行 `pnpm dlx orval@8.17.0 --config
orval.config.ts` 与 `node scripts/orval-nodenext-fixup.mjs`;
Taskfile 的 `api:gen` 任务与 `api-contract.yml` workflow 跑的正是这
一对,每次再生成后都跟一道 porcelain 一致性门。绝不手改
`src/index.ts`,也绝不在 `src/` 里加另一个手写文件去补生成缺口:
工具问题在工具里修(nodenext fixup 脚本把 orval 无扩展名的 mutator
import 重写成 nodenext 构建要求的显式 `.js` 形式——它存在,正是为
了不需要任何桥接文件)。

## 核心 API 与使用要点

每个平台 fragment 都导出自己模块的一组东西——hooks、普通函数、
请求/响应类型与按模块的错误 envelope:

- **authn 组**覆盖会话生命周期——`useAuthnLoginWithPassword`、
  `useAuthnLoginWithSMSCode`、`useAuthnRefreshToken`、
  `useAuthnLogout`、`useAuthnSwitchTenant`、`useAuthnVerifyStepUp`,
  以及注册、发码、`/me`、会话列举与吊销、MFA 与社交登录。它存在
  是因为 [@speed/auth-core](/zh-cn/docs/user-guide/modules/web/auth-core/)
  消费它:spec 一变、生成面超出 auth-core 的调用,那个包的类型检查
  就会失败。
- **其余模块同形**——notification、billing、org、storage、sharing、
  pki、admin、integration 与 ai-gateway 各导出自己模块的 hooks、
  普通函数、类型与错误 envelope。查询与变更都是真正的 react-query
  hooks,返回标准的结果形状。
- **错误 envelope 按模块**——每个 API 都有 `{code, params}` 类型;
  code 在消费方自己的目录里解析成双语用户可见文本——这里不随包
  发布 i18n 资源。
- **生成代码里没有租户概念**:没有 tenant 头(租户上下文在访问
  令牌内部旅行),查询键是裸的 spec 路径。把租户范围的查询套进你
  自己的命名空间键,是消费方外壳的纪律——凡缓存不能活过租户切换
  的地方都要这么做。

## 边界与注意

- 传输关切——base URL、令牌存储、刷新、重试、超时、reporter——
  完全是主机 `createClient` 的配置;生成代码一样都不带。需要不同
  的传输行为?改客户端,绝不动生成面。
- 本包从不创建 `QueryClient`;hooks 需要你的 provider,缓存失效
  通过 react-query 在生成代码暴露的键上做。
- 再生成会整目录覆写 `src/`,后端一半在同一条流程里再生成:先改
  spec(`api/openapi.yaml` → `task api:gen`),两半一起提交,遵守
  API-contract 的顺序。
- 原始字节体、上传与 SSE 不经过这个面——底下的客户端只读写
  JSON 文本。

## Source

- [api-sdk AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md)——包的权威契约。
