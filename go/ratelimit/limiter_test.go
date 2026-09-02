package ratelimit

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
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
		{name: "zero per", limit: Limit{Rate: 5, Per: 0}},
		{name: "negative per", limit: Limit{Rate: 5, Per: -time.Second}},
		{name: "both zero", limit: Limit{Rate: 0, Per: 0}},
		// Per is positive and well within time.Duration's own ~292-year
		// range, but windowTTLFactor*Per (see attachWindowTTL's call site in
		// Allow) is computed as a time.Duration, so doubling a Per this large
		// overflows int64 nanoseconds and wraps to a negative duration. A
		// negative ttl reaching KVStore.Set would store the window's counter
		// key with no expiry at all (Set's own documented contract: "a ttl of
		// zero or less stores the key without an expiry"), pinning it in the
		// store forever instead of letting it age out with its window.
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
	limit := Limit{Rate: 1, Per: maxPer}
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
// itself (see AGENTS.md's Known limitations, and slidingWindowLimiter's own
// doc comment, "Saturating clients permanently lose one slot per window"):
// once a window's recorded count has reached limit.Rate, the Rate-th
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

// TestAllow_SaturatingClient_Rate1_LocksOutPermanently is the sharpest
// instance of TestAllow_SaturatingClient_ConvergesToRateMinusOnePerWindow's
// property: for Rate == 1, "rate-1 admitted per window" is zero. A key hit
// once every window is allowed exactly once, ever, then denied every
// window after that, indefinitely, for as long as it keeps being hit at
// least once per window. See AGENTS.md's Known limitations.
func TestAllow_SaturatingClient_Rate1_LocksOutPermanently(t *testing.T) {
	const per = 200 * time.Millisecond
	const windows = 4
	limit := Limit{Rate: 1, Per: per}
	lim := New(pkgcore.NewMemoryKVStore())
	ctx := context.Background()
	key := "rate-one-client"

	windowStart := waitForFreshWindowStart(per)

	for w := 0; w < windows; w++ {
		target := windowStart.Add(time.Duration(w) * per).Add(per / 10)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}

		dec, err := lim.Allow(ctx, key, limit)
		if err != nil {
			t.Fatalf("window %d: Allow: %v", w, err)
		}

		if want := w == 0; dec.Allowed != want {
			t.Fatalf("window %d: Allowed = %t, want %t (Rate=1 is admitted only in the very "+
				"first window a key is ever used in, then denied permanently -- see AGENTS.md's "+
				"Known limitations)", w, dec.Allowed, want)
		}
	}
}

// TestAllow_WindowExpiry_OldWindowKeyExpires proves attachWindowTTL actually
// attaches an expiry: the storage key backing a window must eventually
// become unreadable in the underlying KVStore, not accumulate forever.
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

	// windowTTLFactor * per is the TTL attachWindowTTL attaches; sleep
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
// burst starts. That is deliberate, not incidental: warming up first
// establishes the window's counter key and attaches its TTL
// (attachWindowTTL) with no concurrent contention, so every hit in the
// burst that follows is a pure KVStore.IncrByFloat call against a key that
// already exists and already has its TTL attached -- exactly the case
// KVStore's own contract guarantees is atomic and lossless under
// concurrency, which is the property this test exists to prove. See
// slidingWindowLimiter's doc comment ("The TTL-attachment race") for why
// concurrently *creating* the same brand-new key is the one scenario this
// package's algorithm cannot make equally race-free with KVStore's given
// primitives -- which is exactly why this test warms the key up first
// rather than starting the burst against a fresh one.
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

func (s *erroringKVStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if s.failMethod == "Set" {
		return s.err
	}
	return s.KVStore.Set(ctx, key, value, ttl)
}

func (s *erroringKVStore) IncrByFloat(ctx context.Context, key string, delta float64) (float64, error) {
	if s.failMethod == "IncrByFloat" {
		return 0, s.err
	}
	return s.KVStore.IncrByFloat(ctx, key, delta)
}

