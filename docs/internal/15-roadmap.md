# 里程碑与交付计划

> 内部按里程碑推进，对外一次性发布 v1.0。

> 每个里程碑的**出口条件都包含 `examples/reference-app` 接入并跑通**。模块 API 没有被真实消费者用起来，不算完成。这是在没有外部试点项目情况下防止闭门造车的唯一手段。

| 里程碑 | 周期 | 内容 | 出口条件 |
|---|---|---|---|
| **M0 地基** | 6-7 周 | **工程基建**（go.work/pnpm workspace/changesets、Taskfile 与 mise 工具链、可复用 CI workflow、架构纪律的 semgrep/depguard 规则、lockstep 发布脚本、安全与许可证扫描、模块生成器）、`pkgcore`（含 **部署模式机制 + 全部基础设施接口与各自的多套实现**：KVStore、EventBus、ObjectStore、Mailer，以及 **i18n 消息注册机制**、**引导配置 koanf 封装**）、`dbkit`（含**字段级加密 Serializer 与盲索引机制**、**PII 日志脱敏**——数据保护必须在第一条数据落库前就位）、`ratelimit`（独立限流原语，M1 `authn` 认证端点上线前必须就绪，见下方说明）、**文档工程化**（模板、`AGENTS.md` 规范、示例编译检查、文档站骨架）、**API 契约工具链**（spec 合并、oapi-codegen / orval 生成、oasdiff 闸门、`@speed/api-sdk` 骨架）、`@speed/tokens`、`@speed/i18n`、`ui-kit`（主题工厂+6个核心组件，**全部中英双语**）、`api-client` | 双方言 DB 测试矩阵跑通；**同一份代码在单进程与分布式两种部署模式下均可启动并通过测试**；i18n lint、文档示例编译检查、契约生成物一致性检查在 CI 生效；改 spec 后不改实现必须编译失败；一次命令能把全部模块以同一版本号发布出去 |
| **M1 租户与身份** | 6-7 周 | `tenancy`（租户解析/隔离三重防护/**系统上下文与白名单**/隔离测试套件）、`config`（动态配置+租户级覆盖+热更新+schema 注册）、**`jobs`（异步任务队列，多套实现）**、`authn`（密码+JWT+OIDC RP+**社交登录 Google/GitHub/微信/钉钉/飞书**+**手机号短信登录**+**MFA（TOTP + 恢复码 + step-up）**+账号绑定管理+**登录日志与会话管理（设备列表/单个下线/全部下线/refresh 轮换与重放检测）**）、`rbac`、`org`（**含多层级组织树与子树数据范围**）、**功能开关注册表与依赖校验**、**审计基础设施**（AuditEvent 模型 + GORM 回调自动采集 + 落库，高级特性留到 M4）、`observability`（OTel+HTTP中间件+Prometheus+Grafana）；`auth-core`/`auth-ui`/`tenancy-ui`/`layout-kit`/`product-shell`；`saasctl` + `create-saas-app` v0.1 | reference-app 跑通「三种登录入口→建集团与门店（多层级）→邀请成员→按权限与子树范围显示数据→绑定解绑第三方账号」；异步任务能提交、重试、查进度；**能查看登录历史、看到已登录设备列表并把其中一台下线**；按最小可用组合（必需模块 + authn + rbac + org）构建时应用仍能正常启动；通过租户隔离测试套件 |
| **M2 媒体与变现** | 6-7 周 | **`storage`**（预签名直传+EXIF剥离+异步派生+生命周期）、**`notification`**（模板+双语+**站内信与 SSE 实时推送**+**类型×渠道偏好矩阵**+**外部联系人同意验证**+异步投递）、`@speed/notification-core` + `notification-ui`、`billing`（订阅 + **信用点预扣/确认/退还**）、`billing/gateway` 子包（Stripe + 支付宝 + 微信）、`metering`（**计费级 outbox + 分析级缓冲两档可靠性**）；`billing-core`/`billing-ui`、`ui-kit` 的 `FileUploader` | reference-app 跑通「购买点数包（国内+国际两种支付各一次）→上传患者照片→点数预扣与退还→用量与账单展示→邮件/短信/站内信按用户偏好分渠道送达→用户关闭某类通知的短信渠道后不再收到短信→**向未验证的患者手机号发送被拒绝，完成同意验证后才能送达**」 |
| **M3 AI 与集成** | 5-6 周 | `ai-gateway`（LLM + **图像生成/图生图 + 异步作业 + 图像用量计量**）、**`sharing`**（分享令牌/过期/访问统计）、**`integration`**（API Key + 三层限流 + 外发 Webhook + SSRF 防护）、`admin`、`admin-shell`；可观测性补齐 Tempo/Loki/预置 Dashboard/告警 | reference-app 跑通「发起 AI 换牙生成→进度展示→失败退点→生成患者分享链接→外发 Webhook 到模拟 CRM」；运营后台可跨租户检索、模拟登录、查审计 |
| **M4 审计合规与硬化** | 4-5 周 | **`compliance`**（在 M0/M1 已就位的加密与审计采集之上，补齐**治理与高级特性**：不可篡改与可选哈希链、审计检索与报表导出、模拟登录双重身份、数据保留与删除策略、被遗忘权、数据可携带导出、分区归档）、 Storybook 组件文档站、**文档站完整化**（快速开始/接入指南/API 参考/架构纪律/升级指南/两种部署模式手册/ADR 集合/错误码索引/自动生成的配置清单）、e2e 测试、租户隔离与权限的安全审计、版本冻结 | **reference-app 完整业务闭环全链路跑通（见 [14 示例应用](14-reference-app.md) 的关键验证链路，含真实图像 API 调用）**；`saasctl new` + `create-saas-app` 生成的全新项目在单进程与分布式两种部署模式下均能一键跑通；发布 v1.0 |

