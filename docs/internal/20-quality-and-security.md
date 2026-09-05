# 质量与安全工程

> 测试分层、代码质量门槛、供应链与运行时安全。这些要求对脚手架比对普通业务项目更严格——一个缺陷会被复制到每一个基于它交付的项目。

## 测试分层

| 层次 | 范围 | 依赖 | 何时跑 |
|---|---|---|---|
| 单元 | 单个包内的逻辑 | 无外部依赖（用进程内实现作 test double） | 每次提交 |
| 集成 | 模块与真实基础设施 | testcontainers 拉 PostgreSQL / Redis | PR 合入前 |
| 契约 | OpenAPI 规范与实现、前后端类型（见 [21 API 契约](21-api-contract.md)） | 无 | 每次提交 |
| 端到端 | reference-app 完整业务链路 | 完整 compose 环境 | 合入 main / 每日 |

**进程内的那批实现同时就是 test double**，不需要为测试再造一套 mock。这是这套接口抽象的第二个正收益（第一个是部署灵活），也是为什么大部分单元测试能在 CI 上秒级跑完。

### 前端测试
Vitest + Testing Library 做组件与 hook 测试；Playwright 做 e2e。UI 包的每个公开组件需有 Storybook story（同时充当文档与视觉回归基线）。

> **实施状态注记（本轮核实）：** Playwright 与 Storybook 两者目前都不存在于仓库——没有任何 Playwright 配置/spec 文件，也没有 `.stories.*` 文件或 Storybook 依赖/配置；`.github/workflows/reusable-npm-package-ci.yml` 自己的 header 明确把"Storybook component previews"列进未接线清单（"ui-kit shipped without a preview harness; the round that introduces one wires it here"）。真实的组件/hook 测试确实是 Vitest + Testing Library；e2e 与视觉回归目前都靠各包自己的 `src/usage-example.test.tsx`（真实机制见 [13 文档规范](13-documentation-standards.md)、[16 验证方式](16-verification.md) 的同一处注记——这一缺口在多份文档里重复出现，均按此注记读）。

### 文件与目录布局

这是硬性约定，不是风格偏好——目的是让 `go test ./...`（默认只跑单元测试，秒级完成）和 `go test -tags=integration ./...`（显式触发，允许慢）这条分层在物理上可执行，而不是靠开发者自觉：

- **单元测试文件名必须是被测目标的前缀**：`registry.go` 对应 `registry_test.go`，`kv.go` 对应 `kv_test.go`（Go 原生约定）；前端同理，`PlanCard.tsx` 对应 `PlanCard.test.tsx`。不与单个源文件一一对应的测试文件，必须用它验证的行为语义命名（如 `concurrency_test.go`），禁止用 `misc`/`extra`/`independent` 这类不表意的名字——名字是未来定位测试的第一手段，含糊的名字让这个手段失效。
- **`example_test.go` 是 Go 惯例的例外**：godoc 可渲染的 `Example*` 函数按约定放在这个文件名下，不受"按目标命名"规则约束。
- **测试工具与帮助类放在独立的测试目录**：Go 侧是模块内的 `internal/testutil` 子包，前端侧是每个包的 `test-utils/` 目录；跨测试文件复用的 fake、builder、断言辅助函数都放这里，不允许在 `_test.go` 里内联重复定义（Go 的 `_test.go` 本身也无法被其他包 import，这是该约定的硬约束，不只是风格要求）。
- **集成测试与单元测试物理分离**：Go 侧每个模块用 `integration_test/` 子目录 + `//go:build integration` 构建标签；前端侧用 Playwright 原生的 `e2e/` 目录。任何一次普通的单元测试运行都不会碰到集成测试。

### 必须存在的专项测试套件
这几项在 [16 验证方式](16-verification.md) 中有详细验收标准，工程上要求它们是**可复用的测试套件**而非散落的用例：

- `tenancytest.AssertIsolated` —— 租户隔离，所有 Repository 必跑
- 同一 seam 各套实现的语义一致性 —— 同一组契约用例在每套实现下结果必须一致
- 双方言矩阵 —— 每个模块在 PostgreSQL 与 SQLite 上各跑一遍
- 迁移测试 —— 从零迁移到最新版本，双方言各验证一次

## 代码质量

