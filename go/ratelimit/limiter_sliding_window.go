package ratelimit

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

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
// The instant t is the calling process's own local wall clock: Allow
// reads time.Now at the moment of the call and derives windowIndex and
// elapsedFraction from it, and New accepts no clock to substitute (the
// package is dependency-free by design) — windows advance by the
// process's local wall clock, never by a time the store or a peer
// supplies. This clock source is the limiter's own behaviour, not a
// policy some other layer chose, and in a deployment where replicas
// share one KVStore it makes the limiter's cross-replica behaviour
// depend on the replicas' clocks agreeing: each replica buckets its hits
// and weights them against boundaries of its own clock, so the shared
// counters describe one coherent sliding window only while the replicas'
// clocks agree, and a replica whose clock disagrees writes its hits into
// keys its peers read as a different window and reads their counters
// through shifted boundaries.
//
// The agreement is exercised for real in this repository:
// examples/reference-app/integration_test/distributed_mode_test.go is a
// two-process composition of this limiter — two replicas sharing one
// Redis-backed KVStore — whose cross-replica lockout assertions rely on
// it: they expect rate-limiting state one replica recorded under its own
// process clock to hold when the other reads it under its own, which
// holds because both processes share one host clock.
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
// happens to create the key. See "The TTL-attachment race" below for why
// the whole sequence is one atomic primitive call.
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
// TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed in
// limiter_sliding_window_test.go for a case that would fail under a naive
// fixed-window implementation.
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
// formula. Avoiding it would need a different, unimplemented algorithm
// (increment only on an allowed call), not a correction to this one.
// TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow in
// limiter_sliding_window_test.go pins the Rate >= 2 shape down
// deterministically,
// independent of intra-window timing, and
// TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched pins the
// Rate == 1 refusal.
//
// # The TTL-attachment race
//
// The window's expiry must be attached atomically with the increment that
// creates the key. A two-call sequence — increment, then attach the expiry
// only on the hit that created the key — carries two failure modes: a
// process crash between the two calls leaves a window's counter without an
// expiry (harmless — nothing ever reads a window other than "current" or
// "immediately previous" again), and a concurrent caller's own increment
// landing in the gap between them is silently overwritten by the second
// call, undercounting the window and admitting more traffic than
// configured — a security-relevant over-admit gap with no hard bound on
// how much a single burst could lose (windowKey mints a brand-new,
// never-before-used storage key every single Per interval, so the gap
// would recur on every window boundary for as long as a caller's key kept
// being hit).
//
// pkgcore.KVStore.IncrByFloatWithTTL exists precisely so the whole
// sequence is one atomic KVStore call: every backend implements it as a
// single atomic operation extending whatever mechanism already makes its
// own IncrByFloat atomic (a mutex-guarded map update, a single Lua script,
// one database-arbitrated upsert, a compare-and-swap retry loop — see
// go/pkgcore/kv's own per-backend doc comments), never as a caller-side
// sequence with a gap between two separate calls for a concurrent
// increment to land in. Allow below calls it unconditionally on every hit,
// passing windowTTLFactor*limit.Per as the ttl: the primitive itself
// ignores that ttl for a key that already exists, so there is no gate to
// get wrong and no second call for either failure mode to land between.
//
// TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost and
// TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached
// in limiter_sliding_window_test.go pin this down: the former reproduces the
// two-call
// interleaving deterministically (it fails against a two-call
// implementation and passes against this one, since the vulnerable call is
// unreachable), and the latter races hundreds of goroutines to create one
// fresh window key at once and proves both that no increment is lost and
// that the key still ends up with its ttl attached.
//
// Do not "fix" a future correctness question in this algorithm by reading
// the key before calling IncrByFloatWithTTL to decide whether to skip
// straight to some other call: a Get-then-branch ahead of the atomic
// increment reintroduces a real lost-update race on every hit, not just
// the first one in a window — strictly worse than the hazards above.
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
	// KVStore.IncrByFloat numeric encoding width (see
	// go/pkgcore/kv_memory.go's kvFloatBitSize): the width
	// readWindowCount parses a stored window
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
// it is a real portability bug, not defensive-programming caution.
// Limit.validate requires only Rate > 0 -- it has no upper bound, and a
// large sentinel such as math.MaxInt is a real convention some callers use
// for "effectively unlimited" instead of skipping the Allow call
// altogether. float64 can only represent integers exactly up to 2^53, so
// float64(limit.Rate) for a Rate that large has already rounded up past
// int64's own maximum by the time this function is ever called: math.MaxInt
// on a 64-bit platform is 2^63-1, which rounds to the nearest representable
// float64, 2^63 -- one past what int64 can hold. The Go spec (Conversions)
// is explicit about what happens next: "if the result type cannot represent
// the value the conversion succeeds but the result value is
// implementation-defined" -- and the two supported architectures differ
// exactly as the spec allows: arm64's FCVTZS saturates on overflow and
// yields math.MaxInt64, while amd64's CVTTSD2SI leaves the x86 "integer
// indefinite" bit pattern, which equals math.MinInt64. Left unclamped, an
// amd64 deployment would observe Decision{Allowed: true, Remaining:
// math.MinInt64} for input that looks entirely correct in local arm64
// development -- a badly wrong signal for anything that inspects Remaining,
// such as a quota header or a "Remaining <= 0" fallback check. Clamping
// first guarantees the int(...)
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
// opaque, caller-owned string throughout this package; this function only
// ever builds a windowKey, it never parses one back apart.
func windowKey(key string, windowIndex int64) string {
	return key + ":" + strconv.FormatInt(windowIndex, 10)
}
