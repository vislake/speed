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

**`task`/`mise` 二进制本身在标准检出环境里可能未安装**（根 `CLAUDE.md` 已有此说明）：`Taskfile.yml` 与其包装的命令都真实存在且能跑，但 `task` 这个 CLI 本身不一定在 `PATH` 上——遇到时直接跑它包装的原始命令（`go test ./...`、`go vet ./...`、`golangci-lint run ./...` 等），不要假设 `task xxx` 就一定可用，先确认 `task` 在 `PATH` 上。`mise` 同理：`task setup` 的工具链腿在 `mise` 缺席时只警告并跳过，不会失败（见下方「工具链版本统一」一节）。

`task release:plan` 是真实任务（非 stub），离线验证「给定版本号下，全部 Go 模块与 npm 包能按同一版本号一致发布」：它包装 `tools/release/lockstep-release.py` 的默认校验模式——退出码 0 仅当计划一致，不写任何文件。用法：`task release:plan VERSION=v1.2.0`。`.github/workflows/release.yml` 手动触发时先由只读的 `verify` job 运行同一校验并跑协调器自测，再由 `publish` job（job 级 `contents: write` + `packages: write`）执行真实发布：推送模块 tag 与仓库根 tag、把十二个 `@speed` 包发布到 GitHub Packages。真实发布动作里，推 tag 与 npm publish 已接线且已真实执行（首发 v0.0.1 半成功并作废的经过见 `docs/internal/18-cicd.md` 发布流水线一节的实施状态注记）；未接线的仅剩 changesets bump 与 GitHub Release。

`task dev` 必须在**单进程部署模式**下工作：单进程、SQLite、零外部依赖。这是单进程部署模式给开发体验带来的直接收益——本地开发不需要 `docker compose up` 拉起一堆容器。

**`task dev` 尚未实现。** 它与 `task seed` 一样是 not-implemented stub（跑起来会打印说明，退出非零）：「后端 + 前端、单进程部署模式、热重载」的组合式开发循环没有接线——`web/` pnpm workspace 与 `examples/reference-app/web` 这个消费者壳都已存在，但都还没有接进一个热重载 runner。真正能跑起来的是 reference-app 服务器本身：`cd examples/reference-app && go run ./cmd/server`（默认监听 `:8080`，SQLite 落 `./reference-app.db`，两者都可用环境变量覆盖），直接跑这一行即可。

**`task test` 的真实形态：** Taskfile 的 `test` task 就是无条件的 `go test {{.ALL_PKGS}}` 加 reference-app 自己的 `go test ./...`，没有任何差异检测或「只跑改动模块」的逻辑，每次调用都跑全部真实模块的单元测试；无 `-race`、无覆盖率（那两项留给 `task test:full`）。差异感知的「只测受影响模块」没有实现。

### 工具链版本统一

用 **mise**（或 asdf）锁定 Go、Node、pnpm、golangci-lint 等版本，配置文件入库。CI 与本地读同一份配置，杜绝「我本地是好的」。

根目录 `.mise.toml` 用 mise 锁定五个工具：task 3.53.1（唯一来源是 Taskfile 头部注释）、go 1.26.8（镜像 `go.work` 指令）、node 24（镜像 `web/.nvmrc`）、pnpm 11.1.2（镜像 `web/package.json` 的 `packageManager`）、golangci-lint 2.11.4（镜像 setup-go-env 的 `GOLANGCI_VERSION`）。与计划句「CI 与本地读同一份配置」有一个诚实偏差：CI 读不到 `.mise.toml`——`actions/setup-go` 的 go-version-file 只解析 go.mod / go.work / go.sum / .go-version，`setup-node` 只读 `web/.nvmrc`——所以 CI 继续读权威源，`.mise.toml` 是本地 `mise install`（`task setup` 的工具链腿）使用的镜像；两份文件并存必然漂移，因此 `tools/check_toolchain.py` 作为漂移闸门接在 fast-check 的 repo-checks job（每次 PR 都跑），任一镜像与权威源不一致即失败。升版本时权威源与 `.mise.toml` 必须一起改，各工具的来源逐条写在 `.mise.toml` 头部注释里。数据库初始化与 lefthook 预提交钩子仍未实现——原因写在 `task setup` 的注释里。

