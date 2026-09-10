package authn

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy"
)

const (
	testTenantA  = pkgcore.TenantID("tenant-a")
	testTenantB  = pkgcore.TenantID("tenant-b")
	testPassword = "a perfectly fine passphrase"
)

// serviceFixture assembles a Service over an in-memory database with the
// standalone deployment mode's own implementations standing in as test
// doubles -- which is the point of those implementations existing.
type serviceFixture struct {
	svc     *Service
	db      *gorm.DB
	kv      pkgcore.KVStore
	bus     pkgcore.EventBus
	clock   *testutil.Clock
	members *testutil.Memberships
	events  *testutil.EventRecorder
	keys    *testutil.KeySource
}

// newServiceFixture builds a fixture over a fresh in-memory KVStore. Tests
// that need to observe or replace the KVStore itself (ratelimit_test.go's
// fail-closed case) use newServiceFixtureWithKV instead.
func newServiceFixture(t *testing.T, extra ...Option) *serviceFixture {
	t.Helper()
	return newServiceFixtureWithKV(t, pkgcore.NewMemoryKVStore(), extra...)
}

func newServiceFixtureWithKV(t *testing.T, kv pkgcore.KVStore, extra ...Option) *serviceFixture {
	t.Helper()
	return newServiceFixtureOn(t, pkgcore.NewMemoryEventBus(), kv, extra...)
}

// newServiceFixtureOn is newServiceFixtureWithKV over an injected bus: the
// service is wired exactly the same way, but bus is the one the test
// provides, so a test can observe the service's own publishes (the sign-in
// tenant enumeration's audited system-context grant among them) or drive a
// publish failure the in-memory bus cannot produce.
func newServiceFixtureOn(t *testing.T, bus pkgcore.EventBus, kv pkgcore.KVStore, extra ...Option) *serviceFixture {
	t.Helper()

	db := testutil.NewDB(t)
	clock := testutil.NewClock(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	members := testutil.NewMemberships()

	keys := testutil.NewKeySource(t, "kid-test")

	events := testutil.NewEventRecorder()
	events.Subscribe(bus, EventUserCreated, EventUserLoggedIn, EventLoginFailed,
		EventSessionRevoked, EventSessionReplayDetected, EventTenantSwitched,
		EventIdentityBound, EventIdentityUnbound, EventMFAEnrolled, EventMFARecoveryCodesRegenerated)

	opts := append([]Option{
		WithKeySource(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithMembershipReader(members),
		WithClock(clock.Now),
		WithPasswordParams(testParams()),
	}, extra...)

	svc, err := NewService(db, bus, kv, opts...)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return &serviceFixture{svc: svc, db: db, kv: kv, bus: bus, clock: clock, members: members, events: events, keys: keys}
}

// registerUser creates an account and records its membership of tenants.
func (f *serviceFixture) registerUser(t *testing.T, email string, tenants ...pkgcore.TenantID) *User {
	t.Helper()
	user, err := f.svc.Register(t.Context(), RegisterInput{
		Email: email, Password: testPassword, DisplayName: "Test User",
	})
	if err != nil {
		t.Fatalf("Register(%s) error = %v", email, err)
	}
	f.members.Add(user.ID, tenants...)
	return user
}

func TestService_RegisterAndLogin(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "signin@example.com", testTenantA)

	if n := f.events.Count(EventUserCreated); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventUserCreated)
	}

	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "SignIn@Example.com", Password: testPassword, IP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if pair.Principal.UserID != user.ID {
		t.Errorf("principal user = %s, want %s", pair.Principal.UserID, user.ID)
	}
	if pair.Principal.TenantID != testTenantA {
		t.Errorf("principal tenant = %q, want %q (the user's only membership)", pair.Principal.TenantID, testTenantA)
	}
	if pair.Principal.Email != "signin@example.com" {
		t.Errorf("principal email = %q, want the stored address", pair.Principal.Email)
	}
	if pair.RefreshToken == "" {
		t.Error("Login() returned no refresh token")
	}

	verified, err := f.svc.Verifier().Verify(t.Context(), pair.AccessToken)
	if err != nil {
		t.Fatalf("the access token Login() issued does not verify: %v", err)
	}
	if verified.SessionID != pair.Principal.SessionID {
		t.Errorf("token sid = %q, want %q", verified.SessionID, pair.Principal.SessionID)
	}
	if !reflect.DeepEqual(verified.AMR, []string{MethodPassword}) {
		t.Errorf("token amr = %v, want [password]", verified.AMR)
	}

	if n := f.events.Count(EventUserLoggedIn); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventUserLoggedIn)
	}
	attempts, err := f.svc.LoginHistory().ListByUser(t.Context(), user.ID, 10)
	if err != nil {
		t.Fatalf("ListByUser() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0].Result != LoginResultSuccess {
		t.Errorf("login history = %+v, want one successful attempt", attempts)
	}
}

// TestService_Login_TruncatesOverWidthClientStringsToTheColumnWidths pins
// the write boundary for client-supplied strings: sessions.device
// (VARCHAR(255)) and sessions.user_agent / login_attempts.user_agent
// (VARCHAR(512)) are the widths the migrations declare, and the two
// dialects disagree about enforcement -- PostgreSQL refuses an over-width
// write (SQLSTATE 22001) where SQLite stores it, so the SAME login with an
// over-width device or user agent would succeed on one dialect and fail on
// the other. The repository write boundary truncates the client-supplied
// strings to the columns' widths, so both dialects store the identical
// value. The exact truncated prefix is asserted, not merely a length:
// truncation must keep the value's HEAD (the part that identifies the
// device), cut at a rune boundary, and never touch a within-width value.
func TestService_Login_TruncatesOverWidthClientStringsToTheColumnWidths(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "trunc@example.com", testTenantA)
	// A separate account for the failed-attempt leg: a wrong password puts
	// its account into the module's progressive lockout (30s after one
	// failure), which must not stand between this test and the correct
	// logins below.
	failureUser := f.registerUser(t, "truncfail@example.com")

	overWidthDevice := strings.Repeat("d", deviceColumnWidth+50)
	overWidthUA := strings.Repeat("u", userAgentColumnWidth+50)

	// A FAILED attempt carries the user agent too (recordFailure), through
	// the same repository boundary as the successful one below.
	if _, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "truncfail@example.com", Password: "definitely not the password",
		UserAgent: overWidthUA, IP: "203.0.113.61",
	}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login(wrong password) error = %v, want ErrInvalidCredentials", err)
	}

	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "trunc@example.com", Password: testPassword,
		Device: overWidthDevice, UserAgent: overWidthUA, IP: "203.0.113.62",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	session, err := f.svc.sessionRepo.FindByID(t.Context(), pair.Principal.SessionID)
	if err != nil {
		t.Fatalf("FindByID(session) error = %v", err)
	}
	if want := strings.Repeat("d", deviceColumnWidth); session.Device != want {
		t.Errorf("session.Device = %d runes, want %d (truncated to sessions.device's VARCHAR width)", utf8.RuneCountInString(session.Device), deviceColumnWidth)
	}
	if want := strings.Repeat("u", userAgentColumnWidth); session.UserAgent != want {
		t.Errorf("session.UserAgent = %d runes, want %d (truncated to sessions.user_agent's VARCHAR width)", utf8.RuneCountInString(session.UserAgent), userAgentColumnWidth)
	}

	assertAttempts := func(userID string, wantCount int) {
		t.Helper()
		attempts, listErr := f.svc.LoginHistory().ListByUser(t.Context(), userID, 0)
		if listErr != nil {
			t.Fatalf("ListByUser() error = %v", listErr)
		}
		if len(attempts) != wantCount {
			t.Fatalf("login history has %d rows, want %d", len(attempts), wantCount)
		}
		for i, attempt := range attempts {
			if want := strings.Repeat("u", userAgentColumnWidth); attempt.UserAgent != want {
				t.Errorf("attempt[%d].UserAgent = %d runes, want %d (truncated to login_attempts.user_agent's VARCHAR width)", i, utf8.RuneCountInString(attempt.UserAgent), userAgentColumnWidth)
			}
		}
	}
	assertAttempts(user.ID, 1)
	assertAttempts(failureUser.ID, 1)

	// The truncated boundary values themselves must still round-trip: a
	// value exactly AT the width is never cut.
	exact, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "trunc@example.com", Password: testPassword,
		Device: strings.Repeat("d", deviceColumnWidth), UserAgent: strings.Repeat("u", userAgentColumnWidth),
		IP: "203.0.113.63",
	})
	if err != nil {
		t.Fatalf("Login(at-width values) error = %v", err)
	}
	exactSession, err := f.svc.sessionRepo.FindByID(t.Context(), exact.Principal.SessionID)
	if err != nil {
		t.Fatalf("FindByID(at-width session) error = %v", err)
	}
	if exactSession.Device != strings.Repeat("d", deviceColumnWidth) || exactSession.UserAgent != strings.Repeat("u", userAgentColumnWidth) {
		t.Error("at-width values were altered by the write boundary; only OVER-width values may be truncated")
	}
}

