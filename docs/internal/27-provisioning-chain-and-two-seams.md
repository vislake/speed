# 27 注册触发供给链与两个平台件:归属裁定、链契约与 reference-app 迁移

> 本文覆盖宿主面审计清单中的两项:**第 17 项**——注册触发的跨模块供给链(reference-app 的 `self_service.go` 链);**第 20 项**——两个小平台件:notification 的静态地址解析器与 billing 的订阅 `EnsureActive`。本文只产出设计,不含代码改动。全部锚点在写作时对当前树逐处复核,引用以符号名为主、`file:line` 为辅;行号随实现轮失效,复核以实现时的符号为准。

## 1 现状核对

### 1.1 链的起点与钩挂点

注册侧的起点是 authn 自己的 HTTP 面:`POST /api/v1/authn/register`(go/authn/api/openapi.yaml 的 `authn_register`)→ `Service.Register`(go/authn/service.go:499)→ `Service.publishTenantless`(:1291)发布 `EventUserCreated`(事件名常量 `authn.user.created`,go/authn/events.go:24;载荷 `UserCreatedPayload` 同文件 :135)。载荷只带 `UserID` 与两个 `Has*` 布尔,不含个人数据,也**不含租户**——`Register` 的注释写明:携带租户的事件会把账号坐进调用者所在租户,并跳过宿主的无租户自服务供给。

链的钩挂点是**宿主在总线上的一条订阅**,不是 authn 里的回调:`wireSelfService`(examples/reference-app/internal/app/self_service.go:792)把 `SelfServiceProvisioner.onUserCreated`(:292)订阅到 `authn.EventUserCreated`(:804),并在 app 自己的 standalone 队列上注册重试 handler。`wireSelfService` 在 demo 种子之后才被调用(server.go:3059),这个先后顺序就是"demo 账号不触发供给、上线后的每次注册都触发"的判别子。

### 1.2 逐跳核对

链本体在 `SelfServiceProvisioner.provision`(:514),各跳与产物:

| # | 跳 | 产物 | 走哪个模块 API | 重入时的收敛机制 |
|---|---|---|---|---|
| 0 | 租户派生 | `tenant-<userID>` | 宿主 `ClinicTenantOf`(:722) | **非铸造**:同一账号恒解出同一租户,是多副本/重投递的收敛锚 |
| 1 | org 根 | 组织树根节点 | 宿主 `ensureClinicRoot`(:687,镜像 org 的 `ensureRoot`)+ `TreeService.CreateRoot` | 已有根即重读;单一根唯一索引 `uq_org_nodes_single_root` 吸收创建竞态 |
| 2 | 座位 | membership 行 | `MemberService.Add`,容忍 `ErrMembershipExists` | 已有座位原样保留,永不改绑 |
| 3 | 内置角色 | rbac 角色行 | `rbac.Service.EnsureBuiltinRoles`(go/rbac/builtin.go:110) | 对账而非重建 |
| 4 | owner 授权 | 角色绑定行 | `rbac.Service.AssignRole(BuiltinRoleOwner)`(go/rbac/assign.go:159),经 `pkgcore.WithActor` 归因 | 绑定已存在即 no-op |
| 5 | 订阅 | demo Plan 上的 Active 订阅 | `DemoEntitlementPlan`(internal/app/demo/demo_entitlements.go:80,resolve-else-create)+ `EnsureDemoSubscription`(同文件:145) | 已有 Active 即原样返回,绝不替换 |
| 6 | 信用点 | 起始余额 | `GrantDemoCredits`(internal/app/demo/demo_credits.go,余额全零守卫 + `CreditService.Grant`) | 余额(Available 与 Reserved)非全零即跳过(见 §3.4 的边界) |
| 7 | 失败恢复 | 重试作业行 | `scheduleProvisionRetry`(:471)→ `jobs.StandaloneQueue.Enqueue`(:517) | 作业类型 `self_service.provision_clinic`(:333),`WithMaxRetries(10)`(:347) |

每一跳都在**诊所租户自己的上下文**里执行(`pkgcore.WithTenant`),读写全部是模块的公开服务调用,没有跨租户读写,也没有对另一模块结构体的导入。

### 1.3 失败面现状

`onUserCreated` 永远返回 nil(:292 起):in-process 总线上 handler 的错误会回灌进 authn 自己的 `Publish`,而注册已经(或即将)答案是 201,所以失败只能记 Error 再自行恢复。恢复链是:

