---
title: "@speed/api-sdk:API 契约的生成半面"
weight: 6
description: "为什么前端类型面由 orval 从合并文档生成:唯一手写传输绑定 bindRequestFn、不进 lockfile 的固定版本生成器、无凭据刷新 mutator、平台合并成员资格,以及为什么生成代码不带租户概念、不带 i18n 资源。"
---

# @speed/api-sdk:API 契约的生成半面

speed API 契约的后端半面是参与编译的生成服务端代码;本包是同一条纪律
的前端半面。操作函数、TanStack Query 钩子与响应模型都是 orval 从合并
OpenAPI 文档生成的输出,它们发出的每一个 HTTP 调用都经由
`@speed/api-sdk` 的传输绑定路由到 `@speed/api-client`,这里除一个绑定文
件外没有任何手写。本页解释全生成面与那条绑定的设计;再生成机制在
[API 契约](/zh-cn/docs/developer-docs/api-contract/)页,怎么消费这个包
看[用户指南的 api-sdk 使用页](/zh-cn/docs/user-guide/modules/web/api-sdk/)。

## 职责与边界

- **`src/` 除一个文件外全是 orval 输出。** `src/index.ts` 整体生成,
  盖着携带固定 orval 版本的 DO-NOT-EDIT 头;`src/runtime.ts` 是本包
  唯一手写源文件,以 `./runtime` 子路径导出。
- **零手写 HTTP**——`speed/no-direct-http` 规则同样作用于生成文件。
  生成的函数调用一个 mutator(`speedRequest`),把它们的 axios 形状
  调用适配到宿主的请求函数上。
- **无租户概念。** 任何地方都没有租户头——租户上下文在访问令牌内部
  ——生成的 query key 是裸 spec 路径字符串;租户 query-key 命名空间
  是消费壳的纪律,永远不是包代码。
- **无 i18n 资源。** 错误是 spec 的类型化 `{code, params}` 信封;码由
  消费包自己的目录解析为双语文本。
- **React 宿主才有 peer**:`react` 与 `@tanstack/react-query` v5 是
  peer,因为宿主共享一个 `QueryClient`——本包从不创建它,钩子离开宿
  主的 provider 树就没有用处。

## 设计:为什么除一个文件外全是生成物

手写 API 调用天生就是漂移——每一行都可能与 spec 悄悄不一致,而除了
集成测试没有任何东西会发现。生成代码无法漂移:spec 是唯一来源,而生
成面参与其消费方的类型系统。`@speed/auth-core`——authn 组的第一个工
作区内编译消费者——对着这些操作做类型检查,所以一次 spec 改动若让重
生成的表面超出会话层的调用,那个包的类型检查就失败——编译强制闭环的
前端半面。

DO-NOT-EDIT 头是机制的一部分,不是装饰:它携带固定的 orval 版本,于
是生成器与已提交产物之间的工具漂移在头部本身显形为 diff。同样的推理
禁止在 `src/` 里加第二个手写文件来补生成缺口——工具问题归工具解决
(见下面的 fixup),永远不归已交付源码。

## 设计:为什么传输绑定是一个文件、一次绑定

生成代码不能知道宿主如何接它的 HTTP 传输——base URL、令牌存储、刷
新、重试、超时、reporter 全是宿主 `createClient` 配置的事。
`runtime.ts` 就是那条边界:`bindRequestFn` 由宿主在 bootstrap 时调用
一次(后绑生效),装上每个生成调用骑乘的请求函数;`speedRequest`
mutator 把 orval 的 axios 形状选项对象映射到 `RequestFn` 契约上。
App 自有 SDK(`task api:gen:app` 为参考应用自有片段生成的输出)
re-export 同一子路径,所以 bootstrap 的一次 `bindRequestFn` 调用同时
服务两个生成面。

存在一个按操作覆盖:authn 会话刷新操作对着第二个 mutator
`speedRequestCredentialless` 生成,它把请求声明为无凭据
(`omitAccessToken`)——刷新用请求体里的刷新令牌认证,它的 401 必须
在 api-client 只认带凭据的刷新规则下保持终局,而不是重入刷新路径。

## 设计:为什么生成器是固定的,而不是被依赖的

orval 固定在 8.17.0,从不进入工作区 lockfile:按需经 `pnpm dlx` 拉
取,每个运行者——Taskfile 的 `api:gen` 任务与 `api-contract.yml` 工
作流——用同一条命令,两者无法漂移。版本提升必须同落
`orval.config.ts` 的注释、Taskfile 与工作流三处。fixup 脚本为一个真
实的缺口而存在:orval 发射不带文件扩展名的 mutator 导入,TypeScript
在 bundler 解析下接受、在 nodenext 下拒绝(TS2835),而本包的构建正
跑在 nodenext 上。`web/scripts/orval-nodenext-fixup.mjs` 在每次再生
成后把这些导入确定性重写为显式 `./runtime.js` 形式,并在 orval 的发
射变化时以非零退出——于是生成器漂移在 CI 失败,而不是交付一个无法
构建的包。

## 设计:合并文档覆盖什么

输入是 `contracts/speed.yaml`——固定版本 redocly `join` 十一个平台模
块片段(admin、ai-gateway、authn、billing、config、integration、
notification、org、pki、sharing、storage)的产物。成员资格由模块驱
动:每个带 HTTP 片段的平台模块都是成员,而参考应用自己的片段(notes、
cases、smilesim)刻意不是——它们是应用自己的 API,由 app 自有生成
腿生成进应用 web 宿主导入的 app 自有 SDK(`src/app-api`)。因此平台片
段只有进入合并才能到达本包;每个模块的组带同样的形状——react-query
钩子、纯函数、类型与错误信封——错误按模块类型化。

## 对外稳定面

冻结的是绑定与过程,不是文件清单:`./runtime` 子路径
(`bindRequestFn`、`speedRequest` mutator、
`speedRequestCredentialless` 覆盖)手写且稳定;DO-NOT-EDIT 边界是一
份契约——`src/index.ts` 就是 orval 8.17.0 从合并文档生成出的样子,
任何直接编辑都是下一次再生成抹掉的违约;peer 家族(react +
react-query v5)是宿主组合所依据的共享 QueryClient 契约。

## Source

- 包契约:[AGENTS.md](https://github.com/vislake/speed/blob/main/web/packages/api-sdk/AGENTS.md)
- 生成器接线:[web/orval.config.ts](https://github.com/vislake/speed/blob/main/web/orval.config.ts)、[web/scripts/orval-nodenext-fixup.mjs](https://github.com/vislake/speed/blob/main/web/scripts/orval-nodenext-fixup.mjs)

## 相关页

- [API 契约](/zh-cn/docs/developer-docs/api-contract/)——本包是其前端半面的 spec-first 纪律、生成腿与 porcelain 门
- [前端架构](/zh-cn/docs/developer-docs/frontend-architecture/)——生成包在分层中的位置
- 怎么用:[用户指南的 api-sdk 页](/zh-cn/docs/user-guide/modules/web/api-sdk/)
- web HTTP 组其余设计页:[api-client](/zh-cn/docs/developer-docs/modules/web/api-client/)——绑定背后的传输;[auth-core](/zh-cn/docs/developer-docs/modules/web/auth-core/)——生成 authn 面的第一个工作区内编译消费者
