---
title: notification
description: "Outbound messaging: the in-app inbox, email and SMS delivery through declared notification types, with consent-verified external recipients and per-type channel preferences."
weight: 2
---

# notification

notification is speed's outbound-message module: it delivers a
tenant's notifications to the people it serves — users through the
in-app inbox, email and SMS, and external contacts only where consent
has been verified first.

## What it is for

Every message your product sends goes through a **declared
notification type**, and the type is declared by the business module
that emits it — `reg.NotificationsSeat().Add` during its own `Register` —
never stored here as a template. A declaration carries the type's
preference group, default channels, opt-out eligibility (verification
codes are transactional) and recipient-visible parameters; the copy
lives in the declaring module's own bilingual locale bundles under the
`<type_key>.<channel>.<part>` id convention and is rendered at
delivery in the recipient's locale. The module's own tables hold no
identity data: a user is an opaque id, and a user recipient's
outbound addresses are read at send time through the host's
`UserAddressResolver` module.

Delivery is asynchronous by construction. `Dispatch` validates and
enqueues one job per recipient per channel; the worker rebuilds tenant
context, re-checks preferences, consent and addresses **at send
time** — nothing that can change between enqueue and delivery is
frozen into the payload — renders the copy, delivers (inbox row,
email, SMS) and settles one `send_records` row per attempted channel.
The in-app inbox is the first-class channel: zero external
dependencies, realtime announcements through the SSE stream
(`GET /api/v1/notifications/stream`), row-then-event ordering.

What it is **not**: not the owner of your product's notification
types (that is your business module's declaration), not a
template-editing console, not an email provider — and it never imports
`authn`, `rbac` or `org`.

## When to choose it

You send messages of any kind — transactional (verification codes,
alerts), event-driven, or preference-routed (per-type × per-channel
choices, defaults from the declaration). You need an in-app inbox
with unread counts; you need to reach people who are **not** users of
any tenant (a consent ledger with double opt-in or business
attestation); you need delivery outcomes auditable per attempt. If
your "messages" are actually outbound HTTP to customer systems, that
is integration's webhook half, not this module.

## Wiring it in

Six options are required — `Register` fails with its own named
`Err*Required` when any is missing:

```go
m := notification.NewModule(db,
    notification.WithSMSSender(pkgcore.NewConsoleSMSSender()), // pkgcore's SMS module — console, HTTP gateway, or a carrier adapter
    notification.WithMailFrom("no-reply@example.com"),
    notification.WithContactEmailIndexer(emailIndexer),   // dbkit.NewBlindIndexer over notification.AddressIndexColumn
    notification.WithContactPhoneIndexer(phoneIndexer),   // index keys must differ from the cipher key
    notification.WithDeliveryQueue(queue),                // a jobs.Queue the delivery handler drains
    notification.WithUserAddressResolver(myResolver),     // reads a user's verified outbound addresses
    // optional: WithSubjectResolver — caller identity for the HTTP surfaces
)
```

The host also subscribes business events to `Deliveries().Dispatch`
(host glue — the module subscribes to nothing but its own event). The
service accessors are `Preferences()`, `Contacts()` and
`Deliveries()`; the HTTP surface is eleven operations under
`/api/v1/notifications` (inbox reads and mark-read, unread count,
type directory, preference get/update, contact roster
create/verify/resend) plus the hand-mounted stream.

## Core concepts and API surface

- **The preference matrix.** Per-type × per-channel rows
  (`notification_preferences`); absence resolves to the type's
  declared defaults. Writes validate against the live type registry —
  an unknown type or channel is refused, never stored unreachable.
  Opt-out is terminal per type.
- **Consent for external contacts.** `VerifiedContact` is one
  consent-gated address on one channel (email or SMS — contacts are
  never in-app), encrypted at rest under a blind-indexed column.
  Consent arrives by double opt-in (a 6-digit code, valid 5 minutes,
  stored as its SHA-256 hash on the row, `VerifyCode` a
  compare-and-swap) or business attestation (`ConsentRef`).
  `unsubscribed` and `bounced` are terminal states every delivery
  refuses; a per-type opt-out narrows one verified contact out of one
  type while keeping the rest. Verify attempts pay a per-address
  budget before the code is judged.
- **Re-checked at send time, always.** Preferences, consent and
  addresses are read by the delivery job, never trusted from the
  payload; a permanent transport failure (`pkgcore.ErrTransportPermanent`,
  the sentinel the transports themselves wrap) marks the tenant's
  contact `bounced`.
- **The outcome log.** One `send_records` row per attempted channel
  under a UNIQUE `(tenant_id, idempotency_key)` index — `succeeded`
  only after the transport accepted, `failed` with a bounded
  classification (never raw transport text), `skipped` with a short
  reason. At-most-once holds across one delivery's retries; the
  crash-between-transport-and-record window is recorded, not designed
  around.
- **Locale discipline.** A user delivery carries the recipient's
  required locale; a missing template or unknown locale is a coded
  internal error, never a fallback to another language. (External
  contacts render in the platform default locale.)

## Limitations and links

- No platform-blacklist writers, no tenant-enforced preference tiers,
  no same-type aggregation or delivery rate limiting, no admin
  template editing, no `@speed/notification-ui` frontend — each
  recorded with its reason in the module's `AGENTS.md`.
- The inbox stream sends no heartbeat; the user-recipient half
  delegates one safety obligation to the host by contract (the
  resolver must return verified addresses — the module cannot detect
  a host that serves unverified ones).
- Coded errors: see the [error code
  index](../../../error-codes/#notification) — the preference group, the
  contact group (code, consent, bounce, rate-limit refusals), the
  dispatch and inbox groups, and the six `Err*Required` wiring
  sentinels.

### Source

- [go/notification/AGENTS.md](https://github.com/vislake/speed/blob/main/go/notification/AGENTS.md) — the authoritative document (delivery pipeline, consent machine, host modules, rules, not-implemented list)
- Related pages: [Platform services](../), the domain guide [Jobs and notifications](../../../domains/jobs-and-notifications/)
