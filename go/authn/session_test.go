package authn

import (
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// sessionFixture is everything a session test needs, assembled once.
type sessionFixture struct {
	manager  *SessionManager
	sessions *SessionRepository
	tokens   *RefreshTokenRepository
	clock    *testutil.Clock
	events   *testutil.EventRecorder
	db       *gorm.DB
}

const (
	testAccessTTL  = 15 * time.Minute
	testRefreshTTL = 24 * time.Hour
	testSessionTTL = 72 * time.Hour
)

func newSessionFixture(t *testing.T, mode RevocationMode) *sessionFixture {
	t.Helper()

	db := testutil.NewDB(t)
	sessions := newTestSessionRepository(t, db)
	tokens := newTestRefreshTokenRepository(t, db)
	clock := testutil.NewClock(time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC))

	bus := pkgcore.NewMemoryEventBus()
	events := testutil.NewEventRecorder()
	events.Subscribe(bus, EventSessionRevoked, EventSessionReplayDetected)

	manager, err := NewSessionManager(sessions, tokens, pkgcore.NewMemoryKVStore(), bus, mode,
		clock.Now, testRefreshTTL, testSessionTTL, testAccessTTL)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	return &sessionFixture{manager: manager, sessions: sessions, tokens: tokens, clock: clock, events: events, db: db}
}

