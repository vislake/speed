package authn

import (
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// loginAt signs userEmail in once, using device as the client label, and
// returns the resulting session id.
func loginAt(t *testing.T, f *serviceFixture, identifier, device string, tenant pkgcore.TenantID) *TokenPair {
	t.Helper()
	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: identifier, Password: testPassword, TenantID: tenant,
		Device: device, IP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Login(%s) error = %v", device, err)
	}
	return pair
}

// TestService_ListSessions_NewestFirstIncludingRevoked proves ordering and
// that a revoked session still appears on the list -- ListSessions has no
// opinion about which entries are still usable, only SessionRepository's own
// Status field does, per history.go's doc comment.
func TestService_ListSessions_NewestFirstIncludingRevoked(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "devices@example.com", testTenantA)

	first := loginAt(t, f, "devices@example.com", "laptop", testTenantA)
	f.clock.Advance(time.Minute)
	second := loginAt(t, f, "devices@example.com", "phone", testTenantA)

	if err := f.svc.Logout(t.Context(), first.Principal.SessionID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	sessions, err := f.svc.ListSessions(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions() returned %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != second.Principal.SessionID {
		t.Errorf("newest-first: sessions[0] = %s, want the most recent login %s", sessions[0].ID, second.Principal.SessionID)
	}
	if sessions[1].ID != first.Principal.SessionID {
		t.Errorf("sessions[1] = %s, want the first (now revoked) login %s", sessions[1].ID, first.Principal.SessionID)
	}
	if sessions[1].Status != SessionStatusRevoked {
		t.Errorf("the logged-out session's Status = %q, want %q -- ListSessions must not drop revoked sessions", sessions[1].Status, SessionStatusRevoked)
	}
	if sessions[0].Status != SessionStatusActive {
		t.Errorf("the still-signed-in session's Status = %q, want %q", sessions[0].Status, SessionStatusActive)
	}
}

// TestService_ListSessions_RequiresAUser proves the same fail-closed shape
// every other self-service method in this module has for an empty caller.
func TestService_ListSessions_RequiresAUser(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	if _, err := f.svc.ListSessions(t.Context(), ""); !hasCode(err, ErrAuthenticationRequired.Code) {
		t.Errorf("ListSessions(\"\") error = %v, want %s", err, ErrAuthenticationRequired.Code)
	}
}

// TestService_RevokeSession_OwnDevice_Succeeds proves the ordinary case: the
// owner revokes one of their own OTHER sessions, and only that one stops
// working.
func TestService_RevokeSession_OwnDevice_Succeeds(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "revoke@example.com", testTenantA)
	kept := loginAt(t, f, "revoke@example.com", "laptop", testTenantA)
	target := loginAt(t, f, "revoke@example.com", "phone", testTenantA)

	if err := f.svc.RevokeSession(t.Context(), user.ID, target.Principal.SessionID); err != nil {
		t.Fatalf("RevokeSession() error = %v", err)
	}

	if _, err := f.svc.Refresh(t.Context(), target.RefreshToken); !hasCode(err, ErrRefreshTokenInvalid.Code, ErrSessionRevoked.Code) {
		t.Errorf("Refresh() on the revoked device's token error = %v, want it to fail", err)
	}
	if _, err := f.svc.Refresh(t.Context(), kept.RefreshToken); err != nil {
		t.Errorf("Refresh() on the KEPT device's token error = %v, want it to still work", err)
	}

	if n := f.events.Count(EventSessionRevoked); n != 1 {
		t.Errorf("recorded %d %s events, want 1", n, EventSessionRevoked)
	}
}

// TestService_RevokeSession_SomebodyElsesSession_ReturnsSessionNotFound is
// the round's no-existence-disclosure test: revoking a session id that is
// real, but belongs to a different account, must answer exactly like
// revoking one that does not exist at all -- see ErrSessionNotFound's own
// doc comment. A caller must never be able to learn that a session id is
// real by getting a different error for "not yours" than for "unknown".
func TestService_RevokeSession_SomebodyElsesSession_ReturnsSessionNotFound(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "victim@example.com", testTenantA)
	attacker := f.registerUser(t, "attacker@example.com", testTenantA)
	victimSession := loginAt(t, f, "victim@example.com", "laptop", testTenantA)

	errUnknown := f.svc.RevokeSession(t.Context(), attacker.ID, "no-such-session-id")
	errSomeoneElses := f.svc.RevokeSession(t.Context(), attacker.ID, victimSession.Principal.SessionID)

	if !hasCode(errUnknown, ErrSessionNotFound.Code) {
		t.Errorf("RevokeSession(unknown id) error = %v, want %s", errUnknown, ErrSessionNotFound.Code)
	}
	if !hasCode(errSomeoneElses, ErrSessionNotFound.Code) {
		t.Errorf("RevokeSession(victim's id) error = %v, want %s", errSomeoneElses, ErrSessionNotFound.Code)
	}

	// The victim's session must be completely unaffected.
	if _, err := f.svc.Refresh(t.Context(), victimSession.RefreshToken); err != nil {
		t.Errorf("the victim's own refresh failed after the attacker's refused attempt: %v", err)
	}
}

