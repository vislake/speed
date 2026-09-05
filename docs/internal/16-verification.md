# 验证方式

> 每一项都对应 CI 中的强制检查或明确的人工验收步骤。没有验证手段的设计约束等于没有约束。

**1. 租户隔离（最高优先级）**
- 每个模块的 Repository 必须跑 `tenancytest.AssertIsolated`，CI 强制
- CI 静态检查：业务模块内不得出现绕过 Repository 的 `db.Table/db.Model/db.Raw`
- 分布式部署模式下的 PG 环境额外验证 RLS 生效：手工构造一条绕过 Go 层的查询，断言返回为空

**2. 实现组装矩阵（与双方言矩阵正交，都要跑）**
- **每个 seam 的契约测试 × 该 seam 的每套实现**：同一套契约用例对 `EventBus` 的进程内实现与 Redis Streams 实现各跑一遍，`KVStore`、`Queue`、`ObjectStore`、`Mailer` 同理。这是防止多套实现语义漂移的唯一手段，也是这套抽象最容易出问题的地方——漂移面随实现数呈 N² 增长，只跑"两遍部署模式"远远不够
- 关键语义一致性断言：配额计数、事件投递顺序、计量汇总结果，在同一 seam 的各套实现下必须一致
- 若干条有代表性的整机组装冒烟（全进程内实现 / 全外部依赖实现），reference-app 的 e2e 各跑一次
- 验证装配校验：把不满足 `MultiReplicaSafe` 的实现装进多副本部署模式，启动必须报错退出，且错误信息点名是哪个 seam 的哪套实现不满足哪条能力

**3. 双方言矩阵**
- `dbtest.NewPostgres(t)`（testcontainers-go）+ `dbtest.NewSQLite(t)`，每个模块测试跑双方言，尽早发现方言漂移
- 迁移文件双方言各自生成后，CI 验证从零迁移到最新版本均可成功

**4. reference-app 端到端**
- Playwright 覆盖：注册/登录/SSO、组织与成员、套餐订阅与两种支付、用量与超额、AI 调用、运营后台
- 每个里程碑出口条件对应一组 e2e 用例，只增不减

> **实施状态注记（本轮核实）：** 上面两条是设计意图，尚未落地——仓库里没有任何 Playwright 配置或 spec 文件，`.github/workflows/e2e.yml` 是 gated stub（guard step 直接失败，不在任何 PR 上触发），推迟到 roadmap M4 的 e2e 条目。reference-app 目前的验证方式是 Go 侧的组合 HTTP 测试（`*_flow_test.go` 系列：notes、org、authn、storage、notification 等，跑在 `full-ci` 标签的 `pr-full.yml` 上）加前端各包自己的 `usage-example.test.tsx`（真实机制见第 6/8 节的注记），浏览器页面本身（挂载、渲染、真实点击）还没有任何自动化覆盖。

**5. 脚手架生成验证（防止模板腐化）**
- CI 定时任务：`saasctl new tmpapp` + `create-saas-app tmpapp-web` → `go build` / `pnpm build` → 先 `docker compose -f docker-compose.standalone.yml up`（应在数十秒内就绪）再 `docker compose up` → 两次都跑冒烟脚本打健康检查与登录接口
- 这条流水线一旦红，说明模板与模块版本已脱节，必须立即修复

> **实现状态注记（2026-09-03，saasctl 轮）：** 本条流水线的"生成 → 构建 → 冒烟"工具侧已在 saasctl 轮真实落地并跑通——`saasctl new`（含 `--with` 选择）materialize 出项目后，经真实 `go mod tidy`（网络）与 `go build` 编译，boot 起来冒烟真实组成的 HTTP 链（healthz、config/public、register、错密码 401、坏 token 401、匿名 403 等逐项断言），外加 `db migrate` 与 `upgrade` 对真实文件的操作；程序与逐项答案记录在 `go/saasctl/AGENTS.md` Testing 章节，是模板与模块版本"此刻未脱节"的证据。但本节的 CI 化形态仍未落地：`scaffold-verify.yml` 仍是 gated stub（其文件头已由本轮更新，记录"生成侧已存在且有离线证明；双部署模式 boot 门仍未接线"），CI 定时触发与上面"CI 定时任务"一行描述的两次 `docker compose`（standalone 与 distributed 各 boot 一次）属 M4 该流水线转正时的范围——届时每轮 PR 不跑它（它按设计是每日 + 发布后），模板腐化的日常防线靠 saasctl 轮之后的仓库内测试（golden 钉死 + materialize 证明）。

