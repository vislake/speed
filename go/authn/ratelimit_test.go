package authn

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// TestRateGuard_CheckLogin_EachDimensionLimitsIndependently proves the
// account dimension and the IP dimension are two separate counters: an
// account well under its own limit is still refused once its IP dimension
// saturates, and vice versa.
func TestRateGuard_CheckLogin_EachDimensionLimitsIndependently(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	// Saturate the IP dimension using many different accounts sharing one
	// IP -- each individual account's own dimension stays far under its
	// own limit throughout.
	var lastErr error
	for i := 0; i < limitLoginByIP.Rate+1; i++ {
		lastErr = guard.CheckLogin(ctx, "account-shared-ip-test", "203.0.113.50")
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckLogin() after saturating the IP dimension error = %v, want ErrRateLimited", lastErr)
	}

	// A DIFFERENT ip, same account, must not be affected by the account
	// dimension yet (it has only been hit once by the loop above under a
	// single account key), proving the two dimensions are independent.
	if err := guard.CheckLogin(ctx, "account-unaffected", "203.0.113.51"); err != nil {
		t.Fatalf("CheckLogin() for an independent account/IP pair error = %v, want nil", err)
	}
}

// TestRateGuard_CheckLogin_AccountDimensionSaturates proves the account
// dimension alone can refuse a login, independent of IP.
func TestRateGuard_CheckLogin_AccountDimensionSaturates(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitLoginByAccount.Rate+1; i++ {
		// A fresh IP every call, so only the account dimension can be
		// the one that eventually refuses.
		lastErr = guard.CheckLogin(ctx, "account-saturate-test", ipForIndex(i))
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckLogin() after saturating the account dimension error = %v, want ErrRateLimited", lastErr)
	}
}

// TestRateGuard_RecordLoginFailure_DelayGrowsThenSaturatesAsLockout proves
// the progressive delay grows with each recorded failure and, once it
// reaches its ceiling, behaves as an unconditional lockout for the
// configured window -- the "lockout after the configured threshold"
// behavior layered on top of go/ratelimit's own plain counters.
func TestRateGuard_RecordLoginFailure_DelayGrowsThenSaturatesAsLockout(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()
	account := "account-lockout-test"

	// One failure: not yet locked (the very next check, an instant later,
	// would only be refused once time.Until(LockedUntil) is positive --
	// loginLockoutBase is 30s, so a check performed at essentially the
	// same instant IS inside that window).
	guard.RecordLoginFailure(ctx, account)
	locked1, remaining1, err := guard.loginLocked(ctx, account)
	if err != nil {
		t.Fatalf("loginLocked() error = %v", err)
	}
	if !locked1 {
		t.Fatalf("loginLocked() after one failure = false, want true (the base delay applies immediately)")
	}

	// Enough further failures to blow well past loginLockoutMax's ceiling.
	for range 10 {
		guard.RecordLoginFailure(ctx, account)
	}
	locked2, remaining2, err := guard.loginLocked(ctx, account)
	if err != nil {
		t.Fatalf("loginLocked() error = %v", err)
	}
	if !locked2 {
		t.Fatalf("loginLocked() after many failures = false, want true")
	}
	if remaining2 > loginLockoutMax {
		t.Errorf("remaining lockout = %v, want it capped at loginLockoutMax = %v", remaining2, loginLockoutMax)
	}
	if remaining2 < remaining1 {
		t.Errorf("remaining lockout SHRANK from %v to %v as failures accumulated, want it to grow", remaining1, remaining2)
	}
}

// TestRateGuard_RecordLoginSuccess_ClearsLockout proves a successful login
// resets the progressive delay, so the account is not penalized by
// failures that happened before it authenticated correctly.
func TestRateGuard_RecordLoginSuccess_ClearsLockout(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()
	account := "account-recovers-test"

	guard.RecordLoginFailure(ctx, account)
	guard.RecordLoginFailure(ctx, account)
	guard.RecordLoginSuccess(ctx, account)

	locked, _, err := guard.loginLocked(ctx, account)
	if err != nil {
		t.Fatalf("loginLocked() error = %v", err)
	}
	if locked {
		t.Errorf("loginLocked() after RecordLoginSuccess = true, want false")
	}
}

// TestRateGuard_UnrelatedAccountNotAffectedByLockout proves one account's
// lockout state is keyed independently of another's.
func TestRateGuard_UnrelatedAccountNotAffectedByLockout(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	for range 10 {
		guard.RecordLoginFailure(ctx, "account-attacked")
	}

	if err := guard.CheckLogin(ctx, "account-innocent", "203.0.113.60"); err != nil {
		t.Errorf("CheckLogin(unrelated account) error = %v, want nil", err)
	}
}

