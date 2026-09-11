---
title: web 组包
weight: 7
description: "speed 产品的前端面——十二个 @speed npm 包按层排布:tokens、i18n、ui-kit、layout-kit 打底,api-client 与 api-sdk 负责 HTTP,auth-core/auth-ui/account-ui/tenancy-ui 组成会话族,product-shell 负责组装,billing-ui 负责账单读取。"
bookCollapseSection: true
---

# web 组包

这十二个 `@speed/*` npm 包是 speed 产品的前端半边。和 Go 模块一样,
它们是你组装进自己应用里的库——没有一个是拿来直接运行的应用——
并且走同一套 lockstep 版本:整个平台只有一个版本号,Go 模块与 npm
包一起发布。本参考文的 Go 侧页面(见[模块参考](/zh-cn/docs/user-guide/modules/))
讲后端;这一组讲所有在浏览器里渲染的东西。

各包分层排布,层序就是依赖序:

- **基础层**——`@speed/tokens`(设计令牌树,无依赖的纯数据)、
  `@speed/i18n`(包了 react-i18next 的 i18n 层,缺键绝不回退到别的
  语言)、`@speed/ui-kit`(把令牌树映射成 MUI v9 主题,外加七个受控
  组件)、`@speed/layout-kit`(共享应用骨架:`AppShell` 与
  `RouteGuard`)。这四个包在这组里各有专页:
  [tokens](./tokens/)、[i18n](./i18n/)、[ui-kit](./ui-kit/)、
  [layout-kit](./layout-kit/)。
- **唯一 HTTP 客户端**——`@speed/api-client` 是手写 HTTP 的唯一归
  宿:可注入的 `fetch`、只存内存的令牌库、静默单飞 401 刷新、保守的
  瞬时重试,一切失败归一成携带 API 封套 code 的 `ApiError`。
  `@speed/api-sdk` 是合并 API 文档的生成式类型面,经唯一的
  `bindRequestFn` 绑定点走同一个客户端。两者都不带 i18n 资源,也没有
  任何租户头——租户上下文住在访问令牌里。
- **会话与身份层**——`@speed/auth-core` 是无头会话状态机;
  `@speed/auth-ui` 渲染登录组件族;`@speed/account-ui` 渲染登录后
  的账户页;`@speed/tenancy-ui` 是租户切换器。
- **组装与业务层**——`@speed/product-shell` 把骨架、登录组件族与
  会话 hooks 组装成三分支视图机(未登录、已登录、会话失效);
  `@speed/billing-ui` 在生成的 billing 操作之上渲染账单文档读取面。

这组里每个包都有专页,讲它做什么、何时选用、怎么接线、核心 API 与
边界。

## 每个包都守的两条纪律

- **HTTP 只发生在一处。**`api-client` 是唯一自己发 HTTP 的包;生成
  的 SDK 经它的 `RequestFn` 接口转发,组件包完全不碰网络——一切交
  互要么经回调上报,要么是会话操作。workspace 的
  `speed/no-direct-http` ESLint 规则把这条钉死:任何其它包 `src`
  里的裸 `fetch`/`XMLHttpRequest`/`axios` 调用都是错误。
- **用户可见文案双语且绝不内联。**凡渲染文本的包都自带一份
  `zh-CN` 与一份 `en-US` 资源,键集完全一致,挂在各自命名空间下经
  `@speed/i18n` 注册——注册在变更前校验键集对等;缺键渲染成键本
  身,绝不是另一种语言的文字。workspace 的 `speed/no-literal-text`
  ESLint 规则拒绝包 `src` 里的内联文本。这是后端「新文案必须双语
  齐发」规则的前端镜像。

同一条纪律让各包不沾产品语义:没有组件会取数或存数,没有包会替宿
主决定租户,会话层以下的包根本不知道访问令牌是什么。

## 与其他指南的关系

[构建前端](/zh-cn/docs/user-guide/domains/frontend-building/)这张领
域页从产品侧讲同一件事——有哪些层、组装宿主要走的四步。identity
组的 Go 模块是这些包经 `api-sdk` 对话的后端;参考应用的 web 宿主
(`examples/reference-app/web`)是把它们全部组装起来的强制首个消费
者,由应用自己在 `APP_WEB_DIST` 下伺服。
