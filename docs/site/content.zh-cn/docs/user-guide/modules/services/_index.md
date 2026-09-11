---
title: 平台服务
description: "能力面组:storage、notification、pki、integration 与 metering——消费者在产品要存媒体、发消息、管密钥、开 API 或计量用量时直接接线的五个 Go 模块。"
weight: 2
bookCollapseSection: true
---

# 平台服务

这五个 Go 模块是平台的高层能力面。模块参考的 core 组提供组装契
约与基础设施地板(`pkgcore`、`dbkit`、`tenancy`、`observability`、
`config`、`jobs`、`ratelimit`)——消费方项目在其**之上**构建,很少
把它**当产品卖**;services 组才是产品真正变成功能的部分:媒体对象、
外发消息、签名密钥与证书、租户对外的 API、用量计量。每个模块都带
真实的数据表与双语迁移、挂载在 `/api/v1/*` 的 OpenAPI 片段,以及声
明的权限、审计动作、事件与 job handler;参考应用是每一个模块的强制
首位消费者。

这里的页面是面向消费团队的用法指南——每个模块做什么、何时选用、
怎么接线、核心概念,以及诚实的局限。它们不是各模块自己
`AGENTS.md` 的替代品,后者始终是权威、最新的文档;每页在 *Source*(
出处)小节链接它。

本组按依赖排序,正如左侧导航所示:`storage`、`notification` 与
`pki` 直接坐在 core 组 `jobs`/`tenancy` 层之上;`integration` 与
`metering` 建在其上。五个模块都期望下层 core 组可用——一个已解析
基础设施接缝的 `Registry`、一个供异步工作使用的 `jobs.Queue`——
所以如果你从零组装宿主,先读 core 组的页面,再读
[快速开始](/zh-cn/docs/user-guide/quickstart/),它生成的起步项目已经把地板
的大部分接好了。

## 本组页面

| 页面 | 它给你什么 | 典型首次用途 |
|---|---|---|
| [storage](storage/) | 媒体对象元数据在数据库、字节在你的对象存储,带服务端复验的三步上传协议 | 用户上传、需要净化后回传的图片/媒体 |
| [notification](notification/) | 外发消息:应用内收件箱、邮件与短信,外部收件人需经同意验证 | 你产品发出的每条消息,走在收件人选定的通道上 |
| [pki](pki/) | 有生命周期的密钥材料:Ed25519 签名密钥与内部 CA,带轮换、撤销与 CRL | 用会轮换的密钥签 token、给内容做证明 |
| [integration](integration/) | 租户对外的 API:API key、三层限流、带签名的外发 webhook | 让伙伴与脚本走机器通道;把事件投给客户 |
| [metering](metering/) | 两级可靠性的用量记录,聚合进按租户的用量汇总 | 计量你产品量到的东西——今天做分析,将来做配额 |

每页结构相同:*它做什么*(含模块刻意**不**做什么)、*何时选用*、
*怎么接线*、*核心概念与 API 面*、*已知限制与链接*。模块应答的结构
化错误码列在[错误码索引](../../error-codes/),每个码一行;页面链接
到各自模块的行。更高层的域指南——[任务与通知](../../domains/jobs-and-notifications/)、
[存储、分享与 AI](../../domains/storage-sharing-and-ai/)、
[计量与账单](../../domains/billing-metering/)——从产品侧覆盖这些
模块;这些页面从接线侧覆盖它们。其余平台模块(身份、租户与组织、
商务与合规)在本参考的其它组里;消费这些模块生成 API 的前端 npm
包列在[模块索引](../../../modules/)。