// TestService_Login_DoesNotDistinguishFailureCauses is the enumeration
// property, stated as a byte-for-byte comparison of what the caller sees.
// An endpoint that answers differently for "no such account" and "wrong
// password" tells an attacker which addresses are registered without their
// ever guessing a password.
func TestService_Login_DoesNotDistinguishFailureCauses(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	known := f.registerUser(t, "known@example.com", testTenantA)

	suspended := f.registerUser(t, "suspended@example.com", testTenantA)
	stored, err := f.svc.Users().FindByID(t.Context(), suspended.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	stored.Status = UserStatusSuspended
	if saveErr := f.svc.Users().Save(t.Context(), stored); saveErr != nil {
		t.Fatalf("Save() error = %v", saveErr)
	}

	noPassword := &User{Email: "social-only@example.com", Status: UserStatusActive}
	if createErr := f.svc.Users().Create(t.Context(), noPassword); createErr != nil {
		t.Fatalf("Create() error = %v", createErr)
	}
	f.members.Add(noPassword.ID, testTenantA)

	// A correct password whose account resolves no membership belongs in the
	// same indistinguishable family: the membership question is only reached
	// AFTER the password verified, so an answer distinguishing it from a
	// wrong password would certify the credential -- the oracle this test
	// exists to forbid. registerUser attaches no membership at all here.
	memberless := f.registerUser(t, "memberless@example.com")

	cases := []struct {
		name       string
		identifier string
		password   string
		wantReason string
	}{
		{name: "unknown account", identifier: "nobody@example.com", password: testPassword, wantReason: FailureReasonUnknownUser},
		{name: "wrong password", identifier: "known@example.com", password: "not the password at all", wantReason: FailureReasonBadPassword},
		{name: "account with no password", identifier: "social-only@example.com", password: testPassword, wantReason: FailureReasonNoPassword},
		{name: "suspended account", identifier: "suspended@example.com", password: testPassword, wantReason: FailureReasonSuspended},
		{name: "correct password with no membership", identifier: "memberless@example.com", password: testPassword, wantReason: FailureReasonNoMembership},
		{name: "identifier with no canonical form", identifier: "   ", password: testPassword, wantReason: FailureReasonUnknownUser},
		{name: "malformed phone identifier", identifier: "1380000", password: testPassword, wantReason: FailureReasonUnknownUser},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, loginErr := f.svc.Login(t.Context(), LoginInput{Identifier: tc.identifier, Password: tc.password})

			appErr, ok := asAppError(loginErr)
			if !ok {
				t.Fatalf("Login() error = %v, want an *apperr.Error", loginErr)
			}
			if !apperr.HasCode(loginErr, ErrInvalidCredentials.Code) {
				t.Fatalf("code = %q, want %q for every failure cause", appErr.Code, ErrInvalidCredentials.Code)
			}
			if appErr.Status != ErrInvalidCredentials.Status {
				t.Errorf("status = %d, want %d", appErr.Status, ErrInvalidCredentials.Status)
			}
			if len(appErr.Params) != 0 {
				t.Errorf("params = %v, want none: a parameter would leak which cause it was", appErr.Params)
			}
		})
	}

	// The distinguishing detail exists, but only in the history row.
	attempts, err := f.svc.LoginHistory().ListByUser(t.Context(), known.ID, 10)
	if err != nil {
		t.Fatalf("ListByUser() error = %v", err)
	}
	if len(attempts) != 1 || attempts[0].FailureReason != FailureReasonBadPassword {
		t.Errorf("login history = %+v, want one bad-password failure", attempts)
	}
	memberlessAttempts, err := f.svc.LoginHistory().ListByUser(t.Context(), memberless.ID, 10)
	if err != nil {
		t.Fatalf("ListByUser() error = %v", err)
	}
	if len(memberlessAttempts) != 1 || memberlessAttempts[0].FailureReason != FailureReasonNoMembership {
		t.Errorf("memberless login history = %+v, want one no-membership failure", memberlessAttempts)
	}
	if n := f.events.Count(EventLoginFailed); n != len(cases) {
		t.Errorf("recorded %d %s events, want %d", n, EventLoginFailed, len(cases))
	}
}

// TestService_Login_NoMembershipIsIndistinguishableFromWrongPassword pins
// the password-confirmation boundary: the login endpoint must not answer an
// anonymous attacker whether the password was right. A wrong password
// answers 401 authn.invalid_credentials; a correct password whose account
// resolved no membership -- no membership anywhere, or none of an
// explicitly requested tenant -- must answer the same, never 403
// authn.tenant_membership_*. Reaching the membership question at all means
// every earlier check passed, so a distinguishable answer would certify the
// credential -- and in a generated project whose membership seam is not
// wired (nil reader) it would do so for EVERY correct password, every
// attempt.
func TestService_Login_NoMembershipIsIndistinguishableFromWrongPassword(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "memberless@example.com") // no membership anywhere
	// Each of the three legs below runs on its own account: the legs are
	// distinct scenarios (no membership anywhere; a tenant-A member asking
	// for tenant B; a wrong password), an address can be registered only
	// once, and the assertions below count the rows each leg's failure
	// leaves in its own account's login history -- the memberless history
	// assertion pins exactly one. The lockout mechanics are worth stating
	// exactly too, because they differ by leg: the two membership-guard
	// failures write their login-history row (recordFailure,
	// FailureReasonNoMembership) and deliberately stop there, never
	// calling guard.RecordLoginFailure -- a correct password is not a
	// credential-guessing failure (see the resolveTenant branch in
	// service.go's login) -- so those accounts' progressive lockout state
	// is untouched, and a repeat attempt on one of them is not refused
	// with authn.account_locked; it re-verifies the password and records
	// again. Only the wrong-password leg feeds the lockout, and on real
	// wall-clock time: RecordLoginFailure proposes time.Now-based
	// deadlines and never sees the fixture's injected clock, so this
	// suite's frozen clock neither starts nor expires a lockout. Within
	// the 30s base delay (loginLockoutBase) a second wrong password on
	// the same account is refused at CheckLogin with authn.account_locked,
	// before the password work and therefore before the membership
	// question.
	f.registerUser(t, "elsewhere@example.com", testTenantA)
	f.registerUser(t, "known@example.com", testTenantA)

	// A correct password whose account has no membership anywhere must
	// answer byte-identically to a wrong password: same code, same status,
	// same absence of parameters.
	_, noMembershipErr := f.svc.Login(t.Context(), LoginInput{
		Identifier: "memberless@example.com", Password: testPassword,
	})
	if noMembershipErr == nil {
		t.Fatal("Login() succeeded for an account with no resolvable membership")
	}

	// The same fold for a correct password naming a tenant the account has
	// no membership of -- never the 403 tenant_membership_required shape a
	// member of another tenant would otherwise draw for the wrong tenant.
	_, wrongTenantErr := f.svc.Login(t.Context(), LoginInput{
		Identifier: "elsewhere@example.com", Password: testPassword, TenantID: testTenantB,
	})
	if wrongTenantErr == nil {
		t.Fatal("Login(correct password, requested tenant without membership) succeeded")
	}
	if !apperr.HasCode(wrongTenantErr, ErrInvalidCredentials.Code) {
		t.Fatalf("wrong-tenant login error = %v, want %q", wrongTenantErr, ErrInvalidCredentials.Code)
	}

	_, wrongPasswordErr := f.svc.Login(t.Context(), LoginInput{
		Identifier: "known@example.com", Password: "not the password at all",
	})
	if wrongPasswordErr == nil {
		t.Fatal("Login(wrong password) succeeded")
	}
	a, aOK := asAppError(noMembershipErr)
	b, bOK := asAppError(wrongPasswordErr)
	if !aOK || !bOK {
		t.Fatalf("Login() errors = %v and %v, want two *apperr.Error values", noMembershipErr, wrongPasswordErr)
	}
	if a.Code != ErrInvalidCredentials.Code {
		t.Fatalf("membership-less login code = %q, want %q", a.Code, ErrInvalidCredentials.Code)
	}
	if a.Code != b.Code || a.Status != b.Status || len(a.Params) != len(b.Params) {
		t.Fatalf("membership-less login error = %v, wrong-password login error = %v, "+
			"want externally identical answers", a, b)
	}

	// The real cause still lands in the login history -- no operational
	// information is lost by the uniform answer.
	memberless, findErr := f.svc.Users().FindByEmail(t.Context(), "memberless@example.com")
	if findErr != nil {
		t.Fatalf("FindByEmail() error = %v", findErr)
	}
	attempts, listErr := f.svc.LoginHistory().ListByUser(t.Context(), memberless.ID, 10)
	if listErr != nil {
		t.Fatalf("ListByUser() error = %v", listErr)
	}
	if len(attempts) != 1 || attempts[0].Result != LoginResultFailure ||
		attempts[0].FailureReason != FailureReasonNoMembership {
		t.Errorf("memberless login history = %+v, want one no-membership failure", attempts)
	}
	if n := f.events.Count(EventLoginFailed); n != 3 {
		t.Errorf("recorded %d %s events, want 3", n, EventLoginFailed)
	}
}

// TestService_Login_RecordsAnAttemptWithoutTheIdentifier proves the login
// history counts attempts per address without storing the address, which is
// what keeps an unmatched attempt from writing a stranger's email into this
// deployment's database.
func TestService_Login_RecordsAnAttemptWithoutTheIdentifier(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "stranger@example.com", Password: testPassword}); err == nil {
		t.Fatal("Login() error = nil for an unknown account")
	}

	var stored []LoginAttempt
	if err := f.db.Find(&stored).Error; err != nil {
		t.Fatalf("read login_attempts: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("recorded %d attempts, want 1", len(stored))
	}
	if stored[0].UserID != "" {
		t.Errorf("user_id = %q, want empty for an identifier that matched no account", stored[0].UserID)
	}
	if stored[0].IdentifierIndex == "" {
		t.Error("identifier_index is empty; the attempt cannot be counted per address")
	}

	wantIndex, err := f.svc.Users().EmailIndexOf("stranger@example.com")
	if err != nil {
		t.Fatalf("EmailIndexOf() error = %v", err)
	}
	if stored[0].IdentifierIndex != wantIndex {
		t.Error("identifier_index does not match the blind index of the attempted address")
	}
	if stored[0].IdentifierIndex == "stranger@example.com" {
		t.Error("the attempted address was stored in plaintext")
	}
}

