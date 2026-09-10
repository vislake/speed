# 仓库结构与发布

> monorepo 布局、统一版本号策略、以及模块如何独立发布给业务项目使用。

单仓库 `speed`，Go workspace + pnpm workspace 统一开发，各模块独立发布。

```
speed/
  go.work
  go/
    pkgcore/  dbkit/  observability/  ratelimit/  tenancy/  config/
    jobs/  storage/  notification/  authn/  rbac/  org/  metering/
    billing/  ai-gateway/  sharing/          # billing/gateway/ 是子包，不是模块
    integration/  compliance/  admin/  pki/  saasctl/
                                              # 各含独立 go.mod 与 AGENTS.md；
                                              # api/openapi.yaml、locales/、
                                              # migrations/ 视模块是否落地
                                              # HTTP 面/i18n 文案/迁移而定，
                                              # 非全员标配
  web/
    packages/{tokens,i18n,ui-kit,api-client,api-sdk,auth-core,auth-ui,
              account-ui,tenancy-ui,layout-kit,product-shell}/
                                              # 上列包均已实现；billing-core、
                                              # billing-ui、notification-core、
                                              # notification-ui、admin-shell
                                              # 尚未实现
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
    docker-compose.dev-tools.yml      # 可选：RustFS / MailHog / 支付沙箱
    grafana/{provisioning,dashboards}/
  docs/
    internal/                                 # 内部设计文档（本目录）
    adr/                                      # 架构决策记录
    upgrade/                                  # 版本升级指南
    site/                                     # 面向业务方的公开文档站
  contracts/speed.yaml                        # 各模块 spec 的合并产物，发布物之一
  .github/workflows/                          # CI/CD 流水线
  CLAUDE.md                                   # 仓库级架构纪律与上手指引
```

**布局树与真实目录结构对照：**

- **布局树的 `deploy/` 一节不存在于仓库中**：根目录没有 `docker-compose*.yml`，
  `grafana/` 编排目录也不存在。镜像构建走 `.github/workflows/reusable-docker-build.yml`
  （reusable workflow，由 `docker-image-ci.yml` 调用——后者在 push 到 main 且改动
  命中其构建输入路径集时自动触发，另支持 `workflow_dispatch` 手动运行；路径集见
  其文件头），构建 `examples/reference-app/Dockerfile`（见 [18 CI/CD](18-cicd.md)）。`task dev`
  按根 `CLAUDE.md` 与 [19 开发工作流](19-dev-workflow.md) 的约定跑单进程
  standalone 模式、SQLite、零外部依赖，不依赖 `docker compose`；分布式模式和
  可观测性栈的编排材料未落地，布局树里的 `deploy/` 一行按"规划"读，不按
  "现状"读。
- **"每个模块内含 docs/、AGENTS.md、api/openapi.yaml、locales/、migrations/"
  这句对现存 Go 模块并不成立**：`AGENTS.md` 与 `go.mod`（即"各含独立 go.mod"）
  是全员标配，但 `docs/` 子目录没有任何模块建立——模块自身文档就是它的
  `AGENTS.md`；`api/` 只存在于落地了 HTTP 面的模块；`locales/` 只存在于需要
  下发 i18n 文案的模块；`migrations/`（含子目录嵌套的迁移目录）只存在于拥有
  数据库表的模块。布局树这行按"落地了对应能力就会长出这个子目录"读，不按
  "每个模块开工时就会有"读。
- **`web/packages` 树只列已实现的包**：布局树注释中列出的包均已实现；
  `billing-core`、`billing-ui`、`notification-core`、`notification-ui`、
  `admin-shell` 等未实现包单列在树注释里标注，不与已实现包混列。

## 版本策略：全模块统一版本号（lockstep）

**所有 Go module 与 npm 包共用同一个版本号，同时发布；只保证同版本模块之间的兼容性。** 不做跨版本兼容矩阵，不支持"tenancy v1.2 配 billing v1.5"这类混搭。

这样换来的简化是决定性的：
- 发布流程退化为"打一次版本、全量发布"，不需要判断哪些模块受影响、要不要跟版
- CI 只需验证一种组合，省掉指数级的兼容性测试矩阵
- 排查线上问题时，"你们用的什么版本"是一个数字而不是一张表
- 模块间互相依赖时直接锁定同版本，不会出现菱形依赖冲突

代价与配套措施：
- 业务项目升级必须**整体升级**所有 `speed` 模块。提供 `saasctl upgrade` 一次性改写 go.mod / package.json 全部相关依赖到目标版本（改写面目前以 go.mod 为限，package.json 侧见下文的落地形态），改写结果写入前经结构自检。
- 某个模块没有任何改动时也会跟着发一个新版本（changelog 里标注 "no changes"），这是可接受的噪音。
- 破坏性变更集中在大版本，配 `docs/upgrade/vX-to-vY.md` 升级指南。