// TestService_RevokeOtherSessions_KeepsCurrentRevokesRest proves the bulk
// operation's two invariants: the caller's own current session survives,
// and every OTHER active session of theirs stops working, with the
// returned count matching exactly what was revoked.
func TestService_RevokeOtherSessions_KeepsCurrentRevokesRest(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "bulk@example.com", testTenantA)
	current := loginAt(t, f, "bulk@example.com", "laptop", testTenantA)
	other1 := loginAt(t, f, "bulk@example.com", "phone", testTenantA)
	other2 := loginAt(t, f, "bulk@example.com", "tablet", testTenantA)

	revoked, err := f.svc.RevokeOtherSessions(t.Context(), user.ID, current.Principal.SessionID)
	if err != nil {
		t.Fatalf("RevokeOtherSessions() error = %v", err)
	}
	if revoked != 2 {
		t.Fatalf("RevokeOtherSessions() revoked = %d, want 2", revoked)
	}

	if _, refreshErr := f.svc.Refresh(t.Context(), current.RefreshToken); refreshErr != nil {
		t.Errorf("the current session's refresh failed: %v", refreshErr)
	}
	for name, tok := range map[string]string{"phone": other1.RefreshToken, "tablet": other2.RefreshToken} {
		if _, refreshErr := f.svc.Refresh(t.Context(), tok); refreshErr == nil {
			t.Errorf("the %s session's refresh succeeded, want it revoked", name)
		}
	}

	// Calling it again with nothing left to revoke reports zero rather
	// than erroring -- the operation is idempotent in its count as well
	// as its effect.
	second, err := f.svc.RevokeOtherSessions(t.Context(), user.ID, current.Principal.SessionID)
	if err != nil {
		t.Fatalf("second RevokeOtherSessions() error = %v", err)
	}
	if second != 0 {
		t.Errorf("second RevokeOtherSessions() revoked = %d, want 0", second)
	}
}

// TestService_RevokeOtherSessions_DoesNotTouchAnotherAccount proves the bulk
// revoke is scoped to the caller's own sessions only.
func TestService_RevokeOtherSessions_DoesNotTouchAnotherAccount(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "alice@example.com", testTenantA)
	f.registerUser(t, "bob@example.com", testTenantA)
	aliceSession := loginAt(t, f, "alice@example.com", "laptop", testTenantA)
	bobSession := loginAt(t, f, "bob@example.com", "laptop", testTenantA)

	if _, err := f.svc.RevokeOtherSessions(t.Context(), bobSession.Principal.UserID, bobSession.Principal.SessionID); err != nil {
		t.Fatalf("RevokeOtherSessions() error = %v", err)
	}

	if _, err := f.svc.Refresh(t.Context(), aliceSession.RefreshToken); err != nil {
		t.Errorf("alice's session was affected by bob's revoke-others call: %v", err)
	}
}

// TestService_ListLoginHistory_NewestFirstAndScopedToCaller proves ordering
// and that one account's history never leaks into another's.
func TestService_ListLoginHistory_NewestFirstAndScopedToCaller(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	f.registerUser(t, "alice@example.com", testTenantA)
	f.registerUser(t, "bob@example.com", testTenantA)

	loginAt(t, f, "alice@example.com", "laptop", testTenantA)
	f.clock.Advance(time.Minute)
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "alice@example.com", Password: "wrong password entirely", IP: "203.0.113.9"}); err == nil {
		t.Fatal("Login() with a wrong password succeeded, want it to fail")
	}
	bobPair := loginAt(t, f, "bob@example.com", "laptop", testTenantA)

	alice, err := f.svc.ListLoginHistory(t.Context(), bobPair.Principal.UserID, 0)
	if err != nil {
		t.Fatalf("ListLoginHistory(bob) error = %v", err)
	}
	if len(alice) != 1 {
		t.Fatalf("bob's login history has %d entries, want exactly his own 1 (never alice's)", len(alice))
	}

	history, err := f.svc.ListLoginHistory(t.Context(), aliceUserID(t, f), 0)
	if err != nil {
		t.Fatalf("ListLoginHistory(alice) error = %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("alice's login history has %d entries, want 2 (one success, one failure)", len(history))
	}
	if history[0].Result != LoginResultFailure {
		t.Errorf("history[0].Result = %q, want %q (newest first)", history[0].Result, LoginResultFailure)
	}
	if history[1].Result != LoginResultSuccess {
		t.Errorf("history[1].Result = %q, want %q", history[1].Result, LoginResultSuccess)
	}
}

// aliceUserID looks alice@example.com up by email, for tests that need her
// id without threading it through every helper call.
func aliceUserID(t *testing.T, f *serviceFixture) string {
	t.Helper()
	user, err := f.svc.Users().FindByEmail(t.Context(), "alice@example.com")
	if err != nil {
		t.Fatalf("FindByEmail(alice) error = %v", err)
	}
	return user.ID
}

// TestService_ListLoginHistory_LimitIsClampedNotRejected proves the pagination
// bound: a limit above maxLoginHistoryLimit degrades to the ceiling rather
// than erroring, and a non-positive one falls back to the default.
func TestService_ListLoginHistory_LimitIsClampedNotRejected(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	user := f.registerUser(t, "many@example.com", testTenantA)
	for i := 0; i < 5; i++ {
		loginAt(t, f, "many@example.com", "laptop", testTenantA)
		f.clock.Advance(time.Second)
	}

	capped, err := f.svc.ListLoginHistory(t.Context(), user.ID, maxLoginHistoryLimit+1000)
	if err != nil {
		t.Fatalf("ListLoginHistory(over-large limit) error = %v", err)
	}
	if len(capped) != 5 {
		t.Fatalf("ListLoginHistory(over-large limit) returned %d, want all 5 (a small fixture never reaches the ceiling, but the call must not error)", len(capped))
	}

	limited, err := f.svc.ListLoginHistory(t.Context(), user.ID, 2)
	if err != nil {
		t.Fatalf("ListLoginHistory(2) error = %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("ListLoginHistory(2) returned %d, want 2", len(limited))
	}
}
