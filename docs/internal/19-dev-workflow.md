# 开发工作流

> 本地环境、分支与提交规范、评审要求。目标是让新人在半小时内跑起来，并让模块间的协作不至于失控。

## 本地开发环境

### 一键启动
统一命令入口用 **Taskfile**（`task` 命令，YAML 定义，跨平台，比 Makefile 更适合混合 Go/Node 的仓库）：

```
task setup          # 安装工具链、拉依赖（数据库初始化尚未实现，见下）
task dev            # 单进程部署模式启动后端 + 前端（热重载，尚未实现，见下）
task test           # 跑全部模块的单元测试（无差异检测，见下）
task test:full      # 全量矩阵
task lint           # 全部 lint
task api:gen        # 合并 spec + 生成后端 interface + 生成前端 sdk
task docs:serve     # 本地预览文档站
task new:module     # 脚手架自身的模块生成器（见下）
task release:plan   # 离线验证某个版本号下全模块的 lockstep 发布计划一致
```

**`task`/`mise` 二进制本身在标准检出环境里可能未安装**（根 `CLAUDE.md` 已有此说明）：`Taskfile.yml` 与其包装的命令都真实存在且能跑，但 `task` 这个 CLI 本身不一定在 `PATH` 上——遇到时直接跑它包装的原始命令（`go test ./...`、`go vet ./...`、`golangci-lint run ./...` 等），不要假设 `task xxx` 就一定可用，先确认 `task` 在 `PATH` 上。`mise` 同理：`task setup` 的工具链腿在 `mise` 缺席时只警告并跳过，不会失败（见下方"工具链版本统一"一节）。

`task release:plan` 是真实任务（非 stub）：离线验证"给定版本号下，全部 Go 模块与 npm 包能按同一版本号一致发布"——包装 `tools/release/lockstep-release.py` 的默认校验模式（退出码 0 仅当计划一致；不写任何文件）。用法：`task release:plan VERSION=v1.2.0`。`.github/workflows/release.yml` 手动触发时运行同一校验加协调器自测（见 [18 CI/CD](18-cicd.md) 的状态注记）；真实发布——推 tag、changesets bump、npm publish、GitHub Release——仍未落地（无发布凭据，同 18 号注记）。

`task dev` 必须在 **单进程部署模式**下工作：单进程、SQLite、零外部依赖。这是单进程部署模式给开发体验带来的直接收益——本地开发不需要 `docker compose up` 拉起一堆容器。

**当前状态：尚未实现。** 和 `task seed` 一样，`task dev` 目前是 not-implemented stub（跑起来会打印说明，退出非零）：它承诺的"后端 + 前端、单进程部署模式、热重载"组合式开发循环还没有接线——`web/` pnpm workspace 与 `examples/reference-app/web` 这个消费者壳都已经真实存在，但都还没有接进一个热重载 runner。真正能跑起来的是 reference-app 服务器本身：`cd examples/reference-app && go run ./cmd/server`（默认监听 `:8080`，SQLite 落 `./reference-app.db`，两者都可用环境变量覆盖），直接跑这一行即可。

**`task test` 的真实实现：** 命令表里"跑受影响模块的测试"是设计意图，不是现状——`Taskfile.yml` 的 `test` task 就是无条件的 `go test {{.ALL_PKGS}}` 加 reference-app 自己的 `go test ./...`，没有任何 diff 检测或"只跑改动模块"的逻辑，每次调用都跑全部真实模块的单元测试（无 `-race`、无覆盖率，那两项留给 `task test:full`）。差异感知的"只测受影响模块"仍未实现。

### 工具链版本统一
用 **mise**（或 asdf）锁定 Go、Node、pnpm、golangci-lint 等版本，配置文件入库。CI 与本地读同一份配置，杜绝"我本地是好的"。

**当前状态：已落地。** 根目录 `.mise.toml` 用 mise 锁定五个工具——task、go、node、pnpm、golangci-lint——每个工具的权威源与当前版本逐条写在 `.mise.toml` 头部注释里（go 镜像 `go.work` 指令、node 镜像 `web/.nvmrc`、pnpm 镜像 `web/package.json` 的 `packageManager`、golangci-lint 镜像 setup-go-env 的 `GOLANGCI_VERSION`）。与计划句"CI 与本地读同一份配置"有一个诚实偏差：CI 读不到 `.mise.toml`——`actions/setup-go` 的 go-version-file 只解析 go.mod / go.work / go.sum / .go-version，`setup-node` 只读 `web/.nvmrc`——所以 CI 继续读权威源，`.mise.toml` 是本地 `mise install`（`task setup` 的工具链腿）使用的镜像；两份文件并存必然漂移，因此 `tools/check_toolchain.py` 作为漂移闸门接在 fast-check 的 repo-checks job（每次 PR 都跑），任一镜像与权威源不一致即失败。升版本时权威源与 `.mise.toml` 必须一起改。数据库初始化与 lefthook 预提交钩子仍未实现——`task setup` 的注释说明了原因。

