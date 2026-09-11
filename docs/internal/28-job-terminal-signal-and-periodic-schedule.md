# 28 jobs 终态信号与周期调度席位:机制裁定、失败面闭合与迁移计划

> 本文覆盖审计记录的两处结构性观察:**终态信号缺口**——队列今天只在死信路径提供 `OnFailure` 钩子,没有任何"某作业已终结(成功/死信/取消)"的平台信号,信用结算因此自建轮询与持久台账;**重复的周期形态**——storage、compliance、pki(两处)、billing、sharing、integration 七处各自手写同一套"窗口化幂等键 + `Enqueue*` 方法 + handler",宿主各自手写 ticker 循环。本文只产出设计,不含代码改动。全部锚点在写作时对当前树逐处复核,引用以符号名为主、`file:line` 为辅;行号随实现轮失效,复核以实现时的符号为准。

## 1 现状核对

### 1.1 队列契约与今天的终态面

- **便携契约**是 `jobs.Queue`(go/jobs/queue.go:21)的三个方法:`Enqueue`/`Get`/`Cancel`。两个部署模式实现同一份契约:`StandaloneQueue`(根包,SQLite 持久化,单写者门 `queue_writers` 保证同一库只有一个 live writer)与 `asynq.Queue`(`queue/asynq` 子包,Redis)。
- **状态机**是 `jobs.Status`(go/jobs/job.go:18):`pending`/`running`/`retrying` 非终态;终态三个——`succeeded`/`dead_letter`/`cancelled`,由 `Status.Terminal()`(job.go:56)判定,terminal 即"没有任何路径再迁移出它"。
- **三个终态写入点**都在 store.go:`completeSucceeded`(:811)、`completeDeadLetter`(:875)、`markCancelled`(:899)。前两者返回 **transition report**(这次写入是否真实完成了迁移),worker.go 的 `execute`(:405)与 `settleFailedAttempt`(:520)只在报告为真后才记日志、指标、跑 `OnFailure`;写入的 `WHERE status = 'running'` 守卫让并发 `Cancel` 赢(取消赢语义),被丢弃的尝试结果永不落库(`logDiscardedOutcome`,worker.go:629)。
- **今天一个作业的终结只能靠轮询 `Queue.Get` 观察到**。唯一的推送面是 `FailureHook.OnFailure`(go/jobs/handler.go:134):仅死信、仅进程内、挂在 Handler 上;两个实现相对死信持久化的时序不同(StandaloneQueue 严格在写入之后、asynq 在归档写之前,handler.go:95-133 逐条写明),且被明确裁定为"队列对失败补偿的全部涉入面——补偿属业务模块"。
- **崩溃面的现状答案**是 `resetInterruptedRecords`(store.go:908):Start 时把残留 `running` 行重置重跑——本模块不存在"至多一次执行",至少一次是既定语义,消费者幂等是每个跨进程消费方的既有义务。
- **队列目前没有总线**:`NewStandaloneQueue(db, opts...)` 与 `asynq.NewQueue(redisOpt, opts...)` 都不接受 `pkgcore.EventBus`;`jobs` 也不是 `pkgcore.Module`(根包无 `Register`),`reg.Jobs` 是外部向它声明的座席(`JobHandlerRegistrar`,go/pkgcore/registry.go:324),由 `jobs.Wire`(go/jobs/wire.go)在 Bootstrap 之后排空。
- **"列终态作业"的唯一读面**是两个实现各自的 `DeadLetterJobs(ctx)`(go/jobs/queue_standalone.go:577;asynq 同名),不在 `Queue` 接口上,且只覆盖死信一类。

### 1.2 观察一:信用结算的轮询与持久台账

`examples/reference-app/internal/smilesim` 是 `CreditService` 的强制第一消费者,其结算路径今天由三件东西拼成:

- `Simulate`(service.go:547)在调用 `aigateway.Gateway.GenerateImage` 之前先 `PreDeduct` 预留(`CreditsPerSimulation`),幂等键在请求期一次性铸造(`"smilesim:" + uuid.NewString()`,pre-enqueue 拿不到作业 id 的原因记在 `Simulate` 自己的注释里);预留行持久化在包私有的 `smilesim_credit_reservations` 表(`reservation_store.go`,命令式建表,与 `go/jobs` 的 `jobsTable` 同形)。
- **主结算腿是轮询驱动的**:`NotifyOnCompletion`(service.go:766)由应用的作业状态路由在每次读之后调用(`internal/app/smilesim.go:265`),读到终态才结算:`StatusSucceeded` 走 `Confirm`、`StatusDeadLetter`/`StatusCancelled` 走 `Refund`(`settleCredit`,service.go:886)。
- **防丢网是第二只 ticker**:`ReconcileOutstandingCredits`(reconcile.go:65)扫全部未结算预留,逐行 `Queue.Get` + `Status.Terminal()`(:113),`StartReconciler`(:149)以 `DefaultReconcileInterval = 5 * time.Minute`(:26)驱动它。reconcile.go:56-60 记录了为什么只能这样:这是跨模块补偿(go/ai-gateway 的 handler 不能调 go/billing,两模块同层),而队列对跨模块情形"没有表达补偿的机制"——于是消费者自建了持久台账 + 轮询 + 清扫三件套。

同文件还记着一条结构性事实:队列今天提供的钩子(`OnFailure`)挂在**跑该作业的模块自己的** handler 上,而这里需要的观察者(结算)是另一个模块——这是"信号不在"而不是"钩子不好"的直接后果。

### 1.3 观察二:重复的周期形态

七处实现了同一套形状,互相在注释里点名"mirroring go/storage's ...":

| # | 模块 | 任务类型 | 排产方法 | 窗口 | 参考应用是否调度 |
|---|---|---|---|---|---|
| 1 | storage | `storage.expiry_sweep` | `LifecycleService.EnqueueExpirySweep`(cleanup.go:435) | `expirySweepWindowSize` = 1h(:104) | 是 |
| 2 | compliance | `compliance.retention_sweep` | `RetentionService.EnqueueRetentionSweep`(retention.go:441) | `retentionSweepWindowSize` = 1h(:71) | 是 |
| 3 | pki | `pki.expiry_scan` | `Service.EnqueueExpiryScan`(job.go:131) | `DefaultExpiryScanWindow` = 1h(:45) | 是 |
| 4 | pki | `pki.crl_regenerate` | `CAService.EnqueueCRLRegenerate`(crl.go:290) | `DefaultCRLRegenerateWindow` = 1h(:229) | 否(刻意,见下) |
| 5 | billing | `billing.poll_pending_payments` | `PollingService.EnqueuePoll`(job.go:245) | `pollIdempotencyWindowSize` = 15m(:55) | 否(全仓无调用方) |
| 6 | sharing | `sharing.expiry_sweep` | `Module.EnqueueExpirySweep`(module.go:244) | 1h | 否(全仓无调用方) |
| 7 | integration | `integration.apikey.expiry_sweep` | `Service.EnqueueAPIKeyExpirySweep`(apikey_sweep.go:98) | `apiKeyExpirySweepWindowSize` = 1h(:56) | 否(全仓无调用方) |

每处的解剖是同一个四件套:窗口起点函数(`now.Truncate(window)`,绝对时钟、不做时区日历切)、确定性幂等键派生(`<namespace>:<tenant?>:<RFC3339>`——storage 是 `storage.sweep:`、compliance 是 `compliance.retention_sweep:`、billing 是 `billing.poll:`、pki 两条不带租户段)、一个 host-facing 的 `Enqueue*` 方法、一个 `reg.Jobs` 上的 handler。窗口键承担三件事:**同窗口的重复入队收敛为一个作业**、**让周期成立**(StandaloneQueue 的幂等键一经解析永久持有,无窗口键就退化为"每库一次")、**死信只毒化自己的窗口**(下一窗口是新键)。

主机侧是两个各写各的 ticker:`examples/reference-app/internal/app/periodic_scheduler.go` 的 `startPeriodicTaskScheduler`(1 分钟 tick,`runPeriodicTasks` 驱动 1/2/3 三处),加 `smilesim.StartReconciler` 自己那只。租户宇宙是宿主资产:`periodicTenantUniverse`(periodic_scheduler.go:166)把 `cfg.HostTenants` 与 go/admin D3 台账(`TenantService.ListAllIDs`)取并集,每 tick 现算。

缺口已被就地记录:

- `go/compliance/AGENTS.md` 的 Known limitations:"this module still ships no schedule of its own, so every other application that wires compliance must add its own schedule point ... or soft-deleted rows are retained until someone does"。
- `docs/internal/24-deferral-roadmap.md` 第 7 章普查行 4(retention sweep 无模块内置调度点):"模块内调度点需 **jobs 级调度器设计**;宿主 cadence 契约已记录"。
- periodic_scheduler.go:17 的现状陈述:"go/jobs deliberately ships no periodic facility of its own -- a task exists only once something enqueues it, and who decides when that happens is the host。"

### 1.4 与审计描述的差异说明

审计把周期形态概括为"四处"(storage 过期清扫、pki CRL 重生成、compliance 清扫、billing rollup)。树中的实际形状:

- **pki CRL 重生成**:实现存在(`EnqueueCRLRegenerate` + 窗口 + handler,`crl.go`),但参考应用**刻意不调度它**——periodic_scheduler.go:105-118 记录了理由(CRL 按需生成、按需读取,定期刷新没有等待它的读者;`pki.Module.Register` 仍声明 handler,只是永不收到任务)。
- **"billing rollup" 在全仓不存在这个符号**:`rollup` 在 `go/` 下零命中。billing 实际存在的周期任务是**支付通道轮询兜底**(`billing.poll_pending_payments`,`job.go` 顶部的文件注释解释了它存在的理由:回调可能早到或永远不到),而且它**同样没有调用方**。
- **重复形态是七处而不是四处**:sharing 与 integration 各有一处,注释自认"mirroring go/storage's"。
- **参考应用实际调度的只有三处**(1/2/3);4/5/6/7 有实现、有窗口键、有 handler,缺的正是"宿主 schedule 点"——compliance AGENTS.md 记录的那个缺口在四处同时成立。

本设计按**真实形状**回答机制问题,并把"四处"的概括更正为"七处共享同一四件套 + 参考应用的两只宿主 ticker"。

## 2 终态信号:机制裁定

### 2.1 三个候选

| | 形态 | 一句话 |
|---|---|---|
| A | **总线终态事件 + 行内 outbox** | 进入终态即欠一次 `jobs.job.terminal` 发布;StandaloneQueue 以作业行自身作持久发件箱,发布成功才盖章;asynq 在其终态钩点直发 |
| B | **队列侧回调注册表** | `Queue` 增加 `RegisterTerminalHook(hook)` 一类的注册面,worker 在每个终态迁移处直接回调 |
| C | **终态台账轮询** | 把 `DeadLetterJobs` 泛化为"按时间窗列全部终态作业"的读面,消费方扫台账 |

### 2.2 裁定:方案 A(总线终态事件,发布义务行内化)

**裁定:队列在两个部署模式的每个真实终态迁移处,向共享总线发布一个 `jobs.job.terminal` 事件;StandaloneQueue 把"欠一次发布"记在作业行自己身上(`terminal_published_at` 为空即待发),由 live writer 的发布遍补发;asynq 在其两处终态钩点直发,时序让步照抄 `FailureHook` 已记录的两模式差异。消费契约三句话:幂等、行是真相、要完整性的消费方保留自己的对账网。**

形态细节:

- **事件与载荷**。类型 `jobs.job.terminal`(命名遵循 `<module>.<entity>.<action>`,与 `billing.subscription.status_changed` 一样用**单类型 + 载荷内状态**,而不是三个类型:`Status` 字段就是三终态之一)。`Event.TenantID` 取作业行上的 `TenantID`——与 worker 重建 Handle 上下文的是同一个字段,后台发布遍没有环境租户可继承(租户陷阱在发布侧同样成立;平台哨兵任务携带的是 `_pki_platform_scan` 一类哨兵值,消费方不得当真租户解析)。载荷为 `JobTerminalEvent`:作业 id、任务类型、终态 `Status`、`Error`(成功时为空)、`Attempts`、`CompletedAt`。**载荷不携带 `Result` 体**(见开放问题 3)。
- **发布义务的产生点**。义务由三个真实终态写入产生:`completeSucceeded`/`completeDeadLetter`(各自照 `execute` 现在消费 transition report 的位置,只有报告为真的迁移才产生;被并发 `Cancel` 吸收的 no-op 不产生——那一次由 `Cancel` 自己那条义务覆盖)与 `markCancelled`(取消也是一个终态,且是结算侧必须看见的一个:`Refund`)。被丢弃的尝试结果不产生任何事件(它没有终态写入)。StandaloneQueue 的实际发布在发布遍(下条),asynq 的在终态钩点直发。
- **行内 outbox(StandaloneQueue)**。`jobs` 表新增可空列 `terminal_published_at`(经 `ensureJobsSchema` 的方言分支增量加列,先例是 `claimed_by` 列的自带分支)。语义只有一句:**状态为终态且 `terminal_published_at IS NULL` 的行,欠恰好一次发布**。发布遍随队列生命周期运行:Start 的 writer gate 证明本进程是 live writer 之后(与 dispatcher 同一道门,多副本竞态由构造即封闭),一个后台循环批量取待发行、逐行 `Publish`、**成功才盖章**;失败记日志、行保持待发、下一遍重试(排序键实现轮定:`markCancelled` 今天不写 `completed_at`,取消行需经 `updated_at` 或两列合并排序)。进程在没有发完时死去,正是崩溃窗口本身——下一次 Start 的发布遍接着发,与 `resetInterruptedRecords` 的恢复精神同构。`Close` 不做特殊冲刷:未发的行下次 Start 补发,不多一条路径。存量行在加列的同时回填(`terminal_published_at = 变更时刻`),历史的终态不补发(见 §2.3)。
- **asynq 腿**。在三个终态点直发:成功点在 `processTaskUncancelled` 的 "job succeeded" 记录点,死信点在 `handleErrorAttempt` 的 archive-bound 分支(该子包自己复刻的归档-重试边界,`queue/asynq/worker.go:228/:297`),取消在 `Cancel` 写定 cancellation marker 之后的路径。时序上发布先于 asynq 自己的归档/完成写——**与 `FailureHook.OnFailure` 已记录的同一条让步**(asynq 没有 post-archive 钩子)。后果是崩溃窗口里可能"重跑后再次到达终态",产生**重复事件而不是丢失事件**,重复由消费幂等吸收。
- **注入**。两个实现各加一个选项:`jobs.WithEventBus(bus)` 与 `asynq.WithEventBus(bus)`(纯增量)。按选项约定的既有规则办事:传 nil 以带码 panic 拒绝(不传选项 = 不发布,是合法组合——无总线的宿主不被迫接一条没人消费的缝);`Wire` 的签名不动(它排空的是 handler 座席,与总线注入是两件事)。
- **消费契约**。(i) 幂等:至少一次投递、分布式模式下总线实现可能按设计丢弃失败 handler(eventbus 各实现的文档),重复与个别丢失都在契约内;(ii) **行是真相**:事件是通知,回读 `Get` 是数据面;在 asynq 腿上前置发布次序意味着事件到达时行可能还没离开 `running`,消费方**不得**以回读为前提(照抄 `FailureHook` 的措辞纪律);(iii) 需要完整性的消费方**保留自己的对账网**,事件只把延迟从"下一次清扫"压到"迁移即达",不替代网。

### 2.3 失败面闭合

设计必须闭合的失败模式与各自的答案(前三条为题面点名的三个,后两条是既有语义的保护条款):

| 失败模式 | 闭合方式 |
|---|---|
| 终态写入与发布之间崩溃 | StandaloneQueue:行内 outbox——"进入终态 = 欠一次发布"是**行状态本身**的性质,发件与业务写入同一行、同一提交,不存在两处写不同步;发布成功才盖章,崩溃后下次 Start 补发。asynq:发布先于归档写,崩溃由 asynq 自己的租约重投递覆盖(重跑 → 再次终态 → 重复事件),两模式都不需要第二套存储 |
| 至少一次重投递 / 重复通知 | 消费契约的幂等条款;两个签发端各自再做一层:StandaloneQueue 的盖章只在 `Publish` 返回 nil 之后;smilesim 的 `Confirm`/`Refund` 本就是比较并交换幂等(CreditService 的既有契约),重复结算收敛到已有行 |
| 多副本竞态 | StandaloneQueue:发布遍只由 live writer 运行(writer gate 是 dispatcher 单写者的同一道门),发布遍与它不可能互相竞态。asynq:终态迁移发生在**唯一持有该任务**的副本上(任务归属即互斥);消费者侧的并发(事件与清扫同时到)由业务幂等收敛 |
| 取消赢(既有语义的保护) | 被丢弃的尝试结果不产生事件(无终态写入);`cancelled` 行恰好产生一类事件。发布遍读取的是行,不重演竞态 |
| 升级存量行 | `terminal_published_at` 加列时一次性回填,历史终态不补发——信号的契约从变更时刻起,不追溯 |

