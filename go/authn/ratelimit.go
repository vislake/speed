package authn

import (
	"context"
	"math"
	"strconv"
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
	//
	// The window is measured from the FIRST failure of the run, not
	// refreshed by every failure after it: KVStore's atomic primitives
	// attach an expiry only on the call that creates the key and never
	// extend a live one (see pkgcore.KVStore.IncrByFloatWithTTL's own doc
	// comment and go/ratelimit's "The TTL-attachment race, closed" for the
	// doctrine), and this module's own earlier sliding refresh was exactly
	// the caller-side Get-then-Set race that doctrine exists to close.
	// RecordLoginFailure's own doc comment states what that costs at the
	// edge of a long run.
	loginLockoutStateTTL = time.Hour

	loginLockoutKeyPrefix = "authn:lockout:login:"
)

// loginLockoutState is what the readers of one account's lockout state see,
// assembled from the two KVStore keys RecordLoginFailure maintains. It is a
// plain in-memory value, never itself stored -- loginLockoutKeys' own doc
// comment explains the split storage layout and why it exists.
type loginLockoutState struct {
	Failures    int
	LockedUntil time.Time
}

// loginLockoutKeys names the two KVStore keys one account's lockout state
// lives under. The split exists because KVStore offers no atomic primitive
// for a two-field value: a shared read-modify-write cycle over a single
// JSON blob -- read it, mutate it, write it back -- loses concurrent
// updates, and the whole point of this layout is that concurrent failures
// are all counted (an attacker bursting the per-minute quota must not keep
// the escalating delay from rising, which is what a lost count does).
//
//   - failuresKey holds the run's failure count as KVStore's own numeric
//     encoding, maintained with IncrByFloatWithTTL: the increment and the
//     attach-expiry-on-creation collapse into one atomic call, so no
//     concurrent recorder can lose its own failure, on a fresh key or a
//     live one alike.
//
//   - deadlineKey holds the run's lockout deadline: the Unix-microsecond
//     instant the account becomes usable again. The deadline only ever
//     moves forward -- every recorded failure proposes now+delay(failures),
//     a later instant than any deadline already on the key -- so concurrent
//     recorders converge on the LATEST proposal with a compare-and-swap
//     (retried on contention) instead of a read-modify-write; no completion
//     order can ever leave the key holding an earlier deadline than the one
//     the run has earned. Its expiry is attached the same atomic way, with
//     a zero-delta increment on the call that creates it. Values are
//     written as plain decimal microseconds and read with
//     strconv.ParseFloat, per the numeric stores' own contract.
//
// Both keys carry loginLockoutStateTTL, attached when the run that creates
// them starts; RecordLoginSuccess deletes both, which is what makes a
// successful sign-in reset the run. The single JSON key an earlier version
// of this module stored the whole state under is never written and never
// read: any value a pre-split deployment left there simply expires within
// its own TTL, and the account it belonged to starts from a clean slate.
func loginLockoutKeys(account string) (failuresKey, deadlineKey string) {
	return loginLockoutKeyPrefix + account + ":failures",
		loginLockoutKeyPrefix + account + ":locked_until"
}

