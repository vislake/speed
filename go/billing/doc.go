// Package billing provides speed's Plan/Feature/Entitlement domain model, a
// channel-agnostic Subscription/Invoice model, and a credits ledger with the
// reserve -> confirm/refund pattern every "pay-per-use that might fail"
// business needs. It sits above authn/rbac/org/metering in the module
// dependency graph and implements the module contract like every other business
// module.
//
// # Entitlements.Check is the one judgment entry point
//
// Business modules call Entitlements.Check to learn whether a tenant's
// current subscription permits a feature, never by reading the
// subscription/plan tables themselves. Check answers "does the plan allow
// this", nothing about money: for a Quota-kind Feature it reads
// go/metering's real-time counter (never metering's summary tables, which
// have aggregation delay and would let an over-quota request through) and
// applies the Grant's OverageMode. go/ai-gateway is a real consumer of the
// judgment: its checkEntitlement gate runs every Chat/ChatStream/
// GenerateImage call through a host-wired EntitlementsService.Check.
//
// # Credits are a separate path from Check
//
// Whether a plan permits a feature and whether a tenant's credit balance
// can cover one unit of it are two independent questions: Check never
// looks at credit_balance, and CreditService's PreDeduct/Confirm/Refund
// never consult a Plan. A per-use business operation typically checks
// both. The ledger's reserve -> confirm/refund pattern is consumed for
// real by examples/reference-app's smilesim flow, which reserves credits
// before an AI generation call and settles the reservation when the
// asynchronous job reaches its terminal status.
//
// # Deliberately bounded surface
//
// The module ships the domain model (Feature/Plan/Grant/Entitlements),
// tenant-custom Plan lookup precedence, a channel-agnostic
// Subscription/Invoice model whose lifecycle is driven by a plain Go call,
// the credits ledger (credit_balance, the append-only credit_transaction
// log, the reserve -> confirm/refund pattern, transactionally safe under
// concurrent deductions), the PaymentGateway seam with three real provider
// implementations under billing/gateway (stripe, alipay, wechat), and a
// read-only HTTP fragment. It does NOT ship actual money movement through
// a live wired payment-gateway consumer, an HTTP surface receiving live
// webhooks, or payment-event-driven Subscription/Invoice transitions --
// those boundaries are recorded in the module's Known limitations.
package billing