func TestService_Register_Validation(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "taken@example.com", testTenantA)

	cases := []struct {
		name     string
		in       RegisterInput
		wantCode string
	}{
		{name: "no identifier", in: RegisterInput{Password: testPassword}, wantCode: ErrIdentifierRequired.Code},
		{name: "password too short", in: RegisterInput{Email: "short@example.com", Password: "abc"}, wantCode: ErrPasswordTooShort.Code},
		{name: "denylisted password", in: RegisterInput{Email: "weak@example.com", Password: "password1234"}, wantCode: ErrPasswordTooWeak.Code},
		{name: "unusable email", in: RegisterInput{Email: "  ", Phone: "", Password: testPassword}, wantCode: ErrIdentifierRequired.Code},
		{name: "unusable phone", in: RegisterInput{Phone: "not-a-number", Password: testPassword}, wantCode: ErrInvalidPhone.Code},
		{name: "duplicate email", in: RegisterInput{Email: "Taken@Example.com", Password: testPassword}, wantCode: ErrEmailAlreadyRegistered.Code},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Register(t.Context(), tc.in)
			if !apperr.HasCode(err, tc.wantCode) {
				t.Fatalf("Register() error = %v, want code %q", err, tc.wantCode)
			}
		})
	}
}

func TestService_Register_DuplicatePhoneIsRejected(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	if _, err := f.svc.Register(t.Context(), RegisterInput{Phone: "+8613800000000", Password: testPassword}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	_, err := f.svc.Register(t.Context(), RegisterInput{Phone: "+86 138 0000 0000", Password: testPassword})
	if !apperr.HasCode(err, ErrPhoneAlreadyRegistered.Code) {
		t.Fatalf("Register(same number, different formatting) error = %v, want code %q", err, ErrPhoneAlreadyRegistered.Code)
	}
}

// TestService_Login_FailsClosedWithoutAMembershipReader is the fail-closed
// rule: an unanswerable "may this person act inside this tenant" is a
// refusal, never a default. The refusal's SHAPE differs by who is asking:
// tenant switching answers an already-authenticated caller and fails closed
// with ErrTenantMembershipUnavailable, while password sign-in answers an
// anonymous caller and folds the same refusal into its uniform
// ErrInvalidCredentials answer -- a distinguishable membership error there
// would certify the password the caller just tried (see Login's doc comment).
func TestService_Login_FailsClosedWithoutAMembershipReader(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "closed@example.com", testTenantA)

	// A real session first, because SwitchTenant checks that the session
	// exists and belongs to the caller before it asks anything about
	// tenants -- an authorization question is only worth asking once the
	// subject is established.
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "closed@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	// Now the same database through a service wired with no reader at all,
	// which is what a host that forgot to inject one produces.
	unwired, err := NewService(f.db, pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(),
		WithKeySource(f.keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithClock(f.clock.Now),
		WithPasswordParams(testParams()),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	// The sign-in still refuses -- never a permissive default -- but with
	// the collapsed credential answer, never the distinguishable 403 a
	// memberless-correct-password would draw from this nil-reader wiring.
	_, err = unwired.Login(t.Context(), LoginInput{Identifier: "closed@example.com", Password: testPassword})
	if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("Login() error = %v, want code %q", err, ErrInvalidCredentials.Code)
	}

	// The authenticated path keeps the distinguishable fail-closed answer.
	_, err = unwired.SwitchTenant(t.Context(), pair.Principal, testTenantA)
	if !apperr.HasCode(err, ErrTenantMembershipUnavailable.Code) {
		t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrTenantMembershipUnavailable.Code)
	}
}

// TestService_Login_FailsClosedWhenMembershipCannotBeRead covers the other
// unanswerable case: the reader exists but errors. The sign-in refuses
// rather than defaulting, and the refusal is the uniform credential answer
// -- the login path never answers an anonymous caller with a membership
// error, whatever the membership question's own fate was (the reader's
// failure is still logged by resolveTenant for the operator, and the
// attempt still lands in the login history under FailureReasonNoMembership).
func TestService_Login_FailsClosedWhenMembershipCannotBeRead(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "broken@example.com", testTenantA)
	f.members.FailWith(errors.New("the membership store is down"))

	_, err := f.svc.Login(t.Context(), LoginInput{Identifier: "broken@example.com", Password: testPassword})
	if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("Login() error = %v, want code %q", err, ErrInvalidCredentials.Code)
	}
}

func TestService_Login_TenantSelection(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "member@example.com", testTenantB, testTenantA)

	t.Run("no requested tenant uses the first membership", func(t *testing.T) {
		pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "member@example.com", Password: testPassword})
		if err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		if pair.Principal.TenantID != testTenantB {
			t.Errorf("tenant = %q, want the first membership %q", pair.Principal.TenantID, testTenantB)
		}
	})

	t.Run("a requested tenant the user belongs to is honoured", func(t *testing.T) {
		pair, err := f.svc.Login(t.Context(), LoginInput{
			Identifier: "member@example.com", Password: testPassword, TenantID: testTenantA,
		})
		if err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		if pair.Principal.TenantID != testTenantA {
			t.Errorf("tenant = %q, want %q", pair.Principal.TenantID, testTenantA)
		}
	})

	t.Run("a requested tenant the user does not belong to is refused", func(t *testing.T) {
		_, err := f.svc.Login(t.Context(), LoginInput{
			Identifier: "member@example.com", Password: testPassword, TenantID: pkgcore.TenantID("someone-elses-tenant"),
		})
		// Refused with the uniform credential answer, never the 403
		// membership errors tenant switching gives an authenticated caller:
		// this refusal happens only after the password verified, so a
		// distinguishable answer would certify it (see Login's doc comment).
		if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
			t.Fatalf("Login() error = %v, want code %q", err, ErrInvalidCredentials.Code)
		}
	})

	t.Run("a user with no membership at all cannot sign in", func(t *testing.T) {
		f.registerUser(t, "orphan@example.com")
		_, err := f.svc.Login(t.Context(), LoginInput{Identifier: "orphan@example.com", Password: testPassword})
		if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
			t.Fatalf("Login() error = %v, want code %q", err, ErrInvalidCredentials.Code)
		}
	})
}

// TestService_SwitchTenant_ReusesTheSessionAndItsRefreshToken pins the design
// rule that switching tenants is not a new sign-in: the same session, the
// same refresh-token family, only a new access token.
func TestService_SwitchTenant_ReusesTheSessionAndItsRefreshToken(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "switcher@example.com", testTenantA, testTenantB)

	first, err := f.svc.Login(t.Context(), LoginInput{Identifier: "switcher@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	f.clock.Advance(time.Minute)
	switched, err := f.svc.SwitchTenant(t.Context(), first.Principal, testTenantB)
	if err != nil {
		t.Fatalf("SwitchTenant() error = %v", err)
	}

	if switched.Principal.SessionID != first.Principal.SessionID {
		t.Errorf("session changed on a tenant switch: %s -> %s", first.Principal.SessionID, switched.Principal.SessionID)
	}
	if switched.RefreshToken != "" {
		t.Error("SwitchTenant() minted a new refresh token; the caller keeps the one it has")
	}
	if switched.AccessToken == first.AccessToken {
		t.Fatal("SwitchTenant() returned the same access token")
	}

	verified, err := f.svc.Verifier().Verify(t.Context(), switched.AccessToken)
	if err != nil {
		t.Fatalf("the switched access token does not verify: %v", err)
	}
	if verified.TenantID != testTenantB {
		t.Errorf("token tenant = %q, want %q", verified.TenantID, testTenantB)
	}

	if n := f.events.Count(EventTenantSwitched); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventTenantSwitched)
	}

	// The refresh token the caller already holds must now mint tokens for
	// the tenant it switched to.
	refreshed, err := f.svc.Refresh(t.Context(), first.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if refreshed.Principal.TenantID != testTenantB {
		t.Errorf("refreshed tenant = %q, want the switched-to tenant %q", refreshed.Principal.TenantID, testTenantB)
	}
}

// TestService_SwitchTenant_RefusesATenantTheUserDoesNotBelongTo covers the
// horizontal-privilege-escalation entry point: the target tenant arrives from
// the client, and it is checked rather than trusted.
func TestService_SwitchTenant_RefusesATenantTheUserDoesNotBelongTo(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "confined@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "confined@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	cases := []struct {
		name     string
		target   pkgcore.TenantID
		wantCode string
	}{
		{name: "someone else's tenant", target: testTenantB, wantCode: ErrTenantMembershipRequired.Code},
		{name: "no tenant at all", target: "", wantCode: ErrTenantMembershipRequired.Code},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.svc.SwitchTenant(t.Context(), pair.Principal, tc.target); !apperr.HasCode(err, tc.wantCode) {
				t.Fatalf("SwitchTenant() error = %v, want code %q", err, tc.wantCode)
			}
		})
	}

	t.Run("a session belonging to another user", func(t *testing.T) {
		other := f.registerUser(t, "other@example.com", testTenantA)
		forged := Principal{UserID: other.ID, SessionID: pair.Principal.SessionID, TenantID: testTenantA}
		if _, err := f.svc.SwitchTenant(t.Context(), forged, testTenantA); !apperr.HasCode(err, ErrTokenInvalid.Code) {
			t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrTokenInvalid.Code)
		}
	})

	t.Run("an unauthenticated principal", func(t *testing.T) {
		if _, err := f.svc.SwitchTenant(t.Context(), Principal{}, testTenantA); !apperr.HasCode(err, ErrAuthenticationRequired.Code) {
			t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrAuthenticationRequired.Code)
		}
	})
}

// TestService_SwitchTenant_RefusesAnExpiredSession pins the expiry half of
// SwitchTenant's session check: a session past its own configured TTL --
// whose Status row nothing here ever proactively flips away from active --
// must refuse the switch, whatever the caller's already-issued access
// token still says. A switch that only consulted session.Status would
// extend the session's practical lifetime by one access-token lifetime.
func TestService_SwitchTenant_RefusesAnExpiredSession(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "switch-after-expiry@example.com", testTenantA, testTenantB)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "switch-after-expiry@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	testutil.ExpireSession(t, f.db, pair.Principal.SessionID, f.clock.Now().Add(-time.Hour))

	if _, err := f.svc.SwitchTenant(t.Context(), pair.Principal, testTenantB); !apperr.HasCode(err, ErrSessionRevoked.Code) {
		t.Fatalf("SwitchTenant(expired session) error = %v, want code %q", err, ErrSessionRevoked.Code)
	}
}

