package sharing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// scriptedLimiter is a minimal, in-test ratelimit.Limiter double whose
// every Allow call answers the same fixed Decision, so a test can drive
// Service's rate-limit checks deterministically without waiting out
// createPerTenantWindow/accessPerIPWindow/accessPerTokenWindow's real
// durations.
type scriptedLimiter struct {
	allowed    bool
	resetAfter time.Duration
	err        error
}

func (s scriptedLimiter) Allow(context.Context, string, ratelimit.Limit) (ratelimit.Decision, error) {
	if s.err != nil {
		return ratelimit.Decision{}, s.err
	}
	return ratelimit.Decision{Allowed: s.allowed, ResetAfter: s.resetAfter}, nil
}

func TestService_RateLimiter_FallsBackToHostKVStore(t *testing.T) {
	svc, _ := newTestService(t, nil)
	limiter, err := svc.rateLimiter()
	if err != nil {
		t.Fatalf("rateLimiter: %v", err)
	}
	if limiter == nil {
		t.Fatalf("rateLimiter() = nil, want a real ratelimit.Limiter over the host's KVStore")
	}
}

func TestService_RateLimiter_PrefersInjectedOverride(t *testing.T) {
	svc, _ := newTestService(t, nil)
	want := scriptedLimiter{allowed: true}
	svc.limiter = want
	got, err := svc.rateLimiter()
	if err != nil {
		t.Fatalf("rateLimiter: %v", err)
	}
	if got != ratelimit.Limiter(want) {
		t.Errorf("rateLimiter() did not return the injected override")
	}
}

// TestService_RateLimiter_NoHostAttached_FailsClosed proves a Service
// built with NewService alone (never attached to a registry) cannot
// silently skip rate limiting -- it reports the wiring error, which
// checkCreateRateLimit, checkAccessIPLimit and checkAccessTokenWrongGuess
// all wrap as ErrInternal through their shared allowRateLimit helper, never
// "allow".
func TestService_RateLimiter_NoHostAttached_FailsClosed(t *testing.T) {
	svc := NewService(newTestDB(t), nil)
	if _, err := svc.rateLimiter(); !errors.Is(err, errShareNoHostRegistry) {
		t.Errorf("rateLimiter() error = %v, want errShareNoHostRegistry", err)
	}
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); !apperr.HasCode(err, ErrInternal.Code) {
		t.Errorf("Create() error = %v, want ErrInternal (rate limiter unavailable must fail closed, not silently allow)", err)
	}
}

func TestService_Create_RateLimited_RefusesWithErrRateLimited(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 42 * time.Second}

	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	assertCode(t, err, ErrRateLimited.Code)

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", err)
	}
	if got := appErr.Params["dimension"]; got != "tenant" {
		t.Errorf("dimension param = %v, want %q", got, "tenant")
	}
	if got := appErr.Params["retry_after_seconds"]; got != 42 {
		t.Errorf("retry_after_seconds param = %v, want 42", got)
	}
}

func TestService_Create_NotRateLimited_Succeeds(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{allowed: true}

	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestService_AccessPublic_RateLimited_RefusesBeforeResolvingTenant(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 7 * time.Second}

	_, err = svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: "203.0.113.1"})
	assertCode(t, err, ErrRateLimited.Code)
}

func TestService_RateLimiter_UnderlyingStoreError_WrapsAsInternal(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{err: errors.New("kv store unavailable")}

	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	assertCode(t, err, ErrInternal.Code)
}

// accessTestIP mints a distinct per-attempt IP so a test can drive the
// per-token dimension alone: accessPerIPRate is 60 per window, so any run
// that stays well below 60 attempts from one address never lets the IP
// dimension interfere with what the token dimension is being made to prove.
func accessTestIP(i int) string {
	return fmt.Sprintf("203.0.113.%d", i)
}

