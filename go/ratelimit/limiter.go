package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// ErrInvalidLimit is returned by Allow when the supplied Limit is not usable
// (a Rate or Per that is zero or negative, or a Per so large that
// windowTTLFactor*Per would overflow a time.Duration -- see maxPer). A
// limiter is a security-relevant primitive: garbage input from a caller is a
// programmer error and must fail loudly and unambiguously, not be silently
// reinterpreted as always-allow or always-deny. Callers classify it with
// errors.Is.
var ErrInvalidLimit = errors.New("ratelimit: invalid limit")

// Limit is the rate a key is allowed to be hit at: Rate occurrences per Per
// duration. Rate must be strictly positive, and Per must be strictly
// positive and no larger than maxPer; Allow rejects anything else with an
// error wrapping ErrInvalidLimit rather than guessing what an invalid Limit
// was meant to do.
type Limit struct {
	// Rate is the number of requests allowed within Per.
	Rate int
	// Per is the length of the window Rate applies to.
	Per time.Duration
}

// validate reports ErrInvalidLimit when l cannot be used to make a decision.
func (l Limit) validate() error {
	if l.Rate <= 0 || l.Per <= 0 {
		return fmt.Errorf("%w: rate=%d per=%s (both must be > 0)", ErrInvalidLimit, l.Rate, l.Per)
	}
	if l.Per > maxPer {
		return fmt.Errorf("%w: per=%s exceeds the maximum of %s (windowTTLFactor*per must fit in a time.Duration)", ErrInvalidLimit, l.Per, maxPer)
	}
	return nil
}

// Decision is the outcome of one Allow call. It is plain data with no
// protocol awareness: translating a denial into, say, an HTTP 429 with
// Retry-After and quota headers is the caller's job, not this package's, so
// that the same Decision works equally for an HTTP handler and for something
// unrelated to HTTP, such as throttling a background job dispatcher.
type Decision struct {
	// Allowed reports whether the request that produced this Decision is
	// itself within limit. The request that pushes the count over Rate is
	// the one denied, not the request after it.
	Allowed bool
	// Remaining is how many more hits the key has in the current window,
	// floored at zero and truncated (not rounded) to an int. It is an
	// approximation, not a precise countdown: see the package's sliding
	// window algorithm below for why it can be fractional before truncation,
	// and clampRemaining's own doc comment for why a very large value is
	// capped at math.MaxInt rather than converted to int directly.
	Remaining int
	// ResetAfter is how long until the current window ends, after which
	// Remaining recovers as the window slides forward.
	ResetAfter time.Duration
}

// Limiter decides whether a hit against key is within limit. Every call is a
// hit: there is no separate "check without recording" mode, matching how
// pkgcore.KVStore.IncrByFloat itself is unconditional.
//
// Limiter deliberately handles exactly one dimension per call. A caller that
// needs several — authn's brute-force guard, which combines IP with an
// endpoint-specific second dimension such as account, email or phone number;
// integration's global+tenant+key API key throttling — calls Allow once per
// dimension and combines the results itself (any one denial denies the
// whole request). This keeps the primitive's shape independent of whichever
// consumer adopts it first, and independent of exactly which dimensions any
// one of them ends up combining.
//
// On any error from the underlying KVStore, Allow returns that error
// unmodified. It never decides fail-open or fail-closed on the caller's
// behalf — that policy choice belongs to whoever is consuming the Decision,
// since the right answer differs by call site (a login guard and a
// best-effort background throttle do not want the same answer to "the store
// is down").
type Limiter interface {
	// Allow records a hit against key and reports whether it is within
	// limit. An invalid limit (see Limit.validate) fails with an error
	// wrapping ErrInvalidLimit before the store is ever touched. A canceled
	// or expired ctx surfaces as whatever error the store's own ctx check
	// returns, per pkgcore.KVStore's contract that every operation honors
	// ctx.
	Allow(ctx context.Context, key string, limit Limit) (Decision, error)
}