### 种子数据

`task seed` 生成一套可用的演示数据：两个租户、多层级组织、若干用户与角色、示例套餐与订阅。reference-app 的演示和本地调试都依赖它，必须保持可用（纳入 CI 检查）。

**`task seed` 尚未实现。** reference-app 目前没有任何演示数据装载路径——`internal/app/server.go` 只硬编码了两个演示 Host→租户映射，各表启动时为空。因此 `task seed` 是 not-implemented stub（打印缺什么、如何临时手动演示，退出非零）。

## 模块生成器

新增一个 Go module 需要八件事：go.mod、目录骨架、`AGENTS.md`、`docs/` 设计文档、迁移目录、测试骨架、CI 矩阵登记、发布登记。其中发布登记不是独立动作——它就是 go.work `use` 条目本身：发布协调器（`tools/release/lockstep-release.py`）在运行时从 go.work 推导每模块 tag 列表，从未登记进 go.work 的模块不可能被打 tag。手工做八件事必然遗漏，所以生成器 `tools/new_module.py` 自动完成其中可以安全自动化的部分，其余以**注册清单**逐项提醒，不让任何一件无声漏掉：

```
python3 tools/new_module.py NAME --description '...' --design-doc docs/internal/NN-name.md
```

生成器一次产出 `go/<name>/` 下的 go.mod（`module github.com/vislake/speed/go/<name>` + 裸 `go 1.23` 指令）、doc.go、`AGENTS.md` 三个文件，仅此而已——这就是 stub 的完整形态。它**从不改写共享仓库文件**（go.work、CI 矩阵、发布脚本等）——一个会静默改写 go.work 与 CI 矩阵的脚手架会让评审 diff 不可读，所以注册类事项以清单打印，交给人逐项执行。`task new:module` 只是转调入口，同样只打印清单、绝不代写共享文件。八件事的覆盖情况：

- **go.mod、目录骨架、`AGENTS.md`**：由生成器产出，即 stub 的全部文件。
- **`docs/` 设计文档**：作为 `--design-doc` 输入参数；尚不存在时生成器仅警告、不失败——但 `AGENTS.md` 的 stub 行已经指向它，设计文档必须与该模块同 PR 提交。
- **CI 矩阵登记、发布登记**：出现在注册清单里（连同 go.work `use` 条目与 roadmap/文档导航登记）。这两类登记漏掉不会立即报错——CI 矩阵漏登记会让模块漏跑 CI，正是生成器要兜住的遗漏；发布登记则与清单第 1 项的 go.work `use` 条目是同一件事。
- **迁移目录、测试骨架**：stub 没有迁移也没有测试，生成器不为它们占位空目录；两者随模块的实现一起落地（版本化迁移与测试要求见根 [CLAUDE.md](../../CLAUDE.md)），空占位反而比骨架阶段更偏离真实状态。

发布协调器（`tools/release/lockstep-release.py`，含其 unittest 套件；入口为 `task release:plan` 与 `.github/workflows/release.yml`）在运行时从 go.work 推导可发布模块集合，因此清单第 1 项的 go.work `use` 条目本身就是发布登记，不存在独立的「每模块 tag 列表」。协调器的完备性检查双向核对 go.work 与 `go/` 目录树：`use` 条目缺 go.mod、`go/` 下存在未登记模块都报错退出——漏了任何一项，`task release:plan` 与 release.yml 的发布验证直接失败。npm 侧的对应物是 `web/.changeset/config.json` 的 fixed group 覆盖集合：新增或移除 npm 包时必须与包列表在同一改动里同步（覆盖不齐同样使发布验证失败）。各模块 go.mod 目前带过渡态 replace 行（`replace ... => ../<模块>`）；把 replace 行改写为真实版本的清理只以纯函数 + testdata 夹具形式存在，**严禁对真实 go.mod 运行**。

`task new:module` 是这层脚手架的 Taskfile 包装，已接线转调本脚本（接线契约见脚本 `--help` 的 epilog）：