**Go 多模块发布**：module path 形如 `github.com/<org>/speed/go/tenancy`，版本 tag 必须用子目录前缀格式 `go/tenancy/v0.1.0`（Go 官方多模块仓库规范）。统一版本意味着一次发布要为每个 Go module 目录各打一个同版本号的 tag——**必须脚本化**，手工打 tag 一定会出错。

> **可用发布前的过渡状态**：在没有任何可用发布、`pkgcore` 没有存活 git tag 的当前阶段，`go build`/`go test` 能通过 `go.work` 的隐式本地路径解析直接工作，但 `go mod tidy` 不认这个——它按"脱离 workspace 也要能独立解析出一个可下载版本"的语义处理新增依赖，找不到 tag 会直接报错。过渡期做法是在依赖方的 `go.mod` 里加一行：
> ```
> replace github.com/vislake/speed/go/pkgcore => ../pkgcore
> ```
> 这条 `replace` 只是让 `go mod tidy` 满意，不影响 `go build`/`go test`（它们本来就用 `go.work` 解析，会忽略这条 replace）。这类临时 `replace` 行的清理点随发布落地（v0.0.1 首发前的清理已随发布提交落过一轮；首发作废后各模块追加的 replace 行随下一个版本号的发布再清理），届时改为要求方 `go.mod` 里的真实版本号。每个新增"依赖仓内另一个尚未发布模块"的模块都会遇到同样的情况，不是 `dbkit` 独有的问题。

**发布机制的落地形态：离线验证与真实发布的执行腿已落地（v0.0.1 首发，2026-09-10）；真实发布类其余事项随 v1.0（M4），清单见 [24 延迟事项路线图](24-deferral-roadmap.md) 第 5 章。**

- **发布协调器（离线验证）**：`tools/release/lockstep-release.py`（纯标准库，自带 unittest 套件）在运行时推导可发布集合——Go 侧为 go.work `use` 条目（`use` 条目本身就是模块的发布登记，见 [19 开发工作流](19-dev-workflow.md)），npm 侧为 `web/packages/*`——打印完整单版本计划（每个模块将获得的 `go/<module>/<version>` tag、仓库根 tag `<version>` 与每个包经 changesets fixed 组将 bump 到的版本），退出码 0 **仅当**计划一致：版本号符合 `^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`（v 必需）、go.work 与 `go/` 目录树双向完备、npm 版本统一、`web/.changeset/config.json` 的 fixed 组恰好覆盖现存包；该版本已存在的 tag（模块 tag 或根 tag）只警告不失败——同一版本重发（部分完成态的恢复路径）是受支持场景。入口：`task release:plan VERSION=v1.2.0`；`.github/workflows/release.yml` 手动触发时先校验版本号格式，再跑协调器默认模式与 `--self-test`，随后由 publish job 执行真实发布。
- **同一版本的 tag 语义（模块 tag 与根 tag 并存、互不替代，同版本 sha 不同是设计）**：一次发布在仓库里留下两组同版本号 tag——
  - **模块 tag `go/<module>/<version>`**（如 `go/tenancy/v0.0.1`）：Go module proxy 的解析面，钉在模块的发布提交上；已发布的模块 tag 永不重打、永不移动（模块代理会缓存 tag，移动会让已解析的消费者拿到不同内容）。v0.0.1 的 21 个模块 tag 发布时全部指向 `fbaaaf98`，后随该版本作废从远端与本地删除（删 tag 不动模块代理已缓存的产物）。
  - **根 tag `<version>`**（如 `v0.0.1`，不带任何前缀）：版本里程碑参照——"该仓库处于 vX.Y.Z" 的单一仓库级 tag。它在发布运行的 checkout 上创建（即 dispatch 时的 main 尖端），因此对早期版本的补发运行而言，它可能指向发布之后的修复提交，与模块 tag 不在同一提交上。**同一版本根 tag 与模块 tag 的 sha 不同是设计使然，不是漂移或事故**——根 tag 标记"版本里程碑于何时记录"，模块 tag 承诺"发布的内容是什么"，角色不同所以两者共存，谁也不替代谁。
  发布流水线对两者一视同仁地执行"已存在即跳过"（部分完成态的补发运行跳过已推送者；见下）；协调器把根 tag 纳入与模块 tag 相同的碰撞检查与本地演练集合。