### 种子数据
`task seed` 生成一套可用的演示数据：两个租户、多层级组织、若干用户与角色、示例套餐与订阅。reference-app 的演示和本地调试都依赖它，必须保持可用（纳入 CI 检查）。

**当前状态：尚未实现。** reference-app 没有独立的演示数据装载器——Taskfile 里的 `task seed` 是 not-implemented stub（说明缺什么、如何临时手动演示，退出非零）；`APP_DEMO_USERS_PASSWORD` 播种的演示账户走的是真实注册路由加成员/授权装配，不是装载器。临时手动演示方式见 `examples/reference-app/README.md` 的 "Try it" 一节（也见 [14 示例应用](14-reference-app.md) 的现状注记）。

## 模块生成器

新增一个 Go module 需要八件事：go.mod、目录骨架、`AGENTS.md`、`docs/` 设计文档、迁移目录、测试骨架、CI 矩阵登记、发布登记。其中发布登记不是独立动作——它就是 go.work `use` 条目本身：发布协调器（`tools/release/lockstep-release.py`）在运行时从 go.work 推导每模块 tag 列表，从未登记进 go.work 的模块不可能被打 tag（详见下方注记）。手工做八件事必然遗漏，所以生成器 `tools/new_module.py` 自动完成其中可以安全自动化的部分，其余以**注册清单**逐项提醒，不让任何一件无声漏掉：

```
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/NN-name.md
```

生成器一次产出标准 stub 形态——`go/<name>/` 下的 go.mod（`module github.com/vislake/speed/go/<name>` + 裸 `go 1.23` 指令）、doc.go、`AGENTS.md` 三个文件，仅此而已。它**从不改写共享仓库文件**（go.work、CI 矩阵、发布脚本等）——一个会静默改写 go.work 与 CI 矩阵的脚手架会让评审 diff 不可读，所以注册类事项以清单打印，交给人逐项执行——`task new:module` 只是转调入口，同样只打印清单、绝不代写共享文件。八件事的覆盖情况：

- **go.mod、目录骨架、`AGENTS.md`**：由生成器产出，即 stub 的全部文件。
- **`docs/` 设计文档**：作为 `--design-doc` 输入参数；尚不存在时生成器仅警告、不失败——但 `AGENTS.md` 的 stub 行已经指向它，设计文档必须与该模块同 PR 提交。
- **CI 矩阵登记、发布登记**：出现在注册清单里（连同 go.work `use` 条目与 roadmap/文档导航登记）。这两类登记漏掉不会立即报错——CI 矩阵漏登记会让模块漏跑 CI，正是生成器要兜住的遗漏；发布登记则与清单第 1 项的 go.work `use` 条目是同一件事，见下方注记。
- **迁移目录、测试骨架**：stub 没有迁移也没有测试，生成器不为它们占位空目录；两者随模块的代码实现落地（版本化迁移与测试要求见根 [CLAUDE.md](../../CLAUDE.md)），比骨架阶段占位更贴近真实状态。

**注记：发布登记的语义随 lockstep 发布脚本落地而简化。** 发布协调器（`tools/release/lockstep-release.py`，含其 unittest 套件；入口为 `task release:plan` 与 `.github/workflows/release.yml`）在运行时从 go.work 推导可发布模块集合——**因此清单第 1 项的 go.work `use` 条目本身就是发布登记**，不再存在独立的"每模块 tag 列表"。配套地，模块漏登不会"悄悄漏 tag"：协调器的完备性检查双向核对 go.work 与 `go/` 目录树（`use` 条目缺 go.mod、`go/` 下存在未登记模块都报错退出），漏了任何一项，`task release:plan` 与 release.yml 的发布验证就直接失败。npm 侧的对应物是 `web/.changeset/config.json` 的 fixed group 覆盖集合：新增或移除 npm 包时必须与包列表在同一改动里同步（覆盖不齐同样使发布验证失败）。首次真实发布要执行的"过渡态 replace 行清理"（把模块 go.mod 里的 `replace ... => ../<模块>` 改写为真实版本）只以纯函数 + testdata 夹具形式交付，**严禁对真实 go.mod 运行**——树的过渡态保留到首次真实发布；详见 [02 仓库结构与发布](02-repo-and-release.md) 的注记与 `tools/release/AGENTS.md`。

`task new:module` 是这层脚手架的 Taskfile 包装，已接线转调本脚本（接线契约见脚本 `--help` 的 epilog）：

```
task new:module NAME=<name> DESCRIPTION='...' DESIGN_DOC=docs/internal/NN-<name>.md
```

