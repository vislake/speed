package authn

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

	result, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: userID})
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

	first, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
	if err != nil {
		t.Fatalf("first EnrollTOTP() error = %v", err)
	}
	second, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
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

// TestEnrollTOTP_ReplacingActiveFactor_RequiresStepUp proves a bare,
// unelevated session cannot replace an already ACTIVE TOTP factor: without
// this check, a stolen access token could silently seize an account's
// second factor by deleting it and enrolling an attacker-known secret in
// its place -- replacing an existing factor is a "changing MFA settings"
// operation.
func TestEnrollTOTP_ReplacingActiveFactor_RequiresStepUp(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-hijack@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)

	// A bare session -- no completed step-up in its AMR, the shape a
	// stolen access token would have -- must be refused.
	if _, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID}); !hasCode(err, ErrStepUpRequired.Code) {
		t.Fatalf("EnrollTOTP(no step-up, active factor exists) error = %v, want ErrStepUpRequired", err)
	}

	// The refused attempt must leave the ORIGINAL factor intact -- proven
	// by successfully stepping up with the original secret's code, exactly
	// as TestVerifyStepUp_TOTPCode_EnrichesAMR proves a fresh factor
	// works. ConfirmTOTP already consumed the current time step's code, so
	// this uses the next step, like that test does.
	principal := loginPrincipal(t, f, user, testTenantA)
	code, err := totp.Code(secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, stepUpErr := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.15"); stepUpErr != nil {
		t.Fatalf("VerifyStepUp(original secret after refused enroll) error = %v, want success", stepUpErr)
	}

	// A session whose AMR already carries a completed second-factor
	// step-up IS allowed to replace the factor.
	elevated := Principal{UserID: user.ID, AMR: []string{MethodPassword, MethodMFATOTP}}
	result, err := f.svc.EnrollTOTP(t.Context(), elevated)
	if err != nil {
		t.Fatalf("EnrollTOTP(with step-up, active factor exists) error = %v, want success", err)
	}
	if result.Secret == secret {
		t.Errorf("EnrollTOTP(with step-up) reused the original secret, want a fresh one")
	}
}

// TestEnrollTOTP_AbandonedReplacement_OldFactorStaysFunctional pins the
// two-phase replacement shape: EnrollTOTP must not delete an existing
// ACTIVE factor at enroll time, before the replacement was ever confirmed.
// A step-up-gated replacement wizard that is started and then cancelled or
// abandoned (account-ui's MfaSection.tsx closeWizard, a pure local reset
// with no server call at all) must leave the account with its working
// second factor and working recovery codes intact -- the wizard's own
// replacingNotice copy says the replacement only takes effect on confirm,
// so nothing less may happen.
//
// The assertions prove the abandonment is harmless: VerifyStepUp with the
// original secret and with an original recovery code both still succeed,
// and RegenerateRecoveryCodes keeps working against the still-active
// original factor -- an abandoned replacement leaves the original factor
// and its recovery codes exactly as they were.
func TestEnrollTOTP_AbandonedReplacement_OldFactorStaysFunctional(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-abandon@example.com", testTenantA)
	originalSecret, originalCodes := enrollAndConfirmTOTP(t, f, user.ID)

	// Start a replacement enrollment with a completed step-up -- exactly
	// the state MfaSection.tsx's startEnroll(true) reaches after its own
	// 403-then-step-up sequence -- then abandon it: no ConfirmTOTP call
	// ever happens, mirroring the wizard simply being closed or navigated
	// away from.
	elevated := Principal{UserID: user.ID, AMR: []string{MethodPassword, MethodMFATOTP}}
	if _, err := f.svc.EnrollTOTP(t.Context(), elevated); err != nil {
		t.Fatalf("EnrollTOTP(replacement) error = %v", err)
	}

	principal := loginPrincipal(t, f, user, testTenantA)

	// The ORIGINAL factor must still satisfy a step-up.
	code, err := totp.Code(originalSecret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.20"); err != nil {
		t.Fatalf("VerifyStepUp(original secret after abandoned replacement) error = %v, want success", err)
	}

	// An ORIGINAL recovery code must still work.
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, originalCodes[1], "203.0.113.20"); err != nil {
		t.Fatalf("VerifyStepUp(original recovery code after abandoned replacement) error = %v, want success", err)
	}

	// The specific dead end the two-phase shape guards against:
	// regenerating recovery codes must keep working against the
	// still-active original factor, not answer ErrMFANotEnrolled.
	if _, err := f.svc.RegenerateRecoveryCodes(t.Context(), user.ID); err != nil {
		t.Fatalf("RegenerateRecoveryCodes(after abandoned replacement) error = %v, want success", err)
	}
}

