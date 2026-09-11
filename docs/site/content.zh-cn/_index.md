---
title: speed
---

# speed

以库分发的模块化单体:独立发布的 Go 模块与 npm 包,业务项目通过
`go get` / `npm install` 拉入并编译进一个二进制。挑你的 SaaS 需要的
模块——身份、多租户、通知、计费、AI、合规——通过一次装配把它们
组合起来,而不是自己从零搭建每一层。

本站有两个主要栏目。

## 用 speed 搭建你的 SaaS

给**使用** speed 模块做自己产品的团队:快速开始、按领域的教程,以及
带可运行示例的逐模块完整参考。

- [用户指南](/zh-cn/docs/user-guide/)——从这里开始。
- [快速开始](/zh-cn/docs/user-guide/quickstart/)——五分钟生成一个启动项目。
- [错误码索引](/zh-cn/docs/user-guide/error-codes/)——speed 系 API
  可能应答的全部错误码清单。

## 开发 speed 本身

给**在 speed 上工作的开发者**:总体架构、背后的设计原则,以及解释
每个模块为什么长成这样的逐模块设计深入。

- [开发者文档](/zh-cn/docs/developer-docs/)——从这里开始;本栏涵盖
  总体架构(模块化单体、模块依赖方向、部署模式与实现组装两条正交轴)、
  设计原则与逐模块设计深入。

## 面向 AI Agent

作为编码 agent 阅读本站?从
[面向 AI Agent](/zh-cn/docs/ai-agents/)开始了解先读什么;本站根部的
[/llms.txt](/llms.txt) 提供全部页面的机器可读索引。仓库自己的
[根 `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)
仍是仓库工作的权威向导。