### 2.4 与 `OnFailure` 的边界

`OnFailure` 原样保留,两者不互相替代:

- `OnFailure`:死信专属、进程内、**挂在自己作业的 handler 上**——"我的作业最终失败了,我的模块要补一步"。
- `jobs.job.terminal`:三终态、经总线、**跨模块可观察**——"某个作业已终结,谁关心谁订阅"。

队列对两者都不解释业务:事件载荷只是一组事实,队列不读 `Result`、不认任务类型的语义。**该信号不是补偿**——它把"何时知道"变成平台能力,"知道之后做什么"仍然是业务模块自己的事(这正是纪律条款"compensation belongs to the business module"在信号侧的对偶)。

### 2.5 备选否决理由

- **B 队列侧回调注册表**:(i) 总线已经是跨模块事实的既有缝,"再加一条平行回调 ABI"正是 27 号文档否决注册回调时用过的理由(事件已经携带同一事实,回调只是平行机制);(ii) 回调是进程内的——分布式模式下"消费方在另一个副本"根本表达不了,而跨模块恰恰是记录在案的用例;(iii) 在 lockstep 下冻结一条新 ABI,收益却与订阅一条既有事件完全重叠;(iv) 回调在 worker 的终态路径上同步执行,慢回调拖住 worker,而总线实现已经把"投递到远处"这件事做完了。
- **C 台账轮询作为主机制**:没有信号就是现状;作为唯一机制,要求每个消费方自建节律、起点与水位——今天正是这样(§1.2),这正是要消掉的部分。台账保留为**持久真相**与消费方对账网的数据面,不作为信号。
- **D 把 `OnFailure` 泛化成 `CompletionHook`(Handler 接口上的成功+失败双钩子)**:(i) 它 per-handler、同模块内——记录在案的 ai-gateway→billing 情形根本表达不了;(ii) 把成功路径也塞进"失败补偿面",混淆了纪律条款划出的边界;(iii) 仍然错过"订阅方是任意模块"这一半。
- **E 队列自己重排周期任务**(把 recurrence 做进 `Enqueue`):并入 §3.5 的调度候选一并否决。

## 3 周期调度席位:机制裁定

### 3.1 缺口与候选

七处共享的四件套(§1.3)与本模块的既有剖解已经证明:缺的不是某个模块的代码,而是**队列侧的调度机制**——这正是 24 号文档普查行 4 记下的前置条件("模块内调度点需 jobs 级调度器设计")。候选:

| | 形态 | 一句话 |
|---|---|---|
| S-A | **便携调度器 + 声明座席** | `jobs.Scheduler` 只经 `Queue.Enqueue` 驱动;模块把自己的周期任务**声明**到 `pkgcore.Registry` 的新座席;窗口键派生统一 |
| S-B | **各实现原生调度** | StandaloneQueue 内建 ticker;asynq 用 `asynq.Scheduler`(leader 选举 + cron) |
| S-C | **自续作业链** | 终态事件 → 业务订阅者重排下一轮 |
| S-D | **维持现状** | 宿主继续各写各的 ticker |

### 3.2 裁定:S-A(便携调度器 + 模块声明制)

**裁定:`jobs` 增加 `Scheduler`——一个只依赖 `Queue.Enqueue` 的便携驱动;周期任务由拥有它的模块声明到 `pkgcore.Registry` 的新座席(Registry 结构模式的适用情形:新增一类注册只改 Registry,不动 `Module` 接口);调度器由宿主启动、宿主提供租户宇宙;幂等键的窗口派生在 jobs 内统一一份。宿主的总开关是"启不启动调度器",不是"逐条批准声明"。**

理由:

1. **一套逻辑两个模式跑同一份代码**。调度器只用便携契约的一个方法(`Enqueue`),`StandaloneQueue` 与 `asynq.Queue` 的调度语义不会分叉(对比 S-B:窗口语义与 cron 语义是两套东西,同一个声明在两个模式下行为不同)。
2. **去重复用已被七处证明的窗口化幂等键**。多副本下"每个副本都启动调度器"的安全性与今天的多副本宿主 ticker 完全同构——收敛靠窗口键,不依赖 asynq.Scheduler 的 leader 选举,也不依赖任何模式的特别能力。
3. **打包纪律**。调度器在 `jobs` 根包,只 import pkgcore 与标准库——不新增任何第三方 import,根包非测试代码既有的第三方依赖(`gorm.io/gorm`、`go.opentelemetry.io/otel`、`github.com/google/uuid`)不因它变化;S-B 若用 `asynq.Scheduler`,要么在根包引入 asynq,要么再开子包,前者直接违反 `docs/internal/03` 的打包裁定。
4. **模块声明制闭合记录的缺口**。24 号文档普查行 4 与 compliance AGENTS.md 记录的缺口形态是"模块没有自己的 schedule 点,每个接入应用都要记得加一个"——声明制让模块**拥有**自己的默认排产(声明 = 任务类型 + 周期 + 租户作用域),宿主侧留总开关。反过来说,若改成"宿主逐条声明",忘记的代价原样保留,只是换了个人忘记。
5. **反投机检查**:七处真实位点(其中四处正缺宿主 schedule 点)、两只真实宿主 ticker、一条在案的 roadmap 前置——不是先建后用。

### 3.3 形态细节

- **座席**。`pkgcore.Registry` 新增字段 `Schedules`(接口形状照既有座席:`Add(decls ...PeriodicTask) error` 拒绝重复类型、`Declarations() []PeriodicTask` 按声明序返回;错误值沿用既有 `ErrDuplicate*` 家族风格)。声明 `PeriodicTask` 的字段:任务类型、周期 `Every`(窗口大小)、作用域 `Scope ∈ {Platform, PerTenant}`。
- **驱动**。`jobs.NewScheduler(q Queue, opts ...SchedulerOption)`,选项含 `WithTenantLister` 与 `WithInterval`(tick 粒度,参考应用今天的 1 分钟),`Start`/`Stop` 生命周期与队列同款;选项校验照既有约定(非法值 = 选项期带码 panic)。
- **租户宇宙 seam**。调度器接受任一个结构性满足 `ListTenants(ctx) ([]pkgcore.TenantID, error)` 的值——形状与 compliance 自己的 `TenantLister`(retention.go:108)逐字对齐,宿主已有的实现不改一行即可同时满足两处(结构类型 seam 的先例:org 的 `Scope`/`FeatureGate`、config 的 `WithResolver`)。参考应用的 `periodicTenantUniverse`(configured ∪ D3 台账)改造成这样一个实现——它本来就是宿主资产,不迁进任何模块。
- **每 tick 的行为**:对每条声明,`Platform` 作用域直接入队一次(平台哨兵租户,先例:`platformScanTenantID`/`platformCRLRegenerateTenantID`);`PerTenant` 作用域经 lister 展开、逐租户入队(每个租户携带自己的上下文,镜像今天 `runPeriodicTasks` 的形状)。幂等键 = 该站点既有的前缀 + 租户段 + 窗口起点(UTC RFC3339),窗口截断在绝对时钟上——**与七处现键逐字对齐是迁移轮的硬要求**(同一 (类型, 租户, 窗口) 经调度器与经手动的 `Enqueue*` 必须解析出同一个键,否则同一窗口会跑两次)。
- **无 lister 的 `PerTenant` 声明 = Start 报错**(点名声明),而不是静默不扫——静默不扫正是 reference-app 的 ledger 半修掉的那个 bug 形态。
- **失败与门控语义**照抄现宿主 ticker 的契约:入队失败记日志、下一 tick 重试;tick 体同步、`time.Ticker` 不排积压。调度器与 worker 的关系是宿主政策:参考应用今天把排产绑在 `cfg.DisableQueueWorker` 门后(注释记录的理由:排一个本副本无法执行的作业没有意义);"只排产不执行"的副本在分布式模式下也是合法组合(`Enqueue` 不要求 `Start`),文档写明两种组合,参考应用保持现状绑法。
- **模块侧**。七处各自的 `Enqueue*` 方法**保留**(锁步冻结,手动/临时触发仍然合法);各模块的 `Register` 增加自己的声明。四件套里的窗口键与 handler 不动,被统一的是**驱动侧**:窗口起点函数、ticker、tick 体、每租户循环从宿主/宿主 glue 收进调度器。

