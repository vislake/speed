package authn

import (
	"bytes"
	"context"
	"errors"
	"math"
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
// enumeration defence at the RESPONSE-BODY layer: a request for a phone
// with no account behind it succeeds with no error and delivers no
// message. This is only half the P2-4 defence -- see
// TestRequestSMSCode_TimingParity_KnownAndUnknownPhoneAnswerInComparableTime
// below for the WALL-CLOCK half: an identical response body sent back
// measurably faster is still a working enumeration oracle.
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

// TestRequestSMSCode_TimingParity_KnownAndUnknownPhoneAnswerInComparableTime
// is the regression for P2-4's other half: the WALL-CLOCK time to answer
// must not tell a known number apart from an unknown one, even though the
// response BODY already does not (proven separately just above). Before
// the fix, an unknown number returned in microseconds (no DB write, no SMS
// send) while a known number paid for both -- a gap an attacker rotating
// IPs (to dodge limitSMSSendByIP) could probe directly. After the fix,
// RequestSMSCode pads both branches up to a shared smsCodeRequestLatencyFloor.
//
// Deliberately NOT t.Parallel(): it temporarily overrides the
// package-level smsCodeRequestLatencyFloor var, which is safe only while
// no other test's body is concurrently executing -- true for a serial
// (non-parallel) test, since every other top-level test in this package
// either has already finished or is paused at its own t.Parallel() call
// (not yet running) for as long as this one is still running. See
// smsCodeRequestLatencyFloor's own doc comment for why a var rather than a
// const, and t.Cleanup below for the restore.
func TestRequestSMSCode_TimingParity_KnownAndUnknownPhoneAnswerInComparableTime(t *testing.T) {
	orig := smsCodeRequestLatencyFloor
	smsCodeRequestLatencyFloor = 30 * time.Millisecond
	t.Cleanup(func() { smsCodeRequestLatencyFloor = orig })

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	knownStart := time.Now()
	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.210"}); err != nil {
		t.Fatalf("RequestSMSCode(known) error = %v", err)
	}
	knownDuration := time.Since(knownStart)

	unknownStart := time.Now()
	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: "+8613800099999", IP: "203.0.113.211"}); err != nil {
		t.Fatalf("RequestSMSCode(unknown) error = %v", err)
	}
	unknownDuration := time.Since(unknownStart)

	// A generous 3x tolerance absorbs scheduler/GC jitter on a loaded CI
	// runner (both durations are dominated by the SAME floor sleep, so
	// genuine timing noise between them is only ever a few milliseconds)
	// while still catching the pre-fix shape: an unfixed unknown-phone
	// path returns in low microseconds against a known-phone path's
	// multiple milliseconds (a real DB write plus a console SMS send) --
	// many orders of magnitude apart, not a mere 3x.
	const toleranceFactor = 3
	if ratio := durationRatio(knownDuration, unknownDuration); ratio > toleranceFactor {
		t.Errorf("timing ratio between known (%v) and unknown (%v) phone requests = %.2f, want <= %d (the timing side channel is not closed)",
			knownDuration, unknownDuration, ratio, toleranceFactor)
	}
}

