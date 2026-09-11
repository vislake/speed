---
title: 任务与通知
weight: 5
description: 你基于 speed 的产品中的后台任务与用户消息——jobs 队列契约,以及 notification 模块的类型注册、收件箱与经过同意验证的投递。
---

# 任务与通知

异步的一半由两个模块承担:`jobs` 运行后台工作(一个可移植的队列契
约、两个实现),`notification` 是消息面——你的产品发出的每条消息都走
一个已声明的通知类型,投递到收件箱、邮箱或手机号。

```mermaid
flowchart LR
    H[你的 handler] -->|Enqueue Task| Q[jobs.Queue]
    Q -->|认领 + 租户上下文| W[worker: 注册的 Handler]
    W -->|完成或重试/死信| Q
    B[业务模块] -->|Dispatch + reg.NotificationsSeat().Add| N[notification]
    N -->|每位收件人每通道一个 job| Q
    Q --> D[投递 job:发送时重查偏好、\n同意、地址]
    D --> I[in_app_messages 行 / email / SMS]
```

## 后台工作:jobs 队列

任何不该在 HTTP 请求内同步跑的操作——长任务、需要重试的工作、响应
发出后仍要发生的工作——都走 `jobs.Queue`。队列是一个小巧的可移植契
约(`Enqueue` / `Get` / `Cancel`),背后是同一个接缝的两个实现:
`StandaloneQueue`(SQLite 支撑,单进程部署)与 `go/jobs/queue/asynq` 的
Redis 支撑 `asynq.Queue`(分布式部署)。你的代码只面向 `Queue` 接口;
跑哪个实现是组装层决定。

- 任务是一个 `Task{Type, TenantID, Payload, IdempotencyKey}`——租户
  **在任务上**,不继承自调用方的 context,因为 worker 在几分钟后
  的另一 context 上运行。队列在每次 `Handle` 调用前重建租户上下文,
  注册的 `Handler` 收到的 `ctx` 已携带 `job.TenantID`。
- 每种任务类型注册一个 `Handler`(`NewHandlerFunc(type, fn)` 把普通
  函数适配成 handler),在 `Start` 之前注册;handler 通过
  `ProgressFn` 回调报告进度,job 记录的状态走
  `pending → running → retrying → succeeded`(或 `dead-letter`)。
- 重试与退避是队列的事(默认 `DefaultMaxRetries = 3`,每次调用可配
  `WithMaxRetries`/`WithDelay`/`WithPriority`)。**补偿不是**:当 job
  耗尽重试进入死信,handler 可以实现 `OnFailure` 跑业务补偿(退信用
  点、关预约)——那个钩子属于你的业务模块,永远不属于队列层。

最少接线(独立形态):

```go
q := jobs.NewStandaloneQueue(db)   // db 来自 dbkit.Open
q.RegisterHandler(jobs.NewHandlerFunc("notes.export", exportHandler))
q.Start(ctx)                        // 在任何 Enqueue 之前
defer q.Close(ctx)
```

## 消息面:notification

每种通知类型是**声明的,不是存模板**:你的模块在 `Register` 期间把
类型注册到注册表的通知席位(`reg.NotificationsSeat().Add(...)`),每个类型带其偏
好组、默认通道与收件人能否退订(验证码是事务性的,不可退订)。文案
存在声明模块自己的双语 locale 包里,投递时按收件人 locale 渲染——
绝不在注册时捕获。

接线 `notification.NewModule` 有六个必选选项,缺任何一个 `Register`
都以各自的错误拒绝:SMS sender、邮件 from 地址、两个联系人盲索引器
(加密的联系人地址只能通过它们查询)、投递队列,以及一个在发送时读
取用户收件人外发地址的 `UserAddressResolver`。

三条收件人路径,一条铁律——**一切在发送时重查,绝不冻结进 payload**:

- **用户投递**——`Dispatch` 通过实时偏好矩阵解析收件人通道(类型声
  明的默认值,被逐类型 × 逐通道的选择覆盖,退订对该类型是终态的),
  渲染文案,收件箱通道落一行 `in_app_messages`,每通道结算一行
  `send_records`。
- **外部联系人**——联系地址必须完成同意验证(`double_opt_in` 码以
  哈希存于行上;验证消息本身是"先同意后发送"规则的唯一例外)。
  `unsubscribed` 与 `bounced` 是每次投递都拒绝的终态。验证尝试在
  查验码之前先扣每地址预算。