**6. 国际化**
- CI lint：UI 包 JSX 中不得出现裸文本节点；Go 代码中面向用户的错误不得使用字面量文案
- 资源完整性检查：`zh-CN` 与 `en-US` 的 key 集合必须完全一致，缺任何一边 CI 报错
- 核心组件提供中英双语的可运行示例，人工过一遍长文案截断问题
- reference-app 的 e2e 至少有一条完整链路跑英文界面，并断言邮件按收件人 locale（而非操作者语言）渲染

> **实施状态注记（本轮核实）：** 第三条原写"Storybook 为核心组件提供…story"，已按现状改写——仓库里没有 Storybook（无 `.stories.*` 文件、无相关依赖或配置），真实提供双语可运行示例的机制是各包自己的 `src/usage-example.test.tsx`（`account-ui`、`auth-ui`、`tenancy-ui`、`api-client` 已落地，`pnpm -r test` 覆盖），不是独立的可视化组件浏览器；这个缺口在第 8、20 节反复出现，均按同一现状改写。第四条的 reference-app e2e 同样尚未落地，见第 4 节的注记。

**7. 配置管理**
- 优先级链测试：同一个键分别从 flag/env/文件/默认值提供，断言覆盖顺序正确
- 缺失必填配置时启动必须 fail-fast 并给出可读错误（含缺失键名与来源提示）
- 动态配置：租户级覆盖回退到全局的解析正确；修改后各实例在事件广播下热生效；敏感项在日志与 API 响应中确实被脱敏；每次变更都产生审计记录
- `saasctl config print` 输出的来源标注与实际一致——**已解除（2026-09-03，saasctl 轮）**：print 与生成应用的引导同源（`internal/appconfig` 是模板内 `cmd/server/config.go` 的孪生，孪生关系由测试钉死），实测两个变体——设了 `SPEED_*` 的环境时每行标 `from <ENV>`、空环境下每行标默认来源——密钥行一律 `[redacted]`，逐行输出与真实解析一致（程序与答案见 `go/saasctl/AGENTS.md` Testing 章节）。注意该行只覆盖引导配置面；动态配置（`configs` 表）的打印仍待其维护面落地。

**8. 文档**
- CI 强制编译并运行所有文档中的代码示例（Go `Example` 测试 + 前端各包自己的 `usage-example.test.tsx`），示例失败等同于构建失败
- 配置清单文档由 schema 自动生成，CI 检查生成结果与仓库内文件一致（防止手改漂移）
- 每个公开导出的 API 必须在文档中有对应条目，CI 做覆盖率检查
- 每个模块的 `AGENTS.md` 存在性检查；仓库根 `CLAUDE.md` 的纪律条目与 CI 检查表一一对应
- 人工验收：用一个未接触过本项目的 AI Agent，仅凭文档完成"接入认证模块并加一个受权限保护的接口"，记录卡点并回补文档

> **实施状态注记（本轮核实）：** 第一条已按现状改写——"Storybook 构建"从未存在，真实机制同第 6 节注记。第二、三、四条这三项 CI 检查目前都没有接线：`go/config/schema.go` 的 schema 类型未导出，没有工具能读出配置项清单，自然也没有生成结果与仓库文件的一致性检查；`docs-check.yml` 自己的 header 明确把"config-reference generation drift"列进"DELIBERATELY NOT WIRED"；API 文档覆盖率检查和 `AGENTS.md` 存在性检查同样不在任何 workflow 里，通读 `.github/workflows/*.yml` 无一处提及。这三项目前都只能算设计意图，回补需要先有导出的 schema 访问器、错误码/API 清单生成脚本和一个存在性检查脚本，是真实的实现缺口而非本轮的文档措辞问题。

