# CI/CD 流水线

> 基于 GitHub Actions。本文档定义流水线的职责划分、触发时机与成本控制策略。
>
> 核心约束：**Go module 与 npm 包不能各写一套 workflow**。全部通过可复用 workflow 与 composite action 统一，否则 CI 配置本身会成为最大的维护负担。

## 流水线总览

| 流水线 | 触发 | 职责 | 目标时长 |
|---|---|---|---|
| `pr-check` | PR 打开/更新 | 受影响模块的 lint + 类型检查 + 单元测试 + 构建 | < 8 分钟 |
| `pr-full` | PR 打 `full-ci` 标签 / 合入前 | 全量矩阵：双部署模式 × 双方言、集成测试 | < 25 分钟 |
| `e2e` | 合入 main / 每日 | reference-app 端到端（Playwright） | < 20 分钟 |
| `security` | PR + 每日 | 依赖漏洞、密钥扫描、SAST、镜像扫描、许可证检查 | < 10 分钟 |
| `docs-check` | 涉及文档或公开 API 的 PR | 文档示例编译运行、链接检查、i18n key 一致性、配置清单漂移 | < 5 分钟 |
| `api-contract` | 涉及 `api/openapi.yaml` 或 handler 的 PR | spec lint、合并冲突检查、**生成物一致性 diff**、**oasdiff 破坏性变更检测** | < 5 分钟 |
| `scaffold-verify` | 每日 + 发布后 | CLI 生成全新项目 → 构建 → 两种部署模式各启动一次 → 冒烟 | < 15 分钟 |
| `release` | 手动触发（指定版本号） | lockstep 全量发布：Go module 逐个打 tag + npm 包逐个发布 + 镜像 + CLI 二进制 | < 30 分钟 |
| `nightly` | 每日 | 全量矩阵 + 性能基准回归 + flaky test 检测 | 不限 |

## 成本控制：不是每个 PR 都跑全量

模块数量 × 2 种部署模式 × 2 方言，相乘后组合数太大，每个 PR 全跑既慢又贵。策略：

1. **路径过滤**（`dorny/paths-filter`）：只跑改动模块及其**下游依赖**模块。依赖关系从 `go.work` 与 workspace 配置自动推导，不手工维护映射表。
2. **分层触发**：PR 阶段跑快速检查（单进程部署模式 + SQLite，无需容器）；合入前跑全量矩阵。这利用了单进程部署模式的一个副产品优势——大部分测试不需要 testcontainers 就能跑。
3. **缓存**：Go module cache、pnpm store、Docker layer、golangci-lint cache 全部启用，按 lockfile 哈希做 key。
4. **并发控制**：同一 PR 的新推送自动取消旧运行（`concurrency` + `cancel-in-progress`）。
5. **超时**：每个 job 设 timeout，防止挂死消耗额度。

## 可复用 workflow 设计