// TestRateGuard_KVStoreError_FailsClosed proves an unreachable KVStore
// refuses a login rather than defaulting to "allow" -- the explicit policy
// assertion this file's own doc comment states: ratelimit itself
// deliberately does not choose fail-open or fail-closed, so authn's own
// guard has to, and the choice here is always closed.
func TestRateGuard_KVStoreError_FailsClosed(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(testutil.FailingKVStore{})
	if err := guard.CheckLogin(t.Context(), "account-x", "203.0.113.70"); err == nil {
		t.Fatalf("CheckLogin() with an unreachable KVStore error = nil, want a refusal")
	}
}

// TestRateGuard_CheckRegister_LimitsByIPAlone proves the registration guard
// has only the one dimension there is an identifier for.
func TestRateGuard_CheckRegister_LimitsByIPAlone(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitRegisterByIP.Rate+1; i++ {
		lastErr = guard.CheckRegister(ctx, "203.0.113.80")
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckRegister() after saturating error = %v, want ErrRateLimited", lastErr)
	}

	if err := guard.CheckRegister(ctx, "203.0.113.81"); err != nil {
		t.Errorf("CheckRegister() from a different IP error = %v, want nil", err)
	}
}

// TestRateGuard_CheckSMSSend_EachDimensionLimitsIndependently mirrors
// TestRateGuard_CheckLogin_EachDimensionLimitsIndependently for the
// code-send endpoint's two dimensions.
func TestRateGuard_CheckSMSSend_EachDimensionLimitsIndependently(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitSMSSendByTarget.Rate+1; i++ {
		lastErr = guard.CheckSMSSend(ctx, "target-send-test", ipForIndex(i))
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckSMSSend() after saturating the target dimension error = %v, want ErrRateLimited", lastErr)
	}

	if err := guard.CheckSMSSend(ctx, "target-unaffected", "203.0.113.90"); err != nil {
		t.Errorf("CheckSMSSend() for an unrelated target error = %v, want nil", err)
	}
}

// TestRateGuard_CheckSMSVerifyWrongGuess_TargetDimensionSaturates mirrors
// TestRateGuard_CheckSMSSend_EachDimensionLimitsIndependently for the
// code-verify endpoint's per-target wrong-guess dimension -- the two guard
// methods (CheckSMSVerifyWrongGuess, CheckSMSVerifyIP below) replaced the
// single CheckSMSVerify this test used to exercise; see ratelimit.go's own
// doc comments for why they were split.
func TestRateGuard_CheckSMSVerifyWrongGuess_TargetDimensionSaturates(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitSMSVerifyByTarget.Rate+1; i++ {
		lastErr = guard.CheckSMSVerifyWrongGuess(ctx, "target-verify-test")
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckSMSVerifyWrongGuess() after saturating the target dimension error = %v, want ErrRateLimited", lastErr)
	}

	if err := guard.CheckSMSVerifyWrongGuess(ctx, "target-verify-unaffected"); err != nil {
		t.Errorf("CheckSMSVerifyWrongGuess() for an unrelated target error = %v, want nil", err)
	}
}

// TestRateGuard_CheckSMSVerifyIP_LimitsIndependently proves the IP
// dimension is a separate counter from the per-target wrong-guess one:
// many different targets sharing one IP saturate it on their own.
func TestRateGuard_CheckSMSVerifyIP_LimitsIndependently(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitSMSVerifyByIP.Rate+1; i++ {
		lastErr = guard.CheckSMSVerifyIP(ctx, "203.0.113.222")
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckSMSVerifyIP() after saturating the IP dimension error = %v, want ErrRateLimited", lastErr)
	}

	if err := guard.CheckSMSVerifyIP(ctx, "203.0.113.223"); err != nil {
		t.Errorf("CheckSMSVerifyIP() for a different IP error = %v, want nil", err)
	}
}

// TestRateGuard_CheckStepUp_EachDimensionLimitsIndependently mirrors the
// above for the step-up endpoint.
func TestRateGuard_CheckStepUp_EachDimensionLimitsIndependently(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitStepUpByAccount.Rate+1; i++ {
		lastErr = guard.CheckStepUp(ctx, "user-stepup-test", ipForIndex(i))
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckStepUp() after saturating the account dimension error = %v, want ErrRateLimited", lastErr)
	}
}

