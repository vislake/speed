// Package notification delivers a tenant's notifications to the people it
// serves, on the channels those people want, and — for contacts who are
// not users of any tenant — only where consent has been verified first.
// Its design lives in docs/internal/07-platform-services.md.
//
// # What notification owns
//
//   - The in-app inbox: per-tenant rows in in_app_messages that a user
//     reads inside the product, plus the realtime hub that wakes a signed-in
//     recipient's stream connection the moment an inbox row lands. This is
//     the module's first-class channel — it has zero external dependencies,
//     so it works in every deployment composition.
//   - The per-type channel preference matrix that decides which channels a
//     type of notification may use, and the consent ledger that decides
//     whether a given external recipient may be messaged at all.
//   - Delivery: the queue-backed pipeline (Deliveries().Dispatch plus the
//     registered queue handler) that produces one rendered, deduplicated,
//     rate-limited message per recipient, per selected channel, with a send
//     record per attempt as the replay-safe outcome log. The pipeline's
//     input is a host's Dispatch call — hosts wire their own module events
//     to it — and its in-app arm writes the inbox row the hub wakes on.
//
// # What notification deliberately does NOT own
//
// The module never imports authn, rbac or org, and has no table in any of
// their domains. A user is an opaque id learned from an authenticated
// caller or a domain event; a tenant's organization structure is the same.
// Notification's own model files cite this for every id they store.
//
// Business modules never depend on notification either: they publish
// domain events, and notification's consumption of them is the host's
// wiring, never the module's — a host subscribes to its own events and
// calls Deliveries().Dispatch, while this module's Register subscribes to
// nothing but its inbox-created event for the local hub fan-out. That
// direction is what keeps the module a leaf of the dependency graph rather
// than a hub every other module must import.
//
// # Tenant and recipient model
//
// Inbox rows are tenant data: every row is bound to exactly one tenant,
// and a person who belongs to several tenants has one inbox per tenant,
// never one inbox shared across them. External recipients (a patient of a
// dental group, say) exist only inside the tenant that verified them.
//
// # Status
//
// Implemented end to end: the in-app inbox and its stream, the per-type
// channel preference matrix, the external-contact consent ledger with
// double opt-in and business attestation, the async delivery pipeline with
// its per-attempt send records, the platform-blacklist table (schema and
// read path — the writers that feed it are deferred), the module's own
// OpenAPI fragment with a generated, compile-checked HTTP handler, and the
// dual-dialect migration set. The reference app is the mandatory first
// consumer of the whole surface. What this round deliberately does not
// ship is recorded, complete and honest, in go/notification/AGENTS.md's
// "Deferred to later rounds" section.
package notification
