package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/authn/internal/totp"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// enrollAndConfirmTOTP is the common enroll-then-confirm sequence every MFA
// test needs before it can exercise step-up or recovery codes.
func enrollAndConfirmTOTP(t *testing.T, f *serviceFixture, userID string) (secret string, recoveryCodes []string) {
	t.Helper()

	result, err := f.svc.EnrollTOTP(t.Context(), userID)
	if err != nil {
		t.Fatalf("EnrollTOTP() error = %v", err)
	}
	if result.Secret == "" || result.ProvisioningURI == "" {
		t.Fatalf("EnrollTOTP() returned an incomplete result: %+v", result)
	}

	code, err := totp.Code(result.Secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	codes, err := f.svc.ConfirmTOTP(t.Context(), userID, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP() error = %v", err)
	}
	if len(codes) != recoveryCodeCount {
		t.Fatalf("ConfirmTOTP() returned %d recovery codes, want %d", len(codes), recoveryCodeCount)
	}
	return result.Secret, codes
}

// TestEnrollTOTP_ReplacesAnyExistingFactor proves enrolling twice replaces
// the pending factor rather than accumulating a second row.
func TestEnrollTOTP_ReplacesAnyExistingFactor(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-replace@example.com", testTenantA)

	first, err := f.svc.EnrollTOTP(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("first EnrollTOTP() error = %v", err)
	}
	second, err := f.svc.EnrollTOTP(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("second EnrollTOTP() error = %v", err)
	}
	if first.Secret == second.Secret {
		t.Errorf("second EnrollTOTP() reused the first secret, want a fresh one")
	}

	// Confirming with a code for the FIRST (abandoned) secret must fail:
	// only the second, current pending factor can be confirmed.
	staleCode, err := totp.Code(first.Secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, staleCode); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("ConfirmTOTP(stale secret's code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestConfirmTOTP_WrongCode_Refused proves confirmation requires a real
// code from the enrolled secret, not any six digits.
func TestConfirmTOTP_WrongCode_Refused(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-wrong@example.com", testTenantA)

	if _, err := f.svc.EnrollTOTP(t.Context(), user.ID); err != nil {
		t.Fatalf("EnrollTOTP() error = %v", err)
	}
	if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, "000000"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("ConfirmTOTP(wrong code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestConfirmTOTP_WithoutEnrolling_Refused proves confirming with nothing
// enrolled is refused, not a panic on a nil factor.
func TestConfirmTOTP_WithoutEnrolling_Refused(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-none@example.com", testTenantA)

	if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, "123456"); !hasCode(err, ErrMFANotEnrolled.Code) {
		t.Errorf("ConfirmTOTP(nothing enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

// TestConfirmTOTP_AlreadyActive_Refused proves confirming an already-active
// factor a second time is refused rather than silently re-confirming.
func TestConfirmTOTP_AlreadyActive_Refused(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-active@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)

	code, err := totp.Code(secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, code); !hasCode(err, ErrMFAAlreadyEnrolled.Code) {
		t.Errorf("ConfirmTOTP(already active) error = %v, want ErrMFAAlreadyEnrolled", err)
	}
}

// TestConfirmTOTP_PublishesEventAndAudit proves enrollment confirmation is
// announced, matching the same security-notice pattern as identity binding.
func TestConfirmTOTP_PublishesEventAndAudit(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-event@example.com", testTenantA)
	enrollAndConfirmTOTP(t, f, user.ID)

	if n := f.events.Count(EventMFAEnrolled); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventMFAEnrolled)
	}
}

// TestRecoveryCodes_AreStoredHashed proves the raw database column never
// holds the plaintext recovery codes handed back to the caller.
func TestRecoveryCodes_AreStoredHashed(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-hashed@example.com", testTenantA)
	_, codes := enrollAndConfirmTOTP(t, f, user.ID)

	var rows []UserRecoveryCode
	if err := f.db.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil {
		t.Fatalf("query user_recovery_codes: %v", err)
	}
	if len(rows) != recoveryCodeCount {
		t.Fatalf("stored %d recovery codes, want %d", len(rows), recoveryCodeCount)
	}
	for _, row := range rows {
		for _, plain := range codes {
			if row.CodeHash == plain {
				t.Fatalf("stored CodeHash equals a plaintext recovery code: %q", plain)
			}
		}
	}
}

// TestRecoveryCode_WorksOnceThenFails proves a recovery code satisfies
// exactly one step-up and is refused on a second attempt.
func TestRecoveryCode_WorksOnceThenFails(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-recovery@example.com", testTenantA)
	_, codes := enrollAndConfirmTOTP(t, f, user.ID)

	principal := loginPrincipal(t, f, user, testTenantA)

	if _, err := f.svc.VerifyStepUp(t.Context(), principal, codes[0], "203.0.113.10"); err != nil {
		t.Fatalf("first VerifyStepUp(recovery code) error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, codes[0], "203.0.113.10"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("second VerifyStepUp(same recovery code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestVerifyStepUp_TOTPCode_EnrichesAMR proves a successful TOTP step-up
// mints an access token whose AMR gained "mfa:totp" without changing the
// refresh token.
func TestVerifyStepUp_TOTPCode_EnrichesAMR(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-stepup@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)

	principal := loginPrincipal(t, f, user, testTenantA)
	if containsString(principal.AMR, MethodMFATOTP) {
		t.Fatalf("a fresh password login already carries %q, want it absent before step-up", MethodMFATOTP)
	}

	// ConfirmTOTP already consumed the CURRENT time step's code as its own
	// replay guard (see UserMFAFactor.LastUsedStep's doc comment), so this
	// step-up generates the code for the NEXT step -- still accepted
	// thanks to totpSkewSteps' tolerance, and a genuinely different code
	// from the confirmation one.
	code, err := totp.Code(secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	pair, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.11")
	if err != nil {
		t.Fatalf("VerifyStepUp() error = %v", err)
	}
	if pair.RefreshToken != "" {
		t.Errorf("VerifyStepUp() minted a refresh token %q, want none (it must reuse the existing one)", pair.RefreshToken)
	}
	if !containsString(pair.Principal.AMR, MethodMFATOTP) {
		t.Errorf("VerifyStepUp() Principal.AMR = %v, want it to contain %q", pair.Principal.AMR, MethodMFATOTP)
	}
	if !containsString(pair.Principal.AMR, MethodPassword) {
		t.Errorf("VerifyStepUp() Principal.AMR = %v, want it to still contain the original %q", pair.Principal.AMR, MethodPassword)
	}
}

// TestVerifyStepUp_TOTPCode_CannotBeReplayed proves a TOTP code accepted
// once by VerifyStepUp cannot immediately be reused for a second step-up.
func TestVerifyStepUp_TOTPCode_CannotBeReplayed(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-replay@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	// See TestVerifyStepUp_TOTPCode_EnrichesAMR's comment: the confirmation
	// step already consumed the current time step's code.
	code, err := totp.Code(secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.12"); err != nil {
		t.Fatalf("first VerifyStepUp() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.12"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("second VerifyStepUp(same code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestVerifyStepUp_RefusesWithoutMFAEnrolled proves a session with no
// second factor to prove cannot satisfy step-up at all.
func TestVerifyStepUp_RefusesWithoutMFAEnrolled(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-absent@example.com", testTenantA)
	principal := loginPrincipal(t, f, user, testTenantA)

	if _, err := f.svc.VerifyStepUp(t.Context(), principal, "123456", "203.0.113.13"); !hasCode(err, ErrMFANotEnrolled.Code) {
		t.Errorf("VerifyStepUp(no factor enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

// TestRequireStepUp_RefusesSessionWithoutSecondFactor proves the middleware
// gates a sensitive action on the CURRENT token's amr, not merely on
// whether the account has MFA enrolled at all.
func TestRequireStepUp_RefusesSessionWithoutSecondFactor(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-gate@example.com", testTenantA)
	enrollAndConfirmTOTP(t, f, user.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	var out observed
	handler := RequireStepUp(observingHandler(&out))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/change-password", nil).
		WithContext(WithPrincipal(t.Context(), principal))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if out.called {
		t.Errorf("RequireStepUp let the request through without a completed step-up")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if got := decodeErrorBody(t, rec).Code; got != ErrStepUpRequired.Code {
		t.Errorf("error code = %q, want %q", got, ErrStepUpRequired.Code)
	}
}

// TestRequireStepUp_AllowsSessionWithSecondFactor proves the middleware
// admits a request whose Principal.AMR already carries a second factor
// (the shape VerifyStepUp's freshly minted token has).
func TestRequireStepUp_AllowsSessionWithSecondFactor(t *testing.T) {
	t.Parallel()

	principal := Principal{UserID: "u1", TenantID: testTenantA, SessionID: "s1", AMR: []string{MethodPassword, MethodMFATOTP}}
	var out observed
	handler := RequireStepUp(observingHandler(&out))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/change-password", nil).
		WithContext(WithPrincipal(t.Context(), principal))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !out.called {
		t.Errorf("RequireStepUp refused a request whose amr already carries a second factor")
	}
}

// TestRequireStepUp_RefusesUnauthenticatedRequest proves the middleware
// refuses a request with no Principal at all, rather than treating a
// missing amr the same as an absent one.
func TestRequireStepUp_RefusesUnauthenticatedRequest(t *testing.T) {
	t.Parallel()

	var out observed
	handler := RequireStepUp(observingHandler(&out))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/authn/change-password", nil))

	if out.called {
		t.Errorf("RequireStepUp let an unauthenticated request through")
	}
	if got := decodeErrorBody(t, rec).Code; got != ErrAuthenticationRequired.Code {
		t.Errorf("error code = %q, want %q", got, ErrAuthenticationRequired.Code)
	}
}

// TestRegenerateRecoveryCodes_InvalidatesPreviousBatch proves regenerating
// replaces the whole set: an old code from before regeneration must stop
// working.
func TestRegenerateRecoveryCodes_InvalidatesPreviousBatch(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-regen@example.com", testTenantA)
	_, oldCodes := enrollAndConfirmTOTP(t, f, user.ID)

	newCodes, err := f.svc.RegenerateRecoveryCodes(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("RegenerateRecoveryCodes() error = %v", err)
	}
	if len(newCodes) != recoveryCodeCount {
		t.Fatalf("RegenerateRecoveryCodes() returned %d codes, want %d", len(newCodes), recoveryCodeCount)
	}
	if newCodes[0] == oldCodes[0] {
		t.Fatalf("RegenerateRecoveryCodes() reused an old code")
	}

	principal := loginPrincipal(t, f, user, testTenantA)
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, oldCodes[0], "203.0.113.14"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("VerifyStepUp(pre-regeneration code) error = %v, want ErrMFAInvalidCode", err)
	}
	if n := f.events.Count(EventMFARecoveryCodesRegenerated); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventMFARecoveryCodesRegenerated)
	}
}

// TestRegenerateRecoveryCodes_WithoutActiveFactor_Refused proves recovery
// codes cannot be minted for a factor that was never confirmed.
func TestRegenerateRecoveryCodes_WithoutActiveFactor_Refused(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-regen-none@example.com", testTenantA)

	if _, err := f.svc.RegenerateRecoveryCodes(t.Context(), user.ID); !hasCode(err, ErrMFANotEnrolled.Code) {
		t.Errorf("RegenerateRecoveryCodes(nothing enrolled) error = %v, want ErrMFANotEnrolled", err)
	}
}

// TestMFAModels_AreNotTenantScoped is the mandatory isolation assertion for
// this round's two identity-domain tables: MFA belongs to the person, not
// to a tenant they act inside.
func TestMFAModels_AreNotTenantScoped(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	t.Run("UserMFAFactor", func(t *testing.T) {
		tenancytest.AssertNotTenantScoped(t, db, UserMFAFactor{},
			func(db *gorm.DB) error {
				return db.Create(&UserMFAFactor{
					ID: newID(), UserID: newID(), Type: MFATypeTOTP,
					Status: MFAFactorStatusPending, CreatedAt: now,
				}).Error
			},
			countOf[UserMFAFactor],
		)
	})

	t.Run("UserRecoveryCode", func(t *testing.T) {
		tenancytest.AssertNotTenantScoped(t, db, UserRecoveryCode{},
			func(db *gorm.DB) error {
				return db.Create(&UserRecoveryCode{
					ID: newID(), UserID: newID(), CodeHash: "x", CreatedAt: now,
				}).Error
			},
			countOf[UserRecoveryCode],
		)
	})
}

// TestIsTOTPShaped pins the shape dispatch VerifyStepUp uses to tell a TOTP
// code from a recovery code.
func TestIsTOTPShaped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code string
		want bool
	}{
		{"123456", true},
		{"000000", true},
		{"12345", false},
		{"1234567", false},
		{"K3QM-7XHP", false},
		{"", false},
		{"12a456", false},
	}
	for _, tc := range cases {
		if got := isTOTPShaped(tc.code); got != tc.want {
			t.Errorf("isTOTPShaped(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

// loginPrincipal signs user in with a password and returns the resulting
// Principal, for tests that need an authenticated session to step up from.
func loginPrincipal(t *testing.T, f *serviceFixture, user *User, tenant pkgcore.TenantID) Principal {
	t.Helper()
	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: user.Email, Password: testPassword, TenantID: tenant, IP: "203.0.113.100",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	return pair.Principal
}
