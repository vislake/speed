# 开发工作流

> 本地环境、分支与提交规范、评审要求。目标是让新人在半小时内跑起来，并让模块间的协作不至于失控。

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

**当前状态：已落地（M0 工具链轮次）。** 根目录 `.mise.toml` 用 mise 锁定五个工具：task 3.53.1（唯一来源是 Taskfile 头部注释）、go 1.25.0（镜像 `go.work` 指令）、node 24（镜像 `web/.nvmrc`）、pnpm 11.1.2（镜像 `web/package.json` 的 `packageManager`）、golangci-lint 2.11.4（镜像 setup-go-env 的 `GOLANGCI_VERSION`）。与计划句"CI 与本地读同一份配置"有一个诚实偏差：CI 读不到 `.mise.toml`——`actions/setup-go` 的 go-version-file 只解析 go.mod / go.work / go.sum / .go-version，`setup-node` 只读 `web/.nvmrc`——所以 CI 继续读权威源，`.mise.toml` 是本地 `mise install`（`task setup` 的工具链腿）使用的镜像；两份文件并存必然漂移，因此 `tools/check_toolchain.py` 作为漂移闸门接在 pr-check 的 repo-checks job（每次 PR 都跑），任一镜像与权威源不一致即失败。升版本时权威源与 `.mise.toml` 必须一起改，各工具的来源逐条写在 `.mise.toml` 头部注释里。数据库初始化与 lefthook 预提交钩子仍未实现——`task setup` 的注释说明了原因，随后续轮次落地。

### 种子数据
`task seed` 生成一套可用的演示数据：两个租户、多层级组织、若干用户与角色、示例套餐与订阅。reference-app 的演示和本地调试都依赖它，必须保持可用（纳入 CI 检查）。

**当前状态：尚未实现。** reference-app 目前没有任何演示数据装载路径——`cmd/server/server.go` 只硬编码了两个演示 Host→租户映射，各表启动时为空。因此 Taskfile 里的 `task seed` 是 not-implemented stub（说明缺什么、如何临时手动演示，退出非零），待 reference-app 接入 authn/org/billing 等模块后随真正的数据装载器一起落地（见 [14 reference-app](14-reference-app.md)）。

## 模块生成器

新增一个 Go module 需要八件事：go.mod、目录骨架、`AGENTS.md`、`docs/` 设计文档、迁移目录、测试骨架、CI 矩阵登记、发布脚本登记。手工做八件事必然遗漏，所以生成器 `tools/new_module.py` 自动完成其中可以安全自动化的部分，其余以**注册清单**逐项提醒，不让任何一件无声漏掉：

```
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/NN-name.md
```

生成器一次产出与现有未实现模块（`go/sharing`、`go/notification`、`go/storage`）完全一致的 stub 形态——`go/<name>/` 下的 go.mod（`module github.com/vislake/speed/go/<name>` + 裸 `go 1.23` 指令）、doc.go、`AGENTS.md` 三个文件，仅此而已。它**从不改写共享仓库文件**（go.work、CI 矩阵、发布脚本等）——一个会静默改写 go.work 与 CI 矩阵的脚手架会让评审 diff 不可读，所以注册类事项以清单打印，交给人逐项执行——`task new:module` 只是转调入口，同样只打印清单、绝不代写共享文件。八件事的覆盖情况：

- **go.mod、目录骨架、`AGENTS.md`**：由生成器产出，即 stub 的全部文件。
- **`docs/` 设计文档**：作为 `--design-doc` 输入参数；尚不存在时生成器仅警告、不失败——但 `AGENTS.md` 的 stub 行已经指向它，设计文档必须与该模块同 PR 提交。
- **CI 矩阵登记、发布脚本登记**：出现在注册清单里（连同 go.work `use` 条目与 roadmap/文档导航登记）。这两类登记漏掉不会立即报错，却会让模块漏跑 CI、漏打 lockstep tag，正是生成器要兜住的遗漏。
- **迁移目录、测试骨架**：stub 没有迁移也没有测试，生成器不为它们占位空目录；两者在模块的实现轮次随代码落地（版本化迁移与测试要求见根 [CLAUDE.md](../../CLAUDE.md)），比骨架阶段占位更贴近真实状态。

