# 仓库结构与发布

> monorepo 布局、统一版本号策略、以及模块如何独立发布给业务项目使用。

单仓库 `speed`，Go workspace + pnpm workspace 统一开发，各模块独立发布。

```
speed/
  go.work
  go/
    pkgcore/  dbkit/  observability/  ratelimit/  tenancy/  config/
    jobs/  storage/  notification/  authn/  rbac/  org/  metering/
    billing/  billing-gateway/  ai-gateway/  sharing/
    integration/  compliance/  admin/  saasctl/
                                              # 各含独立 go.mod
                                              # 每个模块内含 docs/、AGENTS.md、
                                              # api/openapi.yaml、locales/、migrations/
  web/
    packages/{tokens,i18n,ui-kit,api-client,api-sdk,auth-core,auth-ui,
              tenancy-ui,billing-core,billing-ui,
              notification-core,notification-ui,
              layout-kit,product-shell,admin-shell}/
                                              # api-sdk 为 OpenAPI 生成物，禁止手改
    create-saas-app/                           # Node CLI + 前端模板
  templates/
    backend-app/  frontend-product/  frontend-admin/
  examples/
    reference-app/                             # 强制验证消费者
  deploy/
    docker-compose.standalone.yml     # 单容器，SQLite，零外部依赖
    docker-compose.yml                # app + postgres + redis
    docker-compose.observability.yml  # 叠加 LGTM 栈
    docker-compose.dev-tools.yml      # 可选：MinIO / MailHog / 支付沙箱
    grafana/{provisioning,dashboards}/
  docs/
    internal/                                 # 内部设计文档（本目录）
    adr/                                      # 架构决策记录
    upgrade/                                  # 版本升级指南
    site/                                     # 面向业务方的公开文档站
  build/openapi/speed.yaml                    # 各模块 spec 的合并产物，发布物之一
  .github/workflows/                          # CI/CD 流水线
  CLAUDE.md                                   # 仓库级架构纪律与上手指引
```

## 版本策略：全模块统一版本号（lockstep）

**所有 Go module 与 npm 包共用同一个版本号，同时发布；只保证同版本模块之间的兼容性。** 不做跨版本兼容矩阵，不支持"tenancy v1.2 配 billing v1.5"这类混搭。

这样换来的简化是决定性的：
- 发布流程退化为"打一次版本、全量发布"，不需要判断哪些模块受影响、要不要跟版
- CI 只需验证一种组合，省掉指数级的兼容性测试矩阵
- 排查线上问题时，"你们用的什么版本"是一个数字而不是一张表
- 模块间互相依赖时直接锁定同版本，不会出现菱形依赖冲突

代价与配套措施：
- 业务项目升级必须**整体升级**所有 `speed` 模块。提供 `saasctl upgrade` 一次性改写 go.mod / package.json 全部相关依赖到目标版本，并跑一次兼容性自检。
- 某个模块没有任何改动时也会跟着发一个新版本（changelog 里标注 "no changes"），这是可接受的噪音。
- 破坏性变更集中在大版本，配 `docs/upgrade/vX-to-vY.md` 升级指南。

**Go 多模块发布**：module path 形如 `github.com/<org>/speed/go/tenancy`，版本 tag 必须用子目录前缀格式 `go/tenancy/v0.1.0`（Go 官方多模块仓库规范）。统一版本意味着一次发布要为每个 Go module 目录各打一个同版本号的 tag——**必须脚本化**，手工打 tag 一定会出错。

