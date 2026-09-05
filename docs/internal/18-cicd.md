# CI/CD 流水线

> 基于 GitHub Actions。本文档定义流水线的职责划分、触发时机与成本控制策略。
>
> 核心约束：**Go module 与 npm 包不能各写一套 workflow**。全部通过可复用 workflow 与 composite action 统一，否则 CI 配置本身会成为最大的维护负担。

## 流水线总览

| 流水线 | 触发 | 职责 | 目标时长 |
|---|---|---|---|
| `pr-check` | PR 打开/更新 | 受影响模块的 lint + 类型检查 + 单元测试 + 构建 | < 8 分钟 |
| `pr-full` | PR 打 `full-ci` 标签 / 合入前 | 全量矩阵：每个 seam 的契约测试 × 该 seam 的每套实现（[16-verification](16-verification.md) §2 的矩阵形态），双方言，外加代表性整机组装冒烟 | < 25 分钟 |
| `e2e` | 合入 main / 每日 | reference-app 端到端（Playwright） | < 20 分钟 |
| `security` | PR + 每日 | 依赖漏洞、密钥扫描、SAST、镜像扫描、许可证检查 | < 10 分钟 |
| `docs-check` | 涉及文档或公开 API 的 PR | 文档示例编译运行、链接检查、i18n key 一致性、配置清单漂移 | < 5 分钟 |
| `api-contract` | 涉及 `api/openapi.yaml` 或 handler 的 PR | spec lint、合并冲突检查、**生成物一致性 diff**、oasdiff 破坏性变更检测（**未接线，见下方实施状态注记**——待首个发布基线，计划 M4，与 security 行的 govulncheck/trivy 同属"暂缓"而非遗漏） | < 5 分钟 |
| `scaffold-verify` | 每日 + 发布后 | CLI 生成全新项目 → 构建 → 两种部署模式各启动一次 → 冒烟 | < 15 分钟 |
| `release` | 手动触发（指定版本号） | lockstep 全量发布：Go module 逐个打 tag + npm 包逐个发布 + 镜像 + CLI 二进制 | < 30 分钟 |
| `nightly` | 每日 | 全量矩阵 + 性能基准回归 + flaky test 检测 | 不限 |

> **注记（2026-09-04，issue #1 子包拆分轮次已解决——本轮核实）：** 本条曾记录的问题——`.golangci.yml` 三条 SDK 规则的放行写法是 `!**/go/pkgcore/**` 与 `!**/go/jobs/**`，放行**整个模块连同它的全部子包**，导致 `pkgcore` 根包内联 Redis 与 S3 实现（`redis_kv.go`、`s3_objectstore.go` 与接口同包）在规则下完全合规、"每个消费者无条件继承 go-redis 与 minio-go"从未被任何检查报出——**已经修复**：`go/pkgcore` 的 Redis/S3 实现已迁入 `go/pkgcore/eventbus/redis`、`go/pkgcore/kv/redis`、`go/pkgcore/objectstore/s3` 三个实现子包，`go/jobs` 的 asynq 实现迁入 `go/jobs/queue/asynq`，`.golangci.yml` 的 `redis-only-in-pkgcore-and-jobs`/`minio-only-in-pkgcore`/`asynq-only-in-jobs` 三条规则的放行路径也随之收紧到 `!**/go/pkgcore/kv/redis/**` 这样的实现包本身，不再放行整个模块——谁再把 SDK import 写回根包或另一个实现包，CI 现在会直接失败，这正是拆分工作当初设定的验收条件。现状与推演见 `.golangci.yml` 自己的 depguard 注释；issue #1 的其余站点仍按各自轮次推进。

