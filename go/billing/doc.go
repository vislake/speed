// Package billing provides speed's Plan/Feature/Entitlement domain model, a
// channel-agnostic Subscription/Invoice model, and a credits ledger with the
// reserve -> confirm/refund pattern every "pay-per-use that might fail"
// business needs (docs/internal/06-billing-and-metering.md). It sits above
// authn/rbac/org/metering in the module dependency graph
// (docs/internal/01-architecture.md) and implements pkgcore.Module like
// every other business module.
//
// # Entitlements.Check is the one judgment entry point
//
// Business modules -- including the not-yet-built AI gateway -- call
// Entitlements.Check to learn whether a tenant's current subscription
// permits a feature, never by reading the subscription/plan tables
// themselves. Check answers "does the plan allow this", nothing about
// money: for a Quota-kind Feature it reads go/metering's real-time counter
// (never metering's summary tables, which have aggregation delay and would
// let an over-quota request through) and applies the Grant's OverageMode.
//
// # Credits are a separate path from Check
//
// Whether a plan permits a feature and whether a tenant's credit balance
// can cover one unit of it are two independent questions
// (docs/internal/06-billing-and-metering.md is explicit about this split):
// Check never looks at credit_balance, and CreditService's
// PreDeduct/Confirm/Refund never consult a Plan. A per-use business
// operation typically checks both.
//
// # Round 1 of an unbounded number
//
// This is a deliberately bounded foundation round. It ships the domain
// model (Feature/Plan/Grant/Entitlements), tenant-custom Plan lookup
// precedence, a channel-agnostic Subscription/Invoice model whose lifecycle
// is driven by a plain Go call, and the credits ledger (credit_balance,
// the append-only credit_transaction log, and the reserve -> confirm/refund
// pattern, transactionally safe under concurrent deductions). It does NOT
// ship the billing/gateway subpackage, any payment provider SDK or
// webhook/idempotency handling, actual money movement, AI gateway
// integration (go/ai-gateway does not exist yet), or an HTTP surface. See
// AGENTS.md for the complete boundary and known limitations.
package billing
