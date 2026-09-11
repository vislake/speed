package authn

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testPhone is a syntactically valid E.164 phone number every verification
// test registers its user under. dbkit.NormalizePhoneE164 refuses a bare
// national number, so this is deliberately already in E.164 form.
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
	opts := append([]Option{WithSMSSender(pkgcore.NewConsoleSMSSender(buf))}, extra...)
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
// message. This is only half the defence -- see
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
// pins the WALL-CLOCK half of the defence: the time to answer must not
// tell a known number apart from an unknown one, even though the response
// BODY already does not (proven separately just above). An unknown number
// would otherwise return in microseconds (no DB write, no SMS send) while
// a known number pays for both -- a gap an attacker rotating IPs (to dodge
// limitSMSSendByIP) could probe directly. RequestSMSCode pads both
// branches up to a shared smsCodeRequestLatencyFloor.
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
	// 200ms, sized so the floor stays above the known branch's real work
	// -- a persisted code row plus a console send, ~130ms on a loaded CI
	// runner under -race -- and only sleep-overshoot noise is left for the
	// ratio assertion below to absorb.
	smsCodeRequestLatencyFloor = 200 * time.Millisecond
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

	// A generous 3x tolerance absorbs scheduler jitter and sleep overshoot
	// on a loaded CI runner. The 200ms floor above is sized above the
	// known branch's real work (a persisted code row plus a console send,
	// ~130ms on a loaded CI runner under -race), so both branches are
	// padded to the same floor and the ratio stays near 1: only sleep
	// overshoot -- never the work itself -- falls to the tolerance. The
	// assertion still catches the shape with no effective floor: an
	// unknown number's unpadded branch answers in microseconds against a
	// known number's milliseconds (orders of magnitude, not 3x), and the
	// ratio only reaches 3 once that work exceeds 3x the floor (~600ms).
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

// TestLoginWithSMSCode_AuthMetricCountsTheChannel pins the metric
// vocabulary's coverage of the SMS-code sign-in channel: the channel is
// brute-forceable (a six-digit code behind a per-target wrong-guess
// budget, the 10th wrong guess refused), so an operator alerting on
// sign-in failure rate needs the channel's own data point on
// authCountMetricName/authDurationMetricName -- the vocabulary that covers
// password login, refresh and MFA challenge must cover this channel too. A
// wrong guess here is refused and must land under operation=login_sms
// (authOpSMSCodeLogin); without an operation value for the channel, no
// data point carries it and authCounterValue fails the test.
//
// Deliberately not t.Parallel(): it swaps the process-wide global otel
// MeterProvider (see setupAuthMetricsMeterProvider's own doc comment).
func TestLoginWithSMSCode_AuthMetricCountsTheChannel(t *testing.T) {
	reader := setupAuthMetricsMeterProvider(t)
	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.9"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}
	real := extractSentCode(t, &buf)
	wrong := "000000"
	if wrong == real {
		wrong = "111111"
	}

	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{Phone: testPhone, Code: wrong, IP: "203.0.113.9"}); err == nil {
		t.Fatal("LoginWithSMSCode(wrong code) error = nil, want a refusal")
	}

	// "login_sms" is the operation label (authOpSMSCodeLogin in
	// service.go). The literal is asserted rather than the constant, so a
	// regression that labels the channel's data points differently fails
	// this test at runtime -- no data point carries the label -- instead
	// of reusing the very constant the regression changed; a rename of
	// the constant's value breaks this assertion, the same pin a constant
	// reference would give.
	count := collectAuthMetric(t, reader, authCountMetricName)
	if got := authCounterValue(t, count, "login_sms", authOutcomeFailed); got != 1 {
		t.Errorf("%s{operation=login_sms,outcome=failed} = %d, want 1", authCountMetricName, got)
	}
	duration := collectAuthMetric(t, reader, authDurationMetricName)
	if got := authHistogramCount(t, duration, "login_sms", authOutcomeFailed); got != 1 {
		t.Errorf("%s{operation=login_sms,outcome=failed} count = %d, want 1", authDurationMetricName, got)
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

// TestLoginWithSMSCode_AttackerWrongGuesses_DoNotBlockVictimsCorrectCode
// pins the wrong-guess-only budget shape: an attacker who knows the
// victim's phone number but not the code must not be able to hold the
// shared per-target rate-limit budget hostage, permanently denying the
// real holder's own correct attempt for the rest of the window. The budget
// is consumed only after a guess has been determined wrong (ratelimit.go's
// CheckSMSVerifyWrongGuess; LoginWithSMSCode's own doc comment).
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

// TestLoginWithSMSCode_SustainedWrongGuessing_StillLocksViaMaxAttempts
// guards the other side of the wrong-guess budget: the budget shape must
// not weaken brute-force resistance against the code itself. A sustained
// attacker guessing wrong from ONE source still gets the code locked by
// smsCodeMaxAttempts (verifyPhoneLoginCode's own, independent counter),
// which the rate limiter never touches.
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
	// must be refused -- the per-code lock must hold independently of how
	// the shared rate-limit budget is shaped.
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
	f.svc.markPhoneLoginAttempt(t.Context(), stale)

	after, err := f.svc.verificationCodes.FindLatestActive(t.Context(), VerificationPurposePhoneLogin, index, f.svc.now())
	if err != nil {
		t.Fatalf("FindLatestActive() (after) error = %v", err)
	}
	if after.Attempts != 2 {
		t.Errorf("Attempts = %d after a concurrent guess plus this goroutine's own (via a stale record), want 2 (both counted)", after.Attempts)
	}
}

