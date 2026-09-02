# ratelimit

ratelimit is speed's shared rate-limiting primitive: a `Limiter` that decides whether one more hit against a caller-supplied `key` is within a caller-supplied `Limit`, backed entirely by `pkgcore.KVStore`. It sits at the same graph depth as `dbkit`, `observability` and `tenancy` — directly above `pkgcore`, depending on nothing else (`pkgcore -> dbkit / observability / ratelimit -> tenancy -> ...`, see `docs/internal/01-architecture.md`) — and is a **pure library**: unlike every other module in this repository, it does not implement `pkgcore.Module`. It registers no routes, no config schema, no feature flags, no permissions. There is nothing to wire into a Kernel; a consumer just calls `ratelimit.New`.

This module exists because four otherwise-unrelated business modules each independently need rate limiting — `authn`'s login/registration/password-reset brute-force guard (always by IP, plus an endpoint-specific second dimension such as account, email or phone number), `integration`'s three-layer API key throttling (global + tenant + key), `sharing`'s public-link abuse guard, and `ai-gateway`'s per-tenant request-rate limit (independent of its credit-based cost limits) — and re-implementing the same sliding-window counter four times, slightly differently each time, is worse than building it once against the one interface every deployment mode already provides: `pkgcore.KVStore`. See `docs/internal/11-cross-cutting.md`'s rate-limiting section for the full design discussion this module implements, including why it was kept out of `pkgcore` itself (a shared primitive only *some* consumers need does not belong in the dependency floor every module carries).

| Concern | Where |
|---|---|
| `Limiter`, `Limit`, `Decision`, `ErrInvalidLimit`, `New`, and the sliding-window-counter algorithm itself (`slidingWindowLimiter`, `Allow`, and its private helpers) | `limiter.go` |
| Package doc comment | `doc.go` |
| Runnable usage documentation (`Example`, `ExampleLimiter_multipleDimensions`, `ExampleErrInvalidLimit`) | `example_test.go` |

**One dependency, and why there is only one.** ratelimit imports exactly one other speed module: `pkgcore`'s root package, for `pkgcore.KVStore`. Nothing else — no `apperr` (its errors are plain `errors.New` sentinels, matching how `pkgcore`'s own `kv.go` and `tenant.go` define theirs, since these are programmer/caller errors and KVStore-failure passthroughs, not API responses this package shapes itself), no `tenancy`, no third-party package beyond the Go standard library. This is architectural, not incidental minimalism, and it is what the design doc's "no business semantics" framing means concretely:

- ratelimit does not know what a tenant is. A caller that wants per-tenant limiting builds the tenant into `key` itself (e.g. `"ai-gateway:tenant:" + tenantID`) before calling `Allow` — this package never reads `pkgcore.TenantFromContext` or anything tenancy-shaped.
- ratelimit does not know what HTTP is. `Decision` is plain data (see Rules, below); translating a denial into a 429 with `Retry-After` and quota headers is entirely the caller's job.
- ratelimit does not know what "account", "IP", "email" or "API key" mean. Every one of those is just a `key` string a consumer chose; combining several dimensions (authn's IP-plus-account-or-contact guard, integration's global+tenant+key layers) is composition the *caller* does by calling `Allow` once per dimension, never something this package models. See `ExampleLimiter_multipleDimensions` in `example_test.go`.

Beyond `pkgcore`, ratelimit adds **zero** dependencies — no test-only ones either: unit tests use `pkgcore.NewMemoryKVStore()` directly, and there is deliberately no `integration_test/` tier (see Testing, below).

## The algorithm, and why it is a sliding-window *counter* rather than a sliding-window *log*

Time is partitioned into fixed windows of length `limit.Per`. An instant's window index is `t.UnixNano() / int64(limit.Per)` — safe once `Limit.validate` has ruled out `Per <= 0`, since `Per` is already an `int64` count of nanoseconds. Each window gets its own storage key (`windowKey`: the caller's `key`, `":"`, the window index in base 10), so the current window and the immediately preceding one are two distinct keys, and `Allow` reads both.

This shape — one counter per fixed window, not one entry per request — is forced by `pkgcore.KVStore`'s own contract, not a shortcut. A sliding-window *log* needs an ordered, range-queryable structure (typically a sorted set) so individual request timestamps can be trimmed and counted by range; `KVStore` deliberately offers nothing beyond opaque byte values plus `IncrByFloat` and `CompareAndSwap` (see `go/pkgcore/kv.go`'s own doc comment: it is designed against the weakest backend it must run under — an in-memory map in the standalone deployment mode — so it cannot grow a data type only Redis could satisfy without breaking that symmetry). A counter-per-window needs only "increment" plus "an expiry that delimits the window", and `IncrByFloat` plus `Set` already provide exactly that between them:

- **Recording a hit** calls `KVStore.IncrByFloat` on the current window's key first, unconditionally, before `limit` is even consulted for the decision. `IncrByFloat` is one of the two `KVStore` primitives documented as atomic under concurrency (the other, `CompareAndSwap`, isn't needed by this algorithm), and a fresh key "starts from zero and is stored without an expiry" (its own doc comment) — so no concurrent caller ever loses its own increment, and the call whose `IncrByFloat` transitions the key from absent to `1` is unambiguously "the first hit in this window".
- **That first-hit call** additionally attaches an expiry (`attachWindowTTL`), because a freshly-created key otherwise has no TTL and would accumulate forever. It does this by re-`Get`-ting the key's current value and writing it straight back through `Set` with a TTL of `2 * limit.Per` — long enough that this window's key is still readable as "the previous window" for a read landing anywhere in the *next* window, up to just before that window ends. The `2x` factor, and the re-`Get`, both matter — see Known limitations, below.
- **Reading the previous window's count** is a plain `KVStore.Get`: absent (never existed, or already expired) reads as zero either way. The stored bytes are parsed with `strconv.ParseFloat`, matching `IncrByFloat`'s own documented "shortest exact decimal encoding, which callers should parse ... rather than compare as text" — never a string comparison.
- **The two counts are combined** into `weighted = currentCount + previousCount*(1-elapsedFraction)`, where `elapsedFraction` is how far the current instant has moved into the current window (0 at its start, approaching 1 at its end). This is what makes it a *sliding* approximation: right after a window boundary, almost all of the previous window's count still counts against the limit, closing the classic fixed-window hole where a burst timed to straddle a boundary could otherwise get up to 2x the intended rate through. `limiter_test.go`'s `TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed` is a case built specifically to fail under a naive fixed-window implementation and pass under this one.
- `Decision.Allowed` is `weighted <= limit.Rate` **after** this call's own increment — the request that first pushes the count over the limit is the one denied, not the request after it. `Decision.Remaining` is `limit.Rate - weighted`, floored at zero and truncated (not rounded) to an `int`. `Decision.ResetAfter` is `limit.Per` minus the elapsed time into the current window.

`pkgcore.KVStore.CompareAndSwap` is not used by this algorithm at all today. It exists on the interface for a future consumer needing token-bucket semantics (per `docs/internal/11-cross-cutting.md`); this module does not need it and does not reach for it speculatively.

## Public API — `limiter.go`

| Signature | Purpose |
|---|---|
| `type Limit struct { Rate int; Per time.Duration }` | `Rate` occurrences allowed per `Per`. Both fields must be strictly positive |
| `type Decision struct { Allowed bool; Remaining int; ResetAfter time.Duration }` | Plain data, no protocol awareness — see Rules |
| `type Limiter interface { Allow(ctx context.Context, key string, limit Limit) (Decision, error) }` | The entire public surface. Every call is a hit; there is no separate "check without recording" mode |
| `func New(store pkgcore.KVStore) Limiter` | Returns a `Limiter` backed by `store`, hiding the concrete `slidingWindowLimiter` type behind the interface — the same pattern `pkgcore.NewMemoryKVStore` itself uses. Performs no I/O |
| `var ErrInvalidLimit = errors.New(...)` | Wrapped by the error `Allow` returns when `limit.Rate <= 0` or `limit.Per <= 0`. Match with `errors.Is` |

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

