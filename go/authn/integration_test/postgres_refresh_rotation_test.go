//go:build integration

package authn_test

import (
	"sync"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// concurrentRefreshers is how many goroutines race to consume the SAME
// refresh token at once. More than two is deliberate: two racing
// goroutines can pass under SQLite's coarse table-level locking by sheer
// luck even when the underlying compare-and-swap is broken, which is
// exactly the false confidence the frozen round plan's §1.9 calls out; a
// double-digit fan-out against a real PostgreSQL connection pool is what
// actually exercises the row-level CAS RefreshTokenRepository.Consume
// relies on (repository.go's own doc comment on why CAS, not
// SELECT ... FOR UPDATE, is this codebase's answer to the lack of a common
// row-locking primitive across both dialects).
const concurrentRefreshers = 16

// testPassword is this file's own copy of the unit tier's identical
// constant (service_test.go, package authn) -- this external test package
// cannot reach it.
const testPassword = "a perfectly fine passphrase"

// newIntegrationService assembles an authn.Service over db (expected to be
// testutil.NewPostgresDB's PostgreSQL handle) with the minimal options a
// real sign-in and refresh round trip needs: signing keys, the blind-index
// key, members as the membership reader, and fast argon2id parameters so
// the test does not pay real password-hashing cost sixteen times over.
// This mirrors the unit tier's own serviceFixture (service_test.go's
// newServiceFixtureWithKV), which this external test package cannot import
// directly.
func newIntegrationService(t *testing.T, db *gorm.DB, members *testutil.Memberships) *authn.Service {
	t.Helper()

	key, err := authn.GenerateTokenKey("kid-integration")
	if err != nil {
		t.Fatalf("GenerateTokenKey() error = %v", err)
	}
	keys, err := authn.NewKeySet(key)
	if err != nil {
		t.Fatalf("NewKeySet() error = %v", err)
	}

	svc, err := authn.NewService(db, pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(),
		authn.WithSigningKeys(keys),
		authn.WithBlindIndexKey(testutil.BlindIndexKey()),
		authn.WithMembershipReader(members),
		authn.WithPasswordParams(authn.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return svc
}

// TestRefreshRotation_ConcurrentReplay_ExactlyOneWinner_Postgres proves the
// round's most important concurrency guarantee under a REAL database rather
// than SQLite's coarse locking: concurrentRefreshers goroutines all present
// the SAME refresh token to Service.Refresh at once. Exactly one may
// succeed; every other goroutine must observe the replay-detected refusal,
// and the whole token family plus its session end up revoked as a result --
// SessionManager.Rotate's own doc comment describes this as "a client
// racing itself and a thief racing the victim look the same, and the safe
// reading is the second one".
func TestRefreshRotation_ConcurrentReplay_ExactlyOneWinner_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	members := testutil.NewMemberships()
	svc := newIntegrationService(t, db, members)
	ctx := t.Context()

	user, err := svc.Register(ctx, authn.RegisterInput{
		Email: "concurrent-refresh@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, pkgcore.TenantID("tenant-a"))

	pair, err := svc.Login(ctx, authn.LoginInput{
		Identifier: "concurrent-refresh@example.com", Password: testPassword,
		TenantID: pkgcore.TenantID("tenant-a"), IP: "203.0.113.5",
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	token := pair.RefreshToken
	if token == "" {
		t.Fatal("Login() returned no refresh token")
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		failures  int
		otherErrs []error
	)
	start := make(chan struct{})
	wg.Add(concurrentRefreshers)
	for i := 0; i < concurrentRefreshers; i++ {
		go func() {
			defer wg.Done()
			<-start
			_, refreshErr := svc.Refresh(ctx, token)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case refreshErr == nil:
				successes++
			case isReplayError(refreshErr):
				failures++
			default:
				otherErrs = append(otherErrs, refreshErr)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Errorf("successes = %d, want exactly 1 (concurrentRefreshers=%d)", successes, concurrentRefreshers)
	}
	if failures != concurrentRefreshers-1 {
		t.Errorf("replay-refused failures = %d, want %d", failures, concurrentRefreshers-1)
	}
	if len(otherErrs) != 0 {
		t.Errorf("%d goroutines got an unexpected error (want either success or a replay refusal): %v",
			len(otherErrs), otherErrs)
	}

	// The side effect Rotate's doc comment promises: the WHOLE family and
	// its session are revoked, so even the one winning access/refresh
	// token pair from this race must not keep working afterward.
	sessions, listErr := svc.ListSessions(ctx, user.ID)
	if listErr != nil {
		t.Fatalf("ListSessions() error = %v", listErr)
	}
	if len(sessions) != 1 || sessions[0].Status != authn.SessionStatusRevoked {
		t.Errorf("sessions = %+v, want exactly one, revoked as a side effect of the detected replay", sessions)
	}
}

// isReplayError reports whether err is authn.ErrRefreshTokenReused, matched
// on its Code the way every other test in this module does (apperr's
// builders derive a new value per decoration, so a decorated error is never
// identical to the sentinel it came from -- see service.go's hasCode).
func isReplayError(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == authn.ErrRefreshTokenReused.Code
}
