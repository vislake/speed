package ratelimit

import (
	"context"
	"errors"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// waitForFreshWindowStart blocks until shortly after a real window boundary
// for a window of length per, so the caller gets close to a full per of
// runway before the window it lands in ends. Several tests below need to
// control where, relative to a real window boundary, their calls land;
// since Allow always derives its window from the real wall clock (matching
// its documented, dependency-free New(store) constructor, which takes no
// injectable clock), waiting for a real boundary is the only way to do that
// deterministically enough for assertions on sliding/expiry behavior.
func waitForFreshWindowStart(per time.Duration) time.Time {
	for {
		now := time.Now()
		perNanos := int64(per)
		idx := now.UnixNano() / perNanos
		start := time.Unix(0, idx*perNanos)
		if now.Sub(start) < per/10 {
			return start
		}
		time.Sleep(time.Until(start.Add(per)))
	}
}

func TestAllow_InvalidLimit_ReturnsErrInvalidLimit(t *testing.T) {
	tests := []struct {
		name  string
		limit Limit
	}{
		{name: "zero rate", limit: Limit{Rate: 0, Per: time.Second}},
		{name: "negative rate", limit: Limit{Rate: -1, Per: time.Second}},
		{name: "rate one", limit: Limit{Rate: 1, Per: time.Second}},
		{name: "zero per", limit: Limit{Rate: 5, Per: 0}},
		{name: "negative per", limit: Limit{Rate: 5, Per: -time.Second}},
		{name: "both zero", limit: Limit{Rate: 0, Per: 0}},
		// Per is positive and well within time.Duration's own ~292-year
		// range, but windowTTLFactor*Per (see Allow's call to
		// IncrByFloatWithTTL) is computed as a time.Duration, so doubling a
		// Per this large overflows int64 nanoseconds and wraps to a negative
		// duration. A negative ttl reaching KVStore.IncrByFloatWithTTL would
		// store the window's counter key with no expiry at all (its own
		// documented contract: "a ttl of zero or less stores it without
		// one"), pinning it in the store forever instead of letting it age
		// out with its window.
		{name: "per so large 2x overflows time.Duration", limit: Limit{Rate: 5, Per: 200 * 365 * 24 * time.Hour}},
	}

	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec, err := lim.Allow(ctx, "invalid-limit-key", tt.limit)
			if !errors.Is(err, ErrInvalidLimit) {
				t.Fatalf("err = %v, want an error wrapping ErrInvalidLimit", err)
			}
			if dec != (Decision{}) {
				t.Fatalf("Decision = %+v, want the zero value on a validation error", dec)
			}
		})
	}
}

// TestAllow_PerAtMaxBoundary_StillAccepted proves the upper bound added to
// Limit.validate is exact, not off by one in the safe direction: maxPer
// itself -- the largest Per for which windowTTLFactor*Per still fits in a
// time.Duration without overflowing, see maxPer's own doc comment -- must
// still be accepted and produce a normal, working Decision. This guards
// against the overflow fix over-correcting and rejecting the largest Per
// that is actually safe to use.
func TestAllow_PerAtMaxBoundary_StillAccepted(t *testing.T) {
	// Rate 2, never 1: a Rate of 1 is itself refused by validate these days
	// (ErrRateOneUnsupported), and this test is about the Per bound, not the
	// Rate bound.
	limit := Limit{Rate: 2, Per: maxPer}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()

	dec, err := lim.Allow(ctx, "max-per-boundary", limit)
	if err != nil {
		t.Fatalf("Allow with Per at the maximum accepted boundary (%s): %v", maxPer, err)
	}
	if !dec.Allowed {
		t.Fatalf("Decision.Allowed = false, want true for a first hit within limit")
	}
}

func TestAllow_ExactlyAtLimit_AllowedThenOneOverDenied(t *testing.T) {
	const rate = 5
	limit := Limit{Rate: rate, Per: time.Hour} // long window: isolates this case from sliding effects
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "at-limit"

	for i := 1; i <= rate; i++ {
		dec, err := lim.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("hit %d: Allow: %v", i, err)
		}
		if !dec.Allowed {
			t.Fatalf("hit %d: Allowed = false, want true (at or under the limit)", i)
		}
		if want := rate - i; dec.Remaining != want {
			t.Fatalf("hit %d: Remaining = %d, want %d", i, dec.Remaining, want)
		}
	}

	dec, err := lim.Allow(ctx, key, limit)
	if err != nil {
		t.Fatalf("hit %d (one over): Allow: %v", rate+1, err)
	}
	if dec.Allowed {
		t.Fatalf("hit %d (one over the limit): Allowed = true, want false", rate+1)
	}
	if dec.Remaining != 0 {
		t.Fatalf("hit %d (one over the limit): Remaining = %d, want 0", rate+1, dec.Remaining)
	}
}

// TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed proves the sliding part of
// "sliding-window counter": a burst that fills a window, followed by another
// burst just after that window's boundary, must not let close to 2x the
// configured rate through. A naive fixed-window implementation resets its
// count to zero at every boundary with no memory of the previous window, so
// it would allow the full second burst too -- this test would fail against
// that implementation and passes only because previousCount's weight is
// carried forward.
func TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed(t *testing.T) {
	const per = 200 * time.Millisecond
	const rate = 10
	limit := Limit{Rate: rate, Per: per}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "boundary-burst"

	windowStart := waitForFreshWindowStart(per)

	firstAllowed := 0
	for i := 0; i < rate; i++ {
		dec, err := lim.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("first burst hit %d: Allow: %v", i, err)
		}
		if dec.Allowed {
			firstAllowed++
		}
	}
	if firstAllowed != rate {
		t.Fatalf("first burst allowed = %d, want %d (fully allowed: it exactly fills a fresh window)", firstAllowed, rate)
	}

	// Sleep to just past this window's boundary -- only a small way into the
	// next one, so the previous window's weight is still almost entirely in
	// effect (elapsedFraction close to zero).
	sleepUntil := windowStart.Add(per).Add(per / 10)
	if d := time.Until(sleepUntil); d > 0 {
		time.Sleep(d)
	}

	secondAllowed := 0
	for i := 0; i < rate; i++ {
		dec, err := lim.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("second burst hit %d: Allow: %v", i, err)
		}
		if dec.Allowed {
			secondAllowed++
		}
	}

	// Generous threshold, robust to scheduler jitter in the sleeps above: a
	// naive fixed-window implementation would score rate (all of it) here,
	// so anything meaningfully below that proves the previous window's
	// count was carried forward and weighted against this burst.
	if want := rate / 2; secondAllowed > want {
		t.Fatalf("second burst (just past the window boundary) allowed = %d, want <= %d; "+
			"a naive fixed-window implementation would have allowed all %d of it", secondAllowed, want, rate)
	}
}