// TestConfirmTOTP_Replacement_RetiresOldFactorAndCodes proves the OTHER
// half of the two-phase replacement above: a replacement enrollment that IS
// actually confirmed still retires the old factor and its recovery codes,
// exactly as the replacingNotice copy promises -- the replacement takes
// effect, just deferred from enroll time to confirm time.
func TestConfirmTOTP_Replacement_RetiresOldFactorAndCodes(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-replace-confirm@example.com", testTenantA)
	originalSecret, originalCodes := enrollAndConfirmTOTP(t, f, user.ID)

	elevated := Principal{UserID: user.ID, AMR: []string{MethodPassword, MethodMFATOTP}}
	result, err := f.svc.EnrollTOTP(t.Context(), elevated)
	if err != nil {
		t.Fatalf("EnrollTOTP(replacement) error = %v", err)
	}

	code, err := totp.Code(result.Secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	newCodes, err := f.svc.ConfirmTOTP(t.Context(), user.ID, code)
	if err != nil {
		t.Fatalf("ConfirmTOTP(replacement) error = %v", err)
	}
	if len(newCodes) != recoveryCodeCount {
		t.Fatalf("ConfirmTOTP(replacement) returned %d recovery codes, want %d", len(newCodes), recoveryCodeCount)
	}

	principal := loginPrincipal(t, f, user, testTenantA)

	// The OLD secret must no longer satisfy a step-up: it was retired the
	// moment the NEW factor was confirmed.
	oldCode, err := totp.Code(originalSecret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, oldCode, "203.0.113.21"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("VerifyStepUp(old secret after confirmed replacement) error = %v, want ErrMFAInvalidCode", err)
	}

	// The OLD recovery codes must no longer work either -- the confirmed
	// replacement's own regenerateRecoveryCodesLocked call discarded them.
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, originalCodes[0], "203.0.113.21"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("VerifyStepUp(old recovery code after confirmed replacement) error = %v, want ErrMFAInvalidCode", err)
	}

	// Exactly one active row for this user+type must remain -- the
	// partial unique index's own invariant, proven directly rather than
	// only through behaviour.
	var count int64
	if err := f.db.Model(&UserMFAFactor{}).
		Where("user_id = ? AND type = ? AND status = ?", user.ID, MFATypeTOTP, MFAFactorStatusActive).
		Count(&count).Error; err != nil {
		t.Fatalf("count active factors: %v", err)
	}
	if count != 1 {
		t.Errorf("active user_mfa_factors rows for %s = %d, want 1", user.ID, count)
	}
}

