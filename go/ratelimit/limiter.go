package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// ErrInvalidLimit is returned by Allow when the supplied Limit is not usable
// (a Rate or Per that is zero or negative, a Rate of 1 -- which has its own
// coded wrapper, ErrRateOneUnsupported, so the specific reason stays
// distinguishable -- or a Per so large that windowTTLFactor*Per would
// overflow a time.Duration -- see maxPer). A limiter is a security-relevant
// primitive: garbage input from a caller is a programmer error and must fail
// loudly and unambiguously, not be silently reinterpreted as always-allow or
// always-deny. Callers classify it with errors.Is.
var ErrInvalidLimit = errors.New("ratelimit: invalid limit")

// ErrRateOneUnsupported is returned by Allow -- in place of, and wrapping,
// ErrInvalidLimit -- when the supplied Limit has Rate == 1. Whatever Per it
// is paired with, this limiter cannot honour a Rate of 1 literally: "once
// per Per" would require the one hit a window admits to stop counting
// against the window after it, and it never does. The weighted sliding-window
// formula combined with "every call is a hit" (see slidingWindowLimiter's own
// doc comment, "Saturating clients permanently lose one slot per window")
// instead collapses Rate == 1 into "allowed exactly once ever, then denied
// forever": the very first window admits the one hit, every later window
// admits none, and the attempts those windows deny keep their own counters at
// 1, handing the same permanent refusal to each next window. A Rate of 2 or
// more always leaves at least Rate-1 admission per window under the same
// sustained saturation, so the collapse to zero is unique to Rate 1.
//
// Accepting a Limit whose literal semantics cannot be honoured would hand a
// host that wrote Rate: 1 meaning "once per Per" -- the natural spelling of
// "once a day" -- a limiter that locks the key out forever after its first
// use instead, with the permanent-lockout behaviour arriving in production
// only once the window rolls over. Refusal is the only honest answer, on the
// same reasoning the jobs module applies to its own un-honourable
// construction values (WithConcurrency and WithWorkerCount refuse a value
// below 1 at option time because asynqlib.NewServer would silently
// reinterpret it; see go/jobs/queue/asynq): a coded refusal beats silently
// delivering a different, permanently-denying behaviour. Callers that treat
// any invalid Limit as a programmer error keep classifying this one with
// errors.Is(err, ErrInvalidLimit); callers that need the specific reason
// match the code "ratelimit.rate_one_unsupported" with errors.As, or compare
// against this sentinel with errors.Is (Allow returns it undecorated).
//
// A host that genuinely wants roughly one admission per interval should use
// Rate: 2 with the interval as Per: at most two requests are admitted per
// window, and a caller whose attempts genuinely stay within one per window
// is never denied. An exactly-once-per-interval guarantee is not expressible
// at any Rate (a straddling pair of attempts around a window boundary would
// defeat it) and belongs in the caller's own last-success gate, not in a
// rate-limit window.
var ErrRateOneUnsupported = apperr.Invalid("ratelimit.rate_one_unsupported").WithCause(ErrInvalidLimit)

// Limit is the rate a key is allowed to be hit at: Rate occurrences per Per
// duration. Rate must be at least 2 -- a Rate of 1, whatever Per it is
// paired with, cannot be honoured literally by the sliding-window formula
// and is refused with ErrRateOneUnsupported (see that error's own doc
// comment) -- and Per must be strictly positive and no larger than maxPer;
// Allow rejects anything else with an error wrapping ErrInvalidLimit rather
// than guessing what an invalid Limit was meant to do.
type Limit struct {
	// Rate is the number of requests allowed within Per. Values below 2
	// are refused: see Limit's own doc comment and ErrRateOneUnsupported.
	Rate int
	// Per is the length of the window Rate applies to.
	Per time.Duration
}

