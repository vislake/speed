---
title: 面向 AI Agent
weight: 4
---

# 面向 AI Agent

如果你是一个正在帮某人接入 speed 的编码 Agent，或者被指向本站作为
上下文，这一页就是写给你的。speed 自己的文档规范把 Agent 和人类同等
视为一等读者（见
[docs/internal/13](https://github.com/vislake/speed/blob/main/docs/internal/13-documentation-standards.md)，
中文设计说明）——这一页与站点根部的 [llms.txt](/speed/llms.txt) 就是本站对
这条规范的回应。

## 先按这个顺序读

1. **[仓库根 `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)**
   —— 面向任何 AI 编码工具的入门：顶层架构形态、模块依赖方向、最容易
   踩坑的规则，以及其余一切内容在哪里。写得可以在几分钟内从头读到尾。
2. **目标模块自己的 `AGENTS.md`**（`go/<name>/AGENTS.md` 或
   `web/packages/<name>/AGENTS.md`）—— 模块特定的纪律、文件布局、
   已知限制、测试方式。完整清单和直达链接见[模块索引](../modules/)。
3. **[仓库根 `CLAUDE.md`](https://github.com/vislake/speed/blob/main/CLAUDE.md)
   的 Repository Status 一节** —— 见下文，这是你绝不能让它在自己的
   认知里过期的那一份。

## "这东西到底有没有真正实现"的唯一权威来源

> [!WARNING]
> 仓库根 `CLAUDE.md` 的 **Repository Status** 一节，是"今天 CI 里到底
> 真的能跑通并通过什么"这个问题唯一、权威、当前有效的答案。它逐模块
> 指明：具体实现了什么、由哪个 CI 工作流验证（以及在什么触发条件下——
> 每一个 PR、打了 `full-ci` 标签的 PR、还是手动触发），以及哪些还只是
> 占位的桩代码。这个静态站点没办法跟上那一节变化的速度——**不要把本站
> 上的任何内容当作阅读那一节的替代品**，任何状态断言（包括本站自己的
> [实现状态](../status/)页面）在你没有对照它或对照仓库本身核实之前，
> 都不要轻信。

那一节自己陈述、这一页也重申的实用规则是：一个模块目录存在，不等于
它背后有真正的代码——有些还只是一个 `go.mod` 加一行 `doc.go` 加一个
指向设计文档的 `AGENTS.md`。在依赖某个模块之前，先确认它不止是个桩。

## 最容易踩坑的架构规则

### 模块依赖方向

依赖关系严格自底向上：

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification / pki
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

这只是一个粗略的顺序，不是完整的依赖边列表——具体哪个模块 import
了哪个模块，权威说法是
[docs/internal/01-architecture.md](https://github.com/vislake/speed/blob/main/docs/internal/01-architecture.md)
自己的依赖图。两条最容易在第一次接入时踩到的规则：`rbac` 绝不能
import `authn`（鉴权只应该看到认证方组装好的
`Subject{TenantID, UserID}`）；一个模块绝不能为了数据库关联去 import
另一个业务模块的 struct——跨模块的关联只用 ID 引用加领域事件（`org`
只按事件名和 JSON 形状的 payload 去订阅 `authn` 的 `user.created`
事件，从不 import `authn.User`）。

### API 契约：spec 先行，顺序不可协商

编辑 `api/openapi.yaml` → 运行 `task api:gen` → 由此产生的编译失败会
暴露出每一个需要修的 handler → 实现 → 更新前端 → 一起提交。生成出来
的 Go server 接口会参与编译，所以 spec 和实现之间的漂移无法通过编译。
前端这一侧是同样的逻辑：手写的 `fetch`/`axios` 调用只允许出现在
`@speed/api-client` 内部；其余每一个包都只调用生成的 `@speed/api-sdk`
hooks，从不直接发 HTTP 请求。

### 四种数据域

每一张表在被设计之前，先被归类：

| 数据域 | 定义 | 是否 `TenantScoped` | 示例 |
|---|---|---|---|
| 租户数据 | 属于某一个租户，跨租户绝不可见 | 是 | 组织节点、成员关系、订阅、媒体、业务数据 |
| 身份数据 | 属于一个自然人，此人可能属于多个租户 | 否 | `users`、`user_identities`、`sessions`、登录日志 |
| 平台数据 | 全局共享，租户只读 | 否 | 平台级 Plan 定义、社交登录 provider 配置、系统配置 |
| 关联数据 | 连接身份与租户 | 是（按 `tenant_id`） | `memberships` |

这张表存在的目的是强制执行这条规则：`users` 刻意**不**做租户级隔离
（一个人可以属于多个租户，社交登录在任何租户存在之前就已经能成功），
平台级的定义（比如一个计费 Plan）必须对每个租户的兜底查询保持可见。
按这个仓库自己的经验，把这个分类搞错，是多租户实现最早卡住的地方。

### 部署模式 vs 实现组合

这是两条正交的轴，把它们混为一谈正是这个代码库自己文档里记录过的
曾经犯过的设计错误：

- **部署模式** —— 以多少副本运行，因此决定哪些实现是*被允许的*。
- **实现组合** —— 每一个基础设施 seam（`EventBus`、`KVStore`、
  `Mailer`、`ObjectStore`）实际用的是哪一个具体实现。

部署模式并不选择实现——它只是约束实现。每个实现声明自己的能力
（`MultiReplicaSafe`、`SurvivesRestart`、`Stateless`）；每种部署模式
声明自己需要什么；当某种组合无法满足声明的模式时，装配会在启动时
失败，并指明具体是哪个 seam、哪个实现。单进程部署去连真实的
PostgreSQL、真实的 Stripe、真实的 SMTP，是小客户生产环境的正常形态，
不是误用——这个约束只朝一个方向生效。业务代码绝不能对模式分支判断
（`if mode == "standalone"` 在代码评审里是会被打回的，不是风格建议）
——模式差异只应该出现在 kernel 装配那一层。

## 写代码前值得知道的其他规则

- 租户拥有的数据的 repository 必须内嵌 `dbkit.Repository[T]`——绝不
  持有裸的 `*gorm.DB` 自己手写 `WHERE tenant_id = ?`，也绝不在 API
  层接受调用方传入的 `tenant_id`（它只能来自访问令牌的 claims）。
- worker 不会自动继承租户上下文——需要显式重建
  （`pkgcore.WithTenant(ctx, job.TenantID)`），否则 Repository 会
  失败关闭。
- 通知是事件驱动的：业务模块发布领域事件，`notification` 订阅。
  唯一的例外是同步的验证码。外部（非用户）收件人在任何东西发出之前
  都必须先完成同意验证。
- 每一个 bug 修复都要附带一个能复现该 bug 的测试（修复前失败、修复后
  通过）——这个仓库把缺失的回归测试当作修复不完整，而不是可有可无的
  加分项。

完整、可执行的清单——哪些只是代码评审约束、哪些今天真正由 CI
强制执行——见仓库根 `CLAUDE.md` 的 Architecture Discipline 一节。

## 机器可读的入口

站点根部的 [/llms.txt](/speed/llms.txt) 按照 [llms.txt](https://llmstxt.org/)
约定，列出了本站的同一批页面，以及上面提到的仓库文件，供直接抓取本
域名的爬虫或 Agent 使用。
