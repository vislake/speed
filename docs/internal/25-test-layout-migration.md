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

| 族 | 符号 | 说明 |
|---|---|---|
| 装配与配置 | `buildServer`(25 文件直引,其余经基座)、`serverConfig` 及其字段(22)、`configFromEnv`、`mountModuleRoutes`、`openConfiguredAuthnChannels`、`demoHostTenants`、`orgFeatureGate`、`socialChannelFlagKey`、`signInMemberships`/`newSignInMemberships`、`healthzHandler`/`metricsHandler`、`defaultPort`/`defaultSQLitePath` | 流的公共根:39 个起动服务器的测试文件全部经 `buildTestServer` 族 → `buildServer`;`serverConfig` 是变异回调(`func(*serverConfig)`)与读回(cfg.SQLitePath、cfg.Memberships)的载体 |
| 测试密钥默认 | `devConfigKey`、`devOrgIndexKey`、`devNotificationIndexKey`、`devPKILocalKeyCipherKey`、`devBlindIndexKey`、`devPIICipherKey`(server_test.go 引用全组) | `testConfig` 直填 cfg 字段,绕过 `configFromEnv` |
| demo 身份常量 | `demoOwnerUserID`(23)、`demoReaderUserID`、`demoNotesCreatorUserID`、`demoSingleTenantID`、`demoOwnerEmail`、`demoReaderEmail`、`demoAcmeOnlyEmail`、`demoPlatformStaffEmail`、`demoSmileSimRecipientUserID`、`demoSeedAccounts`、`demoOrgUserHeader`、密码常量、`demoUserAddresses`、`demoRouteGuards` | 生产胶水(demo_subject.go/demo_users.go/demo_notification.go)自身也读,故无法整体移出包;黑盒流只能读导出版 |
| demo 解析器/守卫族 | `demoSubjectResolver`、`demoOrgSubjectResolver`、`demoNotesSubjectResolver`、`demoResolveSubject`、`demoSubjectResolverFor`、`demoPermissionFor`、`splitDemoPermission`、`guardModuleRoute`/`guardOrgRoute`/`guardAdminRoute`/`guardIntegrationRoute`/`guardPkiRoute`、`orgRouteGuardDeps`、`notesResource`、`mustResourceOf`、各域 `*PermissionFor` | 白盒直测的对象(demo_subject_test 等);流文件多经 HTTP 间接使用,直接引用集中在 kill-switch/守卫文件 |
| seed 族 | `seedDemoUsers`、`seedDemoGrants`、`seedDemoCredits`、`seedDemoEntitlements`、`seedDemoPlatformStaff`、`addDemoOrgMembership`、`registerDemoUser`、`grantDemoCredits`、`ensureDemoSubscription` | 部分被生产装配调用,部分仅供测试种子 |
| HTTP 路径/响应常量 | `authnAPIPath`、`adminRoutePath`、`adminAuditEventsPath`、`adminUsageSummaryPath`、`aiGatewayRoutePath`、`orgRoutePath`、`pkiRoutePath`、`billingRoutePath`、`clinicNamePath`、`consultSuggestPath`、`teamMembersPath`、`webhookBasePath`、`integrationWhoami*`、`casesMaxRequestBodyBytes`、`maxPhotoBytes`、`smileSimulationContentPath`、`casePhotoContentPath` 等约 20 个 | 与片段线路径相关的部分已被 `fragment_wire_paths_test.go` 收拢为夹具常量;其余散在各生产文件 |
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
| demo_subject_test.go | 留包 | 解析器/守卫族纯逻辑直测 |
| demo_admin_test.go | 留包 | demo_admin 胶水直测(经 seeded server 取 demo staff) |
| demo_notification_test.go | 留包 | payload 提取器纯函数直测 |
| demo_users_test.go | 留包 | seed/查重逻辑直测(registerDemoUser 直呼) |
| demo_user_header_kill_switch_test.go | (iii) 候选 | 解析器族直测 + HTTP 双路径混合;若整体外移需拆出白盒半 |
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
| go/pkgcore/eventbus_conformance_test.go | `NewMemoryEventBus` 满足 `eventbustest.AssertConforms`;内存总线"额外属性"(同步投递错误回传)由 `eventbus_test.go` 在包内另行钉住 | go/pkgcore/eventbustest | 是:`AssertConforms(t, caps, factory)`,签名公开 |
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
- `demo_subject_test.go`、`demo_notification_test.go`、`clinic_name_test.go`:纯单元钉(解析器/守卫族、payload 提取器、诊所名 handler 与错误辅助),各自无跨包辅助依赖;被测对象现居内部 app,后续批次可把它们移到被测代码旁(内部 app 的 `*_test.go`)。
- `demo_admin_test.go`、`demo_users_test.go`:(iii) 之外按侦察表留包的消费证明套件,仍经种子装配跑组合 HTTP;留包意味着 `cmd/server` 仍有少量装配流测试,与验收线"除 (iii) 登记外不再出现"的出入按本表登记。
- `self_service_test.go`、`demo_user_header_kill_switch_test.go`:(iii) 白盒例外按侦察表保留;复核确认其"直测未导出"前提已随 2b-i 消失(全部经导出面),现为纯逻辑单元钉与组合 HTTP 旅程的混合,留包理由按现状更新,待后续批次重新裁定。
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
- **留包文件的引用不动**:`main_test.go`、demo_* 与 clinic_name/self_service/kill-switch 套件仍居 `cmd/server`,指向它们的引用(如 demo_users_test.go 的行号钉)原样保留;两镜像 `test_support_test.go` 各自头部已述镜像关系。
- **error-codes.md 随源再生成**:索引文档内嵌错误常量处注释,其中两处仍述旧路径;同时该生成产物在装配代码迁入 internal/app(导出改名)后即已漂移,本次一并 `tools/gen_error_code_index.py` 再生成归位。

验证:全仓库对 47 个迁走文件名的 `cmd/server/<文件>` 形式引用零命中;裸文件名引用除 flowtests/cmd-server 目录内互引与本文档自身的迁移清单外全部带 `flowtests/` 前缀;reference-app `go build ./...` 通过。根 CLAUDE.md 普查句此前部分改写的宣告(随 2b 提交消息)按本记录修正:当时只重定向了少数文件,残余引用由本记录闭合。