// TestMarkPhoneLoginAttempt_LosingAttemptOnOldCode_NeverCountsAgainstTheNewCode
// pins markPhoneLoginAttempt's retry target: a retry after losing
// MarkAttempt's compare-and-swap must re-read THE RECORD (by id) whose
// compare-and-swap just lost, never "the latest active code for the
// target". If the user re-requests a code while the losing guess's retry is
// still running, a latest-code re-read would pick up the NEW code and count
// the old guess -- aimed at a code the user had already abandoned --
// against the new code's attempt budget, up to pushing the just-issued
// code to its MaxAttempts before the user ever typed it.
//
// Like TestMarkPhoneLoginAttempt_RetriesOnLostRace, the race is reproduced
// deterministically rather than with real goroutines: code A's row is
// advanced out from under the stale snapshot this goroutine holds, code B is
// issued for the same target (exactly what a re-request mid-race does), and
// only then does the losing guess retry. maxAttempts is 1 so that one stray
// count LOCKS the fresh code outright -- the strongest form of the harm --
// and a correct login with code B is asserted afterwards.
func TestMarkPhoneLoginAttempt_LosingAttemptOnOldCode_NeverCountsAgainstTheNewCode(t *testing.T) {
	t.Parallel()

	const maxAttempts = 1
	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf, WithSMSCodeMaxAttempts(maxAttempts))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.51"}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}

	index, err := f.svc.users.PhoneIndexOf(testPhone)
	if err != nil {
		t.Fatalf("PhoneIndexOf() error = %v", err)
	}
	recordA, err := f.svc.verificationCodes.FindLatestActive(t.Context(), VerificationPurposePhoneLogin, index, f.svc.now())
	if err != nil {
		t.Fatalf("FindLatestActive() error = %v", err)
	}
	if recordA.Attempts != 0 {
		t.Fatalf("Attempts = %d for a freshly issued code, want 0", recordA.Attempts)
	}

	// A concurrent wrong guess wins the compare-and-swap on A first -- and,
	// with maxAttempts 1, locks A in the same move. This goroutine still
	// holds the stale snapshot taken before that write.
	won, err := f.svc.verificationCodes.MarkAttempt(t.Context(), recordA.ID, recordA.Attempts, true)
	if err != nil || !won {
		t.Fatalf("simulated concurrent MarkAttempt() = (%v, %v), want (true, nil)", won, err)
	}

	// The user re-requests while the losing guess's retry is still pending:
	// code B is issued for the same target and becomes the latest active
	// code. The clock advances so B's created_at is strictly newer than A's
	// (both codes would otherwise share the fixture clock's instant, making
	// "latest" fall to a random id tie-break).
	f.clock.Advance(time.Second)
	buf.Reset()
	if reqErr := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.52"}); reqErr != nil {
		t.Fatalf("RequestSMSCode() (code B) error = %v", reqErr)
	}
	codeB := extractSentCode(t, &buf)

	// The losing guess for code A retries now. It must land on A -- which is
	// locked and therefore a no-op -- and never touch B.
	f.svc.markPhoneLoginAttempt(t.Context(), recordA)

	latest, err := f.svc.verificationCodes.FindLatestActive(t.Context(), VerificationPurposePhoneLogin, index, f.svc.now())
	if err != nil {
		t.Fatalf("FindLatestActive() (after) error = %v", err)
	}
	if latest.Attempts != 0 {
		t.Errorf("code B Attempts = %d after code A's losing attempt retried, want 0 (A's retry must re-read A by id, never the target's latest active code)", latest.Attempts)
	}
	if latest.Status != VerificationCodeStatusActive {
		t.Errorf("code B Status = %q after code A's losing attempt retried, want %q (A's retry must never lock the just-issued code)", latest.Status, VerificationCodeStatusActive)
	}

	// The decisive harm assertion: code B still signs the user in with its
	// full budget intact.
	if _, err := f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
		Phone: testPhone, Code: codeB, IP: "203.0.113.53",
	}); err != nil {
		t.Errorf("LoginWithSMSCode(real code B) error = %v, want a session (the new code's budget was burned by code A's losing attempt)", err)
	}
}

