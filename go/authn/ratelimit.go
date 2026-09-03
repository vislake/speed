package authn

import (
	"context"
	"encoding/json"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"

	obs "github.com/vislake/speed/go/observability"
)

// Sliding-window limits for every dimension this module's login,
// registration, code-send and code-verify endpoints are guarded on.
//
// These are package-level named constants, per the root CLAUDE.md's
// Configuration and Constants section: they are stable domain defaults,
// not values that vary by environment or that operations needs to tune
// live, and go/ratelimit itself deliberately carries no
// dynamic-configuration hook of its own to plug them into (see its
// AGENTS.md's "No dynamic configuration").
var (
	limitLoginByAccount    = ratelimit.Limit{Rate: 5, Per: time.Minute}
	limitLoginByIP         = ratelimit.Limit{Rate: 20, Per: time.Minute}
	limitRegisterByIP      = ratelimit.Limit{Rate: 10, Per: time.Hour}
	limitSMSSendByTarget   = ratelimit.Limit{Rate: 3, Per: 10 * time.Minute}
	limitSMSSendByIP       = ratelimit.Limit{Rate: 10, Per: 10 * time.Minute}
	limitSMSVerifyByTarget = ratelimit.Limit{Rate: 5, Per: 10 * time.Minute}
	limitSMSVerifyByIP     = ratelimit.Limit{Rate: 20, Per: 10 * time.Minute}
	limitStepUpByAccount   = ratelimit.Limit{Rate: 5, Per: 5 * time.Minute}
	limitStepUpByIP        = ratelimit.Limit{Rate: 20, Per: 5 * time.Minute}
)

const (
	// loginLockoutBase is the delay after the FIRST recorded failure. It
	// doubles with every failure after that, reaching loginLockoutMax
	// after roughly five consecutive failures with the values below --
	// see RecordLoginFailure.
	loginLockoutBase = 30 * time.Second

	// loginLockoutMax is the delay's ceiling, which is what turns
	// "growing delay" into an effective lockout: once a run of failures
	// reaches it, every further failure keeps the account locked for
	// exactly this long, not longer and not shorter.
	loginLockoutMax = 15 * time.Minute

	// loginLockoutStateTTL bounds how long a failure run is remembered.
	// An attacker (or a person who genuinely forgot their password) who
	// waits this long out returns to a clean slate; remembering forever
	// would turn a transient lockout into a de facto permanent one for an
	// account nobody is actively attacking any more.
	loginLockoutStateTTL = time.Hour

	loginLockoutKeyPrefix = "authn:lockout:login:"
)

// loginLockoutState is what rateGuard keeps in the shared pkgcore.KVStore
// per account, encoded as JSON: KVStore's contract is opaque bytes, and this
// module has no reason to invent a binary encoding for two fields.
type loginLockoutState struct {
	Failures    int       `json:"failures"`
	LockedUntil time.Time `json:"locked_until"`
}

// rateGuard is where go/ratelimit's sliding-window counters (raw request
// volume) and this module's own progressive login delay/lockout (business
// logic go/ratelimit deliberately does not implement -- see its AGENTS.md's
// "No multi-dimension or progressive/escalating semantics, on purpose")
// meet. Every method fails CLOSED: an error from the underlying limiter or
// from the KVStore reading the lockout state is treated as "deny", never as
// "allow" -- the same policy Middleware's revocation check and Service's
// membership check already apply to their own unanswerable questions.
type rateGuard struct {
	limiter ratelimit.Limiter
	kv      pkgcore.KVStore
}

// newRateGuard builds a rateGuard over kv, the shared pkgcore.KVStore seam
// -- an in-memory store in the standalone deployment mode, Redis in the
// distributed one, with neither named here.
func newRateGuard(kv pkgcore.KVStore) *rateGuard {
	return &rateGuard{limiter: ratelimit.New(kv), kv: kv}
}

// CheckLogin refuses a login attempt for account (its blind index, or
// empty when the identifier had no canonical form) and ip when either
// sliding-window dimension is over limit, or account is still inside its
// progressive lockout window.
func (g *rateGuard) CheckLogin(ctx context.Context, account, ip string) error {
	if account != "" {
		locked, retryAfter, err := g.loginLocked(ctx, account)
		if err != nil {
			obs.FromContext(ctx).Error("login lockout state could not be read", "error", err)
			return ErrRateLimited
		}
		if locked {
			return ErrAccountLocked.WithParam("retry_after_seconds", int(retryAfter.Seconds()))
		}
		if err := g.allow(ctx, "authn:login:account:"+account, limitLoginByAccount); err != nil {
			return err
		}
	}
	return g.allow(ctx, "authn:login:ip:"+ip, limitLoginByIP)
}