- **收件箱投递**——行提交后才发 `notification.inbox.created`,SSE
  流(`GET /api/v1/notifications/stream`)按"先行后事件"的顺序宣告。

## 完整示例:后台导出笔记,完成后通知用户

用户在浏览器里点了「导出我的笔记」。HTTP 请求只做一件事——把
`notes.export` 任务入队并立刻返回它的 `JobID`——worker 几分钟后跑完
导出,沿途报告进度;一次瞬时失败会被重试,耗尽重试的任务进入死信并
触发业务补偿钩子(这里:撤销请求时做的信用点预留,即参考应用微笑模
拟路径的形态)。导出成功后,handler 向请求者派发一条「导出已就绪」
通知,收件人通道由 notification 模块在发送时按偏好矩阵重新解析。下
面的演练就是该契约的生产侧与消费侧一对;它在内存 SQLite 数据库上独
立运行。

前置条件:你的消费模块在 `go.mod` 里用 `replace` 把各 speed 模块指
到本地 checkout(`go mod tidy` 之后即可);把代码粘进你自己 `main`
包的文件里运行。第二个代码块展示通知那一半——它需要 notification
模块接线由装载器驱动(下方六个必选选项),且类型文案在你的双语
locale 包里——所以以宿主代码形式给出,不在这段演练里运行。

```go
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite" // registers DialectSQLite
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// exportPayload is the HTTP layer's Task.Payload — the queue never interprets it.
type exportPayload struct {

	RequesterID string `json:"requester_id"`
	NoteCount   int    `json:"note_count"`
	AlwaysFail  bool   `json:"always_fail,omitempty"`
}

// The consumer: one Handler per task Type, registered before Start;
// Handle's ctx already carries job.TenantID, rebuilt from the job record.
type notesExportHandler struct{}

func (notesExportHandler) Type() string { return "notes.export" }

func (notesExportHandler) Handle(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
	var p exportPayload
	if err := json.Unmarshal(job.Payload, &p); err != nil {
		return jobs.Result{}, err
	}
	fmt.Printf("[export] attempt %d: exporting %d notes\n", job.Attempts, p.NoteCount)
	if p.AlwaysFail { // every attempt fails: retries, then dead-letter
		return jobs.Result{}, errors.New("notes provider unreachable")
	}
	if job.Attempts == 1 {
		return jobs.Result{}, errors.New("transient provider timeout") // attempt 2 succeeds
	}
	progress(40, "writing rows")
	progress(100, "done")
	return jobs.Result{Data: []byte("id,title\n1,Caries 101\n")}, nil
}

// OnFailure compensates at most once, after the final attempt — refund here
// the reservation made with billing.CreditService.PreDeduct.
func (notesExportHandler) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
	fmt.Printf("[export] job %s dead-lettered: %v — refunding the reservation\n", job.ID, cause)
}

// waitForTerminal polls tenant-scoped Queue.Get until the job is terminal.
func waitForTerminal(ctx context.Context, queue jobs.Queue, id jobs.JobID) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := queue.Get(ctx, id)
		if err == nil && job.Status.Terminal() {
			fmt.Printf("[queue] %s: %s after %d attempts\n", job.ID, job.Status, job.Attempts)
			return
		}
		if time.Now().After(deadline) {
			fmt.Println("[queue] timed out waiting for", id)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runExportWalk() {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: "file:export-walk?mode=memory&cache=shared"})
	must(err)
	queue := jobs.NewStandaloneQueue(db, jobs.WithPollInterval(5*time.Millisecond))
	must(queue.RegisterHandler(notesExportHandler{}))
	must(queue.Start(ctx)) // before any Enqueue

	// The producer — what an HTTP handler body boils down to.
	enqueue := func(key string, alwaysFail bool) {
		payload, _ := json.Marshal(exportPayload{
			RequesterID: "user-7", NoteCount: 3, AlwaysFail: alwaysFail,
		})
		id, err := queue.Enqueue(ctx, jobs.Task{
			Type:           "notes.export",
			TenantID:       pkgcore.TenantID("tenant-acme"),
			Payload:        payload,
			IdempotencyKey: "notes.export:" + key, // replay dedupes onto the first job
		}, jobs.WithMaxRetries(2)) // 2 retries beyond the first attempt
		must(err)
		fmt.Println("enqueued", id, "(the HTTP response returns this JobID immediately)")
		waitForTerminal(pkgcore.WithTenant(ctx, "tenant-acme"), queue, id)
	}

	enqueue("2026-09-09-001", false) // succeeds on attempt 2
	enqueue("2026-09-09-002", true)  // dead-letters, compensation runs
}
```