// New returns a Limiter backed by store, implementing the sliding-window
// counter approximation described on slidingWindowLimiter below. The
// concrete type is unexported, mirroring pkgcore.NewMemoryKVStore's own
// pattern of returning the interface rather than a type callers could depend
// on directly.
//
// store is typically pkgcore.NewMemoryKVStore() in the standalone deployment
// mode or unit tests, and a Redis-backed (or equivalent) KVStore in the
// distributed deployment mode. Either way New performs no I/O: it only
// captures the reference.
func New(store pkgcore.KVStore) Limiter {
	return &slidingWindowLimiter{store: store}
}

// slidingWindowLimiter implements Limiter as a sliding-window-counter
// approximation, not a sliding-window-log: it tracks one counter per fixed
// window rather than a timestamped entry per request. That choice is forced
// by pkgcore.KVStore's own contract, not a shortcut — a sliding-window log
// needs an ordered, range-queryable structure (a sorted set, typically) so
// old entries can be trimmed and counted by timestamp range, and KVStore
// deliberately exposes nothing beyond opaque byte values plus IncrByFloat
// and CompareAndSwap (see kv.go's own doc comment: it is designed against
// the weakest backend it must run under, an in-memory map, so it cannot grow
// a data type only Redis could offer without breaking that symmetry). A
// counter-per-window needs only "increment" plus "an expiry that delimits
// the window", which IncrByFloat and Set already provide between them.
//
// # Algorithm
//
// Time is partitioned into fixed windows of length limit.Per. An instant t's
// window index is t.UnixNano() / int64(limit.Per) — Per is already a count
// of nanoseconds as an int64, so this is a plain integer bucket number, safe
// once Limit.validate has ruled out Per <= 0 (which would otherwise divide
// by zero). Each window gets its own storage key, built by windowKey: the
// caller's key plus ":" plus the window index in base 10. The current
// window and the immediately preceding one (index - 1) are therefore two
// distinct keys — Allow reads both.
//
// Recording a hit increments the current window's key with
// KVStore.IncrByFloat, which is unconditionally called first, before limit
// is even consulted for the decision. IncrByFloat is one of the two
// primitives KVStore documents as atomic under concurrency — the other,
// CompareAndSwap, isn't needed by this algorithm (see kv.go's own doc
// comment). IncrByFloat's own doc comment separately promises it is safe for
// concurrent use, and that a fresh key "starts from zero and is stored
// without an expiry", so no concurrent caller ever loses its own increment —
// every simultaneous Allow call against the same key gets back a distinct,
// correctly-ordered count. See attachWindowTTL below for what happens right
// after, on the specific call whose IncrByFloat created the key.
//
// The previous window's count is read with a plain KVStore.Get
// (readWindowCount): a missing key — either it never existed, or it expired
// — reads as zero, which is exactly "no hits recorded in that window" either
// way. The stored bytes are parsed with strconv.ParseFloat, per
// IncrByFloat's own doc comment ("stored as its shortest exact decimal
// encoding, which callers should parse with strconv.ParseFloat rather than
// compare as text"), never compared or matched as a string.
//
// The two counts are combined into an approximate sliding count:
//
//	weighted = currentCount + previousCount*(1-elapsedFraction)
//
// where elapsedFraction is how far the current instant has moved into the
// current window (0 at the window's start, approaching 1 at its end). This
// is what makes the approximation a sliding window rather than a hard-reset
// fixed one: right after a window boundary, elapsedFraction is near zero, so
// almost all of the previous window's count still counts against the limit,
// closing the classic fixed-window hole where a burst timed to straddle a
// boundary could otherwise get up to 2x the intended rate through (half
// against each window's independent, unshared quota). See
// TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed in limiter_test.go for a
// case that would fail under a naive fixed-window implementation.
//
// Allowed is whether weighted, after this call's own increment, is still
// <= limit.Rate — the call that first pushes the count over the limit is
// itself the one denied. Remaining is limit.Rate minus weighted, floored at
// zero and, at the other end, capped rather than converted to int directly
// (see Decision.Remaining and clampRemaining). ResetAfter is limit.Per minus
// the elapsed time into the current window.
//
// # Saturating clients permanently lose one slot per window
//
// The Rate-th sequential Allow call within a window has currentCount exactly
// limit.Rate, regardless of timing (IncrByFloat's own gapless,
// one-at-a-time increments guarantee this), so weighted = limit.Rate +
// previousCount*(1-elapsedFraction) at that call. Once a previous window's
// recorded count has reached limit.Rate — which needs only limit.Rate calls
// to have landed in it, allowed or not, since every call increments its own
// window's counter — that added term is strictly positive for every
// elapsedFraction in [0,1), because elapsedFraction can never reach exactly
// 1 while a call still belongs to this window. So the Rate-th call is
// denied unconditionally, no matter how late in the window it lands, and
// because the denial still increments its own window's counter, the window
// that denies it also ends with a recorded count of limit.Rate — handing
// the same >=limit.Rate previousCount to the window after it. A client that
// attempts exactly limit.Rate hits every window, sustained rather than
// bursted (the documented contract's own "Rate occurrences allowed per
// Per"), is therefore admitted the full limit.Rate only in the very first
// window it ever uses a key in, then at most limit.Rate-1 every window
// after that, forever, with no self-recovery — a complete, permanent
// lockout at Rate == 1.
//
// This is safe-direction (it never over-admits) and a faithful consequence
// of the weighted formula above combined with "every call is a hit"
// (Limiter's own doc comment), not a defect in this implementation of that
// formula — see AGENTS.md's Known limitations for the full analysis,
// including why avoiding it needs a different, unimplemented algorithm
// (increment only on an allowed call) rather than a correction to this one.
// TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow and
// TestAllow_SaturatingClient_Rate1_LocksOutPermanently in limiter_test.go
// pin this down deterministically, independent of intra-window timing.
//
// # The TTL-attachment race
//
// A freshly-created key has no expiry (IncrByFloat's own documented
// behavior), so left alone, a key would accumulate forever instead of
// aging out with its window. attachWindowTTL closes that gap, but only on
// the one call whose IncrByFloat reported firstHitInWindow — i.e. the call
// that just transitioned the key from absent to existing — matching the
// classic Redis INCR-then-EXPIRE-if-first idiom.
//
// KVStore has no primitive equivalent to Redis's EXPIRE, which sets a TTL
// without touching the value: the only KVStore operation that can attach an
// expiry at all is Set, and Set's own doc comment is explicit that it
// "replac[es] any existing value and expiry" — value and expiry move
// together, never independently. That gap is why attachWindowTTL re-Gets
// the key's current value immediately before writing it back with a TTL,
// instead of blindly re-encoding the "1" its own IncrByFloat call returned:
// blindly reusing a value that is already stale by the time Set runs would
// silently erase any concurrent caller's increment that landed in between,
// which would undercount the window and let more traffic through than
// configured — a real correctness failure, not merely a cosmetic one, and
// one CompareAndSwap cannot fix here either, since its own doc comment says
// just as plainly that "the swap never changes the key's expiry." Re-Get is
// not a full fix — Set still has no compare semantics, so a call landing in
// the narrow gap between attachWindowTTL's own Get and its own Set is still
// clobbered — but it shrinks the exposed window from "however long until
// the creating goroutine gets back around to calling Set" down to the width
// of two back-to-back store calls with no logic in between, which is the
// smallest this gap can be made with the primitives KVStore offers.
//
// What is left, deliberately, has been measured empirically, not just
// assumed, and it is more than the classic idiom's crash-only framing
// suggests. Two genuinely different failures share this one gap:
//
//   - The process crashes between IncrByFloat and attachWindowTTL's Set.
//     The window's counter never gets a TTL, so it outlives its window.
//     This part is exactly as harmless as the classic idiom: nothing ever
//     reads a window other than "current" or "immediately previous" again,
//     and the counter itself is otherwise accurate — no crash recovery
//     needed, just a slightly longer-lived key.
//
//   - No crash: purely from concurrent load, another caller's IncrByFloat
//     lands in the residual Get-to-Set gap and is overwritten by the Set
//     that follows. This is a real correctness gap, not a cosmetic one — an
//     undercounted window admits more traffic than configured — and it is
//     NOT tightly bounded. Deliberately racing hundreds of goroutines to
//     create the same fresh key at once, blindly re-encoding the stale
//     firstHitInWindow value instead of re-Getting first loses double-digit
//     percentages of concurrent hits, consistently. Re-Getting immediately
//     beforehand — what this package actually does — makes the typical
//     loss much smaller (single digits, often zero, across repeated runs),
//     but not a hard bound: the gap is however long the creating
//     goroutine's own Get-then-Set takes to reach the front of the store's
//     internal lock queue, and Go's scheduler gives no upper bound on how
//     long a goroutine can be preempted between two calls under enough
//     contention — a run of this package's own test suite once observed
//     the loss spike to roughly the size of the whole burst before settling
//     back to single digits on every retry after. Treat the realistic
//     expectation as "usually negligible, rarely a real spike", not as a
//     numeric ceiling.
//
//     Do not read "a key's first-ever hit" above as a rare, once-per-caller
//     event: windowKey (below) mints a brand-new, never-before-used storage
//     key every single Per interval, for as long as a caller's key keeps
//     being hit at all — see TestAllow_EveryWindowRollover_MintsFreshKey in
//     limiter_test.go, which proves this structurally, with no concurrency
//     involved. The call that is "first in this window" therefore recurs
//     deterministically on every window boundary, not once in a key's
//     lifetime. An attacker who can burst concurrent requests timed at or
//     after a window rollover — trivial with a brute-force tool's parallel
//     connections, or an HTTP/2 multiplexed flood — reopens this exact gap
//     every Per, indefinitely, against any single identifier under sustained
//     attack: IP, account, share token, tenant id, API key, exactly what this
//     package's four documented consumers (authn, sharing, ai-gateway,
//     integration) key on. Pre-warming a hot key with one sequential Allow
//     call (see TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit's
//     warm-up step) closes the gap only for that one window's key: it defers
//     the exposure by exactly one Per, not for the caller-supplied key's
//     lifetime, because the following window mints its own new, unwarmed key.
//     A consumer facing a continuously- or repeatedly-attacked identifier
//     must either accept this recurring exposure or add its own compensating
//     control on top of this package (a dedicated, single-writer marker key,
//     or tolerating a materially lower effective rate under sustained burst);
//     pre-warming alone is not durable protection against that threat model.
//     None of this is specific to memoryKVStore — it is forced by KVStore's
//     contract (see "Closing this gap" below), so a Redis-backed KVStore's
//     classic INCR-then-EXPIRE-if-first sequence carries the identical race,
//     with a wider gap once a network round trip separates the two calls, not
//     a narrower one.
//
// Both failures are bounded to the one window they happen in — every
// window gets its own independent attempt at its own TTL, so nothing
// compounds across windows — and neither ever crosses keys: two different
// callers' rate limits never observe each other's window state. And both
// are confined to the exact instant a key is created: once a window's key
// exists with its TTL already attached, every later hit against it is a
// pure IncrByFloat call, which KVStore's own contract guarantees is atomic
// and lossless under any amount of concurrency, with no residual gap at
// all.
//
// Closing this gap completely would need a KVStore primitive this package
// does not have and cannot add on its own: something that sets an expiry
// with compare-and-swap semantics on the value in the same atomic step.
// CompareAndSwap looks like a candidate but is not one — its own doc
// comment is explicit that "the swap never changes the key's expiry", on
// both the update path and the create-if-absent path alike, so no sequence
// built from it ever attaches a TTL at all. A design using a second,
// dedicated marker key per window (written once, by exactly one caller, and
// never contended, so its own Set is genuinely race-free) can close the gap
// for the marker itself, at the cost of a second key and its own explicit
// reclamation logic once its marker expires, since IncrByFloat-only keys
// never age out on their own. That heavier design was not built by default
// here, but "nothing needs it" is not why: per the recurrence described
// above, every one of this module's documented consumers (docs/internal's
// rate-limiting section: authn, sharing, ai-gateway, integration) keys on an
// attacker-controlled or attacker-refreshable identifier, so a consumer
// that cannot accept the recurring exposure should add its own compensating
// control rather than assume this package already closes it for them.
//
// Do not "fix" this by reading the key before calling IncrByFloat to decide
// whether to skip straight to Set: a
// Get-then-branch ahead of the atomic increment reintroduces a real
// lost-update race on every hit, not just the first one in a window, which
// is strictly worse.
type slidingWindowLimiter struct {
	store pkgcore.KVStore
}

