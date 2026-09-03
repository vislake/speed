// Package notification delivers a tenant's notifications to the people it
// serves, on the channels those people want, and — for contacts who are
// not users of any tenant — only where consent has been verified first.
// Its design lives in docs/internal/07-platform-services.md.
//
// # What notification owns
//
//   - The in-app inbox: per-tenant rows in in_app_messages that a user
//     reads inside the product. This is the module's first-class channel —
//     it has zero external dependencies, so it works in every deployment
//     composition, and it is the only channel that is fully built in this
//     round (see "Current status" below).
//   - The per-type channel preference matrix that decides which channels a
//     type of notification may use, and the consent ledger that decides
//     whether a given external recipient may be messaged at all.
//   - Delivery: the subscriber that turns a published domain event into one
//     rendered, deduplicated, rate-limited message per recipient, per
//     selected channel.
//
// # What notification deliberately does NOT own
//
// The module never imports authn, rbac or org, and has no table in any of
// their domains. A user is an opaque id learned from an authenticated
// caller or a domain event; a tenant's organization structure is the same.
// Notification's own model files cite this for every id they store.
//
// Business modules never depend on notification either: they publish
// domain events, and notification subscribes. That direction is what keeps
// the module a leaf of the dependency graph rather than a hub every other
// module must import.
//
// # Tenant and recipient model
//
// Inbox rows are tenant data: every row is bound to exactly one tenant,
// and a person who belongs to several tenants has one inbox per tenant,
// never one inbox shared across them. External recipients (a patient of a
// dental group, say) exist only inside the tenant that verified them.
//
// # Current status
//
// This block of the round ships the module skeleton and the in-app inbox's
// storage: the InboxMessage model, its dual-dialect migration, the
// Repository that reads and writes inbox rows, and the module's first
// declared domain event (notification.inbox.created). The preference
// matrix, the consent ledger and the delivery subscriber are the round's
// next blocks, built on exactly this storage.
package notification
