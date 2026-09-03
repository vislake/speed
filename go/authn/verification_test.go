package authn

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testPhone is a syntactically valid E.164 phone number every verification
// test registers its user under. dbkit.NormalizePhoneE164 refuses a bare
// national number (B1's own note for later blocks), so this is
// deliberately already in E.164 form.
const testPhone = "+8613800000099"

// registerPhoneUser creates an account identified by phone only, wires
// tenants for it, and returns it.
func registerPhoneUser(t *testing.T, f *serviceFixture, phone string, tenants ...pkgcore.TenantID) *User {
	t.Helper()
	user, err := f.svc.Register(t.Context(), RegisterInput{
		Phone: phone, Password: testPassword, DisplayName: "Phone User",
	})
	if err != nil {
		t.Fatalf("Register(phone=%s) error = %v", phone, err)
	}
	f.members.Add(user.ID, tenants...)
	return user
}

// smsBuffer builds a fixture whose SMS transport writes to buf, so a test
// can read the exact code that was sent.
func newSMSServiceFixture(t *testing.T, buf *bytes.Buffer, extra ...Option) *serviceFixture {
	t.Helper()
	opts := append([]Option{WithSMSSender(NewConsoleSMSSender(buf))}, extra...)
	return newServiceFixture(t, opts...)
}

// extractSentCode pulls the six-digit code out of the console sender's
// written text, which is the only way a test can learn what code was
// actually generated without reaching into private fields. It scans for a
// maximal run of ASCII digits rather than splitting on whitespace, because
// the rendered zh-CN message has no space between the code and the
// full-width Chinese punctuation that follows it.
func extractSentCode(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	text := buf.String()

	var run []rune
	flush := func() (string, bool) {
		if len(run) == smsCodeDigits {
			return string(run), true
		}
		return "", false
	}
	for _, r := range text {
		if r >= '0' && r <= '9' {
			run = append(run, r)
			continue
		}
		if code, ok := flush(); ok {
			return code
		}
		run = run[:0]
	}
	if code, ok := flush(); ok {
		return code
	}

	t.Fatalf("no %d-digit code found in sms output %q", smsCodeDigits, text)
	return ""
}

// TestRequestSMSCode_UnknownPhone_SendsNothingButSucceeds proves the
// enumeration defence: a request for a phone with no account behind it
// succeeds with no error and delivers no message.
func TestRequestSMSCode_UnknownPhone_SendsNothingButSucceeds(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.1"}); err != nil {
		t.Fatalf("RequestSMSCode(unknown phone) error = %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("RequestSMSCode(unknown phone) sent %q, want nothing sent", buf.String())
	}
}

// TestRequestSMSCode_KnownPhone_SendsCode proves the happy path delivers a
// code through the wired SMSSender.
func TestRequestSMSCode_KnownPhone_SendsCode(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.2"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	code := extractSentCode(t, &buf)
	if len(code) != smsCodeDigits {
		t.Errorf("sent code %q has length %d, want %d", code, len(code), smsCodeDigits)
	}
}

// TestLoginWithSMSCode_HappyPath proves a correct code signs the user in,
// finds the account through the phone blind index in several equivalent
// formattings, and marks the phone verified.
func TestLoginWithSMSCode_HappyPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		loginPhone  string
		registerRaw string
	}{
		{name: "exact E.164", loginPhone: "+8613800000001", registerRaw: "+8613800000001"},
		{name: "spaced E.164 finds the same account", loginPhone: "+86 138 0000 0001", registerRaw: "+8613800000001"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			f := newSMSServiceFixture(t, &buf)
			user := registerPhoneUser(t, f, tc.registerRaw, testTenantA)
			if user.PhoneVerified {
				t.Fatalf("newly registered user is already PhoneVerified, want false before SMS login")
			}

			if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: tc.registerRaw, IP: "203.0.113.3"}); err != nil {
				t.Fatalf("RequestSMSCode() error = %v", err)
			}
			code := extractSentCode(t, &buf)

			pair, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
				Phone: tc.loginPhone, Code: code, TenantID: testTenantA, IP: "203.0.113.3",
			})
			if err != nil {
				t.Fatalf("LoginWithSMSCode() error = %v", err)
			}
			if pair.AccessToken == "" || pair.RefreshToken == "" {
				t.Errorf("LoginWithSMSCode() returned an incomplete token pair: %+v", pair)
			}
			if !containsString(pair.Principal.AMR, MethodSMS) {
				t.Errorf("Principal.AMR = %v, want it to contain %q", pair.Principal.AMR, MethodSMS)
			}

			reloaded, err := f.svc.Users().FindByID(t.Context(), user.ID)
			if err != nil {
				t.Fatalf("FindByID() error = %v", err)
			}
			if !reloaded.PhoneVerified {
				t.Errorf("PhoneVerified = false after a successful SMS login, want true")
			}
		})
	}
}