> **实施状态注记（2026-09，security 轮次）**：上表是设计规格，不是现状。当前真实落地的流水线：`pr-check`（每个 PR）、`pr-full`（打 `full-ci` 标签的 PR）、`docs-check` 与 `api-contract`（按路径过滤触发），以及本轮由 gated stub 转正的 `security`（每个 PR + 每日 05:37 UTC 定时；触发、职责与暂缓项的解除条件见 `.github/workflows/security.yml` 文件头）。`release` 在 release-foundation 轮次转正（此后手动触发）；`e2e`、`nightly`、`scaffold-verify` 仍是 gated stub，由各自轮次接手——scaffold-verify 的文件头已由 saasctl 轮更新：`go/saasctl` v0.1 交付了该流水线的生成侧，`saasctl new` 的真实离线端到端证明（生成→tidy→build→boot→冒烟）逐项记录在 `go/saasctl/AGENTS.md` Testing 章节，但两种部署模式各 boot 一次与每日/发布后触发仍是 M4 门，stub guard 保持原样（见 `.github/workflows/scaffold-verify.yml` 文件头与 [16 验证方式](16-verification.md) §5 的注记）。security 行的设计内容逐项核对：pnpm audit、gitleaks、CodeQL 与许可证扫描已接线；依赖漏洞扫描（Go 侧 govulncheck）与镜像扫描（trivy）暂缓——govulncheck 在当前树有两处独立原因必然失败（标准库公告属于 go.work `go 1.25.0` 工具链线、需 toolchain-pinning 轮抬升 go 指令才能清零；模块类公告经 testcontainers 的测试支撑依赖与 exporter 依赖触达、属各模块自己的升级轮），trivy 在仓库产出镜像前无物可扫——两处的证据与解除条件都在 security.yml 文件头的 DEFERRED 节。

## 成本控制：不是每个 PR 都跑全量

模块数量 × 各 seam 的实现套数 × 2 方言，相乘后组合数太大，每个 PR 全跑既慢又贵。策略：

1. **路径过滤**（`dorny/paths-filter`）：只跑改动模块及其**下游依赖**模块。依赖关系从 `go.work` 与 workspace 配置自动推导，不手工维护映射表。
2. **分层触发**：PR 阶段跑快速检查（全进程内实现 + SQLite，无需容器）；合入前跑全量矩阵。这利用了进程内实现的一个副产品优势——大部分测试不需要 testcontainers 就能跑。
3. **缓存**：Go module cache、pnpm store、Docker layer、golangci-lint cache 全部启用，按 lockfile 哈希做 key。

> **实施状态注记（本轮核实）：** 第 1、3 条都还是设计意图，尚未落地。`pr-check.yml` 自己的文件头明确写着 `dorny/paths-filter` 未接入——现存模块数量还不大，每个 PR 目前无差别跑全部真实模块，等模块集合变大再引入路径过滤，且已经预留了"下游依赖推导"的落点（依托可复用的 `go-module-ci` workflow，接入时只改调用方，不改被调用的可复用 workflow 本身）。Docker layer 缓存同样没有对应设施——仓库里唯一涉及镜像构建的 workflow 是 `.github/workflows/reusable-docker-build.yml`，一个尚无人调用、guard step 直接失败的 gated stub，等第一个产出镜像的应用出现才会真正用到 Docker layer 缓存。第 2 条（分层触发）与 Go module cache / pnpm store / golangci-lint cache 三项缓存是真实落地的，与本条注记不冲突。
4. **并发控制**：同一 PR 的新推送自动取消旧运行（`concurrency` + `cancel-in-progress`）。
5. **超时**：每个 job 设 timeout，防止挂死消耗额度。

## 可复用 workflow 设计

```
.github/
  workflows/
    pr-check.yml                    # 编排：调用下面的可复用 workflow
    release.yml
    ...
    reusable-go-module-ci.yml       # 输入：模块路径 → lint/test/build
    reusable-npm-package-ci.yml     # 输入：包路径 → lint/typecheck/test/build
    reusable-docker-build.yml       # 多架构镜像构建
  actions/
    setup-go-env/           # 统一 Go 版本 + 缓存
    setup-node-env/         # 统一 Node/pnpm 版本 + 缓存
```

新增一个模块时，只需在编排 workflow 的矩阵列表里加一行，不用复制粘贴几十行 YAML。

## 架构纪律的自动化检查

[CLAUDE.md](../../CLAUDE.md) 「Architecture Discipline」一节里的每条纪律都必须有对应的自动检查，否则形同虚设：

