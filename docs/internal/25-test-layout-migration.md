# 25 测试布局迁移:侦察与执行蓝图

## 背景:测试布局规则

测试文件布局遵循一条仓库规则(写入根 `CLAUDE.md` Testing 小节、后端技能 §13、前端技能 §12,措辞按各自语境):

- **单元测试由"层级"定义,不是由 1:1 文件映射定义**:单元测试 = 普通单元运行执行的那些同包、无外部依赖测试。只有单元测试与被测代码同目录——一个目标一个文件(`registry_test.go` 伴 `registry.go`;`PlanCard.test.tsx` 伴 `PlanCard.tsx`),跨越多个源文件的包内行为套件也留在包内,以行为命名。
- **一切非单元测试类进入专有目录,绝不混入源码目录**:真实后端集成 → 每个 Go 包的 `integration_test/`;浏览器端到端 → `e2e/`(Playwright);应用的组合 HTTP/装配流 → 专用的应用级测试目录;跨实现契约套件 → 该 seam 自己的支撑包(`queuetest`/`eventbustest` 模型);迁移套件 → 紧邻所练的迁移集。
- **Go 自身包规则逼出的两类包内例外,显式命名**:godoc `Example*` 函数(`example_test.go`)与必须触达未导出符号的白盒基准(`<target>_bench_test.go`)。
- 无现成专有目录可归的非单元测试,新建一个按用途命名的目录——绝不加入源码目录。

本批次(批次 1)落三处标准家、修两处命名遗留,并产出本文侦察;批次 2 依本文执行迁移(本文只侦察,不移动)。

## 批次 1 已执行的命名遗留

1. `cmd/server/p3_fragment_surface_flow_test.go` 更名为 `fragment_surface_flow_test.go`,函数 `TestP3FragmentSurface_CaseToSimulation_Journey` 更名 `TestFragmentSurface_CaseToSimulation_Journey`;文件头、夹具字符串中的批次记号一并清除(规则禁止批次/轮次代号进入名称与注释)。
2. 三个无 JSX 却以 `.tsx` 命名、被测模块为 `.ts` 的测试套件改回 `.test.ts`:`@speed/auth-ui` 与 `@speed/tenancy-ui` 的 `internal/error-text.test.tsx`(纯逻辑钉住白名单↔语言包平价,渲染覆盖在其姊妹套件),以及 reference-app web 的 `useHashRoute.test.tsx`(纯 `renderHook`,无 JSX)。姊妹包 account-ui/billing-ui 同形套件本就用 `.test.ts`。

## 侦察一:cmd/server 组合 HTTP/装配流的导出面

### 现状

`examples/reference-app/cmd/server` 共 55 个 `_test.go` 文件(编译通过、`-race` 下全绿,由 full-check 的 reference-app job 执行)。装配入口是 `server.go` 的 `buildServer(ctx, serverConfig)`(返回 `http.Handler`、cleanup、`*compliance.Module`),`main.go` 薄壳调用它;测试基座在 `server_test.go`(`testConfig`/`buildTestServer`/`registerAndAuthenticate`/`testPassword`),各域 `build*TestServer`(buildOrgTestServer、buildSmileSimTestServer、buildAdminTestServer、buildNotifTestServer、buildConsultTestServer、buildPeriodicTestServer、buildSeededTestServer、buildWebhookFlowTestServer、buildAttestationTestServer、buildUsageSummaryTestServer 等)定义在各自流测试文件内,再被其他流文件跨文件引用——辅助函数呈网状耦合(图片夹具 `jpegWithExif`、上传 `uploadAndComplete`、假 AI 服务器、邮件/短信捕获 `capturingMailer`/`lockedBuffer` 等被 5–15 个文件共享)。任何把流套件外移成黑盒包的动作,都必须先把这整张辅助网一并迁走或导出。

### 未导出符号清单(黑盒外部测试包所需)

对 55 个测试文件做注释/字符串剥离后的引用矩阵,流测试实际触达的**非测试文件**未导出符号分族如下(括注引用文件数):

> **本表是历史记录(批次 2b 执行前的时点侦察快照),不改**:表中符号名与引用文件数均为当时 `cmd/server` 包内形态的实测;装配迁入 `internal/app`、demo 层再移入 `internal/app/demo` 之后的现状见"批次 2b 处置记录"。

| 族 | 符号 | 说明 |
|---|---|---|
| 装配与配置 | `buildServer`(25 文件直引,其余经基座)、`serverConfig` 及其字段(22)、`configFromEnv`、`mountModuleRoutes`、`openConfiguredAuthnChannels`、`demoHostTenants`、`orgFeatureGate`、`socialChannelFlagKey`、`signInMemberships`/`newSignInMemberships`、`healthzHandler`/`metricsHandler`、`defaultPort`/`defaultSQLitePath` | 流的公共根:39 个起动服务器的测试文件全部经 `buildTestServer` 族 → `buildServer`;`serverConfig` 是变异回调(`func(*serverConfig)`)与读回(cfg.SQLitePath、cfg.Memberships)的载体 |
| 测试密钥默认 | `devConfigKey`、`devOrgIndexKey`、`devNotificationIndexKey`、`devPKILocalKeyCipherKey`、`devBlindIndexKey`、`devPIICipherKey`(server_test.go 引用全组) | `testConfig` 直填 cfg 字段,绕过 `configFromEnv` |
| demo 身份常量 | `demoOwnerUserID`(23)、`demoReaderUserID`、`demoNotesCreatorUserID`、`demoSingleTenantID`、`demoOwnerEmail`、`demoReaderEmail`、`demoAcmeOnlyEmail`、`demoPlatformStaffEmail`、`demoSmileSimRecipientUserID`、`demoSeedAccounts`、`demoOrgUserHeader`、密码常量、`demoUserAddresses` | 生产胶水(internal/app/demo/demo_subject.go/demo_users.go/demo_notification.go)自身也读,故无法整体移出包;黑盒流只能读导出版 |
| demo 解析器/守卫族 | `demoSubjectResolver`、`demoOrgSubjectResolver`、`demoNotesSubjectResolver`、`demoResolveSubject`、`demoSubjectResolverFor`、`demoPermissionFor`、`DemoRouteRules`、`orgNodeScopeLayer`、`orgRouteGuardDeps`、`notesResource`、`mustResourceOf`、各域 `*PermissionFor` | 白盒直测的对象(demo_subject_test 等);流文件多经 HTTP 间接使用,直接引用集中在 kill-switch/守卫文件 |
| seed 族 | `SeedDemoUsers`、`SeedDemoGrants`、`SeedDemoCredits`、`SeedDemoEntitlements`、`SeedDemoPlatformStaff`、`addDemoOrgMembership`、`registerDemoUser`、`GrantDemoCredits`、`EnsureDemoSubscription` | 部分被生产装配调用,部分仅供测试种子 |
| HTTP 路径/响应常量 | `authnAPIPath`、`AdminRoutePath`、`adminAuditEventsPath`、`adminUsageSummaryPath`、`aiGatewayRoutePath`、`orgRoutePath`、`pkiRoutePath`、`billingRoutePath`、`clinicNamePath`、`consultSuggestPath`、`teamMembersPath`、`webhookBasePath`、`integrationWhoami*`、`casesMaxRequestBodyBytes`、`maxPhotoBytes`、`smileSimulationContentPath`、`casePhotoContentPath` 等约 20 个 | 与片段线路径相关的部分已被 `fragment_wire_paths_test.go` 收拢为夹具常量;其余散在各生产文件 |
| 写错误/响应辅助 | `writeClinicNameError`、`writeConsultError`、`writeSmileSimError`、`writeTeamMemberError`、`writeTeamMembersJSON`、`clinicNameResponse` 等 | clinic_name/consult/team_members 服务级测试直测 |
| 自服务域 | `selfServiceProvisioner`、`selfServiceProvisionTask(Type)`、`selfServiceProvisionJobHandler`、`clinicTenantOf`、`wireSelfService`、`userIDFromUserCreatedPayload`、`newProvisionFailureInjector` | self_service_test 白盒直测 |
| main 进程胶水 | `run`、`runHealthcheck`、`observabilityOptions`、`healthcheckArg` | main_test.go 白盒直测 |
| 装配返回句柄 | `*compliance.Module`(buildServer 第三返回值,compliance_flow/compliance_race/tenant_config_reader 用)、`meteringModule`、`rbacService`、`standaloneQueue`(租户配置阅读器流)、`orgSubtreeResolver` | 外部包只能经导出的装配入口获得 |
| 测试文件内辅助(网) | `buildTestServer`/`testConfig`/`registerAndAuthenticate`(server_test.go,几乎全文件引用);`capturingMailer`、`tokenFromMail`、`openSecondDB`、`softDeleteAndBackdate`、`lockedBuffer`、假 AI 服务器族、图片/上传/微笑轮询辅助、org/分享/webhook 请求辅助等(跨 2–17 文件) | `_test.go` 不可跨包导入;外移后这些辅助随文件进入同一新包则无需导出,留在包内的则必须迁走或重造 |