// TestLoginWithSMSCode_WrongCode_Refused proves a wrong code is refused
// with the generic ErrVerificationCodeInvalid and does not sign anyone in.
func TestLoginWithSMSCode_WrongCode_Refused(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.4"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	real := extractSentCode(t, &buf)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	_, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: wrong, IP: "203.0.113.4"})
	if !errors.Is(err, ErrVerificationCodeInvalid) {
		t.Fatalf("LoginWithSMSCode(wrong code) error = %v, want ErrVerificationCodeInvalid", err)
	}
}

// TestLoginWithSMSCode_CodeIsSingleUse proves a code cannot be replayed
// after a successful verification.
func TestLoginWithSMSCode_CodeIsSingleUse(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.5"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	code := extractSentCode(t, &buf)

	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: code, IP: "203.0.113.5"}); err != nil {
		t.Fatalf("first LoginWithSMSCode() error = %v", err)
	}

	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: code, IP: "203.0.113.5"}); !errors.Is(err, ErrVerificationCodeInvalid) {
		t.Fatalf("second LoginWithSMSCode() (replay) error = %v, want ErrVerificationCodeInvalid", err)
	}
}

// TestLoginWithSMSCode_ExpiresAfterTTL proves a code stops working once its
// TTL has passed, using the fixture's manual clock rather than sleeping.
func TestLoginWithSMSCode_ExpiresAfterTTL(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeTTL(DefaultSMSCodeTTL))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.6"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	code := extractSentCode(t, &buf)

	f.clock.Advance(DefaultSMSCodeTTL + time.Second)

	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: code, IP: "203.0.113.6"}); !errors.Is(err, ErrVerificationCodeInvalid) {
		t.Fatalf("LoginWithSMSCode(expired code) error = %v, want ErrVerificationCodeInvalid", err)
	}
}

// TestLoginWithSMSCode_LocksAfterMaxAttempts proves a wrong code increments
// the attempt counter, and that the SAME code -- even guessed correctly
// after the limit -- no longer works once the code has locked.
func TestLoginWithSMSCode_LocksAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	const maxAttempts = 3
	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeMaxAttempts(maxAttempts))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.7"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	real := extractSentCode(t, &buf)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	for i := range maxAttempts {
		if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: wrong, IP: "203.0.113.7"}); !errors.Is(err, ErrVerificationCodeInvalid) {
			t.Fatalf("attempt %d: LoginWithSMSCode(wrong code) error = %v, want ErrVerificationCodeInvalid", i, err)
		}
	}

	// The code has now locked. Even the REAL code must be refused: a
	// locked code requires a fresh one, not one more guess.
	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: real, IP: "203.0.113.7"}); !errors.Is(err, ErrVerificationCodeInvalid) {
		t.Fatalf("LoginWithSMSCode(real code, after lockout) error = %v, want ErrVerificationCodeInvalid", err)
	}
}

