---
title: 关于
weight: 5
---

# 关于 speed

speed 是一个多租户 SaaS 产品家族的共享基础设施。它**不是一个应用
程序**：它是一组版本锁步、独立发布的 Go module 与 npm 包，由业务方
项目接入后编译成单一的二进制文件。各模块在进程内互相调用——没有
服务发现，也没有 Kubernetes 那一套基础设施。

## 整体形态

| 方面 | 设计 |
|---|---|
| 依赖关系 | 严格自底向上：基础模块（`pkgcore`、`dbkit`、`observability`、`ratelimit`、`tenancy`）支撑着上层的一切。 |
| 部署模式与实现组合 | 两条正交的轴。每一个基础设施依赖都是一个接口，可以有多种实现（一种进程内实现，加上 PostgreSQL、Redis、S3 等外部实现）。**部署模式**并不选择实现，它只约束实现：每个实现声明自己的能力，当某种组合无法在声明的模式下运行时，装配会失败。单进程部署可以连接真实的外部服务。业务代码从不对模式分支判断。 |
| 多租户 | 共享数据库配合 `tenant_id` 隔离，由 GORM 插件、强制使用的泛型 repository 基类，以及分布式模式下的 PostgreSQL 行级安全三重把关。 |
| 版本管理 | 锁步版本：所有模块与包共享同一个版本号、一起发布；只支持相同版本号的组合。 |

## 文档如何分发

每个模块都在自己内部带一份 `AGENTS.md`——面向 AI 编码工具的速览：
职责边界、公开 API、典型用法，以及明确的禁止事项——再加一份普通的
`README.md`，让文档随代码一起分发，并且始终与业务方实际拉取的版本
保持一致。（每模块一份的 `docs/usage.md` 是
[docs/internal/13](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md)
记录的长期计划；截至本文写作，恰好只有一个模块 `go/notification`
建了这样一份——
[go/notification/docs/usage.md](https://github.com/vislake/speed/blob/main/go/notification/docs/usage.md)，
其自己的文件头就说明这是写给其他模块参考的模板——其余每一个 Go
module 和每一个 npm 包目前都还只靠自己的 `AGENTS.md` 加
`README.md`。）本站是跨模块的*中心*参考——完整索引见
[模块索引](../modules/)——计划按发布版本分目录，模块自己的文档会
指向这里获取总览。

## 本站现状与计划的对照

**机制决策已经落定**：本站基于 [Hugo](https://gohugo.io) 和
[hugo-book](https://github.com/alex-shpak/hugo-book) 主题构建，把
[docs/internal/13-documentation-standards.md](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md)
刻意推迟到 M4 的静态站点生成器决策提前落地——这与本仓库
`storage`/`notification` 两个模块此前"提前排期落地"的先例是同一种
模式。在候选主题之间选择 hugo-book 而不是 Docsy 的原因：Docsy 需要
Hugo Modules 加一整套 Node/PostCSS 资源构建流水线（按其现行的安装
文档，它的 Bootstrap 与 Font Awesome 资源来自 npm，且需要 PATH 上
存在一个 Dart Sass 编译器），这会重新引入本目录此前一直刻意回避的
Node 依赖，而 hugo-book 没有这个依赖——它现行的版本（v0.15.0）甚至
去掉了更早版本里的 Sass 依赖——只需要 Hugo 这一个二进制本身。中英
双语内容靠的是 **Hugo 自身的多语言机制**，不是某个主题的功能：两个
候选主题本可以同样好地满足 i18n 这条需求，因为无论是 Docsy 的
Bootstrap 外观还是 hugo-book 朴素的外观，底下都是同一套 Hugo 核心的
语言机制。

现在真实存在的：`content.en/` 与 `content.zh-cn/` 下两种语言各自
真实的内容（Hugo 按内容目录分语言的约定，与这个主题自己文档化的结构
一致）、每一页页眉里真正可用的语言切换器、`hugo --minify` 在
`public/` 下产出的站点（已加入 `.gitignore`，从不提交）,以及站点
构建产物根部一份真实的 `llms.txt`，其中的链接已经更新为这份 Hugo
配置实际产出的 URL。让构建产物保持诚实的结构检查是
`tools/check_docs_site.py`（docs-check 流水线）——它现在会先构建站点，
再对照 `public/` 检查同样的属性（必需页面存在、内部链接可解析、
站点能被访问），而不是对照手写的 HTML 源码树。

推迟到更晚里程碑（M4）的：按发布版本分目录（本站像它记录的代码一样
按版本管理），以及完整的文档集合（错误码索引、站点上呈现的 ADR、由
配置 schema 生成的配置参考）。
