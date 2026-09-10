package config

import (
	"context"
	"errors"
	"time"

	"github.com/vislake/speed/go/ratelimit"
)

// This file applies go/ratelimit to this module's entire abuse surface: the
// two pre-auth display endpoints. They are the only routes the module
// serves, they are unauthenticated by design -- a login page's brand and
// flag query must answer before any sign-in exists -- and they carry no
// secret to guess (no token, no password), so the caller's own network
// address is both the only identity either endpoint can key a budget on and
// the only dimension the budget needs: one address's request volume. That
// makes the budget simpler than go/sharing's access route, whose second
// dimension exists because a token and a password can be guessed; here
// there is nothing to guess, only volume to bound.
//
// Both endpoints consume the same key. They serve one page-load worth of
// the same display answer, so a client that fetches both must not get to
// split its budget across two paths; the check itself is one
// ratelimit.Limiter.Allow call on that key, built on the shared
// allowRateLimit helper, with the Limiter built per call over the KVStore
// the Service captured at Attach (rateLimiter, below) -- so every check
// reads whichever implementation the running deployment mode actually
// resolved, and a Service that captured none fails the check rather than
// serving unthrottled.
//
// Deliberately NOT a Module Option or a configuration item: there is no
// meaningful "off" a host would ever deliberately choose for a genuinely
// unauthenticated endpoint, and a live config value cannot be read here
// anyway -- the pre-auth path runs before any tenant is known, and the very
// request this check guards would be the one carrying the value. Package
// constants, the same reasoning go/sharing and go/org record; a declared
// schema this module would then ignore would be a lying schema, worse than
// a constant.

// The rate-limit budget this module applies.
const (
	// preAuthPerIPRate bounds how many requests one address may make per
	// preAuthPerIPWindow, across both endpoints together. The number is
	// shaped by what legitimate anonymous traffic looks like rather than by
	// what abuse looks like: one page load costs one or two of these
	// requests, so a NAT'd office of ordinary visitors stays well inside
	// the budget, while a single address still runs out after two requests
	// per second sustained. An address reaching the budget is refused with
	// the module's own rate-limited error until its window slides on.
	preAuthPerIPRate   = 120
	preAuthPerIPWindow = time.Minute
)

// errNoKVStore reports a Service that captured no KVStore at Attach: only a
// hand-built, zero-value *pkgcore.Registry carries none (pkgcore.NewRegistry
// panics on a nil store), and the rate-limit check treats it as a failure --
// fail closed -- rather than skipping the check, because a check that
// silently passed would leave exactly the volume abuse it bounds unguarded.
var errNoKVStore = errors.New("config: the registry carries no KVStore, so the pre-auth rate-limit check cannot engage")

// rateLimiter returns the Limiter the per-address check runs on, built over
// the KVStore the Service captured at Attach. The Limiter is constructed
// here rather than captured at Attach so the check always reads the seam at
// call time -- the same rule events.go's publish follows for the bus -- and
// so a missing store surfaces as this error instead of a nil dereference.
func (s *Service) rateLimiter() (ratelimit.Limiter, error) {
	if s.kv == nil {
		return nil, errNoKVStore
	}
	return ratelimit.New(s.kv), nil
}

// checkPreAuthIPLimit refuses a request from ip when that address's own
// budget is spent. This dimension is checked UNCONDITIONALLY, in the
// handler before any resolution or store work, and that placement is
// deliberate for the reason go/sharing's per-IP check records: the party an
// address budget protects (the platform, against one address's request
// volume) and the party that consumes it (the address itself) are the same
// caller, so an address that exhausts its own budget refuses only itself --
// no legitimate request of a different caller can ever be held hostage by
// it the way a shared per-target budget can.
//
// The key is the address exactly as clientIP derived it (http.go): the
// module neither trusts nor parses a forwarded-header value, and an empty
// address shares one counter with every other empty-address caller, a
// caller-visible consequence of supplying no better identifier rather than
// a special case.
func (s *Service) checkPreAuthIPLimit(ctx context.Context, ip string) error {
	return s.allowRateLimit(ctx, "config:preauth:ip:"+ip, ratelimit.Limit{
		Rate: preAuthPerIPRate, Per: preAuthPerIPWindow,
	}, "ip")
}

// allowRateLimit is the shared check the dimension above is built from: one
// ratelimit.Limiter.Allow call on key under limit, decorating a denial as
// ErrRateLimited with the dimension named (never the key itself -- a
// caller-visible dimension name is safe, a caller-visible key, which embeds
// an address, is not) and the window's recovery time recorded, and an
// unavailable limiter or store failure as ErrStorage: fail closed, never
// "allow". That is this module's own choice, on the same reasoning as every
// other fail-closed path here: the check is the only throttle these
// endpoints have, so a store outage that read as "allow" would leave the
// endpoint's volume unthrottled for the duration of the outage.
func (s *Service) allowRateLimit(ctx context.Context, key string, limit ratelimit.Limit, dimension string) error {
	limiter, err := s.rateLimiter()
	if err != nil {
		return ErrStorage.WithCause(err)
	}
	decision, err := limiter.Allow(ctx, key, limit)
	if err != nil {
		return ErrStorage.WithCause(err)
	}
	if !decision.Allowed {
		return ErrRateLimited.
			WithParam("dimension", dimension).
			WithParam("retry_after_seconds", ratelimit.RetryAfterSeconds(decision.ResetAfter))
	}
	return nil
}