// TestAllow_ResetAfter_TracksTimeRemainingInWindow proves Decision.ResetAfter
// actually implements its documented formula -- "how long until the current
// window ends", i.e. limit.Per minus the elapsed time into the current
// window (see Decision.ResetAfter's own doc comment, and the "Allowed is
// whether weighted..." paragraph of slidingWindowLimiter's doc comment) --
// rather than merely existing as a field nothing reads. No other test in
// this file, and no Example in example_test.go, ever inspects
// Decision.ResetAfter: every one of them checks only Allowed and Remaining.
// A regression that swapped the operands (returning the *elapsed* time
// instead of the remaining time), always returned zero, always returned
// limit.Per unconditionally, or flipped the sign, would compile and pass
// every other test in this file silently -- exactly what this test exists
// to catch.
//
// The test samples Allow at two known offsets into the same real window --
// one quarter and three quarters of the way through -- and checks
// ResetAfter two ways at each point: an absolute check against the value
// the documented formula predicts (a tolerance proportional to per, the
// same style TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed above uses,
// to absorb scheduler jitter between "when the test measured its offset"
// and "when Allow itself called time.Now()"), and a relative check that
// ResetAfter strictly decreased between the two samples. The relative
// check is what makes a swapped-operand regression unmissable regardless
// of any timing slack shared by both samples: the documented formula's
// ResetAfter always decreases as elapsed time grows, while its swapped
// form (elapsed itself) would instead increase across the same two
// samples.
func TestAllow_ResetAfter_TracksTimeRemainingInWindow(t *testing.T) {
	const per = 500 * time.Millisecond
	const tolerance = per / 5
	limit := Limit{Rate: 1000, Per: per} // high rate: this test is about timing, not admission
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "reset-after"

	windowStart := waitForFreshWindowStart(per)

	sampleAt := func(offset time.Duration) Decision {
		target := windowStart.Add(offset)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}

		dec, err := lim.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("Allow at offset %s into the window: %v", offset, err)
		}

		// Invariant regardless of timing precision: while still inside the
		// window, ResetAfter must describe time remaining in *this* window,
		// never the whole window back again (an "always return limit.Per"
		// regression), and never zero or negative (a sign-flip or
		// always-zero regression).
		if dec.ResetAfter <= 0 || dec.ResetAfter > per {
			t.Fatalf("offset %s: ResetAfter = %s, want a value in (0, %s]", offset, dec.ResetAfter, per)
		}

		elapsed := time.Since(windowStart)
		want := per - elapsed
		if diff := dec.ResetAfter - want; diff < -tolerance || diff > tolerance {
			t.Fatalf("offset %s: ResetAfter = %s, want approximately %s (per %s minus elapsed %s), outside tolerance %s",
				offset, dec.ResetAfter, want, per, elapsed, tolerance)
		}

		return dec
	}

	early := sampleAt(per / 4)
	late := sampleAt(3 * per / 4)

	if late.ResetAfter >= early.ResetAfter {
		t.Fatalf("ResetAfter did not decrease as the window elapsed: early (offset %s) = %s, late (offset %s) = %s -- "+
			"want late < early; a regression returning elapsed time instead of remaining time would increase instead",
			per/4, early.ResetAfter, 3*per/4, late.ResetAfter)
	}
}

// TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow proves a
// documented, deliberately-kept characteristic of the weighted formula
// itself (see slidingWindowLimiter's own doc comment, "Saturating clients
// permanently lose one slot per window"): once a window's recorded count
// has reached limit.Rate, the Rate-th
// sequential Allow call of every window after that is denied
// unconditionally, forever -- a client that always attempts exactly Rate
// hits per window is admitted the full Rate only in the very first window a
// key is ever used in.
//
// Unlike TestAllow_SlidingWindow_BoundaryBurst_IsSmoothed above, this
// assertion needs no control over exactly *where inside* the window each
// hit lands: at the Rate-th sequential call, currentCount is exactly rate
// regardless of timing (IncrByFloat's own gapless, one-at-a-time
// increments), so weighted = rate + previousCount*(1-elapsedFraction).
// Once previousCount >= rate, that sum exceeds rate for every
// elapsedFraction in [0,1) -- elapsedFraction can never reach exactly 1
// while still inside the window -- so Allowed is false no matter when the
// call lands. Only landing all rate hits of one test-window inside that
// same real window needs any timing care at all, which
// waitForFreshWindowStart's runway already provides.
func TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow(t *testing.T) {
	const per = 200 * time.Millisecond
	const rate = 5
	const windows = 3
	limit := Limit{Rate: rate, Per: per}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "saturating-client"

	windowStart := waitForFreshWindowStart(per)

	for w := 0; w < windows; w++ {
		target := windowStart.Add(time.Duration(w) * per).Add(per / 10)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}

		var last Decision
		for i := 1; i <= rate; i++ {
			dec, err := lim.Allow(ctx, key, limit)
			if err != nil {
				t.Fatalf("window %d hit %d: Allow: %v", w, i, err)
			}
			last = dec
		}

		if w == 0 {
			// A fresh key: previousCount is 0, so the full rate is admitted,
			// matching TestAllow_ExactlyAtLimit_AllowedThenOneOverDenied.
			if !last.Allowed {
				t.Fatalf("window 0 hit %d (the Rate-th, on a fresh key): Allowed = false, want true", rate)
			}
			continue
		}

		// Every window from here on inherits previousCount == rate from the
		// one before it (window 0 admitted exactly rate; every later
		// window's own rate attempts, denied or not, still bring its own
		// recorded count to rate -- "every call is a hit"). The Rate-th hit
		// is therefore denied unconditionally -- see this test's own doc
		// comment for why that holds regardless of timing.
		if last.Allowed {
			t.Fatalf("window %d hit %d (the Rate-th): Allowed = true, want false -- "+
				"a previous window at the full rate unconditionally denies this window's "+
				"own Rate-th hit too, self-sustaining forever (see doc comment)", w, rate)
		}
	}
}

// TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched is the
// regression test for the Rate == 1 refusal (see ErrRateOneUnsupported): a
// Limit{Rate: 1} -- the natural spelling of "once per Per", and of "once a
// day" in production -- is rejected by Limit.validate before the store is
// ever touched, with an error that both wraps ErrInvalidLimit (so callers
// that classify every invalid Limit alike keep working) and carries the
// distinct code "ratelimit.rate_one_unsupported" (so a host whose Rate-1
// configuration is refused can recognize exactly what was wrong).
//
// A Rate-1 Limit's literal semantics cannot be honoured by the weighted
// formula: the first-ever hit would be Allowed and every hit in every
// later window denied forever -- the permanent lockout a host pairing
// Rate: 1 with Per: 24h would meet a day after the key's first use, the
// opposite of the "once per Per" semantics it meant (see
// ErrRateOneUnsupported's own doc comment for the full reasoning).
//
// The erroringKVStore wrapper below (failMethod IncrByFloatWithTTL) makes
// the "before the store is ever touched" promise part of the assertion:
// if validate ever stopped short of refusing a Rate-1 Limit, Allow would
// reach the wrapper's failing IncrByFloatWithTTL and surface the
// wrapper's own error instead, failing the ErrInvalidLimit assertions.
// Deterministic, with no timing involved.
func TestAllow_RateOne_RefusedWithCodedReasonBeforeStoreTouched(t *testing.T) {
	limit := Limit{Rate: 1, Per: time.Hour}
	ctx := context.Background()

	fake := &erroringKVStore{
		KVStore:    pkgcore.NewMemoryKVStore(),
		failMethod: "IncrByFloatWithTTL",
		err:        errors.New("store must not be touched for a Rate-1 limit"),
	}
	dec, err := New(fake).Allow(ctx, "rate-one-key", limit)

	if !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("err = %v, want an error wrapping ErrInvalidLimit (Rate 1 is an invalid limit like any other)", err)
	}
	if !errors.Is(err, ErrRateOneUnsupported) {
		t.Fatalf("err = %v, want the ErrRateOneUnsupported refusal -- the Rate-1 case must stay distinguishable from the generic invalid limit", err)
	}
	var appErr *apperr.Error
	if !errors.As(err, &appErr) {
		t.Fatalf("err = %v, want an *apperr.Error to match on its code", err)
	}
	if appErr.Code != "ratelimit.rate_one_unsupported" {
		t.Fatalf("apperr code = %q, want %q", appErr.Code, "ratelimit.rate_one_unsupported")
	}
	if dec != (Decision{}) {
		t.Fatalf("Decision = %+v, want the zero value on a validation error", dec)
	}
}

// TestAllow_WindowExpiry_OldWindowKeyExpires proves the expiry Allow passes
// with every hit is genuinely attached to the window's storage key, so the
// key must eventually become unreadable in the underlying KVStore, not
// accumulate forever: Allow records the hit through
// pkgcore.KVStore.IncrByFloatWithTTL, passing windowTTLFactor*per as the
// ttl, and that primitive stores a freshly-created key with the ttl already
// attached in the same atomic step (per its own contract the ttl applies to
// the hit that creates the key and is ignored for a key that already
// exists). The key therefore exists right after the first hit and is gone
// once windowTTLFactor*per has elapsed. What attaches the expiry and keeps
// it safe under concurrent first hits is pinned by
// TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost and
// TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached;
// this test pins the end-to-end result against the real store.
func TestAllow_WindowExpiry_OldWindowKeyExpires(t *testing.T) {
	const per = 50 * time.Millisecond
	store := pkgcore.NewMemoryKVStore()
	lim := New(store)
	ctx := context.Background()
	limit := Limit{Rate: 100, Per: per}
	key := "expiry-test"

	// Land solidly at the start of a real window so the window index
	// computed here for the storage key matches the one Allow computes for
	// the same hit an instant later.
	windowStart := waitForFreshWindowStart(per)
	windowIndex := windowStart.UnixNano() / int64(per)
	storeKey := windowKey(key, windowIndex)

	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	if _, found, err := store.Get(ctx, storeKey); err != nil || !found {
		t.Fatalf("Get right after the first hit: found=%t err=%v, want found=true", found, err)
	}

	// windowTTLFactor * per is the ttl IncrByFloatWithTTL attaches; sleep
	// comfortably past it so the key is guaranteed to have expired.
	time.Sleep(time.Duration(windowTTLFactor)*per + 100*time.Millisecond)

	if _, found, err := store.Get(ctx, storeKey); err != nil || found {
		t.Fatalf("Get once the TTL should have elapsed: found=%t err=%v, want found=false", found, err)
	}
}

// TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit is the concurrency
// safety case: many simultaneous callers against the same key must never
// collectively see more Allowed=true decisions than the configured limit.
// Per is a full minute, well beyond how long this test takes to run, so it
// exercises exactly one window and isolates concurrency correctness from
// the sliding-window behavior already covered above. Run with -race.
//
// The window is warmed up with one sequential call before the concurrent
// burst starts, so every hit in the burst that follows lands against a key
// that already exists -- isolating "many concurrent hits against a live
// key" from "many concurrent hits racing to create a fresh one", which
// TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached
// covers separately (pkgcore.KVStore.IncrByFloatWithTTL makes a fresh
// key's creation race-free too -- see slidingWindowLimiter's doc comment,
// "The TTL-attachment race"). This test keeps its warm-up because it is
// the cleanest way to isolate "concurrent hits against a live key" as its
// own, narrower claim.
//
// The outcome is not just bounded but exactly determined: with Per a full
// minute and no prior activity, every one of the 200 concurrent calls lands
// in the same window as the warm-up and sees previousCount == 0, so each
// one's Decision depends only on the distinct, gapless count IncrByFloat
// hands it (2, 3, ..., 201, in whatever order the calls happen to
// interleave in -- IncrByFloat's atomicity guarantees the sequence is
// gapless and duplicate-free, never which caller receives which number).
// Exactly 49 of those 200 values (2 through 50 inclusive) are <= rate, so
// exactly 49 concurrent calls must show Allowed, regardless of goroutine
// scheduling -- a stronger, still entirely reliable claim than merely
// bounding the total, and one a race in the counting would visibly break.
func TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit(t *testing.T) {
	const rate = 50
	const callers = 200
	limit := Limit{Rate: rate, Per: time.Minute}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "concurrent-key"

	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("warm-up Allow: %v", err)
	}

	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			dec, err := lim.Allow(ctx, key, limit)
			if err != nil {
				t.Errorf("Allow: %v", err)
				return
			}
			if dec.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()

	// +1 accounts for the warm-up hit (currentCount 1, always allowed) that
	// is not part of the counted goroutine burst.
	if got := allowed.Load() + 1; got > rate {
		t.Fatalf("allowed = %d (including the warm-up hit), want <= %d (the configured limit) across %d concurrent callers plus the warm-up",
			got, rate, callers)
	}
	// Not just "<= rate": the exact value the reasoning above predicts,
	// deterministically, every run. A weaker "<= rate" check alone would
	// not catch a bug that drops or duplicates counts while happening to
	// stay under the limit.
	if got := allowed.Load(); got != rate-1 {
		t.Fatalf("allowed = %d among the 200 concurrent callers, want exactly %d (see this test's own doc comment for why this is deterministic, not just bounded)",
			got, rate-1)
	}
}

