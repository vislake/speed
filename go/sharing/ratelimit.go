package sharing

import (
	"context"
	"net/http"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// This file closes the round-1 known limitation AGENTS.md recorded --
// "Create or Access has no rate limiting" -- by applying go/ratelimit to
// both call sites this module's abuse surface
// actually has -- Service.Create (share-creation abuse, one dimension) and
// Service.AccessPublic (token-guessing and password-guessing abuse, two
// dimensions) -- composed the same way go/org's InviteService.checkRateLimits
// composes its own two dimensions and the same way go/integration's
// LayeredLimiter composes three: one ratelimit.Limiter.Allow call per
// dimension, any single denial denying the whole request, with the
// underlying Limiter built lazily over the host's KVStore (rateLimiter,
// below) so it always reads whichever implementation the running deployment
// mode actually resolved, never one captured before Bootstrap ran.
//
// Deliberately NOT a Module Option: unlike TenantConfigReader (an
// optional-by-design seam with a documented, correct default) or WithQueue
// (optional row hygiene), rate limiting here has no meaningful "off"
// default a host would ever deliberately choose for a genuinely
// unauthenticated public endpoint -- so rather than adding a WithRateLimiter
// wiring point a host could forget, this module reads the registry's own
// KVStore, which every Bootstrap already resolves for every deployment
// mode, through the identical hostSeams.KVStore() seam
// org.InviteService.rateLimiter reads. A host cannot boot this module at
// all without a KVStore somewhere in its composition (pkgcore.Kernel
// resolves one unconditionally), so rate limiting is real the moment
// Module.Register attaches the registry, with no separate opt-in to forget.

// The rate-limit budget this module applies. Package constants, not dynamic
// configuration: sharing cannot read a live go/config value without adding
// that dependency (the same reasoning go/org/AGENTS.md records for its own
// invite rate limits), and a declared config schema this module would then
// ignore would be a lying schema, worse than a constant.
const (
	// createPerTenantRate bounds how many shares one tenant may create per
	// createPerTenantWindow -- the blast radius of a compromised or
	// careless account minting share links faster than any legitimate use
	// of this feature would.
	createPerTenantRate   = 100
	createPerTenantWindow = time.Hour

	// accessPerIPRate bounds how many access attempts one IP address may
	// make per accessPerIPWindow, across every token it tries -- the
	// dimension that catches broad token-guessing: an attacker scanning
	// many possible tokens from one address trips this before exhausting
	// any single share's own budget.
	accessPerIPRate   = 60
	accessPerIPWindow = time.Minute

	// accessPerTokenRate bounds how many access attempts one specific
	// token hash may receive per accessPerTokenWindow, regardless of which
	// IP presents them -- the dimension that catches password-guessing
	// against one known-valid share: an attacker distributing guesses
	// across many source addresses still trips this, because it is keyed
	// on the token, not the caller.
	accessPerTokenRate   = 20
	accessPerTokenWindow = time.Minute
)

// ErrRateLimited reports that Service.Create or Service.AccessPublic denied
// a request under this module's rate-limit budget. Status is 429, not one
// of apperr's five builder shapes, matching go/org's ErrInvitationRateLimited
// and go/integration's identical ErrRateLimited -- a struct literal rather
// than apperr.Forbidden or apperr.Invalid, since neither status fits a
// rate-limit refusal. WithParam("dimension", ...) records which dimension
// tripped (never which token or tenant -- see checkAccessRateLimit's own
// doc comment for why a caller-visible dimension name is safe here but a
// caller-visible key is not) and WithParam("retry_after_seconds", ...)
// records how long until the tripped window recovers.
var ErrRateLimited = &apperr.Error{Code: "sharing.rate_limited", Status: http.StatusTooManyRequests}

// rateLimiter returns the injected limiter (set by a test through
// Service.limiter -- see service_test.go), or builds one over the host's
// KVStore. Building it here rather than caching it at construction is what
// keeps host seams read at call time, the same rule events.go's publish and
// emitSensitiveAudit already follow for the event bus and audit-action
// registrar.
func (s *Service) rateLimiter() (ratelimit.Limiter, error) {
	if s.limiter != nil {
		return s.limiter, nil
	}
	if s.host == nil {
		return nil, errShareNoHostRegistry
	}
	kv := s.host.KVStore()
	if kv == nil {
		return nil, errShareNoKVStore
	}
	return ratelimit.New(kv), nil
}

// checkCreateRateLimit guards Service.Create's one dimension: how many
// shares tenant has created recently. Before Module.Register has attached a
// registry (see Service's own doc comment on being "inert until Register"),
// this reports the wiring error unmodified rather than silently skipping
// the check -- Create's own caller sees ErrInternal either way, since a
// rate limiter that cannot answer must never be treated as "allow" (the
// same fail-closed rule go/ratelimit.Limiter's own doc comment states).
func (s *Service) checkCreateRateLimit(ctx context.Context, tenant string) error {
	limiter, err := s.rateLimiter()
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	decision, err := limiter.Allow(ctx, "sharing:create:tenant:"+tenant, ratelimit.Limit{
		Rate: createPerTenantRate, Per: createPerTenantWindow,
	})
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if !decision.Allowed {
		return ErrRateLimited.
			WithParam("dimension", "tenant").
			WithParam("retry_after_seconds", int(decision.ResetAfter.Seconds()))
	}
	return nil
}

// checkAccessRateLimit guards Service.AccessPublic's two dimensions -- the
// caller's IP address and the token hash being presented -- evaluated in
// that order, any single denial denying the whole call before the other
// dimension is even touched (the identical short-circuit rationale
// go/integration's LayeredLimiter.Allow documents: a request already known
// to be denied should not spend more of the narrower dimension's own
// quota).
//
// The IP key is used as given by the caller (AccessParams.IP, exactly as
// recorded on the access log -- see that field's own doc comment for why
// this module neither parses nor validates it); the token key is the
// ALREADY-HASHED value AccessPublic and Access both key their own
// repository lookups on, never the raw token -- a rate-limit key lives in
// the KV store and tends to appear in diagnostics, and this module's own
// established rule (repository.go's byTokenHash doc comment) is that the
// raw bearer credential never travels anywhere past the caller who
// presented it.
//
// An empty ip (a caller that supplied none) shares one counter with every
// other empty-IP caller, exactly as go/integration's Extractor doc comment
// records for its own optional dimensions -- a caller-visible consequence
// of supplying no better identifier, not a special case this method
// handles.
func (s *Service) checkAccessRateLimit(ctx context.Context, ip, tokenHash string) error {
	limiter, err := s.rateLimiter()
	if err != nil {
		return ErrInternal.WithCause(err)
	}

	dimensions := []struct {
		name  string
		key   string
		limit ratelimit.Limit
	}{
		{"ip", "sharing:access:ip:" + ip, ratelimit.Limit{Rate: accessPerIPRate, Per: accessPerIPWindow}},
		{"token", "sharing:access:token:" + tokenHash, ratelimit.Limit{Rate: accessPerTokenRate, Per: accessPerTokenWindow}},
	}
	for _, d := range dimensions {
		decision, err := limiter.Allow(ctx, d.key, d.limit)
		if err != nil {
			return ErrInternal.WithCause(err)
		}
		if !decision.Allowed {
			return ErrRateLimited.
				WithParam("dimension", d.name).
				WithParam("retry_after_seconds", int(decision.ResetAfter.Seconds()))
		}
	}
	return nil
}
