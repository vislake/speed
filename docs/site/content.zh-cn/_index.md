---
title: speed
---

# speed

speed 是一组以库形式分发的模块化单体：业务方通过 `go get` / `npm install`
接入的一系列独立发布、版本锁步的 Go module 和 npm 包，最终编译进同一个
二进制文件。

{{<button href="docs/quickstart/">}}快速开始{{</button>}}
{{<button href="docs/modules/">}}模块索引{{</button>}}

## 本站导航

- [快速开始](docs/quickstart/) —— 用 `saasctl new` 生成一个可运行的
  起始项目，以及 `saasctl` 的四个命令。
- [模块索引](docs/modules/) —— 业务方项目可以接入的 Go module 与
  npm 包的完整清单，每一项都链接到它自己的文档。
- [面向 AI Agent](docs/ai-agents/) —— 应该先读什么，以及当 AI 编码
  工具负责接入时最容易踩坑的架构纪律。
- [关于 speed](docs/about/) —— 这个项目是什么、各部分如何组合、
  文档如何分发。
- [实现状态](docs/status/) —— 当前实现真实进展到哪一步。

本站基于 [Hugo](https://gohugo.io) 与 [hugo-book](https://github.com/alex-shpak/hugo-book)
主题构建 —— 机制选型的理由与仍然推迟的部分见[关于](docs/about/)。
站点根部有一份机器可读的 [llms.txt](/llms.txt)，每一页的页眉都有语言切换
（English / 中文）。