// TestService_SwitchTenant_StillValidSessionSucceeds guards against
// over-refusing: a session that has not yet reached its own ExpiresAt must
// keep switching tenants.
func TestService_SwitchTenant_StillValidSessionSucceeds(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "switch-still-valid@example.com", testTenantA, testTenantB)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "switch-still-valid@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	f.clock.Advance(time.Minute)
	if _, err := f.svc.SwitchTenant(t.Context(), pair.Principal, testTenantB); err != nil {
		t.Fatalf("SwitchTenant(still-valid session) error = %v, want success", err)
	}
}

// wedgeMembershipReader is a MembershipReader that parks the caller inside
// its ActiveMembership answer for a chosen tenant until the test releases it.
// It is the deterministic wedge TestService_SwitchTenant_ConcurrentRevoke_
// IsRefused needs: SwitchTenant reads the session, then asks membership, then
// writes the new tenant -- so parking the membership answer puts the switch
// exactly between its read and its compare-and-swap while the test revokes
// the session in that window.
type wedgeMembershipReader struct {
	inner    MembershipReader
	blockOn  pkgcore.TenantID
	entered  chan struct{}
	release  chan struct{}
	released bool
}

// ActiveMembership implements MembershipReader.
func (w *wedgeMembershipReader) ActiveMembership(ctx context.Context, userID string, tenantID pkgcore.TenantID) (bool, error) {
	if tenantID == w.blockOn && !w.released {
		select {
		case w.entered <- struct{}{}:
		default:
		}
		select {
		case <-w.release:
		case <-time.After(15 * time.Second):
			// A safety bound so a botched test cannot hang the suite; the
			// test itself always releases well before this.
		}
		w.released = true
	}
	return w.inner.ActiveMembership(ctx, userID, tenantID)
}

// TenantsOf implements MembershipReader.
func (w *wedgeMembershipReader) TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	return w.inner.TenantsOf(ctx, userID)
}

// TestService_SwitchTenant_ConcurrentRevoke_IsRefused pins the
// compare-and-swap report on the switch path: SetCurrentTenant must report
// whether its UPDATE matched a row, because a revoke landing between
// SwitchTenant's read of the session and its tenant update makes the UPDATE
// match zero rows -- if the caller ignored that, it would mint a
// full-lifetime token pair for a session a concurrent sign-out had just
// killed, and a revoked session would keep issuing tokens. The wedge parks
// the switch between its read and its write (both sides of the race run for
// real against the same database) and proves the revoke wins: the switch
// must answer ErrSessionRevoked and mint nothing.
func TestService_SwitchTenant_ConcurrentRevoke_IsRefused(t *testing.T) {
	t.Parallel()

	wedge := &wedgeMembershipReader{
		blockOn: testTenantB,
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	f := newServiceFixture(t, WithMembershipReader(wedge))
	wedge.inner = f.members

	f.registerUser(t, "concurrent-revoke@example.com", testTenantA, testTenantB)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "concurrent-revoke@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	switched := make(chan error, 1)
	go func() {
		_, switchErr := f.svc.SwitchTenant(t.Context(), pair.Principal, testTenantB)
		switched <- switchErr
	}()

	select {
	case <-wedge.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("SwitchTenant() never reached the membership check; the wedge did not engage")
	}

	if revokeErr := f.svc.Sessions().Revoke(t.Context(), pair.Principal.SessionID, RevokeReasonLogout); revokeErr != nil {
		t.Fatalf("Revoke() error = %v", revokeErr)
	}
	close(wedge.release)

	raceErr := <-switched
	if !apperr.HasCode(raceErr, ErrSessionRevoked.Code) {
		t.Fatalf("SwitchTenant() racing a revoke error = %v, want code %q (a revoked session must not mint tokens)", raceErr, ErrSessionRevoked.Code)
	}
	if n := f.events.Count(EventTenantSwitched); n != 0 {
		t.Errorf("recorded %d %s events for a refused switch, want 0", n, EventTenantSwitched)
	}

	stored, err := f.svc.sessionRepo.FindByID(t.Context(), pair.Principal.SessionID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if stored.Status != SessionStatusRevoked {
		t.Errorf("stored status = %q, want %q (the revoke must have won)", stored.Status, SessionStatusRevoked)
	}
}

// TestService_Refresh_ReverifiesMembership is what makes removing someone from
// a tenant actually end their access to it, instead of leaving them signed in
// until the session expires weeks later.
func TestService_Refresh_ReverifiesMembership(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "removed@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "removed@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	f.clock.Advance(time.Minute)
	refreshed, err := f.svc.Refresh(t.Context(), pair.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	f.members.Remove(user.ID, testTenantA)

	f.clock.Advance(time.Minute)
	if _, err := f.svc.Refresh(t.Context(), refreshed.RefreshToken); !apperr.HasCode(err, ErrTenantMembershipRequired.Code) {
		t.Fatalf("Refresh() after removal error = %v, want code %q", err, ErrTenantMembershipRequired.Code)
	}
}

func TestService_Refresh_RefusesASuspendedAccount(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "gets-suspended@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "gets-suspended@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	stored, err := f.svc.Users().FindByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	stored.Status = UserStatusSuspended
	if err := f.svc.Users().Save(t.Context(), stored); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	f.clock.Advance(time.Minute)
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !apperr.HasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("Refresh() error = %v, want code %q", err, ErrInvalidCredentials.Code)
	}
}

// TestService_Refresh_TransientMembershipFailureDoesNotConsumeTheToken pins
// the sequencing boundary: refresh must re-verify membership BEFORE
// rotating (consuming) the presented refresh token. Rotating first would
// leave a transient MembershipReader failure with the presented token
// permanently spent even though the caller never received its replacement;
// the client's own, entirely legitimate retry with that same token would
// then hit the replay detector -- exactly correct behaviour for an
// actually-replayed token, but wrong here -- and revoke the whole
// refresh-token family and the session and fire
// EventSessionReplayDetected, all over what was really a backend hiccup. A
// two-second MembershipReader outage must not get to look identical to a
// stolen refresh token.
func TestService_Refresh_TransientMembershipFailureDoesNotConsumeTheToken(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "flaky-membership@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "flaky-membership@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	f.members.FailWith(errors.New("membership store timeout"))
	f.clock.Advance(time.Minute)

	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !apperr.HasCode(err, ErrTenantMembershipUnavailable.Code) {
		t.Fatalf("Refresh() during a membership outage error = %v, want code %q", err, ErrTenantMembershipUnavailable.Code)
	}

	// There was only ever one presentation of this token. Nothing about a
	// slow membership store makes it a stolen credential, so neither the
	// replay event nor a session revocation may have fired.
	if n := f.events.Count(EventSessionReplayDetected); n != 0 {
		t.Fatalf("EventSessionReplayDetected fired %d times after a transient membership failure, want 0", n)
	}
	if n := f.events.Count(EventSessionRevoked); n != 0 {
		t.Fatalf("EventSessionRevoked fired %d times after a transient membership failure, want 0", n)
	}

	// The store recovers, and the client retries with the SAME token it was
	// never issued a replacement for (it never received one, because the
	// refresh that would have carried it failed). That retry must still
	// work -- the failed attempt above must not have spent the token.
	f.members.FailWith(nil)
	f.clock.Advance(time.Minute)

	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("Refresh() with the same token after the outage cleared, error = %v, want success", err)
	}
}

// TestService_Refresh_ActualReplayStillRevokesTheFamily is the companion
// guard: an actually-replayed refresh token -- one already rotated by a
// prior, successful call -- must still be caught, still revoke the whole
// family and session, and still announce EventSessionReplayDetected.
// Running refresh's re-verification ahead of rotation must not weaken this
// real security property.
func TestService_Refresh_ActualReplayStillRevokesTheFamily(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "replay-victim@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "replay-victim@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	f.clock.Advance(time.Minute)
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); err != nil {
		t.Fatalf("first Refresh() error = %v", err)
	}

	// Present the SAME, now-already-rotated token again: a genuine replay,
	// not a retry after a server-side failure.
	f.clock.Advance(time.Minute)
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !apperr.HasCode(err, ErrRefreshTokenReused.Code) {
		t.Fatalf("replayed Refresh() error = %v, want code %q", err, ErrRefreshTokenReused.Code)
	}

	if n := f.events.Count(EventSessionReplayDetected); n != 1 {
		t.Fatalf("EventSessionReplayDetected fired %d times for an actual replay, want 1", n)
	}
	if n := f.events.Count(EventSessionRevoked); n != 1 {
		t.Fatalf("EventSessionRevoked fired %d times for an actual replay, want 1", n)
	}
}

