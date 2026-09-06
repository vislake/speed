package authn

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	"github.com/vislake/speed/go/pkgcore"
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

	db := testutil.NewDB(t)
	clock := testutil.NewClock(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	members := testutil.NewMemberships()

	keys := testutil.NewKeySource(t, "kid-test")

	bus := pkgcore.NewMemoryEventBus()
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
	return &serviceFixture{svc: svc, db: db, kv: kv, clock: clock, members: members, events: events, keys: keys}
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

// TestService_Login_TruncatesOverWidthClientStringsToTheColumnWidths is the
// P3-13 regression: sessions.device (VARCHAR(255)) and sessions.user_agent /
// login_attempts.user_agent (VARCHAR(512)) are the widths the migrations
// declare, and the two dialects disagree about enforcement -- PostgreSQL
// refuses an over-width write (SQLSTATE 22001) where SQLite stores it, so
// the SAME login with an over-width device or user agent succeeds on one
// dialect and fails on the other. The repository write boundary truncates
// the client-supplied strings to the columns' widths, so both dialects store
// the identical value. The exact truncated prefix is asserted, not merely a
// length: truncation must keep the value's HEAD (the part that identifies
// the device), cut at a rune boundary, and never touch a within-width value.
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
			if appErr.Code != ErrInvalidCredentials.Code {
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
	if n := f.events.Count(EventLoginFailed); n != len(cases) {
		t.Errorf("recorded %d %s events, want %d", n, EventLoginFailed, len(cases))
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
			if !hasCode(err, tc.wantCode) {
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
	if !hasCode(err, ErrPhoneAlreadyRegistered.Code) {
		t.Fatalf("Register(same number, different formatting) error = %v, want code %q", err, ErrPhoneAlreadyRegistered.Code)
	}
}

// TestService_Login_FailsClosedWithoutAMembershipReader is the fail-closed
// rule: an unanswerable "may this person act inside this tenant" is a
// refusal, never a default.
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

	_, err = unwired.Login(t.Context(), LoginInput{Identifier: "closed@example.com", Password: testPassword})
	if !hasCode(err, ErrTenantMembershipUnavailable.Code) {
		t.Fatalf("Login() error = %v, want code %q", err, ErrTenantMembershipUnavailable.Code)
	}

	_, err = unwired.SwitchTenant(t.Context(), pair.Principal, testTenantA)
	if !hasCode(err, ErrTenantMembershipUnavailable.Code) {
		t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrTenantMembershipUnavailable.Code)
	}
}

// TestService_Login_FailsClosedWhenMembershipCannotBeRead covers the other
// unanswerable case: the reader exists but errors.
func TestService_Login_FailsClosedWhenMembershipCannotBeRead(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "broken@example.com", testTenantA)
	f.members.FailWith(errors.New("the membership store is down"))

	_, err := f.svc.Login(t.Context(), LoginInput{Identifier: "broken@example.com", Password: testPassword})
	if !hasCode(err, ErrTenantMembershipUnavailable.Code) {
		t.Fatalf("Login() error = %v, want code %q", err, ErrTenantMembershipUnavailable.Code)
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
		if !hasCode(err, ErrTenantMembershipRequired.Code) {
			t.Fatalf("Login() error = %v, want code %q", err, ErrTenantMembershipRequired.Code)
		}
	})

	t.Run("a user with no membership at all cannot sign in", func(t *testing.T) {
		f.registerUser(t, "orphan@example.com")
		_, err := f.svc.Login(t.Context(), LoginInput{Identifier: "orphan@example.com", Password: testPassword})
		if !hasCode(err, ErrTenantMembershipRequired.Code) {
			t.Fatalf("Login() error = %v, want code %q", err, ErrTenantMembershipRequired.Code)
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
			if _, err := f.svc.SwitchTenant(t.Context(), pair.Principal, tc.target); !hasCode(err, tc.wantCode) {
				t.Fatalf("SwitchTenant() error = %v, want code %q", err, tc.wantCode)
			}
		})
	}

	t.Run("a session belonging to another user", func(t *testing.T) {
		other := f.registerUser(t, "other@example.com", testTenantA)
		forged := Principal{UserID: other.ID, SessionID: pair.Principal.SessionID, TenantID: testTenantA}
		if _, err := f.svc.SwitchTenant(t.Context(), forged, testTenantA); !hasCode(err, ErrTokenInvalid.Code) {
			t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrTokenInvalid.Code)
		}
	})

	t.Run("an unauthenticated principal", func(t *testing.T) {
		if _, err := f.svc.SwitchTenant(t.Context(), Principal{}, testTenantA); !hasCode(err, ErrAuthenticationRequired.Code) {
			t.Fatalf("SwitchTenant() error = %v, want code %q", err, ErrAuthenticationRequired.Code)
		}
	})
}

// TestService_SwitchTenant_RefusesAnExpiredSession is the regression for the
// go/authn audit's other confirmed finding: SwitchTenant checked
// session.Status but never session.ExpiresAt -- the one check Refresh
// already performs -- so a session past its own configured TTL, whose
// Status row nothing here ever proactively flips away from active, stayed
// usable for as long as the caller's already-issued access token remained
// valid, extending the session's practical lifetime by one access-token
// lifetime.
func TestService_SwitchTenant_RefusesAnExpiredSession(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "switch-after-expiry@example.com", testTenantA, testTenantB)

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "switch-after-expiry@example.com", Password: testPassword})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	testutil.ExpireSession(t, f.db, pair.Principal.SessionID, f.clock.Now().Add(-time.Hour))

	if _, err := f.svc.SwitchTenant(t.Context(), pair.Principal, testTenantB); !hasCode(err, ErrSessionRevoked.Code) {
		t.Fatalf("SwitchTenant(expired session) error = %v, want code %q", err, ErrSessionRevoked.Code)
	}
}