### 迁移机制选项与成本估计

Go 不允许外部目录的测试访问 package main 的未导出符号,也不允许导入别的包的 `_test.go` 文件;组合流外移只有三条路:

- **方案 A:导出最小装配/测试面**。在 `cmd/server` 的正式 `.go` 文件(如 `server_testing.go`)导出测试构造器(如 `BuildTestServer` 返回类型化的测试句柄:httptest 服务器、测试配置投影、`*compliance.Module` 等),并把 demo 身份常量、路径常量、key 默认值导出。成本估计:约 40–50 个导出符号(装配入口 1 + 类型体系 serverConfig/测试配置投影 + 常量约 35 + 句柄类型),另加 `signInMemberships` 等价注入面的导出。测试辅助网整体随流迁入新目录(同包互用,不需导出)。代价:package main 的正式文件携带一批仅供测试的导出符号(未引用的导出函数会被链接器丢弃,但构成生产面的"测试味"),与模块库的导出纪律相抵;应用型代码可接受但需显式记录。
- **方案 B:组装代码库化**。把 `buildServer` 及其胶水移入 `internal/` 包(如同模块的 `internal/app`),`main.go` 变薄;流测试目录成为同模块内可导入 `internal/` 的包。成本估计:数千行移动 + `internal/` 边界重排,是本仓库"模块即库"形态下最干净但最重的方案;demo 身份常量/解析器族若随迁进 `internal/app`,测试包可经 `internal/app` 的导出面获得(内部包导出面不必守库 API 冻结纪律)。
- **方案 C:逐文件记录白盒例外**。规则允许"无现成目录可归时新建专有目录",却未给白盒留口;方案 C 是把确需白盒的文件按 Go 包规则列为记录在案的包内例外(如 `export_test.go` 模式只能同包用,跨包无解),适用于数量小、确证不可黑盒的文件。

批次 2 推荐先按方案 A 的导出清单边界做一次"干跑"分类校验(下表),再定 A/C 混合比例;方案 B 作为长期形态记录在案,不列为本迁移的默认路径。

### 逐文件分档

档位定义(对"组合 HTTP/装配流"迁移对象):

- **留包**:按层级本就是单元测试(同包、无外部依赖、普通运行执行)或直测未导出生产函数的白盒单元——规则下它们留在包内,不属于迁移对象。
- **(i) 黑盒可移(小导出面后)**:驱动纯 HTTP;除共享辅助网外只需导出身份/路径常量与装配入口。
- **(ii) 需要测试入口点**:还需配置变异键、装配返回句柄(compliance/metering/rbac/queue/org 服务)、数据库第二连接、邮件/短信捕获注入等——需要方案 A 的"测试装配入口"把这些以类型化形式给出。
- **(iii) 不可分白盒(留包记录)**:直测未导出函数或混合文件,外移需拆分。

| 文件 | 档 | 依据 |
|---|---|---|
| main_test.go | 留包 | run/observabilityOptions/runHealthcheck 进程胶水白盒直测;单元层 |
| cmd/server/demo_subject_test.go | 留包 | 解析器/守卫族纯逻辑直测 |
| cmd/server/demo_admin_test.go | 留包 | demo_admin 胶水直测(经 seeded server 取 demo staff) |
| cmd/server/demo_notification_test.go | 留包 | payload 提取器纯函数直测 |
| cmd/server/demo_users_test.go | 留包 | seed/查重逻辑直测(registerDemoUser 直呼) |
| cmd/server/demo_user_header_kill_switch_test.go | (iii) 候选 | 解析器族直测 + HTTP 双路径混合;若整体外移需拆出白盒半 |
| clinic_name_test.go | 留包 | clinic-name 服务级直测(writeClinicNameError 等) |
| fragment_wire_paths_test.go | 随流迁 | 无 Test 函数的夹具文件(片段线路径常量);流套件的 wire 锚点 |
| frontend_test.go | (ii) | 需 cfg.WebDistDir 装配选项与夹具 dist 目录 |
| server_test.go | (ii) | 基座文件本身 + 32 个 TestBuildServer_* 流测试;整个基座随迁 |
| authn_e2e_test.go | (ii) | buildAuthnE2EServer:渠道/代理/社交桩配置变异 |
| authn_enterprise_oidc_login_start_test.go | (i) | 纯 HTTP;OIDC 由 authn 模块自带桩 |
| admin_flow_test.go | (ii) | admin 域装配 + staff 令牌/审计路径 + rbacService 句柄 |
| org_audit_capture_test.go | (ii) | org+admin 双 server 组装、审计行经 HTTP 读回 |
| org_flow_test.go | (ii) | 邀请邮件捕获 tokenFromMail(需捕获注入) |
| org_clinic_invitation_test.go | (ii) | 同上 + 自服务注册旅程 |
| org_invitation_signin_test.go | (ii) | 同上 |
| org_route_guards_test.go | (ii) | rbacService 句柄 + 守卫域 |
| team_members_test.go | (ii) | 邮件捕获 + seeded 装配 |
| register_tenant_attest_flow_test.go | (i) | 纯 HTTP 注册/成员旅程 |
| self_service_clinic_name_test.go | (i) | 注册旅程 + clinicNamePath 常量 |
| self_service_no_ledger_test.go | (i) | 注册旅程无账本断言 |
| self_service_test.go | (iii) | 直测 selfServiceProvisioner/JobHandler/configFromEnv 混合 |
| self_service_entitlements_test.go | (ii) | billing 模块句柄(openBillingModule) |
| cases_flow_test.go | (i) | 纯 HTTP(cases 辅助网随迁) |
| cases_photos_test.go | (i) | 同上 |
| notes_delete_restore_flow_test.go | (i) | 纯 HTTP 笔记旅程 |
| notification_flow_test.go | (ii) | capturingMailer/lockedBuffer 捕获注入 + 通知装配 |
| smilesim_flow_test.go | (ii) | 同上(短信/邮件捕获)+ cfg |
| smile_journey_flow_test.go | (i) | 纯 HTTP(假 AI 服务器随迁) |
| fragment_surface_flow_test.go | (i) | 纯 HTTP 双片段旅程(原 p3_ 文件) |
| consult_flow_test.go | (i) | 假 AI 服务器注入在随迁辅助内 |
| ai_gateway_flow_test.go | (i) | 同上 |
| usage_summary_flow_test.go | (ii) | admin 装配 + 计量种子 |
| billing_credit_flow_test.go | (ii) | openBillingCredits 信用读回 |
| billing_http_flow_test.go | (ii) | 同上 |
| entitlements_flow_test.go | (ii) | openBillingModule 句柄 |
| attestation_flow_test.go | (i) | 纯 HTTP + pki 路径常量 |
| pki_revoke_gate_flow_test.go | (i) | 纯 HTTP + 常量 |
| periodic_pki_scan_flow_test.go | (ii) | cfg 变异(periodicFlowTickInterval) |
| periodic_scheduler_flow_test.go | (ii) | cfg 变异 + openSecondDB + 物理删行 |
| compliance_flow_test.go | (ii) | buildServer 返回的 compliance 句柄 + openSecondDB |
| compliance_race_test.go | (ii) | 同上 + 并发回拨 |
| tenant_config_reader_flow_test.go | (ii) | compliance/sharing 服务句柄直构阅读器 |
| apikey_flow_test.go | (i) | 纯 HTTP + integration 常量 |
| apikey_authenticate_flow_test.go | (i) | 纯 HTTP + 限流窗口常量 |
| webhook_flow_test.go | (ii) | webhook 接收服务器 + org 邀请触发 + 邮件捕获 |
| webhook_crud_flow_test.go | (ii) | 同上(CRUD 半) |
| storage_flow_test.go | (i) | 纯 HTTP + 上传辅助网 |
| share_journey_flow_test.go | (i) | 纯 HTTP(分享/微笑辅助随迁) |
| sharing_flow_test.go | (i) | 纯 HTTP |
| config_public_endpoint_gates_test.go | (ii) | seedConfigRows 配置行种子写入 |
| obs_route_seed_test.go | (i) | obs 导出 API + 装配入口即足 |
| rbac_node_deleted_reap_test.go | (ii) | org/rbac 服务句柄 + 回收 harness |
| rbac_restore_reinstate_test.go | (ii) | 同上 |