// TestService_Refresh_ActualReplay_LeavesADurableAuditRecord pins the
// durable record a detected refresh-token replay must leave: the module
// audits an ordinary wrong password, so a detected credential theft must
// leave a trace anyone can query afterwards too. It drives the real service
// path -- register, sign in, refresh (which rotates the family), then
// present the now-consumed token again -- through the same construction a
// host uses (a Module registered on a real pkgcore.Registry, exactly
// module.go's Register runs in production, so the registrar wiring under
// test is the real one), and asserts the response lands in the audit trail:
// an authn.session.revoke record with Success=false and RevokeReasonReplay
// as the FailureReason, attributed to the account owner and stamped with
// the tenant the revoked session acted in -- the same shape that tells the
// record apart from an owner-initiated logout, which records Success=true
// under the same action. The detection's own behavior (family rotation,
// session revocation, the two events) is pinned here too, so the record
// cannot be bought by weakening it: if nothing durable were emitted, no
// EventRecorded event would reach the bus at all.
//
// The replay path also writes no login-attempt row -- refresh is not a
// sign-in attempt -- pinned by the history-count assertion below (the one
// row is the password sign-in itself, and stays one through refresh and
// replay).
func TestService_Refresh_ActualReplay_LeavesADurableAuditRecord(t *testing.T) {
	t.Parallel()

	members := testutil.NewMemberships()
	module := newTestModule(t, WithMembershipReader(members))
	bus := pkgcore.NewMemoryEventBus()
	recorder := testutil.NewEventRecorder()
	recorder.Subscribe(bus, audit.EventRecorded, EventSessionReplayDetected, EventSessionRevoked)
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	svc := module.Service()

	user, err := svc.Register(t.Context(), RegisterInput{
		Email: "replay-audit@example.com", Password: testPassword, DisplayName: "Replay Victim",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, testTenantA)

	pair, err := svc.Login(t.Context(), LoginInput{Identifier: "replay-audit@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	history, err := svc.ListLoginHistory(t.Context(), user.ID, 100)
	if err != nil {
		t.Fatalf("ListLoginHistory() error = %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("login history holds %d rows after the sign-in, want 1", len(history))
	}

	// First refresh rotates the family; the second presents the now-consumed
	// token again -- a genuine replay, not a retry after a server-side
	// failure.
	if _, refreshErr := svc.Refresh(t.Context(), pair.RefreshToken); refreshErr != nil {
		t.Fatalf("first Refresh() error = %v", refreshErr)
	}
	if _, replayErr := svc.Refresh(t.Context(), pair.RefreshToken); !apperr.HasCode(replayErr, ErrRefreshTokenReused.Code) {
		t.Fatalf("replayed Refresh() error = %v, want code %q", replayErr, ErrRefreshTokenReused.Code)
	}

	// The detection keeps its own behavior: the security event fires
	// and the session's end is announced.
	if n := recorder.Count(EventSessionReplayDetected); n != 1 {
		t.Fatalf("EventSessionReplayDetected fired %d times for an actual replay, want 1", n)
	}
	if n := recorder.Count(EventSessionRevoked); n != 1 {
		t.Fatalf("EventSessionRevoked fired %d times for an actual replay, want 1", n)
	}

	// The replay leaves one audit row under the session-revoke action, in
	// the refused-presentation shape, attributable to the owner and
	// tenant-stamped -- present where nothing else would be, and
	// distinguishable from a logout.
	evt := findAuditEvent(t, recorder, AuditActionSessionRevoke)
	if evt.Result.Success {
		t.Errorf("Result.Success = true, want false: the replayed refresh was refused (401)")
	}
	if evt.Result.FailureReason != RevokeReasonReplay {
		t.Errorf("Result.FailureReason = %q, want %q (RevokeReasonReplay, the vocabulary the revoked session row itself carries)", evt.Result.FailureReason, RevokeReasonReplay)
	}
	if evt.Resource.Type != "session" || evt.Resource.ID != pair.Principal.SessionID {
		t.Errorf("Resource = %+v, want {Type: session, ID: %s}", evt.Resource, pair.Principal.SessionID)
	}
	if evt.Actor.Type != pkgcore.ActorTypeUser || evt.Actor.ID != user.ID {
		t.Errorf("Actor = %+v, want {Type: %s, ID: %s} (the account owner)", evt.Actor, pkgcore.ActorTypeUser, user.ID)
	}
	auditTenantIs(t, evt, pair.Principal.TenantID)

	// The replay added no login-attempt row: refresh is not a sign-in
	// attempt, whatever the state of its audit record.
	history, err = svc.ListLoginHistory(t.Context(), user.ID, 100)
	if err != nil {
		t.Fatalf("ListLoginHistory() error = %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("login history holds %d rows after refresh and replay, want 1 (the sign-in only)", len(history))
	}
}

func TestService_Logout_EndsTheSession(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "leaver@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "leaver@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	if err := f.svc.Logout(t.Context(), pair.Principal.SessionID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if n := f.events.Count(EventSessionRevoked); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventSessionRevoked)
	}

	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); err == nil {
		t.Fatal("the refresh token still works after sign-out")
	}

	if err := f.svc.Logout(t.Context(), ""); !apperr.HasCode(err, ErrAuthenticationRequired.Code) {
		t.Errorf("Logout(no session) error = %v, want code %q", err, ErrAuthenticationRequired.Code)
	}
}

// TestService_Login_UpgradesAStaleHash proves the corpus migrates on its own:
// a sign-in against a hash created under weaker parameters rewrites it under
// the current ones, at the one moment the plaintext is available and known to
// be correct.
func TestService_Login_UpgradesAStaleHash(t *testing.T) {
	t.Parallel()

	weak := PasswordParams{Memory: 32, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	f := newServiceFixture(t, WithPasswordParams(weak))
	user := f.registerUser(t, "upgrade@example.com", testTenantA)

	before, err := f.svc.Users().FindByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	stale, err := NeedsRehash(before.PasswordHash, testParams())
	if err != nil {
		t.Fatalf("NeedsRehash() error = %v", err)
	}
	if !stale {
		t.Fatal("the fixture did not produce a hash that needs upgrading")
	}

	// A second service over the same database, configured with the raised
	// parameters, exactly as a redeployment would be.
	stronger := newServiceOverDB(t, f, testParams())
	if _, upgradeErr := stronger.Login(t.Context(), LoginInput{Identifier: "upgrade@example.com", Password: testPassword}); upgradeErr != nil {
		t.Fatalf("Login() error = %v", upgradeErr)
	}

	after, err := stronger.Users().FindByID(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if after.PasswordHash == before.PasswordHash {
		t.Fatal("the stored hash was not upgraded after a successful sign-in")
	}
	stale, err = NeedsRehash(after.PasswordHash, testParams())
	if err != nil {
		t.Fatalf("NeedsRehash() error = %v", err)
	}
	if stale {
		t.Error("the upgraded hash still reports as stale")
	}

	// The old password must still work through the new hash.
	if _, againErr := stronger.Login(t.Context(), LoginInput{Identifier: "upgrade@example.com", Password: testPassword}); againErr != nil {
		t.Fatalf("Login() after the upgrade error = %v", againErr)
	}
}

// newServiceOverDB builds a second Service over the fixture's database and
// membership set, standing in for a redeployment with different parameters.
func newServiceOverDB(t *testing.T, f *serviceFixture, params PasswordParams) *Service {
	t.Helper()
	svc, err := NewService(f.db, pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(),
		WithKeySource(f.keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithMembershipReader(f.members),
		WithClock(f.clock.Now),
		WithPasswordParams(params),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return svc
}

func TestNewService_RejectsAnIncompleteWiring(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	bus := pkgcore.NewMemoryEventBus()
	kv := pkgcore.NewMemoryKVStore()

	keys := testutil.NewKeySource(t, "kid-test")
	good := []Option{WithKeySource(keys), WithBlindIndexKey(testutil.BlindIndexKey())}

	cases := []struct {
		name string
		db   *gorm.DB
		bus  pkgcore.EventBus
		kv   pkgcore.KVStore
		opts []Option
	}{
		{name: "no database", bus: bus, kv: kv, opts: good},
		{name: "no event bus", db: db, kv: kv, opts: good},
		{name: "no key-value store", db: db, bus: bus, opts: good},
		{name: "no signing keys", db: db, bus: bus, kv: kv, opts: []Option{WithBlindIndexKey(testutil.BlindIndexKey())}},
		{name: "no blind-index key", db: db, bus: bus, kv: kv, opts: []Option{WithKeySource(keys)}},
		{name: "blind-index key of the wrong length", db: db, bus: bus, kv: kv, opts: []Option{WithKeySource(keys), WithBlindIndexKey([]byte("too short"))}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewService(tc.db, tc.bus, tc.kv, tc.opts...); err == nil {
				t.Error("NewService() error = nil, want a rejection")
			}
		})
	}
}

// setupAuthMetricsMeterProvider installs, as OTel's global MeterProvider for
// the duration of the test, a real SDK MeterProvider backed by a
// ManualReader -- never a Prometheus/OTLP exporter, since this file only
// needs to read back exactly what was recorded -- mirroring
// go/jobs/standalone_queue_test.go's and go/notification/delivery_test.go's
// own helper of the same shape. Deliberately NOT called from a t.Parallel()
// test: it swaps the process-wide global otel MeterProvider, which is safe
// only while no OTHER test's Service is concurrently recording into it (see
// this test's own doc comment).
func setupAuthMetricsMeterProvider(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	otel.SetMeterProvider(mp)
	return reader
}

// collectAuthMetric runs a fresh Collect and returns the single metric named
// name, failing the test if it is missing -- name is always one of
// authCountMetricName/authDurationMetricName.
func collectAuthMetric(t *testing.T, reader *sdkmetric.ManualReader, name string) metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			got = append(got, m.Name)
		}
	}
	t.Fatalf("metric %q not found; metrics present: %v", name, got)
	return metricdata.Metrics{}
}

func authMetricAttrString(attrs attribute.Set, key string) string {
	v, _ := attrs.Value(attribute.Key(key))
	return v.AsString()
}

// authCounterValue returns the int64 Sum value of m's data point labeled
// exactly by operation/outcome, failing the test if m is not a Sum[int64] or
// no matching data point exists.
func authCounterValue(t *testing.T, m metricdata.Metrics, operation, outcome string) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Sum[int64]", m.Name, m.Data)
	}
	for _, dp := range sum.DataPoints {
		if authMetricAttrString(dp.Attributes, "operation") == operation && authMetricAttrString(dp.Attributes, "outcome") == outcome {
			return dp.Value
		}
	}
	t.Fatalf("metric %q has no data point for operation=%q outcome=%q", m.Name, operation, outcome)
	return 0
}

// authHistogramCount returns the observation Count of m's data point labeled
// exactly by operation/outcome, failing the test if m is not a
// Histogram[float64] or no matching data point exists.
func authHistogramCount(t *testing.T, m metricdata.Metrics, operation, outcome string) uint64 {
	t.Helper()
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("metric %q Data = %T, want metricdata.Histogram[float64]", m.Name, m.Data)
	}
	for _, dp := range hist.DataPoints {
		if authMetricAttrString(dp.Attributes, "operation") == operation && authMetricAttrString(dp.Attributes, "outcome") == outcome {
			return dp.Count
		}
	}
	t.Fatalf("metric %q has no data point for operation=%q outcome=%q; data points: %+v", m.Name, operation, outcome, hist.DataPoints)
	return 0
}

