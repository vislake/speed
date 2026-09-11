---
title: metering
description: "Usage recording: one Recorder interface with two reliability tiers — fail-open analytics and billing-grade outbox delivery — aggregated into per-tenant usage summaries."
weight: 5
---

# metering

metering is speed's usage-metering module: a single `Recorder`
interface business code calls to report usage, decoupled entirely from
which backend stores and aggregates it. metering measures and signals;
`go/billing` decides — this module ships no plan, quota or entitlement
model of its own.

## What it is for

A business module reports one `UsageEvent` per unit of usage — tenant,
`Feature`, `IdempotencyKey`, `Quantity`, optional metadata. Two
reliability tiers record those events:

- **Analytics-grade** (`m.Recorder()`, an `AnalyticsRecorder`):
  fail-open. A bounded in-process channel plus a background flush; a
  full buffer drops the event and counts it (`Dropped()`), and a
  retried record is not deduplicated. For usage with no billing
  weight.
- **Billing-grade** (the package-level `Enqueue` + a `Dispatcher`):
  must not silently drop and must not double-count. `Enqueue` writes
  an outbox row **in the caller's own transaction** — the row commits
  exactly when your business write does — deduplicated by a UNIQUE
  `(tenant_id, idempotency_key)` index; `Dispatcher` polls pending
  rows and delivers each into the aggregator, retrying indefinitely
  with a fixed delay, escalating a permanently failing row to an
  Error-level alert past a stated horizon, and settling each delivery
  with a durable idempotency receipt in the same transaction as the
  summary write — a redelivered row can never double-count.

Both tiers converge on the same in-process `Aggregator`: real-time
counters per (tenant, feature, period bucket), database-backed summary
rows (`metering_usage_summaries`, tenant-scoped), and an
edge-triggered overage-threshold event on the bus — published exactly
once per crossing. **One feature belongs to exactly one tier**:
recording the same `Feature` through both paths blends droppable and
must-not-drop data into one row no reader can attribute.

What it is **not**: no Plan/Feature/Entitlement model, credits or
quota enforcement (that is `go/billing`'s domain — billing's
`UsageReader` seam is satisfied structurally by `*metering.Aggregator`
for quota judging); no HTTP surface or OpenAPI fragment (it is a
Go-level API business modules call in-process); no distributed
aggregation backend — the in-process one is the shipped one.

## When to choose it

Any product surface whose use must be counted: feature adoption,
per-tenant dashboards, future quota judgment. Record at the moment of
use — a completed AI call, an API request, a document processed —
choosing the tier by what the number means: analytics-grade for
counters that may lose an event under load, billing-grade (inside the
transaction that already makes the thing being metered durable) when
the count is money. If you need plan- or credit-shaped decisions from
the numbers, pair it with billing; if you need an operator dashboard,
pair the summaries with admin's usage-summary endpoint.

## Wiring it in

```go
m := metering.NewModule(db)          // module in your Kernel.Bootstrap set
m.Start(ctx)                         // starts the analytics flush and dispatcher loops
defer m.Stop()

// analytics grade — fire from the request path, fail open:
if err := m.Recorder().Record(ctx, metering.UsageEvent{
    TenantID:       tenantID,
    Feature:        "ai.chat_tokens",
    IdempotencyKey: "chat:" + callID,
    Quantity:       float64(tokens),
}); err != nil {
    // handle err
}

// billing grade — inside the caller's own transaction:
if _, err := metering.Enqueue(ctx, tx, metering.UsageEvent{
    TenantID:       tenantID,
    Feature:        "image.credits",
    IdempotencyKey: "img:" + jobID,
    Quantity:       1,
}); err != nil {
    // handle err
}
```

Host options set the period bucket (`WithPeriodBucket`, daily or
monthly), overage thresholds (`WithOverageThresholds`), the analytics
buffer and the dispatcher's interval, batch size, retry delay,
escalation horizon and outbox retention. `m.Summaries()` reads the
per-tenant summary rows; `m.Aggregator()` serves real-time counters
and the billing-grade ingest.

## Core concepts and API surface

- **The outbox is platform data on purpose.** `metering_outbox_records`
  is deliberately not tenant-scoped — a background dispatcher must see
  every tenant's pending rows, which a tenant-scoped repository cannot.
- **Dedupe is database-arbitrated.** `Enqueue`'s unique index and the
  receipt's `ON CONFLICT DO NOTHING` keep both tiers idempotent
  without ever aborting a caller transaction — behavior proven on real
  PostgreSQL, where a plain caught-violation retry would poison the
  transaction.
- **Restart-safe counters.** Real-time counters are seeded from the
  committed summary rows on first touch, so a process restart loses
  nothing and never double-fires an overage crossing.
- **Fair retry, honest escalation.** The claim order is the retry
  schedule alone — never-failed rows never starve a once-failed one,
  and a permanently failing row never converges to a terminal state
  without alerting.
- **Coded errors** (`metering.missing_tenant_id`,
  `metering.invalid_quantity`, `metering.field_too_long`,
  `metering.usage_summaries_unconfigured`, ...) — see the [error code
  index](../../../error-codes/#metering). There is deliberately no
  `metering.unknown_feature`: metering has no feature catalog — that
  belongs to billing.

## Limitations and links

- `Dispatcher` assumes **one process** runs against a database at a
  time: its claim is a read, not an atomic claim-and-lock, so two
  concurrent dispatchers waste work (never double-count — the receipts
  arbitrate that) until a real claim step exists.
- `AnalyticsRecorder` does not deduplicate retried records, by design;
  summary writes serialize on a process-wide mutex, with the atomic
  upsert closing the cross-process race; retries have no backoff
  curve.
- The module's own dependency floor is deliberately narrow — pkgcore,
  dbkit and observability, plus GORM, the OpenTelemetry metric packages
  and the UUID package (`google/uuid`).

### Source

- [go/metering/AGENTS.md](https://github.com/vislake/speed/blob/main/go/metering/AGENTS.md) — the authoritative document (tiers, outbox semantics, aggregation, known limitations)
- Related pages: [Platform services](../), the domain guide [Billing and metering](../../../domains/billing-metering/)
