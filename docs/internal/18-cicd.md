# CI/CD 流水线

> 基于 GitHub Actions。本文档定义流水线的职责划分、触发时机与成本控制策略。
>
> 核心约束：**Go module 与 npm 包不能各写一套 workflow**。全部通过可复用 workflow 与 composite action 统一，否则 CI 配置本身会成为最大的维护负担。

## 流水线总览

| 流水线 | 触发 | 职责 | 目标时长 |
|---|---|---|---|
| `fast-check` | PR 打开/更新 | 受影响模块的 lint + 类型检查 + 单元测试 + 构建 | < 8 分钟 |
| `full-check` | PR 打 `full-ci` 标签 / 合入前 | 全量矩阵：每个模块的契约测试 × 该模块的每套实现（[16-verification](16-verification.md) §2 的矩阵形态），双方言，外加代表性整机组装冒烟 | < 25 分钟 |
| `e2e` | 合入 main / 每日 | reference-app 端到端（Playwright） | < 20 分钟 |
| `security` | PR + 每日 | 依赖漏洞、密钥扫描、SAST、镜像扫描、许可证检查 | < 10 分钟 |
| `docs-check` | 涉及文档或公开 API 的 PR | 文档示例编译运行、链接检查、i18n key 一致性、配置清单漂移 | < 5 分钟 |
| `api-contract` | 涉及 `api/openapi.yaml` 或 handler 的 PR | spec lint、合并冲突检查、**生成物一致性 diff**、oasdiff 破坏性变更检测（**未接线，见下方实施状态注记**——基线取 v0.0.1 发布提交 `fbaaaf98` 之树，Go module proxy 只解析 21 个模块中的 17 个；接线计划 M4，与 security 行的 govulncheck/trivy 同属"暂缓"而非遗漏） | < 5 分钟 |
| `scaffold-verify` | 每日 + 发布后 | CLI 生成全新项目 → 构建 → 两种部署模式各启动一次 → 冒烟 | < 15 分钟 |
| `release` | 手动触发（指定版本号） | lockstep 全量发布：Go module 逐个打 tag + npm 包逐个发布 + 镜像 + CLI 二进制 | < 30 分钟 |
| `nightly` | 每日 | 全量矩阵 + 性能基准回归 + flaky test 检测 | 不限 |

> **注记（2026-09-04，issue #1 子包拆分轮次已解决——本轮核实）：** 本条曾记录的问题——`.golangci.yml` 三条 SDK 规则的放行写法是 `!**/go/pkgcore/**` 与 `!**/go/jobs/**`，放行**整个模块连同它的全部子包**，导致 `pkgcore` 根包内联 Redis 与 S3 实现（`redis_kv.go`、`s3_objectstore.go` 与接口同包）在规则下完全合规、"每个消费者无条件继承 go-redis 与 minio-go"从未被任何检查报出——**已经修复**：`go/pkgcore` 的 Redis/S3 实现已迁入 `go/pkgcore/eventbus/redis`、`go/pkgcore/kv/redis`、`go/pkgcore/objectstore/s3` 三个实现子包，`go/jobs` 的 asynq 实现迁入 `go/jobs/queue/asynq`，`.golangci.yml` 的 `redis-only-in-pkgcore-and-jobs`/`minio-only-in-pkgcore`/`asynq-only-in-jobs` 三条规则的放行路径也随之收紧到 `!**/go/pkgcore/kv/redis/**` 这样的实现包本身，不再放行整个模块——谁再把 SDK import 写回根包或另一个实现包，CI 现在会直接失败，这正是拆分工作当初设定的验收条件。现状与推演见 `.golangci.yml` 自己的 depguard 注释；issue #1 的其余站点仍按各自轮次推进。

