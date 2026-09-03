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
task release:plan   # 离线验证某个版本号下全模块的 lockstep 发布计划一致（M0 轮）
```

`task release:plan` 是 M0 发布轮新增的真实任务（非 stub）：离线验证"给定版本号下，全部 Go 模块与 npm 包能按同一版本号一致发布"——包装 `tools/release/lockstep-release.py` 的默认校验模式（退出码 0 仅当计划一致；不写任何文件），是 [02 仓库结构与发布](02-repo-and-release.md) / [18 CI/CD](18-cicd.md) 发布设计的 M0 落地一半。用法：`task release:plan VERSION=v1.2.0`。`.github/workflows/release.yml` 手动触发时运行同一校验加协调器自测（见 [18 CI/CD](18-cicd.md) 的 M0 注记）；真实发布——推 tag、changesets bump、npm publish、GitHub Release——排在 v1.0（M4 里程碑）。

`task dev` 必须在 **单进程部署模式**下工作：单进程、SQLite、零外部依赖。这是单进程部署模式给开发体验带来的直接收益——本地开发不需要 `docker compose up` 拉起一堆容器。

### 工具链版本统一
用 **mise**（或 asdf）锁定 Go、Node、pnpm、golangci-lint 等版本，配置文件入库。CI 与本地读同一份配置，杜绝"我本地是好的"。

**当前状态：已落地（M0 工具链轮次）。** 根目录 `.mise.toml` 用 mise 锁定五个工具：task 3.53.1（唯一来源是 Taskfile 头部注释）、go 1.25.0（镜像 `go.work` 指令）、node 24（镜像 `web/.nvmrc`）、pnpm 11.1.2（镜像 `web/package.json` 的 `packageManager`）、golangci-lint 2.11.4（镜像 setup-go-env 的 `GOLANGCI_VERSION`）。与计划句"CI 与本地读同一份配置"有一个诚实偏差：CI 读不到 `.mise.toml`——`actions/setup-go` 的 go-version-file 只解析 go.mod / go.work / go.sum / .go-version，`setup-node` 只读 `web/.nvmrc`——所以 CI 继续读权威源，`.mise.toml` 是本地 `mise install`（`task setup` 的工具链腿）使用的镜像；两份文件并存必然漂移，因此 `tools/check_toolchain.py` 作为漂移闸门接在 pr-check 的 repo-checks job（每次 PR 都跑），任一镜像与权威源不一致即失败。升版本时权威源与 `.mise.toml` 必须一起改，各工具的来源逐条写在 `.mise.toml` 头部注释里。数据库初始化与 lefthook 预提交钩子仍未实现——`task setup` 的注释说明了原因，随后续轮次落地。

### 种子数据
`task seed` 生成一套可用的演示数据：两个租户、多层级组织、若干用户与角色、示例套餐与订阅。reference-app 的演示和本地调试都依赖它，必须保持可用（纳入 CI 检查）。

**当前状态：尚未实现。** reference-app 目前没有任何演示数据装载路径——`cmd/server/server.go` 只硬编码了两个演示 Host→租户映射，各表启动时为空。因此 Taskfile 里的 `task seed` 是 not-implemented stub（说明缺什么、如何临时手动演示，退出非零），待 reference-app 接入 authn/org/billing 等模块后随真正的数据装载器一起落地（见 [14 reference-app](14-reference-app.md)）。

## 模块生成器

新增一个 Go module 需要八件事：go.mod、目录骨架、`AGENTS.md`、`docs/` 设计文档、迁移目录、测试骨架、CI 矩阵登记、发布登记。其中发布登记不是独立动作——它就是 go.work `use` 条目本身：发布协调器（`tools/release/lockstep-release.py`，M0 轮落地）在运行时从 go.work 推导每模块 tag 列表，从未登记进 go.work 的模块不可能被打 tag（详见下方 M0 注记）。手工做八件事必然遗漏，所以生成器 `tools/new_module.py` 自动完成其中可以安全自动化的部分，其余以**注册清单**逐项提醒，不让任何一件无声漏掉：

```
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/NN-name.md
```

生成器一次产出与现有未实现模块（`go/sharing`、`go/notification`、`go/storage`）完全一致的 stub 形态——`go/<name>/` 下的 go.mod（`module github.com/vislake/speed/go/<name>` + 裸 `go 1.23` 指令）、doc.go、`AGENTS.md` 三个文件，仅此而已。它**从不改写共享仓库文件**（go.work、CI 矩阵、发布脚本等）——一个会静默改写 go.work 与 CI 矩阵的脚手架会让评审 diff 不可读，所以注册类事项以清单打印，交给人逐项执行——`task new:module` 只是转调入口，同样只打印清单、绝不代写共享文件。八件事的覆盖情况：

- **go.mod、目录骨架、`AGENTS.md`**：由生成器产出，即 stub 的全部文件。
- **`docs/` 设计文档**：作为 `--design-doc` 输入参数；尚不存在时生成器仅警告、不失败——但 `AGENTS.md` 的 stub 行已经指向它，设计文档必须与该模块同 PR 提交。
- **CI 矩阵登记、发布登记**：出现在注册清单里（连同 go.work `use` 条目与 roadmap/文档导航登记）。这两类登记漏掉不会立即报错——CI 矩阵漏登记会让模块漏跑 CI，正是生成器要兜住的遗漏；发布登记则与清单第 1 项的 go.work `use` 条目是同一件事，见下方 M0 注记。
- **迁移目录、测试骨架**：stub 没有迁移也没有测试，生成器不为它们占位空目录；两者在模块的实现轮次随代码落地（版本化迁移与测试要求见根 [CLAUDE.md](../../CLAUDE.md)），比骨架阶段占位更贴近真实状态。

**M0 注记：发布登记的语义随 lockstep 发布脚本落地而简化。** 本轮交付的发布协调器（`tools/release/lockstep-release.py`，含其 unittest 套件；入口为 `task release:plan` 与 `.github/workflows/release.yml`）在运行时从 go.work 推导可发布模块集合——**因此清单第 1 项的 go.work `use` 条目本身就是发布登记**，清单第 3 项的表述已相应改为说明这一点，不再存在独立的"每模块 tag 列表"。配套地，模块漏登不再"悄悄漏 tag"：协调器的完备性检查双向核对 go.work 与 `go/` 目录树（`use` 条目缺 go.mod、`go/` 下存在未登记模块都报错退出），漏了任何一项，`task release:plan` 与 release.yml 的发布验证就直接失败。npm 侧的对应物是 `web/.changeset/config.json` 的 fixed group 覆盖集合：新增或移除 npm 包时必须与包列表在同一改动里同步（覆盖不齐同样使发布验证失败）。第一轮真实发布时还要执行的"过渡态 replace 行清理"（把模块 go.mod 里的 `replace ... => ../<模块>` 改写为真实版本）在 M0 只以纯函数 + testdata 夹具形式交付，**严禁对真实 go.mod 运行**——树的过渡态保留到 v1.0（M4）；详见 [02 仓库结构与发布](02-repo-and-release.md) 的 M0 注记与 `tools/release/AGENTS.md`。

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
- [ ] 新增基础设施依赖已提供至少一套零外部依赖的实现，且每套实现都声明能力并通过该 seam 的契约测试
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

**当前状态：前后端两半均已落地（M0，以 reference-app 的 notes 模块为示范），spec-first 闭环真实运转。** 仓库现在有了第一套真实运转的 spec-first 闭环：

- **模块自持 spec 片段**：`examples/reference-app/internal/notes/api/openapi.yaml`——[21 API 契约](21-api-contract.md)"规范的组织与合并"里 `<module>/api/openapi.yaml` 惯例的第一个实例（落在 reference-app 而非 go/ 模块下），生成器配置与生成物同目录：`oapi-codegen.yaml`（钉定 oapi-codegen v2.8.0）生成 `notes-server.gen.go`。
- **`task api:gen` 已从 not-implemented stub 变为真实任务**：在上述 api/ 目录内执行 `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config oapi-codegen.yaml openapi.yaml`，重新生成生成物。
- **"编译失败暴露待改点"真实生效**：`internal/notes/handler.go` 以 `var _ api.ServerInterface = (*Handler)(nil)` 编译期断言实现生成的 interface，并以 `api.HandlerFromMux` 让路由从片段本身推导——往片段加一个 operation 后重新生成，handler 不补实现就编译不过（本轮的编译失败演示即验证此路径）。
- **CI 兜底已接线**：`.github/workflows/api-contract.yml` 在改动 spec 片段 / 生成器配置（含 `web/orval.config.ts` 与 `web/scripts/**`）/ `Taskfile.yml` / 流水线自身的 PR 上触发（路径过滤），后端 oapi-codegen 重新生成后 `git diff --exit-code` 比对生成物，并 `go build` reference-app 保证 handler 跟上 spec——这是 [18 CI/CD](18-cicd.md) 管道表 api-contract 行所规划"生成物一致性 diff"的后端一半；前端一半见下一条。
- **前端 sdk 一半已落地**：`@speed/api-sdk`（`web/packages/api-sdk`）由钉定的 orval 8.17.0 从同一 notes 片段生成 hooks 与 TS 类型（DO-NOT-EDIT 头带钉定版本），`task api:gen` 的前端 leg 执行 `cd web && pnpm dlx orval@8.17.0 --config orval.config.ts && node scripts/orval-nodenext-fixup.mjs`（orval 永不进入 lockfile）；生成代码不直接触碰网络，经包内唯一手写接缝 `src/runtime.ts`（`bindRequestFn(createClient(...))`）路由到 api-client 运行时；orval 发射的无扩展名 mutator 导入由 `web/scripts/orval-nodenext-fixup.mjs` 确定性改写为显式 `.js`（nodenext/TS2835，机制与延期细节见该包 AGENTS.md）。该包进入 pr-check 的 npm 矩阵与 api-contract.yml 的第二个一致性 diff；与后端相同的"task api:gen + CI 重新生成比对"模式，api:gen 的两个 leg 与 api-contract.yml 的两个再生成步骤一一对应、保持 lockstep。

**当前状态（2026-09-03，authn 轮：第二个片段落地）：多片段合并与 lint 已落地，oasdiff 仍未实现。** `go/authn/api/openapi.yaml` 是继 notes 之后的第二个模块 spec 片段，触发了原先"没有合并对象"的那道缺口：仓库根目录的 `redocly.yaml` 定义了合并规则与命名规范 lint 规则，`task api:merge`（`Taskfile.yml`）与 `.github/workflows/api-contract.yml` 用钉定的 `@redocly/cli@2.51.1` 的 `join` 命令把两个片段合并进 `build/openapi/speed.yaml` 并按 `redocly.yaml` 的规则 lint，`git diff --exit-code` 校验该文件与提交版本一致——这一段随 `task api:gen` 的既有骨架一并扩展，不是另起的机制。仍未实现的只剩 oasdiff 破坏性变更闸门（需首个发布基线，计划 M4，已作为机制决策记录而非假闸门）。详见 [21 API 契约](21-api-contract.md) 末尾的实现状态注记。
**当前状态（2026-09-03，auth-core 轮：第三个片段的前端半边与首个编译消费者落地）：orval 前端 leg 改为消费合并文档，oasdiff 仍未实现。** `go/authn/api/openapi.yaml` 成为第三个模块 spec 片段后，`task api:gen` 的前端 leg 随之改为依赖 `api:merge`：钉定的 orval 从合并后的 `build/openapi/speed.yaml`（notes + authn，org 片段仍不在合并列表）生成 `@speed/api-sdk`——api-sdk 从此覆盖两个片段，上一段"尚未落到合并文档"与 api-sdk 单一 notes 源的声称到此为止。同一轮落地的 `@speed/auth-core`（`web/packages/auth-core`）成为 api-sdk 生成面的**首个 in-workspace compile consumer**：其单元套件经 `bindRequestFn` 接缝绑定 scripted `RequestFn` 驱动生成操作并做类型检查，`src/usage-example.test.tsx` 把 README 用法编译执行——"生成层没有消费者做类型检查"的推迟理由随之消除；**运行时端到端消费**（reference-app shell 以真实客户端驱动真实登录）仍属 consumer-shell（`auth-ui`）round。api:gen 后端 leg 继续逐片段独立生成；api-contract.yml 的再生成与 diff 覆盖合并文档的这条前端 leg。仍未实现的只剩 oasdiff 破坏性变更闸门（需首个发布基线，计划 M4）。详见 [21 API 契约](21-api-contract.md) 末尾两条实现状态注记。

**当前状态（2026-09-03，auth-ui 轮：运行时端到端消费以包级 in-form 形态落地，浏览器 leg 移交 shell）：** 上一段 auth-core 轮注记中"**运行时端到端消费**（reference-app shell 以真实客户端驱动真实登录）仍属 consumer-shell（`auth-ui`）round"的声称由本段取代——落地的 `@speed/auth-ui`（`web/packages/auth-ui`，登录组件家族：`SignInScreen`/`PasswordSignInForm`/`SMSSignInForm`/`RegisterForm`/`SocialSignInSection`/`SocialCallbackHandler`/`SignOutButton`/`SessionEndedScreen`）把该消费在包级以 in-form 形态兑现：`src/usage-example.test.tsx` 用**真实 `@speed/api-client`**（`createClient` + 内存 access-token store + 可注入 fetch；fetch 替身以真正的 `Response` 对象作答）经同一 `bindRequestFn` 接缝绑定，编译并执行 README quick start 的组合——密码登录 → 受保护请求（过期的 access token）以 `authn.token_expired` 被拒 → 静默刷新 → 服务端会话死亡（`authn.session_revoked`）→ 收敛匿名 → 再次登录 → `switchLanguage` 到 en-US，六次请求顺序钉死。剩余的浏览器 + 真服务器 leg 与跨路由门禁随 reference-app shell（生成 hooks、租户 query-key 命名空间、`RouteGuard` 门禁随之落地）与 M4 e2e 管线落地。oasdiff 破坏性变更闸门仍未实现（同上两段）。详见 [21 API 契约](21-api-contract.md) 末尾的实现状态注记。

**先写实现再补 spec 是被禁止的**——那等于回到 code-first，失去编译期约束的全部意义。

## CODEOWNERS

地基模块（`pkgcore`、`dbkit`、`tenancy`）与发布流水线设专属 owner，改动需其批准。这几处的错误会波及所有下游模块与已交付项目，值得多一道关卡。

## 依赖管理

- **Renovate** 自动提依赖升级 PR，按周聚合，安全补丁立即提。
- 主要框架（Go、React、MUI、GORM）的大版本升级单独走 PR 并跑全量矩阵。
- **依赖新增需要理由**：脚手架的依赖会传导给所有业务项目，新增第三方依赖需在 PR 中说明必要性与替代方案评估。这一条比在普通业务项目里重要得多。
