---
title: jobs
weight: 6
description: "异步任务队列——一个 Queue/Task/Job/Handler 契约,两个实现:standalone 形态的 StandaloneQueue 与分布式形态的 Redis 承载 queue/asynq 子包。"
---

# jobs

speed 的异步任务队列:两种部署形态共同实现的便携
`Queue`/`Task`/`Job`/`Handler` 契约,加两个实现——`StandaloneQueue`
(standalone 形态的进程内 worker 池,任务表 SQLite 持久化,自身
非测试代码零第三方导入)与 `queue/asynq` 子包的 Redis 承载 `Queue`
(拆进独立子包,只用 standalone 的消费方绝不在 go.mod 里背 asynq 与
go-redis)。长任务归这里:衍生图生成(`storage`)、通知投递
(`notification`)、发票生成(`billing`)、AI 生成任务(`ai-gateway`)、
webhook 投递(`integration`)都经它入队。

## 何时选用

任何不该在 HTTP 请求内同步跑的操作——比一个请求该活的时间更长、
需要重试、调用方不该等它的工作。宿主按部署形态在装配启动时选实现
(业务代码里绝不分支):standalone 用 `NewStandaloneQueue` 配
`dbkit.Open` 的数据库,分布式用 `asynq.NewQueue(redisOpt)`。业务
代码只见 `Queue` 接口——两种形态下 `Task`/`EnqueueOption` 与
`Handler` 完全相同。

## 接线与最少使用

```go
db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
// handle err

queue := jobs.NewStandaloneQueue(db, jobs.WithWorkerCount(8), jobs.WithTenantConcurrencyLimit(2))
if err := queue.RegisterHandler(&ImageGenHandler{svc: svc}); err != nil {
    // handle err
}
if err := queue.Start(ctx); err != nil { // 建 schema,起 dispatcher 与 workers
    // handle err
}
defer queue.Close(shutdownCtx) // 在 ctx 截止前排空在飞任务

id, err := queue.Enqueue(ctx, jobs.Task{
    Type:           "ai.generate_smile",
    TenantID:       tenant, // Task 上的字段,绝不由 ctx 解析
    Payload:        payloadBytes,
    IdempotencyKey: "smile-gen:" + requestID, // 从业务操作派生,绝不随机
}, jobs.WithPriority(jobs.PriorityHigh), jobs.WithMaxRetries(2))
// handle err
```

分布式等价物是同形调用配
`asynq.NewQueue(asynq.RedisClientOpt{Addr: cfg.RedisAddr}, ...)`。
省略选项时的默认:`DefaultMaxRetries` 3、`DefaultTimeout` 5 分钟、
`DefaultWorkerCount` 4、每租户并发 2。构造选项在选项期校验——非法
值以带码 panic 拒绝,绝不静默收下。

`Handler` 不知道自己跑在哪种形态下,它的 `ctx` 已携带
`job.TenantID`:

```go
func (h *ImageGenHandler) Type() string { return "ai.generate_smile" }

func (h *ImageGenHandler) Handle(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
    progress(10, "starting")
    out, err := h.svc.Generate(ctx, job.Payload)
    if err != nil {
        return jobs.Result{}, err // 重试,然后死信
    }
    progress(100, "done")
    return jobs.Result{Data: out}, nil
}

// OnFailure 只在重试全部耗尽后跑一次——这是队列层补偿面的全部。
// 业务补偿(退 credits……)住在这里,绝不住进 jobs。
func (h *ImageGenHandler) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
    h.credits.Refund(ctx, job.TenantID, job.IdempotencyKey)
}
```

## 核心概念与 API 要点

- **租户上下文陷阱**——`Enqueue` 与 `Handle` 跑在完全不同的上下文
  上,可能相隔数小时;上下文无法持久化。worker 在每次 `Handle`
  之前从任务自己的记录重建租户(`StandaloneQueue` 的 `jobContext`,
  `asynq.Queue` 的 `tenant_id` 头)——构造上封闭,所以处理器里的
  `dbkit.Repository[T]` 调用绝不会因缺租户而失败关闭。
  `Task.TenantID` 必须非空(`ErrInvalidTask`);每个任务都可归属到
  某个租户。
- **生命周期**——`Status` 走
  pending → running → retrying → succeeded / dead-letter(加
  cancelled);失败时把尝试数对 `MaxRetries` 比较,指数退避
  (`StandaloneQueue` 用 `WithBackoff`,分布式侧用 asynq 自己的重试
  机制)。领到但没有注册处理器的任务先重试后死信——出现在
  `DeadLetterJobs` 里,绝不无声消失。`Get` 对「无此 id」与「不是你
  的租户」一律答 `jobs.job_not_found`。
- **`StandaloneQueue` 的公平与安全**——dispatcher 的候选窗跨租户
  轮转交错(一个租户的积压饿不死另一个),每租户并发限制准入;一张
  jobs 表恰好一个活队列(`ErrQueueWriterActive`):`Start` 的
  writer 注册门正是让 `StatusRunning` 行崩溃恢复免于双重执行的
  前提。`Cancel` 标记待运行任务使其永不执行;它不抢占在飞 `Handle`
  (分布式队列可经 asynq 尽力而为地打断)。
- **幂等**——非空 `IdempotencyKey` 按 `(tenant, key)` 去重。它
  会成为 JobID 并以 `job_id` 原样进日志,所以键必须从操作自己的
  不透明标识派生——绝不携带 PII 的自由文本。分布式队列的去重活得
  与 asynq 对该记录的保留期一样长(成功任务:
  `completedRetention`,默认 24 小时),不是永远;`StandaloneQueue`
  的 SQLite 行永不删除。
- **可观测性**——每行日志都走 `obs.FromContext`,常量消息加
  `snake_case` 属性;两个实现都接 `jobs.queue.depth` 与
  `jobs.job.duration`/`attempts`/`dead_letter` 结果指标,标签
  `(job_type, status)` / `(queue, status)`——绝无 `tenant_id`,
  按基数规则。

## 边界与注意

- 别假设 worker 自带租户上下文——队列重建之前它没有;这条规则对
  两个实现完全一样。
- 别把业务补偿放进队列层:在处理器里实现 `FailureHook.OnFailure`。
  分布式队列上 `OnFailure` 先于死信落库触发,是成文的顺序差异——
  调用本身才是唯一「此任务已彻底失败」的信号。
- 同一 `Task.Type` 绝不注册两次(`jobs.duplicate_handler_type`);
  处理器要在 `Start` 之前注册——在其处理器注册前就被领走的任务
  只会被重试,不会补服务。
- 命名周期性操作的 `Task.IdempotencyKey` 必须把键限定到它所服务
  的周期(窗口起始后缀),否则本周期任务会对上前一个。
- `asynq.Queue` 内部对每次入队都要 `Retention`——否则成功任务的
  记录被立即删除,`Get` 失效;保留期常量作为构造默认存在自有其理。

## Source

- [jobs AGENTS.md](https://github.com/vislake/speed/blob/main/go/jobs/AGENTS.md)
- [jobs `example_test.go`](https://github.com/vislake/speed/blob/main/go/jobs/example_test.go)
