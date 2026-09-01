# 质量与安全工程

> 测试分层、代码质量门槛、供应链与运行时安全。这些要求对脚手架比对普通业务项目更严格——一个缺陷会被复制到每一个基于它交付的项目。

## 测试分层

| 层次 | 范围 | 依赖 | 何时跑 |
|---|---|---|---|
| 单元 | 单个包内的逻辑 | 无外部依赖（用 demo 形态的内存实现作 test double） | 每次提交 |
| 集成 | 模块与真实基础设施 | testcontainers 拉 PostgreSQL / Redis | PR 合入前 |
| 契约 | OpenAPI 规范与实现、前后端类型（见 [21 API 契约](21-api-contract.md)） | 无 | 每次提交 |
| 端到端 | reference-app 完整业务链路 | 完整 compose 环境 | 合入 main / 每日 |

**demo 形态的内存实现同时就是 test double**，不需要为测试再造一套 mock。这是双形态设计的第二个正收益（第一个是演示轻量），也是为什么大部分单元测试能在 CI 上秒级跑完。

### 前端测试
Vitest + Testing Library 做组件与 hook 测试；Playwright 做 e2e。UI 包的每个公开组件需有 Storybook story（同时充当文档与视觉回归基线）。

### 文件与目录布局

这是硬性约定，不是风格偏好——目的是让 `go test ./...`（默认只跑单元测试，秒级完成）和 `go test -tags=integration ./...`（显式触发，允许慢）这条分层在物理上可执行，而不是靠开发者自觉：

- **单元测试文件名必须是被测目标的前缀**：`registry.go` 对应 `registry_test.go`，`kv.go` 对应 `kv_test.go`（Go 原生约定）；前端同理，`PlanCard.tsx` 对应 `PlanCard.test.tsx`。不与单个源文件一一对应的测试文件，必须用它验证的行为语义命名（如 `concurrency_test.go`），禁止用 `misc`/`extra`/`independent` 这类不表意的名字——名字是未来定位测试的第一手段，含糊的名字让这个手段失效。
- **`example_test.go` 是 Go 惯例的例外**：godoc 可渲染的 `Example*` 函数按约定放在这个文件名下，不受"按目标命名"规则约束。
- **测试工具与帮助类放在独立的测试目录**：Go 侧是模块内的 `internal/testutil` 子包，前端侧是每个包的 `test-utils/` 目录；跨测试文件复用的 fake、builder、断言辅助函数都放这里，不允许在 `_test.go` 里内联重复定义（Go 的 `_test.go` 本身也无法被其他包 import，这是该约定的硬约束，不只是风格要求）。
- **集成测试与单元测试物理分离**：Go 侧每个模块用 `integration_test/` 子目录 + `//go:build integration` 构建标签；前端侧用 Playwright 原生的 `e2e/` 目录。任何一次普通的单元测试运行都不会碰到集成测试。

### 必须存在的专项测试套件
这几项在 [16 验证方式](16-verification.md) 中有详细验收标准，工程上要求它们是**可复用的测试套件**而非散落的用例：

- `tenancytest.AssertIsolated` —— 租户隔离，所有 Repository 必跑
- 双形态一致性 —— 同一组用例在 demo 与 production 下结果必须一致
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

### 覆盖率
不设一刀切的百分比门槛（容易催生无意义的测试），而是：
- 地基模块（`pkgcore`、`dbkit`、`tenancy`、`rbac`、`billing`、`jobs`）要求较高覆盖，且**覆盖率不允许下降**（与基线比对）
- 安全相关路径（租户隔离、权限判定、支付回调、令牌校验）要求分支覆盖完整

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

### 许可证合规（对本项目尤其重要）
脚手架会被用于**对外商业交付**，依赖的许可证会传导给客户项目。CI 中做许可证扫描，**禁止引入 GPL/AGPL 系依赖**；MPL/LGPL 类需单独评估并记录在 ADR 中。这一条在纯内部项目里可以放松，在这里不行。

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
- 仓库内**不得出现任何真实凭证**，包括测试用的沙箱密钥
- CI 密钥用 GitHub Environments 管理，发布流水线的密钥限定在受保护环境并要求人工审批
- 本地开发用 `.env.local`（已在 `.gitignore`），`.env.example` 只放占位符

## 性能基准

对以下热点建立 benchmark 并在 nightly 中做回归检测（相比基线劣化超过阈值即告警）：

- 租户过滤的 Repository 查询开销
- 权限判定（Casbin）的单次判定耗时与缓存命中率
- 动态配置读取（必须走进程内缓存，不能每次查库）
- 会话校验（尤其"立即失效"模式下每请求一次 KV 查询的开销 —— 这个数据决定该模式是否值得默认开启）
- 计量事件采集的吞吐与延迟

## Flaky 测试治理

不稳定测试会侵蚀团队对 CI 的信任，最终导致"红了就重跑"的坏习惯。措施：nightly 重复运行标记不稳定用例，自动开 issue 跟踪；连续不稳定的用例先隔离（标记 skip 并挂跟踪项）再修复，不允许长期挂着一个时红时绿的 CI。