// TestAllow_KVStoreErrors_PropagatedToCaller covers every point in Allow
// that returns a KVStore-originated error, proving each one surfaces to the
// caller unmodified.
func TestAllow_KVStoreErrors_PropagatedToCaller(t *testing.T) {
	wantErr := errors.New("kvstore boom")
	limit := Limit{Rate: 5, Per: time.Minute}
	ctx := context.Background()

	t.Run("IncrByFloat", func(t *testing.T) {
		fake := &erroringKVStore{KVStore: pkgcore.NewMemoryKVStore(), failMethod: "IncrByFloat", err: wantErr}
		dec, err := New(fake).Allow(ctx, "k1", limit)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if dec != (Decision{}) {
			t.Fatalf("Decision = %+v, want the zero value on error", dec)
		}
	})

	t.Run("AttachTTL_Get", func(t *testing.T) {
		// The first hit against a fresh key triggers attachWindowTTL, whose
		// own first step is a Get.
		fake := &erroringKVStore{KVStore: pkgcore.NewMemoryKVStore(), failMethod: "Get", err: wantErr}
		_, err := New(fake).Allow(ctx, "k2", limit)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})

	t.Run("AttachTTL_Set", func(t *testing.T) {
		fake := &erroringKVStore{KVStore: pkgcore.NewMemoryKVStore(), failMethod: "Set", err: wantErr}
		_, err := New(fake).Allow(ctx, "k3", limit)
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})

	t.Run("PreviousWindow_Get", func(t *testing.T) {
		// A second hit in the same window skips attachWindowTTL entirely --
		// only the call whose IncrByFloat creates the key attaches a TTL --
		// so once warmed up with one successful call, the only Get left on
		// the path is readWindowCount's read of the previous window.
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

// setCountingKVStore wraps a real KVStore and counts calls to Set. Set is
// the one store method attachWindowTTL ever calls (see limiter.go's Allow
// and attachWindowTTL) -- nothing else on Allow's path touches it, since
// readWindowCount only ever calls Get -- so counting it in isolation is a
// precise, unambiguous signal for "did attachWindowTTL run on this call",
// independent of the exact-count reasoning
// TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit relies on below.
type setCountingKVStore struct {
	pkgcore.KVStore
	setCalls int
}

func (s *setCountingKVStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.setCalls++
	return s.KVStore.Set(ctx, key, value, ttl)
}

// TestAllow_AttachWindowTTL_SkippedAfterFirstHitInWindow directly and
// specifically isolates the property slidingWindowLimiter's own doc comment
// calls out as deliberate ("The TTL-attachment race") and that Allow's call
// site gates on (currentCount == firstHitInWindow): attachWindowTTL runs
// only on the one call whose IncrByFloat is the first hit in a window, and
// is skipped on every later hit in that same window. Calling it
// unconditionally on every hit would reintroduce the Get-then-Set
// lost-update race described there on every single hit rather than just the
// first -- see the doc comment just above slidingWindowLimiter's own
// declaration: "Do not fix this by reading the key before calling
// IncrByFloat ... a Get-then-branch ahead of the atomic increment
// reintroduces a real lost-update race on every hit, not just the first one
// in a window, which is strictly worse."
//
// Set is called nowhere else on Allow's path, so counting Set calls with
// setCountingKVStore pins the gate down directly and unambiguously: two
// sequential hits against the same key inside one window must produce
// exactly one Set, not two. Before this test, the only thing that happened
// to notice a regression in this exact gate was
// TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit's exact-count
// assertion -- and only incidentally, as a side effect of the lost-update
// race the extra Set calls reintroduce under concurrency, not because that
// test names or documents this gate as a property of its own (confirmed by
// mutation: removing the gate so attachWindowTTL runs unconditionally is
// caught by that concurrency test's exact-count assertion, but leaves
// TestAllow_KVStoreErrors_PropagatedToCaller passing unchanged, since both
// of its Get failure points return the same error either way). This test
// isolates the gate with no concurrency involved at all.
func TestAllow_AttachWindowTTL_SkippedAfterFirstHitInWindow(t *testing.T) {
	fake := &setCountingKVStore{KVStore: pkgcore.NewMemoryKVStore()}
	lim := New(fake)
	ctx := context.Background()
	limit := Limit{Rate: 5, Per: time.Minute} // long window: both hits land in the same one
	key := "ttl-gate-key"

	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("first hit in the window: Allow: %v", err)
	}
	if got := fake.setCalls; got != 1 {
		t.Fatalf("Set calls after the first hit in a fresh window = %d, want exactly 1 "+
			"(attachWindowTTL must run once, on the hit that creates the window's key)", got)
	}

	if _, err := lim.Allow(ctx, key, limit); err != nil {
		t.Fatalf("second hit in the same window: Allow: %v", err)
	}
	if got := fake.setCalls; got != 1 {
		t.Fatalf("Set calls after a second hit in the same window = %d, want still exactly 1 "+
			"(attachWindowTTL must be skipped on every hit after the first, or the Get-then-Set "+
			"race described in slidingWindowLimiter's doc comment reopens on every hit, not just "+
			"the first)", got)
	}
}

// flakyKVStore wraps a real KVStore and makes one named method fail a fixed
// number of times before delegating to the wrapped store as normal. It
// models a *transient* KVStore fault -- one that clears after a bounded
// number of attempts -- as opposed to erroringKVStore above, which fails
// permanently and exists to prove Allow's error-passthrough contract.
// attempts counts every call made to failMethod, including ones after
// failsRemaining has reached zero, so a test can pin down exactly how many
// attempts a retry loop actually made.
type flakyKVStore struct {
	pkgcore.KVStore
	failMethod     string
	err            error
	failsRemaining int
	attempts       int
}

func (s *flakyKVStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if s.failMethod == "Get" {
		s.attempts++
		if s.failsRemaining > 0 {
			s.failsRemaining--
			return nil, false, s.err
		}
	}
	return s.KVStore.Get(ctx, key)
}

func (s *flakyKVStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if s.failMethod == "Set" {
		s.attempts++
		if s.failsRemaining > 0 {
			s.failsRemaining--
			return s.err
		}
	}
	return s.KVStore.Set(ctx, key, value, ttl)
}

// TestAllow_AttachWindowTTL_TransientError_RetriedUntilKeyGetsExpiry
// reproduces the bug attachWindowTTLMaxAttempts fixes. Before that retry
// budget existed, currentCount == firstHitInWindow gated the *only* call to
// attachWindowTTL for a given window (see Allow's call site and
// attachWindowTTLMaxAttempts's own doc comment), so any error there -- even
// one from an ordinary transient fault, not just a process crash -- made
// Allow return the error to the caller with no retry at all. A caller that
// then retried the logical request got back an IncrByFloat count > 1 on the
// identical key (it already existed), which skips attachWindowTTL for the
// rest of that window's life -- stranding the key without a TTL forever,
// not just until the next hit.
//
// This test proves a fault that clears within the retry budget no longer
// has that effect: Allow must succeed with no error at all (the underlying
// fault clears before the budget is exhausted), and the window's key must
// still end up with a real, working TTL -- checked the same way
// TestAllow_WindowExpiry_OldWindowKeyExpires checks it, directly against the
// store, after waiting past the expiry. Before the fix, this test fails at
// the very first assertion: a single Get or Set failure surfaced as an
// error from Allow, with no retry to recover from it.
func TestAllow_AttachWindowTTL_TransientError_RetriedUntilKeyGetsExpiry(t *testing.T) {
	const per = 80 * time.Millisecond
	limit := Limit{Rate: 5, Per: per}
	ctx := context.Background()

	for _, failMethod := range []string{"Get", "Set"} {
		t.Run(failMethod, func(t *testing.T) {
			real := pkgcore.NewMemoryKVStore()
			// Fails on every attempt except the very last one the retry
			// budget allows -- the tightest in-budget recovery there is.
			fake := &flakyKVStore{
				KVStore:        real,
				failMethod:     failMethod,
				err:            errors.New("transient kvstore hiccup"),
				failsRemaining: attachWindowTTLMaxAttempts - 1,
			}
			key := "flaky-" + failMethod

			// Land solidly at the start of a real window so the window
			// index computed here for the storage key matches the one
			// Allow computes for the same hit an instant later (same
			// pattern as TestAllow_WindowExpiry_OldWindowKeyExpires).
			windowStart := waitForFreshWindowStart(per)
			windowIndex := windowStart.UnixNano() / int64(per)
			storeKey := windowKey(key, windowIndex)

			dec, err := New(fake).Allow(ctx, key, limit)
			if err != nil {
				t.Fatalf("Allow: %v, want no error -- a transient failure that clears within "+
					"attachWindowTTLMaxAttempts attempts must not surface to the caller", err)
			}
			if !dec.Allowed {
				t.Fatalf("Decision.Allowed = false, want true for a first hit within limit")
			}
			if fake.failsRemaining != 0 {
				t.Fatalf("failsRemaining = %d, want 0 -- the retry loop should have exhausted "+
					"every injected failure before Allow returned successfully", fake.failsRemaining)
			}

			if _, found, getErr := real.Get(ctx, storeKey); getErr != nil || !found {
				t.Fatalf("Get(%q) right after the hit: found=%t err=%v, want found=true", storeKey, found, getErr)
			}

			// windowTTLFactor * per is the TTL attachWindowTTL attaches;
			// sleep comfortably past it so the key is guaranteed to have
			// expired -- if, and only if, a TTL was actually attached.
			time.Sleep(time.Duration(windowTTLFactor)*per + 100*time.Millisecond)

			if _, found, getErr := real.Get(ctx, storeKey); getErr != nil || found {
				t.Fatalf("Get(%q) once the TTL should have elapsed: found=%t err=%v, want found=false -- "+
					"the key never expired, meaning the retry did not actually attach a TTL", storeKey, found, getErr)
			}
		})
	}
}

// TestAllow_AttachWindowTTL_ExhaustsRetryBudget_ThenPropagatesError proves
// the retry attachWindowTTLMaxAttempts adds is bounded, not unconditional
// resilience: a fault that persists for the whole retry budget still
// surfaces to Allow's caller exactly as before -- fail-closed, per this
// package's documented error-passthrough contract -- after making exactly
// attachWindowTTLMaxAttempts attempts: neither giving up after only one
// (which was the pre-fix behavior this whole fix changes) nor retrying
// forever (which would turn a sustained outage into an Allow call that never
// returns).
func TestAllow_AttachWindowTTL_ExhaustsRetryBudget_ThenPropagatesError(t *testing.T) {
	wantErr := errors.New("sustained kvstore outage")
	limit := Limit{Rate: 5, Per: time.Minute}
	ctx := context.Background()

	fake := &flakyKVStore{
		KVStore:        pkgcore.NewMemoryKVStore(),
		failMethod:     "Get",
		err:            wantErr,
		failsRemaining: attachWindowTTLMaxAttempts + 5, // outlasts the whole retry budget
	}

	dec, err := New(fake).Allow(ctx, "exhausted-budget", limit)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if dec != (Decision{}) {
		t.Fatalf("Decision = %+v, want the zero value on error", dec)
	}
	if fake.attempts != attachWindowTTLMaxAttempts {
		t.Fatalf("attempts = %d, want exactly attachWindowTTLMaxAttempts (%d) -- "+
			"the retry loop must neither give up early nor retry unboundedly", fake.attempts, attachWindowTTLMaxAttempts)
	}
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
// `go test` -- which, per that other test's own doc comment, an unfixed
// build would not do on every architecture.
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
// this input (see clampRemaining's doc comment), so an unfixed build run
// natively on an arm64 machine passes this assertion anyway, and only an
// amd64 run (the architecture the original bug report -- and most
// production and CI environments -- actually runs on) demonstrates the
// failure. TestClampRemaining above is what pins the fix independent of
// which architecture `go test` happens to run on; this test additionally
// proves the fix is actually wired into Allow's own return path, not just
// implemented and unused.
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
// and with no concurrency involved, what the "TTL-attachment race" doc
// comment above (and AGENTS.md's "Known limitations") spell out explicitly:
// windowKey mints a brand-new, never-before-used storage key every single
// Per interval, for as long as a caller's key keeps being hit. "The first
// hit in this window" -- the one condition that triggers attachWindowTTL,
// and therefore the one call per window exposed to the TTL-attachment race
// -- is not a rare, once-per-caller-key event: it recurs on every window
// boundary, forever, for any continuously-hit key.
//
// This test proves the structural half of that claim directly against the
// store: across several consecutive real windows, the storage key Allow is
// about to use has never existed before its own hit lands -- every single
// time, not just on the very first window ever seen. Before the doc fix
// this test guards, "a key's first-ever hit" and "removes the gap for that
// key entirely" (of the pre-warming mitigation) were easy to misread as
// describing a one-time event in a caller key's whole lifetime; this test
// would have flagged that misreading immediately, since it fails outright
// if any of these per-window keys turns out to already exist (which would
// mean windowKey were reusing keys across windows instead of minting a new
// one each time).
//
// The companion, probabilistic half of the finding -- that a concurrent
// burst landing on one of these freshly-minted keys can actually exceed the
// configured rate, and that pre-warming one window does not protect the
// next -- is exactly the TTL-attachment race already covered by this
// file's other tests and the package doc comment. It is deliberately not
// repeated here as a hard pass/fail assertion: like
// TestAllow_ConcurrentCallers_SameKey_NeverExceedsLimit's own doc comment
// explains, how much a race like this bites on any given run depends on
// scheduler luck, not just on whether the code is correct, so asserting an
// exact overshoot count would make this suite flaky rather than more
// trustworthy. The structural fact this test does pin down is what makes
// that race recurring rather than a one-off, which is the part the
// documentation previously understated.
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
		if want := encodeCount(firstHitInWindow); string(value) != string(want) {
			t.Fatalf("iteration %d: stored value = %q, want %q (firstHitInWindow) -- "+
				"this hit should be the first ever recorded against this window's brand-new key", i, value, want)
		}

		// Sleep past this window's end so the next iteration lands in a
		// genuinely new window rather than hitting the same key again.
		if d := time.Until(windowStart.Add(per).Add(per / 10)); d > 0 {
			time.Sleep(d)
		}
	}
}