// durationRatio returns how many times longer the larger of a and b is
// than the smaller, always >= 1. Either duration being non-positive
// (should not happen for a real measured elapsed time, but a defensive
// floor all the same) reports +Inf, so a caller comparing against a finite
// tolerance always fails loudly rather than dividing by (or comparing
// against) zero.
func durationRatio(a, b time.Duration) float64 {
	if a <= 0 || b <= 0 {
		return math.Inf(1)
	}
	if a > b {
		return float64(a) / float64(b)
	}
	return float64(b) / float64(a)
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

// TestLoginWithSMSCode_AttackerWrongGuesses_DoNotBlockVictimsCorrectCode is
// the regression for P2-3: an attacker who knows the victim's phone number
// but not the code used to be able to hold the shared per-target rate-limit
// budget hostage, permanently denying the real holder's own correct
// attempt for the rest of the window. See ratelimit.go's
// CheckSMSVerifyWrongGuess and LoginWithSMSCode's own doc comment for the
// fix.
//
// smsCodeMaxAttempts is deliberately raised well above the rate limiter's
// own Rate so the attacker's wrong guesses exhaust the RATE LIMITER without
// also locking the code itself (verifyPhoneLoginCode's independent
// MaxAttempts counter) -- this test isolates the rate limiter's behavior
// from that separate protection, which
// TestLoginWithSMSCode_SustainedWrongGuessing_StillLocksViaMaxAttempts
// below covers on its own.
func TestLoginWithSMSCode_AttackerWrongGuesses_DoNotBlockVictimsCorrectCode(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeMaxAttempts(limitSMSVerifyByTarget.Rate+5))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.200"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	real := extractSentCode(t, &buf)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	// The attacker, from their OWN IP, knows the victim's phone number but
	// not the code: enough wrong guesses to exhaust the per-target verify
	// budget entirely.
	for i := 0; i < limitSMSVerifyByTarget.Rate; i++ {
		if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
			Phone: testPhone, Code: wrong, IP: "198.51.100.66",
		}); !errors.Is(err, ErrVerificationCodeInvalid) {
			t.Fatalf("attacker attempt %d: LoginWithSMSCode(wrong code) error = %v, want ErrVerificationCodeInvalid", i, err)
		}
	}

	// The real account holder, from a DIFFERENT IP, now tries their own
	// correct code. The per-target budget the attacker just exhausted must
	// not be what decides this call: a correct code always gets through.
	pair, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
		Phone: testPhone, Code: real, TenantID: testTenantA, IP: "203.0.113.200",
	})
	if err != nil {
		t.Fatalf("LoginWithSMSCode(real holder's correct code, after attacker exhausted the target budget) error = %v, want nil", err)
	}
	if pair.AccessToken == "" {
		t.Errorf("LoginWithSMSCode() returned an incomplete token pair: %+v", pair)
	}
}

// TestLoginWithSMSCode_SustainedWrongGuessing_StillLocksViaMaxAttempts is
// the guard regression for P2-3: proving the fix above did not weaken
// brute-force resistance against the code itself. A sustained attacker
// guessing wrong from ONE source still gets the code locked by
// smsCodeMaxAttempts (verifyPhoneLoginCode's own, independent counter),
// which the rate-limiter change never touched.
func TestLoginWithSMSCode_SustainedWrongGuessing_StillLocksViaMaxAttempts(t *testing.T) {
	t.Parallel()

	const maxAttempts = 3
	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeMaxAttempts(maxAttempts))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.201"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	real := extractSentCode(t, &buf)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	// A sustained attacker from ONE source guesses wrong maxAttempts times.
	for i := range maxAttempts {
		if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
			Phone: testPhone, Code: wrong, IP: "198.51.100.77",
		}); !errors.Is(err, ErrVerificationCodeInvalid) {
			t.Fatalf("attempt %d: LoginWithSMSCode(wrong code) error = %v, want ErrVerificationCodeInvalid", i, err)
		}
	}

	// The code is now locked: even the REAL code, from the same source,
	// must be refused -- this is the brute-force defense the rate-limiter
	// fix above must not have weakened.
	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
		Phone: testPhone, Code: real, IP: "198.51.100.77",
	}); !errors.Is(err, ErrVerificationCodeInvalid) {
		t.Fatalf("LoginWithSMSCode(real code, after MaxAttempts reached) error = %v, want ErrVerificationCodeInvalid", err)
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

// TestRequestSMSCode_GatewayFailure_AnswersLikeAnUnknownNumber is the P2-8
// regression: a delivery failure on a REGISTERED number must not answer
// differently from a request for an unregistered number. Before the fix the
// registered branch returned ErrSMSDeliveryFailed when the SMS gateway
// failed while the unknown-number branch returned nil -- a status split
// (500 vs 202) that turned every gateway outage into a registration oracle,
// undoing the response-body defence
// TestRequestSMSCode_UnknownPhone_SendsNothingButSucceeds proves. The real
// error stays in the log line the transport branch writes; the request
// answers identically to a successful send, and a code that never arrived
// simply fails its later verification with the same generic
// ErrVerificationCodeInvalid answer every other dead code gets.
func TestRequestSMSCode_GatewayFailure_AnswersLikeAnUnknownNumber(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t, WithSMSSender(failingSMSSender{}))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.8"}); err != nil {
		t.Fatalf("RequestSMSCode(registered phone, gateway down) error = %v, want nil: a gateway outage must answer exactly like the unknown-number branch, or it discloses that the number is registered", err)
	}
	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: "+8613800000000", IP: "203.0.113.9"}); err != nil {
		t.Fatalf("RequestSMSCode(unknown phone) error = %v", err)
	}
}

// failingSMSSender is an SMSSender whose every Send fails, for proving a
// delivery failure is logged and answered past, never surfaced.
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
