# 风险登记

> 持续跟踪。每个里程碑评审时复核一次，新增风险追加到表尾。

| 风险 | 缓解措施 |
|---|---|
| API 闭门造车 | reference-app 用真实商业需求（AI 微笑模拟平台）作为强制第一消费者，写进每个里程碑出口条件——本轮已证明有效，它直接暴露了 6 个缺口 |
| 模块数量（Go module 与 npm 包）带来的认知与维护负担 | lockstep 版本 + monorepo 让协调成本可控；模块开关让业务方只引入需要的部分；`llms.txt` 与 `AGENTS.md` 降低理解成本 |
| 范围随需求分析持续膨胀 | 已明确"通用能力进脚手架、业务逻辑留业务项目"的判定标准（见 [14 示例应用](14-reference-app.md) 的需求归属表）；后续新增能力必须先回答"是否每个 SaaS 都需要" |
| 点数扣减在并发下超扣 / 失败未退 | 预扣-确认-退还三态设计 + 数据库层原子操作 + 竞态测试；每笔变动留流水可对账 |
| "立即失效"模式给每个请求增加 KV 查询 | 默认走自然过期模式；开启时用本地缓存 + 短 TTL 削减查询；撤销列表只存未过期项，规模极小；压测量化开销后再决定是否默认开启 |
| 审计日志写入量拖垮主库 | 异步落库 + 批量写入；读操作默认不审计（仅敏感读显式标注）；按时间分区 + 过期归档到对象存储 |
| 审计漏记（依赖开发者自觉） | 写操作由 GORM 回调自动捕获，不靠手写；管理员操作全量记录无豁免；CI 用例覆盖模拟登录的双重身份记录 |
| 通知打扰过度导致用户全部关闭 | 类型分组 + 聚合限频 + 合理默认值；安全类与营销类严格分离，避免用户因营销骚扰而连安全通知一起关掉 |
| 6-7 个月才对外，中途需求变化 | 里程碑之间设检查点，允许调整后续范围；模块独立发布使局部返工不影响整体 |
| Go module 与 npm 包的版本协调成本 | monorepo 统一开发 + 全模块统一版本号（lockstep），发布脚本一次性打全部 tag；跨模块改动一个 PR 完成 |
| 统一版本号导致业务项目必须整体升级 | 提供 `saasctl upgrade` 一键改写全部依赖版本并自检；破坏性变更集中在大版本并配升级指南 |
| i18n 遗漏（文案硬编码渗入） | i18n 基建在 M0 就位；CI lint 扫裸文本 + 双语 key 一致性检查；把"双语齐备"写进每个里程碑出口条件 |
| 文档腐化（代码改了文档没改） | 文档与代码同 PR；示例强制 CI 编译运行；配置清单自动生成；API 文档覆盖率检查 |
| 动态配置被滥用（把该硬编码的东西都做成可配） | 配置项需先注册 schema 并说明用途，新增配置走 review；引导配置与动态配置的边界写进架构纪律 |
| 动态配置读取拖慢热路径 | 进程内缓存 + 事件驱动失效，禁止热路径直接查库；压测覆盖权限判断与计量路径 |
| 社交登录账号劫持（凭邮箱自动合并） | 仅在 provider 返回已验证邮箱且在信任列表内才自动关联；否则强制"先登录再绑定"；专项安全用例覆盖 |
| 各社交渠道协议细节差异消耗大量工期 | Google/GitHub 优先（标准 OAuth2）；国内渠道按实际客户需求排序接入，Provider 接口保证后补不影响已有代码；各渠道应用审核周期提前启动 |
| 功能开关组合爆炸导致隐性缺陷 | 依赖图校验让非法组合无法启动；CI 只测三种典型组合；开关只影响路由与任务注册，不影响迁移，降低状态空间 |
| 国内支付资质与联调周期不可控 | M2 前置启动资质申请；网关抽象层先行，Provider 实现可延后接入而不阻塞领域模型 |
| 模板（可 fork 部分）与模块版本脱节 | 模板保持极薄（每文件 <50 行，只做组装）；CI 定时跑生成+构建验证 |
| tenant_id 误入 Prometheus label | CI 断言 + code review checklist；**2026-09 落地**：semgrep 规则 `tools/semgrep_rules/tenant-id-metric-label.yml` 随每个 PR 在 pr-check 的 repo-checks job 运行（命中形状与残余缺口见规则文件头，间接引用等文本匹配不到的形态仍靠 observability 的既有断言测试与 review 兜底） |
| 单进程/分布式两套实现语义漂移（单进程部署模式下测试全绿，切到分布式部署模式才炸） | 同一组用例跑两种部署模式并断言结果一致；接口设计以能力弱的一方为准，不暴露 Redis 特有语义 |
| 有人在业务逻辑里写 `if mode == "standalone"` 分支 | 部署模式分支只允许存在于 Kernel 装配代码。**2026-09 落地**：semgrep 规则 `tools/semgrep_rules/deployment-mode-branch.yml` 按值匹配模式常量比较、`SPEED_DEPLOYMENT_MODE` 读取与 case 分支，kernel 装配文件与 reference-app 入口在 path allowlist；「不得引用 DeploymentMode 常量」的标识符粒度检查（常量经别名跨包流传的形态）是规则文件头里如实记录的残余缺口，由 review 兜底 |
| 单进程部署模式被误用于真实生产/计费 | 启动横幅 + 文档显式声明 + 多副本 fail-fast + 支付默认 MockGateway（真实收款需显式配置） |
| CI 时长失控，全量矩阵随模块增多拖慢每个 PR | 路径过滤只跑受影响模块及下游；PR 阶段只跑单进程部署模式快检、合入前才跑全量；缓存与并发取消 |
| 各模块的 CI 配置重复维护 | 全部走可复用 workflow 与 composite action；新增模块只在矩阵列表加一行 |
| 架构纪律写在文档里但无人遵守 | 每条纪律都有对应的自动检查（semgrep/depguard/自研脚本），见 [18 CI/CD](18-cicd.md) 的纪律检查表（**2026-09 已部分落地**：六条 semgrep 规则、depguard 的 redis/minio/asynq 禁令与许可证扫描器均已接线；落地矩阵与仍属未来轮次的行见 18-cicd.md 纪律检查表下的实施状态注记） |
| 发布时漏打 tag 或版本不一致 | lockstep 发布全流程脚本化；发布后自动触发全新项目生成验证，失败即标记版本不可用 |
| 依赖引入 GPL 系许可证污染商业交付 | CI 许可证扫描，禁止 GPL/AGPL；MPL/LGPL 需 ADR 记录评估结论。**2026-09 落地**：`tools/license_scan.py` 已在 security.yml 的 license job 运行，策略原样执行（selftest 先行、漂移即报错）；Go 侧 34 条、npm 侧 8 条共 42 条依赖的逐条 adjudication 见 `tools/dependency-licenses.json` |
| Flaky 测试侵蚀对 CI 的信任 | nightly 重复运行检测不稳定用例并自动开 issue；连续不稳定先隔离再修，不允许长期红绿摇摆 |
| spec-first 增加接口变更的操作步骤，团队可能绕开 | 自动兜底已基本落地：`task api:gen` 对 reference-app notes 模块的 spec 片段生成后端 interface 与前端 sdk（钉定 orval 生成 `@speed/api-sdk`，经唯一手写接缝调用 api-client 运行时），api-contract.yml 在相关 PR 上重新生成并对前后端生成物各做一次 diff，绕开无法合入（见 [19 开发工作流](19-dev-workflow.md) 的当前状态注记与 [21 API 契约](21-api-contract.md) 末尾的实现状态注记）；oasdiff 破坏性变更闸门与 redocly 合并/lint 仍待交付（首个发布基线与第二个 spec 片段出现时），其间靠 code review 维持 spec-first 顺序；收益（编译期杜绝前后端漂移）远大于成本 |
| 生成代码与手写代码混用导致冲突 | `api-sdk`（纯生成，禁改）与 `api-client`（纯手写运行时）严格分包，生成物整体覆盖不影响手写基建 |
| 外部联系人同意验证被业务方绕过 | 发送前二次校验状态；未验证地址仅允许验证消息；business_attested 路径强制填写凭据引用并留审计 |
| 盲索引密钥轮换需整表重算 | 列入计划内迁移任务走 jobs 批处理；轮换期间新旧索引并存，分批切换 |
| 组织树 + Casbin 前缀匹配的策略规模与性能 | M1 早期打样并压测；策略缓存 + 按租户懒加载；节点数超阈值时告警 |
| `tenant_scope_test.go`（dbkit）长期趋势值得留意：目前 1262 行，已过"远超一千行"的拆分红线但结构清晰、未见真正难以浏览，暂不拆分；如后续继续增长，按目标行为（而非"太长了"本身）重新评估是否拆分，拆分文件必须仍以 `tenant_scope_` 为前缀 | 每次触碰该文件时人工判断一次；不要因为一次性超过某个行数就自动拆 |
| dbkit 的 `Repository[T]` 结构性排除身份/平台数据域（`TenantScoped` 约束要求 T 实现它，而这两类数据按设计必须不实现）——已在 CLAUDE.md 与 dbkit AGENTS.md 中记录为"设计如此"，但目前没有任何身份/平台数据的落地范式（如统一的非租户 repository 帮助类）经过真实验证 | 交给 `authn`/`org` 落地时用真实模型倒推设计，而不是现在凭空造一个未经消费验证的抽象 |
| `tenancy.WithSystemContext` 目前与 `dbkit.Repository[T]` 完全不组合——已在 system_context.go 的文档注释及本模块自己的测试中双向验证：对已经限定在租户 A 的 context 而言，被授予系统上下文后仍只能看到租户 A 的数据（不会放宽可见范围）；对完全没有租户的 context 而言，被授予系统上下文后依然 fail-closed，报错 `pkgcore.ErrNoTenant`（不会顶替租户），不是安全漏洞，隔离没有被打破，如果说有偏差也是偏保守，不会发生跨租户数据泄漏，但确实是当前真实存在的功能缺口：管理员跨租户搜索、跨租户后台任务都无法建立在 `Repository[T]` 搭配 `WithSystemContext` 之上，眼下只能改走裸 SQL 逃生舱（backend-coding-standards 3.2 节）；同一根因还牵出一个更具体的缺口：`docs/internal/04-data-and-tenancy.md` 明确要求系统上下文的审计记录包含 Actor、Purpose 与**影响的记录数**，但 system_context.go 的 `SystemContextEnteredEvent` 没有任何字段承载这一项——而且 `WithSystemContext` 是在授予系统上下文的那一刻——即函数返回前那行 `bus.Publish`——就同步发出审计事件的，与调用方后续会不会、以及如何用这个 context 执行查询完全脱钩，因此在事件产生的那个时间点上没有一个天然的挂载点能把这个数字算出来、填进去（本模块自己的测试里 `repo.List(elevatedA)`、`repo.FindByID(elevatedA, recB.ID)` 这类查询确实会在系统上下文下执行，只是执行发生在审计事件已经发布之后，两者之间未建立对应关系） | 两个方向都还没有取舍：要么将来在 `Repository[T]` 内部实现这条逃生舱（呼应 dbkit `tenant_scope.go` 文档注释里预留的设计位），要么正式追认"`WithSystemContext` 目前只负责审计留痕，跨租户读写一律走裸 SQL 逃生舱"——留到真正出现跨租户消费场景时再决定；无论最终选哪个方向，落地时都必须同时补上 `SystemContextEnteredEvent` 缺失的影响记录数字段及其填充路径，使审计记录满足 `docs/internal/04-data-and-tenancy.md` 第 3 条的完整性要求，而不能只解决组合问题、放着审计缺口不管 |
| `observability.Middleware` 的 `http.route` 指标标签走 `routeLabelLimiter`，其 `MaxRouteLabelValues=256` 上限（middleware.go "Route label caveat" doc comment 与 `go/observability/AGENTS.md` 均已记录）本质上是防未认证调用方指标基数攻击的断路器，不是路由精度机制——它只按目前为止见过多少个不同的 `URL.Path` 计数，不理解也不还原路由模板。今天仓库里所有已注册路由都是字面量（见 `MaxRouteLabelValues` 文档注释引用的 `examples/reference-app/cmd/server/server.go` 的 `mountModuleRoutes`），这条缺口目前是潜在的，尚未被任何真实路由触发；但将来一旦有模块在 `tenancy.Middleware` 下游挂载一条真正参数化的路由（例如 `/api/v1/billing/subscriptions/{id}`），限流器会把每个不同的 `{id}` 当成新的不同值继续记录，直到累计撞上 256 的上限才开始坍缩到 `RouteLabelOverflowValue`——而不是像真正的路由捕获机制那样从一开始就统一坍缩成路由模板本身。也就是说，触顶之前会静默丢失按路由维度的指标粒度，触顶之后所有超限的参数化路由又会混进同一个 `{overflow}` 桶，在 Prometheus/Grafana 上无法分辨具体是哪条路由造成的。这不是安全漏洞：失败方向是"有界但不精确"，不会退化成无界增长，不会打垮 Prometheus，也不涉及租户数据泄漏或越权，纯粹是可观测性精度问题。 | 暂不构建路由捕获机制：真正修好需要一个呼应 `AnnotateTenant` 的机制——`AnnotateTenant` 是事后在 span 上补记 tenant_id，这里则需要在 `routeLabels.label(...)` 打标签之前，把原始 `URL.Path` 换成请求实际匹配到的路由模板，这依赖 `pkgcore.MountedRoute`（或对 mux 的扩展）在匹配发生后把该模板暴露给中间件，本轮基础设施范围内明确不做；留到第一个模块真的要在这条中间件下游挂载参数化路由时再决定是否值得建，在此之前 `MaxRouteLabelValues` 这个断路器本身不需要跟着调整。 |
| `go/jobs` 的 `jobRecord`（`store.go`）被 `go/jobs/AGENTS.md` 的"The persistence model is platform data, not tenant data"一节归为"平台数据"（援引 `docs/internal/04-data-and-tenancy.md` 的数据分域表）——但这个标签本身对不上号：该表给"平台数据"的定义是全局共享、租户只读，而 `jobRecord` 每一行都归属且仅归属一个具体租户（`Task.TenantID` 经 `Task.validate` 强制非空，对应的 `tenant_id` 列本身也是 `not null`），租户还在持续写入它——每次 `Enqueue` 都是一次租户发起的写操作——既不共享也不只读，恰是定义的反面。它也套不进另外三类：不是租户数据，因为调度器的 `claimCandidates`/`deadLetterRecords`（`store.go`）必须一次性跨所有租户扫描候选 Job 才能按优先级派发、做跨租户并发限流，这个访问模式是 `dbkit.Repository[T]` 的 `TenantScoped` 泛型约束结构性地服务不了的；也不是身份数据（不归属自然人）或关联数据（不是身份与租户的桥表）。当前机制没有问题、也是今天唯一可行的方案——绕开 `Repository[T]`，直接用 `dbkit.Open()` 返回的裸 `*gorm.DB`，`pkgcore.WithSystemContext` 并不会让 `Repository[T]` 对 `TenantScoped` 模型放行跨租户读（`dbkit/AGENTS.md` Known limitations 已明确记录）——隔离靠 `store_test.go` 的 `TestJobRecord_NotTenantScoped`（`tenancytest.AssertNotTenantScoped`）加上 `Queue.Get`/`Cancel` 共用的手写校验 `callerMayAccess` 证明，是真实且经测试验证的，这不是安全缺陷，缺的是分类学精度：数据分域表目前的四类里没有一类准确覆盖"租户自有、租户持续写入，但访问模式要求跨租户扫描"这种形状，将来任何同形状的模块（例如 webhook 投递队列、定时任务表）大概率会照抄"就叫平台数据"了事，而平台数据的定义其实并不成立。 | 是在 `docs/internal/04-data-and-tenancy.md` 的分域表里加第五类（例如"内部跨租户扫描、租户自有数据"），还是正式追认"平台数据"对这种形状是一个刻意放宽的标签、并在该文档写明理由，两个方向都还没有取舍——留到下一个出现同形状需求的模块落地时，再一并决定分域表要不要扩展。 |
| `AsynqQueue`（分布式部署模式，`go/jobs/asynq_queue.go`/`asynq_worker.go`）目前只接入了 `jobs.queue.depth` 一个指标（`registerQueueDepthGauge`）；`docs/internal/09-observability.md` must-instrument 表为任务队列域要求的另外三项——`jobs.job.duration`、`jobs.job.attempts`、`jobs.job.dead_letter`——只有 `StandaloneQueue` 接了（`standalone_queue.go` 的 `registerJobMetrics` + `worker.go` 的 `recordJobMetrics`/`recordDeadLetter`），`asynq_worker.go` 的 `processTaskUncancelled`/`handleErrorAttempt` 在成功、重试、进入死信这三个同样的时间点，目前只写了结构化日志（`attempts`/`duration_ms`/`error`），没有对应的 Counter/Histogram。`go/jobs/AGENTS.md` 的 Known limitations 已如实披露这一缺口，定性为"a real, tracked gap, not a deliberate design choice"，本条只是把它正式登记进跨模块风险表。不是安全或正确性问题——隔离与任务执行语义都不受影响——纯粹是分布式部署模式下可观测性不完整：执行时长分位数、失败率、重试次数、死信计数这四类信号在分布式部署模式下暂时只能从结构化日志里查，没有聚合指标可看，唯一接了指标的只有队列积压深度一项。 | 修复方向已经明确，不是待取舍的开放问题：把 `StandaloneQueue` 现成的 Counter/Histogram 模式（`registerJobMetrics`）接进 `asynq_worker.go` 的 `processTaskUncancelled`/`handleErrorAttempt`，记录点与它们已经在打的 `attempts`/`duration_ms`/`error` 结构化日志同位即可，作为已知的 fast-follow 处理。 |