直接运行上面的 `python3` 命令效果相同。同理的 `task new:npm-package` 也尚未实现——web/ 工作区已有多个包，但还没有 npm 包模板脚手架，脚本的 `--category npm` 目前仍直接拒绝。这是脚手架项目对自己的"脚手架化"——如果我们自己都嫌新增模块麻烦，说明模板设计有问题。

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

**当前状态：前后端两半均已落地，spec-first 闭环真实运转。** 各模块与 reference-app 自持 spec 片段，`task api:gen` 逐一执行钉定的 oapi-codegen（v2.8.0）重新生成后端接口——每个片段同目录携带 `oapi-codegen.yaml` 与生成物；`examples/reference-app/internal/notes/api/openapi.yaml` 是"模块自持 spec 片段"惯例的第一个实例，落在 reference-app 而非 go/ 模块下。片段清单以 `tools/api_fragments.json` 为单一来源，`tools/check_api_fragments.py` 做漂移闸门（树、清单与本工作流各 leg 不一致即红）。"编译失败暴露待改点"真实生效：notes 的 handler 以 `var _ api.ServerInterface = (*Handler)(nil)` 编译期断言实现生成的 interface，并以 `api.HandlerFromMux` 让路由从片段本身推导——往片段加一个 operation 后重新生成，handler 不补实现就编译不过。

前端 leg 依赖 `api:merge`：钉定的 redocly 把合并列表内的片段 `join` 进 `build/openapi/speed.yaml`（现含 notes、cases、smilesim、authn、notification、billing 六个片段；org 与 storage 只有后端 leg，理由见各自 `AGENTS.md` 的 Deferred 小节与 Taskfile `api:merge` 头部注释），钉定的 orval 8.17.0 从合并文档生成 `@speed/api-sdk`（DO-NOT-EDIT 头带钉定版本；经 `pnpm dlx` 从 web/ 运行、永不进入 lockfile）；生成代码不直接触碰网络，经包内唯一手写接缝 `src/runtime.ts` 的 `bindRequestFn` 路由到 api-client 运行时；orval 发射的无扩展名 mutator 导入由 `web/scripts/orval-nodenext-fixup.mjs` 确定性改写为显式 `.js`（nodenext/TS2835，机制细节见该包 AGENTS.md）。

CI 兜底：`.github/workflows/api-contract.yml` 在改动 spec 片段 / 生成器配置（含 `web/orval.config.ts` 与 `web/scripts/**`）/ `Taskfile.yml` / 流水线自身的 PR 上触发（路径过滤），每次再生成后跑 `git status --porcelain --untracked-files=all` 一致性闸门——绝不 `git diff --exit-code`（再生成新建文件会静默通过 diff 闸门）——覆盖每个后端片段 + 合并文档 + 前端 sdk，随后再对 reference-app 跑 `go build`；authn 另设自己的再生成与 build leg（其编译强制不必等 full-ci 才得到回答，理由见该工作流头部注记）。生成面的消费侧：`@speed/auth-core` 编译消费生成操作，`@speed/account-ui` 把生成 hooks 渲染进组件树，`examples/reference-app/web` 作为 host 把整套生成面组合进真实 bootstrap——细节见 [21 API 契约](21-api-contract.md) 的实现状态注记。

尚未落地：oasdiff 破坏性变更闸门（需要首个发布基线作比对对象）；驱动服务器所伺服页面的浏览器自动化（`examples/reference-app/web` 的 `index.html` 与 vite 生产构建已由服务器经 `APP_WEB_DIST` 从磁盘伺服、Dockerfile 打进镜像，缺的是驱动该页的浏览器自动化，html-runner/e2e 见 [15 里程碑路线图](15-roadmap.md) M4 行）。

**先写实现再补 spec 是被禁止的**——那等于回到 code-first，失去编译期约束的全部意义。

## CODEOWNERS

地基模块（`pkgcore`、`dbkit`、`tenancy`）与发布流水线设专属 owner，改动需其批准。这几处的错误会波及所有下游模块与已交付项目，值得多一道关卡。

## 依赖管理

- **Renovate** 自动提依赖升级 PR，按周聚合，安全补丁立即提。
- 主要框架（Go、React、MUI、GORM）的大版本升级单独走 PR 并跑全量矩阵。
- **依赖新增需要理由，并附实测数字**：脚手架的依赖会传导给所有业务项目，新增第三方依赖需在 PR 中说明必要性与替代方案评估。这一条比在普通业务项目里重要得多。若新增的是某个 seam 的一套内置实现，PR 还必须附上它给消费者带来的依赖增量——新建空模块、`require` 目标模块、`go mod tidy`（`GOWORK=off`）、数 `// indirect` 条目，测法与理由见 [03 部署模式与实现组装](03-deployment-modes.md) 的约束 6。