```
.github/
  workflows/
    pr-check.yml            # 编排：调用下面的可复用 workflow
    release.yml
    ...
  workflows/reusable/
    go-module-ci.yml        # 输入：模块路径 → lint/test/build
    npm-package-ci.yml      # 输入：包路径 → lint/typecheck/test/build
    docker-build.yml        # 多架构镜像构建
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
| 业务逻辑中禁止 `if mode == "standalone"` 分支 | `semgrep` 规则（仅放行 kernel 装配包） |
| 业务模块间禁止跨模块 import struct | `go-arch-lint` 或 `depguard`（golangci-lint 插件） |
| `rbac` 不得依赖 `authn` | `depguard` 依赖白名单 |
| 业务代码不得 import 具体基础设施 SDK | `depguard`（禁止 `go-redis`、S3 SDK 等出现在业务模块） |
| `tenant_id` 不得作为 Prometheus label | `semgrep` 规则 + 运行时断言测试 |
| UI 包内禁止裸文本节点 | `eslint-plugin-i18next` 或自定义 ESLint 规则 |
| 中英资源 key 集合必须一致 | 自研脚本，diff 两份 JSON/TOML 的 key 集合 |
| 除 `docs/internal/` 外禁止 CJK 字符 | 自研脚本扫描 `.md` 与代码注释（i18n 资源与 `docs/site/` 本地化目录除外） |
| 日志消息必须是常量字符串 | `semgrep`：禁止 `log.*(fmt.Sprintf(...))` 与字符串拼接作为消息 |
| 日志字段名规范 | `semgrep`：属性 key 必须是 `snake_case` 字面量 |
| 前端禁止手写 API 调用 | ESLint 自定义规则：除 `@speed/api-client` 内部外禁止 `fetch`/`axios` 指向后端路径 |
| spec 与实现必须一致 | 生成的 server interface 参与编译；CI 重新生成并 diff，不一致即失败 |
| operationId / schema 命名规范 | redocly lint 自定义规则 |
| 系统上下文只能由白名单模块调用 | 人工评审 + CODEOWNERS（`go/pkgcore`、`go/tenancy`）+ 函数级文档约定；**不是** `depguard`——`WithSystemContext` 与 `TenantID`/`WithTenant`/`apperr` 同属 `pkgcore` 根包同一个 import path，depguard 只能按包路径粒度放行/拒绝，做不到只挡一个符号。已实测验证：把「仅 admin/compliance/jobs/authn/`tenancy` 可 import `pkgcore`」接成 depguard 规则，会连带拦下 `go/dbkit`（真实代码、不在白名单、但合法依赖 `TenantID` 等）23 处无关导入，草稿规则因此未合入。`tenancy` 现已建成，提供审计封装版 `tenancy.WithSystemContext`，业务代码应调用它而非直接调用原语；要让这条纪律真正可静态检查，需要先把 `WithSystemContext` 迁到 `pkgcore` 独立子包（类似 `apperr/`、`config/`），这是一次公开 API 决策，超出本表列出的自动化检查范围 |
| 禁止手写 `WHERE tenant_id = ?` | `semgrep`：租户过滤只能由插件与 Repository 注入，手写即意味着绕过防护 |
| API 层不得接受外部传入的 `tenant_id` | `semgrep` + spec lint：请求参数/请求体中出现 `tenant_id` 字段即拒绝 |
| 禁止 `AutoMigrate` | `semgrep`：生产迁移必须是版本化 SQL |
| 禁止跨模块数据库外键 | 迁移文件 lint：`REFERENCES` 目标表必须属于同一模块 |
| 日志与响应不得输出明文 PII | `semgrep` 检查敏感字段直接进日志；配合脱敏中间件的单元测试 |
| `@speed/api-sdk` 不得手改 | 生成物 diff（改了会被下次生成覆盖，CI 提前拦截） |
| 每个 Repository 必须跑隔离测试 | 自研脚本：扫描 Repository 实现，比对测试覆盖清单 |
| 每个模块的 `go.mod` 必须能脱离 `go.work` 独立构建 / `tidy`（否则 `replace` 了却漏加 `require` 这类问题会被 workspace 的隐式路径解析掩盖，只有真实消费方或首次 lockstep 发布删掉过渡期 `replace` 行后才会暴露，见 [02 仓库结构与发布](02-repo-and-release.md)） | 过渡手段：`go/tenancy` 的 `TestModuleBuildsStandaloneOutsideWorkspace`（`standalone_build_test.go`）用 `os/exec` 以 `GOWORK=off` 跑一遍该模块自己的 `go build ./...`/`go vet ./...`，随 `go test ./...` 默认执行，不依赖任何 CI 配置；真正的 CI 落地后应替换为流水线里对每个模块单独执行的 `GOWORK=off` 构建步骤 |

**这张表是 CI 的核心价值所在**——纪律靠人记会在三个月后失效，靠 CI 才能长期有效。

## 发布流水线（lockstep）

手动触发并输入版本号（如 `v1.2.0`），流水线按序执行：

1. **前置校验**：main 分支绿灯、工作区干净、版本号未被占用、changelog 已生成
2. **全量测试**：跑一次完整矩阵，任何失败即终止
3. **Go 发布**：为每个 Go module 目录各打一个 `go/<module>/v1.2.0` 格式的 tag —— **必须脚本化**，手工逐个打 tag 一定会漏。**首次发布还要多一步**：扫描并清理所有模块 `go.mod` 里指向仓内其他模块的临时 `replace ... => ../<module>` 行（见 [02 仓库结构与发布](02-repo-and-release.md) 的过渡状态说明），替换成刚打好的真实版本号，再跑一次 `go mod tidy` 确认——遗漏任何一条都会导致业务方 `go get` 时因为 `replace` 指向本地路径而直接失败。
4. **npm 发布**：changesets fixed 版本组统一升版并发布，附带 provenance
5. **制品**：多架构（amd64/arm64）Docker 镜像、`saasctl` 多平台二进制（goreleaser）、**合并后的 OpenAPI 规范 `speed.yaml`**（Release 附件 + 打包进 `@speed/api-sdk` + 发布到文档站对应版本目录）
6. **发布后验证**：触发 `scaffold-verify`，用刚发布的版本生成全新项目并跑通，**失败则立即标记该版本为不可用**
7. **产出**：GitHub Release + changelog + SBOM

### 预发布通道
破坏性变更或大版本前，先发 `v1.2.0-rc.1` 到预发布通道，由 reference-app 先行验证，再发正式版。

### 回滚
已发布的 Go module tag 无法真正撤回（module proxy 会缓存），因此**回滚 = 发布修复版本**，不是删 tag。npm 可以 deprecate 但同样不删除。这一点必须在发布流程文档中写明，避免有人试图删 tag 导致更混乱的状态。

## 分支保护

- `main` 分支禁止直接推送，必须经 PR
- 必需检查项：`pr-check`、`security`、`docs-check` 全绿
- 至少一位 reviewer 批准；涉及 `pkgcore`/`dbkit`/`tenancy` 等地基模块时需要指定 owner 批准（CODEOWNERS）
- **合并方式：仅允许 fast-forward**（与团队 Git 规范一致，见 [19 开发工作流](19-dev-workflow.md)）