// loginLockoutDelay returns the progressive delay a run of failures has
// earned: loginLockoutBase for the first failure, doubling per failure
// after it and saturating at loginLockoutMax, which is what turns "growing
// delay" into an effective lockout. It is the pure arithmetic the recorded
// state derives from, split out so the saturation curve is testable
// directly.
func loginLockoutDelay(failures int) time.Duration {
	delay := loginLockoutBase
	for range failures - 1 {
		delay *= 2
		if delay >= loginLockoutMax {
			return loginLockoutMax
		}
	}
	return delay
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
			return ErrAccountLocked.WithParam("retry_after_seconds", retryAfterSecondsFromDuration(retryAfter))
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
//
// The recording is built from KVStore's atomic primitives rather than from
// a read-modify-write cycle over a whole state value, because the two are
// not the same thing under concurrency: a burst of login attempts that all
// fail at once -- the exact shape an attacker produces while bursting the
// per-minute quota -- used to lose most of its own failures (five
// concurrent failures were measured landing as two), and since the delay
// doubles per recorded failure, the lost ones are precisely what keeps the
// progressive lockout from rising. See loginLockoutKeys' own doc comment
// for the layout that closes that, and loginLockoutStateTTL's for the one
// boundary it moves: the failure run's memory window now runs from the
// run's first failure rather than being refreshed by each failure, so a
// run that keeps failing for more than an hour ends at the window's edge
// -- cutting short at most loginLockoutMax of whatever lockout it had
// earned -- and the next failure starts a fresh run. Within a run,
// escalation is unchanged, and no concurrent failure can be lost.
func (g *rateGuard) RecordLoginFailure(ctx context.Context, account string) {
	if account == "" {
		return
	}
	failuresKey, deadlineKey := loginLockoutKeys(account)

	// The failure count: one atomic increment, with the run-memory expiry
	// attached on the same call when this is the run's first failure.
	// Every backend implements this as a single atomic operation, so N
	// concurrent recorders end with a count of exactly N.
	count, err := g.kv.IncrByFloatWithTTL(ctx, failuresKey, 1, loginLockoutStateTTL)
	if err != nil {
		obs.FromContext(ctx).Warn("login lockout count could not be recorded", "error", err)
		return
	}
	failures := int(count)

	// The deadline this failure proposes: now plus the delay the count it
	// just produced has earned. Stored as Unix microseconds in decimal
	// text, an integer well inside float64's exact range.
	proposal := time.Now().Add(loginLockoutDelay(failures)).UnixMicro()

	// The deadline itself: a compare-and-swap that only ever moves the key
	// FORWARD, retried when a concurrent recorder won the race in between.
	// The swap condition -- propose only when the standing deadline is
	// earlier, stop when an equal-or-later one already stands -- keeps the
	// key monotone non-decreasing under any completion order, so a burst's
	// final value is the latest deadline any of its recorders proposed:
	// exactly what the same failures recorded one after another would have
	// left. A recorder whose proposal loses to a later standing deadline
	// has nothing to add -- the account is locked at least as long as its
	// own failure alone would have locked it -- and a stale reader can
	// never move the key backward.
	for range loginLockoutCASAttempts {
		deadlineBytes, found, err := g.kv.Get(ctx, deadlineKey)
		if err != nil {
			obs.FromContext(ctx).Warn("login lockout deadline could not be read", "error", err)
			return
		}
		if !found {
			// The key is absent -- a run is just starting, or a
			// concurrent RecordLoginSuccess deleted it between our
			// count increment and this read. Attach its expiry with a
			// zero-delta increment rather than letting the
			// compare-and-swap below create it with no expiry at all:
			// without this, a sprayed account whose failures race its
			// owner's successes could leave a deadline key behind that
			// never ages out.
			if _, initErr := g.kv.IncrByFloatWithTTL(ctx, deadlineKey, 0, loginLockoutStateTTL); initErr != nil {
				obs.FromContext(ctx).Warn("login lockout deadline could not be initialized", "error", initErr)
				return
			}
			continue
		}
		// The stored encoding is the store's own shortest-decimal float
		// text, which can be exponent-shaped for values this large
		// (pkgcore's contract: parse with strconv.ParseFloat, never as
		// text). Unix microseconds stay exact through float64 well past
		// any real timestamp.
		stored, parseErr := strconv.ParseFloat(string(deadlineBytes), 64)
		if parseErr != nil {
			obs.FromContext(ctx).Warn("login lockout deadline is unreadable", "error", parseErr)
			return
		}
		current := int64(stored)
		if current >= proposal {
			// A deadline at least as late as this failure's already
			// stands; nothing to add.
			return
		}
		// The old value is the exact bytes just read: a compare-and-swap
		// compares bytes, so re-encoding the parsed value could never
		// match a store whose canonical float text differs from ours.
		// The new value is our own plain decimal, which the numeric
		// stores parse and re-encode freely.
		swapped, err := g.kv.CompareAndSwap(ctx, deadlineKey,
			deadlineBytes, []byte(strconv.FormatInt(proposal, 10)))
		if err != nil {
			obs.FromContext(ctx).Warn("login lockout deadline could not be extended", "error", err)
			return
		}
		if swapped {
			return
		}
		// Lost the race to a concurrent recorder: retry against the value
		// it left behind.
	}
	obs.FromContext(ctx).Warn("login lockout deadline could not be extended after repeated contention")
}

// loginLockoutCASAttempts bounds the compare-and-swap retries one recorded
// failure may make against the deadline key. Exhausting it is not a path
// that is expected to be taken: contention means other recorders of the
// SAME account are completing, each of which is itself extending the
// deadline, so even a recorder that gives up here has not cost the account
// its lockout -- only this one failure's own increment toward the next
// escalation remains unrepresented in the deadline (the failure count it
// produced is already on the failures key, so the delay the NEXT failure
// computes still sees it).
const loginLockoutCASAttempts = 8

// RecordLoginSuccess clears account's progressive delay. Deleting a key
// that is not there is not an error, so clearing half-written state (a
// count without a deadline, say) is just as safe as clearing the full run.
func (g *rateGuard) RecordLoginSuccess(ctx context.Context, account string) {
	if account == "" {
		return
	}
	failuresKey, deadlineKey := loginLockoutKeys(account)
	for _, key := range []string{failuresKey, deadlineKey} {
		if err := g.kv.Delete(ctx, key); err != nil {
			obs.FromContext(ctx).Warn("login lockout state could not be cleared", "error", err)
		}
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

// CheckSMSVerifyIP refuses a phone-login code verification attempt from ip
// when the IP dimension is over limit. Unlike CheckSMSVerifyWrongGuess
// below, this is checked unconditionally, before the presented code is
// even compared: it protects against sheer request volume from one
// source, a property that does not depend on WHICH target the request
// names, so consulting it up front creates no hostage-the-victim's-own-
// budget property the way the old, unconditional per-target check did
// (see CheckSMSVerifyWrongGuess's own doc comment).
func (g *rateGuard) CheckSMSVerifyIP(ctx context.Context, ip string) error {
	return g.allow(ctx, "authn:sms:verify:ip:"+ip, limitSMSVerifyByIP)
}

// CheckSMSVerifyWrongGuess records one wrong phone-login code guess against
// target and refuses further guessing once the per-target dimension is
// over limit.
//
// This is deliberately consulted -- and its budget deliberately consumed --
// ONLY after a guess has already been determined wrong (see verification.
// go's LoginWithSMSCode, the one caller), never unconditionally before the
// code is even compared, which is what this dimension's shape used to do
// and what CheckSMSVerifyIP's own IP dimension still does today. A shared
// per-target budget consumed on every attempt regardless of outcome can be
// exhausted by an attacker who does not hold the real code and therefore
// never succeeds -- 5 wrong guesses from anywhere within the window
// permanently deny the real holder's own correct attempt for the rest of
// it, because that attempt is refused by THIS check before it ever reaches
// the comparison that would have told the two apart. Gating the budget on
// "the guess just presented was wrong" instead means a correct code is
// NEVER refused for budget reasons, no matter how many wrong guesses (from
// however many sources) already exhausted it.
//
// This does not weaken brute-force resistance against the code itself:
// that protection is DefaultSMSCodeMaxAttempts (verification.go), a
// per-code counter enforced by verifyPhoneLoginCode independently of this
// rate limiter, which locks a code after a fixed number of wrong guesses
// regardless of source or of this dimension's own state. This dimension's
// remaining role is throttling how often one target's wrong guesses can
// recur across MULTIPLE issued codes within a window -- CheckSMSSend's own
// per-target send limit already bounds how many fresh codes (each with its
// own MaxAttempts budget) a window can produce.
func (g *rateGuard) CheckSMSVerifyWrongGuess(ctx context.Context, target string) error {
	return g.allow(ctx, "authn:sms:verify:target:"+target, limitSMSVerifyByTarget)
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
		return ErrRateLimited.WithParam("retry_after_seconds", retryAfterSecondsFromDuration(decision.ResetAfter))
	}
	return nil
}

// retryAfterSecondsFromDuration converts a lockout or window remainder to
// the whole seconds a Retry-After header is expressed in, rounding UP.
// Retry-After counts whole delay-seconds (RFC 9110, §10.2.3), and the
// truncating int(Seconds()) conversion this replaces would emit 0 for any
// sub-second remainder -- legal per the RFC, but wrong here: 0 means
// "retry immediately", telling a client the window has reset up to a
// second before it actually has and inviting an immediate retry that is
// still refused. Rounding up errs the other way, telling the client to
// wait at most one second longer than it strictly needs.
func retryAfterSecondsFromDuration(remaining time.Duration) int {
	return int(math.Ceil(remaining.Seconds()))
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
	state := loginLockoutState{}
	failuresKey, deadlineKey := loginLockoutKeys(account)

	countRaw, found, err := g.kv.Get(ctx, failuresKey)
	if err != nil {
		return loginLockoutState{}, err
	}
	if found {
		count, parseErr := strconv.ParseFloat(string(countRaw), 64)
		if parseErr != nil {
			return loginLockoutState{}, parseErr
		}
		state.Failures = int(count)
	}

	deadlineRaw, found, err := g.kv.Get(ctx, deadlineKey)
	if err != nil {
		return loginLockoutState{}, err
	}
	if found {
		micro, parseErr := strconv.ParseFloat(string(deadlineRaw), 64)
		if parseErr != nil {
			return loginLockoutState{}, parseErr
		}
		state.LockedUntil = time.UnixMicro(int64(micro))
	}
	return state, nil
}