// TestService_AuthMetrics_RecordCountAndDurationByOperationAndOutcome is the
// regression proof that Service emits the must-instrument authentication
// metrics -- login success/failure rate, MFA challenge volume, refresh
// failure rate -- on authCountMetricName/authDurationMetricName rather
// than leaving every outcome observable only through login_attempts rows
// and structured logs. Every operation/outcome pair in the vocabulary is
// asserted through collectAuthMetric, which fails with "metric ... not
// found" when no data point carries the pair -- the negative control an
// operation the metric path fails to record trips.
//
// Deliberately not t.Parallel(): it swaps the process-wide global otel
// MeterProvider (see setupAuthMetricsMeterProvider's own doc comment).
func TestService_AuthMetrics_RecordCountAndDurationByOperationAndOutcome(t *testing.T) {
	reader := setupAuthMetricsMeterProvider(t)
	f := newServiceFixture(t)
	// Two distinct accounts, deliberately: RecordLoginFailure keys its
	// progressive lockout delay off the account's blind index and
	// real wall-clock time (never the fixture's injected clock -- see its
	// own doc comment), so a failed attempt immediately followed by a
	// SUCCESSFUL one on the SAME account would itself be refused as locked
	// out. Using two accounts keeps the failure and success paths
	// independent, which is all this test needs -- the metric is labeled
	// by operation and outcome, never by account.
	f.registerUser(t, "metrics-fail@example.com", testTenantA)
	user := f.registerUser(t, "metrics-ok@example.com", testTenantA)

	if _, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "metrics-fail@example.com", Password: "wrong password entirely", IP: "203.0.113.20",
	}); err == nil {
		t.Fatal("Login(wrong password) error = nil, want a refusal")
	}
	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "metrics-ok@example.com", Password: testPassword, IP: "203.0.113.20",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	// A successful refresh.
	f.clock.Advance(time.Minute)
	if _, refreshErr := f.svc.Refresh(t.Context(), pair.RefreshToken); refreshErr != nil {
		t.Fatalf("Refresh() error = %v", refreshErr)
	}

	// A successful MFA step-up (TOTP) and a failed one (wrong code).
	_, err = f.svc.EnrollTOTP(t.Context(), Principal{UserID: user.ID})
	if err != nil {
		t.Fatalf("EnrollTOTP() error = %v", err)
	}
	// ConfirmTOTP is itself a step-up-adjacent call but not the metric under
	// test; only VerifyStepUp records authOpMFAChallenge, so its outcome is
	// irrelevant here beyond needing a confirmed factor to challenge against.

	// Built from the existing pair's own Principal rather than
	// loginPrincipal(t, f, ...), which would itself call Login again and
	// throw off the login-succeeded count asserted below.
	principal := pair.Principal
	if _, err := f.svc.VerifyStepUp(t.Context(), principal, "000000", "203.0.113.21"); err == nil {
		t.Fatal("VerifyStepUp(wrong code, unconfirmed factor) error = nil, want a refusal")
	}

	count := collectAuthMetric(t, reader, authCountMetricName)
	if got := authCounterValue(t, count, authOpLogin, authOutcomeFailed); got != 1 {
		t.Errorf("%s{operation=login,outcome=failed} = %d, want 1", authCountMetricName, got)
	}
	if got := authCounterValue(t, count, authOpLogin, authOutcomeSucceeded); got != 1 {
		t.Errorf("%s{operation=login,outcome=succeeded} = %d, want 1", authCountMetricName, got)
	}
	if got := authCounterValue(t, count, authOpRefresh, authOutcomeSucceeded); got != 1 {
		t.Errorf("%s{operation=refresh,outcome=succeeded} = %d, want 1", authCountMetricName, got)
	}
	if got := authCounterValue(t, count, authOpMFAChallenge, authOutcomeFailed); got != 1 {
		t.Errorf("%s{operation=mfa_challenge,outcome=failed} = %d, want 1", authCountMetricName, got)
	}

	duration := collectAuthMetric(t, reader, authDurationMetricName)
	if got := authHistogramCount(t, duration, authOpLogin, authOutcomeSucceeded); got != 1 {
		t.Errorf("%s{operation=login,outcome=succeeded} count = %d, want 1", authDurationMetricName, got)
	}
	if got := authHistogramCount(t, duration, authOpRefresh, authOutcomeSucceeded); got != 1 {
		t.Errorf("%s{operation=refresh,outcome=succeeded} count = %d, want 1", authDurationMetricName, got)
	}
}

// TestRegisterAuthMetrics_Smoke is registerAuthMetrics's own equivalent of
// go/jobs/standalone_queue_test.go's TestRegisterJobMetrics_Smoke:
// registration alone (no operation ever attempted) must not error or panic.
func TestRegisterAuthMetrics_Smoke(t *testing.T) {
	count, duration := registerAuthMetrics()
	if count == nil || duration == nil {
		t.Fatalf("registerAuthMetrics() = (%v, %v), want two non-nil instruments", count, duration)
	}
}

// TestService_UpgradePasswordHash_DoesNotRegressACommittedColumn pins
// upgradePasswordHash's write shape: the rehash must NOT persist through a
// whole-row rewrite of the user the sign-in read BEFORE the argon2
// verification ran. Any column another caller committed on that row between
// the read and the save (a concurrent SMS sign-in marking the phone
// verified, the shape this test stands in for) would be silently undone by
// the stale snapshot's write-back.
//
// The test is deterministic rather than a timed race because the hazard
// does not need real concurrency to materialize: the stale snapshot IS the
// hazard, so handing upgradePasswordHash a user object that predates a
// committed column change reproduces it exactly. Sequence:
//
//  1. an account whose password hash was minted under WEAKER parameters
//     than this Service's own (so NeedsRehash says the corpus is stale);
//  2. a sign-in-shaped read of that user;
//  3. a committed phone-verified flag landing between the read and the
//     write (written directly here -- it stands in for a concurrent SMS
//     sign-in's own commit);
//  4. the rehash.
//
// A whole-row save in step 4 would write the step-2 snapshot's
// PhoneVerified == false back over step 3's commit: the flag would regress.
// The rehash must persist exactly the one column it owns.
func TestService_UpgradePasswordHash_DoesNotRegressACommittedColumn(t *testing.T) {
	// An account whose stored hash is stale against this Service's current
	// parameters: minted under weaker ones.
	weak := testParams()
	weak.Iterations = 1
	strong := weak
	strong.Iterations = 3
	svc := newServiceFixture(t, WithPasswordParams(strong)).svc

	email := "rehash-regression@example.com"
	password := testPassword
	oldHash, err := HashPassword(password, weak)
	if err != nil {
		t.Fatalf("HashPassword(weak): %v", err)
	}
	seed := &User{Email: email, DisplayName: "Rehash Regression", PasswordHash: oldHash}
	createErr := svc.Users().Create(t.Context(), seed)
	if createErr != nil {
		t.Fatalf("Create: %v", createErr)
	}

	// The sign-in-shaped read: the snapshot upgradePasswordHash would be
	// handed, taken before the interfering commit below.
	stale, err := svc.Users().FindByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}

	// The interfering commit: a concurrent SMS sign-in proved the phone and
	// marked the row verified between the snapshot's read and the rehash's
	// write.
	seedErr := svc.Users().db.Table("users").Where("id = ?", seed.ID).Update("phone_verified", true).Error
	if seedErr != nil {
		t.Fatalf("interfering commit: %v", seedErr)
	}

	svc.upgradePasswordHash(t.Context(), stale, password)

	fresh, err := svc.Users().FindByID(t.Context(), seed.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if !fresh.PhoneVerified {
		t.Errorf("PhoneVerified = false after the rehash -- the rehash's whole-row save regressed the committed flag")
	}
	if fresh.PasswordHash == oldHash {
		t.Errorf("PasswordHash unchanged after the rehash -- the corpus migration did not apply")
	}
	if ok, err := VerifyPassword(fresh.PasswordHash, password); err != nil || !ok {
		t.Errorf("stored hash does not verify the password (ok=%v err=%v)", ok, err)
	}
}

// TestService_Register_ConcurrentDuplicateAnswersTheCodedConflict pins the
// duplicate-registration race: when two registrations of one email race,
// the database's unique index (idx_users_email_index) admits exactly one
// insert and refuses the other, and the loser must hear the same coded
// conflict a sequential duplicate hears (Register's pre-checks answer that
// one) -- never a bare internal error, which would tell the client the
// server broke when the truth is that the address is taken.
//
// Deliberately not t.Parallel and run over fresh fixtures per round: the
// losers' inserts only lose when their pre-checks overlap (both read "no
// such account" before either inserts), so every round holds the racers
// behind gateTableReads until ALL of them have completed their
// pre-check read -- the read-then-insert overlap this test exists to pin is
// then exercised no matter how few cores the runner has, instead of
// depending on the goroutine scheduler serializing or overlapping the
// racers by luck. The loop still exists because every round must be
// deterministic: the database arbitrates exactly one winner.
func TestService_Register_ConcurrentDuplicateAnswersTheCodedConflict(t *testing.T) {
	const (
		rounds = 8
		racers = 3
	)
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newServiceFixture(t)
			email := fmt.Sprintf("register-race-%d@example.com", round)

			// See the test's doc comment: no racer's insert may begin before
			// every racer's pre-check read has completed.
			gateTableReads(t, f.db, "users", racers)

			start := make(chan struct{})
			results := make([]error, racers)
			var wg sync.WaitGroup
			wg.Add(racers)
			for i := range racers {
				go func(i int) {
					defer wg.Done()
					<-start
					_, err := f.svc.Register(t.Context(), RegisterInput{
						Email: email, Password: testPassword, DisplayName: "Racer",
					})
					results[i] = err
				}(i)
			}
			close(start)
			wg.Wait()

			successes := 0
			for i, err := range results {
				switch {
				case err == nil:
					successes++
				case apperr.HasCode(err, ErrEmailAlreadyRegistered.Code):
					// The expected answer for every loser: the sequential
					// duplicate's coded conflict.
				default:
					t.Fatalf("racer %d answered %v, want the coded %q conflict: a lost insert race must never surface as a bare internal error (P3-20)", i, err, ErrEmailAlreadyRegistered.Code)
				}
			}
			if successes != 1 {
				t.Fatalf("%d of %d racing registrations succeeded, want exactly 1", successes, racers)
			}
		})
	}
}