const (
	// firstHitInWindow is the value KVStore.IncrByFloat returns exactly when
	// its own call is the one that created the window's counter key — i.e.
	// this hit is the first recorded in that window. Comparing a float64
	// against this exactly is safe here because every increment this
	// package issues is a whole 1, so the running total is always an exact
	// integral value with no accumulated floating-point error.
	firstHitInWindow float64 = 1

	// windowTTLFactor sizes the expiry attached to a window's counter key as
	// a multiple of the window length. It must be at least 2: a window's key
	// can be created as early as the very start of its own window, and it
	// must still be readable as "the previous window" for a read landing as
	// late as just before the *next* window ends — a span of up to two full
	// window lengths after the key's own creation.
	windowTTLFactor = 2

	// maxPer is the largest Limit.Per that Limit.validate accepts. Allow
	// computes windowTTLFactor*Per as a time.Duration (the ttl passed to
	// attachWindowTTL below), and time.Duration is a signed 64-bit count of
	// nanoseconds: a Per above this bound would make that multiplication
	// overflow and silently wrap to a negative duration. KVStore.Set's own
	// documented contract says "a ttl of zero or less stores the key without
	// an expiry", so a negative ttl reaching it would pin the window's
	// counter key in the store forever instead of letting it expire with its
	// window — a permanently-broken state triggered by a Limit value that
	// looks like nothing worse than an unusually long window. A Per this
	// large is never a legitimate rate-limit window, so validate rejects it
	// up front, before Allow ever computes the overflowing multiplication.
	maxPer = time.Duration(math.MaxInt64 / windowTTLFactor)

	// attachWindowTTLMaxAttempts bounds how many times attachWindowTTL
	// retries its Get-then-Set sequence before giving up and returning the
	// last error to Allow's caller.
	//
	// Allow calls attachWindowTTL exactly once per window -- gated on
	// currentCount == firstHitInWindow at its call site -- and nothing else
	// in this package ever attempts to attach a TTL to that same window's
	// key again. Without a retry inside attachWindowTTL itself, that makes
	// any single transient KVStore error during that one call -- a dropped
	// connection, a momentary timeout, one bad node behind a load balancer
	// -- indistinguishable from a process crash landing in the same gap
	// (see "The TTL-attachment race" above): the window's key is left in
	// the store with no expiry, and a caller retrying the logical request
	// gets back a fresh IncrByFloat count > 1 on the identical key (the key
	// already exists), which skips this block for good. A process crash is
	// rare and, per that same doc section, accepted as harmless beyond one
	// longer-lived key; an ordinary transient store error is not rare in a
	// real distributed deployment (Redis behind a network link) and was
	// reachable through nothing more exotic than one badly-timed hiccup --
	// a materially more likely trigger than a crash, and one this package
	// can actually do something about by retrying.
	//
	// A fault that does not clear within this budget is, at that point, a
	// sustained outage rather than a transient blip, and degrades to
	// exactly the single-key, single-window leak the process-crash case
	// already accepts -- not a new, unbounded failure mode.
	//
	// Deliberately no backoff sleep between attempts: Allow sits in a
	// synchronous, latency-sensitive path shared by every consumer (authn's
	// login guard among them), so retrying immediately keeps the
	// overwhelmingly common case -- the first attempt already succeeds --
	// exactly as cheap as before, without picking an arbitrary backoff
	// duration this package has no basis for (it takes no injectable
	// clock at all).
	attachWindowTTLMaxAttempts = 3

	// The following mirror pkgcore's own (unexported) KVStore.IncrByFloat
	// numeric encoding — 'g' format, shortest exact round-trip precision, 64
	// bits (see go/pkgcore/kv.go's kvFloatFormat / kvFloatPrecisionShortest /
	// kvFloatBitSize). They must stay in sync with that encoding: this
	// package only ever writes a window counter's value back with the exact
	// bytes it most recently Get, so in practice a divergence here would
	// only matter for the fallback path in attachWindowTTL. Kept as named
	// constants, rather than inlined, for that reason.
	floatEncodingFormat    = 'g'
	floatEncodingPrecision = -1
	floatEncodingBitSize   = 64
)