func (g *LoginGuard) Allow(ctx context.Context, account, ip string) (bool, error) {
	byAccount, err := g.limiter.Allow(ctx, "authn:login:account:"+account, ratelimit.Limit{Rate: 5, Per: time.Minute})
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

Unit tests (`limiter_test.go`) use `pkgcore.NewMemoryKVStore()` exclusively — no testcontainers, and deliberately **no `integration_test/` tier at all**, unlike `dbkit` and `tenancy`. Per `docs/internal/11-cross-cutting.md`: a real distributed `KVStore` backend's own correctness (Redis or equivalent) is that backend's own test responsibility, not something `go/ratelimit` re-verifies — this package's only contract is with the `KVStore` interface, which `pkgcore.NewMemoryKVStore()` satisfies fully for test purposes.

Coverage includes: exactly-at-the-limit allowed vs. one-over denied (`TestAllow_ExactlyAtLimit_AllowedThenOneOverDenied`); the sliding behavior specifically, via a boundary-straddling burst that would defeat a naive fixed-window implementation (`TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed`); TTL expiry actually happening, observed directly against the store (`TestAllow_WindowExpiry_OldWindowKeyExpires`); concurrent callers on the same key under `-race`, with a deterministic — not just bounded — expected count (`TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit`; see its own doc comment and Known limitations below for why it warms the key up before the concurrent burst); independent keys never interfering (`TestAllow_IndependentKeys_DoNotInterfere`); context cancellation honored (`TestAllow_ContextCanceled_ReturnsContextError`); the `ErrInvalidLimit` validation path (`TestAllow_InvalidLimit_ReturnsErrInvalidLimit`); `KVStore`-error passthrough for every one of `Allow`'s four possible error-return points, using a small test-local `erroringKVStore` fake (`TestAllow_KVStoreErrors_PropagatedToCaller`); and `attachWindowTTL`'s retry of a transient `KVStore` fault, using a second test-local fake that fails a bounded number of times before recovering (`flakyKVStore`) — both that a recovering fault never surfaces to the caller and still leaves the window's key with a real, working TTL (`TestAllow_AttachWindowTTL_TransientError_RetriedUntilKeyGetsExpiry`), and that a fault outlasting the retry budget still propagates after making exactly `attachWindowTTLMaxAttempts` attempts (`TestAllow_AttachWindowTTL_ExhaustsRetryBudget_ThenPropagatesError`).

## Known limitations

**The TTL-attachment race is real, was measured, and is bigger than the classic Redis idiom's "crash-only" framing suggests — read this before assuming the mitigation makes it negligible.** It is also not a one-time, cold-start event: as explained below, it recurs on every window rollover for as long as a key keeps being hit, so "pre-warm once" is not durable protection against a sustained attack. A freshly-created window key has no expiry, so exactly one caller — the one whose `IncrByFloat` call transitions the key from absent to `1` — is responsible for attaching one via `attachWindowTTL`. `KVStore` has no equivalent of Redis's `EXPIRE`, which sets a TTL without touching the value: `Set` is the *only* operation that can attach an expiry at all, and its own doc comment is explicit that it "replac[es] any existing value and expiry" together, never independently. `CompareAndSwap` cannot help either — its own doc comment says just as plainly that "the swap never changes the key's expiry", on both its update path and its create-if-absent path.

This leaves a real gap between the winning caller's `IncrByFloat` and its own follow-up `Set`. Two failures share it:

- **A process crash** in that gap is exactly as harmless as the classic idiom promises: the window's counter never gets a TTL, so it outlives its window — but nothing ever reads a window other than "current" or "immediately previous" again, so an over-long-lived key is the only symptom, and the counter itself stays accurate.
- **Ordinary concurrent load, no crash needed**, is not equally harmless: another caller's `IncrByFloat` can land in the gap and be silently overwritten by the `Set` that follows, undercounting the window and admitting more traffic than configured — a real correctness gap, not a cosmetic one. `attachWindowTTL` re-`Get`s the key's freshest value immediately before its `Set`, instead of blindly re-encoding the stale value its own `IncrByFloat` returned, specifically to shrink this — a version that skips the re-`Get` was measured, deliberately, racing hundreds of goroutines to create one fresh key at once, and consistently lost double-digit percentages of concurrent hits that way. The re-`Get` mitigation makes the typical loss much smaller (single digits, often zero, across repeated runs) but is **not a hard bound**: the same experiment, run enough times, once observed a spike to roughly the size of the whole burst before settling back to single digits on every subsequent retry. The gap's width is however long the winning goroutine's own `Get`-then-`Set` takes to reach the front of the store's internal lock queue, and neither Go's scheduler nor `KVStore`'s contract promises an upper bound on that under enough contention.

What *is* guaranteed, unconditionally: both failure modes are confined to the single instant a key is created. Once a window's key exists with its TTL already attached, every later hit against it is a pure `IncrByFloat` call, which `KVStore`'s contract guarantees is atomic and lossless under any amount of concurrency — no residual gap at all. Both failures are also confined to the one window they happen in (every window gets its own independent attempt at its own TTL, so nothing compounds across windows) and never cross keys.

**A related but distinct case: an ordinary transient `KVStore` error during `attachWindowTTL`'s own `Get` or `Set`** — a dropped connection, a momentary timeout, one bad node behind a load balancer — is neither a process crash nor concurrent load. `attachWindowTTL` retries its `Get`-then-`Set` sequence up to `attachWindowTTLMaxAttempts` times (see that constant's own doc comment in `limiter.go`) before giving up, specifically because this case is far more likely in a real distributed deployment than either of the two above, and, before that retry existed, was not bounded the way they are: any caller retrying the logical request after a failed `Allow` call got back an `IncrByFloat` count > 1 on the identical key, permanently skipping `attachWindowTTL` for the rest of that window's life — a key stuck with no expiry for good, not just "slightly longer-lived". A fault that persists past the retry budget still degrades to exactly the single-key, single-window leak the process-crash case above already accepts, not a new, unbounded one, and `Allow` still returns that error to its caller unmodified either way, per this module's documented error-passthrough contract.