func TestAllow_IndependentKeys_DoNotInterfere(t *testing.T) {
	limit := Limit{Rate: 3, Per: time.Minute}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()

	for i := 1; i <= limit.Rate; i++ {
		dec, err := lim.Allow(ctx, "keyA", limit)
		if err != nil || !dec.Allowed {
			t.Fatalf("priming keyA hit %d: allowed=%t err=%v, want allowed=true", i, dec.Allowed, err)
		}
	}
	dec, err := lim.Allow(ctx, "keyA", limit)
	if err != nil {
		t.Fatalf("Allow(keyA, over limit): %v", err)
	}
	if dec.Allowed {
		t.Fatalf("keyA should now be over its limit, got Allowed = true")
	}

	// keyB is a distinct, fresh key sharing the same Limit value -- it must
	// be entirely unaffected by keyA's usage.
	dec, err = lim.Allow(ctx, "keyB", limit)
	if err != nil {
		t.Fatalf("Allow(keyB): %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("keyB should be unaffected by keyA's usage, got Allowed = false")
	}
	if want := limit.Rate - 1; dec.Remaining != want {
		t.Fatalf("keyB Remaining = %d, want %d (first hit on an independent, fresh key)", dec.Remaining, want)
	}
}

func TestAllow_ContextCanceled_ReturnsContextError(t *testing.T) {
	lim := New(pkgcore.NewMemoryKVStore())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dec, err := lim.Allow(ctx, "any-key", Limit{Rate: 5, Per: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want an error wrapping context.Canceled", err)
	}
	if dec != (Decision{}) {
		t.Fatalf("Decision = %+v, want the zero value on error", dec)
	}
}

// setInjectsConcurrentIncrementKVStore wraps a real KVStore and, on every
// Set call against injectKey, first performs a real IncrByFloat(+1) against
// that same key (the promoted method of the embedded store, since this type
// itself never overrides IncrByFloat) before delegating to the embedded
// store's own Set. injected records whether this ever actually fired, so a
// test can compute how many real increments landed against the key
// regardless of which code path Allow takes to get there.
//
// This models a concurrent caller's increment landing in the gap of a
// two-call expiry attachment: a Get of the key's current value followed by
// a Set writing it back with a ttl attached. Anything that increments the
// key between those two calls is exactly what this wrapper's Set override
// reproduces deterministically, with no goroutines or scheduler luck
// involved (see TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost
// below for the proof, and slidingWindowLimiter's "The TTL-attachment
// race" for the race itself).
type setInjectsConcurrentIncrementKVStore struct {
	pkgcore.KVStore
	injectKey string
	injected  bool
}

func (s *setInjectsConcurrentIncrementKVStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if key == s.injectKey {
		s.injected = true
		if _, err := s.IncrByFloat(ctx, key, 1); err != nil {
			return err
		}
	}
	return s.KVStore.Set(ctx, key, value, ttl)
}

// TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost proves
// deterministically -- with no goroutines, timing, or scheduler luck
// involved -- that no increment is ever lost to a caller-side TTL-attach
// gap, the security-relevant over-admit failure mode a two-call
// "increment, then attach the expiry on creation" sequence would carry (a
// concurrent increment landing in the gap between the Get and the Set is
// silently overwritten, undercounting the window with no hard bound; see
// slidingWindowLimiter's "The TTL-attachment race"). The interleaving is
// forced: setInjectsConcurrentIncrementKVStore's Set override performs the
// "concurrent" increment itself, synchronously, at the exact instant a
// caller-side Set write-back would have raced against one.
//
// A two-call Allow against a fresh key would run exactly this sequence:
// IncrByFloat creates the key at "1"; since that is the first hit in the
// window, a caller-side expiry attachment Gets "1" back, then calls Set
// with a ttl -- and this wrapper's Set override fires first, bumping the
// real store's value to "2" via a genuine IncrByFloat call, before the
// caller-side Set proceeds to write back the stale "1" it read a moment
// earlier (now carrying a ttl, but the wrong number). Two real increments
// would have happened against the key (the Allow hit itself, and the
// injected one), but the final stored value would reflect only one of
// them -- exactly the silent undercount this assertion fails on for a
// two-call implementation.
//
// Allow's actual path collapses "increment" and "attach the ttl, but only
// on creation" into one atomic pkgcore.KVStore.IncrByFloatWithTTL call,
// with no caller-side Set anywhere on that path -- so this wrapper's Set
// override is never reached at all (fake.injected stays false), the
// wrapper's own injected increment never happens, and the store correctly
// reflects the single real increment the Allow call actually made. The
// assertion below computes its expected count from whether the injection
// actually fired (rather than hardcoding it), so it is a meaningful
// regression proof in both directions: it fails if a real increment ever
// goes missing while the injection point is reachable, and it passes,
// non-vacuously, precisely because the injection point is structurally
// unreachable from Allow.
func TestAllow_ConcurrentIncrementInTTLAttachGap_NeverLost(t *testing.T) {
	// A single sequential Allow call, not a burst spanning any real
	// duration, so a short window is all this test needs -- no risk of
	// crossing a window boundary mid-test the way a long-running concurrent
	// burst would carry, which is why other tests in this file use a much
	// longer Per.
	const per = time.Second
	limit := Limit{Rate: 1_000_000, Per: per}
	ctx := context.Background()
	key := "ttl-attach-gap"

	// Land solidly at the start of a real window so the window index computed
	// here for the storage key matches the one Allow computes for the same
	// hit an instant later (same pattern as TestAllow_WindowExpiry_OldWindowKeyExpires).
	windowStart := waitForFreshWindowStart(per)
	windowIndex := windowStart.UnixNano() / int64(per)
	storeKey := windowKey(key, windowIndex)

	real := pkgcore.NewMemoryKVStore()
	fake := &setInjectsConcurrentIncrementKVStore{KVStore: real, injectKey: storeKey}
	lim := New(fake)

	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	encoded, found, err := real.Get(ctx, storeKey)
	if err != nil || !found {
		t.Fatalf("Get(%q) after the hit: found=%t err=%v, want found=true", storeKey, found, err)
	}
	got, err := strconv.ParseFloat(string(encoded), floatEncodingBitSize)
	if err != nil {
		t.Fatalf("stored value %q does not parse as a float: %v", encoded, err)
	}

	// Exactly one real increment (Allow's own hit) always lands; a second
	// lands only if Allow's code path still reaches a caller-side Set call
	// at all after IncrByFloat.
	want := float64(1)
	if fake.injected {
		want = 2
	}
	if got != want {
		t.Fatalf("stored count = %v (the injected concurrent increment fired: %t), want %v -- "+
			"a real increment against %q was silently lost in the TTL-attachment gap, the exact "+
			"over-admit finding this test exists to catch", got, fake.injected, want, storeKey)
	}
}

// barrierAroundFirstHitKVStore wraps a real KVStore and forces the exact
// interleaving TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached
// needs to prove deterministically, rather than leaving it to
// goroutine-scheduling luck to decide whether the "other" concurrent
// callers' increments happen to land while the first hit on the fresh
// window key is still being recorded (see that test's own doc comment for
// why a plain, unsynchronized goroutine race cannot make this guarantee:
// on a fast machine the first-hit goroutine's increment can commit before
// any sibling goroutine has even reached its own Allow call, so the race
// a two-call TTL-attachment implementation was vulnerable to simply may
// not occur in a given run).
//
// Allow's write path is one atomic pkgcore.KVStore.IncrByFloatWithTTL
// call (see slidingWindowLimiter's "The TTL-attachment race": "increment"
// and "attach the expiry on creation" are collapsed into it, and Allow
// never reads the current window's key back with Get or calls Set at all
// -- readWindowCount's Get is the *previous* window's key, always distinct
// from watchKey) -- so there is only one call site left on Allow's write
// path to synchronize around, and the firstHit gate is the only
// synchronization this wrapper needs to carry.
//
// The gate works off that one call: firstHit closes the instant the very
// first increment against watchKey commits, observed from
// IncrByFloatWithTTL's own return of exactly 1, which only the call that
// creates the key can see. Every "other" concurrent caller blocks on
// firstHit before calling Allow at all, so none of them can race to create
// watchKey themselves -- exactly one goroutine (the test's own, calling
// Allow directly, never launched behind this gate) is guaranteed to be the
// one that creates it, its creating increment carrying the window's ttl
// with it in the same atomic step, while every sibling increment that
// follows races the others against the now-live key. Releasing the gate
// from inside the creating call itself -- rather than at some point
// between two separate calls -- is what makes the burst genuinely
// concurrent without ever recreating the vulnerable interleaving, which
// is what the test below proves.
type barrierAroundFirstHitKVStore struct {
	pkgcore.KVStore
	watchKey string

	firstHitOnce sync.Once
	firstHit     chan struct{}
}

func (s *barrierAroundFirstHitKVStore) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	result, err := s.KVStore.IncrByFloatWithTTL(ctx, key, delta, ttl)
	if key == s.watchKey && err == nil && result == 1 {
		// The creating increment has committed, with the ttl attached in
		// this same atomic call -- there is no gap left to protect, so
		// releasing immediately is exactly as safe as waiting.
		s.firstHitOnce.Do(func() { close(s.firstHit) })
	}
	return result, err
}

// TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached
// proves the first hit on a brand-new window key, recorded while a burst of
// concurrent callers is already lined up behind it, loses no increment and
// leaves the key with its ttl attached -- deliberately with NO warm-up
// call, unlike TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit above,
// which warms the key up first specifically so its own burst never has to
// create the key during the test (see that test's own doc comment). A
// two-call TTL attachment could not make this scenario fully race-free: a
// concurrent caller's increment landing between the first hit's read of
// its own just-created count and that count's caller-side Set write-back
// was silently overwritten (see slidingWindowLimiter's "The TTL-attachment
// race").
//
// barrierAroundFirstHitKVStore's firstHit gate makes the interleaving
// deterministic instead of scheduler-luck-dependent: the otherCallers
// concurrent Allow calls below cannot begin until the lone first-hit call's
// creating increment has committed, the gate releasing from inside
// IncrByFloatWithTTL's own return of 1, so exactly one goroutine -- the
// test's own, never launched behind the gate -- creates watchKey and every
// sibling increment lands against the live key in a genuine concurrent
// burst. If a gate ever released between two separate calls instead, the
// same setup would make the loss unconditional, not merely probable; the
// actual single-call path resolves the identical setup without any
// coordination at all beyond the atomic primitive itself.
//
// pkgcore.KVStore.IncrByFloatWithTTL's atomicity guarantees both halves of
// the contract survive the burst: the final stored count equals exactly
// the number of callers (no increment lost), and the key ends up with a
// ttl attached -- checked here the same way
// TestAllow_WindowExpiry_OldWindowKeyExpires checks it, by waiting past
// the attached ttl and confirming the key is then gone.
//
// Per is a full second, generous enough that every caller -- a trivial,
// in-memory, mutex-protected operation with no I/O -- reliably completes
// inside the one window this test needs, without the flakiness a much
// shorter Per would risk (a caller landing in the next window instead,
// which would undercount this test's own target key for a reason having
// nothing to do with the property under test).
func TestAllow_ConcurrentFirstHits_SameFreshKey_NoIncrementLostAndTTLAttached(t *testing.T) {
	const per = time.Second
	const otherCallers = 299
	const totalCallers = otherCallers + 1     // + the one first-hit call this test issues directly
	limit := Limit{Rate: 1_000_000, Per: per} // high rate: this test is about counting, not admission
	ctx := context.Background()
	key := "concurrent-fresh-key"

	// Land solidly at the start of a real window so the window index computed
	// here for the storage key matches the one Allow computes for the same
	// hit an instant later (same pattern as TestAllow_WindowExpiry_OldWindowKeyExpires).
	windowStart := waitForFreshWindowStart(per)
	windowIndex := windowStart.UnixNano() / int64(per)
	storeKey := windowKey(key, windowIndex)

	real := pkgcore.NewMemoryKVStore()
	fake := &barrierAroundFirstHitKVStore{
		KVStore:  real,
		watchKey: storeKey,
		firstHit: make(chan struct{}),
	}
	lim := New(fake)

	var wg sync.WaitGroup
	errs := make(chan error, otherCallers)
	wg.Add(otherCallers)
	for i := 0; i < otherCallers; i++ {
		go func() {
			defer wg.Done()
			<-fake.firstHit // never race to create the key ourselves
			if _, err := lim.Allow(ctx, key, limit); err != nil {
				errs <- err
			}
		}()
	}
	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("Allow (the first hit): %v", err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Allow (a concurrent caller): %v", err)
	}

	encoded, found, err := real.Get(ctx, storeKey)
	if err != nil || !found {
		t.Fatalf("Get(%q) after the burst: found=%t err=%v, want found=true", storeKey, found, err)
	}
	got, err := strconv.ParseFloat(string(encoded), floatEncodingBitSize)
	if err != nil {
		t.Fatalf("stored value %q does not parse as a float: %v", encoded, err)
	}
	if want := float64(totalCallers); got != want {
		t.Fatalf("stored count = %v, want %v: an increment was lost among %d Allow calls converging on the same fresh window key",
			got, want, totalCallers)
	}

	// windowTTLFactor * per is the ttl Allow passes to IncrByFloatWithTTL,
	// which stores a freshly-created key with that ttl already attached;
	// sleep comfortably past it so the key is guaranteed to have expired --
	// if, and only if, the creating hit actually attached one.
	time.Sleep(time.Duration(windowTTLFactor)*per + 100*time.Millisecond)
	if _, found, err := real.Get(ctx, storeKey); err != nil || found {
		t.Fatalf("Get(%q) once the ttl should have elapsed: found=%t err=%v, want found=false -- "+
			"the key never got a ttl attached when the burst's creating hit stored it", storeKey, found, err)
	}
}

// erroringKVStore wraps a real KVStore and forces a specific error from one
// named method, delegating everything else to the wrapped store. It exists
// to prove Allow returns a KVStore failure to the caller unmodified, rather
// than swallowing it or silently choosing fail-open or fail-closed on the
// caller's behalf -- that decision belongs to whoever consumes the Decision,
// not to this package.
type erroringKVStore struct {
	pkgcore.KVStore
	failMethod string
	err        error
}

func (s *erroringKVStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if s.failMethod == "Get" {
		return nil, false, s.err
	}
	return s.KVStore.Get(ctx, key)
}

func (s *erroringKVStore) IncrByFloatWithTTL(ctx context.Context, key string, delta float64, ttl time.Duration) (float64, error) {
	if s.failMethod == "IncrByFloatWithTTL" {
		return 0, s.err
	}
	return s.KVStore.IncrByFloatWithTTL(ctx, key, delta, ttl)
}

// TestAllow_KVStoreErrors_PropagatedToCaller covers every point in Allow
// that returns a KVStore-originated error, proving each one surfaces to the
// caller unmodified.
func TestAllow_KVStoreErrors_PropagatedToCaller(t *testing.T) {
	wantErr := errors.New("kvstore boom")
	limit := Limit{Rate: 5, Per: time.Minute}
	ctx := context.Background()

	t.Run("IncrByFloatWithTTL", func(t *testing.T) {
		// The one write call left on Allow's fixed path: every hit, first or
		// not, goes through it -- there is no longer a separate
		// TTL-attachment step to fail independently.
		fake := &erroringKVStore{KVStore: pkgcore.NewMemoryKVStore(), failMethod: "IncrByFloatWithTTL", err: wantErr}
		dec, err := New(fake).Allow(ctx, "k1", limit)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if dec != (Decision{}) {
			t.Fatalf("Decision = %+v, want the zero value on error", dec)
		}
	})

	t.Run("PreviousWindow_Get", func(t *testing.T) {
		// The only Get left on Allow's fixed path is readWindowCount's read
		// of the previous window, once a first hit has landed successfully.
		fake := &erroringKVStore{KVStore: pkgcore.NewMemoryKVStore()}
		lim := New(fake)
		if _, err := lim.Allow(ctx, "k4", limit); err != nil {
			t.Fatalf("warm-up Allow: %v", err)
		}
		fake.failMethod = "Get"
		fake.err = wantErr
		_, err := lim.Allow(ctx, "k4", limit)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})
}

// TestClampRemaining pins clampRemaining's contract directly, independent of
// any Limiter or KVStore: negative floors to zero, an ordinary value
// truncates as before, and -- the case this test exists to guard -- a value
// int cannot represent clamps to math.MaxInt instead of being converted
// directly. See clampRemaining's own doc comment for why that last case is a
// real, confirmed bug (an architecture-dependent conversion result) rather
// than defensive-programming caution.
//
// This table pins the fix deterministically on every architecture, unlike
// TestAllow_RateNearMaxInt_RemainingClampedConsistently below: it asserts
// clampRemaining's contract directly, rather than relying on a bare
// int(remaining) actually misbehaving on whatever machine happens to run
// `go test` -- which, per that other test's own doc comment, it does not
// do on every architecture.
func TestClampRemaining(t *testing.T) {
	tests := []struct {
		name      string
		remaining float64
		want      int
	}{
		{name: "negative floors to zero", remaining: -1, want: 0},
		{name: "zero stays zero", remaining: 0, want: 0},
		{name: "ordinary fractional value truncates toward zero", remaining: 4.9, want: 4},
		{name: "large but safely representable value converts directly", remaining: 1e15, want: 1_000_000_000_000_000},
		{name: "at the clamp threshold clamps to math.MaxInt", remaining: float64(math.MaxInt), want: math.MaxInt},
		{name: "far past int's range clamps to math.MaxInt", remaining: 1e300, want: math.MaxInt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampRemaining(tt.remaining); got != tt.want {
				t.Fatalf("clampRemaining(%v) = %d, want %d", tt.remaining, got, tt.want)
			}
		})
	}
}