| 纪律 | 检查手段 |
|---|---|
| 禁止绕过 `Repository[T]` 直接用 `db.Table/Model/Raw` | `semgrep` 自定义规则 |
| 业务逻辑中禁止 `if mode == "standalone"` 分支 | `semgrep` 规则（残余风险探测器；文件级 allowlist 仅两个 kernel 装配模式决策点：`deployment_mode.go` 的必需能力分发与 reference-app 入口的 env 读取） |
| 业务模块间禁止跨模块 import struct | `go-arch-lint` 或 `depguard`（golangci-lint 插件） |
| `rbac` 不得依赖 `authn` | `depguard` 依赖白名单 |
| 业务代码不得 import 具体基础设施 SDK | `depguard`（禁止 `go-redis`、S3 SDK 等出现在业务模块）。**放行路径必须是实现所在的包，不是它归属的模块**——理由见下方注记 |
| `tenant_id` 不得作为 Prometheus label | `semgrep` 规则 + 运行时断言测试 |
| UI 包内禁止裸文本节点 | `eslint-plugin-i18next` 或自定义 ESLint 规则 |
| 中英资源 key 集合必须一致 | 自研脚本，diff 两份 JSON/TOML 的 key 集合 |
| 除 `docs/internal/` 外禁止 CJK 字符 | 自研脚本扫描 `.md` 与代码注释（i18n 资源与 `docs/site/` 本地化目录除外） |
| 日志消息必须是常量字符串 | `semgrep`：禁止 `log.*(fmt.Sprintf(...))` 与字符串拼接作为消息 |
| 日志字段名规范 | `semgrep`：属性 key 必须是 `snake_case` 字面量 |
| 前端禁止手写 API 调用 | ESLint 自定义规则：除 `@speed/api-client` 内部外禁止 `fetch`/`axios` 指向后端路径 |
| spec 与实现必须一致 | 生成的 server interface 参与编译；CI 重新生成并 diff，不一致即失败 |
| operationId / schema 命名规范 | redocly lint 自定义规则 |
| 系统上下文只能由白名单模块调用 | 人工评审 + 函数级文档约定；**不是** `depguard`——`WithSystemContext` 与 `TenantID`/`WithTenant`/`apperr` 同属 `pkgcore` 根包同一个 import path，depguard 只能按包路径粒度放行/拒绝，做不到只挡一个符号。已实测验证：把「仅 admin/compliance/jobs/authn/`tenancy` 可 import `pkgcore`」接成 depguard 规则，会连带拦下 `go/dbkit`（真实代码、不在白名单、但合法依赖 `TenantID` 等）23 处无关导入，草稿规则因此未合入。`tenancy` 现已建成，提供审计封装版 `tenancy.WithSystemContext`，业务代码应调用它而非直接调用原语；要让这条纪律真正可静态检查，需要先把 `WithSystemContext` 迁到 `pkgcore` 独立子包（类似 `apperr/`、`config/`），这是一次公开 API 决策，超出本表列出的自动化检查范围。仓库里目前没有任何 CODEOWNERS 文件（`go/pkgcore`、`go/tenancy` 均无）——这是一处已知的真实实现缺口，白名单纯靠人工把关 |
| 禁止手写 `WHERE tenant_id = ?` | `semgrep`：租户过滤只能由插件与 Repository 注入，手写即意味着绕过防护 |
| API 层不得接受外部传入的 `tenant_id` | `semgrep` + spec lint：请求参数/请求体中出现 `tenant_id` 字段即拒绝 |
| 禁止 `AutoMigrate` | `semgrep`：生产迁移必须是版本化 SQL |
| 禁止跨模块数据库外键 | 迁移文件 lint：`REFERENCES` 目标表必须属于同一模块 |
| 日志与响应不得输出明文 PII | `semgrep` 检查敏感字段直接进日志；配合脱敏中间件的单元测试 |
| `@speed/api-sdk` 不得手改 | 生成物 diff（改了会被下次生成覆盖，CI 提前拦截） |
| 每个 Repository 必须跑隔离测试 | 自研脚本：扫描 Repository 实现，比对测试覆盖清单 |
| 每个模块的 `go.mod` 必须能脱离 `go.work` 独立构建 / `tidy`（否则 `replace` 了却漏加 `require` 这类问题会被 workspace 的隐式路径解析掩盖，只有真实消费方或首次 lockstep 发布删掉过渡期 `replace` 行后才会暴露，见 [02 仓库结构与发布](02-repo-and-release.md)） | 过渡手段：`go/tenancy` 的 `TestModuleBuildsStandaloneOutsideWorkspace`（`standalone_build_test.go`）用 `os/exec` 以 `GOWORK=off` 跑一遍该模块自己的 `go build ./...`/`go vet ./...`，随 `go test ./...` 默认执行，不依赖任何 CI 配置；真正的 CI 落地后应替换为流水线里对每个模块单独执行的 `GOWORK=off` 构建步骤 |