// TestService_Register_InsertConflictProbeAnswersTheCodedConflicts pins the
// insert-conflict translator mapRegisterCreateConflict deterministically.
// The translator only ever runs when a registration's INSERT was refused by
// the database's unique index -- a state that, end to end, only a
// concurrent duplicate registration produces (Register's pre-checks answer
// the sequential duplicate before any insert), which is why the concurrent
// race tests above exercise it. This test calls the translator directly on
// the post-race states it was written for -- a committed account answering
// the identifier probe -- so the probe's answers are pinned by every run,
// not by whether a wall-clock race happened to overlap that run.
func TestService_Register_InsertConflictProbeAnswersTheCodedConflicts(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	emailUser := f.registerUser(t, "mapper-email@example.com", testTenantA)
	phoneUser, err := f.svc.Register(t.Context(), RegisterInput{Phone: "+8613912345678", Password: testPassword})
	if err != nil {
		t.Fatalf("Register(phone) error = %v", err)
	}
	ctx := t.Context()

	// A refusal that is not a duplicate passes through untouched: the
	// translator never turns an unrelated insert failure into a coded
	// conflict.
	other := errors.New("injected: users insert refused")
	if got := f.svc.mapRegisterCreateConflict(ctx, &User{Email: emailUser.Email}, other); !errors.Is(got, other) {
		t.Fatalf("mapRegisterCreateConflict(non-duplicate) = %v, want the input error passed through", got)
	}

	// The email probe: an insert of an email-only account refused as a
	// duplicate, with the racing account's commit already visible -- the
	// state every loser of an email registration race is in.
	if got := f.svc.mapRegisterCreateConflict(ctx, &User{Email: emailUser.Email}, gorm.ErrDuplicatedKey); !errors.Is(got, ErrEmailAlreadyRegistered) {
		t.Fatalf("mapRegisterCreateConflict(email duplicate) = %v, want ErrEmailAlreadyRegistered", got)
	}

	// The phone probe (with a free email so the email probe misses first,
	// mirroring Register's own probe order): a duplicate phone insert whose
	// racing account's commit is already visible answers
	// ErrPhoneAlreadyRegistered, never a bare internal error.
	if got := f.svc.mapRegisterCreateConflict(ctx, &User{Email: "fresh@example.com", Phone: phoneUser.Phone}, gorm.ErrDuplicatedKey); !errors.Is(got, ErrPhoneAlreadyRegistered) {
		t.Fatalf("mapRegisterCreateConflict(phone duplicate) = %v, want ErrPhoneAlreadyRegistered", got)
	}
}

// TestService_Register_CallerContextTenantNeverReachesEventUserCreated
// pins the pre-tenant publish at the service layer: Service.Register
// publishes authn.user.created through Service.publishTenantless, so the
// event never picks a TenantID up from the context it is published in. The
// hazard the shape guards is real: a host composition whose tenancy
// middleware resolves even allowlisted pre-auth routes --
// go/tenancy.WithAllowlist exempts a route from the 403 on a RESOLUTION
// FAILURE, it does not skip resolution -- hands Register a context carrying
// the CALLER's own tenant whenever the caller holds a valid bearer (the
// api-client attaches the held token to every request by default), so an
// event that inherited the context tenant would stamp the caller's tenant
// onto the new account's user.created event. Subscribers would then read
// that stamp as the account's tenant: org's handleUserCreated
// (go/org/events.go) would seat the account in the caller's tenant, and a
// host's tenant-less self-service provisioning would skip it -- the account
// would get a seat in the caller's tenant and no workspace of its own, and
// any authenticated tenant member could add arbitrary new accounts to their
// tenant with no invitation, permission check or email verification.
//
// Registration is pre-tenant: the account is created in no tenant, and the
// event must carry no tenant regardless of what the caller's context holds.
func TestService_Register_CallerContextTenantNeverReachesEventUserCreated(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)

	// A register under a tenant-bearing caller context -- the shape a
	// signed-in caller's register POST reaches the handler in, once a
	// tenancy middleware has resolved their bearer. The event must carry
	// no tenant: the tenant the caller merely holds is not an attestation
	// registration may act on.
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)
	if _, err := f.svc.Register(ctx, RegisterInput{
		Email: "caller-ctx@example.com", Password: testPassword, DisplayName: "Caller Ctx",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	evt, ok := f.events.First(EventUserCreated)
	if !ok {
		t.Fatal("no EventUserCreated was published")
	}
	if evt.TenantID != "" {
		t.Errorf("EventUserCreated TenantID = %q, want empty: a registration under a tenant-bearing caller context must "+
			"not stamp the caller's tenant (org's subscriber would seat the account in tenant %q, and a host's "+
			"tenant-less self-service provisioning would skip it)",
			evt.TenantID, testTenantA)
	}

	// The no-tenant baseline: a plain pre-auth register keeps publishing
	// the same tenant-less event.
	if _, err := f.svc.Register(t.Context(), RegisterInput{
		Email: "plain-ctx@example.com", Password: testPassword, DisplayName: "Plain Ctx",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	events := f.events.Events()
	baseline := events[len(events)-1]
	if baseline.Type != EventUserCreated {
		t.Fatalf("last published event type = %q, want %q", baseline.Type, EventUserCreated)
	}
	if baseline.TenantID != "" {
		t.Errorf("EventUserCreated TenantID = %q, want empty on a plain no-tenant register", baseline.TenantID)
	}
}

// TestService_Providers_ExposesTheWiredRegistry pins the Providers accessor,
// the one place a host's login page reads the wired social-channel list from
// after construction: a service assembled with no social channels must
// expose an empty registry, and one assembled with a channel must expose
// that channel's name.
func TestService_Providers_ExposesTheWiredRegistry(t *testing.T) {
	t.Parallel()

	none := newServiceFixture(t)
	if got := none.svc.Providers(); got == nil {
		t.Fatal("Providers() = nil, want the wired registry")
	} else if names := got.Names(); len(names) != 0 {
		t.Errorf("Providers().Names() = %v with no social channel wired, want empty", names)
	}

	gitHub := newServiceFixture(t, WithSocialProviders(&stubProvider{name: ProviderGitHub}))
	if names := gitHub.svc.Providers().Names(); !slices.Equal(names, []string{ProviderGitHub}) {
		t.Errorf("Providers().Names() = %v, want [%s]", names, ProviderGitHub)
	}
}

// TestService_SwitchTenant_RefusesWhenTheSessionIsUnknown pins the
// not-found half of SwitchTenant's session lookup: a principal whose token
// names a session this deployment has no row for must not mint a fresh pair
// -- the answer is the same session-revoked refusal a revoked session earns,
// because from the caller's side the two are indistinguishable and the token
// is dead either way.
func TestService_SwitchTenant_RefusesWhenTheSessionIsUnknown(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "switch-unknown-session@example.com", testTenantA)

	_, err := f.svc.SwitchTenant(t.Context(), Principal{UserID: user.ID, SessionID: "no-such-session"}, testTenantB)
	if !apperr.HasCode(err, ErrSessionRevoked.Code) {
		t.Fatalf("SwitchTenant(unknown session) error = %v, want code %q", err, ErrSessionRevoked.Code)
	}
}

// TestService_SwitchTenant_RefusesWhenTheAccountIsGone pins the account
// lookup SwitchTenant performs after its membership re-check: a session
// whose user row has been erased (a database restore, a compliance erasure
// behind authn's back) must not receive a token pair for the target tenant.
func TestService_SwitchTenant_RefusesWhenTheAccountIsGone(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "switch-account-gone@example.com", testTenantA)
	principal := loginPrincipal(t, f, user, testTenantA)

	if err := f.db.Where("id = ?", user.ID).Delete(&User{}).Error; err != nil {
		t.Fatalf("delete user row: %v", err)
	}
	_, err := f.svc.SwitchTenant(t.Context(), principal, testTenantA)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SwitchTenant() error = %v, want ErrNotFound from the user lookup", err)
	}
}

// TestService_SwitchTenant_RefusesASuspendedAccount pins the account-status
// half of SwitchTenant's user lookup: an operator suspension between a
// sign-in and a tenant switch must stop the switch the same way it stops a
// refresh -- the account's credentials no longer certify anything.
func TestService_SwitchTenant_RefusesASuspendedAccount(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "switch-suspended@example.com", testTenantA)
	principal := loginPrincipal(t, f, user, testTenantA)

	if err := f.db.Model(&User{}).Where("id = ?", user.ID).Update("status", UserStatusSuspended).Error; err != nil {
		t.Fatalf("suspend user: %v", err)
	}
	_, err := f.svc.SwitchTenant(t.Context(), principal, testTenantA)
	if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrInvalidCredentials.Code)
	}
}