func (f *sessionFixture) start(t *testing.T) (*Session, IssuedRefreshToken) {
	t.Helper()
	session, issued, err := f.manager.Start(t.Context(), StartSessionInput{
		UserID: "user-1", TenantID: pkgcore.TenantID("tenant-a"),
		AMR: []string{MethodPassword}, Device: "laptop", IP: "203.0.113.9",
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	return session, issued
}

// TestSessionManager_Start_StoresOnlyTheTokenDigest proves the opaque secret
// is never persisted: a dump of refresh_tokens must not yield a usable
// credential.
func TestSessionManager_Start_StoresOnlyTheTokenDigest(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	session, issued := f.start(t)

	if issued.Secret == "" {
		t.Fatal("Start() returned no refresh secret")
	}
	if issued.Record.TokenHash == issued.Secret {
		t.Fatal("the stored token hash equals the secret; the token is stored in the clear")
	}
	if issued.Record.RotatedFrom != "" {
		t.Errorf("RotatedFrom = %q, want empty for the first token of a family", issued.Record.RotatedFrom)
	}
	if session.CurrentTenantID != "tenant-a" {
		t.Errorf("CurrentTenantID = %q, want the tenant the session started in", session.CurrentTenantID)
	}

	var count int64
	if err := f.db.Raw("SELECT COUNT(*) FROM refresh_tokens WHERE token_hash = ?", issued.Secret).Row().Scan(&count); err != nil {
		t.Fatalf("probe the raw table: %v", err)
	}
	if count != 0 {
		t.Error("the raw refresh_tokens table contains the plaintext secret")
	}
}

// TestSessionManager_Rotate_IssuesAReplacementAndInvalidatesTheOld is the
// rotation contract: one use per token, and the replacement stays inside the
// same family so replay detection has something to revoke.
func TestSessionManager_Rotate_IssuesAReplacementAndInvalidatesTheOld(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	session, first := f.start(t)

	f.clock.Advance(time.Minute)
	rotatedSession, second, err := f.manager.Rotate(t.Context(), first.Secret)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotatedSession.ID != session.ID {
		t.Errorf("Rotate() returned session %s, want the original %s", rotatedSession.ID, session.ID)
	}
	if second.Secret == first.Secret {
		t.Fatal("Rotate() reissued the same secret")
	}
	if second.Record.FamilyID != first.Record.FamilyID {
		t.Errorf("family changed on rotation: %s -> %s", first.Record.FamilyID, second.Record.FamilyID)
	}
	if second.Record.RotatedFrom != first.Record.ID {
		t.Errorf("RotatedFrom = %q, want the token it replaced (%q)", second.Record.RotatedFrom, first.Record.ID)
	}

	consumed, err := f.tokens.FindByHash(t.Context(), hashRefreshSecret(first.Secret))
	if err != nil {
		t.Fatalf("FindByHash() error = %v", err)
	}
	if consumed.Status != RefreshTokenStatusRotated || consumed.ConsumedAt == nil {
		t.Errorf("the presented token = %+v, want rotated with a consumption timestamp", consumed)
	}

	// The replacement must itself be usable, or rotation would be a
	// one-shot sign-out.
	if _, _, err := f.manager.Rotate(t.Context(), second.Secret); err != nil {
		t.Fatalf("Rotate(replacement) error = %v", err)
	}
}

// TestSessionManager_Rotate_ReplayRevokesTheFamilyAndTheSession is the most
// important test in this file. Presenting an already-consumed refresh token
// means a copy of it exists somewhere it should not, and the response is not
// to refuse that one request but to revoke everything descended from the
// login -- otherwise whoever stole it stays signed in with the token they
// already rotated for themselves.
func TestSessionManager_Rotate_ReplayRevokesTheFamilyAndTheSession(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	session, first := f.start(t)

	f.clock.Advance(time.Minute)
	_, second, err := f.manager.Rotate(t.Context(), first.Secret)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	// The victim's client rotated normally. Now the thief presents the
	// copy of the token that was already consumed.
	_, _, replayErr := f.manager.Rotate(t.Context(), first.Secret)
	if !hasCode(replayErr, ErrRefreshTokenReused.Code) {
		t.Fatalf("Rotate(consumed token) error = %v, want code %q", replayErr, ErrRefreshTokenReused.Code)
	}

	stored, err := f.sessions.FindByID(t.Context(), session.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if stored.Status != SessionStatusRevoked {
		t.Errorf("session status = %q, want %q after a replay", stored.Status, SessionStatusRevoked)
	}
	if stored.RevokeReason != RevokeReasonReplay {
		t.Errorf("revoke reason = %q, want %q", stored.RevokeReason, RevokeReasonReplay)
	}

	// The thief's freshly rotated token must be dead too. Revoking only
	// the replayed token would leave it working, which is the whole point.
	if _, _, err := f.manager.Rotate(t.Context(), second.Secret); err == nil {
		t.Fatal("the token rotated before the replay still works; the family was not revoked")
	}

	if n := f.events.Count(EventSessionReplayDetected); n != 1 {
		t.Fatalf("recorded %d %s events, want 1", n, EventSessionReplayDetected)
	}
	evt, _ := f.events.First(EventSessionReplayDetected)
	payload, ok := evt.Payload.(SessionReplayDetectedPayload)
	if !ok {
		t.Fatalf("payload is %T, want SessionReplayDetectedPayload", evt.Payload)
	}
	if payload.SessionID != session.ID || payload.FamilyID != first.Record.FamilyID {
		t.Errorf("payload = %+v, want the replayed session and family", payload)
	}
}

// TestSessionManager_Rotate_ConcurrentUseOfOneTokenIsTreatedAsAReplay pins the
// consequence that has to be stated rather than discovered: two simultaneous
// refreshes with the same token are indistinguishable from a theft, exactly
// one wins, and the session ends.
func TestSessionManager_Rotate_ConcurrentUseOfOneTokenIsTreatedAsAReplay(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	session, first := f.start(t)

	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		replays int
	)
	wg.Add(racers)
	for range racers {
		go func() {
			defer wg.Done()
			_, _, err := f.manager.Rotate(t.Context(), first.Secret)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case hasCode(err, ErrRefreshTokenReused.Code):
				replays++
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d concurrent rotations of one token succeeded, want exactly 1", winners)
	}
	if replays != racers-1 {
		t.Fatalf("%d losers reported a replay, want %d", replays, racers-1)
	}

	stored, err := f.sessions.FindByID(t.Context(), session.ID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if stored.Status != SessionStatusRevoked {
		t.Errorf("session status = %q, want %q: a losing concurrent refresh is read as a replay", stored.Status, SessionStatusRevoked)
	}
}

func TestSessionManager_Rotate_RejectsUnusableTokens(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		wantCode string
		setup    func(t *testing.T, f *sessionFixture) string
	}{
		{
			name:     "empty",
			wantCode: ErrRefreshTokenInvalid.Code,
			setup:    func(*testing.T, *sessionFixture) string { return "" },
		},
		{
			name:     "unknown",
			wantCode: ErrRefreshTokenInvalid.Code,
			setup:    func(*testing.T, *sessionFixture) string { return "not-a-token-anyone-issued" },
		},
		{
			name:     "expired",
			wantCode: ErrRefreshTokenInvalid.Code,
			setup: func(t *testing.T, f *sessionFixture) string {
				_, issued := f.start(t)
				f.clock.Advance(testRefreshTTL + time.Minute)
				return issued.Secret
			},
		},
		{
			name:     "bound to a revoked session",
			wantCode: ErrRefreshTokenInvalid.Code,
			setup: func(t *testing.T, f *sessionFixture) string {
				session, issued := f.start(t)
				if err := f.manager.Revoke(t.Context(), session.ID, RevokeReasonLogout); err != nil {
					t.Fatalf("Revoke() error = %v", err)
				}
				return issued.Secret
			},
		},
		{
			name:     "past the session's own expiry",
			wantCode: ErrSessionRevoked.Code,
			setup: func(t *testing.T, f *sessionFixture) string {
				_, issued := f.start(t)
				// The session outlives no refresh token by
				// default, so widen the token's life to reach
				// the session bound first.
				f.clock.Advance(testSessionTTL + time.Minute)
				token, err := f.tokens.FindByHash(t.Context(), hashRefreshSecret(issued.Secret))
				if err != nil {
					t.Fatalf("FindByHash() error = %v", err)
				}
				token.ExpiresAt = f.clock.Now().Add(time.Hour)
				if err := f.db.Save(token).Error; err != nil {
					t.Fatalf("extend the token's expiry: %v", err)
				}
				return issued.Secret
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSessionFixture(t, RevocationModeNatural)
			presented := tc.setup(t, f)
			_, _, err := f.manager.Rotate(t.Context(), presented)
			if !hasCode(err, tc.wantCode) {
				t.Fatalf("Rotate() error = %v, want code %q", err, tc.wantCode)
			}
		})
	}
}

// TestSessionManager_Revoke_IsIdempotentAndAnnouncesOnce proves a second
// sign-out of the same session does not produce a second security notice.
func TestSessionManager_Revoke_IsIdempotentAndAnnouncesOnce(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	session, issued := f.start(t)

	for range 3 {
		if revokeErr := f.manager.Revoke(t.Context(), session.ID, RevokeReasonLogout); revokeErr != nil {
			t.Fatalf("Revoke() error = %v", revokeErr)
		}
	}
	if n := f.events.Count(EventSessionRevoked); n != 1 {
		t.Errorf("recorded %d %s events across three revocations, want 1", n, EventSessionRevoked)
	}

	token, err := f.tokens.FindByHash(t.Context(), hashRefreshSecret(issued.Secret))
	if err != nil {
		t.Fatalf("FindByHash() error = %v", err)
	}
	if token.Status != RefreshTokenStatusRevoked {
		t.Errorf("refresh token status = %q, want %q after the session was revoked", token.Status, RefreshTokenStatusRevoked)
	}

	// Revoking a session that never existed is not an error: the caller
	// already has the outcome it wanted.
	if err := f.manager.Revoke(t.Context(), "no-such-session", RevokeReasonLogout); err != nil {
		t.Errorf("Revoke(missing session) error = %v, want nil", err)
	}
}

// TestSessionManager_RevocationModes covers the documented trade between the
// two modes: natural expiry does no per-request work and lets an outstanding
// access token live out its lifetime; immediate mode records the session so
// the middleware can refuse it at once.
func TestSessionManager_RevocationModes(t *testing.T) {
	t.Parallel()

	t.Run("natural mode records nothing and never reports a revocation", func(t *testing.T) {
		f := newSessionFixture(t, RevocationModeNatural)
		session, _ := f.start(t)

		if got := f.manager.Mode(); got != RevocationModeNatural {
			t.Fatalf("Mode() = %q, want %q", got, RevocationModeNatural)
		}
		if err := f.manager.Revoke(t.Context(), session.ID, RevokeReasonLogout); err != nil {
			t.Fatalf("Revoke() error = %v", err)
		}
		revoked, err := f.manager.IsRevoked(t.Context(), session.ID)
		if err != nil {
			t.Fatalf("IsRevoked() error = %v", err)
		}
		if revoked {
			t.Error("IsRevoked() = true in natural mode; that mode deliberately keeps no list")
		}
	})

	t.Run("immediate mode reports the revocation at once", func(t *testing.T) {
		f := newSessionFixture(t, RevocationModeImmediate)
		session, _ := f.start(t)

		revoked, err := f.manager.IsRevoked(t.Context(), session.ID)
		if err != nil {
			t.Fatalf("IsRevoked() error = %v", err)
		}
		if revoked {
			t.Fatal("IsRevoked() = true for a live session")
		}

		if revokeErr := f.manager.Revoke(t.Context(), session.ID, RevokeReasonLogout); revokeErr != nil {
			t.Fatalf("Revoke() error = %v", revokeErr)
		}
		revoked, err = f.manager.IsRevoked(t.Context(), session.ID)
		if err != nil {
			t.Fatalf("IsRevoked() error = %v", err)
		}
		if !revoked {
			t.Error("IsRevoked() = false right after a revocation in immediate mode")
		}

		// An unrelated session must be unaffected.
		other, _, err := f.manager.Start(t.Context(), StartSessionInput{UserID: "user-2", TenantID: "tenant-a"})
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		revoked, err = f.manager.IsRevoked(t.Context(), other.ID)
		if err != nil {
			t.Fatalf("IsRevoked() error = %v", err)
		}
		if revoked {
			t.Error("revoking one session reported another as revoked")
		}
	})
}

// TestSessionManager_IsRevoked_ReportsAStoreFailure proves the middleware is
// given something to fail closed on. Reporting "not revoked" for an
// unreachable store would silently re-enable every session an operator had
// just signed out.
func TestSessionManager_IsRevoked_ReportsAStoreFailure(t *testing.T) {
	t.Parallel()

	sessions := newTestSessionRepository(t, testutil.NewDB(t))
	tokens := newTestRefreshTokenRepository(t, testutil.NewDB(t))
	clock := testutil.NewClock(time.Now())

	manager, err := NewSessionManager(sessions, tokens, testutil.FailingKVStore{}, pkgcore.NewMemoryEventBus(),
		RevocationModeImmediate, clock.Now, testRefreshTTL, testSessionTTL, testAccessTTL)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}

	revoked, err := manager.IsRevoked(t.Context(), "any-session")
	if err == nil {
		t.Fatal("IsRevoked() error = nil for an unreachable store")
	}
	if revoked {
		t.Error("IsRevoked() = true alongside an error; the caller must decide from the error")
	}
}

func TestNewSessionManager_RejectsAnIncompleteWiring(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	sessions := newTestSessionRepository(t, db)
	tokens := newTestRefreshTokenRepository(t, db)
	kv := pkgcore.NewMemoryKVStore()
	bus := pkgcore.NewMemoryEventBus()

	cases := []struct {
		name     string
		sessions *SessionRepository
		tokens   *RefreshTokenRepository
		kv       pkgcore.KVStore
		bus      pkgcore.EventBus
		now      func() time.Time
	}{
		{name: "no session repository", tokens: tokens, kv: kv, bus: bus, now: time.Now},
		{name: "no token repository", sessions: sessions, kv: kv, bus: bus, now: time.Now},
		{name: "no key-value store", sessions: sessions, tokens: tokens, bus: bus, now: time.Now},
		{name: "no event bus", sessions: sessions, tokens: tokens, kv: kv, now: time.Now},
		{name: "no clock", sessions: sessions, tokens: tokens, kv: kv, bus: bus},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSessionManager(tc.sessions, tc.tokens, tc.kv, tc.bus, RevocationModeNatural,
				tc.now, testRefreshTTL, testSessionTTL, testAccessTTL)
			if err == nil {
				t.Error("NewSessionManager() error = nil, want a rejection")
			}
		})
	}
}

// TestNewSessionManager_UnknownModeFallsBackToNatural proves an unrecognised
// mode never leaves revocation in an undefined state.
func TestNewSessionManager_UnknownModeFallsBackToNatural(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	manager, err := NewSessionManager(newTestSessionRepository(t, db), newTestRefreshTokenRepository(t, db),
		pkgcore.NewMemoryKVStore(), pkgcore.NewMemoryEventBus(), RevocationMode("something-else"),
		time.Now, testRefreshTTL, testSessionTTL, testAccessTTL)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	if got := manager.Mode(); got != RevocationModeNatural {
		t.Errorf("Mode() = %q, want %q", got, RevocationModeNatural)
	}
}

func TestSessionManager_Start_RequiresAUser(t *testing.T) {
	t.Parallel()

	f := newSessionFixture(t, RevocationModeNatural)
	if _, _, err := f.manager.Start(t.Context(), StartSessionInput{TenantID: "tenant-a"}); err == nil {
		t.Error("Start() error = nil for an input with no user id")
	}
}