**这张表是 CI 的核心价值所在**——纪律靠人记会在三个月后失效，靠 CI 才能长期有效。

> **实施状态注记（2026-09，security 轮次）**：表中由 `semgrep` 承担的行已有六条规则落地（本轮新增），见 `tools/semgrep_rules/`——`raw-gorm-bypass.yml`（第 1 行 `db.Table/Model/Raw`，path allowlist 放行 go/dbkit 与 go/jobs 的合法存储层文件）、`deployment-mode-branch.yml`（第 2 行，按值匹配模式比较与 `SPEED_DEPLOYMENT_MODE` 读取，仅放行两个 kernel 装配模式决策点：`go/pkgcore/deployment_mode.go` 的必需能力分发与 reference-app 入口的 env 读取）、`tenant-id-metric-label.yml`（第 6 行，配合 observability 既有的标签断言测试）、`non-constant-log-message.yml`（第 10 行）、`handwritten-tenant-id-filter.yml`（第 16 行，放行 go/dbkit 与 go/jobs 存储层）、`gorm-automigrate-ban.yml`（第 18 行，未来防线，当前零真实调用点）。每条规则的文件头写明：对应的纪律行、命中形状、path allowlist 与放行理由、残余缺口；配套 planted-violation fixture（`testdata/<规则>/{positive,negative}.go`）证明规则真的会响。六条规则随每个 PR 在 pr-check 的 repo-checks job 运行（临时 venv 安装 semgrep，扫 `go/`、`examples/`、`tools/` 三棵子树，fixture 子树在 CLI 层排除；CI 首绿前版本故意不 pin），运行方式与执行状态见 `tools/README.md`。两点如实披露：其一，semgrep 对 `examples/reference-app/internal/notes/repository.go:19`（内嵌实例化泛型 `*dbkit.Repository[Note]`）始终抛 PartialParsing 异常、该行不参与分析——它是结构体嵌入声明，与六条规则的命中形状均无关，暂无盲区，但后续新增规则必须知道这个文件扫不全；其二，各规则文件头列出的残余缺口（动态拼装、别名间接引用等文本匹配不到的形态）仍由 code review 兜底。depguard 侧，第 5 行「业务代码不得 import 具体基础设施 SDK」本轮落地三条规则（redis / minio / asynq）——**这三条规则随后又被 issue #1 子包拆分轮次进一步收紧**：放行粒度从本轮落地时的整模块（`!**/go/pkgcore/**`、`!**/go/jobs/**`）改为各自实现归属的子包（`!**/go/pkgcore/kv/redis/**` 等），详见本文档开头的注记——rules 与 files-list allowlist 的取舍记录在 `.golangci.yml` 的 depguard 注释里；第 15 行「系统上下文白名单」维持评审制——该行已写明 depguard 做不到符号粒度、草稿规则为何未合入，与落地现状一致。此前轮次已落地的行：第 7 行（ESLint `no-literal-text` 规则在 npm-package-ci 每包 lint 腿运行）、第 8 行（中英 key 一致性在 docs-check）、第 9 行（CJK 扫描在 repo-checks）、第 13 行（spec 一致性在 api-contract）、第 23 行（`GOWORK=off` 独立构建在 go-module-ci 第 5 腿，过渡期 `standalone_build_test.go` 保留本地执行）。其余行——跨模块 import struct 与 `rbac` 依赖 `authn`（go-arch-lint / 依赖白名单）、日志字段名规范、前端手写 API 调用、API 层接受 `tenant_id` 参数、跨模块外键迁移 lint、`@speed/api-sdk` 禁改、Repository 隔离测试覆盖脚本——的自动化仍是未来轮次，不在此虚报；PII 直入日志行的运行时一半（脱敏中间件与它的单元测试）已在 go/observability 落地，缺的是 semgrep 静态检查那一半。