// TestRetryAfterSeconds pins RetryAfterSeconds' boundary matrix across the
// inputs a caller can actually pass: sub-second remainders -- the ordinary
// tail of every exhausted window -- round up to 1, whole seconds pass
// through, and a remainder of zero or less floors at 0 (retry now is true
// once nothing remains; a negative count would be ungrammatical in the
// delay-seconds vocabulary the conversion feeds). The top clamp, reachable
// only through the float core retryAfterSeconds with a value no
// time.Duration can carry on a 64-bit int platform, is pinned separately
// below the table. See RetryAfterSeconds' own doc comment for the shape's
// rationale.
func TestRetryAfterSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		remaining time.Duration
		want      int
	}{
		{name: "sub-second window tail rounds up", remaining: 900 * time.Millisecond, want: 1},
		{name: "one millisecond rounds up", remaining: time.Millisecond, want: 1},
		{name: "exactly one second passes through", remaining: time.Second, want: 1},
		{name: "one second and a bit rounds up", remaining: 1100 * time.Millisecond, want: 2},
		{name: "window just reset floors at zero", remaining: 0, want: 0},
		{name: "elapsed remainder floors at zero", remaining: -3 * time.Second, want: 0},
		{name: "negative sub-second floors at zero", remaining: -500 * time.Millisecond, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RetryAfterSeconds(tt.remaining); got != tt.want {
				t.Fatalf("RetryAfterSeconds(%v) = %d, want %d", tt.remaining, got, tt.want)
			}
		})
	}

	// The float core's top clamp: a value int cannot represent answers
	// math.MaxInt, never an implementation-defined conversion. 2^63 -- the
	// nearest float64 to math.MaxInt -- is the smallest such value.
	if got := retryAfterSeconds(2 * float64(math.MaxInt)); got != math.MaxInt {
		t.Errorf("retryAfterSeconds(past int's range) = %d, want math.MaxInt", got)
	}
	if got := retryAfterSeconds(float64(math.MaxInt)); got != math.MaxInt {
		t.Errorf("retryAfterSeconds(at the clamp threshold) = %d, want math.MaxInt", got)
	}
	if got := retryAfterSeconds(1e15); got != 1_000_000_000_000_000 {
		t.Errorf("retryAfterSeconds(1e15) = %d, want 1e15 -- a large representable value converts directly", got)
	}
}