```
task new:module NAME=<name> DESCRIPTION='...' DESIGN_DOC=docs/internal/NN-<name>.md
```

直接运行上面的 `python3` 命令效果相同。npm 包模板由同一脚本的 `--category npm` 提供：它把十二个已发布 `@speed` 包共有的公共核心骨架物化到 `<target-dir>/web/packages/<name>/`（package.json、tsconfig.json、tsconfig.build.json、src/index.ts、src/index.test.ts、README.md、AGENTS.md），即一个尚无 API 的包通过四个 npm-package-ci 腿（pnpm lint/typecheck/test/build）所需的文件；Taskfile 的 `task new:npm-package` 包装尚未接线，接线后与 `new:module` 包装 go 类别的关系相同。这是脚手架项目对自己的「脚手架化」——如果我们自己都嫌新增模块麻烦，说明模板设计有问题。

`--category app` 面向的是另一半场景：往一个**已存在的应用**里加应用侧业务模块，产物是参考应用 notes 模块的同一形态（doc.go / model.go / repository.go / handler.go 与编译期 `api.ServerInterface` 断言 / handler_test.go / 双方言迁移对 / 双语 locale 对 / OpenAPI 片段与其 oapi-codegen 配置），落地在 `<应用根>/internal/<name>/`；应用根由 `--target-dir` 指定（必须含应用的 go.mod），脚本读取它推导模块的 import 路径，保证物化出来即可编译。生成器**不**写 `api/<name>-server.gen.go`——那是 pinned 生成器的产物，清单第一步给出与仓库 api 生成腿同一版本的 oapi-codegen 命令；物化 + 生成 + `go build` + `go test` 走通一次，就是这一类别形状的验证。脚手架的双语 locale 模板以真实文件形式放在 `tools/new_module_locales/`（tools/ 是英文区，中文文案不能内联在脚本源码里），顺带被 `tools/check_i18n_keys.py` 的键集奇偶校验覆盖。

## 分支与合并策略

采用 **trunk-based**：短生命周期分支（建议不超过 3 天），频繁合入 `main`。

**Git 规范（团队既定，严格执行）**：
- **合并前必须先 rebase 到目标分支**，保持线性历史
- **只允许 fast-forward 合并**（`git merge --ff-only`）；无法 ff 时先 rebase，不用 merge commit

线性历史对本项目有额外价值：lockstep 版本下需要频繁在历史中定位「某个版本包含哪些改动」，非线性历史会让 `git log` 与二分排查变得困难。

## 提交规范

**Conventional Commits**，作用域用模块名。`type` 与 `scope` 用英文；描述部分的语言见 `.claude/skills/commit-convention/SKILL.md`：

```
feat(tenancy): 支持子域名解析租户
fix(billing): 修复信用点并发扣减超扣
docs(jobs): 补充 worker 内租户上下文重建说明
chore(ci): 缓存 golangci-lint 结果
```

驱动三件事：changelog 自动生成、破坏性变更识别（`!` 或 `BREAKING CHANGE`）、PR 标题校验。

**Pre-commit hooks（lefthook）尚未实现**；设计意图是格式化、快速 lint、提交信息校验。重活留给 CI，本地钩子必须秒级完成，否则会被绕过。

## PR 要求

PR 模板包含一份 checklist，对应仓库根 [CLAUDE.md](../../CLAUDE.md) 「Architecture Discipline」的纪律：

- [ ] 新增 Repository 已跑 `tenancytest.AssertIsolated`
- [ ] 新增基础设施依赖已提供至少一套零外部依赖的实现，且每套实现都声明能力并通过该模块的契约测试
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

改任何对外接口都必须按固定顺序：**先改 spec → 重新生成 → 编译失败暴露待改点 → 补实现 → 补前端 → 同一个 PR 提交**。**先写实现再补 spec 是被禁止的**——那等于回到 code-first，失去编译期约束的全部意义。

`task api:gen` 一键完成「合并 spec + 生成后端 interface + 生成前端 sdk」，其各 leg 与 api-contract.yml 的再生成步骤一一对应、保持 lockstep。PR 中 spec 与生成物必须同时存在，CI 会重新生成并做一致性比对——spec-first 闭环真实运转，现状如下：

