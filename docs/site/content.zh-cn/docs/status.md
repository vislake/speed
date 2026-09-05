---
title: 实现状态
weight: 6
---

# 实现状态

权威、始终最新的说法在仓库根
[CLAUDE.md](https://github.com/vislake/speed/blob/main/CLAUDE.md) 的
Repository Status 一节，本页刻意不重复它——逐模块的重复统计只会在
下一个模块下一轮落地的那一刻就开始过期。下面是在本页最后一次写作时
对照真实仓库核实过的粗粒度快照；请把它当作一个方向性参考，而不是
信息源本身。

> [!NOTE]
> **用里程碑编号来描述状态已经不再是这里有用的框架** —— 实现进度早已
> 远远超出本站最初跟踪的 M0 里程碑。规划中的模块图谱大部分已经落地；
> 路线图的里程碑编号
> （[docs/internal/15-roadmap.md](https://github.com/vislake/speed/blob/main/docs/internal/15-roadmap.md)）
> 仍然记录着每个模块最初被排在哪一轮，但有几个模块的交付明显早于那个
> 排期窗口。

## 今天真实存在的

| 领域 | 状态 |
|---|---|
| Go module | 根 `go.work` 里列出的模块**全部 21 个**都有真实、经过测试的实现——`go build`、`go vet`、`golangci-lint run ./...` 和 `go test -race` 全部通过。清单见[模块索引](../modules/)。 |
| Go 的 CI 覆盖 | 全部 **21** 个模块都在 `fast-check.yml`（每一个 pull request，加上每一次推送到 `main`）里有自己的矩阵行——`golangci-lint`、`go vet`、开启 race 检测的单元测试、workspace 上下文构建，以及 `GOWORK=off` 的独立构建，逐模块执行，`go/admin` 也不例外。 |
| npm 包 | `web/packages/` 下的 **11** 个 `@speed/*` 包，加上 reference app 自己的 web host 作为未纳入版本管理的第十二个 workspace 成员，都已实现、经过测试，lint/typecheck/build 全部干净通过。 |
| API 契约 | 六份 OpenAPI 片段（notes、org、authn、storage、notification、sharing）驱动 spec 先行的生成流程；其中三份（notes、authn、notification）汇入合并后的文档，同时生成 `@speed/api-sdk` 前端。`admin` 与 `pki` 也各自有自己的片段，但还没有接入 `api-contract` 流水线。 |

## CI 流水线

| 流水线 | 触发条件 | 覆盖范围 |
|---|---|---|
| `fast-check.yml` | 每一个 pull request，加上每一次推送到 `main` | 21 个模块矩阵的 lint、vet 与开启 race 检测的单元测试；11 个 npm 包加 reference-app web host 的 lint/typecheck/test/build；仓库级检查（`docs/internal` 之外的 CJK 扫描、workspace 级构建、`go.work` 漂移检测、架构纪律相关的 semgrep 规则）。 |
| `full-check.yml` | 打了 `full-ci` 标签的 PR，加上每一次推送到 `main` | 同一份模块矩阵，再加上基于 Docker 的 PostgreSQL/Redis/MinIO 集成测试层，以及 reference app 自己的组合式 HTTP 流程测试。 |
| `docs-check.yml` | 触碰文档或 i18n 资源的 PR | i18n key 集合的一致性（zh-CN 对 en-US），以及本站自己的结构检查（`tools/check_docs_site.py`，现在对照一次真实的 `hugo build` 产物执行）。 |
| `api-contract.yml` | 触碰 API 契约工具链的 PR | 从 OpenAPI spec 重新生成后端接口（对已合并的片段，同时重新生成前端 SDK），并重新构建 reference app，让没有对应实现的 spec 变更无法通过编译。 |
| `security.yml` | 每一个 PR，加上每天一次的定时任务 | 依赖审计、密钥扫描、CodeQL、许可证检查。 |
| `release.yml` | 仅手动触发 | 完全离线校验一份锁步、单一版本号的发布计划。目前没有接入任何发布凭据——真正的发布是更晚的 v1.0 里程碑。 |
| `docs-site-deploy.yml` | 推送到 `main` 且触碰 `docs/site/**`，加上手动触发 | 用 `hugo --minify` 构建本站，把 `public/` 部署到 GitHub Pages。 |

`e2e.yml`、`nightly.yml` 与 `scaffold-verify.yml` 目前是刻意保持
门控关闭的桩，还没有在任何 pull request 上触发。

## 还没做完的

- **分布式部署模式**本身从来没有真正启动过——每个模块都只在单进程
  模式下被证明可用，分布式模式所需的基础设施（Redis Streams 等）
  目前也只在单进程拓扑内部被验证过。
- 面向所有者的分享链接管理（`sharing`）目前只有 service 层，还没有
  HTTP 接口。
- `compliance` 已经落地保留期清扫、即时"被遗忘权"擦除与导出投递；
  剩下的范围更窄——数据库级别的不可变强制、可选的哈希链、格式化的
  报告导出、分区归档。
- `admin` 的第二轮工作——角色管理、用量看板、按租户强制执行——尚未
  落地。
- 浏览器页面与端到端测试这两条线，以及 v1.0 发布本身（真正的包
  发布），都还在前面。

## 自己核实权威状态

仓库根 `CLAUDE.md` 的 Repository Status 一节逐模块说明了今天在 CI
里真正跑通并通过的是什么——它本身就是写来被*核实*而不是被轻信的：
在依赖某个断言之前，对照工作流文件和模块自己的测试核实一遍，这也
正是本页自己那些事实的收集方式。它不属于本站；需要当前、逐模块的
细节时，把它和这一页一起打开看。为什么这一点对正在仓库里工作的
Agent 尤其重要，见[面向 AI Agent](../ai-agents/)。
