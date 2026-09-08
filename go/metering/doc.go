// Package metering provides usage metering: a single Recorder interface
// business modules call to report usage, decoupled entirely from which
// backend actually stores and aggregates it (docs/internal/06-billing-and-metering.md's
// "same pipeline, swappable backend" framing). It sits above authn/rbac/org
// in the module dependency graph
// (docs/internal/01-architecture.md) and implements pkgcore.Module like
// every other business module.
//
// # Two reliability tiers, not two variants of one thing
//
// The design doc is explicit that "fail-open, never block the caller" is
// correct for analytics-grade metering and wrong for billing-grade
// metering -- dropping one event there is one uncollected charge, silently.
// This module ships both tiers as genuinely different call shapes, not one
// mechanism with a flag:
//
//   - Analytics-grade: AnalyticsRecorder, an in-process bounded channel plus
//     a background flush goroutine. A full buffer drops the event and
//     increments a counter rather than blocking the caller -- see
//     AnalyticsRecorder's doc comment for the drop behavior spelled out in
//     full, per this repository's no-silent-caps discipline.
//   - Billing-grade: Enqueue, an outbox-pattern helper the CALLER runs
//     inside its own database transaction (the same "write a row in the
//     same transaction as the business write" shape as
//     go/dbkit's audit_capture.go plugin, but this is metering's own table,
//     not dbkit's, and a helper function the caller invokes explicitly
//     rather than an automatic GORM callback). A background Dispatcher then
//     delivers the row into the same aggregation pipeline, retrying
//     indefinitely on failure -- see Enqueue's and Dispatcher's doc
//     comments.
//
// Both tiers funnel into the same aggregation pipeline -- an in-process
// real-time quota counter plus a database-backed usage-summary row -- but
// through two different entry points on Aggregator: the plain Ingest for
// analytics-grade, and IngestBillingGrade for billing-grade, which
// additionally records a durable idempotency receipt in the SAME database
// transaction as the summary write, so Dispatcher redelivering a row whose
// earlier delivery attempt already committed (a crash, or merely a
// transient failure, between that commit and the outbox row's own
// mark-delivered write) applies the event exactly once rather than
// double-counting it. This is the design doc's "same pipeline" property --
// a caller who later needs to move billing-grade delivery onto a
// jobs-queue-driven poller, or add a Redis/PostgreSQL-backed aggregation
// backend, changes what feeds Aggregator's ingest methods without touching
// how any business module calls Record or Enqueue.
//
// # One feature belongs to exactly one reliability tier
//
// The shared pipeline makes this a prohibition, not a style suggestion:
// both tiers fold into the same per-tenant, per-feature, per-period
// counter entries and UsageSummary rows, and neither carries a tier
// marker, so a feature recorded through both tiers -- AnalyticsRecorder's
// fail-open Record on some call sites and the billing-grade Enqueue on
// others -- is a blend no reader can attribute. The mixing cost is
// concrete: the row's quantity then mixes data that may be dropped by
// design (the analytics tier, whose retried Record is not deduplicated
// either, so a retry can fold a second time into the same row) with data
// that must neither drop nor double-count (the billing-grade tier), and a
// billing figure read from that row silently contains quantities it was
// allowed to lose. A feature whose records must not be lost is recorded
// through the billing-grade path alone -- it already feeds the same
// real-time counters and summary rows -- and the analytics tier is for
// features that carry no billing weight. The prohibition is caller
// discipline, not pipeline enforcement: nothing in Aggregator
// distinguishes the tiers structurally. See UsageEvent.Feature's own doc
// comment and AGENTS.md's "Reliability tiers" section.
//
// # Round 1 of an unbounded number
//
// This is a deliberately bounded foundation round. It ships the Recorder
// pipeline end to end (both reliability tiers), the in-process aggregation
// backend only (real-time sync.Map counters plus SQLite/PostgreSQL
// usage-summary tables -- no Redis Streams, no PostgreSQL atomic-increment
// backend, no TimescaleDB raw-detail storage), and a threshold-crossing
// event published on pkgcore.EventBus when a tenant's real-time counter for
// a feature passes a configured limit. It does NOT ship the Plan/Feature/
// Entitlement domain model, actual blocking or allowance decisions for
// over-quota calls, or credits: docs/internal/06-billing-and-metering.md's
// domain-model section places all three in go/billing, which has since
// shipped them in its own round-1 foundation (Feature/Plan/Grant/
// Entitlements, EntitlementsService.Check's Block/AllowAndBill/Notify
// decision, and the credits ledger) -- metering measures and signals,
// go/billing decides. go/billing is also this module's first real
// consumer: its UsageReader interface compile-asserts *metering.Aggregator
// (billing/module.go) and its EntitlementsService quota decisions read
// Aggregator.RealtimeCount through that seam (billing/entitlements.go) --
// see AGENTS.md's "billing is metering's first consumer" note. See
// AGENTS.md's Known limitations for the complete boundary.
package metering
