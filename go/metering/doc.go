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
// Both tiers funnel into the same place: Aggregator.Ingest, which
// increments an in-process real-time quota counter and upserts a
// database-backed usage-summary row. This is the design doc's "same
// pipeline" property -- a caller who later needs to move
// billing-grade delivery onto a jobs-queue-driven poller, or add a
// Redis/PostgreSQL-backed aggregation backend, changes what feeds
// Aggregator.Ingest without touching how any business module calls Record
// or Enqueue.
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
// Entitlement domain model (docs/internal/06-billing-and-metering.md's own
// domain-model section -- that is go/billing's, a still-unstarted module),
// actual blocking of over-quota calls (metering measures and signals, it
// does not enforce), or credits (pre-deduct/confirm/refund is a separate,
// synchronous path the same design doc places in go/billing, not here).
// See AGENTS.md's Known limitations for the complete boundary.
package metering
