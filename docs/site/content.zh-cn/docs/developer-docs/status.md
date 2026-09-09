---
title: 实现状态
weight: 8
description: "实现当下真实走到哪:一份粗粒度快照,仓库根 CLAUDE.md 的 Repository Status 一节才是权威、始终最新的说法。"
aliases: ["/docs/status"]
---

# 实现状态

什么是真实存在的,权威且始终最新的说法在仓库根
[CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md) 的
*Repository Status* 一节,本页刻意不重复它——逐模块的重复统计只
会在下一个模块轮次落地的那一刻就开始过期。下面是本页最后一次写
作时对照真实仓库核实的粗粒度快照;把它当作方向性参考,而不是
信息源。

本页属于开发者文档栏;它是本站原先顶层状态页的继任者,旧页目前
仍留在 `/docs/status/`,本页为该地址带了 alias。原页的 CI 流水线
表格如今活在[贡献指南](/zh-cn/docs/developer-docs/contributing/)
里,那里说明各条流水线何时跑什么。

> [!NOTE]
> **里程碑编号已经不再描述当前状态。** 路线图的里程碑
> ([docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md))
> 仍然记录着每个模块最初被排在哪一轮,但好几个模块明显早于那个
> 窗口交付——读路线图看的是排期,不是实现走到哪。

## 今天真实存在的

| 领域 | 状态 |
|---|---|
| Go module | 根 `go.work` 里的每个模块都有真实、经过测试的实现——构建、vet、lint 与开启 race 的单元套件全部通过——每个模块在 `fast-check.yml` 有自己的 CI 矩阵行,`full-check.yml` 里还有基于 Docker、对着真实 PostgreSQL、Redis 与 RustFS 的集成层。 |
| Web 包 | `web/` workspace 下的 `@speed/*` 包,加上 reference app 的 web host 作为未纳入版本管理的外部成员,都已实现并测试;lint、严格 typecheck、测试与构建逐包在 CI 里干净通过。 |
| API 契约 | spec 先行的闭环覆盖平台模块的 OpenAPI 片段:`api-contract.yml` 从合并文档重新生成每个后端接口与前端 SDK,对提交产物做一致性闸门并重建 reference app——没有对应实现的 spec 变更无法通过编译。reference app 自己的片段走它独立的 app 自有生成流。 |
| 部署模式 | standalone 是默认,也是日常开发模式。分布式部署是被真实证明的,不只是进程内组合:集成测试会启动两个真实副本,跑在真实的 Redis、RustFS 与 SMTP 之上;每日的 scaffold-verify 任务还会在两种模式下各生成并启动一个真实项目。 |
| 数据库方言 | SQLite 在每个 pull request 上跑;PostgreSQL 在打了 `full-ci` 标签的 pull request 与每一次推送到 `main` 上跑,经由集成层。 |

## 还没做完的

- 驱动服务器所服务页面的浏览器自动化——`e2e` 流水线仍是刻意
  门控的 stub。
- 第一个带真实发布的正式 tag:目前没有任何东西发布到任何
  registry;路线图的发布里程碑覆盖此事。
- `nightly` 流水线的回归任务(flaky 测试检测、基准对比)仍是
  门控的 stub。

## 自己核实权威状态

仓库根 CLAUDE.md 的 *Repository Status* 一节逐模块说明今天在 CI
里真正跑通并通过的是什么——它本身是写来被*核实*而非被轻信的:
依赖某个断言前,对照工作流文件与模块自己的测试核实一遍,本页的
事实正是这样收集的。它不属于本站;需要当前、逐模块的细节时,把
它与本页一起打开。为什么这对在仓库里工作的 Agent 尤其重要,见
[面向 AI Agent](/zh-cn/docs/ai-agents/)。

## Source

- [仓库根 CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md)
- [docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md)
