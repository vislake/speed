# ratelimit

ratelimit is speed's shared rate-limiting primitive: a `Limiter` that decides whether one more hit against a caller-supplied `key` is within a caller-supplied `Limit`, backed entirely by `pkgcore.KVStore`. It sits at the same graph depth as `dbkit`, `observability` and `tenancy` — directly above `pkgcore`, depending on nothing else (`pkgcore -> dbkit / observability / ratelimit -> tenancy -> ...`, see `docs/internal/01-architecture.md`) — and is a **pure library**: unlike every other module in this repository, it does not implement `pkgcore.Module`. It registers no routes, no config schema, no feature flags, no permissions. There is nothing to wire into a Kernel; a consumer just calls `ratelimit.New`.

This module exists because six otherwise-unrelated business modules each independently need rate limiting — `ai-gateway`'s per-tenant request-rate limit (independent of its credit-based cost limits), `authn`'s login/registration/password-reset brute-force guard (always by IP, plus an endpoint-specific second dimension such as account, email or phone number), `integration`'s three-layer API key throttling (global + tenant + key), `notification`'s contact verification-code send and verify budgets (per address and per tenant, the send window a day and the verify window one code lifetime so wrong guesses stay bounded while a code is live; the per-address keys carry the address's blind index, never the plaintext), `org`'s invitation-delivery limits (per tenant and per recipient, the latter keyed by the recipient address's blind index), and `sharing`'s public-link abuse guard — and re-implementing the same sliding-window counter six times, slightly differently each time, is worse than building it once against the one interface every deployment mode already provides: `pkgcore.KVStore`. See `docs/internal/11-cross-cutting.md`'s rate-limiting section for the full design discussion this module implements, including why it was kept out of `pkgcore` itself (a shared primitive only *some* consumers need does not belong in the dependency floor every module carries).

| Concern | Where |
|---|---|
| `Limiter`, `Limit`, `Decision`, `ErrInvalidLimit`, `New`, and the sliding-window-counter algorithm itself (`slidingWindowLimiter`, `Allow`, and its private helpers) | `limiter.go`, `limiter_sliding_window.go` |
| Package doc comment | `doc.go` |
| Runnable usage documentation (`Example`, `ExampleLimiter_multipleDimensions`, `ExampleErrInvalidLimit`) | `example_test.go` |

**One dependency, and why there is only one.** ratelimit imports exactly one other speed module: `pkgcore` — its root package for `pkgcore.KVStore`, plus the `pkgcore/apperr` subpackage for exactly one deliberately-coded error, `ErrRateOneUnsupported` (same module, no new `go.mod` entry). Every other error this package returns is a plain `errors.New` sentinel (matching how `pkgcore`'s own `kv.go` and `tenant.go` define theirs) or a KVStore-failure passthrough, since those are programmer/caller errors and transport failures, not API responses this package shapes itself; the Rate-1 refusal is the single exception that needs a machine-readable code, because a host whose configuration it refuses must be able to tell "Rate of 1" apart from the generic invalid limit — the same one-coded-refusal reasoning the jobs module's `WithConcurrency`/`WithWorkerCount` options apply at option time (see Error index). No `tenancy`, no third-party package beyond the Go standard library. This is architectural, not incidental minimalism, and it is what "no business semantics" means concretely:

- ratelimit does not know what a tenant is. A caller that wants per-tenant limiting builds the tenant into `key` itself (e.g. `"ai-gateway:tenant:" + tenantID`) before calling `Allow` — this package never reads `pkgcore.TenantFromContext` or anything tenancy-shaped.
- ratelimit does not know what HTTP is. `Decision` is plain data (see Rules, below); translating a denial into a 429 with `Retry-After` and quota headers is entirely the caller's job. The one protocol-vocabulary helper the package ships, `RetryAfterSeconds`, is a pure duration-to-whole-seconds conversion with no protocol behavior of its own — it does not build headers or responses, and a non-HTTP consumer of `Decision` (say, a background-job throttle) never needs it.
- ratelimit does not know what "account", "IP", "email" or "API key" mean. Every one of those is just a `key` string a consumer chose; combining several dimensions (authn's IP-plus-account-or-contact guard, integration's global+tenant+key layers) is composition the *caller* does by calling `Allow` once per dimension, never something this package models. See `ExampleLimiter_multipleDimensions` in `example_test.go`.

Beyond `pkgcore`, ratelimit adds **zero** dependencies — no test-only ones either: unit tests use `pkgcore.NewMemoryKVStore()` directly, and there is deliberately no `integration_test/` tier (see Testing, below).

## The algorithm, and why it is a sliding-window *counter* rather than a sliding-window *log*

Time is partitioned into fixed windows of length `limit.Per`. An instant's window index is `t.UnixNano() / int64(limit.Per)` — safe once `Limit.validate` has ruled out `Per <= 0`, since `Per` is already an `int64` count of nanoseconds. Each window gets its own storage key (`windowKey`: the caller's `key`, `":"`, the window index in base 10), so the current window and the immediately preceding one are two distinct keys, and `Allow` reads both.

**The windows are the calling process's own local wall clock.** `Allow` reads `time.Now` at the moment of the call and derives the window index and phase from it, and `New` accepts no clock to substitute (the package is dependency-free by design — which is also why the unit suite has to wait for real window boundaries rather than drive a fake clock; see `waitForFreshWindowStart` in `limiter_sliding_window_test.go`): windows advance by the process's local wall clock, never by a time the store or a peer supplies. That clock source is the limiter's own behaviour, not a policy some other layer chose, and in a deployment where replicas share one KVStore it makes the limiter's cross-replica behaviour depend on the replicas' clocks agreeing — each replica buckets its hits and weights them against boundaries of its own clock, so the shared counters describe one coherent sliding window only while the replicas' clocks agree, and a replica whose clock disagrees writes its hits into keys its peers read as a different window and reads their counters through shifted boundaries. The agreement is exercised for real in this repository: `examples/reference-app/integration_test/distributed_mode_test.go` is a two-process composition of this limiter — two replicas sharing one Redis-backed KVStore — whose cross-replica lockout assertions rely on it: they expect rate-limiting state one replica recorded under its own process clock to hold when the other reads it under its own, which holds because both processes share one host clock.

This shape — one counter per fixed window, not one entry per request — is forced by `pkgcore.KVStore`'s own contract, not a shortcut. A sliding-window *log* needs an ordered, range-queryable structure (typically a sorted set) so individual request timestamps can be trimmed and counted by range; `KVStore` deliberately offers nothing beyond opaque byte values plus `IncrByFloat` and `CompareAndSwap` (see `go/pkgcore/kv.go`'s own doc comment: it is designed against the weakest backend it must run under — an in-memory map in the standalone deployment mode — so it cannot grow a data type only Redis could satisfy without breaking that symmetry). A counter-per-window needs only "increment" plus "an expiry that delimits the window", and `IncrByFloat` plus `Set` already provide exactly that between them:

- **Recording a hit** calls `KVStore.IncrByFloatWithTTL` on the current window's key, unconditionally, before `limit` is even consulted for the decision, passing a ttl of `2 * limit.Per` on every single call. `IncrByFloatWithTTL` atomically increments the key **and** attaches that ttl in the same step, but only on the call that creates the key — a key that already exists is incremented with its own existing expiry left untouched, mirroring `IncrByFloat`'s own non-extension rule (its own doc comment: a fresh key "starts from zero and is stored without an expiry"; `IncrByFloatWithTTL`'s starts from zero with the given ttl instead). No caller-side gate singles out "the first hit in this window": the ttl attaches on the creating call and only on it — see Known limitations, below, for the two-call alternative's failure modes.
- **Reading the previous window's count** is a plain `KVStore.Get`: absent (never existed, or already expired) reads as zero either way. The stored bytes are parsed with `strconv.ParseFloat`, matching `IncrByFloat`'s own documented "shortest exact decimal encoding, which callers should parse ... rather than compare as text" — never a string comparison.
- **The two counts are combined** into `weighted = currentCount + previousCount*(1-elapsedFraction)`, where `elapsedFraction` is how far the current instant has moved into the current window (0 at its start, approaching 1 at its end). This is what makes it a *sliding* approximation: right after a window boundary, almost all of the previous window's count still counts against the limit, closing the classic fixed-window hole where a burst timed to straddle a boundary could otherwise get up to 2x the intended rate through. `limiter_sliding_window_test.go`'s `TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed` is a case built specifically to fail under a naive fixed-window implementation and pass under this one.
- `Decision.Allowed` is `weighted <= limit.Rate` **after** this call's own increment — the request that first pushes the count over the limit is the one denied, not the request after it. `Decision.Remaining` is `limit.Rate - weighted`, floored at zero and truncated (not rounded) to an `int`. `Decision.ResetAfter` is `limit.Per` minus the elapsed time into the current window.

`pkgcore.KVStore.CompareAndSwap` is not used by this algorithm: a sliding-window counter needs only increment-plus-read. The interface method exists for consumers that want compare-and-swap semantics of their own; this module does not reach for it speculatively.

## Public API — `limiter.go`, `limiter_sliding_window.go`

| Signature | Purpose |
|---|---|
| `type Limit struct { Rate int; Per time.Duration }` | `Rate` occurrences allowed per `Per`. `Rate` must be at least 2 and `Per` strictly positive (and no larger than `maxPer`); a `Rate` of 1 is refused — see `ErrRateOneUnsupported` |
| `type Decision struct { Allowed bool; Remaining int; ResetAfter time.Duration }` | Plain data, no protocol awareness — see Rules |
| `type Limiter interface { Allow(ctx context.Context, key string, limit Limit) (Decision, error) }` | The entire decision surface. Every call is a hit; there is no separate "check without recording" mode |
| `func New(store pkgcore.KVStore) Limiter` | Returns a `Limiter` backed by `store`, hiding the concrete `slidingWindowLimiter` type behind the interface — the same pattern `pkgcore.NewMemoryKVStore` itself uses. Performs no I/O |
| `func RetryAfterSeconds(remaining time.Duration) int` | The one conversion this module's consumers share: a remaining wait (typically a denied `Decision.ResetAfter`) to the whole-second count a `retry_after_seconds` parameter or `Retry-After` header carries. Rounds **up** (a sub-second remainder — the ordinary tail of an exhausted window — must not collapse to the "retry immediately" meaning of 0), floors a negative remainder at 0, and caps an unrepresentable count at `math.MaxInt` (the `clampRemaining` rule). Every consumer of a denied `Decision` writes its retry hint through this function so the conversion's boundary lives in one tested place; per-consumer copies of the conversion drift apart in rounding direction and extreme-value handling (the consumers: authn, integration, sharing, ai-gateway, notification, org) |
| `var ErrInvalidLimit = errors.New(...)` | Wrapped by the error `Allow` returns when `limit.Rate <= 0`, `limit.Rate == 1` (as `ErrRateOneUnsupported`) or `limit.Per <= 0` (or `Per` exceeds `maxPer`). Match with `errors.Is` |
| `var ErrRateOneUnsupported = apperr.Invalid("ratelimit.rate_one_unsupported").WithCause(ErrInvalidLimit)` | The error `Allow` returns for `Rate == 1`, wrapping `ErrInvalidLimit`: a Rate of 1 cannot be honoured literally by the weighted sliding-window formula (see Known limitations), so it is refused rather than silently delivered as a permanent lockout. Match with `errors.Is` or `errors.As` on the code |

`Allow` returns any `KVStore` error unmodified, including a canceled/expired `ctx`'s error (every `KVStore` operation honors `ctx` per its own contract). It never decides fail-open or fail-closed on the caller's behalf — see Allow's own doc comment and Rules, below.

## Typical integration

```go
package authn

import (
	"context"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// LoginGuard illustrates the shape of authn's brute-force guard: two
// independent dimensions -- here account and IP, though the real guard
// combines IP with whichever second dimension fits the endpoint (account,
// email, or phone number) -- combined by calling Allow once per dimension
// and denying the whole attempt if either one is over limit. ratelimit has
// no built-in notion of "dimensions" or of escalating/progressive backoff --
// both are authn's own business logic layered on top of the plain counts
// Allow reports; see this module's doc comment and
// docs/internal/11-cross-cutting.md.
type LoginGuard struct {
	limiter ratelimit.Limiter
}

func NewLoginGuard(store pkgcore.KVStore) *LoginGuard {
	return &LoginGuard{limiter: ratelimit.New(store)}
}

func (g *LoginGuard) Allow(ctx context.Context, accountBlindIndex, ip string) (bool, error) {
	// accountBlindIndex is the identifier's blind index -- an opaque,
	// keyed HMAC digest over the normalized email/phone, computed by the
	// caller before the guard is invoked (authn uses dbkit.NewBlindIndexer)
	// -- never the plaintext identifier itself: the key string lands
	// verbatim in the KVStore, so a plaintext email or phone number in one
	// is PII at rest. See ExampleLimiter_multipleDimensions in example_test.go.
	byAccount, err := g.limiter.Allow(ctx, "authn:login:account:"+accountBlindIndex, ratelimit.Limit{Rate: 5, Per: time.Minute})
	if err != nil {
		return false, err
	}
	byIP, err := g.limiter.Allow(ctx, "authn:login:ip:"+ip, ratelimit.Limit{Rate: 20, Per: time.Minute})
	if err != nil {
		return false, err
	}
	return byAccount.Allowed && byIP.Allowed, nil
}
```

This pattern — one `Limiter`, several independently-keyed `Allow` calls composed by the caller — is compiled and run under CI as `ExampleLimiter_multipleDimensions` in `example_test.go`, alongside the single-dimension walkthrough (`Example`) and the validation walkthrough (`ExampleErrInvalidLimit`), each with an `// Output:` comment asserted against the real printed output.

## Testing

Unit tests (`limiter_test.go`, `limiter_sliding_window_test.go`) use `pkgcore.NewMemoryKVStore()` exclusively — no testcontainers, and deliberately **no `integration_test/` tier at all**, unlike `dbkit` and `tenancy`. A real distributed `KVStore` backend's own correctness (Redis or equivalent) is that backend's own test responsibility, not something `go/ratelimit` re-verifies: this package's only contract is with the `KVStore` interface, which `pkgcore.NewMemoryKVStore()` satisfies fully for test purposes.

Coverage includes: exactly-at-the-limit allowed vs. one-over denied (`TestAllow_ExactlyAtLimit_AllowedThenOneOverDenied`); the sliding behavior specifically, via a boundary-straddling burst that would defeat a naive fixed-window implementation (`TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed`); TTL expiry actually happening, observed directly against the store (`TestAllow_WindowExpiry_OldWindowKeyExpires`); concurrent callers on an already-existing key under `-race`, with a deterministic — not just bounded — expected count (`TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit`); concurrent callers racing to *create* the same fresh key, with both the exact count and the ttl attachment proven under that race (`TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached`); the TTL-attachment race, reproduced deterministically without goroutine timing (`TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost` — see Known limitations below); independent keys never interfering (`TestAllow_IndependentKeys_DoNotInterfere`); context cancellation honored (`TestAllow_ContextCanceled_ReturnsContextError`); the `ErrInvalidLimit` validation path, including the Rate-1 refusal with its coded reason and its before-the-store-is-touched proof (`TestAllow_InvalidLimit_ReturnsErrInvalidLimit`, `TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched`); and `KVStore`-error passthrough for every one of `Allow`'s two possible error-return points, using a small test-local `erroringKVStore` fake (`TestAllow_KVStoreErrors_PropagatedToCaller`).

## Known limitations

**The TTL-attachment race.** Attaching a freshly-created window key's expiry with a caller-side `Get`-then-`Set` sequence — run only on the one call whose increment transitions the key from absent to `1` — would carry two failure modes: a process crash between the increment and the `Set` leaves a window's counter permanently without an expiry (harmless — nothing ever reads a window other than "current" or "immediately previous" again), and, far more significantly, a concurrent caller's own increment landing in the gap between the `Get` and the `Set` is silently overwritten by that `Set` — a real, security-relevant over-admit gap, recurring on every window boundary for as long as a key keeps being hit (`windowKey` mints a brand-new key every single `Per` interval), with no hard bound on how much a single burst could lose.

`pkgcore.KVStore.IncrByFloatWithTTL` removes both failure modes by collapsing "increment" and "attach the window's expiry, but only on the hit that creates the key" into one atomic `KVStore` call — every backend implements it as a single atomic operation (a mutex-guarded map update, a single Lua script, one database-arbitrated upsert, a compare-and-swap retry loop, depending on the backend; see `go/pkgcore/kv`'s own per-backend doc comments), never as a caller-side sequence with a gap for a concurrent increment to land in. `Allow` calls it unconditionally on every hit, passing `2 * limit.Per` as the ttl: the primitive itself ignores that ttl for a key that already exists, so there is no gate to get wrong and no second call for either failure mode to land between. `TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost` in `limiter_sliding_window_test.go` reproduces the two-call interleaving deterministically (it fails against a two-call implementation and passes against this one, since the vulnerable call is unreachable), and `TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached` races hundreds of goroutines to create one fresh window key at once and proves both that no increment is lost and that the key still ends up with its ttl attached.

One narrower residual is worth naming for completeness: a process crash *during* that single atomic call (between the backend applying the increment and durably recording the attached ttl, for a backend whose own atomicity mechanism is not itself crash-atomic at that granularity) could still leave a counter without its expiry — the same "harmless, slightly longer-lived key" outcome as the crash case above, never the concurrency-loss outcome. No backend in this repository has been observed to exhibit this.

**No multi-dimension or progressive/escalating semantics, on purpose.** `Allow` handles exactly one dimension per call — see this file's own intro and `Limiter`'s doc comment. `authn`'s "delay growing with repeated failures, up to a lockout" behavior is business logic built on top of the plain counts `Allow` reports, not a mode this package understands. Do not add a multi-key or escalation-aware variant of `Allow` speculatively; this shape is deliberate, not an oversight to fill in later.

**No dynamic configuration.** `Limit` values are supplied by each call site directly in code; no schema-backed, operator-tunable settings exist here. This is deliberate for the same reason ratelimit implements no `pkgcore.Module`: it is a library, not a component the Kernel assembles.

**A client that saturates its Rate every window converges permanently to Rate-1 admitted per window, not Rate — proven algebraically, not merely observed.** At the Rate-th sequential `Allow` call within any window, `currentCount` is exactly `limit.Rate` (`IncrByFloat`'s own gapless, one-at-a-time increments guarantee this no matter how those calls are timed), so `weighted = limit.Rate + previousCount*(1-elapsedFraction)`. The moment a *previous* window's recorded count has reached `limit.Rate` — which needs only `limit.Rate` calls to have landed in it, whether or not every one of them was itself `Allowed`, since "every call is a hit" applies to denied calls too — that Rate-th call in the *current* window is denied unconditionally: `previousCount*(1-elapsedFraction) > 0` for every `elapsedFraction` in `[0,1)`, because `elapsedFraction` can never reach exactly `1` while a call still belongs to this window. Timing within the window does not save it, however late it lands.

This is self-sustaining, not a one-window blip: the denied call still increments its own window's counter (again, "every call is a hit"), so a window that denies its own Rate-th call still ends with a recorded count of `limit.Rate`, handing the identical `previousCount >= limit.Rate` to the window after it. A client that keeps attempting exactly `limit.Rate` hits every window — sustained rather than bursted, exactly the documented contract's own "Rate occurrences allowed per Per" — is admitted the full `limit.Rate` only in the very first window it ever uses a key in, then at most `limit.Rate - 1` every window after that, forever, with no self-recovery. `TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow` in `limiter_sliding_window_test.go` pins this shape down deterministically, with no dependency on intra-window timing (the denial holds for every `elapsedFraction`, so unlike the sliding-boundary test above, it needs no delicate control over exactly where within a window a hit lands).

At `Rate == 1` — where "at most `limit.Rate - 1` per window" is zero — the collapse is a complete, permanent lockout from a key's second window on: the first-ever hit is admitted, every later window's hits are denied, for as long as the key keeps being hit at least once per window. That is exactly why `Limit.validate` refuses a Rate of 1 outright (`ErrRateOneUnsupported`, see Error index): whatever `Per` a host pairs it with, the literal reading "once per Per" — the natural spelling of "once a day" — cannot be honoured, and the permanent lockout would arrive in production only when the first window rolled over, a day after the key's first use for `Per: 24h`. The lockout shape is unreachable — validation rejects Rate 1 before the store is ever touched, pinned by `TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched`.

This is safe-direction — it never admits more than configured, only less — and is a faithful consequence of the weighted formula above combined with "every call is a hit" (see this file's own Public API table and `Limiter`'s doc comment), not a coding defect: nothing here contradicts `weighted = currentCount + previousCount*(1-elapsedFraction)` as specified. It does, however, contradict a plain reading of "Rate occurrences allowed per Per" for any consumer whose traffic saturates its configured limit continuously rather than bursting under it — a plausible pattern for `ai-gateway`'s per-tenant request-rate limit or `integration`'s API-key throttling, both named among this module's documented consumers. Avoiding it would mean not incrementing a window's counter for a call that is itself denied — a different, unimplemented algorithm (the classical "check, then increment only if allowed" sliding-window counter), not a correction to this one — and doing that correctly under `KVStore`'s primitives, without reopening a Get-then-branch race of the kind `Allow`'s own doc comment warns against (its closing paragraph explains why a Get-then-branch ahead of the atomic increment is strictly worse, not better), is a shape change to `Allow`, not a bug fix. As shipped, the behavior is documented, tested and deliberate — known behavior, not silently absorbed loss.

## Rules

**Dependencies**
- Do not add a second speed-module dependency to this package. ratelimit sits directly above `pkgcore`, at the same graph depth as `dbkit`/`observability`/`tenancy`; depending on any of those (or anything above them) would very likely create a cycle, and none of them are needed for what this package does.
- Do not add a third-party dependency. This package is pure standard library plus `pkgcore`, and every dependency added here lands in every consuming module's build.

**Shape**
- Do not give `Limiter` a second method, an HTTP-specific `Decision` variant, or a bulk/multi-key `Allow`. One dimension per call, composed by the caller, is deliberate — see Known limitations and `docs/internal/11-cross-cutting.md`.
- Do not make ratelimit implement `pkgcore.Module`. It is intentionally a pure library with nothing to register.
- Do not read `pkgcore.TenantFromContext` (or any other tenancy-shaped context value) inside this package. A caller that wants per-tenant limiting encodes the tenant into `key` itself.

**Using `Allow`**
- Do not treat an error from `Allow` as a denial, or swallow it and treat it as an allow. `Allow` returns `KVStore` failures unmodified specifically so each call site can make its own fail-open/fail-closed choice — see `Allow`'s own doc comment for why that choice does not belong to this package.
- Do not construct a `Limit` from unchecked input without expecting `ErrInvalidLimit` on `Rate <= 0`, `Rate == 1` (surfaced as `ErrRateOneUnsupported`, which still matches `ErrInvalidLimit` through `errors.Is`) or `Per <= 0`. This package fails loudly rather than silently choosing always-allow or always-deny for a security-relevant primitive.
- Do not spell "once per Per" as `Rate: 1`. It is refused (`ErrRateOneUnsupported`): this limiter cannot honour that semantics — a Rate of 1 collapses into "allowed once ever, then denied forever" under the weighted sliding-window formula (see Known limitations). A Rate of 2 with the interval as `Per` admits at most two per window and never denies a caller whose attempts genuinely stay within one per window; a hard exactly-once-per-interval cap is not expressible at any Rate and belongs in the caller's own last-success gate.
- Do not assume `Decision.Remaining` is an exact countdown. It is `limit.Rate` minus a *weighted*, approximate sliding count, floored at zero and truncated to an `int` — it can read `0` even when `Decision.Allowed` was `true` for that same call, because the true weighted count was a fraction just under `limit.Rate`.
- A brand-new key under heavy concurrent load at the exact instant of its creation is exactly as precise as an already-existing one — `pkgcore.KVStore.IncrByFloatWithTTL` makes window-key creation atomic (see Known limitations). No compensating control (a pre-warming call, a dedicated marker key) is needed on this account; do not add one speculatively.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrInvalidLimit` (`ratelimit: invalid limit`) | `Allow` given a `Limit` with `Rate <= 0` or `Per <= 0` (or `Per` beyond `maxPer`); `Rate == 1` arrives through `ErrRateOneUnsupported`, below | Caller/programmer error — fix the call site; never treat as a runtime allow/deny signal |
| `ErrRateOneUnsupported` (code `ratelimit.rate_one_unsupported`, wrapping `ErrInvalidLimit`) | `Allow` given a `Limit` with `Rate == 1` | Same handling as `ErrInvalidLimit` — the code (or `errors.Is` against this sentinel) distinguishes "Rate of 1" from the generic invalid limit, so a host whose Rate-1 configuration is refused can recognize exactly what was wrong. A Rate of 1 can never be honoured literally: it collapses into a permanent lockout (see Known limitations), so it is refused before the store is touched |
| *(none — passthrough)* | Any error from the underlying `pkgcore.KVStore` (`IncrByFloatWithTTL` or `Get`), including a canceled/expired `ctx` | Returned unmodified. The caller decides fail-open vs. fail-closed for its own use case; ratelimit never decides this on the caller's behalf |

Design rationale — including why this module exists outside `pkgcore`, the six documented consumers named in this file's opening section, and why the algorithm is a sliding-window *counter* rather than a sliding-window *log* — lives in `docs/internal/11-cross-cutting.md`'s rate-limiting section (internally titled "rate limiting: independent module, single dimension, no business semantics"); the module dependency graph is in `docs/internal/01-architecture.md`.