// TestVerificationCodeModel_IsNotTenantScoped is the mandatory isolation
// assertion for the module's verification-code table: a code is issued to a
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

// TestRequestSMSCode_GatewayFailure_AnswersLikeAnUnknownNumber pins the
// delivery-failure answer: a delivery failure on a REGISTERED number must
// not answer differently from a request for an unregistered number. If the
// registered branch answered ErrSMSDeliveryFailed when the SMS gateway
// failed while the unknown-number branch returned nil, the status split
// (500 vs 202) would turn every gateway outage into a registration oracle,
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

func (failingSMSSender) Send(context.Context, pkgcore.SMS) error {
	return errSMSSendFailed
}

var errSMSSendFailed = errors.New("verification_test: sms send deliberately fails")

// TestRequestSMSCode_RenderFailure_AnswersLikeAnUnknownNumber pins the
// rendering-failure answer: a registered phone whose SMS body cannot be
// rendered must not answer differently from a request for an unregistered
// number. Only the registered branch ever renders -- the unknown-number
// branch burns a code and stops -- so surfacing the render error as
// ErrInternal would answer 500 for registered numbers while unregistered
// ones answered nil, a registration oracle for as long as the locale
// bundles are broken.
//
// Deliberately NOT t.Parallel(): it swaps the package-level SMS locale
// cache (loadSMSLocaleMessages' smsLocaleOnce/smsLocaleMessages/smsLocaleErr)
// for one with no bundles at all, which makes renderSMSCode fail for any
// locale. The serialization argument is
// TestRequestSMSCode_TimingParity_KnownAndUnknownPhoneAnswerInComparableTime's:
// a non-parallel test's body never runs concurrently with a parallel test's,
// so no other test can observe the fake. The cleanup re-fires the cache from
// the real embedded files rather than restoring saved values, because a
// sync.Once cannot be copied.
func TestRequestSMSCode_RenderFailure_AnswersLikeAnUnknownNumber(t *testing.T) {
	smsLocaleOnce = sync.Once{}
	smsLocaleMessages = map[string]map[string]string{}
	smsLocaleErr = nil
	smsLocaleOnce.Do(func() {})
	t.Cleanup(func() {
		smsLocaleOnce = sync.Once{}
		smsLocaleMessages = nil
		smsLocaleErr = nil
		if _, err := loadSMSLocaleMessages(); err != nil {
			t.Errorf("restore the real SMS locale bundles: %v", err)
		}
	})

	var buf bytes.Buffer
	f := newSMSServiceFixture(t, &buf)
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: testPhone, IP: "203.0.113.10"}); err != nil {
		t.Fatalf("RequestSMSCode(registered phone, render failure) error = %v, want nil: a render failure must answer exactly like the unknown-number branch, or it discloses that the number is registered", err)
	}
	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: "+8613800000000", IP: "203.0.113.11"}); err != nil {
		t.Fatalf("RequestSMSCode(unknown phone) error = %v", err)
	}
}

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

