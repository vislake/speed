---
title: 面向 AI Agent
weight: 4
---

# 面向 AI Agent

如果你是帮助别人集成 speed 的编码 agent——或者被指向本站作为上下文——
本页就是为你准备的。本站把 agent 与人类视为同等的头等读者;本页与
站根的 [/llms.txt](/llms.txt) 就是对此的回答。

## 先读这些

1. **[根 `AGENTS.md`](https://github.com/vislake/speed/blob/main/AGENTS.md)**
   ——给任何 AI 编码工具的向导:顶层形态、模块依赖方向,以及最容易在
   首次集成时绊倒你的规则。写来可以在几分钟内从头读完。
2. **先选定本站的一侧,再读那一侧:**
   - **把 speed 集成进产品?**[用户指南](/zh-cn/docs/user-guide/)把每个
     产品领域从头走到尾;对你正在接线的模块,读[模块参考](/zh-cn/docs/user-guide/modules/)
     中它的页面——做什么、何时选用、怎么接线、示例。
   - **扩展或调试 speed 本身?**[开发者文档](/zh-cn/docs/developer-docs/)
     解释总体架构、设计原则,以及逐模块设计深入(每个模块被什么取舍塑
     成今天的样子)。[总体架构](/zh-cn/docs/developer-docs/architecture/)
     页载有模块依赖图。
3. **该模块自己的 `AGENTS.md`**(`go/<name>/AGENTS.md` 或
   `web/packages/<name>/AGENTS.md`)——模块级纪律、接线要求、已知限制、
   测试设置。本站每个模块页的 Source 小节都链到该模块的 `AGENTS.md`;
   把它当作该模块的权威描述。

## 最容易踩中的架构规则

### 模块依赖方向

依赖严格自底向上:

```
pkgcore -> dbkit / observability / ratelimit -> tenancy -> config / jobs -> storage / notification / pki
        -> authn / rbac / org / metering -> billing / ai-gateway / sharing / integration
        -> compliance -> admin
```

这是粗粒排序——[总体架构](/zh-cn/docs/developer-docs/architecture/)页
有完整图景。最容易让首次集成者犯错的规则有两条:`rbac` 绝不 import
`authn`(授权只认识 `Subject{TenantID, UserID}`,由认证侧组装),以及
模块绝不因数据库关系 import 另一个业务模块的类型——跨模块关系是
ID 引用加领域事件(`org` 按名称与 JSON 形载荷订阅 `authn` 的
`user.created` 事件;它从不 import `authn.User`)。

### API 契约:先契约后代码,顺序不可逆

改 `api/openapi.yaml` → 跑 `task api:gen` → 编译失败暴露每个待修的
handler → 实现 → 更新前端 → 一起提交。生成的 Go server 接口参与编译,
所以契约与实现之间的漂移无法编译通过。前端镜像同一纪律:手写
`fetch`/`axios` 只允许出现在 `@speed/api-client` 内部;其它包一律调用
生成的 `@speed/api-sdk` hooks,绝不直接 HTTP。

### 四数据域

每张表在设计之前先分类,而不是之后:

| 数据域 | 定义 | `TenantScoped`? | 例 |
|---|---|---|---|
| 租户数据 | 属于一个租户,绝不跨租户可见 | 是 | org 节点、成员关系、订阅、媒体、业务数据 |
| 身份数据 | 属于自然人,可属于多个租户 | 否 | `users`、`user_identities`、`sessions`、登录日志 |
| 平台数据 | 全局共享,租户只读 | 否 | 平台级 Plan 定义、社交登录提供商配置、系统配置 |
| 关联数据 | 桥接身份与租户 | 是(按 `tenant_id`) | `memberships` |

这张表存在的理由:`users` 故意**不**租户化(一个人可以属于多个租户,
社交登录在任何租户存在之前就成功),平台级定义如计费 Plan 必须对每个
租户的回退查找可见。按本代码库自己的经验,分类搞错是多租户实现最先
卡住的地方。

### 部署模式 × 实现组装

两条正交轴,把二者混为一谈是这套代码库自己的历史记录里记下的
错误:

- **部署模式**——以多少个副本运行,因此哪些实现是*允许的*。
- **实现组装**——每条基础设施模块(`EventBus`、`KVStore`、`Mailer`、
  `ObjectStore`)实际用哪个实现。

部署模式不选择实现——它只约束实现。每个实现声明能力
(`MultiReplicaSafe`、`SurvivesRestart`、`Stateless`);每个部署模式声明
它要求什么;当组装不能满足声明的模式时,启动失败,
点名组件、缺失的能力位与模式。
单进程部署连真 PostgreSQL、真 Stripe、真 SMTP 是小客户生产安装的
寻常形态,不是误用——约束只向一个方向。业务代码绝不按模式分支
(`if mode == "standalone"` 是代码评审拒绝项,不是风格洁癖)——模式
差异只属于内核接线。

## 写代码前值得知道的规则

- 租户数据仓库必须嵌入 `dbkit.Repository[T]`——绝不手握裸
  `*gorm.DB` 手写 `WHERE tenant_id = ?`,绝不在 API 层接受调用方提供
  的 `tenant_id`(它只来自访问令牌声明)。
- worker 不继承租户上下文——显式重建
  (`pkgcore.WithTenant(ctx, job.TenantID)`),否则 Repository 关闭失败。
- 通知是事件驱动的:业务模块发布领域事件,`notification` 订阅。唯一
  例外是同步验证码。外部(非用户)收件人必须先完成同意验证才发送。
- 每个 bug 修复都带复现它的测试(修复前失败、修复后通过)。

可执行的纪律全表在[开发者文档](/zh-cn/docs/developer-docs/design-principles/),
各模块的 `AGENTS.md` 陈述模块专属规则。

## 机器可读入口

站根的 [/llms.txt](/llms.txt) 按 [llms.txt](https://llmstxt.org/)
惯例列出每一节与每一页,供直接抓取本站域的爬虫或 agent 使用。