// TestMarkPhoneLoginAttempt_RetriesOnLostRace proves a wrong guess that
// loses MarkAttempt's compare-and-swap race to a CONCURRENT wrong guess
// against the SAME code still gets its own attempt recorded, rather than
// being silently dropped and under-counting the shared counter.
//
// The race is reproduced deterministically rather than with real
// goroutines: this test reads the record once (the "stale" snapshot a
// goroutine would be holding when it loses the race), then commits a
// SEPARATE MarkAttempt call that advances the real row out from under
// it -- exactly what a concurrent wrong guess landing between
// verifyPhoneLoginCode's own read and write would do -- before finally
// calling markPhoneLoginAttempt with the stale snapshot.
func TestMarkPhoneLoginAttempt_RetriesOnLostRace(t *testing.T) {
	t.Parallel()

	const maxAttempts = 5
	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeMaxAttempts(maxAttempts))
	registerPhoneUser(t, f, testPhone, testTenantA)
	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.21"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}

	index, err := f.svc.users.PhoneIndexOf(testPhone)
	if err != nil {
		t.Fatalf("PhoneIndexOf() error = %v", err)
	}
	stale, err := f.svc.verificationCodes.FindLatestActive(t.Context(), VerificationPurposePhoneLogin, index, f.svc.now())
	if err != nil {
		t.Fatalf("FindLatestActive() error = %v", err)
	}
	if stale.Attempts != 0 {
		t.Fatalf("Attempts = %d for a freshly issued code, want 0", stale.Attempts)
	}

	// A concurrent wrong guess wins the compare-and-swap first, advancing
	// Attempts from 0 to 1 behind this goroutine's back.
	won, err := f.svc.verificationCodes.MarkAttempt(t.Context(), stale.ID, stale.Attempts, false)
	if err != nil || !won {
		t.Fatalf("simulated concurrent MarkAttempt() = (%v, %v), want (true, nil)", won, err)
	}

	// This goroutine's own guess -- still holding the STALE record it read
	// before the concurrent write landed -- must still get counted, not
	// silently dropped by a single unretried MarkAttempt(id, 0, ...) call
	// that loses this exact race.
	f.svc.markPhoneLoginAttempt(t.Context(), index, stale)

	after, err := f.svc.verificationCodes.FindLatestActive(t.Context(), VerificationPurposePhoneLogin, index, f.svc.now())
	if err != nil {
		t.Fatalf("FindLatestActive() (after) error = %v", err)
	}
	if after.Attempts != 2 {
		t.Errorf("Attempts = %d after a concurrent guess plus this goroutine's own (via a stale record), want 2 (both counted)", after.Attempts)
	}
}

// TestVerificationCodeModel_IsNotTenantScoped is the mandatory isolation
// assertion for this round's identity-domain table: a code is issued to a
// phone number, not to a tenant, so it must stay visible whatever tenant
// happens to be in the calling context.
func TestVerificationCodeModel_IsNotTenantScoped(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	tenancytest.AssertNotTenantScoped(t, db, VerificationCode{},
		func(db *gorm.DB) error {
			return db.Create(&VerificationCode{
				ID: newID(), Purpose: VerificationPurposePhoneLogin, TargetIndex: newID(),
				CodeHash: "x", MaxAttempts: 5, Status: VerificationCodeStatusActive,
				CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			}).Error
		},
		countOf[VerificationCode],
	)
}

// TestSMSDeliveryFailure_ReturnsError proves a transport failure surfaces
// as ErrSMSDeliveryFailed rather than a silent success.
func TestSMSDeliveryFailure_ReturnsError(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t, WithSMSSender(failingSMSSender{}))
	registerPhoneUser(t, f, testPhone, testTenantA)

	err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.8"})
	if !hasCode(err, ErrSMSDeliveryFailed.Code) {
		t.Fatalf("RequestSMSCode() error = %v, want ErrSMSDeliveryFailed", err)
	}
}

// failingSMSSender is an SMSSender whose every Send fails, for proving a
// delivery failure surfaces rather than being silently swallowed.
type failingSMSSender struct{}

func (failingSMSSender) Send(context.Context, SMS) error {
	return errSMSSendFailed
}

var errSMSSendFailed = errors.New("verification_test: sms send deliberately fails")

// containsString reports whether s contains v. A small local helper rather
// than slices.Contains at every call site, kept for readability next to
// the AMR assertions above.
func containsString(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
