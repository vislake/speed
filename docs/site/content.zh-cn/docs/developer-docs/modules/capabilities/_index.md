---
title: 能力模块组设计
weight: 4
description: "依赖图顶层五个能力模块——billing、ai-gateway、sharing、compliance、admin——的设计页:每个模块为什么长成这样,以及贯穿全组的共同设计主线。"
bookCollapseSection: true
---

# 能力模块组设计

这五页设计文档是[用户指南"能力模块组"](/zh-cn/docs/user-guide/modules/capabilities/)在开发者文档一侧的镜像:用户指南的页面讲每个模块做什么、宿主怎么接线;这些页面讲它为什么长成这样——边界、被否决的方案,以及承载设计的机制。

五个模块位于模块依赖图的最顶层。它下面的每个模块是每个二进制都要组装的底座或能力面;这一组则是产品**卖**什么东西时才挑选的。五个都不是必需,各回答一个不同的产品问题:

- **billing**——商务领域:`Plan`/`Feature`/`Entitlements`、渠道无关的 `Subscription`/`Invoice` 生命周期,以及按用付费的信用账本。设计问题:怎样让"向用户收费"可以安全地构建——数据库仲裁的余额、幂等的结算,以及被严格挡在接缝之后的支付渠道。
- **ai-gateway**——通往各家 LLM 与图像生成端点的一扇供应商无关的门。设计问题:如何在抽象所有供应商的同时不让抽象僵化,以及网关与为它付费的业务逻辑之间的界线画在哪里。
- **sharing**——指向内部资源的受控公开链接。设计问题:一个未经认证的访客可以被允许做什么——几乎什么都不能,五条强制规则全部落在代码里。
- **compliance**——保留期清扫、被遗忘权、数据导出与审计检索。设计问题:一个治理层如何在**不拥有任何表、不发明任何机制**的前提下,跨其它模块的表删除与投递数据。
- **admin**——平台运营后台。设计问题:最顶层的模块如何在它之下的每个模块之间渲染、检索与操作,而不变成第二套身份体系或数据库层的"上帝视图"。

合起来读,五页贯穿一条连续的设计主线。前两个——billing 与 ai-gateway——坐在同一依赖层,共享本组的核心纪律:同层模块永不互相 import,它们的连接是结构类型化的接缝(`billing` 的 `Entitlements.Check` 判定与 `metering` 的用量记录以镜像接口的形状进入 ai-gateway 的调用路径,而非以 import 的形式——两页各自解释自己为什么长成能互相咬合的样子)。compliance 直接坐在它们之上,**可以** import 它要编排的对象——包括为导出投递真实 import `go/sharing`,与下方同层接缝纪律形成刻意对比。admin 坐在最顶端,是本组唯一被认可的例外:它直接 import 其下每个模块的具体包,因为它是所有这些模块的运营操作面。页面之间也互相引用:sharing 的过期策略服务 compliance 的导出投递;compliance 的 `AuditQuery` 服务 admin 的审计壳;admin 写下的双身份审计记录,正是 compliance 的查询维度读回来的东西。

按模块依赖序展开的设计故事,在本节其余组(core、services、identity、tools)继续;[开发者文档](/zh-cn/docs/developer-docs/)hub 是永远最新的地图。[总体架构](/zh-cn/docs/developer-docs/architecture/)页是本组的地基——五页假设的模块接线契约、数据分域与部署轴线。每页设计文档都带 Source 一节,链回该模块自己的 `AGENTS.md`,每条论断都可对照原文核实。

## 页面

- [billing](/zh-cn/docs/developer-docs/modules/capabilities/billing/)——商务领域模型、信用账本的预扣/确认/退还与单语句仲裁、支付网关接缝。
- [ai-gateway](/zh-cn/docs/developer-docs/modules/capabilities/ai-gateway/)——供应商无关的 chat 与图像 provider、纯异步的图像管道、以及与 billing 同层而不 import 的结构接缝。
- [sharing](/zh-cn/docs/developer-docs/modules/capabilities/sharing/)——公开分享链接的五条强制规则、从令牌解析租户、投递成功才计次的浏览统计。
- [compliance](/zh-cn/docs/developer-docs/modules/capabilities/compliance/)——在参与者之上编排、为何不拥有任何表、每个删除与审计机制实际住在哪里。
- [admin](/zh-cn/docs/developer-docs/modules/capabilities/admin/)——例外形状的顶层模块:直接 import、租户台账、作为授权凭据的模拟登录、经审计的跨租户读取。

## Source

- 模块纪律:[go/billing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md)、[go/ai-gateway/AGENTS.md](https://github.com/vislake/speed/blob/main/go/ai-gateway/AGENTS.md)、[go/sharing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md)、[go/compliance/AGENTS.md](https://github.com/vislake/speed/blob/main/go/compliance/AGENTS.md)、[go/admin/AGENTS.md](https://github.com/vislake/speed/blob/main/go/admin/AGENTS.md)