// TestAllow_RateNearMaxInt_RemainingClampedConsistently exercises the fix
// through the public Allow API for the scenario the bug report was about: a
// caller using a very large Limit.Rate (math.MaxInt is a real convention for
// "effectively unlimited", used instead of skipping the Allow call
// altogether) must never see an internally inconsistent Decision -- Allowed
// true alongside a deeply negative Remaining.
//
// Before the fix, this exact scenario -- Rate: math.MaxInt, one hit -- could
// only be observed to fail on this package's own darwin/arm64 development
// host by cross-compiling to amd64 and running the binary under Rosetta:
// arm64's own float64->int conversion happens to saturate correctly for
// this input (see clampRemaining's doc comment), so a bare int(remaining)
// conversion passes this assertion anyway on an arm64 machine, and only an
// amd64 run (the architecture most production and CI environments run on)
// demonstrates the failure. TestClampRemaining above pins clampRemaining's
// own contract independent of which architecture `go test` happens to run
// on; this test additionally proves the clamping is actually wired into
// Allow's own return path, not just implemented and unused.
func TestAllow_RateNearMaxInt_RemainingClampedConsistently(t *testing.T) {
	limit := Limit{Rate: math.MaxInt, Per: time.Minute}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()

	dec, err := lim.Allow(ctx, "unlimited-sentinel", limit)
	if err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("Allowed = false, want true (well within a math.MaxInt rate)")
	}
	if dec.Remaining < 0 {
		t.Fatalf("Remaining = %d, want >= 0 (Allowed=true must never pair with a negative Remaining)", dec.Remaining)
	}
	if dec.Remaining != math.MaxInt {
		t.Fatalf("Remaining = %d, want %d (math.MaxInt: float64(limit.Rate) has already rounded up past it, "+
			"so this call's tiny increment is absorbed by floating-point rounding and clampRemaining's top clamp applies)",
			dec.Remaining, math.MaxInt)
	}
}