// TestService_Login_RateLimited_ReturnsErrRateLimited proves the guard is
// actually wired into Service.Login, end to end, rather than existing only
// as an unused collaborator.
//
// Every failed attempt here also feeds RecordLoginFailure, so the account
// may end up refused by EITHER the plain sliding window (ErrRateLimited)
// or the progressive lockout that failure recording grows (ErrAccountLocked)
// -- whichever saturates first. Both are the guard doing its job; this test
// asserts only that ONE of the two closes the door, not which.
func TestService_Login_RateLimited_ReturnsErrRateLimited(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "ratelimited@example.com", testTenantA)

	var lastErr error
	for i := 0; i < limitLoginByAccount.Rate+1; i++ {
		_, lastErr = f.svc.Login(t.Context(), LoginInput{
			Identifier: "ratelimited@example.com", Password: "definitely wrong", IP: ipForIndex(i),
		})
	}
	if !hasCode(lastErr, ErrRateLimited.Code) && !hasCode(lastErr, ErrAccountLocked.Code) {
		t.Fatalf("Login() after repeated failures error = %v, want ErrRateLimited or ErrAccountLocked", lastErr)
	}
}

// TestService_Register_RateLimited_ReturnsErrRateLimited proves the guard
// is wired into Service.Register.
func TestService_Register_RateLimited_ReturnsErrRateLimited(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	var lastErr error
	for i := 0; i < limitRegisterByIP.Rate+1; i++ {
		_, lastErr = f.svc.Register(t.Context(), RegisterInput{
			Email: "register-flood-" + ipForIndex(i) + "@example.com", Password: testPassword, IP: "203.0.113.99",
		})
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("Register() after saturating the IP dimension error = %v, want ErrRateLimited", lastErr)
	}
}

// ipForIndex returns a distinct, syntactically plausible IPv4 address per
// index, so a loop can vary "the other dimension" without colliding.
func ipForIndex(i int) string {
	return fmt.Sprintf("198.51.100.%d", i%254+1)
}

// TestLoginLockoutDelay_DoublesPerFailureThenSaturates pins the pure delay
// arithmetic the recorded lockout state derives from: the base delay for the
// first failure, doubled per failure after it, capped at loginLockoutMax.
func TestLoginLockoutDelay_DoublesPerFailureThenSaturates(t *testing.T) {
	t.Parallel()

	if got := loginLockoutDelay(1); got != loginLockoutBase {
		t.Errorf("loginLockoutDelay(1) = %v, want the base delay %v", got, loginLockoutBase)
	}
	if got := loginLockoutDelay(2); got != 2*loginLockoutBase {
		t.Errorf("loginLockoutDelay(2) = %v, want %v (doubled)", got, 2*loginLockoutBase)
	}
	if got := loginLockoutDelay(3); got != 4*loginLockoutBase {
		t.Errorf("loginLockoutDelay(3) = %v, want %v", got, 4*loginLockoutBase)
	}
	// loginLockoutMax is reached after roughly five consecutive failures
	// with the shipped constants; every failure past that point must keep
	// the ceiling, never exceed it.
	var sawCeiling bool
	for failures := 1; failures < 20; failures++ {
		if got := loginLockoutDelay(failures); got > loginLockoutMax {
			t.Fatalf("loginLockoutDelay(%d) = %v, exceeds the ceiling %v", failures, got, loginLockoutMax)
		} else if got == loginLockoutMax {
			sawCeiling = true
		}
	}
	if !sawCeiling {
		t.Errorf("loginLockoutDelay never reached the ceiling %v within 20 failures", loginLockoutMax)
	}
}

// TestRateGuard_RecordLoginFailure_ConcurrentFailures_NoneLost is the
// regression for the audit finding that RecordLoginFailure was a
// non-atomic read-modify-write over one JSON state value: five concurrent
// failures measured landing as two, and since the progressive delay doubles
// per recorded failure, the lost ones are exactly what keeps the lockout
// from escalating under a burst. The recording must be built from the
// KVStore's atomic primitives, so N concurrent failures against one account
// end with a failure count of exactly N and a lockout deadline that has
// converged on the burst.
func TestRateGuard_RecordLoginFailure_ConcurrentFailures_NoneLost(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()
	account := "account-concurrent-burst"

	const recorders = 25
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range recorders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			guard.RecordLoginFailure(ctx, account)
		}()
	}
	close(start)
	wg.Wait()

	state, err := guard.readLoginLockoutState(ctx, account)
	if err != nil {
		t.Fatalf("readLoginLockoutState() error = %v", err)
	}
	if state.Failures != recorders {
		t.Errorf("Failures = %d after %d concurrent recorded failures, want exactly %d -- concurrent failures must not be lost",
			state.Failures, recorders, recorders)
	}

	locked, remaining, err := guard.loginLocked(ctx, account)
	if err != nil {
		t.Fatalf("loginLocked() error = %v", err)
	}
	if !locked {
		t.Error("loginLocked() = false after the burst, want true -- the deadline must have converged on the burst's lockout")
	}
	if remaining > loginLockoutMax {
		t.Errorf("remaining lockout = %v, want it capped at loginLockoutMax = %v", remaining, loginLockoutMax)
	}
}