**9. 第三方登录与功能开关**
- 账号关联安全用例（最高优先级）：构造"第三方返回未验证邮箱且与已有用户邮箱相同"的场景，断言**不会**自动合并账号；`state` 缺失或不匹配时回调必须被拒绝；回调地址不在白名单时拒绝
- 解绑保护：仅剩一种登录方式时解绑必须失败
- 各渠道用官方沙箱或 mock server 覆盖授权码换取流程；`MockSocialProvider` 作为该 seam 的一套实现，让本地无需真实应用凭证即可跑通
- 开关组合：CI 跑最小集 / 全开 / 典型交付组合三种构建；非法开关组合（如开启超额计费但禁用 metering）必须启动失败
- 被禁用功能的接口返回 404，且 `/api/system/features` 状态与实际一致

> **实施状态注记（本轮核实）：** 三种构建组合的矩阵尚未接线——通读 `.github/workflows/*.yml` 没有任何 job 构造"最小集/全开/典型交付组合"这三种开关组合分别构建，仍是设计意图。

**10. 身份与组织**
- **密码**：argon2id 参数生效（哈希前缀可验证）；弱口令字典拦截；密码策略走动态配置且改动即时生效
- **MFA**：TOTP 时间窗口容差正确；恢复码一次性生效且用后作废；高风险操作触发 step-up 重新验证；强制策略下未启用 MFA 的用户被引导至启用流程而非直接放行
- **组织树**：子树数据范围正确（上级能看下级、下级看不到上级与兄弟节点）；成员在树中移动后权限即时随之变化；前缀匹配判定在千级节点下的延迟纳入性能基准（判定引擎已改为自建，见 [05 身份与访问](05-identity-and-access.md) 的「实现落地更正」；该压测归属 `org` 轮次，因为造树的是 `org`）
- **成员移除**：从租户移除成员后，其针对该租户的 access token 立即失效（不等自然过期）

**11. 异步任务、媒体与集成**
- 任务队列：重试与指数退避生效；幂等键下重复提交只执行一次；worker 内租户上下文正确重建（构造一个不带 tenantctx 的任务，断言 Repository fail-closed 而非跨租户读取）；SQLite 任务表实现下重启后未完成任务能恢复
- **计费级计量的 outbox**：在业务事务提交后立即杀掉进程，重启后待投递记录仍在且被正确投递（证明"业务成功但计量丢失"物理上不可能）；投递失败持续重试并告警
- **点数预扣-退还**：模拟 AI 任务失败，断言点数被完整退还且流水可对账；并发扣减不出现超扣（用竞态测试覆盖）
- 存储：上传伪装扩展名的文件必须被服务端 MIME 校验拦截；EXIF（含 GPS）确实被剥离；私有对象无预签名时不可访问；删除源文件时派生资源同步清理
- 分享链接：过期后失效；撤销后立即失效；令牌不可枚举（长度与随机性检查）
- 外发 Webhook：签名可被接收方验证；**指向内网地址的订阅必须被 SSRF 防护拒绝**；失败进入死信且可手动重投
- 限流：超限返回 429 且响应头正确；租户级与 Key 级限流互不串扰
- 合规：加密字段在数据库中确为密文；密钥轮换后旧数据仍可解密；保留期到期自动清理生效；日志中不出现明文 PII

**12. 通知、审计与会话**
- 偏好矩阵：关闭某类型的某渠道后确实不再从该渠道收到；安全类通知无法被完全关闭；营销类可一键退订且邮件含退订链接；租户强制策略能覆盖用户个人设置
- 站内信：未读计数准确；SSE 在分布式多实例下能正确扇出（起两个实例，验证连到 A 实例的客户端能收到 B 实例产生的通知）
- 审计自动采集：更新一条记录后，审计里能查到正确的 before/after diff 且敏感字段已脱敏
- **模拟登录审计**：管理员模拟用户执行操作后，审计记录中 `Actor` 是被模拟用户、`OnBehalfOf` 是管理员，两者都不缺失；被模拟用户收到通知
- 不可篡改：应用角色对审计表执行 UPDATE/DELETE 必须失败；开启哈希链时篡改任一行能被校验工具检出
- **审计归档**：到期分区被导出到对象存储后才 DROP；归档动作本身留下审计记录；归档后按时间范围查询能提示"该时段已归档"而非静默返回空
- **会话撤销**：下线某设备后，该设备的 refresh 立即失败；开启"立即失效"模式时其 access token 也当场失效；关闭该模式时在 15 分钟内自然失效
- **refresh 重放检测**：重复使用已作废的 refresh token，整个会话族被撤销且用户收到安全通知
- 异常登录：模拟异地登录触发安全通知；连续失败触发锁定