- 同步尝试失败 → `scheduleProvisionRetry` 入队一条 `SelfServiceProvisionTask`(payload 只带 user id;job 的 `TenantID` 是诊所,worker 由 jobs 契约重建租户上下文,go/jobs/worker.go 的 `jobContext`);
- handler `SelfServiceProvisionJobHandler.Handle`(:408)重跑同一个 `provision`;队列按 `DefaultBackoffBase = 1s`、`DefaultBackoffMax = 5m`(go/jobs/queue_standalone.go:73/:77)退避重试;
- 预算耗尽即死信,`OnFailure`(:446,jobs.FailureHook,go/jobs/handler.go:134)补一条宿主自己的终局 Error,点名账号。

这条"事件只发一次、失败靠队列自重试"的设计,org 模块自己的文档已经认定并写死:org 的订阅者(go/org/events.go:300-304)明说"transient database error ... is logged and never retried (nothing re-fires the event), so a host that needs its new users' workspaces guaranteed converges them through a queue-backed retry of its own"。宿主链正是那个 "queue-backed retry of its own"。

### 1.4 org 的两路分工与"自建租户是宿主显式调用"的既有裁定

org 的订阅者 `handleUserCreated`(go/org/events.go:305)只处理**带租户**的事件(给新用户在该租户里的 workspace 与座位);对无租户的自助注册事件明确跳过,注释把"租户诞生路径"点名为宿主显式调用(go/org/events.go:288,措辞 `CreateTenantRoot`——该符号在树中不存在,实际落点见 §1.5)。reference-app 的订阅恰好是同一条分工的另一半:只处理**无租户**事件,于是两条自动路径永不重复供给同一账号(两处注释互相点名)。

### 1.5 并行轮刚收掉的两跳手工形状

`TreeService.EnsureRoot`(go/org/tree.go:214)与 `MemberService.EnsureRootSeat`(go/org/membership.go:482)已落地:前者是"找根否则建根、吸收创建竞态"的一跳一次调用,后者把"根 + 座位"合成一次幂等调用(已有根/已有座位原样返回,不重命名、不改绑)。demo 种子已改走它(`addDemoOrgMembership`,internal/app/demo/demo_users.go:209)。本链的 1、2 两跳仍停在手工形状(`ensureClinicRoot` + `Add` 容忍 `ErrMembershipExists`),是本设计 §6 的迁移对象。

顺带记录一处文档漂移:go/org/events.go:288 的注释把宿主的显式调用写成 `CreateTenantRoot`,树中无此符号;实现轮应把注释改为实际 API 名(`TreeService.EnsureRoot` / `MemberService.EnsureRootSeat`),与内容规范"注释只述当前状态"对齐。

### 1.6 与审计描述的差异说明

审计把第 17 项描述为"跨四个模块的回调形状"。当前树中的实际形状是**总线订阅 + 队列重试**,与 authn 之间没有任何回调 ABI;链的每一跳都是对模块公开服务的调用。本设计因此按真实形状回答"链归谁"的问题,并把"authn 注册回调"作为候选形态之一记录否决理由(§2.3),而不是把它当作现状。

## 2 第 17 项:链归谁

### 2.1 三个候选

| | 形态 | 一句话 |
|---|---|---|
| A | **平台能力** | 新增(或并入上位模块的)一个"租户供给"能力,注册后自动铺开整个租户;宿主只提供策略参数或回调 |
| B | **宿主装配 + 平台逐跳帮手** | 链留在宿主(现状形态),平台把每一跳做成首等幂等 API、把重试机制做成队列能力、把失败语义写成契约 |
| C | **模块事件链** | 各模块自己订阅 `authn.user.created`,各自做自己那一跳(org 已经是这个形状的半个先例) |

### 2.2 裁定:方案 B

**裁定:链留在宿主装配;平台侧的义务是"逐跳首等且幂等 + 重试机制 + 契约文本",不是"替宿主决定注册后发生什么"。** 理由:

1. **链的一半是产品策略,不是平台事实**。"注册即建租户"本身是产品决策(许多产品是邀请制或加入既有 workspace);建出来的租户叫什么、订哪个 Plan、送多少信用点,全是宿主的词汇。org 已经把这件事裁定过:无租户事件的租户诞生路径是"the explicit ... call a host makes when a tenant is born"(go/org/events.go:288)。平台若把整链收走,只能以"策略回调"的方式把同样的决策还给宿主——绕一圈回到宿主装配,还多冻结一套回调 ABI。
2. **平台该收的是每一跳的形状,而这正是最近几轮的方向**。`EnsureRoot`/`EnsureRootSeat` 落地的判词就是"把重推导的机制移进模块、宿主保留属于自己的东西"。本设计把同一标准应用到其余跳:billing 的 `EnsureActive`(§5)、notification 的 resolver(§4);rbac 两跳与 credits 已经够首等(`EnsureBuiltinRoles`/`AssignRole`/`CreditService.Grant`)。
3. **可行性与成本**:方案 A 的载体只能是新模块或既有的最上位模块(链要同时 import org/rbac/billing)。lockstep 下每加一个模块要付 `go.work` 条目、CI 矩阵行、`AGENTS.md`、changesets fixed-group 条目、版本 tag 五件套;纪律清单已裁定"模块只在需要独立发布节奏或可独立消费时成立",这里两条都不成立。塞进 admin 之类上位模块则把产品策略写进运营后台的域。
4. **失败语义的归属**:上述恢复机制(重试作业、预算、终局信号)是 jobs 队列的公开能力;把它包成"平台供给作业"反而会把业务补偿写进队列层——纪律清单明令禁止("Do not put business compensation in the queue layer")。

### 2.3 备选否决理由

- **A 平台能力(新模块/上位模块内建供给)**:除 §2.2 的成本与策略理由外,还有一条循环论证——要把"哪一跳做什么"参数化到平台,平台要么给出一个回调注册表(回到审计描述的"回调形状",而且把它冻成 lockstep 公共 API),要么替宿主写死 Plan/信用点/命名(平台凭空发明产品决策)。两者都不优于宿主装配。
- **C 模块事件链**:即 rbac 订阅注册事件、billing 订阅注册事件,各自完成自己那一跳。否决理由:(i) 每跳都要各自判断它不拥有的策略(建不建订阅?订哪个 Plan?);(ii) 没有协调者就不存在顺序与恢复——org 的注释已经点破"事件不重发,失败无人重试",每个订阅方要各自再拉一套队列重试,把同一机制复制 N 份;(iii) 分步之间的可见状态由总线投递顺序决定,部分失败没有单一的责任人与终局信号;(iv) 租户 id 是"派生、非铸造"的收敛锚,而这是宿主的策略,模块无从推导;(v) 动作发生在远处——一次未付费的注册静默创建 billing 行,排障要跨模块猜。org 现有订阅保持原样是另一回事:它反应的是"既有租户内的事实"(带租户事件),不是"租户诞生"政策,边界正在这里。
- **D authn 注册回调(`WithRegistrationHook` 一类)**:即把链做成 authn 的一个回调选项。否决理由:(i) 它会把错误/失败语义压回 authn 的请求路径——org 的文档已经论证过为什么把错误回灌进 `Publish` 是有害的(会让 org 的理解失败表现为 authn 的创建失败),回调 ABI 是同一问题的冻结版;(ii) 事件已经携带同一事实,总线(In-process 或 Redis)是同一套机制,回调只是平行机制;(iii) 回调把供给钉死在"处理注册请求的那一个副本"上,而事件订阅在多副本下的收敛行为(派生租户 + 幂等跳)已经设计成可重复。附带说明:authn 目前**没有**任何 hook/callback 机制(树中不存在此类选项),此候选如要成立是新机制。
- **E 进共享宿主内核**:宿主中性内核(存活探针、预认证允许列表、挂载标签、生命周期、固定中间件链)现居平台模块 `go/app`,由 `tools/check_host_composition.py` 钉住"单一来源"(两个宿主共同 import,任何一方不得自行重声明/重长内核语句)。供给链是策略 + 模块胶水,把 org/rbac/billing 的 import 拉进这个宿主中性模块,既违 [14 示例应用](14-reference-app.md)"只属于一个宿主的行为不进内核"的边界,也让内核失去中性。
- **F 通用"幂等步骤序列 + 重试"原语(进 pkgcore/jobs)**:把 provision 抽象成"一串可重入步骤 + 队列重试"的通用机制。否决:这是把业务补偿放进队列层(纪律明禁);且宿主现有形状(一个 job 类型 + 一个 handler + 一个失败钩子)已经足够短,抽象只增加间接层。

## 3 链契约:顺序与失败恢复

### 3.1 为什么是前向收敛而不是回滚

`provision` 是不可回滚的,而且**不应该**回滚:

- 注册在 authn 侧已经提交并且已经答案是 201;删账号去"补偿"会和那句答案以及身份库自身的记录冲突(authn 的注册语义是"账号已存在")。
- 跨模块事务不存在:各模块的写入各自提交,modular monolith 没有跨模块事务,唯一的事务性组合(业务写入 + outbox)是 billing-grade metering 的专属机制。
- 每一步都是**幂等**的,所以"再跑一遍"就是天然补偿;队列重试是这一补偿的执行器。终局失败(死信)则交给人:死信行 + `OnFailure` 的 `user_id` 指出哪个账号需要人工供给。