> **实施状态注记（2026-09-03，authn 轮次；片段顺序已按本轮核实更正）**：上一条注记里点名"仍是未来轮次"的 operationId / schema 命名规范一行，本轮已落地——`go/authn/api/openapi.yaml` 触发了合并的存在意义（它不是继 notes 之后的第二个模块 spec 片段，`go/org/api/openapi.yaml` 更早落地，只是 org 的片段当时不参与合并，详见 [19 开发工作流](19-dev-workflow.md) 的同一处更正）：仓库根目录的 `redocly.yaml` 定义了 `rule/speed-operation-id-format`、`rule/speed-schema-name-format` 两条 error 级命名规范规则，`Taskfile.yml` 的 `task api:merge` 与 `.github/workflows/api-contract.yml` 用钉定的 `@redocly/cli@2.51.1` 把两个片段 `join` 进 `build/openapi/speed.yaml` 后按这两条规则 `lint`，`git diff --exit-code` 校验该文件与提交版本一致。详见 [19 开发工作流](19-dev-workflow.md) 与 [21 API 契约](21-api-contract.md) 各自的实现状态注记。

> **实施状态注记（2026-09，deployment-composition 轮次更新）**：第 2 行规则随改造转为残余风险探测器——部署模式从选择器变成约束（`NewKernel` 不再收模式、四个 seam 从各自的实现注册表解析带能力声明的实现），`registry.go`、observability `init.go`、reference-app `main.go` 的模式分支被移除，allowlist 相应删到上一段句读的两个决策点；按 `Capability` 分支（如 `caps.Has(MultiReplicaSafe)`）是改造引入的新词汇，尚无纪律行，规则头与 `tools/README.md` 的残余缺口列明确记录为不覆盖。pr-full 的真实矩阵随之对齐 [16-verification](16-verification.md) §2 的形态：pkgcore 的 Docker 腿把三套有真实后端的 seam 契约测试（`kvstoretest`/`eventbustest` 对真实 Redis、`objectstoretest` 对 MinIO/S3）各跑一遍——mailer 的 SMTP 实现无容器后端，同一套 `mailertest.AssertConforms` 在单元层对进程内 fake SMTP server 跑；reference-app job 新增其 Docker-backed 组装腿（真实 Redis `EventBus` 注入、standalone 拓扑启动整机——正是 §2 的"代表性整机组装冒烟"），Taskfile `test:full` 同步镜像。

## 发布流水线（lockstep）

手动触发并输入版本号（如 `v1.2.0`），流水线按序执行：

1. **前置校验**：main 分支绿灯、工作区干净、版本号未被占用、changelog 已生成
2. **全量测试**：跑一次完整矩阵，任何失败即终止
3. **Go 发布**：为每个 Go module 目录各打一个 `go/<module>/v1.2.0` 格式的 tag —— **必须脚本化**，手工逐个打 tag 一定会漏。**首次发布还要多一步**：扫描并清理所有模块 `go.mod` 里指向仓内其他模块的临时 `replace ... => ../<module>` 行（见 [02 仓库结构与发布](02-repo-and-release.md) 的过渡状态说明），替换成刚打好的真实版本号，再跑一次 `go mod tidy` 确认——遗漏任何一条都会导致业务方 `go get` 时因为 `replace` 指向本地路径而直接失败。
4. **npm 发布**：changesets fixed 版本组统一升版并发布，附带 provenance
5. **制品**：多架构（amd64/arm64）Docker 镜像、`saasctl` 多平台二进制（goreleaser）、**合并后的 OpenAPI 规范 `speed.yaml`**（Release 附件 + 打包进 `@speed/api-sdk` + 发布到文档站对应版本目录）
6. **发布后验证**：触发 `scaffold-verify`，用刚发布的版本生成全新项目并跑通，**失败则立即标记该版本为不可用**
7. **产出**：GitHub Release + changelog + SBOM