// TestService_AccessPublic_TokenBudgetExhaustedByWrongPasswords_AdmitsTheCorrectHolder
// pins the after-judgment consumption of the per-token wrong-guess budget
// (ratelimit.go's checkAccessTokenWrongGuess): the budget is
// accessPerTokenRate wrong-credential attempts per window, keyed on the
// share's token hash and shared by every IP that presents it. Consumed on
// every attempt regardless of outcome, it could be exhausted by a
// leaked-link holder who does not hold the password and would then hold
// the legitimate password-holder's own correct attempt hostage for the
// rest of the window -- that attempt refused with 429 before it ever
// reached the comparison that would have admitted it. Consumed only AFTER
// a guess has been judged wrong -- the shape go/authn's
// CheckSMSVerifyWrongGuess applies to its own per-target budget -- the
// budget cannot deny a correct attempt: a correct attempt is never judged
// wrong, so it never pays and is never refused.
//
// Legs 1-2 pin the property: twenty wrong-password attempts (one per
// distinct IP, so the per-IP dimension stays out of the way) exhaust the
// per-token budget, and the legitimate holder's correct attempt that
// follows still reaches the password comparison and succeeds. Leg 3 pins
// that the budget still binds illegitimate attempts -- consumed only by
// the wrong guesses, the very next wrong guess is the budget's
// twenty-first hit and is refused with ErrRateLimited on the token
// dimension.
func TestService_AccessPublic_TokenBudgetExhaustedByWrongPasswords_AdmitsTheCorrectHolder(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "s3cret"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Leg 1: exhaust the per-token budget with wrong-password attempts. Each
	// pays the real argon2id comparison before it is answered, exactly as any
	// recognized-token refusal must (rule 5's constant-time equalization), so
	// this leg is a deliberate, bounded stand-in for a leaked-link holder
	// spraying guesses from many addresses.
	for i := 0; i < accessPerTokenRate; i++ {
		wrong := fmt.Sprintf("wrong-%d", i)
		_, guessErr := svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: accessTestIP(i + 1), Password: &wrong})
		assertCode(t, guessErr, ErrNotAccessible.Code)
	}

	// Leg 2: THE regression assertion. The correct attempt arrives after the
	// per-token budget was exhausted by wrong guesses -- it must still reach
	// the password comparison and succeed.
	if _, correctErr := svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: accessTestIP(100), Password: &password}); correctErr != nil {
		t.Fatalf("AccessPublic(correct password after token budget exhausted by wrong guesses) error = %v, want success", correctErr)
	}

	// Leg 3: the budget still binds illegitimate attempts. The correct
	// attempt above consumed nothing, so this next wrong guess is the
	// budget's twenty-first hit and is refused after judgment with
	// ErrRateLimited on the token dimension.
	wrong := "still-wrong"
	_, lastErr := svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: accessTestIP(101), Password: &wrong})
	assertCode(t, lastErr, ErrRateLimited.Code)

	appErr, ok := apperr.As(lastErr)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", lastErr)
	}
	if got := appErr.Params["dimension"]; got != "token" {
		t.Errorf("dimension param = %v, want %q", got, "token")
	}
}

// TestService_AccessPublic_IPDimensionStillLimitsUnconditionally pins
// that the per-IP dimension stays unconditional -- in deliberate contrast
// to the per-token wrong-guess budget's after-judgment consumption: its
// protected party and its consumable party are the same caller (an
// address that exhausts its own budget refuses only itself), so it may
// stay unconditional -- checked in accessPublicPrelude before the token
// is even resolved, refusing an over-budget address whatever it presents,
// a fully legitimate correct attempt included. The IP budget is filled
// with attempts at unrecognized tokens -- each answered cheaply by the
// token-index lookup with no argon2id burn, which is exactly the scanning
// shape the dimension exists to cap -- and the attempt that pushes the
// address over its own limit is the correct holder's: refused with
// ErrRateLimited on the IP dimension before the password comparison is
// ever reached.
func TestService_AccessPublic_IPDimensionStillLimitsUnconditionally(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "s3cret"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ip := accessTestIP(200)
	for i := 0; i < accessPerIPRate; i++ {
		// Every fill attempt uses its own never-issued token: an
		// unrecognized token is answered cheaply (one rate-limit hit plus one
		// token-index lookup, no burn), and no two fills share a per-token
		// key, so the only budget that accumulates is the IP's own.
		_, fillErr := svc.AccessPublic(context.Background(), fmt.Sprintf("never-issued-token-%d", i), AccessParams{IP: ip})
		assertCode(t, fillErr, ErrNotAccessible.Code)
	}

	// The attempt that pushes the address over accessPerIPRate -- the
	// legitimate holder's fully correct attempt -- is still refused up front,
	// before the share is even looked up: unconditional means unconditional.
	_, lastErr := svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: ip, Password: &password})
	assertCode(t, lastErr, ErrRateLimited.Code)

	appErr, ok := apperr.As(lastErr)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", lastErr)
	}
	if got := appErr.Params["dimension"]; got != "ip" {
		t.Errorf("dimension param = %v, want %q", got, "ip")
	}
}