所以链的正确性由三个属性支撑:**(a) 确定性租户 id(派生,非铸造);(b) 逐跳幂等;(c) 持久重试 + 终局点名**。这也解释了为什么 demo 判别子(订阅安装在种子之后)与"事件只发一次"都必须保持:链的恢复能力完全来自宿主自己的重试,而不是总线的重投递。

### 3.2 逐跳幂等契约

任何宿主照此配方复制这条链时,每一跳必须满足:

1. **对外部事实的派生标识**:租户 id 从注册事实派生(同账号恒同租户),不铸造、不依赖前置账本;
2. **ensure 形 API**:已存在即原样返回,吸收并发创建竞态(`EnsureRoot`/`EnsureRootSeat`/`EnsureActive` 是模板);"重入"包括"上一次在中间跳死掉后的重跑",所以任何跳都不得假设前面的跳是本次调用建出来的;
3. **租户上下文显式重建**:每跳在自己的租户上下文内执行(worker 不继承租户,jobs 契约由 job 的 `TenantID` 重建);
4. **依赖序**:产物有依赖的跳按序执行(角色行先于指名它们的授权);彼此独立的跳沿用与别的路径相同的顺序(订阅先于信用点,与 boot 种子同序),让两条路径的产物逐字一致;
5. **失败即错误返回**:单跳吞掉的失败不会被重试,禁止;
6. **不宣称原子性**:链不承诺"要么全有要么全无",只承诺"最终全有,或死信后被点名"。

### 3.3 hop 3 失败:一次具体推演

按 §1.2 的编号(下文"六跳"指 1-6 的供给跳,不含 0 派生与 7 恢复),设第 3 跳(内置角色)失败(典型成因:与注册自身写入的数据库争用、SQLite 文件忙):

- **已落地**:根 + 座位(1、2 跳)。账号**可以登录**——登录侧的成员关系解析读的就是这些行(sign_in_memberships.go 的跨租户读),同时 4、5、6 跳未落地:owner 没有授权(什么也做不了),没有订阅与余额(受门控的 AI 路由会被 `aigateway.entitlement_denied` 拒绝)。
- **同步路径**:注册照常答案是 201(条款:201 表示账号已存在,不代表诊所已铺完);Error 行点名账号与租户;`scheduleProvisionRetry` 入队。
- **恢复**:重试 handler 重跑从头到尾的六跳;前三跳是 no-op(根已存在、座位已存在且不改绑、角色对账),第 3 跳重新执行,之后 4-6 跳首次落地。队列退避 1s 起、10 次预算,典型瞬时成因在数秒内收敛。
- **终局**:预算耗尽即死信 + `OnFailure` 点名账号。此状态下的后果取决于失败的跳:1-2 跳失败 = 登录被拒(无成员的 401 与错密码不可区分,这是刻意的);3-4 跳失败 = 能登录但无权限;5-6 跳失败 = 能登录能操作,受权益门控的表面被拒。**这里有一处现存文案的过度承诺**:`OnFailure` 与 queue-nil 分支的文案一律说 "the account cannot sign in until it is provisioned by hand",在 5/6 跳失败时是夸大;实现轮应把终局文案改成不承诺具体后果的说法(失败跳名已在 wrapped error 里,`cause` 自带)。

顺序本身也是设计的一部分:**可见性由弱到强的顺序**。根与座位先落(登录即可用),再落权限、权益与余额——失败的窗口里,账号要么不可用(1-2 跳),要么可用但受限,不会出现"有权限但无租户"这类矛盾中间态。

### 3.4 已识别的残留弱点(如实记录,不在本设计内解决)

- **信用点跳的"全零余额"守卫是启发式**:`GrantDemoCredits` 以"Available 与 Reserved 全零"判断"未播种过"。重试若在诊所恰好把种子余额花到**正好全零**之后到达,会二次播种。internal/app/demo/demo_credits.go 与 internal/app/demo/demo_entitlements.go 已把这一条如实记录为有界幂等的接受项(与"canceled 按启动周期保持"同级)。根治需要一个"播种已发生"的持久标记,属产品/账本设计,不在本项内。
- **订阅跳的双 active 竞态**:本设计 §5 一并处理。
- **demo 判别子的多副本窗口**:`wireSelfService` 的注释已记录——另一个副本的重叠启动窗口里,它的 demo 种子事件可能到达本副本的订阅;收敛幂等、不改变 demo 账号的既有答案,是"排序判别子"在多副本下的残留成本,保持现状。
- **载荷探针的重复**:`selfServiceUserIDKeys`(:734)与 org 的 `userCreatedUserIDKeys`(go/org/events.go:419)是同一组拼写的两份声明,注释互相点名。这是刻意的协调点(跨模块事件载荷以字符串键为结构契约,探针走 `pkgcore.EventPayloadString`,各消费方自己声明可接受的拼写),不是缺陷,保持现状。