// Allow implements Limiter.Allow. See slidingWindowLimiter's own doc comment
// for the algorithm.
func (l *slidingWindowLimiter) Allow(ctx context.Context, key string, limit Limit) (Decision, error) {
	if err := limit.validate(); err != nil {
		return Decision{}, err
	}

	now := time.Now()
	perNanos := int64(limit.Per)
	windowIndex := now.UnixNano() / perNanos
	elapsedNanos := now.UnixNano() % perNanos

	currentKey := windowKey(key, windowIndex)
	previousKey := windowKey(key, windowIndex-1)

	currentCount, err := l.store.IncrByFloat(ctx, currentKey, 1)
	if err != nil {
		return Decision{}, err
	}

	if currentCount == firstHitInWindow {
		// A distinct name, not another "err", so this does not shadow the
		// outer err declared above (govet's shadow check, run under
		// golangci-lint, flags that as a real finding, not a style nit: a
		// shadowed err here would be easy to mistake for updating the outer
		// one if this block ever grows past its current single statement).
		if ttlErr := l.attachWindowTTL(ctx, currentKey, time.Duration(windowTTLFactor)*limit.Per); ttlErr != nil {
			return Decision{}, ttlErr
		}
	}

	previousCount, err := l.readWindowCount(ctx, previousKey)
	if err != nil {
		return Decision{}, err
	}

	elapsedFraction := float64(elapsedNanos) / float64(perNanos)
	weighted := currentCount + previousCount*(1-elapsedFraction)

	return Decision{
		Allowed:    weighted <= float64(limit.Rate),
		Remaining:  clampRemaining(float64(limit.Rate) - weighted),
		ResetAfter: limit.Per - time.Duration(elapsedNanos),
	}, nil
}

