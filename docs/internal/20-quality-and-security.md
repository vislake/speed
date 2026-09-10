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
每个已发布模块（go.work 的全部 22 个模块：21 个 `go/*` 模块加 `examples/reference-app`）都要过两道自动化的线：
- **80% 下限**：单元套件语句覆盖率不得低于 80%，度量排除生成文件（`.gen.go`——pinned oapi-codegen 的输出是生成器写的，不是测试标的）。旧版此处"不设一刀切的百分比门槛（容易催生无意义的测试）"的论证被 2026-09 的产品决策取代，决策变更记录见下方实施状态注记
- **覆盖率不允许下降**：与入库基线比对（有效容差按模块规模校准：取 0.15 个百分点与「两条语句在该模块普查中的点数」（2 × 100 / 语句总数）中的较大者，2 条语句的规模地板使抖动预算不随模块规模变化；0.15 点的主容差按实测负载敏感抖动上沿设定——0.05 时期按六个地基模块约 0.03 的实测上沿；扩展到全部模块后，go/authn 的 MFA 并发确认竞速测试暴露约 0.12 的上沿，主容差随之上调，详见下方注记）——先于下限存在的地基模块规则，保留至今：测得的覆盖即使仍高于 80%，只要比基线低得超过容差同样失败，防止覆盖率在高位缓慢滑落
- 安全相关路径（租户隔离、权限判定、支付回调、令牌校验）要求分支覆盖完整

> **实施状态注记（本轮核实）：** "覆盖率不允许下降（与基线比对）"是设计意图，尚未落地——通读所有 workflow 文件，没有任何覆盖率采集、基线存储或 diff 比对的机制；地基模块要求较高覆盖、安全路径要求分支覆盖完整目前都只靠 code review 把关，没有自动化数字门槛。
>
> **实施状态注记（2026-09，tooling 批次）：** 上一条注记记录的空白已由 `tools/check_coverage_baseline.py` 关闭一半：六个地基模块的"覆盖率不允许下降"现在是真实机制——每个地基模块跑 `go test -coverprofile` 后把精确语句覆盖率与入库的 `tools/coverage-baselines.json` 行比较（容差 0.05 个百分点，按实测的负载敏感抖动上沿设定），下降即失败；`--update` 重新记录基线（蓄意下调必须与记录基线在同一改动里、理由写进提交信息），`--selfcheck` 保证基线文件自身完整（每个行都是一个活的地基模块、每个地基模块都有一行）。CI 接线：reusable-go-module-ci 新增第 3b 腿（每模块 job 都跑比较，门外的模块打印说明后通过），fast-check 的 repo-checks 跑 selfcheck。尚未自动化的另一半是"安全相关路径要求分支覆盖完整"——那需要 `-coverpkg` 级的分支覆盖度量与逐路径清单，仍靠 code review 把关，本批次不虚报。
>
> **实施状态注记（2026-09，全量门禁批次）：** 门禁扩到全部 22 个已发布模块（go.work 的 21 个 `go/*` 模块加 reference-app），并在"不下降"之上加了 **80% 下限**——本段正文原句"不设一刀切的百分比门槛"对下限不再成立：产品决策取代了它，对"不允许下降"的论证保留，下限与基线两道比较都过才绿。度量口径同步新增规则：普查排除 `.gen.go`（oapi-codegen 提交产物，生成器写的代码不是测试标的），`--update` 与 `--check` 同口径；口径变了数字就变，基线文件在同一次改动里全部重新实测记录（基线记录与定义该量的改动同行）。下限不可重基线化：`--update` 拒绝记录低于下限的实测值——下限是产品决策而不是可记录的基线，跌破它只能加测试，或与脚本里 `LOWER_BOUND_PP` 及本段一起改决策。容差随之从 0.05 提到 0.15 个百分点：扩展到全部模块后的实测暴露了比六个地基模块时代更宽的单次测量抖动——go/authn 的 MFA 并发确认竞速测试（八个 goroutine 抢一个 pending factor）里，输家是撞上 SQLite 写锁还是干净落败取决于调度，输家分支的语句被不被覆盖纯看运气，实测单次可差约 0.12 点（authn 普查仅 3076 条语句，约 4-5 条）；主容差按噪声上沿取 0.15，有效容差另按模块规模校准——取 0.15 点与「两条语句在该模块普查中的点数」中的较大者，2 条语句的规模地板使小模块的抖动预算不被固定点数容差低估；无论哪一档，都远小于任何"加代码不写测试"的真实下降。CI 接线：reusable-go-module-ci 第 3b 腿对每个 go/ 矩阵行跑双比较（21 个 go/ 模块，每个 PR 和每次 push 到 main 都测）；reference-app 不是 go-module-ci 矩阵行（它在 go/ 之外，fast-check 只构建它、其单元套件只在 full-check 跑），它的覆盖门禁作为独立步骤接在 full-check 的 reference-app job 单元套件之后——fast-check 不 gate 应用的覆盖率：应用套件本就不在 fast-check 跑，每个 PR 再加一次完整测量只是重复一个不存在于那里的运行，每次 push 到 main 的 full-check 才是它的执行点。fast-check 的 repo-checks 继续跑 `--selfcheck`（行集合随之扩到 22）。"安全相关路径要求分支覆盖完整"仍未自动化，同上一条注记，本批次不虚报。
>
> **实施状态注记（2026-09，web 覆盖门禁批次）：** 前端侧覆盖门禁的度量口径与首次数值落地如下，CI 接线待补测试轮次完成。口径与 Go 门禁对齐到 statement（语句）覆盖率，但按包独立计：每个 web 发布包（`web/packages/*` 十二个包）加外部成员 `examples/reference-app/web` 各过 **80% 下限**，度量范围是包自身的 `src/**`（宿主应用即它自己的 `src/`），生成产物排除——`@speed/api-sdk` 的 `src/index.ts`（orval 输出，DO-NOT-EDIT 头）不计入，手写接缝 `src/runtime.ts` 照常计量；`include` 限定到 src 也使别名解析的兄弟包源码永远不算进引用方包的数字。度量机制：workspace 根精确安装 `@vitest/coverage-v8@4.1.11`（与 vitest 4.1.11 同版本），13 份 vitest 配置各带 v8 provider 的 coverage 块（`include: ['src/**']`、`reporter: ['text-summary', 'json-summary']`——vitest 4.1.11 的 CoverageOptions 键是单数 `reporter`，复数 `reporters` 会被静默忽略，正是本批次踩到并改正的坑；tokens/i18n/api-client 三个此前没有 vitest 配置的包新增纯 coverage 配置，其余测试选项全部留默认，套件行为抽查验证不变——tokens 与 api-client 两包带/不带 coverage 的用例数一致）。门禁将解析各包 `coverage/coverage-summary.json`；本地运行方式 `pnpm --dir <包目录> exec vitest run --coverage`。首次实测（2026-09-09，13 个测量对象全部干净跑通，无异常包）：