### 3.5 链的配方落点

方案 B 的代价是"每个交付项目都要自己再推导这条链"。缓解办法是把配方写成宿主可读的文档(§6 的最后一节给出落点建议与开放问题 Q3),而不是把它藏进某个模块实现。

## 4 第 20(a) 项:notification 静态地址解析器

### 4.1 现状

模块接口已经存在:`notification.UserAddressResolver`(go/notification/delivery.go:248,`UserAddresses` :219)是"用户投递在发送时解析收件地址"的结构化模块接口,**必装**:`Module.Register` 在缺失时以 `ErrUserAddressResolverRequired`(go/notification/errors.go:359)拒绝启动,`WithUserAddressResolver`(module.go:210)是注入点。契约三条:发送时读取(非入队时);缺地址不是错误(渠道跳过并记一条 skipped 记录);返回的地址必须是宿主**已验证**的地址——用户路径上模块不做验证,resolver 就是全部"可否发送"的门(delivery.go 的契约文本明说这只能写在契约里)。

**手写重写有三处**:

1. reference-app 的 `demoUserAddressResolver`(internal/app/demo/demo_notification.go:117)+ `DemoUserAddresses` 静态表(:100),经 `notification.WithUserAddressResolver(demoUserAddressResolver{})` 装配(server.go:1857);
2. 模块自己的 godoc `Example`:`exampleUserResolver`(go/notification/example_test.go:17)是一张 switch 表——同一形状的第三份;
3. 模块测试里的 stub(`stubUserResolver`,module_test.go)。测试替身不算产品重写,但它同样指向同一结论:这个形状被反复手写。

### 4.2 形状:`go/notification/staticaddr` 子包

```go
// New returns a notification.UserAddressResolver answering from a fixed
// per-user table. The table is copied at construction: later mutation of
// the caller's map never changes what the resolver answers, and the
// resolver is safe for concurrent use.
func New(addresses map[string]notification.UserAddresses) notification.UserAddressResolver
```

设计要点:

- **子包而非根包**:实现不与它实现的接口同包(纪律清单),方向单向 `staticaddr → notification`,notification 永不 import 它——与 `billing/gateway`、`authn/demoseed`、`pki/signer/vault` 同一打包模型;零第三方依赖,不摊任何成本给消费者。
- **API 姿态**:纯新增(lockstep 下不触任何既有导出面);`UserAddressResolver` 接口本身不改。
- **表即运营方的声明**:包文档第一句就要写清 `UserAddressResolver` 契约里的义务——这张表是运营方对自己用户"已验证地址"的声明,静态表只服务"地址由运营方静态持有"的部署形状(演示、单租户小安装、测试);地址会动态变化(用户改绑、验证流程)的宿主必须自己实现 resolver 读自己的地址库。
- **构造期拷贝**:投递在发送时读 resolver(worker 上下文),并发只读;拷贝后调用方再改 map 不影响解析,也消除 data race。
- **缺行 = 无地址**:返回空 `UserAddresses` 与 nil,与契约"缺地址不是错误"逐字一致。

### 4.3 备选否决理由

- **做成模块选项 `WithStaticUserAddresses(map)`**:把实现放进 `notification` 根包,与接口同包,违打包纪律;也让"模块根包不含实现"的先例(billing/pki/authn 各模块的实现全在子包)出现例外。pkgcore 的 `kv.memory` 内置不是反例:那是 **kv 模块自身**的内建组件(带 capability 位、可被组合选中),而 `UserAddressResolver` 是模块声明、宿主实现的模块接口。
- **放进 pkgcore**:pkgcore 在 notification 之下,方向反了(为一张 notification 域的表而让平台最底层的模块认识它);且 pkgcore 没有 notification 的 `UserAddresses` 类型。
- **只留宿主侧、文档化 5 行模式**:这正是审计点名的现状;三处重写说明"5 行"没有阻止重复,且必装模块接口的最小可用装配应当是一行。
- **命名带 `demo` 立场(仿 `demoseed`)**:静态地址表对单租户生产安装同样成立(地址由运营方持有),把它结构性锁进 demo 反而误伤;立场改由包文档承载(§4.2 第二条),`demoseed` 的实体是"注册演示账号"这个 demo-only 动作,两者不同。此判断列入开放问题 Q2 复核。
- **放进测试支持包(`queuetest` 模型)**:resolver 有生产用途(演示部署、小安装、骨架),不是测试专属。