> 每个里程碑的出口条件都隐含三条，任一不满足该项不算完成：
> 1. 新增基础设施能力必须至少提供 **一套零外部依赖的实现**（使其在单进程组装下可用、并可充当 test double），且**每套实现都声明自身能力、通过该 seam 的契约测试**
> 2. 新增的任何用户可见文案必须同时具备 **中英两份资源**
> 3. 新增的公开 API 必须配套 **使用文档 + 可编译示例 + `AGENTS.md` 条目**，与代码在同一个 PR 内

合计约 **27-32 周（约 6.5-7.5 个月）**。相比最初估算增加约 8-10 周，来自部署模式与实现组装的抽象、动态配置与功能开关、六个社交登录渠道、文档与工程基建（CI 矩阵、纪律自动化检查、发布流水线），以及本轮由真实需求验证出的六项补齐（jobs / storage / notification / sharing / integration / compliance）与三项扩展（AI 多模态、credits、组织树）。

**这 8-10 周的增量是被真实需求与工程化要求逼出来的，不是范围蔓延。** 如果按原方案发布 v1.0，第一个真实项目接入时会立刻发现没有任务队列、没有媒体存储、没有通知系统，届时再补要付出改接口 + 已交付项目升级的双重代价。这三项都是前置投入型的——现在省下的时间，后面会以数倍代价还回来（文案返工、文档腐化、配置散落各处）。前后端在每个里程碑内并行，前端可先用 mock 数据开发 UI，接口契约以 OpenAPI 文档先行约定。

**数据保护必须前移，不能留到 M4**：字段级加密、盲索引、PII 日志脱敏、审计采集这四项已分别前移到 M0 与 M1。理由是它们保护的数据从 M1 就开始产生——手机号在 M1 的短信登录就要落库，如果 M4 才加密，M1-M3 的存量数据全是明文，届时要做一次带停机风险的全表迁移；审计同理，M4 才接的话前三个阶段的操作全部无据可查。M4 留下的是治理层（保留策略、归档、报表、被遗忘权），这些确实可以后置。

**限流同理需要前移**：`ratelimit` 保护的是 M1 `authn` 一落地就会上线的登录/注册/密码重置端点。如果 M0 不提供这个共享原语，M1 的认证接口只能先无防护上线，等暴力破解风险倒逼补救时，代价是已发布认证 API 的行为变更——和数据保护前移是同一类论证。

