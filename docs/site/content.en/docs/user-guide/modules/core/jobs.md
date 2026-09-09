---
title: jobs
weight: 6
description: "The asynchronous task queue — one Queue/Task/Job/Handler contract implemented twice: StandaloneQueue for the standalone deployment mode, the Redis-backed queue/asynq subpackage for the distributed one."
---

# jobs

speed's asynchronous task queue: the portable
`Queue`/`Task`/`Job`/`Handler` contract both deployment modes
implement, plus the two implementations — `StandaloneQueue` (the
standalone mode's in-process worker pool over a SQLite-persisted task
table, zero third-party imports in its own non-test code) and the
`queue/asynq` subpackage's Redis-backed `Queue` (split into its own
subpackage so a standalone-only consumer never carries asynq and
go-redis in its `go.mod`). Long-running work belongs here: `storage`
derives thumbnails, `notification` delivers, `billing` generates
invoices, `ai-gateway` runs generation jobs, `integration` delivers
webhooks — all through this queue.

## When to choose it

Any operation that must not run synchronously inside an HTTP request
— a job that takes longer than a request should live, a retryable
side effect, work a caller should not wait for. The host chooses the
implementation by deployment mode at kernel startup (never branched
on in business code): `NewStandaloneQueue` over a `dbkit.Open`
database for standalone, `asynq.NewQueue(redisOpt)` for distributed.
Business code sees only the `Queue` interface — the same
`Task`/`EnqueueOption`s and the same `Handler` on both.

## Wiring and minimal use

```go
db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
// handle err

queue := jobs.NewStandaloneQueue(db, jobs.WithWorkerCount(8), jobs.WithTenantConcurrencyLimit(2))
if err := queue.RegisterHandler(&ImageGenHandler{svc: svc}); err != nil {
    // handle err
}
if err := queue.Start(ctx); err != nil { // creates schema, launches dispatcher and workers
    // handle err
}
defer queue.Close(shutdownCtx) // drains in-flight jobs up to ctx's deadline

id, err := queue.Enqueue(ctx, jobs.Task{
    Type:           "ai.generate_smile",
    TenantID:       tenant, // a field on Task, never resolved from ctx
    Payload:        payloadBytes,
    IdempotencyKey: "smile-gen:" + requestID, // derived from the operation, never random
}, jobs.WithPriority(jobs.PriorityHigh), jobs.WithMaxRetries(2))
// handle err
```

The distributed equivalent is the same call shape with
`asynq.NewQueue(asynq.RedisClientOpt{Addr: cfg.RedisAddr}, ...)`;
omitted options default to `DefaultMaxRetries` 3, `DefaultTimeout`
5 minutes, `DefaultWorkerCount` 4, per-tenant concurrency 2.
Construction options validate at option time — invalid values are
refused with coded panics, never accepted silently.

A `Handler` never knows which deployment mode it runs under, and its
`ctx` already carries `job.TenantID`:

```go
func (h *ImageGenHandler) Type() string { return "ai.generate_smile" }

func (h *ImageGenHandler) Handle(ctx context.Context, job *jobs.Job, progress jobs.ProgressFn) (jobs.Result, error) {
    progress(10, "starting")
    out, err := h.svc.Generate(ctx, job.Payload)
    if err != nil {
        return jobs.Result{}, err // retried, then dead-lettered
    }
    progress(100, "done")
    return jobs.Result{Data: out}, nil
}

// OnFailure runs once, after every retry is exhausted — the queue's
// entire compensation surface. Business compensation lives here, never in jobs.
func (h *ImageGenHandler) OnFailure(ctx context.Context, job *jobs.Job, cause error) {
    h.credits.Refund(ctx, job.TenantID, job.IdempotencyKey)
}
```

## Core concepts and API essentials

- **The tenant context trap** — `Enqueue` and `Handle` run on
  completely different contexts, possibly hours apart; a context
  cannot be persisted. The worker rebuilds the tenant from the
  job's own record (`jobContext` on `StandaloneQueue`, the
  `tenant_id` header on `asynq.Queue`) before every `Handle` —
  closed by construction, so a `dbkit.Repository[T]` call inside a
  handler never fails closed for lack of a tenant. `Task.TenantID`
  is required non-empty (`ErrInvalidTask`).
- **Lifecycle** — `Status` runs
  pending → running → retrying → succeeded / dead-letter (plus
  cancelled); on failure, attempts are compared against `MaxRetries`
  with exponential backoff (`WithBackoff` on `StandaloneQueue`,
  asynq's own retry machinery on the distributed side). `Get`
  answers `jobs.job_not_found` for "no such id" and "not your
  tenant" indistinguishably; dead-lettered jobs are listed by
  `DeadLetterJobs`, never silently dropped.
- **Fairness and safety on `StandaloneQueue`** — the dispatcher's
  candidate window interleaves round-robin across tenants (one
  tenant's backlog cannot starve another), per-tenant concurrency
  limits admission, and exactly one live queue per jobs table
  (`ErrQueueWriterActive`): `Start`'s writer gate is what makes
  crash recovery of `StatusRunning` rows safe against double
  execution. `Cancel` marks a pending job so it never runs; it does
  not preempt an in-flight `Handle` (the distributed queue can,
  best-effort, via asynq).
- **Idempotency** — a non-empty `IdempotencyKey` dedupes per
  `(tenant, key)`. It becomes the JobID and is logged verbatim as
  `job_id`, so keys must be derived from the operation's own opaque
  identifiers — never free text carrying PII. On the distributed
  queue dedupe lasts as long as asynq's retention of the record
  (succeeded jobs: `completedRetention`, default 24h), not forever;
  `StandaloneQueue`'s SQLite row is never deleted.
- **Observability** — every log line goes through `obs.FromContext`
  with constant messages and `snake_case` attributes; both
  implementations wire `jobs.queue.depth` and the
  `jobs.job.duration`/`attempts`/`dead_letter` outcome instruments —
  never with `tenant_id` as a label.

## Boundaries and pitfalls

- Do not assume a worker has tenant context — it does not until the
  queue rebuilds it; the rule applies identically on both
  implementations.
- Do not put business compensation in the queue layer: implement
  `FailureHook.OnFailure` in your handler. `OnFailure` firing before
  the dead-letter write has persisted (distributed queue) is a
  documented ordering difference — the call itself is the one signal.
- Never register the same `Task.Type` twice
  (`jobs.duplicate_handler_type`), and register handlers before
  `Start` — a job claimed before its handler registered is retried,
  not retroactively served.
- A `Task.IdempotencyKey` naming a periodic operation must scope the
  key to the period it is for (window-start suffix), or the period's
  job dedupes against the previous one.
- `asynq.Queue` needs `Retention` on every enqueue internally — a
  succeeded job's record is otherwise deleted instantly and `Get`
  breaks; the retention constants are construction defaults for a
  reason.

## Source

- [jobs AGENTS.md](https://github.com/vislake/speed/blob/main/go/jobs/AGENTS.md)
- [jobs `example_test.go`](https://github.com/vislake/speed/blob/main/go/jobs/example_test.go)
