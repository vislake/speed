---
title: billing
description: "Commerce: the Plan/Feature/Entitlement domain model, a channel-agnostic subscription and invoice lifecycle, and the credits ledger — with an optional payment-gateway layer and a read-only HTTP surface behind it."
weight: 1
---

# billing

billing is speed's commerce module: the `Plan`/`Feature`/`Grant`/
`Entitlements` domain model, a channel-agnostic `Subscription`/`Invoice`
lifecycle, and the credits ledger — the second, parallel billing mode
credit-based pay-per-use products use alongside subscriptions. An
optional payment-gateway layer (`PaymentGateway`, a registry, three real
provider implementations) and a deliberately read-only HTTP fragment
complete the surface.

## What it is for

Three halves. **The domain model.** A platform-wide `Plan` catalog that
tenants can override with custom rows, grants of `FeatureKindBoolean`,
`FeatureKindUnlimited` and `FeatureKindQuota`, and
`Entitlements.Check(ctx, featureKey, requested)` — the single judgment
entry point answering "does this tenant's current subscription permit
this": per-tenant `Subscription` lifecycle (`created`/`active`/
`past_due`/`canceled`) driven by plain Go calls that publish
`EventSubscriptionStatusChanged`. **The credits ledger.**
`CreditService.PreDeduct` → `Confirm`/`Refund` is the reserve-then-settle
pattern every "pay-per-use that might fail" operation needs; `Grant`
tops up; `Expire` (keyed, retry-safe) is the at-most-once write a
scheduled expiry sweep calls; `Balance`/`Transactions` read back. The
ledger is append-only and concurrency-safe through one
database-arbitrated `UPDATE` per mutation. **The payment-gateway layer.**
`PaymentGatewayRegistry` plus three real providers
(`go/billing/gateway/{stripe,alipay,wechat}`), a deduplicated
`PaymentEvent` ledger, and `PollingService`, the active-polling fallback
for a channel webhook that never arrives.

What it is **not**: it is not metering — no usage collection lives here,
and quota judgment reads `go/metering`'s real-time counter through the
small `UsageReader` module, never a summary table (which has aggregation
delay). No inbound-webhook HTTP endpoint is mounted, so no live
`PaymentEvent` drives a `Subscription` transition. No scheduled credit
expiry ships — the mechanism exists, the sweep is product policy plus a
`jobs`-wiring host's work. Its HTTP fragment is read-only by decision.

## When to choose it

Your product charges: subscriptions, credit packs, or per-use gates.
Consult `Entitlements.Check` before a paid operation (the reference app
judges every AI call against it); run `PreDeduct` before work that may
fail and `Confirm`/`Refund` when it settles. Add the gateway layer the
day a real payment channel must be collected. If the numbers come from
usage, pair it with [metering](/docs/user-guide/modules/services/metering/);
if you need an operator dashboard over the results, pair it with
[admin](/docs/user-guide/modules/capabilities/admin/)'s usage-summary endpoint.

## Wiring it in

```go
b := billing.NewModule(db, nil) // nil UsageReader: quota grants fail closed
// in the component set your composition selects. Then:

// "may this tenant use feature X" — before the paid work:
d, err := b.Entitlements().Check(ctx, "model:chat:default", 1)
if err != nil || !d.Allowed { /* refuse */ }
// d.Remaining == nil means unbounded; the Reason vocabulary is
// billing.DecisionReasonOK, billing.DecisionReasonFeatureDisabled,
// billing.DecisionReasonQuotaExceeded, billing.DecisionReasonNoSubscription.

// pay-per-use: reserve before the work, settle after it:
res, err := b.Credits().PreDeduct(ctx, billing.PreDeductInput{
    Amount:        10,
    IdempotencyKey: "smilesim:" + reqID, // mandatory, becomes the row's ID
    Reason:        "ai_generation:job_123",
})
if err != nil { /* insufficient balance etc. */ }
// ... do the paid work ...
if _, err = b.Credits().Confirm(ctx, res.ID); err != nil { /* refund instead */ }
```

The `UsageReader` argument is `*metering.Aggregator` when you judge
quota-kind grants; `NewModule(db, nil)` is legal and makes such a grant
answer `billing.usage_reader_unconfigured` (fail closed) — Boolean
grants never consult it. Gateway wiring is optional: `billing.WithQueue(queue)`
arms the polling fallback, `billing.WithGateways(map)` or the registry's
`Build` (blank-import the provider subpackage) supplies channels.

## Core concepts and API surface

- **`Plan` is a dual-domain table, not `TenantScoped`.** `tenant_id`
  empty-string sentinel means platform-wide (the `go/config` answer);
  `PlanStore.Resolve` checks the tenant-custom row first and falls back
  to the platform-wide catalog. Isolation proof is
  `AssertNotTenantScoped`, and scoped `Get`/`Update` can touch only the
  named scope.
- **The credit ledger is append-only.** `CreditTransaction` has a
  composite `(id, tenant_id)` key — a `PreDeduct` row's ID *is* the
  caller's idempotency key — and its repository exposes `Insert` and
  reads, never `Update`/`Delete` (reflection-pinned). Mutations funnel
  through one guarded `UPDATE` (`applyBalanceDelta`) whose WHERE clause
  keeps both buckets non-negative, so concurrent deductions cannot
  overdraw.
- **Retry is answered with the first run's own row.** A retried
  `PreDeduct` reads back its earlier reservation on the same transaction
  (`ON CONFLICT DO NOTHING`, never a transaction-poisoning violation —
  the divergence only a real PostgreSQL tier can show, which is why
  billing has one). A keyed `Expire` that collides with a row of another
  kind answers `billing.idempotency_key_collision`.
- **The five state-changing credit methods emit their declared audit
  actions after commit** (`Grant`/`PreDeduct`/`Confirm`/`Refund`/
  `Expire`), recording the ledger row and the resulting balance.
- **Module accessors:** `Plans()`, `Subscriptions()`, `Invoices()`,
  `Credits()`, `Entitlements()`, `PaymentEvents()`, `Polling()`.
- **Coded errors** (`billing.insufficient_credits`,
  `billing.plan_not_found`, `billing.webhook_signature_invalid`, ...) —
  see the [error code index](/docs/user-guide/error-codes/#billing).

## Limitations and links

- No caller moves real money: the providers sign and verify real
  requests, but no reference-app or other consumer calls `CreateCharge`
  against a live Stripe/Alipay/WeChat account.
- `SubscriptionService.Active` assumes at most one active subscription
  per tenant; nothing enforces it at the database level either.
- The unkeyed `Expire` (operator one-off) is deliberately not idempotent
  under retry — the keyed form is what a sweep uses. The sweep itself is
  a host's `jobs` scheduling plus a product-policy decision.
- `Invoice` and the Quota/`UsageReader` judgment path have real,
  tested APIs but no in-workspace caller beyond the compiled examples;
  `go/billing/AGENTS.md` records the precise consumer status.
- The HTTP surface (`GET /api/v1/billing/credits/balance`,
  `.../credits/transactions`) is read-only; a refund is observable as a
  deduct row's status becoming `refunded`, never as a silent balance
  change.

### Source

- [go/billing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/billing/AGENTS.md) — the authoritative document (domain model, ledger semantics, gateway layer, limitations)
- Related pages: [metering](/docs/user-guide/modules/services/metering/), [ai-gateway](/docs/user-guide/modules/capabilities/ai-gateway/), the domain guide [Billing and metering](/docs/user-guide/domains/billing-metering/)