**M0 状态注记（离线验证轮）：** 上面七步是完整目标。M0 发布脚本轮（roadmap M0 的"changesets / lockstep 发布脚本"条目）落地的是第一步的前置预检与第三步"脚本化打 tag"的离线半程，真实发布排到 v1.0（M4）：

- **`.github/workflows/release.yml` 的现状 = 只验证、不发布**：手动触发、输入版本号（`workflow_dispatch` 的 `version` 输入承载版本号，第 1 步的"版本号未被占用"预检由协调器查重实现）。步骤为：校验版本号格式（正则 `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`，v 必需；`workflow_dispatch` 输入不支持 pattern 校验，故用 grep 步骤）→ 跑发布协调器默认模式 → 跑协调器自测。工作流权限仅 `contents: read`，**不接任何发布凭据——它不可能真实发布，这是本轮的刻意设计**。
- **协调器 `tools/release/lockstep-release.py`**（默认模式 = 离线验证；`--self-test` = 自带 unittest 套件）：推导可发布集合（go.work `use` 条目 + `web/packages/*`），检查版本格式、查重（`git tag -l`——因此 checkout 必须 `fetch-depth: 0`，浅克隆看不到 tag 会静默废掉查重预检）、go.work↔`go/` 树双向完备、npm 版本统一、`web/.changeset/config.json` fixed 组恰好覆盖现存包，全绿后打印完整单版本发布计划（每模块 tag、每包 bump 后版本）。本地入口 `task release:plan VERSION=v1.2.0`。
- **第 3 步（Go 发布）的两半**：打 tag 以硬闸本地模式存在于协调器（`--apply` 必须配 `--allow-local-tag-creation`，只创建本地、永不推送的 tag，仅用于在 scratch checkout 演练）；首次发布的 replace 清理以纯函数 + `tools/release/testdata/` 夹具交付，**严禁对真实 go.mod 运行**——树的过渡态保留到 v1.0（[02 仓库结构与发布](02-repo-and-release.md) 的 M0 注记）。
- **第 2、4、5、6、7 步的接线逐一等待**：第 2 步等 pr-full 的全量编排成熟（当前 pr-full 跑十一模块 Docker 集成矩阵——`dbkit`、`tenancy`、`jobs`、`pkgcore`、`config`、`rbac`、`org`、`authn`、`storage`、`notification`、`integration`——本轮核实自 `pr-full.yml` 自己的 `integration-tiers` job `matrix.module` 列表，而非任何文档转述 + reference-app 单测与 Redis 组装集成测试）；第 4 步等 M4（changesets 未安装进 web/、仓库无 npm 凭据）；第 5 步等制品轮（goreleaser 配置、镜像构建、spec 合并工具均未落地）；第 6 步等 `scaffold-verify.yml` 脱离 gated stub（现状见根 CLAUDE.md）；第 7 步等 M4。预发布通道与回滚策略（见下）不变，随真实发布轮实现。
- **验收口径**：该 M0 条目的退出条件是"一次命令能把全部模块以同一版本号发布出去"可**离线证明**——release.yml 每次手动触发（版本格式合法、各项预检全绿即通过）就是这份证明；首次端到端真实运行在 v1.0（M4）。协调器的绝对禁令与"树变化时该动什么"见 `tools/release/AGENTS.md`。

### 预发布通道
破坏性变更或大版本前，先发 `v1.2.0-rc.1` 到预发布通道，由 reference-app 先行验证，再发正式版。

### 回滚
已发布的 Go module tag 无法真正撤回（module proxy 会缓存），因此**回滚 = 发布修复版本**，不是删 tag。npm 可以 deprecate 但同样不删除。这一点必须在发布流程文档中写明，避免有人试图删 tag 导致更混乱的状态。

## 分支保护

- `main` 分支禁止直接推送，必须经 PR
- 必需检查项：`pr-check`、`security`、`docs-check` 全绿
- 至少一位 reviewer 批准；涉及 `pkgcore`/`dbkit`/`tenancy` 等地基模块时需要指定 owner 批准——设计意图，尚未落地：仓库里没有任何 CODEOWNERS 文件，这条 owner 审批目前无工具强制，纯靠人工把关
- **合并方式：仅允许 fast-forward**（与团队 Git 规范一致，见 [19 开发工作流](19-dev-workflow.md)）