**关于 storage 的排期（2026-09 更新）**：M2 的 `storage` 单元格已提前于本表窗口作为独立模块轮落地（在 M1 收尾后的实现流中完成）。已交付：模块本体——双方言迁移的 `objects`/`object_derivatives` 两表、Create→Upload→Complete 传输生命周期与存储字节再校验、缩略图异步派生、崩溃收敛的删除与过期回收——HTTP 面七个操作与 OpenAPI 片段（`/api/v1/storage`）、PostgreSQL + MinIO 双 Docker 集成腿，以及 reference-app 端到端接入。与 [07 平台服务](07-platform-services.md)「媒体存储与处理」目标设计的偏差（上传走**服务端中转流式**而非预签名直传、短时效预签名 URL 未落地等）与逐项落地/未落地对照，见该小节的**实现状态**标注；模块能力边界见 `go/storage/AGENTS.md`。

**关于 `FileUploader` 的排期（2026-09 更新）**：M2 行内容单元格列出的 `ui-kit` 的 `FileUploader` 已作为独立组件轮提前于 M2 计划窗口落地（紧随 storage 模块轮）。已交付：完全受控的队列组件——队列就是 host 自己的 `rows` 状态，每行状态与进度按 props 原样渲染，每次 pick/取消/重试/移除经 `onSelectFiles`/`onCancel`/`onRetry`/`onRemove` 回调上报；上传传输（预校验、并发与网络调用）是 host 自己的代码、可中止（host 的 AbortController），**组件零网络**，不持有 File 超过接收它的 event handler。首版曾把「队列内驻 + host 注入 `execute(file, { signal, onProgress })` 逐文件执行」作为「组件全部受控」纪律的具名例外放行，该 carve-out 形态经 2026-09-04 评审裁决取消——本组件与全家一致，纪律不再有具名例外（与 `ConfirmDialog` 的 armed 状态同类的交互局部状态仍仅限交互本身）。消费证明：组件行为套件（rows 由 host 持有、回调被逐次记录，覆盖全部交互状态，含三条回归：live FileList 快照、非有限进度、announce 去重）+ README quick-start 的 host 上传面板（`src/usage-example.test.tsx`）；浏览器页面腿——把壳的 bootstrap 挂进真实浏览器页面、由 reference-app 服务器伺服——随 M4 的 e2e/HTML-runner 工作落地（与下文本文件 consumer-shell 注记的浏览器 leg 声称一致）。storage 的 api-sdk 前端腿仍未恢复：orval 只跑合并文档里的片段（notes + authn），storage 片段要等合并文档下一次扩展——org 片段先排队的 org-web 轮（21 API 契约注记同指），storage 搭同一班再生进入；wire 契约权威现为 `go/storage/api/openapi.yaml` 本身；延期记录互指（`go/storage/AGENTS.md` 的 deferral 表、Taskfile `api:gen` 头部注释、ui-kit AGENTS.md 的 deferral 条目）与本条共同记录。M2 行其余单元格与上一条 storage 注记不变。

**关于 M0 的排期**：从 4-5 周上调到 6-7 周。M0 要完成的是 CI 矩阵、纪律检查规则、发布脚本、模块生成器、契约工具链、pkgcore 全套双实现、dbkit、ratelimit、前端基础包和文档工程——这些没有一项能演示，但每一项做不扎实后面都会反复返工。压缩 M0 是这类项目最常见也最昂贵的错误。