**13. 语言与日志规范**
- CI 扫描：`docs/internal/` 之外的 `.md` 与代码注释中不得出现 CJK 字符（i18n 资源、`docs/site/` 本地化目录除外）
- 日志消息为常量字符串，无 `fmt.Sprintf` 拼接；属性 key 为 `snake_case`
- 从 context 取 logger：构造一条请求，断言日志中确实带有 `trace_id` 与 `tenant_id`
- 脱敏生效：写入含手机号、令牌的日志，断言输出中已打码

**14. API 契约**
- CI 重新生成前后端代码并与仓库产物 diff，不一致即失败
- 删除 spec 中一个字段后，后端必须编译失败（证明 server interface 确实参与编译约束）
- `oasdiff` 能正确识别破坏性变更并拦截未标记的 PR
- ESLint 规则确实拦截手写的 `fetch`/`axios` 后端调用
- 合并后的 spec 通过 redocly lint，无 operationId / schema 命名冲突

> `oasdiff` 破坏性变更闸门本身仍待交付，需要首个发布基线，计划落在 M4——与 [21 API 契约](21-api-contract.md) 末尾的实现状态注记及 `api-contract.yml` 自己 DELIBERATELY NOT WIRED 一节记录的机制决策一致，不是遗漏。

**15. 数据分域与系统上下文**
- 身份数据与平台数据跑 `AssertNotTenantScoped`：断言它们**不会**被误加租户过滤
- 系统上下文：非白名单模块 import `WithSystemContext` 由 code review / CODEOWNERS 把关（`go/pkgcore`、`go/tenancy`）；每次进入系统上下文都产生审计记录
- 分布式 PG 下用受 RLS 约束的角色执行跨租户查询必须返回空，切到 `BYPASSRLS` 角色才可见
- 从租户移除成员后，该用户针对该租户的 access token 立即失效

> **实施精确化（本轮核实，与 [04 数据层与多租户](04-data-and-tenancy.md) 的"实现落地更正"一致）：** 第二条原写"CI 拒绝"，已改为真实机制——depguard 只能按整个 import path 粒度放行/拒绝一个文件，做不到只挡 `WithSystemContext` 这一个符号（`pkgcore` 根包同时还装着 `TenantID`/`WithTenant`/`apperr`，把"仅白名单可 import `pkgcore`"接成 depguard 规则会连带拦下 `go/dbkit` 23 处无关合法导入），这条草稿规则因此未合入，完整推演见 `.golangci.yml` 自己的 depguard 注释。白名单目前靠 code review 与 `go/pkgcore`、`go/tenancy` 的 CODEOWNERS 加两个函数的文档注释把关，不是静态检查。

**16. 外部联系人与同意**
- 未验证地址除验证消息外一律拒绝发送
- 双向确认流程：验证消息有频率上限，超限被拒
- 退订后任何消息（含事务性）不再发送；重新同意后恢复
- 同意的租户隔离：A 租户的同意不能被 B 租户使用
- 盲索引：同一手机号在不同格式输入下（+86 前缀有无、空格）能查到同一条记录

**17. 可观测性自验**
- `docker compose -f docker-compose.yml -f docker-compose.observability.yml up` 后，Grafana 预置面板应立即有数据
- 验证 Loki 日志可通过 trace_id 跳转 Tempo
- 验证 Prometheus 中**不存在**任何带 `tenant_id` label 的指标（加一条 CI 断言，防止有人无意间加回去）