// TestRateGuard_RecordLoginSuccess_ClearsBothStateKeys proves a successful
// login removes every piece of the split lockout state: if it left the
// deadline key behind, a stale deadline from a cleared run could keep
// locking an account whose failures were forgiven.
func TestRateGuard_RecordLoginSuccess_ClearsBothStateKeys(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()
	account := "account-clears-both-keys"

	for range 5 {
		guard.RecordLoginFailure(ctx, account)
	}
	guard.RecordLoginSuccess(ctx, account)

	state, err := guard.readLoginLockoutState(ctx, account)
	if err != nil {
		t.Fatalf("readLoginLockoutState() error = %v", err)
	}
	if state.Failures != 0 || !state.LockedUntil.IsZero() {
		t.Errorf("state after RecordLoginSuccess = %+v, want a clean slate", state)
	}
	locked, _, err := guard.loginLocked(ctx, account)
	if err != nil {
		t.Fatalf("loginLocked() error = %v", err)
	}
	if locked {
		t.Error("loginLocked() after RecordLoginSuccess = true, want false")
	}
}

// cannedLimiter is a ratelimit.Limiter whose every Allow answers one fixed
// decision, letting a rateGuard test drive a denial (or an allow) carrying
// a caller-chosen ResetAfter without depending on wall-clock window
// timing.
type cannedLimiter struct {
	decision ratelimit.Decision
}

func (l cannedLimiter) Allow(context.Context, string, ratelimit.Limit) (ratelimit.Decision, error) {
	return l.decision, nil
}

// TestRateGuard_Allow_SubSecondResetAfter_RetryAfterRoundsUp pins the
// Retry-After translation boundary at the sliding-window denial site --
// the NORMAL one, reached at the tail of every exhausted window: a denial
// whose window still has a sub-second remainder must carry 1, never the 0
// a truncating int(Seconds()) conversion would emit. Retry-After: 0 (RFC
// 9110, §10.2.3) is legal but means "retry immediately", and an immediate
// retry against a window that has not reset is refused again -- a mild
// amplification the header exists to prevent. A 900ms remainder answers 1.
func TestRateGuard_Allow_SubSecondResetAfter_RetryAfterRoundsUp(t *testing.T) {
	t.Parallel()

	guard := &rateGuard{
		limiter: cannedLimiter{decision: ratelimit.Decision{Allowed: false, ResetAfter: 900 * time.Millisecond}},
		kv:      pkgcore.NewMemoryKVStore(),
	}
	err := guard.allow(t.Context(), "authn:test:subsecond-window", limitLoginByAccount)
	if !hasCode(err, ErrRateLimited.Code) {
		t.Fatalf("allow() error = %v, want ErrRateLimited", err)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	if got := appErr.Params["retry_after_seconds"]; got != 1 {
		t.Errorf("retry_after_seconds param = %v, want 1 -- a 900ms remainder must round up, not truncate to 0", got)
	}
}

// TestRateGuard_CheckLogin_LockoutTail_RetryAfterRoundsUp pins the same
// boundary at the account-lockout denial site (the edge case: only the
// last instant of a lockout window carries a sub-second remainder). An
// account whose lockout still has 900ms to run answers ErrAccountLocked
// with 1, not 0 -- a 0 would invite an immediate retry while the lockout
// still holds. The deadline key is seeded directly so the test never
// waits on the real lockout window.
func TestRateGuard_CheckLogin_LockoutTail_RetryAfterRoundsUp(t *testing.T) {
	kv := pkgcore.NewMemoryKVStore()
	guard := &rateGuard{
		// The limiter never denies: the seeded lockout alone must refuse
		// the login, so the assertion isolates the lockout path.
		limiter: cannedLimiter{decision: ratelimit.Decision{Allowed: true}},
		kv:      kv,
	}
	account := "account-lockout-tail-test"
	_, deadlineKey := loginLockoutKeys(account)
	deadline := time.Now().Add(900 * time.Millisecond).UnixMicro()
	if err := kv.Set(t.Context(), deadlineKey, []byte(strconv.FormatInt(deadline, 10)), loginLockoutStateTTL); err != nil {
		t.Fatalf("seed the lockout deadline: %v", err)
	}

	err := guard.CheckLogin(t.Context(), account, "203.0.113.77")
	if !hasCode(err, ErrAccountLocked.Code) {
		t.Fatalf("CheckLogin() inside a seeded lockout error = %v, want ErrAccountLocked", err)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	if got := appErr.Params["retry_after_seconds"]; got != 1 {
		t.Errorf("retry_after_seconds param = %v, want 1 -- a 900ms lockout remainder must round up, not truncate to 0", got)
	}
}
