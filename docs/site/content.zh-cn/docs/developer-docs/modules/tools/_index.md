---
title: 工具组
weight: 6
description: "模块设计区最末的一组:saasctl——面向业务方的 CLI。它是唯一永不组装进运行内核的交付物,以及它为什么仍然随库发布。"
bookCollapseSection: true
---

# 工具组

本模块设计区里的其它各组,文档化的都是会被你组装进二进制的 Go
模块或 npm 包:它们实现 `the module contract`、注册进装配的 `ComponentRegistry`、
在装配里启动。本组文档化的是唯一一个都不做的交付物——
**saasctl**,面向业务方的 CLI。它不实现 `the module contract`,不带任何
表、迁移、HTTP fragment 或权限,运行中的产品永远不会从它这里服务
出任何路由。它根本不在模块依赖图里——不在任何东西之上,也不在任
何东西之下。

它的工作是项目边界。speed 交付的是库,而 saasctl 是塑造业务方实
际运行的那个应用的工具:生成起始项目(`new`)、把项目的 speed
require 重写到同一个锁步发布版本(`upgrade`)、在任何启动之前应用
所要求模块的 schema(`db migrate`)、预览一次启动会跑在什么配置上
(`config print`)。每个动作都发生在开发期,作用在文件与数据库文件
上,在启动之前与启动之外——从不进入运行中的内核。它的消费方是生
成项目,这正是参考应用——每个*库*模块的强制首消费者——刻意永不
接线它的原因;本组的设计页完整解释这层关系。

| 页面 | 给你什么 |
|---|---|
| [saasctl](/zh-cn/docs/developer-docs/modules/tools/saasctl/) | 为什么业务方故事以锁步发布里的 CLI 模块形态存在——即开即用的模板、原位 `go.mod` 改写、由 require 图驱动的迁移、启动配置的双生——以及为什么参考应用永不接线这个工具 |

## 相关阅读

- 用户指南从操作者一侧文档化同一个工具:[四个命令的用法](/zh-cn/docs/user-guide/modules/tools/saasctl/)
  与它所在的[工具组](/zh-cn/docs/user-guide/modules/tools/)。
- [仓库与发布](/zh-cn/docs/developer-docs/repo-and-release/)——锁步
  版本制,让 `upgrade` 成为一次单版本改写的语境,以及版本语法被本
  CLI 镜像的发布协调器。
- [总体架构](/zh-cn/docs/developer-docs/architecture/)——模块化单
  体、saasctl 立于其外的模块图,以及塑造本组设计页所讲两条消费方
  故事的强制首消费者规则。