// TestService_Refresh_RefusesWhenTheAccountIsGone pins refresh's own user
// lookup: the membership re-check passes (the reader still lists the id),
// but the account row itself has been erased -- a database restore or a
// compliance erasure behind authn's back -- so no fresh pair may be minted
// for it.
func TestService_Refresh_RefusesWhenTheAccountIsGone(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "refresh-account-gone@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: user.Email, Password: testPassword, TenantID: testTenantA, IP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	err = f.db.Where("id = ?", user.ID).Delete(&User{}).Error
	if err != nil {
		t.Fatalf("delete user row: %v", err)
	}
	_, err = f.svc.Refresh(t.Context(), pair.RefreshToken)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Refresh() error = %v, want ErrNotFound from the user lookup", err)
	}
}

// TestService_Register_ConcurrentDuplicatePhoneAnswersTheCodedConflict is
// the phone twin of the email race test above: when two registrations of
// the same phone number both pass the sequential pre-checks, the database's
// unique phone index admits exactly one insert and the loser must receive
// the same coded conflict the sequential duplicate answers -- never a bare
// internal error. Like its email twin, every round gates the racers'
// pre-check reads behind gateTableReads so the read-then-insert
// overlap is exercised on every run rather than by scheduler luck.
func TestService_Register_ConcurrentDuplicatePhoneAnswersTheCodedConflict(t *testing.T) {
	const (
		rounds = 8
		racers = 3
	)
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newServiceFixture(t)
			phone := fmt.Sprintf("+861390000%04d", round)

			// See the email twin's doc comment: no racer's insert may begin
			// before every racer's pre-check read has completed.
			gateTableReads(t, f.db, "users", racers)

			start := make(chan struct{})
			results := make([]error, racers)
			var wg sync.WaitGroup
			wg.Add(racers)
			for i := range racers {
				go func(i int) {
					defer wg.Done()
					<-start
					_, err := f.svc.Register(t.Context(), RegisterInput{
						Phone: phone, Password: testPassword, DisplayName: "Racer",
					})
					results[i] = err
				}(i)
			}
			close(start)
			wg.Wait()

			successes := 0
			for i, err := range results {
				switch {
				case err == nil:
					successes++
				case apperr.HasCode(err, ErrPhoneAlreadyRegistered.Code):
					// The expected answer for every loser: the sequential
					// duplicate's coded conflict.
				default:
					t.Fatalf("racer %d answered %v, want the coded %q conflict: a lost insert race must never surface as a bare internal error", i, err, ErrPhoneAlreadyRegistered.Code)
				}
			}
			if successes != 1 {
				t.Fatalf("%d of %d racing registrations succeeded, want exactly 1", successes, racers)
			}
		})
	}
}

// recordingMembershipReader wraps a MembershipReader and records, per call,
// the system context the call was made under -- the observable half of the
// elevation resolveTenant performs before the cross-tenant enumeration.
type recordingMembershipReader struct {
	inner MembershipReader

	mu    sync.Mutex
	calls []membershipCall
}

// membershipCall is one recorded invocation: which method ran, and the
// system reason its context carried (sawSystem false when it carried none).
type membershipCall struct {
	method    string
	reason    pkgcore.SystemReason
	sawSystem bool
}

func (r *recordingMembershipReader) ActiveMembership(ctx context.Context, userID string, tenantID pkgcore.TenantID) (bool, error) {
	r.record("ActiveMembership", ctx)
	return r.inner.ActiveMembership(ctx, userID, tenantID)
}

func (r *recordingMembershipReader) TenantsOf(ctx context.Context, userID string) ([]pkgcore.TenantID, error) {
	r.record("TenantsOf", ctx)
	return r.inner.TenantsOf(ctx, userID)
}

func (r *recordingMembershipReader) record(method string, ctx context.Context) {
	reason, ok := pkgcore.SystemReasonFromContext(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, membershipCall{method: method, reason: reason, sawSystem: ok})
}

// callsOfMethod returns every recorded call to method, in order.
func (r *recordingMembershipReader) callsOfMethod(method string) []membershipCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []membershipCall
	for _, call := range r.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

// refusingBus fails Publish for one event type while delegating everything
// else to the wrapped bus: the in-memory bus cannot produce a publish
// failure, and tenancy.WithSystemContext fails the whole grant closed when
// one happens.
type refusingBus struct {
	pkgcore.EventBus
	eventType string
}

func (b *refusingBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	if evt.Type == b.eventType {
		return errors.New("refusingBus: publish refused")
	}
	return b.EventBus.Publish(ctx, evt)
}

var _ pkgcore.EventBus = (*refusingBus)(nil)

// TestService_Login_NoRequestedTenant_EnumeratesUnderTheAuditedSystemContext
// pins the elevation: a no-tenant sign-in asks which tenants the account
// belongs to, and that cross-tenant question reaches the reader under the
// module's own audited system-context grant -- actor the account asking,
// purpose SystemPurposeSignInTenantEnumeration -- with the matching
// tenancy.system_context.entered event published exactly once.
func TestService_Login_NoRequestedTenant_EnumeratesUnderTheAuditedSystemContext(t *testing.T) {
	t.Parallel()

	members := testutil.NewMemberships()
	reader := &recordingMembershipReader{inner: members}

	bus := pkgcore.NewMemoryEventBus()
	var enteredMu sync.Mutex
	var entered []tenancy.SystemContextEnteredEvent
	bus.Subscribe(tenancy.EventSystemContextEntered, func(_ context.Context, evt pkgcore.Event) error {
		if payload, ok := evt.Payload.(tenancy.SystemContextEnteredEvent); ok {
			enteredMu.Lock()
			entered = append(entered, payload)
			enteredMu.Unlock()
		}
		return nil
	})
	f := newServiceFixtureOn(t, bus, pkgcore.NewMemoryKVStore(), WithMembershipReader(reader))

	user, err := f.svc.Register(t.Context(), RegisterInput{
		Email: "enumeration@example.com", Password: testPassword, DisplayName: "Enumeration User",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "enumeration@example.com", Password: testPassword, IP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if pair.Principal.TenantID != testTenantA {
		t.Fatalf("signed into tenant %q, want %q", pair.Principal.TenantID, testTenantA)
	}

	calls := reader.callsOfMethod("TenantsOf")
	if len(calls) != 1 {
		t.Fatalf("TenantsOf was called %d times, want 1", len(calls))
	}
	if !calls[0].sawSystem {
		t.Fatal("TenantsOf ran without a system context; the enumeration must be elevated")
	}
	if calls[0].reason.Actor != user.ID {
		t.Errorf("system reason actor = %q, want the account asking %q", calls[0].reason.Actor, user.ID)
	}
	if calls[0].reason.Purpose != SystemPurposeSignInTenantEnumeration {
		t.Errorf("system reason purpose = %q, want %q", calls[0].reason.Purpose, SystemPurposeSignInTenantEnumeration)
	}

	enteredMu.Lock()
	defer enteredMu.Unlock()
	if len(entered) != 1 {
		t.Fatalf("tenancy.system_context.entered published %d times, want exactly 1 per enumeration", len(entered))
	}
	if entered[0].Actor != user.ID || entered[0].Purpose != SystemPurposeSignInTenantEnumeration {
		t.Errorf("audit event = {actor %q, purpose %q}, want {%q, %q}",
			entered[0].Actor, entered[0].Purpose, user.ID, SystemPurposeSignInTenantEnumeration)
	}
}

// TestService_Login_RequestedTenant_DoesNotElevate pins the other half of
// the elevation contract: a sign-in that names its tenant asks a
// tenant-scoped membership question inside that tenant, so no system
// context is taken and no enumeration is performed.
func TestService_Login_RequestedTenant_DoesNotElevate(t *testing.T) {
	t.Parallel()

	members := testutil.NewMemberships()
	reader := &recordingMembershipReader{inner: members}
	f := newServiceFixture(t, WithMembershipReader(reader))

	user, err := f.svc.Register(t.Context(), RegisterInput{
		Email: "scoped@example.com", Password: testPassword, DisplayName: "Scoped User",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, testTenantA)

	if _, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "scoped@example.com", Password: testPassword, TenantID: testTenantA, IP: "203.0.113.9",
	}); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	active := reader.callsOfMethod("ActiveMembership")
	if len(active) != 1 {
		t.Fatalf("ActiveMembership was called %d times, want 1", len(active))
	}
	if active[0].sawSystem {
		t.Fatal("ActiveMembership ran under a system context; only the cross-tenant enumeration takes the grant")
	}
	if n := len(reader.callsOfMethod("TenantsOf")); n != 0 {
		t.Fatalf("TenantsOf was called %d times for a tenant-scoped sign-in, want 0", n)
	}
}

// TestService_Login_EnumerationGrantWithoutAuditRecord_FailsClosed pins the
// fail-closed leg: when the audited grant cannot be recorded (the bus
// refuses the system-context event), no enumeration runs and the sign-in
// folds the refusal into the uniform invalid-credentials answer -- nobody
// signs in on the strength of an unrecorded cross-tenant read.
func TestService_Login_EnumerationGrantWithoutAuditRecord_FailsClosed(t *testing.T) {
	t.Parallel()

	members := testutil.NewMemberships()
	reader := &recordingMembershipReader{inner: members}
	bus := &refusingBus{EventBus: pkgcore.NewMemoryEventBus(), eventType: tenancy.EventSystemContextEntered}
	f := newServiceFixtureOn(t, bus, pkgcore.NewMemoryKVStore(), WithMembershipReader(reader))

	user, err := f.svc.Register(t.Context(), RegisterInput{
		Email: "no-audit@example.com", Password: testPassword, DisplayName: "Unaudited User",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, testTenantA)

	_, err = f.svc.Login(t.Context(), LoginInput{
		Identifier: "no-audit@example.com", Password: testPassword, IP: "203.0.113.9",
	})
	if !apperr.HasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("Login() error = %v, want the uniform %q refusal", err, ErrInvalidCredentials.Code)
	}
	if n := len(reader.callsOfMethod("TenantsOf")); n != 0 {
		t.Fatalf("TenantsOf ran %d times with the audit record unwritable, want 0", n)
	}
}