**This does not mean the exposure is a rare, once-per-caller-key event.** `windowKey` mints a brand-new, never-before-used storage key every single `Per` interval — `windowKey(key, windowIndex)`, and `windowIndex` only ever increases — so the call that is "first in this window" recurs, deterministically, on *every* window boundary for as long as a caller's key keeps being hit, not once in that key's lifetime. An attacker who can burst concurrent requests timed at or after a window rollover — trivial with a brute-force tool's parallel connections, or an HTTP/2 multiplexed flood — reopens this exact gap every `Per`, indefinitely, against any single identifier under sustained attack: exactly the shape of IP, account, share-token, tenant-id and API-key traffic that `authn`, `sharing`, `ai-gateway` and `integration` respectively key on. Pre-warming a hot key with one sequential `Allow` call closes the gap only for **that one window's key**: it defers the exposure by exactly one `Per`, not for the caller-supplied key's lifetime, since the following window mints its own new, unwarmed key. `limiter_test.go`'s own `TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit` warms up and then stays inside that single window for its whole run, which is what lets it assert an exact, deterministic expected count — it is not evidence that pre-warming durably protects a continuously-attacked key across window boundaries. A consumer facing sustained or repeated attack traffic against one identifier must either accept this recurring exposure or add its own compensating control on top (a dedicated, single-writer marker key, or tolerating a materially lower effective rate under sustained burst). None of this is `memoryKVStore`-specific — it is forced by `KVStore`'s own contract (see below), so a Redis-backed `KVStore`'s classic `INCR`-then-`EXPIRE`-if-first sequence carries the identical race, with a **wider** gap once a network round trip separates the two calls, not a narrower one.

Closing this gap completely would need either a `KVStore` primitive this module does not have and is not this module's place to add unilaterally (something that sets an expiry with compare-and-swap value semantics in one atomic step), or a second, dedicated marker key per window that only one caller ever writes and nothing ever contends for (genuinely race-free for the marker itself), at the cost of a second storage key per window plus explicit reclamation logic once that marker expires — `IncrByFloat`-only keys never age out on their own, unlike this module's current single-key design. That heavier design was not built here by default, but "no consumer needs it" is not why: every one of this module's documented consumers (`docs/internal/11-cross-cutting.md`'s rate-limiting section: `authn`, `integration`, `sharing`, `ai-gateway`) keys on an attacker-controlled or attacker-refreshable identifier, and the recurrence described above means the exposure is live against all four continuously, not just at cold start. A consumer that cannot accept it should add its own compensating control now rather than wait for an incident to show that pre-warming alone was not protection.

**No multi-dimension or progressive/escalating semantics, on purpose.** `Allow` handles exactly one dimension per call — see this file's own intro and `Limiter`'s doc comment. `authn`'s "delay growing with repeated failures, up to a lockout" behavior is business logic built on top of the plain counts `Allow` reports, not a mode this package understands. Do not add a multi-key or escalation-aware variant of `Allow` speculatively; the design doc is explicit that this shape is deliberate, not an oversight to fill in later.

**No dynamic configuration.** `Limit` values are supplied by each call site directly in code, not registered as a schema-backed, operator-tunable setting the way `docs/internal/11-cross-cutting.md`'s configuration-management section describes for other modules' knobs. This is deliberate for the same reason ratelimit implements no `pkgcore.Module`: it is a library, not a component the Kernel assembles.