// TestService_SwitchTenant_StillValidSessionSucceeds guards the fix above
// against over-refusing: a session that has not yet reached its own
// ExpiresAt must keep switching tenants exactly as before.
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

// TestService_SwitchTenant_ConcurrentRevoke_IsRefused is the regression for
// the audit finding that SessionRepository.SetCurrentTenant dropped its own
// compare-and-swap result: a revoke landing between SwitchTenant's read of
// the session and its tenant update made the UPDATE match zero rows while
// still returning nil, so the caller minted a full-lifetime token pair for a
// session a concurrent sign-out had just killed -- a revoked session kept
// issuing tokens. The wedge parks the switch between its read and its write
// (both sides of the race run for real against the same database) and proves
// the revoke wins: the switch must answer ErrSessionRevoked and mint nothing.
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
	if !hasCode(raceErr, ErrSessionRevoked.Code) {
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
	if _, err := f.svc.Refresh(t.Context(), refreshed.RefreshToken); !hasCode(err, ErrTenantMembershipRequired.Code) {
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
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !hasCode(err, ErrInvalidCredentials.Code) {
		t.Fatalf("Refresh() error = %v, want code %q", err, ErrInvalidCredentials.Code)
	}
}

// TestService_Refresh_TransientMembershipFailureDoesNotConsumeTheToken is the
// regression for the go/authn audit's sequencing finding: refresh used to
// rotate (consume) the presented refresh token BEFORE re-verifying
// membership, so a transient MembershipReader failure left the presented
// token permanently spent even though the caller never received its
// replacement. The client's own, entirely legitimate retry with that same
// token then hit Rotate's replay detector -- which is exactly correct
// behaviour for an actually-replayed token, but wrong here -- and revoked
// the whole refresh-token family, the session, and fired
// EventSessionReplayDetected, all over what was really a backend hiccup. A
// two-second MembershipReader outage does not get to look identical to a
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

	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !hasCode(err, ErrTenantMembershipUnavailable.Code) {
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
// guard for the fix above: an actually-replayed refresh token -- one already
// rotated by a prior, successful call -- must still be caught, still revoke
// the whole family and session, and still announce
// EventSessionReplayDetected. Reordering refresh's re-verification ahead of
// rotation must not weaken this real security property.
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
	if _, err := f.svc.Refresh(t.Context(), pair.RefreshToken); !hasCode(err, ErrRefreshTokenReused.Code) {
		t.Fatalf("replayed Refresh() error = %v, want code %q", err, ErrRefreshTokenReused.Code)
	}

	if n := f.events.Count(EventSessionReplayDetected); n != 1 {
		t.Fatalf("EventSessionReplayDetected fired %d times for an actual replay, want 1", n)
	}
	if n := f.events.Count(EventSessionRevoked); n != 1 {
		t.Fatalf("EventSessionRevoked fired %d times for an actual replay, want 1", n)
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

	if err := f.svc.Logout(t.Context(), ""); !hasCode(err, ErrAuthenticationRequired.Code) {
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
// regression proof that Service actually emits the
// docs/internal/09-observability.md must-instrument row for the
// authentication domain -- login success/failure rate, MFA challenge
// volume, refresh failure rate -- rather than leaving every outcome
// observable only through login_attempts rows and structured logs. Before
// registerAuthMetrics/recordAuthMetric existed, every collectAuthMetric call
// below would fail with "metric ... not found", which is the negative
// control this test relies on.
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

// TestService_UpgradePasswordHash_DoesNotRegressACommittedColumn is the
// regression test for upgradePasswordHash's write shape. The method used to
// persist its rehash through UserRepository.Save -- a whole-row rewrite of
// the user the sign-in read BEFORE the argon2 verification ran. Any column
// another caller committed on that row between the read and the save (a
// concurrent SMS sign-in marking the phone verified, the shape this test
// stands in for) was silently undone by the stale snapshot's write-back.
//
// The test is deterministic rather than a timed race because the hazard
// does not need real concurrency to materialize: the stale snapshot IS the
// bug, so handing upgradePasswordHash a user object that predates a
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
// Before the fix, step 4's whole-row Save writes the step-2 snapshot's
// PhoneVerified == false back over step 3's commit: the flag regresses. The
// rehash must persist exactly the one column it owns.
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

// TestService_Register_ConcurrentDuplicateAnswersTheCodedConflict is the
// P3-20 regression: when two registrations of one email race, the database's
// unique index (idx_users_email_index) admits exactly one insert and refuses
// the other, and the loser must hear the same coded conflict a sequential
// duplicate hears (Register's pre-checks answer that one) -- never a bare
// internal error, which would tell the client the server broke when the
// truth is that the address is taken.
//
// Deliberately not t.Parallel and run over fresh fixtures per round: the
// losers' inserts only lose when their pre-checks overlap (both read "no
// such account" before either inserts), a wall-clock race -- the loop only
// costs the broken code its luck.
func TestService_Register_ConcurrentDuplicateAnswersTheCodedConflict(t *testing.T) {
	const (
		rounds = 8
		racers = 3
	)
	for round := 1; round <= rounds; round++ {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newServiceFixture(t)
			email := fmt.Sprintf("register-race-%d@example.com", round)

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
				case hasCode(err, ErrEmailAlreadyRegistered.Code):
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