`task new:module` 是这层脚手架的 Taskfile 包装，已接线转调本脚本（接线契约见脚本 `--help` 的 epilog）：

```
task new:module NAME=<name> DESCRIPTION='...' DESIGN_DOC=docs/internal/NN-<name>.md
```

直接运行上面的 `python3` 命令效果相同。同理的 `task new:npm-package` 也尚未实现——web/ 工作区虽已存在（`@speed/tokens`、`@speed/i18n` 已落地），但还没有 npm 包模板脚手架，脚本的 `--category npm` 目前仍直接拒绝。这是脚手架项目对自己的"脚手架化"——如果我们自己都嫌新增模块麻烦，说明模板设计有问题。

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

**当前状态：后端一半已落地（M0，以 reference-app 的 notes 模块为示范），其余待工具链轮次交付。** 仓库现在有了第一套真实运转的 spec-first 闭环：

- **模块自持 spec 片段**：`examples/reference-app/internal/notes/api/openapi.yaml`——[21 API 契约](21-api-contract.md)"规范的组织与合并"里 `<module>/api/openapi.yaml` 惯例的第一个实例（落在 reference-app 而非 go/ 模块下），生成器配置与生成物同目录：`oapi-codegen.yaml`（钉定 oapi-codegen v2.8.0）生成 `notes-server.gen.go`。
- **`task api:gen` 已从 not-implemented stub 变为真实任务**：在上述 api/ 目录内执行 `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config oapi-codegen.yaml openapi.yaml`，重新生成生成物。
- **"编译失败暴露待改点"真实生效**：`internal/notes/handler.go` 以 `var _ api.ServerInterface = (*Handler)(nil)` 编译期断言实现生成的 interface，并以 `api.HandlerFromMux` 让路由从片段本身推导——往片段加一个 operation 后重新生成，handler 不补实现就编译不过（本轮的编译失败演示即验证此路径）。
- **CI 兜底已接线**：`.github/workflows/api-contract.yml` 在改动 spec 片段 / 生成器配置 / `Taskfile.yml` / 流水线自身的 PR 上触发（路径过滤），重新生成后 `git diff --exit-code` 比对生成物，并 `go build` reference-app 保证 handler 跟上 spec——这是 [18 CI/CD](18-cicd.md) 管道表 api-contract 行所规划"生成物一致性 diff"的后端一半。

仍未实现——上面计划句描述的仍是完整目标：合并各模块片段成 `build/openapi/speed.yaml`（目前只有 notes 一个片段，没有合并对象）、redocly 规范 lint、orval 前端 sdk 与 `@speed/api-sdk`（前端 sdk 包待后续 web/ 工作区轮次落地：api-client/api-sdk）、oasdiff 破坏性变更闸门。这些随 API 契约工具链轮次（[15 roadmap](15-roadmap.md)）交付，届时 `task api:gen` 与 api-contract.yml 在现有骨架上扩展；详见 [21 API 契约](21-api-contract.md) 末尾的实现状态注记。

**先写实现再补 spec 是被禁止的**——那等于回到 code-first，失去编译期约束的全部意义。

## CODEOWNERS

地基模块（`pkgcore`、`dbkit`、`tenancy`）与发布流水线设专属 owner，改动需其批准。这几处的错误会波及所有下游模块与已交付项目，值得多一道关卡。

## 依赖管理

- **Renovate** 自动提依赖升级 PR，按周聚合，安全补丁立即提。
- 主要框架（Go、React、MUI、GORM）的大版本升级单独走 PR 并跑全量矩阵。
- **依赖新增需要理由**：脚手架的依赖会传导给所有业务项目，新增第三方依赖需在 PR 中说明必要性与替代方案评估。这一条比在普通业务项目里重要得多。