// TestWindowKey_DistinctPairsNeverCollide locks in the collision-freedom
// windowKey's own doc comment proves: distinct (key, windowIndex) pairs must
// never produce the same storage-key string, no matter what key contains.
// This is not a spot check -- it exercises exactly the key shapes a
// delimiter-escaping bug would need to trip on (keys with embedded colons,
// IPv6-address-shaped keys, and keys already shaped like "something:123")
// crossed with a range of windowIndex values that includes both int64
// extremes, and fails the instant any two distinct pairs land on the same
// string.
func TestWindowKey_DistinctPairsNeverCollide(t *testing.T) {
	keys := []string{
		"",
		"a",
		"plain-key",
		"a:1",
		"a:1:2",
		"a:1:23",
		"a:-5",
		"a:-5:3",
		"user:employee",
		"user:employee:1",
		"already:key:456",
		"tenant:user:with:many:colons:here",
		"2001:db8::1",      // IPv6-address-shaped
		"::1",              // IPv6-address-shaped
		"fe80::1%eth0",     // IPv6-address-shaped, with a zone id
		"::ffff:192.0.2.1", // IPv6-mapped-IPv4-shaped
		"foo:123",          // already "key:number"-shaped
		"foo:-123",         // already "key:-number"-shaped
		"foo:0",
		"x:9223372036854775807", // key itself ends in an int64-extreme-looking suffix
		"x:-9223372036854775808",
		"-",
		"-1",
		"0",
		"colon:",
		":colon",
	}
	indices := []int64{
		0, 1, -1, 2, -2, 3, 23, -23, 123, -123, 456, 789,
		math.MaxInt64, math.MinInt64, math.MaxInt64 - 1, math.MinInt64 + 1,
	}

	type pair struct {
		key   string
		index int64
	}
	seen := make(map[string]pair, len(keys)*len(indices))
	for _, k := range keys {
		for _, idx := range indices {
			got := windowKey(k, idx)
			this := pair{k, idx}
			if prev, ok := seen[got]; ok && prev != this {
				t.Fatalf("windowKey(%q, %d) = %q, but windowKey(%q, %d) already produced the same string -- "+
					"distinct (key, windowIndex) pairs must never collide", k, idx, got, prev.key, prev.index)
			}
			seen[got] = this
		}
	}
}

// TestAllow_EveryWindowRollover_MintsFreshKey pins down, deterministically
// and with no concurrency involved, a structural fact the former
// "TTL-attachment race" doc comment relied on to explain why that race
// recurred rather than being a one-time event: windowKey mints a
// brand-new, never-before-used storage key every single Per interval, for
// as long as a caller's key keeps being hit. That recurrence is what used
// to matter for the race (now closed -- see slidingWindowLimiter's doc
// comment, "The TTL-attachment race, closed") and still matters on its own
// structural merits: "the first hit in this window" is not a rare,
// once-per-caller-key event, it recurs on every window boundary, forever,
// for any continuously-hit key, which is worth pinning down independent of
// any race it once exposed.
//
// This test proves exactly that against the store: across several
// consecutive real windows, the storage key Allow is about to use has never
// existed before its own hit lands -- every single time, not just on the
// very first window ever seen -- which fails outright if any of these
// per-window keys turns out to already exist (meaning windowKey were
// reusing keys across windows instead of minting a new one each time).
func TestAllow_EveryWindowRollover_MintsFreshKey(t *testing.T) {
	const per = 80 * time.Millisecond
	const windows = 4
	// Rate is high enough that Allowed/Remaining are never in question here
	// -- this test is only about which storage key gets touched, not about
	// limit decisions.
	limit := Limit{Rate: 1_000_000, Per: per}
	store := pkgcore.NewMemoryKVStore()
	lim := New(store)
	ctx := context.Background()
	key := "rollover-fresh-key"

	seenWindowIndices := make(map[int64]bool, windows)
	for i := 0; i < windows; i++ {
		// Land solidly at the start of a real window so the window index
		// computed here for the storage key matches the one Allow computes
		// for the same hit an instant later (same pattern as
		// TestAllow_WindowExpiry_OldWindowKeyExpires above).
		windowStart := waitForFreshWindowStart(per)
		windowIndex := windowStart.UnixNano() / int64(per)
		if seenWindowIndices[windowIndex] {
			t.Fatalf("iteration %d: windowIndex %d repeated -- waitForFreshWindowStart should always land in a new window", i, windowIndex)
		}
		seenWindowIndices[windowIndex] = true

		storeKey := windowKey(key, windowIndex)
		if _, found, err := store.Get(ctx, storeKey); err != nil {
			t.Fatalf("iteration %d: Get(%q) before this window's first hit: %v", i, storeKey, err)
		} else if found {
			t.Fatalf("iteration %d: storeKey %q already exists before this window's first hit -- "+
				"windowKey did not mint a fresh key for this window, which would mean the TTL-attachment "+
				"race is confined to a true once-per-caller-key event after all (it is not: see this "+
				"test's own doc comment)", i, storeKey)
		}

		if _, err := lim.Allow(ctx, key, limit); err != nil {
			t.Fatalf("iteration %d: Allow: %v", i, err)
		}

		value, found, err := store.Get(ctx, storeKey)
		if err != nil || !found {
			t.Fatalf("iteration %d: Get(%q) right after the hit: found=%t err=%v, want found=true", i, storeKey, found, err)
		}
		got, err := strconv.ParseFloat(string(value), floatEncodingBitSize)
		if err != nil {
			t.Fatalf("iteration %d: stored value %q does not parse as a float: %v", i, value, err)
		}
		if got != 1 {
			t.Fatalf("iteration %d: stored value = %v, want 1 -- "+
				"this hit should be the first ever recorded against this window's brand-new key", i, got)
		}

		// Sleep past this window's end so the next iteration lands in a
		// genuinely new window rather than hitting the same key again.
		if d := time.Until(windowStart.Add(per).Add(per / 10)); d > 0 {
			time.Sleep(d)
		}
	}
}