> **实施状态注记（2026-09，security 轮次）**：上表是设计规格，不是现状。当前真实落地的流水线：`fast-check`（每个 PR，加上每次直接推送到 main——参见「分支保护」一节，main 允许直接推送是团队常态，这条 push 触发器才是大多数运行的真实来源）、`full-check`（打 `full-ci` 标签的 PR，加上每次直接推送到 main——和 `fast-check` 同样的理由，同样的代价：每次 push 都要多付约 25 分钟的 Docker 集成测试）、`docs-check` 与 `api-contract`（按路径过滤触发），以及本轮由 gated stub 转正的 `security`（每个 PR + 每日 05:37 UTC 定时；触发、职责与暂缓项的解除条件见 `.github/workflows/security.yml` 文件头）。`release` 在 release-foundation 轮次转正（此后手动触发）；`e2e` 已从 gated stub 转为真实运行——`workflow_dispatch` 加每日 04:23 UTC 定时，合入 main 的 push 触发刻意未开启、待一次 dispatched 运行变绿后接上（触发细节与理由见 `.github/workflows/e2e.yml` 文件头）；`nightly` 仍是 gated stub；`scaffold-verify` 由 saasctl-distributed-mode 轮次转正（此后见下一条注记的详细记录），由各自轮次接手。security 行的设计内容逐项核对：pnpm audit、gitleaks、CodeQL 与许可证扫描已接线；govulncheck-wiring 轮次核实并解除了依赖漏洞扫描（Go 侧 govulncheck）当初暂缓的两处独立原因——标准库公告随 go.work `go` 指令抬升到 1.26.8（当时最新的已修复版本）清零，模块类公告（经 testcontainers 测试支撑依赖触达的 golang.org/x/crypto）按各模块自己的升级纪律逐一打了补丁版本——现已接线为 `deps-go` 任务，对二十一个已实现模块逐一跑 `govulncheck`；镜像扫描（trivy）仍暂缓，仓库产出镜像前无物可扫。两者的证据与解除条件都在 security.yml 文件头，govulncheck 那部分现在记录的是"做了什么"而非"为什么暂缓"。

> **实施状态注记（saasctl-distributed-mode 轮次）**：`scaffold-verify.yml` 从 gated stub 转为真实运行——`workflow_dispatch` 加每日 03:00 UTC 定时（与 security.yml 自己的 05:37 UTC 定时错开，避免两者抢占同一时段的 runner 容量；发布后触发仍未接线——真实发布事件自 v0.0.1 起已存在（release.yml 的 dispatch 即发布），缺的是触发接线，随 v1.0（M4））。真实跑的是 `go/saasctl/integration_test/scaffold_dual_mode_test.go` 的一个测试：对 `authn+org+rbac`（五个合法选集里最丰富的一个，也是唯一真正触达 authn「SMS 发送器」模块分布式模式条件注入分支的选集）做一次真实的 `saasctl new` → 真实联网 `go mod tidy` → 真实 `go build` → `saasctl db migrate` → 单进程模式启动并冒烟，再对同一构建产物重复 migrate 与启动一次，这次带 `APP_DEPLOYMENT_MODE=distributed`（2026-09-06 起：早先是 `SPEED_DEPLOYMENT_MODE`，fix/saasctl-app-prefix-drift 轮次把生成项目的引导变量前缀改成了 `APP_*`）并指向本测试自己拉起的真实 Redis / RustFS（S3 兼容）/ Mailpit 容器（与 examples/reference-app 自己的 distributed_mode_test.go 用同一组镜像 pin）。**这不是五个选集的全量覆盖**：另外四个选集（`none`、`authn`、`authn+rbac`、`authn+org`）仍只有 go/saasctl 自己离线单测套件（`internal/new` 的 golden 字节级校验）加本轮人工跑过一次的 B4 式真实构建证明,没有进 CI 的自己的双模式 boot 证明——这是一个刻意记录的、有理由的子集选择（root CLAUDE.md「Reference App」一节对 go/pki X.509 层、go/integration 未接消费者两处已经认可的同一种"有代表性子集 + 明说理由"处理方式），而不是疏漏。`create-saas-app` 与前端脚手架仍完全未建，这条流水线目前只覆盖 M4 验收口径的 Go 一半。

## 成本控制：不是每个 PR 都跑全量

模块数量 × 各模块的实现套数 × 2 方言，相乘后组合数太大，每个 PR 全跑既慢又贵。策略：

1. **路径过滤**（`dorny/paths-filter`）：只跑改动模块及其**下游依赖**模块。依赖关系从 `go.work` 与 workspace 配置自动推导，不手工维护映射表。
2. **分层触发**：PR 阶段跑快速检查（全进程内实现 + SQLite，无需容器）；合入前跑全量矩阵。这利用了进程内实现的一个副产品优势——大部分测试不需要 testcontainers 就能跑。
3. **缓存**：Go module cache、pnpm store、Docker layer、golangci-lint cache 全部启用，按 lockfile 哈希做 key。

