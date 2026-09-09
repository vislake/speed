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