// validate reports ErrInvalidLimit when l cannot be used to make a decision.
func (l Limit) validate() error {
	if l.Rate <= 0 || l.Per <= 0 {
		return fmt.Errorf("%w: rate=%d per=%s (both must be > 0)", ErrInvalidLimit, l.Rate, l.Per)
	}
	if l.Rate == 1 {
		// See ErrRateOneUnsupported for why Rate == 1 is refused: whatever
		// Per it is paired with, the weighted sliding-window formula cannot
		// honour "once per Per" -- it would collapse into "allowed once ever,
		// then denied forever". Refusing here, before the store is touched,
		// beats accepting a Limit whose literal semantics are undeliverable.
		return ErrRateOneUnsupported
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
// KVStore.IncrByFloatWithTTL, which is unconditionally called first, before
// limit is even consulted for the decision, passing windowTTLFactor*limit.Per
// as the ttl on every single call. IncrByFloatWithTTL's own doc comment
// promises it is safe for concurrent use, and that a fresh key is stored
// with the given ttl already attached in the same atomic step that creates
// it, while a key that already exists is incremented with its own existing
// expiry left untouched — so no concurrent caller ever loses its own
// increment, on a fresh key or a live one alike, and no separate call or
// gate is needed to attach the window's expiry on the specific hit that
// happens to create the key. See "The TTL-attachment race, closed" below
// for why this collapses what used to be a two-call, race-prone sequence
// into one atomic primitive call.
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
// after that, forever, with no self-recovery. At Rate == 1 — where
// "at most limit.Rate-1" is zero — that collapse is a complete, permanent
// lockout from a key's second window on, whatever Per, which is exactly why
// Limit.validate refuses a Rate of 1 outright (ErrRateOneUnsupported):
// "once per Per" cannot be honoured literally by this formula, and a coded
// refusal beats silently delivering "allowed exactly once ever, then denied
// forever" to a host that wrote Rate: 1 meaning "once a day".
//
// This is safe-direction (it never over-admits) and a faithful consequence
// of the weighted formula above combined with "every call is a hit"
// (Limiter's own doc comment), not a defect in this implementation of that
// formula — see AGENTS.md's Known limitations for the full analysis,
// including why avoiding it needs a different, unimplemented algorithm
// (increment only on an allowed call) rather than a correction to this one.
// TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow in
// limiter_test.go pins the Rate >= 2 shape down deterministically,
// independent of intra-window timing, and
// TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched pins the
// Rate == 1 refusal.
//
// # The TTL-attachment race, closed
//
// Earlier versions of this package attached a freshly-created window key's
// expiry with a caller-side Get-then-Set sequence run only on the specific
// hit whose IncrByFloat call created the key (the classic Redis
// INCR-then-EXPIRE-if-first idiom), because KVStore exposed no primitive
// that could set an expiry and increment atomically in one step. That
// sequence had two disclosed failure modes: a process crash between the
// increment and the Set left a window's counter permanently without an
// expiry (harmless -- nothing ever reads a window other than "current" or
// "immediately previous" again), and, far more importantly, a concurrent
// caller's own increment landing in the residual gap between the Get and
// the Set was silently overwritten by that Set, undercounting the window
// and admitting more traffic than configured -- a real, security-relevant
// over-admit gap, not a cosmetic one, recurring on every window boundary
// for as long as a caller's key kept being hit (windowKey mints a
// brand-new, never-before-used storage key every single Per interval), with
// no hard bound on how much a single burst could lose.
//
// pkgcore.KVStore.IncrByFloatWithTTL closes that gap completely by
// collapsing "increment" and "attach the window's expiry, but only on the
// hit that creates the key" into one atomic KVStore call: every backend
// implements it as a single atomic operation extending whatever mechanism
// already makes its own IncrByFloat atomic (a mutex-guarded map update, a
// single Lua script, one database-arbitrated upsert, a compare-and-swap
// retry loop -- see go/pkgcore/kv's own per-backend doc comments), never as
// a caller-side sequence with a gap between two separate calls for a
// concurrent increment to land in. Allow below calls it unconditionally on
// every hit, passing windowTTLFactor*limit.Per as the ttl: the primitive
// itself ignores that ttl for a key that already exists, so there is no
// gate to get wrong and no residual race left to document -- both of the
// old failure modes (the crash-only gap and the concurrency-only gap) are
// gone, not merely narrowed, because there is no longer a second call for
// either one to land between.
//
// TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost and
// TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached
// in limiter_test.go pin this down: the former reproduces the exact
// pre-fix sequence deterministically (it fails against the old
// two-call implementation and passes against this one, since the fix
// makes the vulnerable call unreachable at all), and the latter races
// hundreds of goroutines to create one fresh window key at once and proves
// both that no increment is lost and that the key still ends up with its
// ttl attached.
//
// Do not "fix" a future correctness question in this algorithm by reading
// the key before calling IncrByFloatWithTTL to decide whether to skip
// straight to some other call: a Get-then-branch ahead of the atomic
// increment reintroduces a real lost-update race on every hit, not just the
// first one in a window, which is strictly worse than anything this section
// used to document.
type slidingWindowLimiter struct {
	store pkgcore.KVStore
}

const (
	// windowTTLFactor sizes the expiry attached to a window's counter key as
	// a multiple of the window length. It must be at least 2: a window's key
	// can be created as early as the very start of its own window, and it
	// must still be readable as "the previous window" for a read landing as
	// late as just before the *next* window ends — a span of up to two full
	// window lengths after the key's own creation.
	windowTTLFactor = 2

	// maxPer is the largest Limit.Per that Limit.validate accepts. Allow
	// computes windowTTLFactor*Per as a time.Duration (the ttl passed to
	// IncrByFloatWithTTL below), and time.Duration is a signed 64-bit count
	// of nanoseconds: a Per above this bound would make that multiplication
	// overflow and silently wrap to a negative duration. KVStore.Set's own
	// documented contract says "a ttl of zero or less stores the key without
	// an expiry", so a negative ttl reaching it would pin the window's
	// counter key in the store forever instead of letting it expire with its
	// window — a permanently-broken state triggered by a Limit value that
	// looks like nothing worse than an unusually long window. A Per this
	// large is never a legitimate rate-limit window, so validate rejects it
	// up front, before Allow ever computes the overflowing multiplication.
	maxPer = time.Duration(math.MaxInt64 / windowTTLFactor)

	// floatEncodingBitSize mirrors pkgcore's own (unexported)
	// KVStore.IncrByFloat numeric encoding width (see go/pkgcore/kv.go's
	// kvFloatBitSize): the width readWindowCount parses a stored window
	// counter's value with.
	floatEncodingBitSize = 64
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

	currentCount, err := l.store.IncrByFloatWithTTL(ctx, currentKey, 1, time.Duration(windowTTLFactor)*limit.Per)
	if err != nil {
		return Decision{}, err
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