### 4.4 迁移(reference-app 与模块自身)

- server.go:1857 的 `notification.WithUserAddressResolver(demoUserAddressResolver{})` 改为 `notification.WithUserAddressResolver(staticaddr.New(DemoUserAddresses))`;
- 删除 `demoUserAddressResolver`(类型、`Resolve`、编译期断言,共 internal/app/demo/demo_notification.go:105-125 一段)与其说明;`DemoUserAddresses`(:100)保留,它是 app 的数据(flowtests/smilesim_flow_test.go:968、integration_test 用它做断言),不再是 resolver 的实现;
- `example_test.go` 的 `exampleUserResolver`(:17-36)改用它自己所在模块的子包 `staticaddr`(example 是外部测试包 `notification_test`,可以 import;示例顺带演示最小装配);
- 模块文档:`go/notification/AGENTS.md` 补子包条目(shipped surface 与文件表),子包自带 `example_test.go`(可编译示例)与单元测试(解析、缺行、拷贝语义、`-race` 并发读)。

## 5 第 20(b) 项:billing 订阅 `EnsureActive`

### 5.1 现状

宿主手工 ensure:`EnsureDemoSubscription`(internal/app/demo/demo_entitlements.go:145)做"读 `SubscriptionService.Active` → 无则 `Create(CreateInput{PlanID})`(go/billing/subscription.go:195)→ `Activate`(:257)"三步,两个调用方:boot 种子 `SeedDemoEntitlements`(:181)与供给链第 5 跳。`SubscriptionService.Active`(:242)只读 `status == "active"` 的行,并在文档里**假设每租户至多一条 active**;`go/billing/AGENTS.md` 的 Known limitations 明说这个假设**没有任何数据库层约束**(第 155 行:多订阅并存需要自己的 uniqueness 决定,现在两处都没有)。

### 5.2 形状:`SubscriptionService.EnsureActive`

```go
// EnsureActive returns the tenant's Active subscription; when the tenant
// has none it creates one against in.PlanID and activates it.
func (s *SubscriptionService) EnsureActive(ctx context.Context, in CreateInput) (*Subscription, error)
```

语义(逐条):

- **既有 Active(任意 Plan)原样返回**:与 `EnsureRoot` 的"既有根不被重命名/改绑"同一 ensure 惯用法——**`in.PlanID` 是建期参数,只在新订时被读**。租户已有一条别的 Plan 的 Active 时,ensure 不动它(供给/种子绝不踩运营者或别的流程建好的订阅);
- **无 Active → `Create` + `Activate`**:组合两个既有公开调用,不引入新的事务语义。Create 成功、Activate 前进程死的窗口留一条惰性 `created` 行:无害(`Active` 与权益判定都只看 active 行),重跑 ensure 会再建一条并激活;不引入"收养 created 行"的分支(见 §5.3);
- **非 active 行永不复活**:`created`/`past_due`/`canceled` 的下一步是渠道/业务的决定,不是 ensure 的;`canceled` 是终态这一条完全保持。宿主现有"本进程内 cancel 后不复活、下次启动种子重跑才重建"的语义逐字保留(重建 = 新订一条,而不是复活旧行);
- **状态变更事件**:`EventSubscriptionStatusChanged` 仍由既有 `transition` 路径按原样发布(`EnsureActive` 不新增事件);
- **入参校验**:`in.PlanID` 走与 `Create` 同一套 Plan 作用域校验(租户自定义 Plan 或平台 Plan,`ErrPlanNotFound` 同文案);
- **API 姿态**:纯新增(lockstep 下不破坏任何既有调用方);既有 `Create`/`Active`/`Activate` 的签名与语义不动。

### 5.3 备选否决理由