// clampRemaining converts "how many more hits are left in the window" to
// Decision.Remaining's int, clamping both ends instead of ever converting a
// value int cannot represent: below zero to zero (weighted has already
// reached or passed limit.Rate; Decision.Remaining's own doc comment calls
// this "floored at zero"), and, symmetrically, above int's own range to
// math.MaxInt.
//
// That top clamp is reachable with entirely legitimate input, and skipping
// it is a real, confirmed portability bug, not defensive-programming
// caution. Limit.validate requires only Rate > 0 -- it has no upper bound,
// and a large sentinel such as math.MaxInt is a real convention some callers
// use for "effectively unlimited" instead of skipping the Allow call
// altogether. float64 can only represent integers exactly up to 2^53, so
// float64(limit.Rate) for a Rate that large has already rounded up past
// int64's own maximum by the time this function is ever called: math.MaxInt
// on a 64-bit platform is 2^63-1, which rounds to the nearest representable
// float64, 2^63 -- one past what int64 can hold. The Go spec (Conversions)
// is explicit about what happens next: "if the result type cannot represent
// the value the conversion succeeds but the result value is
// implementation-defined". That is not theoretical here. Verified
// empirically, both ways, on the very same machine: int(2^63) on this
// package's darwin/arm64 development host saturates to math.MaxInt64
// (arm64's FCVTZS instruction saturates on overflow), but the identical Go
// source cross-compiled to amd64 and executed under Rosetta on that same
// host produces math.MinInt64 instead (amd64's CVTTSD2SI leaves the x86
// "integer indefinite" bit pattern on overflow, which happens to equal
// MinInt64). Left unclamped, a real production amd64 deployment would
// observe Decision{Allowed: true, Remaining: math.MinInt64} for input that
// looks entirely correct in local arm64 development -- a badly wrong signal
// for anything that inspects Remaining, such as a quota header or a
// "Remaining <= 0" fallback check. Clamping first guarantees the int(...)
// conversion below only ever runs on a value already inside int's range,
// which the spec guarantees converts identically on every platform, so
// Remaining for this input stays the same large, positive value everywhere
// instead of merely "not negative" on some architectures and not others.
func clampRemaining(remaining float64) int {
	switch {
	case remaining < 0:
		return 0
	case remaining >= float64(math.MaxInt):
		return math.MaxInt
	default:
		return int(remaining)
	}
}