分档统计:留包(单元层)6;白盒例外 (iii) 2(self_service_test、demo_user_header_kill_switch_test);夹具随流 1(fragment_wire_paths_test);迁移对象 (i) 19、(ii) 27,共 46 文件(server_test.go 的基座计在 (ii) 内)。

## 侦察二:五个 conformance 驱动文件

仓库里 `*_conformance_test.go` 命名的共五个;共同形态:它们都已是**外部测试包**(`pkgcore_test`/`jobs_test`)的薄驱动——真正的共享契约套件早已住在各 seam 的支撑包,驱动文件只因"内部测试文件导入会回导 pkgcore/jobs 的支撑包会成环"而被迫外部化(文件头自述这是既有布局规则认名的机械例外)。

| 文件 | 钉什么 | 套件所在(支撑包) | 支撑包黑盒可导入? |
|---|---|---|---|
| go/pkgcore/eventbus_conformance_test.go | `NewMemoryEventBus` 满足 `eventbustest.AssertConforms`;内存总线"额外属性"(同步投递错误回传)由 `memory_eventbus_test.go` 在包内另行钉住 | go/pkgcore/eventbustest | 是:`AssertConforms(t, caps, factory)`,签名公开 |
| go/pkgcore/kv_conformance_test.go | `NewMemoryKVStore` 以能力 0(单实例、无能力声明)满足 `kvstoretest.AssertConforms` 的单实例检查段 | go/pkgcore/kvstoretest(另有 AssertSurvivesRestart) | 是 |
| go/pkgcore/mailer_conformance_test.go | `NewSMTPMailer` 满足 `mailertest.AssertConforms`,假中继为进程内 net.Listener(单元层,无 Docker);文件内 `smtpMailerFor` 因环约束重复了 smtp_mailer_test.go 的 mailerFor | go/pkgcore/mailertest | 是 |
| go/pkgcore/objectstore_conformance_test.go | `NewLocalObjectStore(t.TempDir())` 满足 `objectstoretest.AssertConforms` | go/pkgcore/objectstoretest(另有 AssertSurvivesRestart) | 是 |
| go/jobs/queue_conformance_test.go | `NewStandaloneQueue(dbtest.NewSQLite(t), …)` 满足 `queuetest.AssertConforms`;asynq 侧同套件由其 integration_test 的驱动运行 | go/jobs/queuetest(另有 AssertFailsClosedOnUnreadableCancellationState) | 是 |

要点与移动评估:

1. **共享套件已是可导入支撑包**(五个 `AssertConforms` 全为导出函数,工厂参数形态即"实现自证契约"的驱动协议),跨实现契约套件一类的去向在规则文本里本就是"queuetest/eventbustest 模型"——该模型的套件一半早已就位。
2. 剩余问题是**驱动文件本身**的去向。它们钉的是 pkgcore/jobs **根包自有内建实现**(内存总线/内存 KV/SMTP/本地对象存储/StandaloneQueue),与实现同目录正是"紧邻被测代码"的单元层姿态(全部无外部依赖、普通单元运行执行、无 Docker),唯包名是外部的。按"单元测试=层级"的判据,它们随根包单元层留目录、以 `package x_test` 为机械例外,与规则文字相容;把它们硬塞进支撑包反而颠倒归属(支撑包是契约的家,不是某一实现的测试家——redis/asynq 等子包实现各自的驱动就放在自己子包里)。
3. 若批次 2 裁定这些驱动也必须离开根目录,唯一合法去处是各自支撑包内以 `eventbustest_test` 式外部测试文件安放;技术上无环(外部测试包可导入 pkgcore 根),但会造成"内存实现的合规驱动住在 eventbustest、与 redis 驱动的家(redis 子包)不一致"的归属错位。结论:建议维持现状并写入标准文档作为该类别的样板注释,而非迁移。

## 批次 2 执行建议

1. 先做一次"导出面干跑":按方案 A 清单在分支上试导出最小装配入口与常量,确认 (i) 类 21 文件可编译为外部黑盒包;(ii) 类逐文件设计入口点形态(句柄聚合体/捕获注入选项/DB 连接选项);(iii) 与留包文件保持原样并在此文档登记例外理由。
2. 迁移目录形态建议:同模块内专有目录(如 `examples/reference-app/cmd/servertest/` 或参考模块 `integration_test/` 命名惯例),随迁文件仅保留 `package …_test`;CI 矩阵(reference-app job 的 `go test` 路径)同步;若采纳"测试装配入口"文件,确认 fast-check/full-check 的 lint/vet 对新导出面无告警。
3. 前端侧组合流当前都在单元层(脚本化 fetch,无真服务器),不属本迁移;Playwright e2e 已在 `e2e/`。
4. 验收:迁移后 reference-app 全套件在普通单元运行与 `-race` 下绿;除 (iii) 登记外,`cmd/server/` 源码目录不再出现组合 HTTP/装配流测试;本文档表格与实际情况收敛后,把类别样板(conformance 驱动留根目录、流套件入应用级测试目录)回填后端技能 §13 的示例清单。


## 批次 2a 处置记录(套件并入回退)

批次 2a 把一批"行为命名测试文件"逐文件裁定为并入或保留。判据沿用本文开头布局规则与后端技能 §13:套件映射单一主导源 → 并入该源既有测试文件;真双源交互或跨包结构约束 → 保留行为命名文件;并入使目标越过 ~1000 行 → 机械拆分并保留目标名前缀。

### 已并入

| 文件 | 去向 | 备注 |
|---|---|---|
| go/org/email_index_column_drift_test.go | go/org/invitation_test.go | EmailIndexColumn 单一源 invitation.go |
| go/jobs/queue/asynq/metric_registration_failure_test.go | go/jobs/queue/asynq/queue_test.go | registerJobMetrics/registerQueueDepthGauge 属 queue.go |
| go/jobs/queue/asynq/unreachable_redis_test.go | go/jobs/queue/asynq/queue_test.go | 不可达后端错误面属 queue.go |
| go/dbkit/dialect/sqlite/busy_timeout_test.go | go/dbkit/dialect/sqlite/dialect_sqlite_test.go | busy_timeout 契约属 dialect_sqlite.go |
| go/dbkit/audit/append_only_test.go | go/dbkit/audit/repository_test.go | append-only 应用层属 repository.go 的 insert-only 方法集 |
| go/storage/sweep_window_test.go | go/storage/cleanup_test.go | expiry-sweep 窗口与幂等键属 cleanup.go |
| go/saasctl/internal/template/preauth_exemption_test.go | go/saasctl/internal/template/embed_test.go | 唯一源 embed.go |
| go/saasctl/internal/template/readme_consistency_test.go | go/saasctl/internal/template/embed_test.go | 唯一源 embed.go |
| go/billing/poll_window_test.go | go/billing/job_test.go | poll 窗口与幂等键属 job.go |
| go/compliance/retention_sweep_window_test.go | go/compliance/retention_test.go;并入后 config 接线族拆至 retention_config_test.go | 目标文件越过 ~1000 行,机械拆分保留 retention_ 前缀 |
| examples/reference-app/internal/smilesim/disconnect_robustness_test.go | 并入 service 测试族;credit-settlement 族与断连套件拆至 service_settlement_test.go | 目标文件越过 ~1000 行,机械拆分保留 service_ 前缀 |
| go/jobs/option_validation_test.go | go/jobs/standalone_queue_test.go;并入后 cancel-race 族拆至 standalone_queue_cancel_test.go | 目标文件越过 ~1000 行,机械拆分保留 standalone_queue_ 前缀 |
| go/dbkit/soft_delete_unique_index_test.go | go/dbkit/soft_delete_test.go | partial unique index 属 soft_delete.go |

### 裁定保留(行为命名文件合法,记录理由)