- **可发布集合的判定**：由 go.work `use` 条目本身决定——漏登记模块会让发布验证直接失败（完备性 drift 检查），不会悄悄漏 tag。`examples/reference-app` 被显式排除在可发布集合之外：它是仓库的消费者模块（保持消费者 go.mod 与 `replace` 行），从不被打 tag 或发布。
- **真实发布的执行腿（v0.0.1 落地）**：`release.yml` 由只读验证扩展为"验证 + 发布"两 job；写权限（contents: write 推 tag、packages: write 发 npm）只挂 publish job。tag 步用协调器自己的解析器从 go.work 推导模块集，把模块 tag 与根 tag 一并推送，已存在的任一 tag 一律跳过（同一版本的补发运行跳过已推送者、可走完 Go 半程，已发布 tag 永不重打或移动）。npm 步按依赖序逐包发布，发布前先以 `npm view @speed/<package>@<version>` 探测 GitHub Packages registry，已存在的版本跳过——任意"前 k 包已上"的部分发布态都能收敛，且任何版本不会被发布两次（双重发布防护由两侧的 skip 承担）。v0.0.1 首轮运行半成功：Go 21 个模块 tag 曾推送发布（后随该版本作废从远端与本地删除、版本号不再复用），npm 十二包因 `@speed` scope 未关联仓库 owner 的 GitHub Packages 安装而全部 403（仓库外 org 配置，非代码缺陷，十二包零上 registry）；org 侧关联后，由下一个版本号的发布完整收敛。协调器 `--apply` 模式保持硬闸在 `--allow-local-tag-creation`（仅创建本地、永不推送的 tag，只用于在 scratch checkout 上演练打 tag 半程）；上面 blockquote 要求的过渡态 replace 清理以纯函数 + `tools/release/testdata/` 夹具交付并**严禁对真实 go.mod 运行**——首次发布的清理由独立提交在 dispatch 前落地（见 release.yml 的 guard 注释）。

**npm 发布**：changesets 配置为 fixed 版本组（所有包锁在一起同步升版），与 Go 侧共用同一版本号——`web/.changeset/config.json` 的 fixed 组覆盖现存全部 `@speed/*` 包；changesets 本体未安装，无条目、无 bump 运行，覆盖一致性由协调器校验而非 changesets 保证（见 `web/.changeset/README.md`）。`react`/`react-dom`/`@mui/material`/`@emotion/*` 一律声明为 peerDependencies，避免下游出现多份 React/MUI 实例。

**CLI 分工**：`saasctl`（Go，`go:embed` 后端模板）生成后端骨架并提供 `saasctl db migrate`；`create-saas-app`（Node）生成前端骨架。各自用本生态原生方式分发，不强行统一。

**CLI 的落地形态：** Go 半边是 `go/saasctl`——`new`（后端骨架生成）、`upgrade`（go.mod 版本改写）、`db migrate`（SQLite 迁移应用）与 `config print`（引导配置来源展示）四个命令全部接线；`create-saas-app` 与前端模板尚未实现。落地对正文有四处如实偏差或精确化：

- **布局树的 `templates/` 一节不存在于仓库中**：后端模板不以 `templates/backend-app/…` 存放，而是 `go:embed` 收在 `go/saasctl/internal/template/project/` 下——一个自带模板的可执行二进制，业务方 `saasctl new` 时无需另拉模板仓库，模板的"可编译性由真实 materialize + tidy + build 证明、绝不就地编译"由 go:embed 布局与 `//go:build ignore` 标记双重保证。前端模板尚未实现，存放处未定；布局树的 `templates/` 一行按"规划"读，不按"现状"读。
- **脚手架过渡态形态**：`saasctl new` 生成的项目 go.mod 不写 `replace ... => ../pkgcore` 这种相对路径，而是——speed require 的版本串以 replace 覆盖为准、混合取值：模板自身的 require 行钉零占位版本 `v0.0.0-00010101000000-000000000000`，被图中另一 speed 模块按真实版本 require 的模块经 MVS 留在该真实版本——现为 `v0.0.1`：dbkit、observability、pkgcore、tenancy，以及间接的 ratelimit、jobs（jobs 随含 authn 的选集）；每条 replace 指向 `--speed-root` 解析出的 speed checkout 绝对路径（`SPEED_ROOT`、ancestor go.work probe 依次兜底），完整的间接 require 块、不随 go.sum（首次 consumer 侧 `go mod tidy` 写出的 go.sum 不触碰这些行）——模板内嵌 go.mod 是 tidy-pruned 的 golden，materialize 后按字面替换。上文 blockquote 的"临时 replace 清理"对脚手架生成物同样适用，清理点随下一个版本号的发布落地（届时 `new` 改为生成真实版本 requires）。
- **`saasctl upgrade` 的改写面**：`upgrade` 只改写 go.mod——`golang.org/x/mod/modfile` 仅重写每条 speed require 的版本 token，replace 块、`// indirect` 标记、注释与格式逐字节保留，幂等；package.json 侧的改写随 `create-saas-app` 一并实现。
- **分发形态**：`saasctl` 以 goreleaser 多平台二进制发布（真实发布流水线的制品步骤），`create-saas-app` 以 npm 包发布——各自原生分发；`saasctl` 需要本地 speed checkout（见 `go/saasctl/AGENTS.md` 的 Speed-root resolution）。

每个 `new` 出来的项目都经真实 tidy/build/boot/冒烟（`db migrate` 与 `upgrade` 跑真实文件），记录在 `go/saasctl/AGENTS.md` Testing 章节；该过程的 CI 化形态是 scaffold-verify 流水线（见 [18 CI/CD](18-cicd.md)）。

