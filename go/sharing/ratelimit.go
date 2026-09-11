package sharing

import (
	"context"
	"net/http"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// This file applies go/ratelimit to both call sites this module's abuse
// surface actually has -- Service.Create (share-creation abuse, one
// dimension) and Service.AccessPublic (token-guessing and password-guessing
// abuse, two dimensions enforced at two deliberately different points in the
// access flow: the caller-IP dimension unconditionally, in
// accessPublicPrelude before the presented token is even resolved, and the
// per-token dimension only after a presented credential has been judged
// wrong, inside authorizeAttempt -- see checkAccessIPLimit and
// checkAccessTokenWrongGuess for why each dimension's consumption sits
// where it does). Every check is one ratelimit.Limiter.Allow call on the
// dimension's own key, built on the shared allowRateLimit helper, with the
// underlying Limiter built lazily over the host's KVStore (rateLimiter,
// below) so it always reads whichever implementation the running deployment
// mode actually resolved, never one captured before the assembly ran.
//
// Deliberately NOT a Module Option: unlike TenantConfigReader (an
// optional-by-design seam with a documented, correct default) or WithQueue
// (optional row hygiene), rate limiting here has no meaningful "off"
// default a host would ever deliberately choose for a genuinely
// unauthenticated public endpoint -- so rather than adding a WithRateLimiter
// wiring point a host could forget, this module reads the registry's own
// KVStore, which the builtin composition always resolves for every deployment
// mode, through the identical hostSeams.KVStore() seam
// org.InviteService.rateLimiter reads. A host cannot boot this module at
// all without a KVStore somewhere in its composition (the builtin composition always resolves one), so rate limiting is real the moment
// Module.Register attaches the registry, with no separate opt-in to forget.

// The rate-limit budget this module applies. Package constants, not dynamic
// configuration: sharing cannot read a live go/config value without adding
// that dependency (the same reasoning org applies to its own invite rate
// limits), and a declared config schema this module would then ignore
// would be a lying schema, worse than a constant.
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
	// any single share's own wrong-guess budget. Unconditional by design:
	// the party an IP budget protects (the platform against that address's
	// request volume) and the party that consumes it (the address itself)
	// are the same caller, so no attempt of anyone else's can ever be held
	// hostage by it (checkAccessIPLimit's own doc comment).
	accessPerIPRate   = 60
	accessPerIPWindow = time.Minute

	// accessPerTokenRate bounds how many WRONG-CREDENTIAL attempts one
	// specific token hash may make per accessPerTokenWindow, regardless of
	// which IP presents them -- the dimension that catches password-guessing
	// against one known-valid, password-protected share: an attacker
	// distributing guesses across many source addresses still trips this,
	// because it is keyed on the token, not the caller. The budget is
	// deliberately consumed only after an attempt has been judged wrong
	// (checkAccessTokenWrongGuess's own doc comment): a fully legitimate
	// attempt -- a correct password, or any attempt on a passwordless
	// share -- never pays it, so per-token volume of legitimate access is
	// not capped across IPs. Capping every attempt regardless of outcome
	// would let one link holder who lacks the password exhaust the budget
	// and deny the legitimate password-holder's correct attempt, the
	// hostage property checkAccessTokenWrongGuess's doc comment argues
	// against in full; volume abuse of a share whose credentials are fully
	// known is left to the per-IP dimension and the share's own MaxViews
	// ceiling.
	accessPerTokenRate   = 20
	accessPerTokenWindow = time.Minute
)

// ErrRateLimited reports that Service.Create, Service.AccessPublic or the
// public access route's authorizePublicAccess denied a request under this
// module's rate-limit budget. Status is 429, not one
// of apperr's five builder shapes, matching go/org's ErrInvitationRateLimited
// and go/integration's identical ErrRateLimited -- a struct literal rather
// than apperr.Forbidden or apperr.Invalid, since neither status fits a
// rate-limit refusal. WithParam("dimension", ...) records which dimension
// tripped (never which token or tenant -- see allowRateLimit's own
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
// the check -- Create's own caller sees ErrInternal either way, because a
// limiter that cannot answer is never treated as "allow" here: this module
// fails closed on its own, since a check that silently passed while its
// store was down would leave exactly the share-creation abuse this budget
// bounds (a compromised or careless tenant minting links) unguarded.
func (s *Service) checkCreateRateLimit(ctx context.Context, tenant string) error {
	return s.allowRateLimit(ctx, "sharing:create:tenant:"+tenant, ratelimit.Limit{
		Rate: createPerTenantRate, Per: createPerTenantWindow,
	}, "tenant")
}