| 文件 | 保留理由 |
|---|---|
| go/jobs/queue/asynq/job_outcome_metrics_recording_test.go | 真双源交互:worker.go 的 processTaskUncancelled/handleErrorAttempt 与 queue.go 的 registerJobMetrics 一起被测 |
| go/jobs/queue/asynq/marker_fail_closed_test.go | 真双源交互:worker.go 的 dispatchAfterMarkerRead/handleErrorAttempt 与 queue.go 的 readCancelMarker 一起被测 |
| go/notification/address_index_column_test.go | 双源交互:AddressIndexColumn 钉 VerifiedContact(contact.go)与 PlatformBlacklist(blacklist.go)两模型的 gorm tag 与迁移列;contact_test.go 已超行 |
| go/notification/hub_http_test.go | 双源交互:handler.go 的 handleStream 经 hub.go 的订阅在真实 HTTP 服务器上被测,handler_test.go 只覆盖无 socket 半 |
| go/observability/factory_vars_test.go | 白盒/黑盒包边界:测 init.go 的未导出 otlpFactory/metricsReaderFactory,必须留 package observability;init_test.go 是黑盒 observability_test,两者不可同文件,按技能 §13 白盒机械例外命名 |
| go/observability/exporter/otlp/invalid_utf8_export_test.go | 跨包组合:obs.Init + obs.Middleware + 本包导出器一起被测,非单一源;该目录无其它非 example 测试文件可并入 |
| go/observability/exporter/prometheus/otlp_not_registered_test.go | 依赖"本测试二进制从不导入 exporter/otlp"的二进制形态;并入 prometheus_test.go 不改变被测语义,文件自述此归属理由 |
| go/dbkit/tenant_scope_tenantmodel_test.go | 双源交互:TenantModel(tenant_scope.go)经 repository.go 的 Repository[T] 被测;并入 tenant_scope_test.go 会使其 1521+257 超行,并入 repository_test.go 会错置被测对象 |

### conformance 驱动裁定

五个根目录 `*_conformance_test.go` 驱动(pkgcore 四个、jobs 一个)维持原状:它们是钉住 pkgcore/jobs 根包自有内建实现的外部包薄驱动,与被测实现同目录即"紧邻被测代码"的单元层姿态(无外部依赖、普通单元运行执行),共享契约套件本身已住在可导入支撑包(eventbustest/kvstoretest/mailertest/objectstoretest/queuetest),驱动文件只是"实现自证契约"的最后一跳;把驱动塞进支撑包会颠倒归属(支撑包是契约的家,不是任一实现的测试家)。此裁定作为该类别样板,回填进后端技能 §13 示例清单的后续修订。


## 批次 2b 处置记录:组合 HTTP/装配流套件迁入应用级测试目录

2b 依上文侦察表执行装配流套件的目录迁移。前置(2b-i)已把 `cmd/server` 的装配代码整体移入可导入的 `examples/reference-app/internal/app`(包 `app`),`cmd/server` 只剩 `main.go` 薄壳,内部 app 的导出面即测试入口面;本记录(2b-ii)移动测试文件本身,`cmd/server` 的 `_test.go` 从 55 个减到 9 个。

### 目标目录设计

新目录为 **`examples/reference-app/flowtests/`**,包名 **`flowtests`**,纯测试目录(全部文件为 `_test.go`,无 build tag,普通单元运行执行——与 `integration_test/`(Docker 层,`-tags=integration`)互不混淆)。

- 位置取应用级而非 `cmd/` 之下:它是应用装配的测试面,不属于命令;`internal/app` 的 Go internal 导入规则要求目录位于 `examples/reference-app/` 之下,此位置满足。
- 包名取 `flowtests` 而非 `<pkg>_test` 外部式:目录内没有可配对的基础包,外部式命名是虚构配对;迁移文件的机械改动因此只有一行 package 子句。
- 迁移后 flowtests 套件驱动内部 app 导出的 `BuildServer`/`ServerConfig`/demo 身份与常量面,经真实 HTTP 与真实 SQLite 跑组合装配,与迁移前行为逐字节一致(断言未动,仅 package/导入行变更)。

### 迁移清单(46 + 1 文件)与侦察表逐项对齐

侦察表 (i) 19 文件、(ii) 27 文件(含基座 server_test.go)全部随迁,夹具 `fragment_wire_paths_test.go` 随流迁。文件级处置与侦察表一致,无重分档;仅两处按规则做了行数驱动的机械拆分:

| 原文件 | 拆分 | 分片与内容 |
|---|---|---|
| server_test.go(2091 行) | 3 片 | `server_test.go`(基座辅助 + 笔记/权限/隔离/未认证/租户提示/审计装配流,886 行)、`server_config_test.go`(ConfigFromEnv 族 + 根密钥派生 + 特征开关/社交渠道旗标单元,~800 行)、`server_guards_test.go`(预认证端点守卫 + healthz/metrics allowlist + distributed 装配拒绝,~435 行) |
| periodic_scheduler_flow_test.go(1185 行) | 2 片 | `periodic_scheduler_flow_test.go`(过期/保留清扫两腿,~816 行)、`periodic_scheduler_flow_clinic_test.go`(自注册诊所腿与共享辅助,~383 行,文件头承接原文件头中该腿的叙述) |

### 留包文件复核与例外登记(8 文件)

2b-i 把被测代码移入内部 app 后,侦察表"留包/白盒"栏的原始前提(与 package main 同包直测未导出符号)已不成立——cmd/server 已无未导出生产代码可测,8 个文件现都只经内部 app 的导出面工作。按侦察表处置保留,理由按现状登记:

- `main_test.go`:唯一真正的命令包单元测试(run/runHealthcheck/observabilityOptions/healthcheckArg 直测),留包前提完好。
- `cmd/server/demo_subject_test.go`、`cmd/server/demo_notification_test.go`、`clinic_name_test.go`:纯单元钉(解析器/守卫族、payload 提取器、诊所名 handler 与错误辅助),各自无跨包辅助依赖;被测对象现居内部 app,后续批次可把它们移到被测代码旁(内部 app 的 `*_test.go`)。
- `cmd/server/demo_admin_test.go`、`cmd/server/demo_users_test.go`:(iii) 之外按侦察表留包的消费证明套件,仍经种子装配跑组合 HTTP;留包意味着 `cmd/server` 仍有少量装配流测试,与验收线"除 (iii) 登记外不再出现"的出入按本表登记。
- `self_service_test.go`、`cmd/server/demo_user_header_kill_switch_test.go`:(iii) 白盒例外按侦察表保留;复核确认其"直测未导出"前提已随 2b-i 消失(全部经导出面),现为纯逻辑单元钉与组合 HTTP 旅程的混合,留包理由按现状更新,待后续批次重新裁定。
- 与 2b-i 的 doc.go 表述衔接:`internal/app/doc.go` 现述 flowtests 为装配流套件的目录。

### 跨包辅助镜像(Go 测试辅助不可跨包导入的代价)

侦察表预告的"留在包内的必须迁走或重造"双向兑现,各建一个支撑文件,内容从原定义逐字节抽取(仅剔去指代源文件自身的表述),头部注明镜像关系与保持同步的要求:

- `cmd/server/test_support_test.go`:镜像迁走的基座辅助(testConfig/buildTestServer/registerAndAuthenticate/notesRequestAs/assertPermissionDenied/note 线型与辅助)与 cases/notification 请求辅助及 `casesPath` 常量,供留包的 demo/自服务/kill-switch 套件使用。
- `flowtests/test_support_test.go`:镜像留包套件里的 demo 身份与自服务旅程辅助(demoLogin、种子口令、browserSignIn、registerFreshAccount、自服务常量、failOnceProvisioning、waitForClinicMembership),供随迁的 admin/pki/团队/自服务/诊所邀请套件使用。

### 导出增补

无。2b-i 的导出面覆盖 46+1 个迁移文件与 8 个留包文件的实际引用(编译即证,未新增任何内部 app 导出)。

### 验证记录

- 普通单元运行(`go test ./...`,examples/reference-app):cmd/server 4.97s、flowtests 164.10s,全绿;`-race` 同套件绿。
- `go vet ./...`、`golangci-lint run ./...` 0 issues、gofmt 干净(与 CI 形态一致)。
- 应用二进制 `go build ./cmd/server` 通过;`tools/scan_cjk.py` 全树干净;reference-app web 侧未触及。
- 装配流套件仍属无外部依赖的普通单元运行,留在单元套件内(CI 的 `go test ./...` 自动覆盖新目录,工作流无需改动)。

## 迁移后的引用重定向记录

