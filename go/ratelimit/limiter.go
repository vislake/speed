package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

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
	// Remaining recovers as the window slides forward. Callers that report
	// the wait as a whole-second count convert it with RetryAfterSeconds.
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

// RetryAfterSeconds converts a remaining wait -- typically a denied
// Decision's ResetAfter -- to the whole number of seconds a caller reports
// as the retry wait, the vocabulary RFC 9110 (10.2.3) defines for the
// Retry-After header and this repository's structured-error envelopes use
// for their "retry_after_seconds" parameter. The package's Decision stays
// plain data with no protocol awareness (see its own doc comment); this
// function is the one vocabulary conversion every consumer of a denied
// Decision performs, offered here because a conversion each consumer would
// otherwise copy is exactly the boundary that drifts apart in rounding
// direction and extreme-value handling. One function is the family's
// single written shape.
//
// The conversion rounds UP. A sub-second remainder is the ordinary tail of
// every exhausted window -- a window ends at an arbitrary phase, so the
// instant a denial lands is uniform within the window's last second -- and
// truncating it to 0 would report "retry immediately" (the meaning RFC 9110
// gives Retry-After: 0) up to a second before the window has actually
// reset, inviting an immediate retry that is still refused. Rounding up
// errs the other way: the hint says at most one second more than the window
// needs.
//
// The extremes have a stated disposition. A negative remaining duration --
// the reset instant has already passed -- converts to 0: retry now is the
// true answer, and a negative count would not merely mislead but be
// ungrammatical in the delay-seconds vocabulary (1*DIGIT) this conversion
// feeds. A remaining duration whose whole-second count int cannot represent
// converts to math.MaxInt instead of being converted directly, the same
// rule clampRemaining applies to Decision.Remaining: the Go spec
// (Conversions) leaves the float-to-int conversion of an out-of-range value
// implementation-defined. No time.Duration input reaches that bound where
// int is 64 bits -- a duration spans at most about 292 years, roughly 9.2e9
// seconds -- but a 32-bit int's far smaller max is reachable with a Per
// near the package's own maxPer ceiling, so the cap is a real guard, not
// decoration.
func RetryAfterSeconds(remaining time.Duration) int {
	return retryAfterSeconds(remaining.Seconds())
}

// retryAfterSeconds converts a remaining wait already expressed in seconds
// (RetryAfterSeconds, which this backs, splits the duration) to whole
// seconds, rounding up and clamping both extremes before the int
// conversion: below zero to 0, at or beyond int's own ceiling to
// math.MaxInt. The float form exists so the top clamp is testable with
// values no time.Duration can carry; the shape it implements is
// RetryAfterSeconds' shape.
func retryAfterSeconds(seconds float64) int {
	seconds = math.Ceil(seconds)
	if seconds < 0 {
		return 0
	}
	if seconds >= float64(math.MaxInt) {
		return math.MaxInt
	}
	return int(seconds)
}