同一故事的 notification 那一半——你的模块在 Register 时声明类型,导出
handler 在成功路径上派发:

```go
// In your module's Register: the type lands in the preference matrix;
// its copy lives in your bilingual locale bundle under
// <type_key>.<channel>.<part> ids, rendered in the recipient's locale.
if err := reg.NotificationsSeat().Add(pkgcore.NotificationType{
	Key:                   "reports.export_ready",
	Group:                 "reports",
	DefaultChannels:       []string{"in_app", "email"},
	RecipientVisibleParams: []string{"export_id"},
	Unsubscribable:        true, // the recipient may silence this type
}); err != nil {
	return err
}

// In the export handler's success path. deliveries is the host module's
// DeliveryService (module.Deliveries()), wired with its six required
// options and bootstrapped, as the reference app does.
notifCtx := pkgcore.WithTenant(context.WithoutCancel(ctx), job.TenantID)
_, err := deliveries.Dispatch(notifCtx, notification.Dispatch{
	TypeKey: "reports.export_ready",
	Recipient: notification.DispatchRecipient{
		Class:  notification.RecipientClassUser,
		UserID: requesterID, // from the export payload
	},
	Locale: recipientLocale, // e.g. i18n.LocaleZHCN — never guessed by the module
	Params: map[string]any{"export_id": exportID},
})
```

演练展示了两个契约事实:重试、调度与死信归队列,而*补偿*(OnFailure
里的退款)留在你的业务 handler 里——另外,一次通知派发绝不冻结投递
决策:类型的 `DefaultChannels` 只在收件人没有存储偏好时生效,投递
job 在发送时重查偏好、地址与同意。

运行步骤:

1. 在你的消费 `go.mod` 里为 `go/dbkit`、`go/pkgcore`、`go/jobs` 加
   `replace` 行,然后 `go mod tidy`。
2. 把第一个代码块放进你自己 `main` 包的文件,运行 `go run .`。
3. 第二个代码块是已接好 notification 模块的应用的宿主代码——要看
   完整可运行组合,启动参考应用跑它的通知流(链接见下)。

预期输出(`dead-lettered` 与 `dead_letter` 两行可能先后互换——
`OnFailure` 在死信写库后紧接着执行):

```text
enqueued <job id> (the HTTP response returns this JobID immediately)
[export] attempt 1: exporting 3 notes
[export] attempt 2: exporting 3 notes
[queue] <job id>: succeeded after 2 attempts
enqueued <job id> (the HTTP response returns this JobID immediately)
[export] attempt 1: exporting 3 notes
[export] attempt 2: exporting 3 notes
[export] attempt 3: exporting 3 notes
[export] job <job id> dead-lettered: notes provider unreachable — refunding the reservation
[queue] <job id>: dead_letter after 3 attempts
```

`<job id>` 是 `Enqueue` 返回的 id——自己打印它,或像 `waitForTerminal`
那样用 `Queue.Get` 读任务记录。

在参考应用中看到它:

- [examples/reference-app/internal/app/server.go](https://github.com/vislake/speed/blob/main/examples/reference-app/internal/app/server.go)
  ——应用的组合选中 `queue.standalone` 组件,装配的 `Start` 阶段由该
  组件执行它的一次 `jobs.Wire` 调用,把 `reg.Jobs` 上声明的每个
  handler 交给它的独立队列并启动;每个模块的 handler(storage 派生、
  notification 投递,以及本模式的各种变体)都骑在同一个队列上。
- [examples/reference-app/internal/app/demo/demo_notification.go](https://github.com/vislake/speed/blob/main/examples/reference-app/internal/app/demo/demo_notification.go)
  ——notes.note.created 事件被转成真实的 `Deliveries().Dispatch` 调
  用;[examples/reference-app/flowtests/notification_flow_test.go](https://github.com/vislake/speed/blob/main/examples/reference-app/flowtests/notification_flow_test.go)
  在组合好的 HTTP 栈上驱动整条投递。
- [go/jobs/example_test.go](https://github.com/vislake/speed/blob/main/go/jobs/example_test.go)
  ——本演练的队列一半,由模块自己的单元套件编译并执行。

## 下一步

- `jobs` 与 `notification` 的完整逐模块页(用法、选项、示例)将落在
  本栏的模块参考区。
- [错误码索引](../../error-codes/)——这两个模块可能应答的全部错误码。