| 测量对象 | statements | lines | functions | branches |
|---|---|---|---|---|
| @speed/tokens | 100%（38/38） | 100%（38/38） | 100%（7/7） | 94.73%（18/19） |
| @speed/product-shell | 100%（35/35） | 100%（35/35） | 100%（4/4） | 96.66%（29/30） |
| @speed/api-sdk（生成文件排除后仅 src/runtime.ts） | 100%（10/10） | 100%（10/10） | 100%（4/4） | 100%（4/4） |
| @speed/auth-ui | 99.56%（228/229） | 99.55%（225/226） | 100%（46/46） | 93.39%（99/106） |
| @speed/auth-core | 98.87%（176/178） | 98.87%（176/178） | 100%（37/37） | 96.36%（106/110） |
| @speed/layout-kit | 98.63%（72/73） | 98.61%（71/72） | 95.45%（21/22） | 90.62%（58/64） |
| @speed/billing-ui | 97%（97/100） | 96.96%（96/99） | 100%（27/27） | 95.45%（84/88） |
| @speed/account-ui | 96.79%（362/374） | 96.78%（361/373） | 100%（73/73） | 91.17%（310/340） |
| @speed/ui-kit | 96.14%（299/311） | 96.95%（287/296） | 98.82%（84/85） | 93.91%（278/296） |
| @speed/api-client | 95.45%（378/396） | 95.45%（378/396） | 98.57%（69/70） | 93.63%（250/267） |
| examples/reference-app/web | 93.51%（1312/1403） | 93.43%（1296/1387） | 94.3%（265/281） | 83.51%（785/940） |
| @speed/tenancy-ui | 92.3%（60/65） | 92.18%（59/64） | 91.66%（11/12） | 86.11%（31/36） |
| @speed/i18n | 89.12%（213/239） | 89.51%（205/229） | 87.8%（36/41） | 85.53%（136/159） |

全部 13 个测量对象首测即高于 80% 线（最低 @speed/i18n 89.1%）。CI 强制已落地（2026-09-09）：npm-package-ci 的每包 test 腿现以 `pnpm test --coverage` 运行，13 份 vitest 配置的 `coverage.thresholds` 全局下限为 **statements 80 + branches 80**，任一低于下限即 vitest 非零退出、该包 job 失败；下限即强制机制，未复制 Go 门禁的"不允许下降"基线比较。lines 与 functions 不设下限：13 个对象里 lines 与 statements 逐包差不超过 0.81pp（ui-kit 一行 lines 还高于 statements），近乎同一度量，不写两遍；functions 首测最低 87.8%（i18n），余量远大于 branches 的 3.51pp（reference-app web），且一个函数完全未被调用必然同时拉低 statements 与 branches，已被这两维抓住。接线后带阈值复测 i18n/ui-kit/reference-app web/api-sdk 四个代表包，全部通过且数值与下表逐位一致。边界情况已探明并记录：api-sdk 排除生成文件后只剩 runtime.ts 一条可测源，其数字单独标注；测量会在各包目录留下 `coverage/` 产物目录，仓库 .gitignore 尚无对应条目，属后续清理项。

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