### Go
- **golangci-lint**：全仓库一份配置，启用 `govet`、`staticcheck`、`errcheck`、`gosec`、`depguard`、`bodyclose`、`sqlclosecheck` 等
- **gofumpt** 格式化（比 gofmt 更严格，减少格式争议）
- **竞态检测**：`go test -race` 在 CI 中默认开启。计量计数、任务队列、缓存这些并发热点必须有竞态测试

### 前端
- **TypeScript strict 模式**，禁用 `any`（`@typescript-eslint/no-explicit-any` 为 error）
- **ESLint + Prettier**，配置作为共享包发布，业务项目可直接继承
- 公开包必须导出完整类型定义，`tsc --noEmit` 与 `publint` 校验打包产物

  **实施状态注记（本轮核实）：** `publint` 尚未接线——`.github/workflows/reusable-npm-package-ci.yml` 自己的 header 把"publint publish-shape validation and changesets wiring"列为明确未接线项，理由是目前还没有任何 `@speed/*` 包真正发布过，等 web 侧发布机制轮次落地再一并接入。`tsc --noEmit` 是真实落地的（每包 lint/typecheck leg 的一部分）。

### 覆盖率
不设一刀切的百分比门槛（容易催生无意义的测试），而是：
- 地基模块（`pkgcore`、`dbkit`、`tenancy`、`rbac`、`billing`、`jobs`）要求较高覆盖，且**覆盖率不允许下降**（与基线比对）
- 安全相关路径（租户隔离、权限判定、支付回调、令牌校验）要求分支覆盖完整

> **实施状态注记（本轮核实）：** "覆盖率不允许下降（与基线比对）"是设计意图，尚未落地——通读所有 workflow 文件，没有任何覆盖率采集、基线存储或 diff 比对的机制；地基模块要求较高覆盖、安全路径要求分支覆盖完整目前都只靠 code review 把关，没有自动化数字门槛。

### 警告治理
**警告视为一等问题**，与团队既定规范一致：编译警告、lint 警告、废弃 API 警告、React 控制台警告、a11y 警告、竞态检测警告，全部不得静默忽略或抑制。CI 中新增警告即失败；确需保留的必须有显式豁免注释并说明原因与跟踪项。

## 安全工程

### 供应链
| 措施 | 工具 |
|---|---|
| Go 依赖漏洞扫描 | `govulncheck` |
| npm 依赖漏洞扫描 | `pnpm audit` + Renovate 安全告警 |
| 密钥泄漏扫描 | `gitleaks`（提交钩子 + CI 双重） |
| 静态安全分析 | GitHub CodeQL |
| 容器镜像扫描 | `trivy` |
| SBOM 生成 | 每次发布产出，随 Release 附件发布 |
| npm 发布来源证明 | provenance（GitHub OIDC 签名） |

> **实施状态注记（2026-09，security 轮次）**：上表各行现状——`pnpm audit`（第 2 行前半）已上线：security.yml 的 deps-js job 审计提交的 web/ lockfile，本地实测无已知漏洞；Renovate（第 2 行后半）待依赖自动化轮次。`gitleaks`（第 3 行）的 CI 一半本轮上线（secrets job：v8.30.1 按 release 校验和固定，`.gitleaks.toml` 扩展默认规则集并带唯一一条 path allowlist——放行 go/observability 脱敏测试故意种植的 secret 形状 fixture，那正是该层测试存在的意义，放行理由与残余风险写在文件注释里，真实凭据依然禁止入库）；pre-commit 钩子一半留给 dev-workflow 轮次。GitHub CodeQL（第 4 行）本轮上线（sast job：Go + TypeScript/JavaScript，Go 提取器原生支持 go.work 工作区，一次分析覆盖全部模块）。`govulncheck`（第 1 行）与 `trivy`（第 5 行）暂缓——govulncheck 在当前树有两处独立原因必然失败（标准库公告属于 go.work `go 1.25.0` 工具链线、由 toolchain-pinning 轮抬升 go 指令后清零；模块类公告经 testcontainers 的测试支撑依赖与 exporter 依赖触达、属各模块自己的升级轮），trivy 在发布轮产出镜像前无物可扫——证据与解除条件记录在 security.yml 文件头的 DEFERRED 节。SBOM 与 provenance（第 6、7 行）随发布相关轮次落地。许可证扫描（下节）本轮已落地。