> **实施状态注记（本轮核实，reference-app-docker-image 轮次前）：** 第 1 条还是设计意图，尚未落地。`fast-check.yml` 自己的文件头明确写着 `dorny/paths-filter` 未接入——现存模块数量还不大，每个 PR 目前无差别跑全部真实模块，等模块集合变大再引入路径过滤，且已经预留了"下游依赖推导"的落点（依托可复用的 `go-module-ci` workflow，接入时只改调用方，不改被调用的可复用 workflow 本身）。第 2 条（分层触发）与 Go module cache / pnpm store / golangci-lint cache 三项缓存是真实落地的，与本条注记不冲突。第 3 条（Docker layer 缓存）的状态由下一条注记接续记录。
>
> **实施状态注记（reference-app-docker-image 轮次）：** 上一条注记记录的"仓库里无镜像可建、`reusable-docker-build.yml` 是无人调用的 gated stub"现状已改变——`examples/reference-app/Dockerfile` 是本轮新增的第一个真实、可构建的镜像来源（多阶段构建：`golang:1.26.8-bookworm` 编译阶段 + `gcr.io/distroless/static-debian12:nonroot` 运行阶段，`CGO_ENABLED=0`——`go/dbkit` 的 SQLite 驱动 `github.com/glebarez/sqlite`/`modernc.org/sqlite` 全链路纯 Go、无 cgo，本轮逐一确认过——最终镜像约 47MB），`reusable-docker-build.yml` 的 guard stub 已移除，换成真正的多架构构建（`linux/amd64` + `linux/arm64`）外加 Docker layer 缓存（`actions/cache`，以 Dockerfile 内容哈希为 key，第 3 条的落地就在这里）；新增的 `docker-image-ci.yml`（push 到 main 且改动命中其构建输入路径集时自动触发，另支持 `workflow_dispatch`，flat 放在 `.github/workflows/` 根目录，不是子目录——本仓库早前正是在这一点上踩过 GitHub Actions 的硬性限制）是这个可复用 workflow 的第一个真实调用方，构建 `examples/reference-app/Dockerfile`。
>
> **实施状态注记（docker-build-native-arm 轮次，取代上一条注记里的多架构构建描述）：** 上一条注记描述的多架构 leg——单 runner 靠 QEMU 模拟 `linux/arm64`、既不 `--push` 也不 `--load`、构建完即丢弃——已被 `chore(ci): build docker images natively on parallel amd64/arm64 runners` 整体替换，QEMU 模拟不再存在于这条流水线的任何一步：`reusable-docker-build.yml` 现在跑两个并行的原生 builder job，`ubuntu-latest`（amd64）与 `ubuntu-24.04-arm`（arm64，GitHub 正式 GA、公开可用的原生 Arm64 托管 runner），各自的 `docker buildx build` 从不指定超出本机 CPU 真实指令集的 `--platform`，因此两个架构都是真被构建出来、真被 `--load` 进本机 Docker daemon 启动、curl 健康路由并确认镜像自带的 `HEALTHCHECK` 收敛到 `healthy`——arm64 现在也获得和 amd64 同样真实的原生冒烟验证,不再是"只证明建得出来、从未真正跑过"。冒烟通过后，每个架构 job 把自己的单架构镜像推到本仓库自己的 GHCR 命名空间（`ghcr.io/<owner>/<image_name>`），打上架构后缀 tag（`<tag>-amd64` / `<tag>-arm64`），认证用的是这条 workflow 自带的 `GITHUB_TOKEN`（`packages: write` 权限），不是另外配置的凭据——一个仓库的内置 token 推送到自己的 GHCR 命名空间是 GitHub Actions 的标准零配置能力，不是本文件说的"发明凭据"。第三个 job（`manifest`）依赖两个 builder job，用 `docker buildx imagetools create` 把两个已经推送、已经验证过的单架构镜像合并成一个真实的 OCI 多架构 manifest list，落在不带架构后缀的普通 tag 下，再用 `imagetools inspect` 打印合并结果、确认两个原生平台确实都在里面。
>
> 这**仍然不是完整发布形态**，但理由和上一条注记记的不一样了：这些 tag 只是 CI 证明用途（architecture-suffixed 与 run-scoped），不是带版本号的发布制品——真正面向公众的 registry 发布、挂上版本 tag、goreleaser，仍是 M4 工作，与下方「发布流水线」一节第 5 步的注记一致。另有一处本轮同样如实披露、值得记在这里而不是只留在 commit message 里的缺口：每次 `workflow_dispatch` 都会在 GHCR 里留下三个新 tag（两个架构后缀 + 一个合并 manifest），目前没有任何清理或保留策略去回收这些积累下来的 package version——谁来定期清理、按什么规则清理，还没有答案。`release` 与 `e2e` 都是真实运行的流水线，都不经这条可复用构建——`release` 是验证 + 发布的真实发布流水线（推 tag、发 npm 包，见下方「发布流水线」一节）；`e2e` 直接构建并启动 Go 服务器，不产出镜像；`nightly` 仍是 gated stub，目标调用方接线未做。`docker-image-ci.yml` 是这条可复用构建目前唯一的真实调用方。
>
> 速度数据：本仓库暂未留存这条流水线原生并行构建与旧 QEMU 单 runner 方案的正式对比基准（例如具体跑多少分钟），此处不引用一个无法在仓库里核实、日后容易过时的数字；粗略而言，两个原生 runner 并行构建、任何一侧都不再排队等待 QEMU 逐条指令模拟，是这次替换在直觉上、也在架构描述上明确带来的速度收益,只是没有一次可追溯的正式测量把它钉成一个具体倍数。
4. **并发控制**：同一 PR 的新推送自动取消旧运行（`concurrency` + `cancel-in-progress`）。
5. **超时**：每个 job 设 timeout，防止挂死消耗额度。

