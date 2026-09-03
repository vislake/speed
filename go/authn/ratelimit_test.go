package authn

import (
	"fmt"
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
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

// TestRateGuard_CheckSMSVerify_EachDimensionLimitsIndependently mirrors the
// above for the code-verify endpoint.
func TestRateGuard_CheckSMSVerify_EachDimensionLimitsIndependently(t *testing.T) {
	t.Parallel()

	guard := newRateGuard(pkgcore.NewMemoryKVStore())
	ctx := t.Context()

	var lastErr error
	for i := 0; i < limitSMSVerifyByTarget.Rate+1; i++ {
		lastErr = guard.CheckSMSVerify(ctx, "target-verify-test", ipForIndex(i))
	}
	if !hasCode(lastErr, ErrRateLimited.Code) {
		t.Fatalf("CheckSMSVerify() after saturating the target dimension error = %v, want ErrRateLimited", lastErr)
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