### 3.4 备选否决理由

- **S-B 各实现原生调度**:(i) 两套语义(窗口截断 vs cron 表达式)对同一份声明给出不同行为,跨模式一致性无从声明;(ii) `asynq.Scheduler` 是 leader 选举 + cron 引擎,而这里需要的只是"固定窗口、幂等收敛",引入的是能力冗余;(iii) 打包成本见 §3.2 第 3 条;(iv) StandaloneQueue 的内建 ticker 仍是新机制,没有省掉任何东西,只是复制一份。
- **S-C 自续作业链**:(i) 续排逻辑回到每个业务模块(每类任务自己的"终态 → 重排下一轮"代码 + 首轮种子),把刚从宿主收走的重复又按模块摊开;(ii) 死信断链需要每处单独再实现"死信也要续排",而现窗口键形态里这是天然成立的(下一窗口是新键);(iii) 固定延迟会随执行时长漂移,现窗口语义是固定节律;(iv) 与 periodic_scheduler.go:17 的现状裁定("who decides when that happens is the host")距离更大——链的起点与终止反而失去了单一的责任点。
- **S-D 维持现状**:重复已七处自认"mirroring";缺 schedule 点的四处(4/5/6/7)每多接入一个应用就多一次"忘记加"的机会,compliance 的 Known limitations 原文记录的就是这个后果。

## 4 迁移计划

### 4.1 信用结算路径(internal/smilesim)

- **主路径**:宿主装配处新增一条 `jobs.job.terminal` 订阅(安装位置比照 `wireSelfService` 的既有宿主 glue 形状)。订阅者只做一件事:对事件里的作业查自己的预留台账(`reservation_store.get`),有行则调用**现行** `settleCredit()`——结算语义、幂等、孤儿行分支全部零改动。结算延迟从"至多 5 分钟或客户端某次轮询"压到"迁移即达"。
- **通知半边**:`EventSimulationCompleted` 的发布可从轮询驱动挪进同一订阅者,现 latch 语义(恰一次、发布被拒可重试)原样保留;`NotifyOnCompletion` 的轮询调用点(`internal/app/smilesim.go:265`)随之可退休,或保留为第二条腿(开放问题 6)。
- **对账网保留**:`ReconcileOutstandingCredits` 继续存在(总线实现可能按设计丢弃失败 handler、消费方可能停机),其**驱动力**从 `StartReconciler` 专用 goroutine 迁到调度席位——声明一条 `smilesim.*` 任务,handler 跑 `ReconcileOutstandingCredits`,`DefaultReconcileInterval` 成为声明的 `Every`。宿主第二只 ticker 消失。
- **结论**:结算路径**不需要为信号重构任何业务逻辑**——需要变的只有"何时被叫醒"。`Confirm`/`Refund` 的比较并交换幂等已经是"被叫醒两次"的答案。

### 4.2 七处周期任务与宿主

- **声明迁移**:七处各自在 `Register` 里声明(1/2/5/6/7 为 `PerTenant`,3/4 为 `Platform`);`Enqueue*` 方法与各自的键前缀保留。
- **参考应用**:`periodic_scheduler.go` 的 ticker 循环删除;`periodicTenantUniverse` 改造成宿主的 `TenantLister` 实现(configured ∪ D3 台账的逻辑原样保留);启动门(`cfg.DisableQueueWorker`)原样保留,成为"是否 `Start` 调度器"。`flowtests/periodic_scheduler_flow_test.go` 的两段 two-boot 证明继续作为端到端验收(证明的对象从"宿主 ticker"换成"调度器",断言不变)。
- **CRL 重生成(4)**:声明即排产——`pki` 在 `Register` 里把该任务声明在座席上,凡启动 `jobs.Scheduler` 的宿主(参考应用在内)都按声明窗口(默认一小时)对它排产;参考应用的校验侧读的是行状态与证书链而非 CRL,所以"没有等待它的读者"这一事实不变,变化只在:将来有了读者也无需新写 ticker,座席上的这条声明原本就生效。
- **5/6/7 的新增效果**:这三处今天有实现、无调用方(§1.4);声明制落地即获得各自的默认排产点——billing 的支付轮询、sharing 的分享过期清扫、integration 的密钥过期清扫首次真正跑起来,这是该席位对"模块内调度点"缺口的直接闭合(24 号文档普查行 4 的挂账随本轮出清)。