## 可复用 workflow 设计

```
.github/
  workflows/
    fast-check.yml                    # 编排：调用下面的可复用 workflow
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
| 系统上下文只能由白名单模块调用 | 人工评审 + 函数级文档约定；**不是** `depguard`——`WithSystemContext` 与 `TenantID`/`WithTenant`/`apperr` 同属 `pkgcore` 根包同一个 import path，depguard 只能按包路径粒度放行/拒绝，做不到只挡一个符号。已实测验证：把「仅 admin/compliance/jobs/authn/`tenancy` 可 import `pkgcore`」接成 depguard 规则，会连带拦下 `go/dbkit`（真实代码、不在白名单、但合法依赖 `TenantID` 等）23 处无关导入，草稿规则因此未合入。`tenancy` 现已建成，提供审计封装版 `tenancy.WithSystemContext`，业务代码应调用它而非直接调用原语；要让这条纪律真正可静态检查，需要先把 `WithSystemContext` 迁到 `pkgcore` 独立子包（类似 `apperr/`、`config/`），这是一次公开 API 决策，超出本表列出的自动化检查范围。`.github/CODEOWNERS` 已经落地，覆盖 `go/pkgcore`、`go/dbkit`、`go/tenancy`，但它只能要求"这些路径的改动需要指定 owner 过目"，做不到"只有白名单模块能调用 `WithSystemContext`"这种符号级约束——两者是不同粒度的问题，CODEOWNERS 落地并不代表这条纪律已经可以自动检查，白名单仍然纯靠人工把关 |
| 禁止手写 `WHERE tenant_id = ?` | `semgrep`：租户过滤只能由插件与 Repository 注入，手写即意味着绕过防护 |
| API 层不得接受外部传入的 `tenant_id` | `semgrep` + spec lint：请求参数/请求体中出现 `tenant_id` 字段即拒绝 |
| 禁止 `AutoMigrate` | `semgrep`：生产迁移必须是版本化 SQL |
| 禁止跨模块数据库外键 | 迁移文件 lint：`REFERENCES` 目标表必须属于同一模块 |
| 日志与响应不得输出明文 PII | `semgrep` 检查敏感字段直接进日志；配合脱敏中间件的单元测试 |
| `@speed/api-sdk` 不得手改 | 生成物 diff（改了会被下次生成覆盖，CI 提前拦截） |
| 每个 Repository 必须跑隔离测试 | 自研脚本：扫描 Repository 实现，比对测试覆盖清单 |
| 每个模块的 `go.mod` 必须能脱离 `go.work` 独立构建 / `tidy`（否则 `replace` 了却漏加 `require` 这类问题会被 workspace 的隐式路径解析掩盖，只有真实消费方或首次 lockstep 发布删掉过渡期 `replace` 行后才会暴露，见 [02 仓库结构与发布](02-repo-and-release.md)） | 过渡手段：`go/tenancy` 的 `TestModuleBuildsStandaloneOutsideWorkspace`（`standalone_build_test.go`）用 `os/exec` 以 `GOWORK=off` 跑一遍该模块自己的 `go build ./...`/`go vet ./...`，随 `go test ./...` 默认执行，不依赖任何 CI 配置；真正的 CI 落地后应替换为流水线里对每个模块单独执行的 `GOWORK=off` 构建步骤 |

**这张表是 CI 的核心价值所在**——纪律靠人记会在三个月后失效，靠 CI 才能长期有效。

> **实施状态注记（2026-09，security 轮次）**：表中由 `semgrep` 承担的行已有六条规则落地（本轮新增），见 `tools/semgrep_rules/`——`raw-gorm-bypass.yml`（第 1 行 `db.Table/Model/Raw`，path allowlist 放行 go/dbkit 与 go/jobs 的合法存储层文件）、`deployment-mode-branch.yml`（第 2 行，按值匹配模式比较与 `SPEED_DEPLOYMENT_MODE` 读取，仅放行两个 kernel 装配模式决策点：`go/pkgcore/deployment_mode.go` 的必需能力分发与 reference-app 入口的 env 读取）、`tenant-id-metric-label.yml`（第 6 行，配合 observability 既有的标签断言测试）、`non-constant-log-message.yml`（第 10 行）、`handwritten-tenant-id-filter.yml`（第 16 行，放行 go/dbkit 与 go/jobs 存储层）、`gorm-automigrate-ban.yml`（第 18 行，未来防线，当前零真实调用点）。每条规则的文件头写明：对应的纪律行、命中形状、path allowlist 与放行理由、残余缺口；配套 planted-violation fixture（`testdata/<规则>/{positive,negative}.go`）证明规则真的会响。六条规则随每个 PR 在 fast-check 的 repo-checks job 运行（临时 venv 安装 semgrep，扫 `go/`、`examples/`、`tools/` 三棵子树，fixture 子树在 CLI 层排除；CI 首绿前版本故意不 pin），运行方式与执行状态见 `tools/README.md`。两点如实披露：其一，semgrep 对 `examples/reference-app/internal/notes/repository.go:19`（内嵌实例化泛型 `*dbkit.Repository[Note]`）始终抛 PartialParsing 异常、该行不参与分析——它是结构体嵌入声明，与六条规则的命中形状均无关，暂无盲区，但后续新增规则必须知道这个文件扫不全；其二，各规则文件头列出的残余缺口（动态拼装、别名间接引用等文本匹配不到的形态）仍由 code review 兜底。depguard 侧，第 5 行「业务代码不得 import 具体基础设施 SDK」本轮落地三条规则（redis / minio / asynq）——**这三条规则随后又被 issue #1 子包拆分轮次进一步收紧**：放行粒度从本轮落地时的整模块（`!**/go/pkgcore/**`、`!**/go/jobs/**`）改为各自实现归属的子包（`!**/go/pkgcore/kv/redis/**` 等），详见本文档开头的注记——rules 与 files-list allowlist 的取舍记录在 `.golangci.yml` 的 depguard 注释里；第 15 行「系统上下文白名单」维持评审制——该行已写明 depguard 做不到符号粒度、草稿规则为何未合入，与落地现状一致。此前轮次已落地的行：第 7 行（ESLint `no-literal-text` 规则在 npm-package-ci 每包 lint 腿运行）、第 8 行（中英 key 一致性在 docs-check）、第 9 行（CJK 扫描在 repo-checks）、第 13 行（spec 一致性在 api-contract）、第 23 行（`GOWORK=off go build ./...` + `GOWORK=off go vet ./...` 测试类型检查在 go-module-ci 第 5 腿，过渡期 `standalone_build_test.go` 保留本地执行）。其余行——跨模块 import struct 与 `rbac` 依赖 `authn`（go-arch-lint / 依赖白名单）、日志字段名规范、前端手写 API 调用、API 层接受 `tenant_id` 参数、跨模块外键迁移 lint、`@speed/api-sdk` 禁改、Repository 隔离测试覆盖脚本——的自动化仍是未来轮次，不在此虚报；PII 直入日志行的运行时一半（脱敏中间件与它的单元测试）已在 go/observability 落地，缺的是 semgrep 静态检查那一半。

> **实施状态注记（2026-09-03，authn 轮次；片段顺序已按本轮核实更正）**：上一条注记里点名"仍是未来轮次"的 operationId / schema 命名规范一行，本轮已落地——`go/authn/api/openapi.yaml` 触发了合并的存在意义（它不是继 notes 之后的第二个模块 spec 片段，`go/org/api/openapi.yaml` 更早落地，只是 org 的片段当时不参与合并，详见 [19 开发工作流](19-dev-workflow.md) 的同一处更正）：仓库根目录的 `redocly.yaml` 定义了 `rule/speed-operation-id-format`、`rule/speed-schema-name-format` 两条 error 级命名规范规则，`Taskfile.yml` 的 `task api:merge` 与 `.github/workflows/api-contract.yml` 用钉定的 `@redocly/cli@2.51.1` 把两个片段 `join` 进 `contracts/speed.yaml` 后按这两条规则 `lint`，`git diff --exit-code` 校验该文件与提交版本一致。详见 [19 开发工作流](19-dev-workflow.md) 与 [21 API 契约](21-api-contract.md) 各自的实现状态注记。

> **实施状态注记（2026-09，deployment-composition 轮次更新）**：第 2 行规则随改造转为残余风险探测器——部署模式从选择器变成约束（`NewKernel` 不再收模式、四个模块从各自的实现注册表解析带能力声明的实现），`registry.go`、observability `init.go`、reference-app `main.go` 的模式分支被移除，allowlist 相应删到上一段句读的两个决策点；按 `Capability` 分支（如 `caps.Has(MultiReplicaSafe)`）是改造引入的新词汇，尚无纪律行，规则头与 `tools/README.md` 的残余缺口列明确记录为不覆盖。full-check 的真实矩阵随之对齐 [16-verification](16-verification.md) §2 的形态：pkgcore 的 Docker 腿把三套有真实后端的模块契约测试（`kvstoretest`/`eventbustest` 对真实 Redis、`objectstoretest` 对 RustFS/S3）各跑一遍——mailer 的 SMTP 实现无容器后端，同一套 `mailertest.AssertConforms` 在单元层对进程内 fake SMTP server 跑；reference-app job 新增其 Docker-backed 组装腿（真实 Redis `EventBus` 注入、standalone 拓扑启动整机——正是 §2 的"代表性整机组装冒烟"），Taskfile `test:full` 同步镜像。
>
> **实施状态注记（2026-09，tooling 批次）**：上一条注记点名"仍是未来轮次"的行里，本批次落地了四条、如实保留四条：
> - **`rbac` 不得依赖 `authn`（第 4 行）**：depguard 单边规则 `rbac-must-not-import-authn` 已随 go/rbac 成为真实 lint 目标而落地（`.golangci.yml`，只约束 `go/rbac/**` 非测试文件；双向验证：植入侵入性 import 即红、移除即绿）。第 3 行"跨模块 import struct"的一般形态仍未自动化——需要 go-arch-lint 或把 [01-architecture](01-architecture.md) 的模块图逐模块誊成 fan-in allowlist（誊错一行会让所有模块一起红），`.golangci.yml` 的注记已如实改写为这个理由。
> - **日志字段名规范（第 11 行）**：semgrep 规则 `snake-case-log-attribute-key.yml`（第七个规则）——结构化 logger 四个方法的键值对里，键字面量不是 snake_case（出现大写、连字符、空格等 `[a-z0-9_.]` 之外字符）即命中；`http.Error` 的 stdlib 形态与 sibling 规则同样被 pattern-not 排除。带 planted positive/negative fixture，真实树扫描零命中。
> - **API 层不得接受外部传入的 `tenant_id`（第 17 行）**：自研 spec lint `tools/check_spec_request_tenant_id.py` 扫描全部后端片段（13 个文件）的请求体 schema（含传递 $ref）与参数；唯一例外是 authn 的四个 pre-auth 租户指名请求 schema（其 spec 头部论证"request, never a grant"，resolveTenant 每次重验成员资格）——同名 schema 出现在 authn 自己的 spec 之外照样命中。
> - **禁止跨模块数据库外键（第 19 行）**：自研迁移 lint `tools/check_migration_cross_module_fks.py`——REFERENCES 目标表必须由引用文件所在模块自己的迁移 CREATE；与 gorm-automigrate-ban 同类属未来防线（树里今天零 REFERENCES），planted fixture 是牙齿。
> - **如实保留的四条**：第 12 行（前端禁止手写 API 调用）与第 22 行（Repository 隔离测试覆盖）早于本批次已落地（`web/eslint.config.mjs` 的 `speed/no-direct-http` 规则、repo-checks 的 `check_repo_isolation.py`），本次核实确认；第 20 行（`@speed/api-sdk` 禁改）的自动化仍是 api-contract 再生成一致性闸门的触发范围问题——手改但不动 spec/toolchain 的文件不会触发该流水线，检查形态与 api-contract 的路径过滤强耦合，需要一次独立的再生成比对或生成物哈希闸门，未在本批次强行实现；第 21 行（日志/响应明文 PII）的 semgrep 静态一半未实现——go/observability 的脱敏在 sink 端按 key 名与值形态全量遮盖、无 per-call 退出（`redact.go`），静态文本规则无法区分"动态值已遮盖/未遮盖"，造不出不误伤的真规则，需要数据流分析，仍由 review 兜底。接线位置如实分列：depguard 规则经 go-module-ci 的 golangci leg 随每次模块 lint 生效；semgrep 第七规则随 ruleset 目录自动并入 repo-checks 的 semgrep 步骤（fixture self-check 同步覆盖）；spec lint 与迁移 lint 是两个新增的 repo-checks 纯 python 步骤。

## 发布流水线（lockstep）

手动触发并输入版本号（如 `v1.2.0`），流水线按序执行：

1. **前置校验**：main 分支绿灯、工作区干净、版本号未被占用、changelog 已生成
2. **全量测试**：跑一次完整矩阵，任何失败即终止
3. **Go 发布**：为每个 Go module 目录各打一个 `go/<module>/v1.2.0` 格式的 tag —— **必须脚本化**，手工逐个打 tag 一定会漏。**首次发布还要多一步**：扫描并清理所有模块 `go.mod` 里指向仓内其他模块的临时 `replace ... => ../<module>` 行（见 [02 仓库结构与发布](02-repo-and-release.md) 的过渡状态说明），替换成刚打好的真实版本号，再跑一次 `go mod tidy` 确认——遗漏任何一条都会导致业务方 `go get` 时因为 `replace` 指向本地路径而直接失败。
4. **npm 发布**：changesets fixed 版本组统一升版并发布，附带 provenance
5. **制品**：多架构（amd64/arm64）Docker 镜像、`saasctl` 多平台二进制（goreleaser）、**合并后的 OpenAPI 规范 `speed.yaml`**（Release 附件 + 打包进 `@speed/api-sdk` + 发布到文档站对应版本目录）
6. **发布后验证**：触发 `scaffold-verify`，用刚发布的版本生成全新项目并跑通，**失败则立即标记该版本为不可用**
7. **产出**：GitHub Release + changelog + SBOM

**实施状态注记（2026-09，发布机制）：** 上面七步是完整目标。发布机制的两条腿：离线预检（第一步的前置校验与第三步"脚本化打 tag"的离线半程）编排在 roadmap M0 的"changesets / lockstep 发布脚本"条目下，由协调器承担；真实发布的执行腿已随首次真实发布 v0.0.1（2026-09-10）落地在 `release.yml`——验证 + 发布两 job。真发布类的其余事项仍随 v1.0（M4）：npmjs.org registry、changesets 流程与 npm provenance、SBOM 与 GitHub Release 对象、制品腿（goreleaser 二进制、版本化镜像发布、`speed.yaml` 附件与文档站版本目录）、oasdiff 接线、post-release 触发、trivy、许可证扫描传递依赖扩展（清单见 [24 延迟事项路线图](24-deferral-roadmap.md) 第 5 章）：

- **`.github/workflows/release.yml` 的现状 = 验证 + 发布两 job**：手动触发、输入版本号（`workflow_dispatch` 的 `version` 输入承载版本号，第 1 步的"版本号未被占用"预检由协调器查重实现）。`verify` job 以 `contents: read` 只读运行：校验版本号格式（正则 `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`，v 必需；`workflow_dispatch` 输入不支持 pattern 校验，故用 grep 步骤）→ 跑协调器默认模式 → 跑协调器自测。`publish` job（`needs: verify`）持有写权限（`contents: write` 推 tag、`packages: write` 发 npm）并执行真实发布：先拒绝在任一可发布 `go.mod` 仍带指向仓内其他模块的临时 `replace` 时打 tag；再从 go.work 推导模块集，创建并推送本版本全部模块 tag `go/<module>/<version>` 与仓库根 tag（裸版本号，在本次运行自己的 checkout 创建），已存在的任一 tag 一律跳过（已发布 tag 永不重打、永不移动，部分完成态的补发运行由此收敛）；最后按依赖序把 `web/` 十二个 `@speed` 包发布到 GitHub Packages registry（`npm.pkg.github.com`，以内置 `GITHUB_TOKEN` 认证；发布前先探测 registry、已存在的版本跳过；预发布版本进 `next` dist-tag）。首次真实运行（v0.0.1，2026-09-10）半成功：Go 21 个模块 tag 推送成功；npm 十二包因 `@speed` scope 未关联仓库 owner 的 GitHub Packages 安装而全部 403（仓库外 org 配置，非代码缺陷，十二包零上 registry）；该版本随后作废——21 个模块 tag 已从远端与本地删除、版本号不复用，org 侧关联后由下一个版本号的发布完整收敛。
- **协调器 `tools/release/lockstep-release.py`**（默认模式 = 离线验证；`--self-test` = 自带 unittest 套件）：推导可发布集合（go.work `use` 条目 + `web/packages/*`），检查版本格式、查重（`git tag -l`——因此 checkout 必须 `fetch-depth: 0`，浅克隆看不到 tag 会静默废掉查重预检）、go.work↔`go/` 树双向完备、npm 版本统一、`web/.changeset/config.json` fixed 组恰好覆盖现存包，全绿后打印完整单版本发布计划（每模块 tag、仓库根 tag、每包 bump 后版本）。本地入口 `task release:plan VERSION=v1.2.0`。
- **第 3 步（Go 发布）的两半**：tag 的推送由 `release.yml` 的 publish job 承担（见上）；协调器的 `--apply` 保持硬闸在 `--allow-local-tag-creation`（只创建本地、永不推送的 tag，仅用于在 scratch checkout 演练打 tag 半程）。过渡态 `replace` 的清理以纯函数 + `tools/release/testdata/` 夹具交付，协调器**严禁对真实 go.mod 运行**：对真实 go.mod 的清理由独立提交在 dispatch 前落地（v0.0.1 首发即如此），当前树在首发作废后重新携带过渡态 `replace` 行，下一次发布的发布提交再把它们清掉（[02 仓库结构与发布](02-repo-and-release.md) 的「可用发布前的过渡状态」注记）。
- **第 2、4、5、6、7 步的接线现状**：第 2 步（发布前跑一次完整矩阵）未接线——`verify` job 跑的是协调器离线验证与自测，不跑测试矩阵；全量矩阵仍由 `full-check` 承担（其 `integration-tiers` job 现按 `matrix.module` 跑十四模块 Docker 集成矩阵——`dbkit`、`tenancy`、`jobs`、`pkgcore`、`config`、`rbac`、`org`、`authn`、`storage`、`notification`、`integration`、`metering`、`billing`、`pki`——外加 reference-app job 的 lint + 单测 + Redis 组装集成测试）。第 4 步的发布半已落地（十二包发布到 GitHub Packages，见上）；changesets 本体仍未安装进 `web/`，npm provenance 与 npmjs.org registry 仍候 M4。第 5 步的镜像构建与推送半已落地——`examples/reference-app/Dockerfile` 与真正可用的 `reusable-docker-build.yml` 都已接线（见上方「成本控制」一节的注记），单架构镜像经原生冒烟后推入本仓库 GHCR 命名空间并合并出多架构 manifest，但这些 tag 只是 CI 证明用途（架构后缀与 run-scoped），**不是"挂上版本 tag 的发布制品"**——版本化镜像发布、goreleaser 配置、`speed.yaml` 作为 Release 附件与文档站版本目录，仍在 M4 的制品腿上。第 6 步的 `scaffold-verify` 自身已真实运行（见上方流水线总览的 scaffold-verify 注记），缺的只是"发布后触发"的接线。第 7 步（SBOM + GitHub Release 对象）仍候 M4。预发布通道的 `-rc.N` 形态已随发布执行腿接线（进 `next` dist-tag）；回滚策略（见下）与 release.yml 头注一致。
- **验收口径**：该 M0 条目的退出条件是"一次命令能把全部模块以同一版本号发布出去"。离线证明（release.yml 每次手动触发、版本格式合法且各项预检全绿即通过）与首次端到端真实运行（v0.0.1）都已发生，半成功态与其作废经过见上；半成功留下的恢复机制（已存在 tag 一律跳过；npm 逐包探测、已存在的版本跳过）保留为流水线的部分态语义。协调器的绝对禁令与"树变化时该动什么"见 `tools/release/AGENTS.md`。

### 预发布通道
破坏性变更或大版本前，先发 `v1.2.0-rc.1` 到预发布通道，由 reference-app 先行验证，再发正式版。

### 回滚
已发布的 Go module tag 无法真正撤回（module proxy 会缓存），因此**回滚 = 发布修复版本**，不是删 tag。npm 可以 deprecate 但同样不删除。这一点必须在发布流程文档中写明，避免有人试图删 tag 导致更混乱的状态。

## 分支保护

- `main` 分支允许直接推送——这是团队实际的工作方式：本地完成、rebase 到 main 顶部、fast-forward 推送即可，不强制经过 PR。本轮核实：710 次提交中只有 1 次真正走了 PR（`#3`，Dependabot 自动发起的依赖升级，人工点击合并），其余全部是直接推送——这不是历史遗留的例外，是常态。
- `fast-check` 在每次 PR 和每次直接推送到 main 时都跑（`.github/workflows/fast-check.yml` 的 `push: branches: [main]` 触发器），是 main 健康与否的事后可见信号；`security`/`docs-check` 各自按路径过滤或每日定时触发。三者都不是 GitHub 分支保护意义上"合并前必须全绿"的强制闸门——main 上出现红灯之后要靠人工在 Actions 页面发现、随后补一个修复提交去收敛，而不是被拦在推送之前。GitHub 上确实没有为 main 配置任何原生分支保护规则（`gh api repos/.../branches/main/protection` 返回 404），这与本条描述的实际工作方式是一致的，不是缺口。
- reviewer 批准与 CODEOWNERS：常规工作没有强制 review 步骤；`.github/CODEOWNERS` 文件存在（覆盖 `pkgcore`/`dbkit`/`tenancy` 等地基层模块），但 GitHub 侧"Require review from Code Owners"未启用，目前不生效。仅有的一次 PR 合并（Dependabot）经过了人工点击合并这一步，可以视为对自动化变更的最低限度把关。
- **合并方式：仅允许 fast-forward**（与团队 Git 规范一致，见 [19 开发工作流](19-dev-workflow.md)）——这一条是真实执行的：`git log --merges` 为空，历史保持线性。
