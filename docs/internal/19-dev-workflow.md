# 开发工作流

> 本地环境、分支与提交规范、评审要求。目标是让新人在半小时内跑起来，并让 21 个模块的协作不至于失控。

## 本地开发环境

### 一键启动
统一命令入口用 **Taskfile**（`task` 命令，YAML 定义，跨平台，比 Makefile 更适合混合 Go/Node 的仓库）：

```
task setup          # 安装工具链、拉依赖、初始化数据库
task dev            # 单进程部署模式启动后端 + 前端（热重载）
task test           # 跑受影响模块的测试
task test:full      # 全量矩阵
task lint           # 全部 lint
task api:gen        # 合并 spec + 生成后端 interface + 生成前端 sdk
task docs:serve     # 本地预览文档站
task new:module     # 脚手架自身的模块生成器（见下）
```

`task dev` 必须在 **单进程部署模式**下工作：单进程、SQLite、零外部依赖。这是单进程部署模式给开发体验带来的直接收益——本地开发不需要 `docker compose up` 拉起一堆容器。

### 工具链版本统一
用 **mise**（或 asdf）锁定 Go、Node、pnpm、golangci-lint 等版本，配置文件入库。CI 与本地读同一份配置，杜绝"我本地是好的"。

### 种子数据
`task seed` 生成一套可用的演示数据：两个租户、多层级组织、若干用户与角色、示例套餐与订阅。reference-app 的演示和本地调试都依赖它，必须保持可用（纳入 CI 检查）。

## 模块生成器

新增一个 Go module 需要：go.mod、目录骨架、`AGENTS.md`、`docs/`、迁移目录、测试骨架、CI 矩阵登记、发布脚本登记。手工做八件事必然遗漏，所以提供 `task new:module` 一次生成，并附带**自检清单**。

同理 `task new:npm-package`。这是脚手架项目对自己的"脚手架化"——如果我们自己都嫌新增模块麻烦，说明模板设计有问题。

## 分支与合并策略

采用 **trunk-based**：短生命周期分支（建议不超过 3 天），频繁合入 `main`。

**Git 规范（团队既定，严格执行）**：
- **合并前必须先 rebase 到目标分支**，保持线性历史
- **只允许 fast-forward 合并**（`git merge --ff-only`）；无法 ff 时先 rebase，不用 merge commit

线性历史对本项目有额外价值：lockstep 版本下需要频繁在历史中定位"某个版本包含哪些改动"，非线性历史会让 `git log` 与二分排查变得困难。

## 提交规范

**Conventional Commits**，作用域用模块名。`type` 与 `scope` 用英文；描述部分的语言见 `.claude/skills/commit-convention/SKILL.md`：

```
feat(tenancy): 支持子域名解析租户
fix(billing): 修复信用点并发扣减超扣
docs(jobs): 补充 worker 内租户上下文重建说明
chore(ci): 缓存 golangci-lint 结果
```

驱动三件事：changelog 自动生成、破坏性变更识别（`!` 或 `BREAKING CHANGE`）、PR 标题校验。

**Pre-commit hooks**（lefthook）：格式化、快速 lint、提交信息校验。重活留给 CI，本地钩子必须秒级完成，否则会被绕过。

## PR 要求

PR 模板包含一份 checklist，对应仓库根 [CLAUDE.md](../../CLAUDE.md) 「Architecture Discipline」的纪律：

- [ ] 新增 Repository 已跑 `tenancytest.AssertIsolated`
- [ ] 新增基础设施依赖已提供单进程与分布式两套实现
- [ ] 新增用户可见文案已补齐中英双语
- [ ] 新增公开 API 已附使用文档 + 可编译示例 + `AGENTS.md` 条目
- [ ] 接口变更已先改 OpenAPI spec，前后端生成物已一并提交
- [ ] 涉及外部联系人发送的改动，已确认走了同意验证流程
- [ ] **Bug 修复已附带能复现该 bug 的测试**（修复前失败、修复后通过）
- [ ] 构建与测试过程中的**新增警告已处理或已在 PR 中说明原因**

后两条是团队既定规范，在本项目同样适用且不因自动化流程而豁免：
- **每个 bug 修复必须带一个真正针对该 bug 的测试**。如果确实无法添加，必须在 PR 描述中说明原因与后续计划，由 reviewer 确认。
- **警告视为一等问题**（编译警告、lint 警告、废弃 API 警告、React 控制台警告、竞态检测警告等），不得静默忽略或抑制。无法当下修复的必须显式说明并留下跟踪项。

## 契约变更流程

改任何对外接口都必须按 [21 API 契约](21-api-contract.md) 的顺序：**先改 spec → 重新生成 → 编译失败暴露待改点 → 补实现 → 补前端 → 同一个 PR 提交**。

`task api:gen` 一键完成"合并 spec + 生成后端 interface + 生成前端 sdk"。PR 中 spec 与生成物必须同时存在，CI 会重新生成并比对。

**先写实现再补 spec 是被禁止的**——那等于回到 code-first，失去编译期约束的全部意义。

## CODEOWNERS

地基模块（`pkgcore`、`dbkit`、`tenancy`）与发布流水线设专属 owner，改动需其批准。这几处的错误会波及所有下游模块与已交付项目，值得多一道关卡。

## 依赖管理

- **Renovate** 自动提依赖升级 PR，按周聚合，安全补丁立即提。
- 主要框架（Go、React、MUI、GORM）的大版本升级单独走 PR 并跑全量矩阵。
- **依赖新增需要理由**：脚手架的依赖会传导给所有业务项目，新增第三方依赖需在 PR 中说明必要性与替代方案评估。这一条比在普通业务项目里重要得多。
