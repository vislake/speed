---
title: 能力模块组
weight: 4
description: "依赖图顶层的产品面能力模块——billing、ai-gateway、sharing、compliance 与 admin——按设计皆可选用,按产品卖什么来挑,而非按每个二进制需要什么。"
bookCollapseSection: true
---

# 能力模块组

这五个 Go 模块坐在模块依赖图的最顶层。模块参考的 core 组是每个
speed 二进制都要组装的底座(`pkgcore`、`dbkit`、`tenancy`、
`observability`、`config`、`jobs`、`ratelimit`),services 组是产品
真正变成功能的能力面(`storage`、`notification`、`pki`、
`integration`、`metering`);本组则是产品要卖东西时才挑的:商务与按
用付费的门禁、对着厂商端点发起的 AI 调用、受控地公开分享内部资源、
对平台持有数据的治理、以及平台运营者自己的控制台。

五个模块没有一个必选。依赖图里它们之下的每个模块不带它们也能启
动;一个都不想要的宿主,直接从自己组合选出的组件集里省掉
即可。它们是按产品决策挑选的,而且挑在下面两组之上:每页的「怎么
接线」一节点名它要组合的 services 与 core 模块(billing 判定
`go/metering` 记录的用量,compliance 经 `go/sharing` 投递导出,
admin 则扇入其下所有模块)。

## 本组页面

| 页面 | 它给你什么 | 典型首次用途 |
|---|---|---|
| [billing](billing/) | Plan/Feature/Entitlement 领域模型、渠道无关的订阅与账单生命周期、信用账本,外加支付网关层 | 给产品收费:订阅、按用付费积分,以及「这个租户能不能用这个」的门禁 |
| [ai-gateway](ai-gateway/) | 厂商无关的对话与图像生成网关:Provider 注册表、BYOK 凭据、异步图像任务 | 经同一个门面调用 LLM 或图像厂商,租户自带密钥 |
| [sharing](sharing/) | 指向内部资源的公开分享链接,五条强制安全规则与全量访问日志 | 匿名、单次查看资源所有者选择分享的一个资源 |
| [compliance](compliance/) | 保留窗口清扫、被遗忘权编排、导出收集与投递、只读审计查询 | 让保留与擦除对你各模块已存的数据真正生效 |
| [admin](admin/) | 平台员工运营控制台:租户账本与停用、冒充、跨租户搜索、审计查询与导出、角色管理、用量仪表盘 | 在其它每个模块已提供的能力之上搭运营者界面 |

各页与其它组同形:*它做什么*(含模块刻意**不**做什么)、*何时选
用*、*怎么接线*、*核心概念与 API 面*、*已知限制与链接*。模块应答
的结构化错误码列在[错误码索引(English)](/docs/user-guide/error-codes/),
每个码一行。下面各组的模块页——[core 组](/zh-cn/docs/user-guide/modules/core/)与
[services 组](/zh-cn/docs/user-guide/modules/services/)——覆盖这五
个模块脚下的地板;[域指南](/zh-cn/docs/user-guide/domains/)覆盖产品
侧:[计量与账单](/zh-cn/docs/user-guide/domains/billing-metering/)与
[存储、分享与 AI](/zh-cn/docs/user-guide/domains/storage-sharing-and-ai/)
离本组最近。

平台的其它组在同一参考里与本组并立——身份(authn、rbac、org)、工
具(saasctl)与各 web 包。本组页面在组合需要的地方点名那些模块,但
只链接已经存在的组页(core 与 services);其余组页与它们同期落地。
