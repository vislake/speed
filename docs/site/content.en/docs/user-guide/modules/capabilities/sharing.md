---
title: sharing
description: "Public share links: a controlled, fully logged, revocable entry point that lets an unauthenticated external visitor view one internal resource — with five mandatory security rules enforced in code."
weight: 3
---

# sharing

sharing is speed's public-share module: a controlled entry point that
lets an unauthenticated external visitor view one internal resource —
a patient viewing their own simulation result, a customer viewing a
report, an anonymous one-time result page. The module holds no bytes:
a share names a `ResourceRef`, and the host's `ResourceResolver` seam
turns a granted share into actual content.

## What it is for

`Service.Create` mints a share — drawing a 256-bit `crypto/rand`
bearer token, hashing an optional password, resolving the expiry —
and returns the raw token **exactly once**. `Access` (authenticated
host call) and `AccessPublic` (genuinely anonymous visitor, tenant
resolved from the token alone) resolve a token into the share it names
or refuse; `Revoke` withdraws one with immediate effect; `Get`/
`List`/`ListAccessLog` serve the owner's own view, including who
viewed a share and how many times. An expiry sweep
(`Module.EnqueueExpirySweep`, a `jobs` task a host schedules) marks
expired or view-exhausted shares and refunds interrupted view
reservations. Two HTTP surfaces ship in one fragment: `PathAccess`
(`GET /api/v1/sharing/access` — genuinely public, `Cache-Control:
no-store` on every response, password over the `X-Sharing-Password`
header) and `PathShares` (five owner-facing operations: create, list,
get, revoke, access log).

The module enforces five mandatory rules, each with a passing test:
tokens are cryptographically random; no share can ever be
never-expiring (a `Forever` request is refused, an explicit expiry is
validated against a ceiling, and a nil one resolves to the tenant's
configured default, 30 days when unconfigured); revocation takes
effect on the very next access check with **no caching anywhere**;
every access — granted or denied — lands in the access log; and every
refusal reason (unknown token, revoked, expired, view-exhausted,
missing or wrong password) answers the identical
`sharing.not_accessible`.

What it is **not**: no browser-facing password-entry page (the route
accepts a password header; rendering an HTML prompt is host frontend
work); no generic sensitivity classification (`CreateParams.Sensitive`
is a caller-supplied flag that fires the `sharing.share.create_sensitive`
audit action); no CDN-safe posture of its own — revocation is only
immediate if the share page and the resource declare `no-store` and
never sit behind a CDN, which is a deployment decision the module
cannot enforce, only warn about.

## When to choose it

A resource inside your product must be reachable by someone with no
account: a patient's result, a customer's report, a one-time download.
The five rules exist because this surface is the most leak-prone part
of a SaaS — forced expiry, instant revocation, full logging and
outward-identical refusals are the minimum a share link should meet.
Pair it with [storage](/docs/user-guide/modules/services/storage/) for
byte-holding resources, and with [compliance](/docs/user-guide/modules/capabilities/compliance/) when the
shared thing is a data export.

## Wiring it in

```go
m := sharing.NewModule(db,
    sharing.WithResourceResolver(storageResolver), // turns ResourceRef into bytes
    sharing.WithQueue(queue),                      // arms the expiry sweep
    // optional: sharing.WithTenantConfigReader(cfg) for a tenant-tunable default
)
svc := m.Service()

one := 1 // MaxViews is *int; nil = unlimited views
res, err := svc.Create(ctx, sharing.CreateParams{
    ResourceRef: objectID, // opaque to sharing; your resolver's vocabulary
    MaxViews:    &one,
    Sensitive:   true,     // fires the sensitive-create audit action
})
// res.Token is returned exactly once — send it to the viewer:
// https://your.host/api/v1/sharing/access?token=<res.Token>
```

A host's `ResourceResolver` implementation typically adapts
`storage.ObjectService.OpenContent` (the reference app's resolver is
exactly that composition). The public route must be allowlisted in
your `tenancy.Middleware` (`sharing.PathAccess`, GET only) and the
owner-facing path gated like any other module surface — the module
performs no authorization of its own on the five operations. The
anonymous entry point is where the view is consumed *on delivery*: the
access route records a limited share's view only after the full body
reached the viewer, running the same reserve → confirm/refund shape
the credits ledger uses, so a broken first attempt never spends a
single-view share.

## Core concepts and API surface

- **Only the token's SHA-256 hash is stored.** A leaked database backup
  yields no usable link; lookups are hash comparisons.
- **`AccessPublic` resolves the tenant from the token itself** through
  a deliberately narrow platform-data table (`shareTokenIndex`,
  `AssertNotTenantScoped`) — the one thing an anonymous caller cannot
  supply — then re-enters the ordinary `Access`, so every rule's
  enforcement point stays exactly where it is. No system-context escape
  hatch is involved.
- **`Share.ExpiresAt` is never nil on a row**: a nil request resolves
  to the default, an explicit value must lie within the operative
  ceiling (`MaxExplicitShareLifetime`, 30 days, raised to a tenant's
  configured default when longer), and a tenant's own default is
  honored unchanged.
- **Denied refusals are cheap; recognized ones are equalized.** An
  unknown token is refused with one index lookup and no argon2id burn
  (a scanner-amplification guard, timing-pinned); every recognized-token
  refusal path pays the same constant-time password check. The one
  outward difference: a spent per-token wrong-guess budget answers
  `sharing.rate_limited`, disclosing that the token exists — recorded
  honestly as the accepted cost of pacing guessers.
- **Access logging is write-atomic with the view count.** A granted
  access whose log row cannot commit fails the access (`sharing.internal_error`)
  rather than leaving a trail-less grant; overlong log metadata is
  truncated at the write boundary.
- **Coded errors** — `sharing.not_accessible`,
  `sharing.expiry_out_of_range`, `sharing.rate_limited`,
  `sharing.resource_unavailable` and the rest — are indexed in the
  [error code index](/docs/user-guide/error-codes/#sharing).

## Limitations and links

- `clientIP` trusts only the direct connection's `RemoteAddr`, never
  `X-Forwarded-For`; a host behind a trusted reverse proxy normalizes
  the address itself.
- A `MaxViews`-limited share serves one viewer at a time: while a
  delivery is in flight, a concurrent fetch is refused — an unlimited
  share (or one share per recipient) is the shape for simultaneous
  viewers.
- The access log's append-only discipline is a module convention, not
  a database backstop: the table must stay erasable for a compliance
  regime's retention and erasure paths. The module registers a
  retention participant (`sharing.access_log`) so a host's compliance
  sweep reaps old entries.
- No frontend ships for share management (`@speed/sharing-ui` or a
  host page); the reference app drives both HTTP surfaces directly.

### Source

- [go/sharing/AGENTS.md](https://github.com/vislake/speed/blob/main/go/sharing/AGENTS.md) — the authoritative document (the five rules, serving protocol, tenant resolution, limitations)
- Related pages: [storage](/docs/user-guide/modules/services/storage/), [compliance](/docs/user-guide/modules/capabilities/compliance/), the domain guide [Storage, sharing and AI](/docs/user-guide/domains/storage-sharing-and-ai/)