> **首次发布前的过渡状态**（Round 2 实现 `dbkit` 依赖 `pkgcore` 时确认）：在第一次 lockstep 发布、也就是 `pkgcore` 还没有任何 git tag 之前，`go build`/`go test` 能通过 `go.work` 的隐式本地路径解析直接工作，但 `go mod tidy` 不认这个——它按"脱离 workspace 也要能独立解析出一个可下载版本"的语义处理新增依赖，找不到 tag 会直接报错。过渡期做法是在依赖方的 `go.mod` 里加一行：
> ```
> replace github.com/vislake/speed/go/pkgcore => ../pkgcore
> ```
> 这条 `replace` 只是让 `go mod tidy` 满意，不影响 `go build`/`go test`（它们本来就用 `go.work` 解析，会忽略这条 replace）。**首次 lockstep 发布为所有模块打完 tag 后，必须清理掉所有这类临时 `replace` 行**，改为要求方 `go.mod` 里的真实版本号——发布脚本需要包含这一步，不能只顾打 tag 不管清理跨模块依赖引用。每个新增"依赖仓内另一个尚未发布模块"的模块都会遇到同样的情况，不是 `dbkit` 独有的问题。

**M0 状态注记（lockstep 发布脚本轮，对应 roadmap M0 的"changesets / lockstep 发布脚本"条目）：** 上面的设计保持不变；M0 按"离线验证优先"落地了它的前半，真实发布明确排到 v1.0（M4）。

- **已落地——发布协调器（离线验证）**：`tools/release/lockstep-release.py`（纯标准库，自带 unittest 套件）在运行时推导可发布集合——Go 侧为 go.work `use` 条目（`use` 条目本身就是模块的发布登记，见 [19 开发工作流](19-dev-workflow.md) 的 M0 注记），npm 侧为 `web/packages/*`——打印完整单版本计划（每个模块将获得的 `go/<module>/<version>` tag、每个包经 changesets fixed 组将 bump 到的版本），退出码 0 **仅当**计划一致：版本号符合 `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`（v 必需）、该版本无既有 tag、go.work 与 `go/` 目录树双向完备、npm 版本统一、`web/.changeset/config.json` 的 fixed 组恰好覆盖现存包。入口：`task release:plan VERSION=v1.2.0`；`.github/workflows/release.yml` 手动触发时先校验版本号格式，再跑协调器默认模式与 `--self-test`。
- **已落地——npm 侧 bootstrap**：`web/.changeset/config.json` 的 fixed 组覆盖现存三包（`@speed/i18n`、`@speed/tokens`、`@speed/ui-kit`），保证 npm 侧"全包同版"；changesets 本体未安装，无条目、无 bump 运行，覆盖一致性由协调器校验而非 changesets 保证（见 `web/.changeset/README.md`）。
- **明确排到 v1.0（M4）**：真实发布——推 tag、changesets version/bump、`npm publish`、GitHub Release 与发布产物。release.yml 只读（`permissions: contents: read`），不接任何发布凭据；协调器 `--apply` 模式硬闸在 `--allow-local-tag-creation`（仅创建本地、永不推送的 tag，只用于在 scratch checkout 上演练打 tag 半程）。上面 blockquote 要求的过渡态 replace 清理以纯函数 + `tools/release/testdata/` 夹具交付并**严禁对真实 go.mod 运行**——树的过渡态保留到 v1.0。
- **集合判定机制的变化**：哪些模块属于可发布集合，从"人工维护的每模块 tag 列表"改为 go.work `use` 条目本身——漏登记模块会让发布验证直接失败（完备性 drift 检查），不再可能悄悄漏 tag。`examples/reference-app` 被显式排除在可发布集合之外：它是仓库的消费者模块（保持消费者 go.mod 与 `replace` 行），从不被打 tag 或发布。

**npm 发布**：changesets 配置为 fixed 版本组（所有包锁在一起同步升版），与 Go 侧共用同一版本号。`react`/`react-dom`/`@mui/material`/`@emotion/*` 一律声明为 peerDependencies，避免下游出现多份 React/MUI 实例。

**CLI 分工**：`saasctl`（Go，`go:embed` 后端模板）生成后端骨架并提供 `saasctl db migrate`；`create-saas-app`（Node）生成前端骨架。各自用本生态原生方式分发，不强行统一。