2b 只移动了文件本身;仓库散文对这批套件的旧 `cmd/server/<文件>_test.go` 归属的引用仍残留 102 处(91d6cb95 上实测:落于 98 行、跨 41 个文件),分布在根 CLAUDE.md 普查、模块 AGENTS/README、`docs/internal` 各实现对照、reference-app 内部包注释与 web 侧测试工具注释里。本记录逐项重定向到 `flowtests/`(全仓库 `cmd/server/<47 个迁走文件>` 前缀引用为零命中):

- **形态随句子原有路径保留**:原 `examples/reference-app/cmd/server/X_test.go` 改为 `examples/reference-app/flowtests/X_test.go`,原短式 `cmd/server/X_test.go` 改为 `flowtests/X_test.go`;裸文件名引用(前文已点名 reference-app 的句子)补 `flowtests/` 前缀。
- **按行为重定向拆分文件**:引用 `server_test.go` 时若所指行为随机械拆分到了 `server_config_test.go`(ConfigFromEnv/根密钥/特征开关族)或 `server_guards_test.go`(healthz/metrics allowlist、分布式装配拒绝族),则指向分片所在文件;web 侧对 `server_test.go:443-445`(notes 写门 403 钉)等行号引用同步到新家 `flowtests/server_test.go:438-440` 的实际行。`periodic_scheduler_flow_test.go` 的两 boot 过期清扫/保留腿仍在同名文件,引用保持不变式地指向 `flowtests/` 同名文件。
- **留包文件的引用不动**:`main_test.go`、demo_* 与 clinic_name/self_service/kill-switch 套件仍居 `cmd/server`,指向它们的引用(如 cmd/server/demo_users_test.go 的行号钉)原样保留;两镜像 `test_support_test.go` 各自头部已述镜像关系。
- **error-codes.md 随源再生成**:索引文档内嵌错误常量处注释,其中两处仍述旧路径;同时该生成产物在装配代码迁入 internal/app(导出改名)后即已漂移,本次一并 `tools/gen_error_code_index.py` 再生成归位。

验证:全仓库对 47 个迁走文件名的 `cmd/server/<文件>` 形式引用零命中;裸文件名引用除 flowtests/cmd-server 目录内互引与本文档自身的迁移清单外全部带 `flowtests/` 前缀;reference-app `go build ./...` 通过。根 CLAUDE.md 普查句此前部分改写的宣告(随 2b 提交消息)按本记录修正:当时只重定向了少数文件,残余引用由本记录闭合。


---

# 批次 3 处置记录:无 target 单元测试迁入模块专有单元测试目录(规则细化、侦察与迁移蓝图)

本记录由新用户规则驱动:单元层级、但没有针对的 target 源文件的测试文件(不是 `<源>_test.go`、也不是 1:1 映射)一律离开源码目录、进入单独的测试目录。它细化本文开头"批次 1"写入的布局规则:批次 1 允许"跨越多个源文件的包内行为套件留在包内、以行为命名",批次 2a 把"映射单一主导源"的套件并入该源既有测试文件;**残留(无单一 target 者)由本规则迁移出包**。2a/2b 部分"裁定保留"记录的效力随之更新:黑盒可迁者迁,白盒不可迁者按 Go 强制例外登记留包。本批次只侦察、定规则、出清单,不移动文件;移动由执行批次依清单进行。

## 规则细化后的文本(落三处标准家 + 本文)

三处措辞按各自语境写入(英文):根 `CLAUDE.md` Testing 小节、后端技能 §13、前端技能 §12;本文为中文记录。核心语句:

- 有 target 的单元测试紧邻其 target:一个源一个文件;跨源套件若映射单一主导源,并入该源自己的测试文件;行数驱动的机械拆分保留目标名前缀。
- **无单一 target 的单元层测试**——无主导源的行为套件、仓库/模块形态检查、模块根包自有内建的契约驱动——进入**模块的专有单元测试目录 `go/<module>/unittest/`(包名 `unittest`)**,对其包做黑盒外置测试,绝不散落在源码包里。
- Go 自身包规则逼出的例外留在源码包内、逐一登记(逐文件清单与理由见本记录):必须触达未导出符号的白盒套件;`example_test.go`(godoc 示例);`<target>_bench_test.go`(白盒基准);紧邻 `go:embed` 迁移集的迁移套件;钉"本测试二进制不导入某包"的形态测试。
- 前端 TS 无包边界机制,本规则不适用(前端 §12 明示):TS 行为套件继续就地共置。

## 侦察三:残留清单与逐文件分档

### 方法与范围

对 `go/` 与 `examples/` 下全部 `*_test.go`(排除 `integration_test/`、`flowtests/`、`e2e/` 等既有专有目录)做机械扫描,共 485 个文件;其中 362 个为 `<源>_test.go` 且源在目录内(有 target,合规),123 个无同名源文件。123 个中 57 个为 `*example*` 命名文件(全部仅含 godoc `Example*` 函数,归入命名例外),余 **66 个为逐文件分档对象**。判据:包名(包内/外部);是否引用本包非测试文件的未导出声明(白盒,以词法探针初筛 + 人工复核,探针局限见"验证记录");钉什么;target 状态。带 `//go:build` 者(非单元层)另行记录。

### 分档代码

| 码 | 类 | 判定 |
|---|---|---|
| U | 无 target 黑盒可迁 | 迁入模块 `unittest/` 目录 |
| WB | 白盒(引本包未导出生产符号) | 留包登记(Git 规则强制) |
| S | `<源>` 前缀机械拆分片(目标名保留) | 留包(2a 拆分惯例) |
| M | 迁移套件(钉迁移集/go:embed 固定) | 留包(既有类别) |
| BENCH | 白盒基准 | 留包(命名例外) |
| SHAPE | 钉"本测试二进制不导入某包" | 留包登记(Git 规则强制) |
| OOS | 带 build tag 的非单元层文件 | 不属本规则 |
| SELF | 契约支撑包自验套件 | 留包(映射支撑包单一源,2a 式并入队列) |
| T | 支撑包双入口合测自述例外 | 留包登记 |
| FIXT | 零测试函数夹具 | 随其消费方留包/随迁 |
| G | cmd/server 集群(2b 收尾重裁定) | 按 target 旁置/应用级目录路由 |

### 逐文件分档总表(66 文件)