### 许可证合规（对本项目尤其重要）
脚手架会被用于**对外商业交付**，依赖的许可证会传导给客户项目。CI 中做许可证扫描，**禁止引入 GPL/AGPL 系依赖**；MPL/LGPL 类需单独评估并记录在 ADR 中。这一条在纯内部项目里可以放松，在这里不行。

> **实施状态注记（2026-09，security 轮次；依赖计数已按本轮核实更正）**：已落地——`tools/license_scan.py` 在 security.yml 的 license job 运行（先跑内置 selftest 再扫真实依赖树）；Go 侧 46 条、npm 侧 9 条共 55 条依赖的逐条 adjudication 见 `tools/dependency-licenses.json`（`jq '.dependencies | length'` 实测 55，按 `ecosystem` 分组 go=46、npm=9），扫描器会把「manifest 与真实依赖树不一致」或「新增依赖缺 adjudication」当作漂移直接报错，策略与本节一致（GPL/AGPL 拒绝、MPL/LGPL 需 ADR、未知许可证 fail-closed）。唯一一条 MPL/LGPL 依赖——`go/pki` Vault Transit 签名后端所需的 `github.com/hashicorp/vault/api`（MPL-2.0）——已由 `docs/adr/0003-accept-mpl2-for-pki-signer-vault.md` 完成裁定，manifest 条目携带对应的 `adr` 字段。

### 安全测试专项
以下场景必须有自动化用例，它们在 [16 验证方式](16-verification.md) 中已定义验收标准：

- 跨租户数据访问（含绕过 Go 层的裸 SQL，验证 PostgreSQL RLS 兜底）
- 社交登录的账号劫持（未验证邮箱不得自动合并账号）
- 会话撤销与 refresh token 重放检测
- 外发 Webhook 的 SSRF 防护
- 分享链接的枚举与越权访问
- 上传文件的 MIME 伪装与 EXIF 泄漏
- 权限提升（低权限角色尝试越权操作）

### 密钥管理
- 仓库内**不得出现任何真实凭证**，包括测试用的沙箱密钥；密钥泄漏由 CI 强制：security.yml secrets job 每 PR + 每日全树扫描（gitleaks，`.gitleaks.toml` 里唯一一条 allowlist 放行 go/observability 脱敏测试的 secret 形状 fixture——是替身值不是凭证，理由见该文件注释，扫描对其余所有文件保持完整效力）
- CI 密钥用 GitHub Environments 管理，发布流水线的密钥限定在受保护环境并要求人工审批
- 本地开发用 `.env.local`（已在 `.gitignore`），`.env.example` 只放占位符

## 性能基准

对以下热点建立 benchmark 并在 nightly 中做回归检测（相比基线劣化超过阈值即告警）：

- 租户过滤的 Repository 查询开销
- 权限判定（`rbac` 自建引擎，见 [05](05-identity-and-access.md) 的「实现落地更正」）的单次判定耗时与缓存命中率
- 动态配置读取（必须走进程内缓存，不能每次查库）
- 会话校验（尤其"立即失效"模式下每请求一次 KV 查询的开销 —— 这个数据决定该模式是否值得默认开启）
- 计量事件采集的吞吐与延迟

## Flaky 测试治理

不稳定测试会侵蚀团队对 CI 的信任，最终导致"红了就重跑"的坏习惯。措施：nightly 重复运行标记不稳定用例，自动开 issue 跟踪；连续不稳定的用例先隔离（标记 skip 并挂跟踪项）再修复，不允许长期挂着一个时红时绿的 CI。

> **实施状态注记（本轮核实）：** 上面两节都是纯设计意图，尚未落地——`.github/workflows/nightly.yml` 整个是 gated stub（触发即在 guard step 失败，不接受任何调度），其自己的文件头如实记录了阻塞原因：性能基准回归这一半，仓库里当前一个 `func Benchmark` 都没有（`grep "func Benchmark"` 遍历 `go/` 与 `examples/` 零命中），benchmark 随各热点归属的模块在 roadmap M1-M3 落地；flaky 检测这一半需要一个有 `issues: write` 权限的 token 来自动开 issue，等实现轮次落地时一并接入并过一次安全审查。全量矩阵这一半本身已经不再是阻塞点——`pr-full` 已经真实存在——但只要没有 benchmark 套件，`nightly` 就整体停在 gated stub。