// attachWindowTTL gives currentKey an expiry after this call's own
// IncrByFloat call created it. See slidingWindowLimiter's doc comment
// ("The TTL-attachment race") for why it re-Gets the value instead of
// reusing the stale firstHitInWindow constant, and for the race that leaves;
// see attachWindowTTLMaxAttempts's own doc comment for why the Get-then-Set
// sequence itself is retried up to that many times before giving up.
func (l *slidingWindowLimiter) attachWindowTTL(ctx context.Context, currentKey string, ttl time.Duration) error {
	var (
		fresh []byte
		found bool
		err   error
	)
	for attempt := 0; attempt < attachWindowTTLMaxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// A cancelled or expired ctx fails identically on every
			// remaining attempt -- stop now instead of burning the rest of
			// the retry budget on a call that cannot possibly succeed.
			return ctxErr
		}

		fresh, found, err = l.store.Get(ctx, currentKey)
		if err != nil {
			continue
		}
		if !found {
			// Should not happen in practice — ttl is always well above zero and
			// nothing else in this package ever deletes a window key — but fall
			// back to the one hit this call itself is certain of, rather than
			// attaching a TTL to a value pulled out of thin air.
			fresh = encodeCount(firstHitInWindow)
		}
		if err = l.store.Set(ctx, currentKey, fresh, ttl); err == nil {
			return nil
		}
	}
	return err
}