- **后端片段**：`task api:gen` 与 api-contract.yml 覆盖十一个平台模块 spec 片段——org、storage、authn、notification、billing、sharing、pki、admin、integration、ai-gateway、config；片段清单以 `tools/api_fragments.json` 的 `fragments` 为单一来源（同一清单的 `app_owned` 列出 reference-app 自有的 notes、cases、smilesim——它们归应用自有生成流，见下），`tools/check_api_fragments.py` 做漂移闸门（树、清单与本工作流各 leg 不一致即红）。片段按 `<module>/api/openapi.yaml` 惯例组织，生成器配置与生成物同目录；reference-app 自己的片段落在应用内而非 go/ 模块下（notes 在 `examples/reference-app/internal/notes/api/openapi.yaml`，其 `oapi-codegen.yaml` 钉定 oapi-codegen v2.8.0，生成 `notes-server.gen.go`，由 `task api:gen:app` 再生成）。后端 leg 在片段目录内执行 `go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0 -config oapi-codegen.yaml openapi.yaml`，逐片段重新生成。
- **「编译失败暴露待改点」真实生效**：handler 以 `var _ api.ServerInterface = (*Handler)(nil)` 编译期断言实现生成的 interface，并以 `api.HandlerFromMux` 让路由从片段本身推导——往片段加一个 operation 后重新生成，handler 不补实现就编译不过。
- **多片段合并与 lint**：仓库根 `redocly.yaml` 定义合并规则与命名规范 lint；`task api:merge` 与 api-contract.yml 用钉定的 `@redocly/cli@2.51.1` 的 `join` 命令把片段合并进 `contracts/speed.yaml` 并按 `redocly.yaml` 的规则 lint。合并文档现为十一个平台模块片段（admin、ai-gateway、authn、billing、config、integration、notification、org、pki、sharing、storage）；凡带 HTTP 片段的平台模块一律进合并文档（模块驱动策略，见 docs/internal/21-api-contract.md），orval 前端 leg 依赖合并结果，从该文档生成 `@speed/api-sdk`。reference-app 自有的 notes、cases、smilesim 不进平台合并——`task api:gen:app` 把它们并入应用自有合并文档 `examples/reference-app/web/app-openapi.yaml` 并生成应用自有 SDK（`src/app-api`），门禁在 full-check 的 reference-app job（见 docs/internal/21-api-contract.md 的应用自有生成流节）。
- **前端 sdk**：`@speed/api-sdk`（`web/packages/api-sdk`；DO-NOT-EDIT 头带钉定版本）由钉定的 orval 8.17.0 生成 hooks 与 TS 类型；`task api:gen` 的前端 leg 执行 `cd web && pnpm dlx orval@8.17.0 --config orval.config.ts && node scripts/orval-nodenext-fixup.mjs`——`pnpm dlx` 使 orval 永不进入 lockfile。生成代码不直接触碰网络：它经包内唯一手写绑定 `src/runtime.ts`（`bindRequestFn(createClient(...))`，由宿主在启动时绑定）路由到 api-client 运行时；orval 发射的无扩展名 mutator 导入由 `web/scripts/orval-nodenext-fixup.mjs` 确定性改写为显式 `.js`（nodenext 构建拒绝无扩展名相对导入，TS2835）。api-sdk 进入 fast-check 的 npm 矩阵，并与合并文档一起受 api-contract.yml 的一致性闸门覆盖。
- **CI 兜底**：`.github/workflows/api-contract.yml` 在改动平台 spec 片段 / 生成器配置（含 `web/orval.config.ts` 与 `web/scripts/**`）/ `Taskfile.yml` / 流水线自身的 PR 上触发（路径过滤）。每次再生成后的一致性闸门是 porcelain 形态（再生成新建文件会静默通过 diff 闸门，故用状态检查）——共十三道（十一个平台后端片段 + 合并文档 + 前端 sdk），随后再对 reference-app 跑 `go build` 保证平台 handler 跟上 spec（authn 另设自己的再生成与 build leg）。应用自有产物（三片段后端 gen、应用合并文档、应用 SDK）由 full-check 的 reference-app job 跑 `task api:gen:app` 同一组命令并做同形态闸门。