// smsZhCNMarker and smsEnUSMarker are stable substrings of the two shipped
// templates for authn.sms.verification_code, used to tell which language a
// delivered body renders in without depending on the code or minutes values.
const (
	smsZhCNMarker = "验证码"
	smsEnUSMarker = "verification code"
)

// TestSMSLocale_Chain drives the SMS body's language chain at the rule's
// level: the request's own language (transporting the frontend chain's
// resolved value) outranks the account's stored locale, the stored locale
// is the fallback for a request that sent no usable header, and the
// platform default closes a chain that resolves nothing.
func TestSMSLocale_Chain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		acceptLanguage string
		stored         string
		want           string
	}{
		{"request language outranks the stored locale", "en-US", "zh-CN", "en-US"},
		{"request language prefix matches", "zh", "en-US", "zh-CN"},
		{"stored locale answers when the request sent nothing", "", "zh-CN", "zh-CN"},
		{"stored locale answers when the request matches nothing", "fr-FR", "zh-CN", "zh-CN"},
		{"stored value that is not a shipped language is skipped", "", "de-DE", DefaultLocale},
		{"an empty chain lands on the platform default", "", "", DefaultLocale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := smsLocale(tc.acceptLanguage, tc.stored); got != tc.want {
				t.Errorf("smsLocale(%q, %q) = %q, want %q", tc.acceptLanguage, tc.stored, got, tc.want)
			}
		})
	}
}

// TestRequestSMSCode_LanguageChain_DeliversInTheNegotiatedLanguage pins the
// chain end to end through the real send path: the same account (stored
// locale zh-CN) receives an English body when the request carries an
// en-US Accept-Language and a Chinese body when it carries none, and a
// request whose header matches nothing falls back to the stored locale.
func TestRequestSMSCode_LanguageChain_DeliversInTheNegotiatedLanguage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		acceptLanguage string
		wantMarker     string
	}{
		{"request language wins over the stored locale", "en-US", smsEnUSMarker},
		{"stored locale answers a request with no header", "", smsZhCNMarker},
		{"stored locale answers a request matching nothing", "fr-FR", smsZhCNMarker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			f := newSMSServiceFixture(t, &buf)
			if _, err := f.svc.Register(t.Context(), RegisterInput{
				Phone: testPhone, Password: testPassword, DisplayName: "Phone User",
				Locale: "zh-CN",
			}); err != nil {
				t.Fatalf("Register() error = %v", err)
			}
			if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{
				Phone: testPhone, AcceptLanguage: tc.acceptLanguage, IP: "203.0.113.77",
			}); err != nil {
				t.Fatalf("RequestSMSCode() error = %v", err)
			}
			if sent := buf.String(); !strings.Contains(sent, tc.wantMarker) {
				t.Errorf("sent SMS %q, want it to carry the %q template", sent, tc.wantMarker)
			}
		})
	}

	t.Run("an account with no stored locale receives the platform default", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		f := newSMSServiceFixture(t, &buf)
		registerPhoneUser(t, f, testPhone, testTenantA)
		if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{
			Phone: testPhone, IP: "203.0.113.78",
		}); err != nil {
			t.Fatalf("RequestSMSCode() error = %v", err)
		}
		if sent := buf.String(); !strings.Contains(sent, smsEnUSMarker) {
			t.Errorf("sent SMS %q, want the platform default's %q template", sent, smsEnUSMarker)
		}
	})
}