| 文件 | 包 | 盒 | 钉什么 | 分档 |
|---|---|---|---|---|
| go/pkgcore/eventbus_conformance_test.go | pkgcore_test | 黑(外) | `NewMemoryEventBus` 满足 `eventbustest.AssertConforms` | U(驱动) |
| go/pkgcore/kv_conformance_test.go | pkgcore_test | 黑(外) | `NewMemoryKVStore` 满足 `kvstoretest.AssertConforms` | U(驱动) |
| go/pkgcore/mailer_conformance_test.go | pkgcore_test | 黑(外) | `NewSMTPMailer` 满足 `mailertest.AssertConforms`(进程内假中继) | U(驱动) |
| go/pkgcore/objectstore_conformance_test.go | pkgcore_test | 黑(外) | `NewLocalObjectStore` 满足 `objectstoretest.AssertConforms` | U(驱动) |
| go/pkgcore/standalone_build_test.go | pkgcore | 自(无引用) | 模块 GOWORK=off 独立可建/可 vet(仓库形态) | U(需路径锚定编辑) |
| go/pkgcore/go_work_use_block_test.go | pkgcore | 自(无引用) | go.work `use` 块覆盖全部模块(仓库形态) | U(需路径锚定编辑) |
| go/pkgcore/kernel_shutdown_test.go | pkgcore | 白 | 关停顺序:preset 键、`seamCloserOf`、注册模块生命周期 | WB |
| go/pkgcore/eventbus/postgres/wedged_local_publish_marks_bounded_test.go | postgres | 白 | 卡死本地 Publish 的 mark 有界(`go:build integration` 包内切片) | OOS |
| go/pkgcore/eventbustest/accepts_buses_that_cannot_report_handler_errors_test.go | eventbustest | 黑(包内) | 套件验收侧不过度断言(支撑包自验,映射 assert_conforms.go) | SELF |
| go/pkgcore/eventbustest/assert_conforms_redis_pair_test.go | eventbustest | 黑(包内) | 双实例检查经 miniredis 真跑(支撑包自验) | SELF |
| go/pkgcore/eventbustest/assert_conforms_rejects_defective_buses_test.go | eventbustest | 白 | check\* 拒绝方向有牙(支撑包自验) | SELF |
| go/jobs/queue_conformance_test.go | jobs_test | 黑(外) | `NewStandaloneQueue` 满足 `queuetest.AssertConforms` | U(驱动) |
| go/jobs/fail_closed_cancellation_state_test.go | jobs_test | 黑(外) | `AssertFailsClosedOnUnreadableCancellationState` 经真表改名注入 | U(驱动族) |
| go/jobs/claim_window_test.go | jobs | 黑(包内) | 认领未启动窗口 Attempts 诚实性(store.go+worker.go 双源) | U(需辅助迁出) |
| go/jobs/candidate_window_fairness_test.go | jobs | 白 | 候选窗口公平(claimBatchSize) | WB |
| go/jobs/database_fault_paths_test.go | jobs | 白 | store 层故障路径(jobRecord/createJobsTableSQL/写者注册门/reset 等 20 测) | WB |
| go/jobs/metric_ordering_test.go | jobs | 白 | 成功/取消竞态下时长直方图顺序(jobDurationMetricName、markCancelled) | WB |
| go/jobs/metric_registration_failure_test.go | jobs | 白 | 指标注册失败即建队失败(jobDurationMetricName) | WB |
| go/jobs/retryable_start_test.go | jobs | 白 | 可重试启动(建表/丢表, jobsTable) | WB |
| go/jobs/single_writer_test.go | jobs | 白 | 单写者注册(ensureJobsSchema) | WB |
| go/jobs/writer_gate_liveness_test.go | jobs | 白 | 写者门活性(queueWritersTable、findByID) | WB |
| go/jobs/scheduler_test.go | jobs | 白 | 周期调度器(声明遍历、租户展开、窗口截断、键派生、生命周期与选项校验) | WB |
| go/jobs/standalone_queue_cancel_test.go | jobs | 黑(包内) | standalone_queue_test 取消竞态族(2a 行数拆分片) | S |
| go/jobs/standalone_queue_bench_test.go | jobs | 白 | StandaloneQueue 基准 | BENCH |
| go/jobs/queue/asynq/job_outcome_metrics_recording_test.go | asynq | 白 | handleErrorAttempt/wrapFailedAttempt 双源结果度量 | WB |
| go/jobs/queue/asynq/marker_fail_closed_test.go | asynq | 白 | dispatchAfterMarkerRead/errCancelMarkerUnreadable | WB |
| go/jobs/queuetest/assert_fails_closed_rejects_defective_queues_test.go | queuetest | 白 | 故障层中间态拒绝有牙(支撑包自验) | SELF |
| go/jobs/queuetest/assert_fails_closed_rejects_swallowing_queues_test.go | queuetest | 白 | 吞读故障拒绝有牙(支撑包自验) | SELF |
| go/dbkit/tenant_scope_tenantmodel_test.go | dbkit | 黑(包内) | TenantModel 三路测(tenant_scope.go 测试族行数拆分片,头注自述) | S |
| go/dbkit/audit/migrations_test.go | audit | 黑(包内) | audit 迁移集 round-trip/列宽常量(迁移套件) | M |
| go/dbkit/dbtest/dbtest_test.go | dbtest_test | 黑(外) | NewSQLite+NewPostgres 双入口合测(支撑包,头注自述例外) | T |
| go/notification/address_index_column_test.go | notification | 白 | VerifiedContact+PlatformBlacklist 表名/列常量双模型 | WB |
| go/notification/hub_http_test.go | notification | 白 | handleStream 经 hub 真 HTTP(apiPath) | WB |
| go/notification/subscribe_test.go | notification | 白 | 订阅类型过滤与坏公告丢弃(直读 module.hub,module.go:85 未导出字段) | WB |
| go/notification/delivery_bench_test.go | notification | 白 | 派生投递键基准 | BENCH |
| go/notification/preference_service_bench_test.go | notification | 黑 | 偏好解析基准 | BENCH |
| go/observability/factory_vars_test.go | observability | 白 | init.go 未导出 otlpFactory/metricsReaderFactory 并发替换 | WB |
| go/observability/exporter/otlp/invalid_utf8_export_test.go | otlp_test | 黑(外) | 非法 UTF-8 请求导出批仍到达(跨包组合) | U |
| go/observability/exporter/prometheus/otlp_not_registered_test.go | prometheus_test | 黑(外) | 未注册 otlp 工厂即失败;钉二进制不导入 exporter/otlp | SHAPE |
| go/admin/impersonation_service_start_dispatch_test.go | admin | 白 | 冒名开始即派发安全通知(notificationGroupSecurity 生产常量) | WB |
| go/compliance/retention_config_test.go | compliance | 黑(包内) | 保留配置接线族(retention_test 2a 行数拆分片) | S |
| go/sharing/sweep_window_test.go | sharing | 白 | 过期清扫窗口/幂等键/恢复(expirySweepHandler 等) | WB |
| go/pki/enqueue_window_test.go | pki | 白 | 过期扫描/CRL 再生窗口幂等键 | WB |
| go/pki/key_ref_widening_test.go | pki | 白 | 引用加宽迁移(tableSigningKeys 等) | WB |
| go/authn/signin_channel_feature_gates_test.go | authn | 白 | 渠道特征开关拒绝(service.go 生产 hasCode) | WB |
| go/authn/password_bench_test.go | authn | 白 | 口令哈希基准 | BENCH |
| go/authn/token_bench_test.go | authn | 白 | 令牌签发基准 | BENCH |
| go/authn/standalone_build_test.go | authn | 自 | 模块 GOWORK=off 独立可建(仓库形态) | U(需路径锚定编辑) |
| go/tenancy/standalone_build_test.go | tenancy | 自 | 模块 GOWORK=off 独立可建(仓库形态) | U(需路径锚定编辑) |
| go/ratelimit/no_cjk_characters_test.go | ratelimit | 自 | 模块目录注释/Markdown 无 CJK(仓库形态) | U(需路径锚定编辑) |
| go/integration/webhook_redelivery_test.go | integration | 白 | 重投递幂等键/载荷形状/重投递语义 | WB |
| go/billing/gateway/alipay/testkeys_test.go | alipay | —(夹具) | RSA 键对夹具,零测试函数,消费方 gateway_test 等 | FIXT |
| go/billing/gateway/wechat/testkeys_test.go | wechat | —(夹具) | 键/密文夹具,零测试函数,消费方 decrypt/notify/sign 测试 | FIXT |
| go/org/migrations/upgrade_test.go | migrations | 黑(包内) | 0008 升级(0007 时代库,go:embed 子集) | M |
| go/org/migrations/upgrade_postgres_test.go | migrations | 黑(包内) | 同上 PostgreSQL 腿(build tag) | M |
| go/metering/migrations/upgrade_test.go | migrations | 黑(包内) | 0007 升级(0006 时代库,go:embed 子集) | M |
| go/metering/migrations/upgrade_postgres_test.go | migrations | 黑(包内) | 同上 PostgreSQL 腿(build tag) | M |
| go/rbac/service_bench_test.go | rbac | 白 | 服务基准(白盒触 svc.cache/grantKey) | BENCH |
| examples/reference-app/cmd/server/demo_subject_test.go | main | 黑(app 面) | 解析器/守卫纯逻辑(target 现居 internal/app) | G→internal/app |
| examples/reference-app/cmd/server/demo_notification_test.go | main | 黑(app 面) | payload 提取纯逻辑(target 现居 internal/app) | G→internal/app |
| examples/reference-app/cmd/server/clinic_name_test.go | main | 黑(app 面) | 诊所名 4 路由 + 1 直测(混合) | G→拆分(路由半→flowtests,直测半→internal/app) |
| examples/reference-app/cmd/server/demo_user_header_kill_switch_test.go | main | 黑(app 面) | kill-switch 解析器逻辑 + HTTP 双路径(混合) | G→拆分(逻辑半→internal/app,HTTP 半→flowtests) |
| examples/reference-app/cmd/server/demo_admin_test.go | main | 黑(app 面) | seeded 装配消费证明(旅程) | G→flowtests |
| examples/reference-app/cmd/server/demo_users_test.go | main | 黑(app 面) | 种子账号门/重启存续旅程 | G→flowtests |
| examples/reference-app/cmd/server/self_service_test.go | main | 黑(app 面) | 自服务注册旅程 + 1 直测 | G→flowtests |
| examples/reference-app/cmd/server/test_support_test.go | main | —(夹具) | 基座辅助镜像(2b 为留包套件所建) | G→随消费方分迁/清退 |
| examples/reference-app/internal/smilesim/service_settlement_test.go | smilesim | 白 | 结算/孤儿退款前缀族(2a 拆分片 + 白盒) | S |