// TestService_AccessTokenWrongGuess_RateLimitedDecoration pins the
// token-dimension check's own refusal shape against a scripted limiter,
// the same decoration every other dimension check in this module applies:
// an exhausted budget answers ErrRateLimited with the dimension named
// "token" and the window's recovery time recorded.
func TestService_AccessTokenWrongGuess_RateLimitedDecoration(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 42 * time.Second}

	err := svc.checkAccessTokenWrongGuess(context.Background(), "some-token-hash")
	assertCode(t, err, ErrRateLimited.Code)

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", err)
	}
	if got := appErr.Params["dimension"]; got != "token" {
		t.Errorf("dimension param = %v, want %q", got, "token")
	}
	if got := appErr.Params["retry_after_seconds"]; got != 42 {
		t.Errorf("retry_after_seconds param = %v, want 42", got)
	}
}

// TestService_AccessTokenWrongGuess_StoreError_WrapsAsInternal pins the
// token-dimension check's fail-closed handling of an underlying store
// failure, mirroring the same guarantee every other check this module runs
// carries: a rate limiter that cannot answer must never be treated as
// "allow".
func TestService_AccessTokenWrongGuess_StoreError_WrapsAsInternal(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{err: errors.New("kv store unavailable")}

	err := svc.checkAccessTokenWrongGuess(context.Background(), "some-token-hash")
	assertCode(t, err, ErrInternal.Code)
}

// TestService_Access_WrongPassword_UnaffectedByExhaustedAnonymousBudget pins
// the gating decision behind authorizeAttempt's chargeTokenBudget flag: the
// per-token wrong-guess budget is the genuinely unauthenticated surface's
// own machinery (Access's own doc comment explains why), so an exhausted
// budget must never change what the host's own authenticated Access calls
// answer. The budget is exhausted here through the anonymous surface's own
// check on the share's token key -- the identical key authorizeAttempt's
// charge would spend -- and an authenticated Access wrong-password attempt
// that follows must still answer the ordinary rule-5 ErrNotAccessible, not
// the anonymous surface's ErrRateLimited: an in-process host caller is
// never refused, and never charged, by a budget whose consumption belongs
// to the surface an external attacker can actually reach.
func TestService_Access_WrongPassword_UnaffectedByExhaustedAnonymousBudget(t *testing.T) {
	svc, _ := newTestService(t, nil)
	password := "s3cret"
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1", Password: &password})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < accessPerTokenRate; i++ {
		if budgetErr := svc.checkAccessTokenWrongGuess(context.Background(), hashShareToken(created.Token)); budgetErr != nil {
			t.Fatalf("checkAccessTokenWrongGuess attempt %d: %v", i, budgetErr)
		}
	}

	wrong := "wrong"
	_, accessErr := svc.Access(testCtx(), created.Token, AccessParams{Password: &wrong})
	assertCode(t, accessErr, ErrNotAccessible.Code)
}

// TestService_Create_SubSecondWindowTail_RetryAfterRoundsUp pins the
// retry_after_seconds translation boundary at the shared denial site every
// dimension funnels through (allowRateLimit): a denial whose window still
// has a sub-second remainder -- the NORMAL tail of every exhausted window --
// must carry 1, never the 0 a truncating int(Seconds()) conversion emits
// (Retry-After: 0 means "retry immediately", inviting an immediate retry
// against a window that has not reset). A negative remainder (a degenerate
// canned decision; the real limiter's ResetAfter is always inside (0, Per])
// must carry 0, never a negative whole-second count.
func TestService_Create_SubSecondWindowTail_RetryAfterRoundsUp(t *testing.T) {
	svc, _ := newTestService(t, nil)

	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 900 * time.Millisecond}
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); err == nil {
		t.Fatal("Create succeeded against a denying limiter, want the rate-limit denial")
	} else {
		assertRetryAfterSeconds(t, err, 1)
	}

	svc.limiter = scriptedLimiter{allowed: false, resetAfter: -3 * time.Second}
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); err == nil {
		t.Fatal("Create succeeded against a denying limiter, want the rate-limit denial")
	} else {
		assertRetryAfterSeconds(t, err, 0)
	}
}

// assertRetryAfterSeconds fails t unless err is the module's rate-limit
// denial carrying the given retry_after_seconds value.
func assertRetryAfterSeconds(t *testing.T, err error, want int) {
	t.Helper()
	assertCode(t, err, ErrRateLimited.Code)
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	if got := appErr.Params["retry_after_seconds"]; got != want {
		t.Errorf("retry_after_seconds param = %v, want %d", got, want)
	}
}