> **实施状态注记（2026-09，security 轮次）**：上表各行现状——`pnpm audit`（第 2 行前半）已上线：security.yml 的 deps-js job 审计提交的 web/ lockfile，本地实测无已知漏洞；Renovate（第 2 行后半）待依赖自动化轮次。`gitleaks`（第 3 行）的 CI 一半本轮上线（secrets job：v8.30.1 按 release 校验和固定，`.gitleaks.toml` 扩展默认规则集并带唯一一条 path allowlist——放行 go/observability 脱敏测试故意种植的 secret 形状 fixture，那正是该层测试存在的意义，放行理由与残余风险写在文件注释里，真实凭据依然禁止入库）；pre-commit 钩子一半留给 dev-workflow 轮次。GitHub CodeQL（第 4 行）本轮上线（sast job：Go + TypeScript/JavaScript，Go 提取器原生支持 go.work 工作区，一次分析覆盖全部模块）。`govulncheck`（第 1 行）在 govulncheck-wiring 轮次接线：当初暂缓的两处独立原因——标准库公告属于 go.work 工具链线（已从 `1.25.0` 抬升到 1.26.8，当时最新的已修复版本，清零了那批公告）；模块类公告（经 testcontainers 测试支撑依赖触达的 golang.org/x/crypto）由各模块自己打了补丁版本——都已解除，deps-go job 现在对二十一个已实现模块逐一跑 `govulncheck@v1.1.4`。`trivy`（第 5 行）仍暂缓，仓库在发布轮产出镜像前无物可扫——证据记录在 security.yml 文件头。SBOM 与 provenance（第 6、7 行）随发布相关轮次落地。许可证扫描（下节）本轮已落地。

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

> **实施状态注记（benchmark-suite 轮次）：** 上面清单里的四组热点已随归属模块落地为可跑的 benchmark（本轮的 `feat/benchmark-suite` 分支），全部满足"`go test -bench` 直接运行、无 Docker 无网络"的约束（SQLite 走进程内临时文件，基础设施 seam 走进程内实现）：
>
> - `go/jobs` 的 `standalone_queue_bench_test.go`：单次持久化 enqueue 的写路径成本（校验、id 生成、带幂等键唯一索引检查的事务插入、日志行），以及单个任务走完 dispatcher claim → worker 执行 → 终态落库的全链路延迟——分 15ms 与默认 200ms 两种轮询周期跑，两条结果的差就是轮询粒度对端到端延迟的贡献；
> - `go/notification` 的 `delivery_bench_test.go` 与 `preference_service_bench_test.go`：投递键派生（每次投递尝试的每个 channel 都重算，canonical JSON + SHA-256），以及 `ResolveForDelivery` 的按 (收件人, 类型) 发送时偏好重查，覆盖无存储行（走类型默认值）、有存储行、轮转收件人三种真实状态；
> - `go/authn` 的 `password_bench_test.go` 与 `token_bench_test.go`：`DefaultPasswordParams`（argon2id 成本下限，OWASP 首推配置）下的哈希与校验——每次派生约 19 MiB 内存，这一条就是"提高成本前先看当前硬件的实测"的依据——以及访问令牌的签发与每请求校验（含 KeySource seam 的逐次取钥）；
> - `go/rbac` 的 `service_bench_test.go`：单次权限判定，三个子基准分别报告缓存命中的允许/拒绝（稳态的每请求代价，实测零分配）与失效后重载（一次 revoke 之后那次判定要付的完整数据库重载，正是"立即失效"形态的代价）。
>
> benchmark 随归属模块入库本身不再欠账；nightly 的回归腿（基线采集、阈值、对比告警）仍未落地，与 flaky 腿一起等实现轮次（见下节注记——当前唯一的外部前置是 `issues: write` token）。

## Flaky 测试治理

不稳定测试会侵蚀团队对 CI 的信任，最终导致"红了就重跑"的坏习惯。措施：nightly 重复运行标记不稳定用例，自动开 issue 跟踪；连续不稳定的用例先隔离（标记 skip 并挂跟踪项）再修复，不允许长期挂着一个时红时绿的 CI。

> **实施状态注记（本轮核实；benchmark-suite 轮次更新）：** `.github/workflows/nightly.yml` 仍是 gated stub（触发即在 guard step 失败，不接受任何调度），但阻塞清单已缩短到一项外部前置——性能基准回归这一半的"仓库里没有 benchmark 套件"已解除：四组模块归属的 benchmark 随 benchmark-suite 轮次落地（`go/jobs`、`go/notification`、`go/authn`、`go/rbac`，各测什么见上节实施状态注记；`grep "func Benchmark"` 遍历 `go/` 与 `examples/` 已非零命中），nightly 的文件头随之改记剩余阻塞项；flaky 检测这一半仍需要一个有 `issues: write` 权限的 token 来自动开 issue，等实现轮次落地时一并接入并过一次安全审查——这是当前唯一剩下的门。全量矩阵这一半早已不是阻塞点（`full-check` 真实存在）；benchmark 回归腿的基线/阈值机制与 flaky 腿一起随实现轮次落地，在那之前 `nightly` 整体停在 gated stub。