计数:U 13;WB 20;S 4;M 5;BENCH 6;SHAPE 1;OOS 1;SELF 5;T 1;FIXT 2;G 8。= 66。

不属于分档对象的说明:`*example*` 命名 57 文件(godoc 示例例外)与 `<target>_test.go` 362 文件合规;`cmd/server/main_test.go` 有 target(main.go,包内白盒单元),留包。

## 设计决定:模块专有单元测试目录 `go/<module>/unittest/`

Go 包规则的推演:测试文件移到另一个目录就成为不同包,只能经被测包的导出面黑盒测试;白盒文件按 Go 规则根本不能迁(例外登记见上)。专有目录必须仍在普通单元运行里被执行(无 build tag、无 `-tags` 门),否则 CI 覆盖与单元套件丢失它。三个候选形态对比:

- **(a) 模块根旁的每模块测试目录**(如 `go/jobs/behaviortests/`):模块即发布单元,CI 按模块跑 `go test ./...`,目录天然被覆盖;同目录文件共享一个包,故各模块只需一个目录、一个包名。
- (b) 逐包平级目录:粒度最细,但白盒文件反正不能迁,黑盒可迁文件多数位于模块根包;为每个子包开目录徒增目录数,与 (a) 等价收益。
- (c) 每模块一个目录收纳其全部包的黑盒可迁套件:外部测试包可导入模块内任意包(含 `internal/`),(a) 的形态已天然支持,无需分层。

选定 (a):目录名与包名取 **`unittest/`**、**`unittest`** —— 与 `integration_test/` 按层级命名的惯例平行,目录即用途;全 `_test.go` 文件目录只被 `go test` 编译、不进入 `go build`,对消费者零成本(flowtests 先例已验证:CI 的 `go test ./...` 自动覆盖新目录,工作流无需改动)。模块根包内建实现的契约驱动、仓库形态检查、无主导源行为套件全部可黑盒导入模块根包,迁入即满足。

**明确不迁的例外及其 Go 理由**:白盒(WB 20,引未导出生产符号);迁移套件(M 5,`go:embed` 相对路径不可用 `..`,迁出即断;其中 dbkit/audit 一套钉迁移集 round-trip 与列宽常量,同属此类);`otlp_not_registered`(SHAPE,与任何导入 `exporter/otlp` 的文件同目录即毁掉被测形态);集成标签包内切片(OOS);契约支撑包自验套件(SELF 5,映射支撑包单一源 assert_conforms.go/assert_fails_closed.go,且 teeth 白盒;属 2a 式并入既有 `<源>_test.go` 的合并队列,不是迁移对象);支撑包双入口合测(T);拆分片(S 4,保留 `<源>` 前缀的既有测试族成员);夹具(FIXT 2,消费方留包即随留);基准与 example 按命名例外。

## 迁移清单(执行批次蓝图)

### U:迁入 `go/<module>/unittest/`(13 文件)

| 文件 | 现包 | 迁移编辑要求 |
|---|---|---|
| go/pkgcore/{eventbus,kv,mailer,objectstore}_conformance_test.go | pkgcore_test | package 子句改 `unittest`,引用已限定,原样保留;文件头"外部测试包机械例外"叙述在搬迁后仍真(支撑包回导成环),需按新目录改写 |
| go/pkgcore/standalone_build_test.go | pkgcore | package→`unittest`;路径锚定编辑:`filepath.Dir(thisFile)` 改为自 `runtime.Caller` 上溯至含 go.mod 的模块根 |
| go/pkgcore/go_work_use_block_test.go | pkgcore | 同上;仓库根锚点 `..`,`..` 改自模块根上溯至 go.work 所在根 |
| go/jobs/queue_conformance_test.go、go/jobs/fail_closed_cancellation_state_test.go | jobs_test | package→`unittest`,引用已限定 |
| go/jobs/claim_window_test.go | jobs | 加 `import jobs` 并全量限定导出符号;其包内辅助 newTestQueue/startQueue/pollJob/waitTerminal(standalone_queue_test.go,留包)不可跨包,按共享辅助规则迁最小集入 `go/jobs/internal/testutil`(已有该包,加导出构造器) |
| go/authn/standalone_build_test.go、go/tenancy/standalone_build_test.go | authn/tenancy | package→`unittest`;路径锚定编辑(上溯 go.mod) |
| go/ratelimit/no_cjk_characters_test.go | ratelimit | package→`unittest`;路径锚定编辑:`WalkDir(".")` 改为自模块根上溯后 `WalkDir(moduleRoot)` |
| go/observability/exporter/otlp/invalid_utf8_export_test.go | otlp_test | 迁入 go/observability/unittest/;package→`unittest`(引 otlp 空导入;注意与 otlp_not_registered 的 SHAPE 互斥——后者必须留在 prometheus 目录,二者永不同目录) |
### G:cmd/server 集群(8 文件,2b 收尾"待后续批次重新裁定"的兑现;非本规则迁移对象而是 target 旁置/应用级目录路由)

| 文件 | 路由 | 编辑要求 |
|---|---|---|
| cmd/server/demo_subject_test.go、cmd/server/demo_notification_test.go | internal/app/(package app_test) | package 子句 main→app_test;引用已 `app.` 限定、保留;文件内自足辅助随迁 |
| clinic_name_test.go、cmd/server/demo_user_header_kill_switch_test.go | 拆分:纯逻辑腿→internal/app(app_test);经 buildTestServer 的 HTTP 腿→flowtests | 拆分边界按各 Test 是否驱动组合服务器定,执行批次定并回填本表 |
| cmd/server/demo_admin_test.go、cmd/server/demo_users_test.go、self_service_test.go | flowtests(旅程/装配类) | package→flowtests;所需 demo 身份/种子/自服务辅助 flowtests/test_support_test.go 镜像已含,按编译补缺 |
| test_support_test.go | 随消费方清退 | 消费方全部离场后 cmd/server 无使用者(2b 镜像对侧 flowtests/server_test.go 与 flowtests/test_support_test.go 已含等值辅助);删除并在对侧头注同步镜像关系 |
| (留) main_test.go | cmd/server | 有 target(main.go),不动 |

路由完成后 cmd/server 只剩 main.go + main_test.go,2b 验收线"除登记外不再出现装配流测试"闭合。

### 留包例外登记(45 文件)

WB 20(白盒,Go 强制):kernel_shutdown、factory_vars、hub_http、address_index_column、subscribe、sweep_window、webhook_redelivery、enqueue_window、key_ref_widening、signin_channel_feature_gates、impersonation_service_start_dispatch、jobs 7 件(database_fault_paths/candidate_window_fairness/metric_ordering/metric_registration_failure/retryable_start/single_writer/writer_gate_liveness)、asynq 2 件。S 4(拆分片):standalone_queue_cancel、retention_config、tenant_scope_tenantmodel、smilesim service_settlement。M 5(迁移套件):org/metering migrations 各 2、audit 1。BENCH 6。SHAPE 1。OOS 1。SELF 5。T 1。FIXT 2。逐文件理由见总表"分档"列;执行批次不改动这些文件(仅登记)。

## 验证记录