**关于 M1 行前端五个 web 包的状态（2026-09 更新）**：M1 行内容单元格点名的五个 web 包已全部交付——`auth-core`/`auth-ui`/`layout-kit` 在各自轮次落地，`tenancy-ui` 与 `product-shell`（本表所称"shell"的租户面一半，`product-shell` 把 `AppShell` 框架、登录组件家族与会话 hooks 组装成三分支视图机，`tenancy-ui` 提供其 `userMenu` 里的租户切换控件）在 product-shell 轮落地。本注记所在提交同时完成两项普查纠正：changesets 固定版本组从 10 个补到 11 个（此前漏掉了 `@speed/account-ui`），reference-app 的 consumer shell（web 前端目录）已建立——作为 `web/` pnpm workspace 的外部成员（与交付项目同位置），pr-check 的 npm 矩阵随之是十二行：十一个 web 包加 app 一行，跑同一套 lint/typecheck/测试/build leg（app 不参与版本化，永不进入固定组）。web 侧的强制消费证明随之升级为两层标准：包内"形态层面"的 usage-example 旅程（真实 api-client + fetch 替身）之上，consumer shell 的套件把组合后的答案钉住——壳的 vitest 套件经脚本化 demo-server 替身（`src/test-utils/demo-server.ts`，按真实服务器的方式作答、含 rbac 403 deny 开关）驱动真实客户端与组合树，真实组成的服务器（真实注册/登录/授权路由、真实策略）由同轮 Go 侧套件（`cmd/server/demo_users_test.go`、`demo_subject_test.go`）驱动、作为替身所镜像事实的源——两腿合起来钉住端到端答案，四个消费面的 reachable-error 白名单由专项套件双向对齐到 34 个服务器码；详见根 CLAUDE.md 普查与 [12 前端架构](12-frontend.md)、[21 API 契约](21-api-contract.md) 的实现状态注记。仍在表内的：`saasctl`/`create-saas-app` 属 Go 侧脚手架内容（`saasctl new` 生成的骨架目前不含 web 前端），由各自的 Go 轮次推进，不在本注记范围；`admin-shell`（M3 行，面向平台员工）未动；浏览器 + 真服务器 leg——把 app 的 bootstrap 挂进真实浏览器页面、由 reference-app 服务器伺服——随 M4 的 e2e/HTML-runner 工作落地，此前该 leg 在本 shell 内以组件旅程测试形态兑现。

**关于 `pki` 的排期（2026-09-04 新增）**：`go/pki`（签名密钥与 X.509 证书的生命周期管理）是本表原有排期之外的计划外模块，设计见 [22 密钥与证书生命周期](22-pki.md)。它的需求来源不是 reference-app，而是对一套真实生产系统证书子系统的诊断——那套系统与 speed 无代码关系，只作需求镜子，不存在迁移需求。落地涉及对已交付的 M1 `authn` 的回头改造：删除 `WithSigningKeys`、签名密钥改由 `KeySource` 提供、JWT 算法允许列表从单一 EdDSA 放松为 `{EdDSA, ES256}` 并新增"header 的 alg 必须与密钥声明一致"这道检查（放松的原因是 AWS KMS 不支持 Ed25519，放松后的安全性高于现状，论证见该文档）。连带影响 `saasctl` 的四套项目模板及其 golden 文件比对。分四轮交付，轮次划分见该文档末节。**一处明确破例**：该模块分两层，密钥生命周期层由 `authn` 消费、reference-app 间接消费，满足本表的强制消费者条件；X.509 层暂时没有真实消费者（reference-app 不签发证书，需求来源的那套 DBaaS 系统是 Java 的、不会成为 speed 消费者），仍然实现是因为需求已被真实系统验证过，但它按破例处理——必须有 CI 编译运行的 godoc `Example`、在 `AGENTS.md` 如实标注未经真实消费验证、并允许第一个真实消费者接入时做破坏性调整。理由与三条约束见该文档的"X.509 层暂时没有真实消费者"一节。

**建议的中途试点（重要）**：虽然对外一次性发布 v1.0，但强烈建议在 **M2 结束时**（认证、组织、计费、存储、通知齐备）就找一个真实小项目试点接入，而不是等到 v1.0。理由：那时地基已经稳定但尚未大面积铺开，发现 API 问题的修改成本最低；等到 v1.0 之后再发现，改动要同时波及脚手架和已交付项目。reference-app 能验证"能不能用"，只有真实项目能验证"好不好用"。

**排期风险提示**：M2 因为国内外支付双通道都在 MVP 内，比原估多 1-2 周（微信/支付宝的商户资质申请、沙箱联调、回调验签往往比预期慢）。建议提前启动商户资质申请，不要等到 M2 才开始。