// recordingSMSSender keeps every message it is asked to send, so a test can
// assert the whole SMS the seam received -- the console sender only prints
// the rendered text, which is precisely the half a template-typed carrier
// adapter does NOT route by.
type recordingSMSSender struct {
	mu   sync.Mutex
	sent []pkgcore.SMS
}

func (s *recordingSMSSender) Send(_ context.Context, sms pkgcore.SMS) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, sms)
	return nil
}

func (s *recordingSMSSender) messages() []pkgcore.SMS {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]pkgcore.SMS(nil), s.sent...)
}

// TestRequestSMSCode_SeamCarriesMessageIdentityAndParams pins the identity
// the phone-login code's send carries on the SMS seam: the message id the
// body was rendered from, the locale it was rendered in, and exactly the
// two values it interpolated -- the code itself and the lifetime, whose
// string form comes from the same constant the rendered body interpolates.
func TestRequestSMSCode_SeamCarriesMessageIdentityAndParams(t *testing.T) {
	t.Parallel()

	sender := &recordingSMSSender{}
	f := newServiceFixture(t, WithSMSSender(sender))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{
		Phone: testPhone, AcceptLanguage: "zh-CN", IP: "203.0.113.80",
	}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("SMS sender sent %d messages, want the one verification code", len(msgs))
	}
	sms := msgs[0]
	if sms.MessageID != smsVerificationCodeMessageID {
		t.Errorf("SMS MessageID = %q, want %q", sms.MessageID, smsVerificationCodeMessageID)
	}
	if sms.Locale != "zh-CN" {
		t.Errorf("SMS Locale = %q, want the requested zh-CN locale the body rendered in", sms.Locale)
	}
	if len(sms.Params) != 2 {
		t.Fatalf("SMS Params = %v, want exactly code and minutes", sms.Params)
	}
	code := sms.Params["code"]
	if len(code) != smsCodeDigits || !strings.Contains(sms.Text, code) {
		t.Errorf("SMS Params[code] = %q, want the %d-digit code the rendered body carries", code, smsCodeDigits)
	}
	if want := strconv.Itoa(int(f.svc.smsCodeTTL / time.Minute)); sms.Params["minutes"] != want {
		t.Errorf("SMS Params[minutes] = %q, want %q (the same lifetime the render interpolates)", sms.Params["minutes"], want)
	}
}

// TestRequestSMSCode_EmptyStoredLocale_SeamCarriesTheRenderedFallback pins
// the seam locale against the render's fallback, the asymmetry that makes
// the seam value the render's own: a user whose stored locale is empty (and
// whose request carries no language) has the body rendered in DefaultLocale,
// and the send must therefore carry DefaultLocale -- reporting the empty
// request-side value would make a template-typed carrier adapter fail
// closed on a delivery the render itself handled.
func TestRequestSMSCode_EmptyStoredLocale_SeamCarriesTheRenderedFallback(t *testing.T) {
	t.Parallel()

	sender := &recordingSMSSender{}
	f := newServiceFixture(t, WithSMSSender(sender))
	registerPhoneUser(t, f, testPhone, testTenantA)

	if err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{
		Phone: testPhone, IP: "203.0.113.81",
	}); err != nil {
		t.Fatalf("RequestSMSCode() error = %v", err)
	}

	msgs := sender.messages()
	if len(msgs) != 1 {
		t.Fatalf("SMS sender sent %d messages, want the one verification code", len(msgs))
	}
	if msgs[0].Locale != DefaultLocale {
		t.Errorf("SMS Locale = %q, want the post-fallback %q the body was actually rendered in", msgs[0].Locale, DefaultLocale)
	}
	if !strings.Contains(msgs[0].Text, smsEnUSMarker) {
		t.Errorf("SMS text = %q, want the platform default's %q template rendered", msgs[0].Text, smsEnUSMarker)
	}
}
