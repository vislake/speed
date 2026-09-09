---
title: integration
description: "A tenant's outward-facing API: API keys issued to scripts and partners, three-layer rate limiting, and outbound webhooks that deliver internal events as signed HTTP calls."
weight: 4
---

# integration

integration is a tenant's outward-facing API surface: API keys a
tenant issues to its own scripts and third-party systems, the
three-layer rate limiting that protects the platform, the tenant and
the individual key from one another, and outbound webhooks that turn a
business module's internal domain events into HMAC-signed HTTP
deliveries.

## What it is for

**API keys.** `Service.Create` generates a raw key and returns it to
the caller exactly once; only its SHA-256 hash and a plaintext display
prefix are ever stored. Every key carries a mandatory, forced expiry (the host's
`WithMaxAPIKeyLifetime`, one year by default — a never-expiring key is
refused, not clamped). `Rotate` issues a replacement and revokes the
predecessor; `Revoke` marks; `Authenticate` resolves a presented raw
key to its owning tenant from the hash alone, refusing with one
outward-identical `ErrAuthenticationFailed`. Scopes are frozen at
issuance (validated against the creator's current permissions through
the `PermissionLister` seam) and never re-derived.
`LayeredLimiter` composes global/tenant/key rate-limit layers;
`HTTPGuard` translates a denial into a 429 with `Retry-After` and
`X-RateLimit-*` headers. `AuthMiddleware` transports `Authenticate`
over HTTP: the bearer credential rides in the `X-API-Key` header
(never `Authorization`, which the session layer claims first).

**Outbound webhooks.** A tenant subscribes (`WebhookSubscription`) to
public event types; `EventMapping` declarations, wired by the host at
construction, map a business module's internal event to a versioned
public schema — internal events are never forwarded raw. On a match
the module fans out to the tenant's active subscriptions, computes the
rendered body once, and enqueues one delivery job per subscription
(6 retries, then dead-letter). Every delivery is signed
`v1=HMAC-SHA256(secret, "<timestamp>.<body>")` with the timestamp
inside the covered content, and SSRF is defended twice: at creation
and again at dial time on every attempt — which is what defeats DNS
rebinding. `DeleteWebhookSubscription` marks rather than removes; a
restore always lands the subscription **paused** (`Active = false`) —
resuming outbound HTTP to a third-party URL must be an explicit act.
`RedeliverWebhookDelivery` re-enqueues a dead-lettered delivery
(Service-level).

What it is **not**: not a production inbound gateway — scope
enforcement over `AuthenticatedAPIKey.Scopes` is deliberately left to
your host; no per-tenant delivery-volume limits on the webhook half;
and the CRUD surface is session-authenticated tenant administration,
deliberately not gated by `HTTPGuard` (which guards
key-authenticated traffic, not key management).

## When to choose it

Your tenants need machine access: scripts, partner systems, CI — any
caller that authenticates with a long-lived secret rather than a user
session — with per-key rate limits and revocation. You need to deliver
your platform's events to customers' endpoints with signatures they
can verify, and you want the internal event schema kept versioned and
private.

## Wiring it in

```go
m := integration.NewModule(db,
    integration.WithWebhookQueue(queue), // a jobs.Queue the delivery handler drains
    integration.WithEventMapping(integration.EventMapping{
        InternalType:  "org.member.joined",          // the business event this host published
        PublicType:    "org.member.joined",          // what subscribers name in EventTypes
        PublicVersion: "v1",                         // breaking changes ship as new mappings
        Transform:     transformMemberJoined,        // func(ctx, pkgcore.Event) (json.RawMessage, error) — host-owned
    }),
    // validates requested scopes against the creator's permissions
    // (rbac's Authorizer implements the same shape):
    integration.WithPermissionLister(func(ctx context.Context, tenantID, userID string) ([]string, error) {
        return az.ListPermissions(ctx, rbac.Subject{TenantID: pkgcore.TenantID(tenantID), UserID: userID})
    }),
    // optional: WithMaxAPIKeyLifetime, WithSubjectResolver, WithMembershipChecker, WithAuthenticationGuard
)
```

The HTTP surface — ten operations under `/api/v1/integration`
(apikey create/list/rotate/revoke; webhook
list/create/update/delete/restore plus the deliveries log) — is a thin
translation of the `Service` methods above. Timing: `Register`
subscribes the mappings and claims the delivery handler; the `Service`
is built later, in `Attach`, after `Bootstrap` returns.

## Core concepts and API surface

- **The key material is never stored** — hash-only lookup, prefix
  display; the webhook `Secret` is the one reversible exception (every
  delivery re-derives the HMAC from it).
- **Outward-identical refusals.** `Authenticate` never distinguishes
  why a key failed, and revoke, rotate and the webhook CRUD collapse
  "never existed" and "belongs to another tenant" into one not-found.
- **Fan-out is idempotent.** The delivery row carries a derived
  idempotency key (subscription, public type/version, rendered body,
  occurrence), the queue task the same key, and the database a UNIQUE
  backstop — an at-least-once bus never double-delivers one
  occurrence.
- **Rate limiting is ordered.** Global, then tenant, then key, with
  short-circuit on the first denial; a zero-value layer is disabled; a
  limiter error is a 500, never a silent allow.
- **The permission split** is `integration:apikey:*` vs
  `integration:webhook:*` (read/manage each); the audit actions cover
  key create/revoke and the webhook CRUD quartet.

## Limitations and links

- `Rotate` is two writes, not one transaction — a failure between
  them leaves two live keys (a safe-direction surplus, reported, never
  a lockout).
- The API-key expiry sweep (`EnqueueAPIKeyExpirySweep`) is
  host-scheduled optional work — correctness never depends on it
  (`Authenticate` checks expiry per call; the sweep reclaims disk).
- Delivery is not exactly-once on the send side: the
  crash-after-accept window re-sends; a delivery's payload is computed
  once at fan-out, so a retry never picks up a mapping fix.
- The two override seams (`WithWebhookURLValidator`,
  `WithWebhookHTTPClient`) exist for offline tests only — a
  production host must never wire them.
- Coded errors: see the [error code
  index](../../error-codes/#integration) — `integration.authentication_failed`,
  `integration.rate_limited`, `integration.webhook_url_blocked`,
  `integration.scope_not_held_by_creator` and the rest.

### Source

- [go/integration/AGENTS.md](https://github.com/vislake/speed/blob/main/go/integration/AGENTS.md) — the authoritative document (key lifecycle, seams, webhook pipeline, adjudications, limitations)
- Design rationale: [docs/internal/07-platform-services.md](https://github.com/vislake/speed/blob/main/docs/internal/07-platform-services.md)
- Related pages: [Platform services](../), [pki](pki/), [notification](notification/)