**A client that saturates its Rate every window converges permanently to Rate-1 admitted per window, not Rate — proven algebraically, not merely observed.** At the Rate-th sequential `Allow` call within any window, `currentCount` is exactly `limit.Rate` (`IncrByFloat`'s own gapless, one-at-a-time increments guarantee this no matter how those calls are timed), so `weighted = limit.Rate + previousCount*(1-elapsedFraction)`. The moment a *previous* window's recorded count has reached `limit.Rate` — which needs only `limit.Rate` calls to have landed in it, whether or not every one of them was itself `Allowed`, since "every call is a hit" applies to denied calls too — that Rate-th call in the *current* window is denied unconditionally: `previousCount*(1-elapsedFraction) > 0` for every `elapsedFraction` in `[0,1)`, because `elapsedFraction` can never reach exactly `1` while a call still belongs to this window. Timing within the window does not save it, however late it lands.

This is self-sustaining, not a one-window blip: the denied call still increments its own window's counter (again, "every call is a hit"), so a window that denies its own Rate-th call still ends with a recorded count of `limit.Rate`, handing the identical `previousCount >= limit.Rate` to the window after it. A client that keeps attempting exactly `limit.Rate` hits every window — sustained rather than bursted, exactly the documented contract's own "Rate occurrences allowed per Per" — is admitted the full `limit.Rate` only in the very first window it ever uses a key in, then at most `limit.Rate - 1` every window after that, forever, with no self-recovery. At `Rate == 1` this is a complete, permanent lockout: the second-ever use of a key is denied, and every use after that, indefinitely, for as long as the key keeps being hit at least once per window. `TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow` and `TestAllow_SaturatingClient_Rate1_LocksOutPermanently` in `limiter_test.go` pin both shapes of this down deterministically, with no dependency on intra-window timing (the denial holds for every `elapsedFraction`, so unlike the sliding-boundary test above, these need no delicate control over exactly where within a window a hit lands).

This is safe-direction — it never admits more than configured, only less — and is a faithful consequence of the weighted formula above combined with "every call is a hit" (see this file's own Public API table and `Limiter`'s doc comment), not a coding defect: nothing here contradicts `weighted = currentCount + previousCount*(1-elapsedFraction)` as specified. It does, however, contradict a plain reading of "Rate occurrences allowed per Per" for any consumer whose traffic saturates its configured limit continuously rather than bursting under it — a plausible pattern for `ai-gateway`'s per-tenant request-rate limit or `integration`'s API-key throttling, both named among this module's documented consumers. Avoiding it would mean not incrementing a window's counter for a call that is itself denied — a different, unimplemented algorithm (the classical "check, then increment only if allowed" sliding-window counter), not a correction to this one — and doing that correctly under `KVStore`'s primitives without reopening a Get-then-branch race of the kind the TTL-attachment race above already warns against is exactly the sort of shape change to `Allow` that Rules, below, reserves for the design doc, not a unilateral change here. Revisit this trade-off deliberately, in `docs/internal/11-cross-cutting.md`'s rate-limiting section, if a real consumer's continuously-saturating traffic makes the tax matter in practice; until then, treat it as documented, tested, known behavior, not silently absorbed loss.

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
- Do not construct a `Limit` from unchecked input without expecting `ErrInvalidLimit` on `Rate <= 0` or `Per <= 0`. This package fails loudly rather than silently choosing always-allow or always-deny for a security-relevant primitive.
- Do not assume `Decision.Remaining` is an exact countdown. It is `limit.Rate` minus a *weighted*, approximate sliding count, floored at zero and truncated to an `int` — it can read `0` even when `Decision.Allowed` was `true` for that same call, because the true weighted count was a fraction just under `limit.Rate`.
- Do not expect perfect precision from a brand-new key under heavy concurrent load at the exact instant of its creation — see Known limitations, and note that "brand-new key" recurs on every window rollover, not just once per caller-supplied key. Pre-warming a hot key with one sequential `Allow` call protects only that one window; it is not durable protection for a continuously- or repeatedly-attacked identifier, which needs its own compensating control instead.

## Error index

| Sentinel | Triggered by | Handling |
|---|---|---|
| `ErrInvalidLimit` (`ratelimit: invalid limit`) | `Allow` given a `Limit` with `Rate <= 0` or `Per <= 0` | Caller/programmer error — fix the call site; never treat as a runtime allow/deny signal |
| *(none — passthrough)* | Any error from the underlying `pkgcore.KVStore` (`Get`, `Set`, or `IncrByFloat`), including a canceled/expired `ctx` | Returned unmodified. The caller decides fail-open vs. fail-closed for its own use case; ratelimit never decides this on the caller's behalf |

Design rationale — including why this module exists outside `pkgcore`, the four documented consumers, and why the algorithm is a sliding-window *counter* rather than a sliding-window *log* — lives in `docs/internal/11-cross-cutting.md`'s rate-limiting section (internally titled "rate limiting: independent module, single dimension, no business semantics"); the module dependency graph is in `docs/internal/01-architecture.md`.