// checkAccessIPLimit refuses an access attempt from ip when the caller's own
// per-IP budget is spent. This dimension is checked UNCONDITIONALLY, in
// accessPublicPrelude before the presented token is even resolved, and that
// placement is deliberate: an IP budget's protected party (the platform,
// against one address's request volume across every token it tries) and its
// consuming party (the address itself) are the same caller, so an address
// that exhausts its own budget refuses only itself -- no attempt of a
// different caller, however legitimate, can ever be held hostage by it the
// way a shared per-target budget can (checkAccessTokenWrongGuess's own doc
// comment argues that dimension's opposite placement). Refusing an
// over-budget address up front, before the token lookup, is also what keeps
// the unrecognized-token path cheap: a scanner spraying random tokens pays
// one rate-limit hit plus one token-index lookup per guess, never an
// argon2id burn (AccessPublic's own doc comment has the full
// anti-amplification argument).
//
// The key is used as given by the caller (AccessParams.IP, exactly as
// recorded on the access log -- see that field's own doc comment for why
// this module neither parses nor validates it). An empty ip (a caller that
// supplied none) shares one counter with every other empty-IP caller,
// exactly as go/integration's Extractor doc comment records for its own
// optional dimensions -- a caller-visible consequence of supplying no
// better identifier, not a special case this method handles.
func (s *Service) checkAccessIPLimit(ctx context.Context, ip string) error {
	return s.allowRateLimit(ctx, "sharing:access:ip:"+ip, ratelimit.Limit{Rate: accessPerIPRate, Per: accessPerIPWindow}, "ip")
}

// checkAccessTokenWrongGuess records one wrong-credential attempt against a
// password-protected share's token and refuses further guessing once the
// per-token budget is spent.
//
// This is deliberately consulted -- and its budget deliberately consumed --
// ONLY after the attempt has already been judged illegitimate, from
// authorizeAttempt's wrong-credential branch, its one caller; never
// unconditionally before the password comparison. A shared per-target
// budget consumed on every attempt regardless of outcome can be exhausted
// by an attacker who holds the link but not the password -- and the entire
// point of protecting a share with a password is that the link may leak --
// permanently denying the legitimate password-holder's own correct attempt
// for the rest of the window, because that attempt is refused by the budget
// check before it ever reaches the comparison that would have told the two
// apart. Gating the budget on "the presented credential was just judged
// wrong" instead means a correct attempt is NEVER refused for budget
// reasons: it is never judged wrong, so it never reaches this check at all,
// no matter how many wrong guesses from however many sources already
// exhausted the budget. This is the identical reasoning go/authn applies to
// its own per-target wrong-guess dimension (go/authn/ratelimit.go's
// CheckSMSVerifyWrongGuess and its doc comment, argued against the same
// hostage property).
//
// This does not weaken brute-force resistance: every wrong guess still pays
// its full argon2id comparison before the budget is touched (the module's
// constant-time equalization, see burnSharePasswordCheck's doc comment; the
// comparison must run to judge the guess wrong), the per-IP dimension still
// caps how fast any one source can
// present guesses, and once the budget is spent the guesser's further
// wrong guesses are refused with ErrRateLimited rather than the 404-shaped
// refusal an under-budget wrong guess answers with. The one cost of the
// after-judgment shape is recorded on accessPerTokenRate's own comment: a
// fully legitimate attempt never pays the per-token budget, so per-token
// volume of legitimate access is no longer capped across IPs -- the price
// of no longer letting one holder deny another.
//
// The key is the ALREADY-HASHED value AccessPublic and Access both key
// their own repository lookups on, never the raw token -- a rate-limit key
// lives in the KV store and tends to appear in diagnostics, and this
// module's own established rule (repository.go's byTokenHash doc comment)
// is that the raw bearer credential never travels anywhere past the caller
// who presented it.
func (s *Service) checkAccessTokenWrongGuess(ctx context.Context, tokenHash string) error {
	return s.allowRateLimit(ctx, "sharing:access:token:"+tokenHash, ratelimit.Limit{Rate: accessPerTokenRate, Per: accessPerTokenWindow}, "token")
}

// allowRateLimit is the shared check every dimension above is built from:
// one ratelimit.Limiter.Allow call on key under limit, decorating a denial
// as ErrRateLimited with the dimension named (never the key itself -- a
// caller-visible dimension name is safe here but a caller-visible key is
// not, see ErrRateLimited's own doc comment) and the window's recovery time
// recorded, and an unavailable limiter or store failure as ErrInternal:
// fail closed, never "allow". That is this module's own choice, the same
// for every dimension: each of the three checks above guards an
// abuse-facing surface (share-creation volume, token guessing,
// wrong-credential guessing) whose only throttle is the check itself, so
// a store outage that read as "allow" would leave that abuse unthrottled.
func (s *Service) allowRateLimit(ctx context.Context, key string, limit ratelimit.Limit, dimension string) error {
	limiter, err := s.rateLimiter()
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	decision, err := limiter.Allow(ctx, key, limit)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if !decision.Allowed {
		return ErrRateLimited.
			WithParam("dimension", dimension).
			WithParam("retry_after_seconds", ratelimit.RetryAfterSeconds(decision.ResetAfter))
	}
	return nil
}