生成面有真实的消费证明，分三个层次：

- **in-workspace compile consumer**：`@speed/auth-core`（`web/packages/auth-core`）是 api-sdk 生成面的 in-workspace compile consumer——其单元套件经 `bindRequestFn` 绑定 scripted `RequestFn` 驱动生成操作并做类型检查，`src/usage-example.test.tsx` 把 README 用法编译执行，生成层由此有真实消费者做类型检查。
- **包级 in-form 的运行时端到端消费**：`@speed/auth-ui`（`web/packages/auth-ui`，登录组件家族：`SignInScreen`/`PasswordSignInForm`/`SMSSignInForm`/`RegisterForm`/`SocialSignInSection`/`SocialCallbackHandler`/`SignOutButton`/`SessionEndedScreen`）的 `src/usage-example.test.tsx` 用真实 `@speed/api-client`（createClient + 内存 access-token store + 可注入 fetch；fetch 替身以真正的 `Response` 对象作答）经同一 `bindRequestFn` 绑定，编译并执行 README quick start 的组合——密码登录 → 受保护请求（过期的 access token）以 `authn.token_expired` 被拒 → 静默刷新 → 服务端会话死亡（`authn.session_revoked`）→ 收敛匿名 → 再次登录 → `switchLanguage` 到 en-US，六次请求顺序钉死。
- **host 级组合消费**：`examples/reference-app/web` 是 reference-app 的 web 宿主，位于 `web/` pnpm workspace 之外、恰在交付型 consumer project 的位置（workspace 外部成员，private，永不 versioned，不进入 changesets fixed group）。其 `src/main.tsx` 的 `bootstrapReferenceApp` 组合全部 host 契约：注册七个命名空间（六个包族加 reference-app 自己的 bundle）、memory-only auth-core 会话、应用唯一的 api-client（环境自带的 fetch，会话静默刷新接 401-refresh leg）一次绑定进 api-sdk 的 `bindRequestFn` 绑定点、ProductShell 视图机挂进 AppShell 框架。notes 半边经生成的 react-query hooks 行使，读取走**租户命名空间的 query key**（叠加在 api-sdk 生成的裸 spec-path key 之上——命名空间是壳纪律，不是生成物）；跨路由门禁真实存在：notes 视图 `RouteGuard` 的 status 派生自 notes list 这一 permission fetch 本身，服务器 rbac 层对无 `notes:read` 的调用方回答 403 `rbac.permission_denied`，门禁对拒绝 fail closed；authn 半边经 auth-core 会话与 auth-ui/account-ui/tenancy-ui 组件面行使，在同一 bound client 之下。壳带真实 `index.html` 与 vite 生产构建：reference-app 服务器经 `APP_WEB_DIST` 从磁盘伺服该构建，Dockerfile 打进镜像，壳的 bootstrap 由页面挂载。

尚未实现、如实披露的边界：

- **oasdiff 破坏性变更闸门**：不存在。破坏性变更检测需要发布基线才有比较对象——v0.0.1 作废，但其发布提交 `fbaaaf98` 之树仍提供完整基线（main 的祖先；Go module proxy 只解析 21 个模块中的 17 个）；闸门的缺席是如实披露的机制决策，而非以假闸门占位。
- **浏览器自动化**：Playwright 套件（`examples/reference-app/web/e2e`，27 个 spec 文件、chromium/webkit/ipad 三个项目）已驱动真实宿主；`.github/workflows/e2e.yml` 以按需触发加每日定时运行，加上每次推送到 main（push 触发已按既定条件开启——以首次全绿的 dispatched 运行为据，即 2026-09-11 的第三次 dispatched 运行；触发细节与理由见 `.github/workflows/e2e.yml` 文件头）。`@deployment` 门在指向真实部署时（需 `E2E_BASE_URL`）驱动该部署的伺服页面。

## 文件拆分与移动的引用清扫