// TestConfirmTOTP_WrongCode_Refused proves confirmation requires a real
// code from the enrolled secret, not any six digits.
func TestConfirmTOTP_WrongCode_Refused(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-wrong@example.com", testTenantA)

	if _, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID}); err != nil {
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

// TestRecoveryCode_ConsumedCode_AnswersUsedNotInvalid pins the spent-code
// classification: a recovery code satisfies exactly one step-up, and the
// second presentation of the SAME code -- the shape a superseded-race loser
// or a double submit leaves -- must still be REFUSED (the code is
// single-use; the refusal is unchanged), but it must answer
// authn.mfa_code_used rather than the authn.mfa_invalid_code a never-
// issued code answers: the code was real and is gone, which is the truth
// a step-up surface needs to stop telling its user the code is wrong and
// retryable. The code strings are literals rather than sentinel .Code
// references, so the test does not depend on the very sentinel it pins.
func TestRecoveryCode_ConsumedCode_AnswersUsedNotInvalid(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-recovery@example.com", testTenantA)
	_, codes := enrollAndConfirmTOTP(t, f, user.ID)

	principal := loginPrincipal(t, f, user, testTenantA)

	if _, err := f.svc.VerifyStepUp(t.Context(), principal, codes[0], "203.0.113.10"); err != nil {
		t.Fatalf("first VerifyStepUp(recovery code) error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, codes[0], "203.0.113.10"); !hasCode(err, "authn.mfa_code_used") {
		t.Errorf("second VerifyStepUp(same recovery code) error = %v, want code authn.mfa_code_used (refused, but honestly: consumed, not invalid)", err)
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

// TestVerifyStepUp_ConsumedTOTPCode_AnswersUsedNotInvalid pins the
// spent-code classification on the TOTP path: a TOTP code accepted once by
// VerifyStepUp -- the winner of a step-up, or the loser of a superseded
// race whose code still verified server-side -- cannot immediately be
// reused, and the re-submission of that SAME code must still be REFUSED
// (the replay guard, step <= LastUsedStep, is the security control and is
// unchanged), but it must answer authn.mfa_code_used rather than the
// authn.mfa_invalid_code a never-valid code answers: the code is spent,
// and the caller must be told so instead of being asked to retry a code
// that can never verify again. The code strings are literals rather than
// sentinel .Code references, so the test does not depend on the very
// sentinel it pins.
func TestVerifyStepUp_ConsumedTOTPCode_AnswersUsedNotInvalid(t *testing.T) {
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
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.12"); !hasCode(err, "authn.mfa_code_used") {
		t.Errorf("second VerifyStepUp(same code) error = %v, want code authn.mfa_code_used (refused, but honestly: consumed, not invalid)", err)
	}
}

// TestVerifyStepUp_WrongTOTPCode_AnswersInvalidCode is regression (b) of
// the honest-step-up-error round, the pin that keeps the split one-way: a
// code the factor's secret never produces must keep answering
// authn.mfa_invalid_code -- the spent-code distinction must never leak to
// a guesser, who can only ever present codes that fail the actual TOTP
// check.
func TestVerifyStepUp_WrongTOTPCode_AnswersInvalidCode(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-wrong@example.com", testTenantA)
	enrollAndConfirmTOTP(t, f, user.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	if _, err := f.svc.VerifyStepUp(t.Context(), principal, "000000", "203.0.113.12"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("VerifyStepUp(wrong code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestVerifyStepUp_AnotherUsersRecoveryCode_AnswersInvalidCode is the
// recovery half of regression (b): a recovery code issued to ANOTHER user
// is a code no row of this user's matches, so it must keep answering
// authn.mfa_invalid_code -- the consumed-versus-never-issued split must
// not turn another user's real code into a distinguishable answer here.
func TestVerifyStepUp_AnotherUsersRecoveryCode_AnswersInvalidCode(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-cross@example.com", testTenantA)
	other := f.registerUser(t, "mfa-cross-other@example.com", testTenantA)
	_, otherCodes := enrollAndConfirmTOTP(t, f, other.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	if _, err := f.svc.VerifyStepUp(t.Context(), principal, otherCodes[0], "203.0.113.12"); !hasCode(err, ErrMFAInvalidCode.Code) {
		t.Errorf("VerifyStepUp(another user's recovery code) error = %v, want ErrMFAInvalidCode", err)
	}
}

// TestVerifyStepUp_RefusesAnExpiredSession pins the expiry half of
// VerifyStepUp's session check: a session past its own configured TTL --
// whose Status row nothing here ever proactively flips away from active --
// must not complete a step-up challenge and mint a fresh access token,
// whatever the caller's currently-held access token still says.
func TestVerifyStepUp_RefusesAnExpiredSession(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "stepup-after-expiry@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	testutil.ExpireSession(t, f.db, principal.SessionID, f.clock.Now().Add(-time.Hour))

	// See TestVerifyStepUp_TOTPCode_EnrichesAMR's comment: the confirmation
	// step already consumed the current time step's code.
	code, err := totp.Code(secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.13"); !hasCode(err, ErrSessionRevoked.Code) {
		t.Fatalf("VerifyStepUp(expired session) error = %v, want code %q", err, ErrSessionRevoked.Code)
	}
}

// TestVerifyStepUp_StillValidSessionSucceeds guards against
// over-refusing: a session that has not yet reached its own ExpiresAt must
// keep completing step-up.
func TestVerifyStepUp_StillValidSessionSucceeds(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "stepup-still-valid@example.com", testTenantA)
	secret, _ := enrollAndConfirmTOTP(t, f, user.ID)
	principal := loginPrincipal(t, f, user, testTenantA)

	code, err := totp.Code(secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, code, "203.0.113.14"); err != nil {
		t.Fatalf("VerifyStepUp(still-valid session) error = %v, want success", err)
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
// the module's two identity-domain MFA tables: MFA belongs to the person,
// not to a tenant they act inside.
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

// TestMFAFactorRepository_Confirm_SecondConfirmOfAnActivatedFactorLoses
// pins the deterministic half of the confirm race: the loser of a confirm
// race is a Confirm whose elevation UPDATE matches no row -- the factor is
// no longer pending, because a concurrent confirm already activated it. The
// repository must report that loss, so the service layer can refuse BEFORE
// it regenerates the recovery-code batch the winner just displayed. A
// second confirm of the same pending id must never look like a win.
func TestMFAFactorRepository_Confirm_SecondConfirmOfAnActivatedFactorLoses(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-confirm-loser@example.com", testTenantA)

	result, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
	if err != nil {
		t.Fatalf("EnrollTOTP() error = %v", err)
	}
	pending, err := f.svc.mfaFactors.FindPendingByUserAndType(t.Context(), user.ID, MFATypeTOTP)
	if err != nil {
		t.Fatalf("FindPendingByUserAndType() error = %v", err)
	}
	code, err := totp.Code(result.Secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}
	ok, step := totp.Validate(result.Secret, code, totpSkewSteps)
	if !ok {
		t.Fatal("the freshly computed code does not validate")
	}

	// The winner: this Confirm is the one whose elevation UPDATE matches
	// the pending row.
	won, err := f.svc.mfaFactors.Confirm(t.Context(), user.ID, MFATypeTOTP, pending.ID, f.svc.now(), step)
	if err != nil || !won {
		t.Fatalf("first Confirm() = (%v, %v), want (true, nil)", won, err)
	}

	// The loser: the identical Confirm again -- exactly what a concurrent
	// ConfirmTOTP whose read of the pending row predates the winner's
	// commit executes. Its UPDATE matches no row, and it must report the
	// loss rather than a silent nil.
	won, err = f.svc.mfaFactors.Confirm(t.Context(), user.ID, MFATypeTOTP, pending.ID, f.svc.now(), step)
	if err != nil {
		t.Fatalf("second Confirm() error = %v, want a clean (false, nil)", err)
	}
	if won {
		t.Fatal("second Confirm() = true: a confirm whose elevation matched no row reported victory; the caller would regenerate the winner's recovery-code batch and announce a second enrollment")
	}
}

// TestConfirmTOTP_ConcurrentConfirmsOfOnePendingFactor_HaveOneWinner pins
// the service-level confirm race: several simultaneous ConfirmTOTP calls
// with the same valid code race over one pending factor. Exactly one may
// win the elevation, regenerate the recovery-code batch and announce the
// enrollment. Every loser must be refused with the already-active answer
// BEFORE it touches the recovery-code batch: a loser whose pending-factor
// read landed before the winner's commit would otherwise run Confirm to a
// no-op and then regenerate the batch the winner had just displayed,
// replacing the codes the winner's screen was showing, and announce a
// second enrollment over one code.
//
// Deliberately not t.Parallel() and run over several fresh fixtures: the
// losers' reads only beat the winner's commit under genuine concurrency,
// and a single round of a wall-clock race can come out the safe way even
// on a shape that lets losers regenerate. The loop exists because every
// round must be deterministic -- the database arbitrates exactly one
// winner.
func TestConfirmTOTP_ConcurrentConfirmsOfOnePendingFactor_HaveOneWinner(t *testing.T) {
	const (
		rounds = 5
		racers = 8
	)
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newServiceFixture(t)
			user := f.registerUser(t, "mfa-race@example.com", testTenantA)
			result, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
			if err != nil {
				t.Fatalf("EnrollTOTP() error = %v", err)
			}
			code, err := totp.Code(result.Secret, time.Now())
			if err != nil {
				t.Fatalf("totp.Code() error = %v", err)
			}

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				success  int
				refused  int
				unwanted []error
			)
			wg.Add(racers)
			for range racers {
				go func() {
					defer wg.Done()
					_, err := f.svc.ConfirmTOTP(t.Context(), user.ID, code)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil:
						success++
					case hasCode(err, ErrMFAAlreadyEnrolled.Code), hasCode(err, ErrMFANotEnrolled.Code):
						refused++
					default:
						unwanted = append(unwanted, err)
					}
				}()
			}
			wg.Wait()

			if len(unwanted) != 0 {
				t.Fatalf("losers answered unexpected errors: %v", unwanted)
			}
			if success != 1 {
				t.Fatalf("%d concurrent confirms succeeded, want exactly 1 (a losing confirm must be refused, not regenerate the winner's recovery-code batch)", success)
			}
			if refused != racers-1 {
				t.Fatalf("%d losers were refused, want %d", refused, racers-1)
			}
			if n := f.events.Count(EventMFAEnrolled); n != 1 {
				t.Errorf("%d %s events published, want exactly 1: a losing confirm announced an enrollment", n, EventMFAEnrolled)
			}
			var rows []UserRecoveryCode
			if err := f.db.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil {
				t.Fatalf("query user_recovery_codes: %v", err)
			}
			if len(rows) != recoveryCodeCount {
				t.Errorf("stored %d recovery codes, want %d (a losing confirm regenerated the batch)", len(rows), recoveryCodeCount)
			}
		})
	}
}

// failRecoveryCodeCreatesWhile makes every user_recovery_codes insert on db
// fail with an injected error while fail is true, by way of a GORM create
// callback -- the deterministic stand-in for a batch write that dies
// halfway through a regeneration. The hook is scoped by table name, so the
// MFA-factor writes around it pass untouched, and is removed on cleanup.
func failRecoveryCodeCreatesWhile(t *testing.T, db *gorm.DB, fail *bool) {
	t.Helper()
	const hookName = "authn_test_fail_recovery_code_create"
	if err := db.Callback().Create().Before("gorm:create").Register(hookName, func(tx *gorm.DB) {
		if *fail && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "user_recovery_codes" {
			tx.AddError(errors.New("injected: recovery-code batch write refused"))
		}
	}); err != nil {
		t.Fatalf("register the recovery-code create failure hook: %v", err)
	}
	t.Cleanup(func() {
		*fail = false
		_ = db.Callback().Create().Remove(hookName)
	})
}

// TestConfirmTOTP_RegenerationFailure_KeepsThePreviousBatch pins the
// confirm side of the batch replacement: ConfirmTOTP activates the pending
// factor (atomically retiring the one it replaces) and then regenerates the
// recovery-code batch. A regeneration that failed between deleting the old
// batch and inserting the new one would leave the account on a fresh ACTIVE
// factor with ZERO backup codes -- nothing left to get back in with when
// the authenticator device is lost. The replacement is one transaction, so
// a refused insert rolls the deletion back and the previous batch survives.
func TestConfirmTOTP_RegenerationFailure_KeepsThePreviousBatch(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-confirm-fail@example.com", testTenantA)
	_, _ = enrollAndConfirmTOTP(t, f, user.ID)

	// A second enrollment starts a replacement (allowed: the principal
	// carries a completed step-up) and leaves a pending row to confirm.
	elevated := Principal{UserID: user.ID, AMR: []string{MethodPassword, MethodMFATOTP}}
	result, err := f.svc.EnrollTOTP(t.Context(), elevated)
	if err != nil {
		t.Fatalf("EnrollTOTP() error = %v", err)
	}
	code, err := totp.Code(result.Secret, time.Now())
	if err != nil {
		t.Fatalf("totp.Code() error = %v", err)
	}

	// From here on, the recovery-code batch insert is refused.
	failing := true
	failRecoveryCodeCreatesWhile(t, f.db, &failing)

	if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, code); err == nil {
		t.Fatal("ConfirmTOTP() succeeded although the recovery-code write was refused")
	}

	var rows []UserRecoveryCode
	if err := f.db.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil {
		t.Fatalf("query user_recovery_codes: %v", err)
	}
	if len(rows) != recoveryCodeCount {
		t.Errorf("stored %d recovery codes after a refused regeneration, want the previous batch of %d intact (P3-23): the account was left with zero backup codes on its active factor", len(rows), recoveryCodeCount)
	}
}

// TestRegenerateRecoveryCodes_WriteFailure_KeepsThePreviousBatch pins the
// plain regenerate path: RegenerateRecoveryCodes must never land the
// account on its active factor with zero backup codes either, so its
// replacement of the batch is the same single transaction.
func TestRegenerateRecoveryCodes_WriteFailure_KeepsThePreviousBatch(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "mfa-regenerate-fail@example.com", testTenantA)
	_, _ = enrollAndConfirmTOTP(t, f, user.ID)

	failing := true
	failRecoveryCodeCreatesWhile(t, f.db, &failing)

	if _, err := f.svc.RegenerateRecoveryCodes(t.Context(), user.ID); err == nil {
		t.Fatal("RegenerateRecoveryCodes() succeeded although the recovery-code write was refused")
	}

	var rows []UserRecoveryCode
	if err := f.db.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil {
		t.Fatalf("query user_recovery_codes: %v", err)
	}
	if len(rows) != recoveryCodeCount {
		t.Errorf("stored %d recovery codes after a refused regeneration, want the previous batch of %d intact (P3-23)", len(rows), recoveryCodeCount)
	}
}

// TestMFAFactorRepository_SecondPendingRowForOneUserIsRefused pins the
// deterministic half of the pending-row race: the database itself must
// refuse a second PENDING row for one (user, type), under the partial
// unique index migration 0011 (idx_user_mfa_factors_user_type_pending). The
// service-level race test below exercises the concurrent path; this test
// pins the schema that makes two pending rows unrepresentable -- without
// it, nothing would refuse the second insert and ConfirmTOTP's pending
// lookup could take whichever row happened to come first.
func TestMFAFactorRepository_SecondPendingRowForOneUserIsRefused(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	repo, err := NewMFAFactorRepository(db)
	if err != nil {
		t.Fatalf("NewMFAFactorRepository() error = %v", err)
	}
	first := &UserMFAFactor{UserID: "user-1", Type: MFATypeTOTP, Secret: "first-secret", CreatedAt: time.Now()}
	if err := repo.Create(t.Context(), first); err != nil {
		t.Fatalf("create the first pending row: %v", err)
	}

	// A second, racing enrollment's row for the same user and type: the
	// database must refuse it.
	second := &UserMFAFactor{UserID: "user-1", Type: MFATypeTOTP, Secret: "second-secret", CreatedAt: time.Now()}
	createErr := repo.Create(t.Context(), second)
	if createErr == nil {
		var rows []UserMFAFactor
		if err := db.Where("user_id = ? AND type = ? AND status = ?",
			"user-1", MFATypeTOTP, MFAFactorStatusPending).Find(&rows).Error; err != nil {
			t.Fatalf("query pending rows: %v", err)
		}
		t.Fatalf("a second PENDING row for one (user, type) was inserted (%d rows now), want the database to refuse it (P3-22): ConfirmTOTP could take the wrong enrollment", len(rows))
	}
	if !errors.Is(createErr, gorm.ErrDuplicatedKey) {
		t.Fatalf("the refused second insert error = %v, want gorm.ErrDuplicatedKey", createErr)
	}

	// The first row is unharmed, and a pending row may still coexist with an
	// ACTIVE one (the 0010 design) -- only a second pending row is refused.
	active := &UserMFAFactor{
		UserID:      "user-1",
		Type:        MFATypeTOTP,
		Secret:      "active-secret",
		Status:      MFAFactorStatusActive,
		ConfirmedAt: &first.CreatedAt,
		CreatedAt:   time.Now(),
	}
	if err := repo.Create(t.Context(), active); err != nil {
		t.Fatalf("an ACTIVE row alongside the pending one was refused: %v", err)
	}
}

// TestEnrollTOTP_ConcurrentEnrollsLeaveExactlyOnePendingRow pins the
// pending-row race: racing enroll requests for one user must leave exactly
// one PENDING row -- the one whose secret the user was most recently shown
// -- and never two rows for a confirm to take the wrong one. Each
// enrollment is one replace transaction (MFAFactorRepository.ReplacePending)
// whose losing insert the database refuses under migration 0011's partial
// index, so every round below is deterministic: exactly one row survives
// and it is the last enrollment to commit, the one the user was most
// recently shown.
func TestEnrollTOTP_ConcurrentEnrollsLeaveExactlyOnePendingRow(t *testing.T) {
	const (
		rounds = 12
		racers = 4
	)
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newServiceFixture(t)
			user := f.registerUser(t, "mfa-enroll-race@example.com", testTenantA)

			var (
				wg       sync.WaitGroup
				mu       sync.Mutex
				secrets  []string
				unwanted []error
			)
			wg.Add(racers)
			for range racers {
				go func() {
					defer wg.Done()
					result, err := f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						unwanted = append(unwanted, err)
						return
					}
					secrets = append(secrets, result.Secret)
				}()
			}
			wg.Wait()

			if len(unwanted) != 0 {
				t.Fatalf("enrolls answered unexpected errors: %v", unwanted)
			}
			if len(secrets) != racers {
				t.Fatalf("only %d of %d concurrent enrolls succeeded", len(secrets), racers)
			}

			var pending []UserMFAFactor
			if err := f.db.Where("user_id = ? AND type = ? AND status = ?",
				user.ID, MFATypeTOTP, MFAFactorStatusPending).Find(&pending).Error; err != nil {
				t.Fatalf("query pending factors: %v", err)
			}
			if len(pending) != 1 {
				t.Fatalf("%d PENDING rows after %d concurrent enrolls, want exactly 1 (P3-22): a confirm could take the wrong enrollment", len(pending), racers)
			}

			// The surviving row must be one of the enrollments the callers
			// were shown, and confirming with its secret's code must
			// succeed -- the account lands on the factor whose secret the
			// user actually saw, whatever the interleaving.
			found := false
			for _, secret := range secrets {
				if secret == pending[0].Secret {
					found = true
					break
				}
			}
			if !found {
				t.Fatal("the surviving pending row's secret matches none of the enroll responses")
			}
			code, err := totp.Code(pending[0].Secret, time.Now())
			if err != nil {
				t.Fatalf("totp.Code() error = %v", err)
			}
			if _, err := f.svc.ConfirmTOTP(t.Context(), user.ID, code); err != nil {
				t.Fatalf("ConfirmTOTP(surviving enrollment's secret) error = %v", err)
			}
		})
	}
}
