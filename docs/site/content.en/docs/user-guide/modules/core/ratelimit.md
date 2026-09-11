---
title: ratelimit
weight: 7
description: "The shared KVStore-backed rate-limiting primitive — one sliding-window counter dimension per Allow call, no tenant or HTTP semantics, composed by the caller."
---

# ratelimit

speed's shared rate-limiting primitive: a `Limiter` that decides
whether one more hit against a caller-supplied `key` is within a
caller-supplied `Limit`, backed entirely by `pkgcore.KVStore`. It is a
**pure library** — unlike every other module in the platform it does
not implement `the module contract`: nothing to register, nothing to wire
into a Kernel, a consumer just calls `ratelimit.New(store)`. It exists
because six otherwise-unrelated modules each need rate limiting —
`authn`'s login brute-force guard, `integration`'s three-layer API-key
throttling, `notification`'s verification-code budgets, `org`'s
invitation-delivery limits, `ai-gateway`'s per-tenant request limits,
`sharing`'s public-link abuse guard — and one shared primitive against
the one interface every deployment mode provides beats six slightly
different re-implementations.

## When to choose it

Whenever you must bound how often something happens, per some key your
business logic defines. The limiter deliberately knows nothing about
your domain: no tenants, no HTTP, no accounts, no IPs — those are all
just `key` strings you choose. Want per-tenant limiting? Build the
tenant into the key (`"ai-gateway:tenant:" + tenantID`). Want
IP-plus-account? Call `Allow` once per dimension and deny when either
refuses — multi-dimension composition is the caller's, by design.

## Wiring and minimal use

```go
limiter := ratelimit.New(store) // store: pkgcore.KVStore — memory, Redis, ...

byAccount, err := limiter.Allow(ctx,
    "authn:login:account:"+accountBlindIndex, // the identifier's blind index, never the plaintext
    ratelimit.Limit{Rate: 5, Per: time.Minute})
if err != nil {
    return false, err // a KVStore failure — you decide fail-open or fail-closed
}
byIP, err := limiter.Allow(ctx, "authn:login:ip:"+ip,
    ratelimit.Limit{Rate: 20, Per: time.Minute})
if err != nil {
    return false, err
}
return byAccount.Allowed && byIP.Allowed, nil
```

`Allow` is the entire surface: every call is a hit (there is no
"check without recording" mode), and `Decision{Allowed, Remaining,
ResetAfter}` is plain data — translating a denial into a 429 with
`Retry-After` and quota headers is your HTTP layer's job, using the
one protocol helper the package ships, `RetryAfterSeconds(remaining)`
(rounds up, so a sub-second tail never reads as "retry immediately").

## Core concepts and API essentials

- **The algorithm** — a sliding-window *counter*, not a log: time is
  partitioned into fixed windows of `limit.Per`, each window is one
  KVStore key, and a decision weighs the current window's count plus
  the previous window's count decaying by elapsed time —
  `current + previous*(1-elapsed)`. That smooths the classic
  fixed-window hole where a boundary-straddling burst could get up to
  2x the configured rate. Recording the hit is one atomic
  `IncrByFloatWithTTL` call (the ttl attaches on the call that
  creates the key, never extending a live one) — the race a
  caller-side increment-then-`Set` sequence would reopen is closed by
  the primitive, which is exactly why `KVStore` gained it.
- **Limits and validation** — `Limit{Rate, Per}` requires `Rate >= 2`
  and a positive `Per`: a `Rate` of 1 is refused outright with the
  coded `ratelimit.rate_one_unsupported` (wrapping
  `ErrInvalidLimit`), because under the weighted formula a rate of 1
  collapses into "allowed once ever, then denied forever" — refused
  before the store is touched, never silently delivered as a
  permanent lockout. Do not spell "once a day" as `Rate: 1`; a
  caller needing a hard exactly-once cap builds its own last-success
  gate.
- **The store contract** — `Allow` returns any `KVStore` error
  unmodified, including a cancelled context's; it never decides
  fail-open or fail-closed on your behalf, so each call site makes
  its own choice for its own risk. Windows advance by the calling
  process's wall clock — replicas sharing one KVStore describe one
  coherent window only while their clocks agree.

## Boundaries and pitfalls

- Keys land verbatim in the KVStore: an email or phone number in a
  key is PII at rest. Key by blind indexes (via `dbkit.NewBlindIndexer`)
  where the identifier is sensitive.
- There are no progressive or escalating semantics — authn's
  "delay grows with failures up to a lockout" is business logic
  layered on the plain counts, not a mode of this package.
- `Decision.Remaining` is a weighted approximation, floored at zero:
  it can read `0` on a call whose `Allowed` was `true`. Do not treat
  it as an exact countdown.
- Documented convergent behavior, not a bug: a client that saturates
  its `Rate` every window is admitted at most `Rate - 1` per window
  from the second window on (every call is a hit, including denied
  ones). The limiter only ever admits less than configured — the
  safe direction — but a sustained-saturation consumer (an API-key
  throttle, say) should know the shape.
- No dynamic configuration: `Limit` values are per call site in
  code. There is no schema-backed, operator-tunable setting — that
  would require the module machinery this library deliberately
  lacks.
- Do not give `Limiter` a second method, an HTTP-specific
  `Decision`, or a multi-key `Allow`; do not make it a
  `the module contract`; do not read tenant context inside it. One
  dimension per call, composed by the caller, is the whole design.

## Source

- [ratelimit AGENTS.md](https://github.com/vislake/speed/blob/main/go/ratelimit/AGENTS.md)