拆分或移动文件（拆包、改名、换目录）会同时打断仓库里一批不以 import 关系表达的引用：闸门里硬编码的路径、工作流的触发集合与步骤引用、allowlist 与禁调表、清单文件、Dockerfile 的 `COPY` 列表——这些引用不会自动跟随，只会在下一次 CI 运行时逐层暴露。因此每一轮拆分/移动都必须做一次全仓引用清扫（旧文件名一轮、旧符号名一轮），并把逐条处置作为该轮的验收项。

**旧文件名**——对每个被移动、改名或拆走内容的路径逐一核对下列面（先做一次仓库根全仓 `grep -rn` 兜底，再逐面确认）：

- **`tools/**`**：按路径引用文件的 checker 与 generator——硬编码的输入路径（如 `check_env_example_consistency.py` 的声明点常量、`check_host_composition.py` 的命名 allowlist 条目）与随附清单（`api_fragments.json`、`migration_parity_exceptions.json`、`coverage-baselines.json`、`dependency-licenses.json` 的 `used_by`）。
- **`.github/workflows/**`**：两处都要查——触发器的 `paths:` 集合（目录与文件逐行枚举），与步骤体的脚本引用（`run: python3 tools/...` 及其参数、头部注释里的成员枚举行）。
- **`tools/semgrep_rules/**`**：每条规则的 `paths.exclude` allowlist——旧路径留在名单里成为死条目，新路径漏配则在规则应豁免处直接报红。
- **根 `.golangci.yml`**：depguard 等规则的 `files:` glob 与允许/禁止的导入路径。
- **`examples/reference-app/Dockerfile` 与根 `.dockerignore`**：`COPY` 列表必须与 go.work 的 `use` 集合同构（workspace 成员目录缺一即构建失败）；`.dockerignore` 的排除规则核对是否仍对准真实路径。
- **`tools/README.md`**：每个脚本的专节与「Running in CI and locally」清单。
- **文档锚点**：`docs/internal/**`、`docs/site/**`、各 `AGENTS.md` 与 README、`.claude/skills/**` 中引用旧路径的行。

**旧符号名**——对被改名或抽取的标识符，在禁/允名单位置 grep：

- `tools/semgrep_rules/*.yml` 的匹配模式与 `paths.exclude`，及其 `testdata/` 的 `positive.go` / `negative.go` 夹具——夹具与真实代码一起老化会让规则静默失效：`semgrep_fixture_check.py` 只证明规则会触发在自家夹具上，不证明夹具仍像真实代码。
- `tools/check_host_composition.py` 的禁调符号表与「文件 + 符号」命名 allowlist。
- `.golangci.yml` 的导入路径 deny/allow。
- `tools/semgrep_fixture_check.py` 活性核对涉及的环境变量名。

**生成物重新生成，不手改**：凡嵌入路径或「文件:行号」的生成物，改完跑生成器并过 `--check` 复核——错误码索引页（`python3 tools/gen_error_code_index.py`，其行含构造点 `文件:行号`，配套 `check_error_code_index_coverage.py`）。

**验收**：逐条列出两轮 grep 的每个命中及处置（已更新 / 核实不受影响），并反向核对新路径、新符号已在应出现的位置补齐——拆分出的新文件最常漏在 allowlist、`COPY` 列表与触发集合之外；只写「已清扫」不列条目不算通过。

## CODEOWNERS

地基模块（`pkgcore`、`dbkit`、`tenancy`）与发布流水线设专属 owner，改动需其批准。这几处的错误会波及所有下游模块与已交付项目，值得多一道关卡。

## 依赖管理

- **Renovate** 自动提依赖升级 PR，按周聚合，安全补丁立即提。
- 主要框架（Go、React、MUI、GORM）的大版本升级单独走 PR 并跑全量矩阵。
- **依赖新增需要理由，并附实测数字**：脚手架的依赖会传导给所有业务项目，新增第三方依赖需在 PR 中说明必要性与替代方案评估。这一条比在普通业务项目里重要得多。若新增的是某个模块的一套内置实现，PR 还必须附上它给消费者带来的依赖增量——新建空模块、`require` 目标模块、`go mod tidy`（`GOWORK=off`）、数 `// indirect` 条目。