### 4.3 有意不动的部分

- `OnFailure` 与 `self_service.go` 的注册供给重试链:不迁移——死信补偿是它自己的事,终态事件与它边界清晰(§2.4),现状自洽。
- `jobs.Wire`/`RegisterHandler`/各模块的 `WithQueue` 接线:不动。
- 队列自身的 dispatch/claim/bookkeeping 机制:不动。

## 5 实现轮次划分

- **R1 终态信号机制**(go/jobs 根包 + `queue/asynq`):事件类型与载荷、两个实现的 `WithEventBus`、StandaloneQueue 的 `terminal_published_at` 列(含存量回填)与发布遍、asynq 的三处发布点、契约文本(AGENTS.md 与 `FailureHook` 边界段的对偶)、测试(崩溃后补发、取消赢不产生丢弃事件、重复容忍、两模式契约一致、发布失败不阻塞 worker 终态路径)。消费方零改动——无人订阅时事件是无副作用的 no-op,信号可以先落地。
- **R2 周期调度席位**(pkgcore + go/jobs + 七模块 + reference-app):`Registry.Schedules` 座席、`jobs.Scheduler` + lister seam + 统一窗口键派生、七处声明、参考应用 ticker 删除与 lister 改造、键对齐(pin 每个站点的前缀,保证同一窗口两条路径同键)、flow test 照旧。R2 出清 24 号文档普查行 4。
- **R3 信用结算迁移**(reference-app/internal/smilesim):订阅者 + 通知半边 + `ReconcileOutstandingCredits` 上席位。**不硬依赖 R2**:R2 未落地时 reconciler 驱动力保持 `StartReconciler` 现状,R3 只做信号侧;R2 已落地则一并迁。
- **顺序理由**:R1 先于 R3(结算需要信号);R1 与 R2 都重度触碰 `go/jobs` 同几个文件,串行落地避免并行改动;R2 的席位对 R3 是可选增强而非前置。
- **R4(硬化,等 asynq 腿获得真实消费方)**:补投扫描(开放问题 1)。

## 6 开放问题

1. **asynq 腿的补投闭合**。"发布先于归档"把队列侧崩溃转化为重复而非丢失,但 `completed` 状态的保留期是 `DefaultCompletedRetention`(24h):一次超过该窗口的停机,可能让"成功"事件永久缺失(死信行归档持久,不受此限)。是否需要 Start 时对 archived/completed 的补投扫描加 Redis 侧已发布标记,待 asynq 腿出现真实消费方时裁决(参考应用今天不 import `queue/asynq`)。
2. **`jobs.job.terminal` 的类型目录声明归属**。`EventRegistrar.Publishes` 的调用方按约定是模块,而 `jobs` 不是 `pkgcore.Module`。实现轮需要在"给非模块发布者开一条声明路径"与"沿用目录声明是契约、不是发布前置条件的现状(仅常量 + 文档)"之间裁定。
3. **载荷是否携带 `Result` 体**。本文裁定不携带(事件是通知,行是数据面);若通知半边证明 asynq 的前置发布次序下无法用回读取结果,再重新评估。
4. **声明是否携带 Enqueue 选项**(优先级/重试/超时)与宿主能否覆盖声明周期。今天的七处键与 handler 不携带额外选项,最小声明够用;宿主覆盖能力等真实诉求。
5. **窗口键的最终格式**。七处前缀不统一(storage 是 `storage.sweep:` 而非任务类型前缀),迁移轮需要逐处 pin 出"调度器键 = 手动态键"的字符串,并写进各站点的窗口测试(先例:`go/compliance/retention_test.go` 的窗口套件、`go/pki/enqueue_window_test.go` 已用真实 `StandaloneQueue` 钉窗口性质)。
6. **作业状态路由是否保留轮询驱动的 `NotifyOnCompletion` 调用**作为通知的第二条腿。保留 = 通知有两条腿(现有行为);退休 = 通知单靠事件 + 对账网。裁定取决于"通知丢失"的容忍度,属产品判断。
7. **升级存量行的回填时点语义**:本文定为"加列时刻盖章";实现轮需在双方言(尤其 PostgreSQL 的 DDL 事务边界)下钉住这一点。
