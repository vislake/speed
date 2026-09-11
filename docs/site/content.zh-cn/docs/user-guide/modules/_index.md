---
title: 模块参考
weight: 0
description: "用户指南的按模块读法——每个 Go 模块一页:做什么、何时选用、怎么接线、核心概念与边界。"
aliases: ["/docs/modules"]
bookCollapseSection: true
---

# 模块参考

speed 不是一个你直接运行的应用,而是你可以拉进自己产品、组装进
一个二进制的、独立发布的 Go 模块与 npm 包集合。[领域页](../domains/)
把一个产品需求从头走到尾;本栏是另一种读法:**一个模块一页**,按模块
依赖顺序排列,覆盖该模块做什么、何时选用、怎么接线、核心概念,以及
边界与坑。

## 本栏结构

页面按模块依赖方向分组——一组用到的东西都在它下面的组里,模块绝不
反向依赖:

- **core**——每个二进制与每个其他模块都踩在它上面的依赖底座:
  [pkgcore](./core/pkgcore/)、[dbkit](./core/dbkit/)、
  [tenancy](./core/tenancy/)、
  [observability](./core/observability/)、
  [config](./core/config/)、[jobs](./core/jobs/)、
  [ratelimit](./core/ratelimit/)。
- **services**——建立在 core 组之上的平台服务:`storage`、
  `notification`、`pki`。
- **identity**——你的用户是谁、能做什么:`authn`、`rbac`、`org`。
- **capabilities**——依赖图顶层的产品能力模块:`metering`、
  `billing`、`sharing`、`integration`、`ai-gateway`、`compliance`、
  `admin`。
- **app**——依赖图顶层的装配层:[app](./app/),唯一没有业务域的
  模块——装载器、七阶段组件驱动与固定中间件链,每个宿主都经它
  组装。
- **tools**——面向开发者的工具:`saasctl`。
- **web**——`@speed` npm 包(tokens、i18n、ui-kit、api-client、
  api-sdk、layout-kit、auth-core、auth-ui、tenancy-ui、
  product-shell、account-ui、billing-ui),这些模块的前端对应物。

每个组都有自己的导览页([core](./core/) 已在此;其余组同此形态),
每个模块页自足:页尾的 Source 小节链接该模块自己的
`AGENTS.md`——完整的 API 表、规则与错误索引都以它为准,本站只做
浓缩。

## 与领域页的关系

领域页——[身份与访问](../domains/identity-access/)是其中之一,一个
产品需求一页——回答「我的产品需要 X:涉及哪些模块、最少步骤、示例」;
模块页回答「我要集成模块 X:完整用法」。两者从相反方向指向同一批
事实;每个模块应答的错误码统一收录在[错误码索引](../error-codes/)。

还没构建过 speed 服务,先走[快速开始](/zh-cn/docs/user-guide/quickstart/);
[演练](../walkthrough-reference-app/)与
[操作](../operating/)页覆盖本栏分解之前的组装形态。