- **4 个可迁文件抽查(阅读确认只经导出面/公共 API)**:pkgcore/standalone_build_test.go(全文:零包引用,仅 stdlib+exec,路径锚定 filepath.Dir(thisFile));ratelimit/no_cjk_characters_test.go(全文:零包引用,`WalkDir(".")` 走模块目录);observability/exporter/otlp/invalid_utf8_export_test.go(外部包 otlp_test,obs/exporter 引用全限定);jobs/queue_conformance_test.go(外部包 jobs_test,imports dbtest/jobs/queuetest,引用全限定)。mailer/kv/eventbus/objectstore 四驱动与 queue_conformance 同形(文件头同注,引用全限定)。
- **白盒判定确认(未导出引用实证)**:factory_vars_test.go L41-45 赋值 otlpFactory/metricsReaderFactory(init.go 包变量);kernel_shutdown_test.go L353 `seamCloserOf(...)`、L227 preset 键(map 键为包内常量);database_fault_paths_test.go L175/L330/L674 直呼 acquireWriterRegistration/insertRecord/completeDeadLetter 等 store.go 未导出函数;jobs metric_ordering L99 markCancelled、writer_gate L95 findByID(L544 queueWritersTable);sharing sweep_window L293 `expirySweepHandler{svc: svc}` 结构体字面量;admin impersonation L439 notificationGroupSecurity(module.go:124 生产常量);authn signin L90 hasCode(service.go:1328 生产函数);pki enqueue_window L385 crlRegenerateIdempotencyKey 直呼、L145 taskTypeExpiryScan;webhook_redelivery L41/L85 webhookDeliveryJobPayload/redeliveryIdempotencyKey;smilesim service_settlement L711 orphanRefundJobIDPrefix;hub_http L68 apiPath;notification subscribe L60/L123 直读 module.hub(module.go:85 未导出字段,无访问器)。jobs/claim_window_test.go 判为黑盒(词法探针零生产未导出命中 + 通读确认)。
- **探针局限**:词法探针只捕非限定未导出标识符;选择器形态(obj.未导出方法/字段)与包内测试辅助网靠通读与抽查补;编译是最终权威——执行批次以迁移后 `go test ./...`(模块内)与 `-race` 全绿为门,任何清单误判在编译门暴露即按 WB 登记回填本表。
- 本批次无文件移动,未跑迁移后测试;被改三处标准文件为纯文档,`tools/scan_cjk.py` 规则对 `docs/internal/` 豁免。

## 执行批次注意

1. 旧"conformance 驱动留根目录"样板(2a/2b 记录与多文件头注)被本规则取代为"迁模块 unittest/";相关文件头注中引旧规则文字处随迁改写(注释只述当前状态)。
2. `unittest/` 目录全部 `_test.go`,无 build tag;模块 CI `go test ./...` 自动覆盖,工作流不改。
3. 有 target 的 `cmd/server` 集群路由属 2b 收尾,与 U 迁移分开提交更清晰;拆分边界回填本表。
4. 白盒留包文件不改动;SELF 5 件列入 2a 式并入队列(并入既有 assert_conforms_test.go/assert_fails_closed_test.go),不属本迁移。

## 批次 3 执行记录:U 清单 13 文件全部落地

执行批次按上文 U 迁移清单逐文件迁移,每模块一个提交、按模块验证(普通 `go test ./...`、`go vet ./...`、`golangci-lint run ./...` 0 issues、gofmt 干净、覆盖率门 `tools/check_coverage_baseline.py --check` 通过)。`unittest/` 目录全部 `_test.go`、无 build tag,模块 CI 的 `go test ./...` 自动覆盖,工作流零改动(逐模块跑通确认,`go build ./...`/`go vet ./...` 对纯测试目录同样零干扰)。

### 逐文件落地

| 文件(迁移前 → 后) | 包 | 资格化/编辑 |
|---|---|---|
| go/pkgcore/{eventbus,kv,mailer,objectstore}_conformance_test.go → go/pkgcore/unittest/ | pkgcore_test → unittest | 引用本已全限定,原样保留;四文件头"外部测试包机械例外"叙述改写为 unittest 家叙述(成环理由保留为"为何必须黑盒"的一节);mailer 驱动的 smtpMailerFor 连同文件迁入,头注同步 |
| go/pkgcore/standalone_build_test.go → go/pkgcore/unittest/ | pkgcore → unittest | 模块根锚定:自 runtime.Caller 上溯至含 go.mod 的模块根(原来就是 filepath.Dir(thisFile));模块根新增包级 helper moduleRootOf;go_work_use_block_test.go 的仓库根锚点 `..`/`..` 改为自模块根上溯至首个 go.work,无则 skip(语义与原来"固定两层上溯找不到即 skip"在 monorepo 检出与独立消费两种环境下等价,walk 上溯在共享 /tmp 之类环境有杂散 go.work 风险,已按模块根为起点、遇到即停,本仓库检出两跳即达) |
| go/jobs/{queue_conformance,fail_closed_cancellation_state}_test.go → go/jobs/unittest/ | jobs_test → unittest | 引用已全限定,原样保留;头注改写 |
| go/jobs/claim_window_test.go → go/jobs/unittest/ | jobs → unittest | 判为黑盒成立(自身只触导出面);加 `import jobs`,导出符号全量限定(NewHandlerFunc/Task/Job/JobID/Result/ProgressFn/StatusRunning/StatusSucceeded);四辅助换用本目录镜像(见下) |
| go/authn/standalone_build_test.go → go/authn/unittest/ | authn → unittest | 同上溯锚定(moduleRootOf) |
| go/tenancy/standalone_build_test.go → go/tenancy/unittest/ | tenancy → unittest | 同上溯锚定 |
| go/ratelimit/no_cjk_characters_test.go → go/ratelimit/unittest/ | ratelimit → unittest | `WalkDir(".")` 改为自模块根 `WalkDir(moduleRoot)`(unittest 目录下 "." 只扫到自身);上溯 helper 入文件 |
| go/observability/exporter/otlp/invalid_utf8_export_test.go → go/observability/unittest/ | otlp_test → unittest | 目的地按蓝图(模块 unittest/,非 exporter 目录);obs/otlp 引用全限定原样;与 exporter/prometheus 的 otlp_not_registered(SHAPE)互斥成立——两者分属不同测试二进制,SHAPE 钉的"本二进制不导入 exporter/otlp"不受影响;文件头注明其旧目录为何不能留 |

每个测试函数与断言行为逐字节不变;留包套件与白盒登记文件零改动。

### claim_window 的辅助迁移(蓝图偏差记录)

蓝图设想把 newTestQueue/startQueue/pollJob/waitTerminal 迁入 `go/jobs/internal/testutil`。编译门否定了该去处:testutil(metrics 辅助的家)被留包的 package jobs 包内测试(metric_registration_failure_test.go 等)导入,而 testutil 一旦导入 jobs 根即成环("import cycle not allowed in test")。既不能改 production 面(ensureJobsSchema 不可导出),四辅助又不能原样搬(其 newTestQueue 触达未导出 ensureJobsSchema)。落地形态:unittest 目录自持镜像 `go/jobs/unittest/test_support_test.go`(2b 跨包镜像先例的同一模式),头部注明镜像关系、同步义务与一处刻意差异——包内版在构造期 eager 建 schema,镜像版延后到 Start(claim_window 流程恒先 Start 后 Enqueue,两版行为一致);包内原辅助与其 8 个留包消费方不动。

### 验证记录

- 逐模块普通单元运行全绿,新 `unittest/` 包被 `go test ./...` 覆盖(逐模块实测:pkgcore 4.98s、jobs 3.03s、authn 1.25s、tenancy 1.64s、ratelimit 0.24s、observability 1.78s);go vet、golangci-lint 0 issues、gofmt 干净。
- go_work_use_block 测试在迁移后仍实跑并命中本仓库 go.work(monorepo 检出内两跳找到);各 standalone_build 测试以模块根为工作目录 GOWORK=off 实跑 `go build ./...`+`go vet ./...`。
- 覆盖率门:五个模块通过(未及基线);`go/jobs` 越界一次(83.8415→83.4604,差 0.38pt > 0.15 容差),对照测量证实为迁移所致——同一断言从 unittest 测试二进制执行后不再归属 jobs 包自身 profile,测试并未减少,已按工具机制 `--update` 重录基线并在地提交信息写明理由(仍高于 80% 地板 3.5pt)。pkgcore 实测 81.50 高于其记录基线,无需重录。
- `tools/scan_cjk.py` 全树干净;`go build github.com/vislake/speed/go/...`(workspace 上下文)通过。
- 散文引用重定向:go/pkgcore/AGENTS.md 单元层段与 internal/testutil 段、go/jobs/AGENTS.md 单元层段、tools/README.md 的 CJK 扫描器引用随迁改写;SKILL §13 蓝图中已述目标布局,无需改;docs/internal/18/24 中相关行为是历史记录(过渡期手段与已闭 deferral 行),不改。

### 提交

按模块八笔提交(正文只述结论性 why,不引批次/轮次代号),全部 fast-forward 单线:
`test(pkgcore)`(六文件+AGENTS)、`test(jobs)`(三文件+test_support 镜像+AGENTS+基线重录)、`test(authn)`、`test(tenancy)`、`test(ratelimit)`(+tools/README)、`test(observability)`,后随两笔包内注释重定向(`test(pkgcore)`/`test(jobs)`:smtp_mailer_test.go、fake_smtp_server.go、standalone_queue_test.go、queuetest/assert_conforms_test.go 中指向迁移文件旧归属/旧包名的注释改为现状)。