// RecordLoginFailure grows account's progressive delay, which saturates at
// loginLockoutMax -- see this file's own doc comment for why one
// continuously-growing value plays both the "delay" and the "lockout"
// role. A KVStore failure here is logged and swallowed rather than
// returned: the login attempt itself has already been refused or accepted
// by the caller, and failing to RECORD that fact must not additionally
// fail the response the caller already committed to.
func (g *rateGuard) RecordLoginFailure(ctx context.Context, account string) {
	if account == "" {
		return
	}
	state, err := g.readLoginLockoutState(ctx, account)
	if err != nil {
		obs.FromContext(ctx).Warn("login lockout state could not be read while recording a failure", "error", err)
		state = loginLockoutState{}
	}
	state.Failures++
	delay := loginLockoutBase
	for range state.Failures - 1 {
		delay *= 2
		if delay >= loginLockoutMax {
			delay = loginLockoutMax
			break
		}
	}
	state.LockedUntil = time.Now().Add(delay)

	encoded, err := json.Marshal(state)
	if err != nil {
		obs.FromContext(ctx).Warn("login lockout state could not be encoded", "error", err)
		return
	}
	if err := g.kv.Set(ctx, loginLockoutKeyPrefix+account, encoded, loginLockoutStateTTL); err != nil {
		obs.FromContext(ctx).Warn("login lockout state could not be recorded", "error", err)
	}
}

// RecordLoginSuccess clears account's progressive delay.
func (g *rateGuard) RecordLoginSuccess(ctx context.Context, account string) {
	if account == "" {
		return
	}
	if err := g.kv.Delete(ctx, loginLockoutKeyPrefix+account); err != nil {
		obs.FromContext(ctx).Warn("login lockout state could not be cleared", "error", err)
	}
}

// CheckRegister refuses a registration attempt from ip when the IP-only
// dimension is over limit -- there is no account yet to key a second
// dimension on.
func (g *rateGuard) CheckRegister(ctx context.Context, ip string) error {
	return g.allow(ctx, "authn:register:ip:"+ip, limitRegisterByIP)
}

// CheckSMSSend refuses a phone-login code REQUEST for target (the phone
// number's blind index) and ip when either dimension is over limit.
func (g *rateGuard) CheckSMSSend(ctx context.Context, target, ip string) error {
	if err := g.allow(ctx, "authn:sms:send:target:"+target, limitSMSSendByTarget); err != nil {
		return err
	}
	return g.allow(ctx, "authn:sms:send:ip:"+ip, limitSMSSendByIP)
}

// CheckSMSVerify refuses a phone-login code VERIFICATION for target and ip
// when either dimension is over limit.
func (g *rateGuard) CheckSMSVerify(ctx context.Context, target, ip string) error {
	if err := g.allow(ctx, "authn:sms:verify:target:"+target, limitSMSVerifyByTarget); err != nil {
		return err
	}
	return g.allow(ctx, "authn:sms:verify:ip:"+ip, limitSMSVerifyByIP)
}

// CheckStepUp refuses a step-up verification for account (the user id) and
// ip when either dimension is over limit.
func (g *rateGuard) CheckStepUp(ctx context.Context, account, ip string) error {
	if err := g.allow(ctx, "authn:stepup:account:"+account, limitStepUpByAccount); err != nil {
		return err
	}
	return g.allow(ctx, "authn:stepup:ip:"+ip, limitStepUpByIP)
}

// allow is the shared sliding-window check every dimension above is built
// from. It never treats a limiter error as "allow": go/ratelimit's own
// contract (Allow's doc comment) is that it returns a KVStore failure
// unmodified so each call site can choose fail-open or fail-closed for
// itself, and every endpoint this module guards is security-sensitive
// enough that the choice here is always closed.
func (g *rateGuard) allow(ctx context.Context, key string, limit ratelimit.Limit) error {
	decision, err := g.limiter.Allow(ctx, key, limit)
	if err != nil {
		obs.FromContext(ctx).Error("rate limit check failed", "error", err)
		return ErrRateLimited
	}
	if !decision.Allowed {
		return ErrRateLimited.WithParam("retry_after_seconds", int(decision.ResetAfter.Seconds()))
	}
	return nil
}

// loginLocked reports whether account is currently inside its progressive
// lockout window, and if so for how much longer.
func (g *rateGuard) loginLocked(ctx context.Context, account string) (bool, time.Duration, error) {
	state, err := g.readLoginLockoutState(ctx, account)
	if err != nil {
		return false, 0, err
	}
	remaining := time.Until(state.LockedUntil)
	if remaining <= 0 {
		return false, 0, nil
	}
	return true, remaining, nil
}

// readLoginLockoutState reads account's lockout state, returning the zero
// value (never locked, zero failures) when none is stored yet.
func (g *rateGuard) readLoginLockoutState(ctx context.Context, account string) (loginLockoutState, error) {
	raw, found, err := g.kv.Get(ctx, loginLockoutKeyPrefix+account)
	if err != nil {
		return loginLockoutState{}, err
	}
	if !found {
		return loginLockoutState{}, nil
	}
	var state loginLockoutState
	if err := json.Unmarshal(raw, &state); err != nil {
		return loginLockoutState{}, err
	}
	return state, nil
}