// readWindowCount returns the count stored under key, treating an absent key
// — never created, or already expired — as zero either way, per KVStore
// Get's own contract that both are indistinguishable to a caller.
func (l *slidingWindowLimiter) readWindowCount(ctx context.Context, key string) (float64, error) {
	value, found, err := l.store.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	count, err := strconv.ParseFloat(string(value), floatEncodingBitSize)
	if err != nil {
		// The offending value is deliberately left out of the error, matching
		// pkgcore.ErrNotNumeric's own reasoning: a KVStore may hold sensitive
		// data, and this package's storage keys can embed caller-supplied
		// identifiers (account ids, IP addresses, ...).
		return 0, fmt.Errorf("ratelimit: window counter value is not a valid float: %w", err)
	}
	return count, nil
}

// windowKey builds the per-window storage key for key at windowIndex: the
// caller's key, ":", and the window index in base 10. The current window and
// the previous one are always two distinct keys, since windowIndex differs.
//
// This needs no delimiter-escaping on key, and that omission is deliberate,
// not an oversight. strconv.FormatInt's output is always drawn from
// "-0123456789" and therefore never itself contains ':', so the ':' this
// function inserts is always the one colon in the result whose suffix reads
// as a valid base-10 integer all the way to the end of the string. Any other
// colon already present inside key cannot also have that property: the
// characters after it would have to include this function's own trailing
// ':'+digits, and ':' is not a digit. That makes (key, windowIndex) -> string
// collision-free by construction for every key -- one embedding colons, an
// IPv6-address-shaped key, or a key already shaped like "something:123" all
// included -- crossed with every windowIndex, not merely the sample crossed
// in TestWindowKey_DistinctPairsNeverCollide below. key is treated as an
// opaque, caller-owned string throughout this package (see AGENTS.md); this
// function only ever builds a windowKey, it never parses one back apart.
func windowKey(key string, windowIndex int64) string {
	return key + ":" + strconv.FormatInt(windowIndex, 10)
}

// encodeCount encodes count the same way pkgcore's KVStore.IncrByFloat does,
// so a value this package writes with Set is parsed identically to one
// IncrByFloat itself wrote.
func encodeCount(count float64) []byte {
	return strconv.AppendFloat(nil, count, floatEncodingFormat, floatEncodingPrecision, floatEncodingBitSize)
}