- **留在宿主做 app 级函数**:审计点名的现状。boot 种子与供给链两个消费方都收敛到它,但"ensure 一个 active 订阅"是订阅生命周期的模块语义(涉及 Active 的"至多一条"假设、Plan 校验、状态机合法性),不是产品策略;放进模块,lockstep 冻结的是正确的抽象层。
- **仓储层 upsert(按 (tenant, plan) 唯一)**:把 ensure 做成数据库层 upsert 要求新的唯一约束(tenant_id, plan_id)——这与"至多一条 active"是不同的不变量,且会顺手裁定"每 Plan 一条"这个没人要做出的决定。
- **ensure 时复活 created/past_due 行**:见 §5.2 第三条;且 `past_due → active` 是账务回收决定(欠费补缴),ensure 静默做它是越权。
- **复活 Canceled 行**:无此备选——现行语义就是"canceled 行保留、重建立新行",`EnsureActive` 不改变这一点。
- **只加数据库约束、不加 API**:约束不解决"两个调用方手写三步"的重复;两者互补(§5.4)。

### 5.4 至多一条 active:数据库级约束的选项与代价

**建议(列入开放问题 Q1):同轮把"每租户至多一条 active"上升为数据库不变量**,形态是部分唯一索引:

```sql
CREATE UNIQUE INDEX uq_billing_subscriptions_one_active
    ON billing_subscriptions (tenant_id) WHERE status = 'active';
```

- 两个方言都原生支持部分唯一索引(SQLite ≥ 3.8.0 与 PostgreSQL),语句逐字一致,能通过迁移对齐门(`tools/check_migration_parity.py` 的文本对齐),不需要 exceptions 条目;新增 0007 一对(双方言手写、同文本);v0.0.1 已作废(21 个模块 tag 已从远端与本地删除,代理缓存不可撤回)、零部署,无既有数据需要先体检。
- 有索引时 `EnsureActive` 的并发语义:两个并发 ensure 各建一条 created 行都合法;先激活者拿下索引,后者的 `Activate` 得到 duplicate-key 错——`EnsureActive` 捕获它、重读 Active、返回赢家的行(org `EnsureRoot` 吸收创建竞态的同一惯用法;`errors.Is(err, gorm.ErrDuplicatedKey)` 的先例在 go/org/tree.go 的 `CreateRoot`)。输者的 created 行留为惰性孤儿。
- 无索引时(若 Q1 被推迟):两条 active 并存的窗口与宿主手工 ensure 的现状完全相同——`EnsureActive` 的文档必须如实写明这一点,不能假装原子。
- **代价与它裁定的问题**:索引把"多订阅并存不支持"从 v0 的假设变成不变量。这正是 AGENTS.md 记录为未定的 "uniqueness decision";收紧方向与现状(`Active` 的假设、权益判定只认一条)一致,反向(真多订阅)需要的是另一套设计(Active 的签名、权益合并规则),不会被这条索引堵死任何已承诺的能力。既有测试中没有任何一个在同一租户下激活两条订阅(已核对 billing 各测试文件与 integration_test;subscription_test.go 的并发用例是同一条订阅上的 Cancel/MarkPastDue 竞态)。

### 5.5 迁移

- `EnsureDemoSubscription` 的体收缩为一次 `EnsureActive`(保留 app 级包装:它承载"demo Plan + 租户上下文 + 错误包装"的宿主策略文本),或按两个调用方点内联;两处调用点(种子、供给链)行为不变;
- 其文档里的 "bounded idempotence" 段落按新形状改写:幂等语义移到 `EnsureActive` 的 godoc,app 侧只留"本 app 何时调用它"的策略句(内容规范:注释述当前状态);
- `go/billing/AGENTS.md`:shipped surface 增补 `EnsureActive`;Known limitations 的"至多一条 active 无约束"条目按 Q1 的裁定改写;
- 测试:模块侧新增 `EnsureActive` 单测(无 active → 建并激活;已有 active → 原样返回、不换 Plan;canceled 后 → 建新行;Q1 落地时加并发双 ensure 的收敛用例与索引存在性 pin);
- flowtests 的既有 pin 全部保持:entitlements_flow_test.go 的"cancel 后同一请求被 `aigateway.entitlement_denied` 拒绝"这条路径含 `Subscriptions().Cancel` 与 Active 读取,迁移后语义逐字不变。

## 6 reference-app 合并迁移计划

三项落地后,链的宿主代码只剩它自己的策略:租户派生、命名、事件订阅、失败恢复。逐文件清单:

| 文件 | 改动 | 行为影响 |
|---|---|---|
| `examples/reference-app/internal/app/self_service.go` | 1、2 跳改 `orgModule.Members().EnsureRootSeat(tenantCtx, userID, name, "workspace")`;删 `ensureClinicRoot`;5 跳改 `EnsureActive` 路径(经 app 包装);终局文案去掉"can not sign in"的过度承诺;注释按现状改写 | 逐字保持(座位/根/订阅/信用点的产物相同;`created` 的 Debug 行改述为无条件 Debug 或删除,仅日志) |
| `internal/app/demo/demo_entitlements.go` | `EnsureDemoSubscription` 体改 `EnsureActive` 调用 | 逐字保持 |
| `internal/app/demo/demo_notification.go` | 删 `demoUserAddressResolver` 与其断言;保留 `DemoUserAddresses` | 逐字保持(同一张表、同一解析结果) |
| `internal/app/server.go` | `WithUserAddressResolver(staticaddr.New(DemoUserAddresses))` | 逐字保持 |
| `go/org/events.go` | 注释把不存在符号 `CreateTenantRoot` 改为 `EnsureRoot`/`EnsureRootSeat` | 无(runtime 零改动) |
| `go/billing/subscription.go` + 测试 + AGENTS | 新 `EnsureActive`(§5) | 纯新增 + 文档 |
| `go/billing/migrations/{sqlite,postgres}/0007_*.sql` | Q1 裁定后:部分唯一索引一对 | 纯新增 |
| `go/notification/staticaddr/**`(新) | 子包 + doc + Example + 单测 | 纯新增 |
| `go/notification/example_test.go`、`AGENTS.md` | Example 与文档改走 `staticaddr` | 无 |
| `.claude/skills/backend-coding-standards/SKILL.md` | 供给链配方短节(见 Q3):事件只发一次 → 宿主队列重试 → 逐跳 ensure 契约(§3.2 六条) | 无 |

**行为保持的验收 pin**:`flowtests/self_service_clinic_name_test.go`、`self_service_no_ledger_test.go`、`self_service_entitlements_test.go`、`entitlements_flow_test.go`、`notification_flow_test.go`、`smilesim_flow_test.go` 全部**不改一行**通过;`APP_FAIL_SELF_SERVICE_PROVISION` 注入路径(`FailSelfServiceProvision`,bootstrap.go:420)的恢复旅程与 `web/e2e/self-service-signup.spec.ts` 保持通过;模块侧新增单测按 §4.4/§5.5。

**不迁移清单(有意保留在宿主)**:`ClinicTenantOf` 派生、`clinicRootNameFor`/命名的兜底、`DemoEntitlementPlan` 的 resolve-else-create、`GrantDemoCredits` 的金额与守卫、事件订阅的安装时机(demo 判别子)、重试作业类型/预算/`OnFailure`、`FailProvision` 注入点。

## 7 开放问题(需要裁定)

- **Q1**:**"每租户至多一条 active 订阅"是否上升为数据库不变量(0007 部分唯一索引)?** 它同时终结 `go/billing/AGENTS.md` Known limitations 记录的 "uniqueness decision" 未定项。设计建议:收紧,与 `EnsureActive` 同轮落地(理由与代价见 §5.4);若裁定推迟,`EnsureActive` 仍可先落,文档按"无约束"如实写明。
- **Q2**:`staticaddr` 的立场复核:静态表按"运营方声明 = 已验证"的通用件落地(§4.2,设计建议),还是按 demo-only 命名收窄?另外,saasctl 生成骨架目前完全不接 notification——要不要在此轮之后让骨架消费它(第二个消费面)。
- **Q3**:供给链配方的落点与消费面:写进 `.claude/skills/backend-coding-standards`(交付项目的 AI/人写代码时读它,设计建议)还是 [14 示例应用](14-reference-app.md);以及要不要把"注册即建租户"作为骨架的默认装配之一(生成骨架是"可自由编辑"的最小骨架,默认装配的边界需要裁定)。

## 8 工作量与轮次建议

三项彼此独立,可各自成轮、各自绿:

1. **轮一(billing)**:`EnsureActive` + 单测 + AGENTS(+Q1 若裁定则 0007 与并发用例);reference-app 两个调用点切过去。
2. **轮二(notification)**:`staticaddr` 子包 + 示例/单测 + AGENTS;reference-app 与模块 Example 切换。
3. **轮三(reference-app 链收尾)**:org `EnsureRootSeat` 采纳、`ensureClinicRoot` 删除、终局文案修正、注释对齐(org events.go 的 `CreateTenantRoot`);配方短节(Q3 落点裁定后)。

每轮独立小提交、ff-only;轮一与轮二无依赖,可并行。**并行约束:轮一与轮三都改 `self_service.go`(轮一切订阅跳的调用点,轮三改 1、2 跳与该链的注释、终局文案),两者不得并行——按"轮一 → 轮三"先后执行,或把两轮对 `self_service.go` 的编辑合并进同一轮。**
