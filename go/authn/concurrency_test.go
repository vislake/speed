package authn

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// This file pins the conflict-retry envelope's contract deterministically
// (which errors retry -- dbkit.IsRetryableConflict's transient-contention
// class -- which surface immediately, the attempt bound, the exhaustion
// answer, and the insert residue rule), and its end-to-end half: the real
// cross-connection SQLite tournament that holds the file's write lock past
// the dialect's busy_timeout while the sign-in and preferences requests run.

// retryableConflictErr returns an error dbkit.IsRetryableConflict
// classifies as transient contention, using the same driver wording the
// classifier matches (modernc.org/sqlite renders SQLITE_BUSY with the
// result-code name parenthesized).
func retryableConflictErr() error {
	return errors.New("database is locked (5) (SQLITE_BUSY)")
}

// TestWithConflictRetry_RetriesOnlyTransientConflicts pins the retry
// predicate: an op that fails with a retryable conflict twice and then
// succeeds runs three times and reports success -- never surfacing the
// transient failures a single-attempt call would have surfaced.
func TestWithConflictRetry_RetriesOnlyTransientConflicts(t *testing.T) {
	t.Parallel()

	calls := 0
	err := withConflictRetry(func() error {
		calls++
		if calls < 3 {
			return retryableConflictErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withConflictRetry = %v, want nil after two transient conflicts", err)
	}
	if calls != 3 {
		t.Errorf("op ran %d times, want 3 -- a transient conflict must be retried", calls)
	}
}

// TestWithConflictRetry_NonRetryableErrorSurfacesImmediately pins the other
// half of the predicate: an error outside dbkit.IsRetryableConflict's class
// must return on the first attempt, unretried and unwrapped, so the retry
// can never mask a real failure behind repeated attempts.
func TestWithConflictRetry_NonRetryableErrorSurfacesImmediately(t *testing.T) {
	t.Parallel()

	boom := errors.New("no such table: users")
	calls := 0
	err := withConflictRetry(func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("withConflictRetry = %v, want the original error", err)
	}
	if calls != 1 {
		t.Errorf("op ran %d times, want 1 -- a non-retryable error must not be retried", calls)
	}
}

// TestWithConflictRetry_ExhaustingTheBudgetReturnsTheLastConflict pins the
// exhaustion answer: an op that conflicts on every attempt runs exactly
// txRetryBudget times and returns the last conflict error raw, which the
// callers' existing error mapping turns into the same internal error a
// single un-retried attempt produces.
func TestWithConflictRetry_ExhaustingTheBudgetReturnsTheLastConflict(t *testing.T) {
	t.Parallel()

	calls := 0
	err := withConflictRetry(func() error {
		calls++
		return retryableConflictErr()
	})
	if err == nil || !dbkit.IsRetryableConflict(err) {
		t.Fatalf("withConflictRetry = %v, want the last conflict error", err)
	}
	if calls != txRetryBudget {
		t.Errorf("op ran %d times, want exactly %d -- the retry must be bounded", calls, txRetryBudget)
	}
}

// TestWithInsertConflictRetry_DuplicateOnRetryWithTheRowPresentIsCompletion
// pins the residue rule: a duplicate-key refusal raised by a retry, with
// the row verifiably present, is the earlier attempt's own write having
// landed -- completion, not a failure.
func TestWithInsertConflictRetry_DuplicateOnRetryWithTheRowPresentIsCompletion(t *testing.T) {
	t.Parallel()

	inserts, verifies := 0, 0
	err := withInsertConflictRetry(
		func() error {
			inserts++
			if inserts == 1 {
				return retryableConflictErr()
			}
			return gorm.ErrDuplicatedKey
		},
		func() (bool, error) {
			verifies++
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("withInsertConflictRetry = %v, want nil -- the duplicate is the earlier attempt's own row", err)
	}
	if inserts != 2 || verifies != 1 {
		t.Errorf("insert ran %d times / verify ran %d times, want 2 / 1", inserts, verifies)
	}
}

// TestWithInsertConflictRetry_DuplicateOnRetryWithNoRowSurfaces pins the
// boundary of that rule: a duplicate raised by a retry whose key is NOT
// present is some other uniqueness constraint genuinely refusing the
// insert, and that error must surface unchanged.
func TestWithInsertConflictRetry_DuplicateOnRetryWithNoRowSurfaces(t *testing.T) {
	t.Parallel()

	inserts := 0
	err := withInsertConflictRetry(
		func() error {
			inserts++
			if inserts == 1 {
				return retryableConflictErr()
			}
			return gorm.ErrDuplicatedKey
		},
		func() (bool, error) { return false, nil },
	)
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("withInsertConflictRetry = %v, want the duplicate-key error -- no row behind it means a real refusal", err)
	}
	if inserts != 2 {
		t.Errorf("insert ran %d times, want 2", inserts)
	}
}

// TestWithInsertConflictRetry_DuplicateOnFirstAttemptIsNotResidue pins that
// the residue rule only ever applies to a RETRY: a duplicate refused on the
// first attempt has nothing of this call behind it and is returned without
// consulting the row at all.
func TestWithInsertConflictRetry_DuplicateOnFirstAttemptIsNotResidue(t *testing.T) {
	t.Parallel()

	verifies := 0
	err := withInsertConflictRetry(
		func() error { return gorm.ErrDuplicatedKey },
		func() (bool, error) {
			verifies++
			return true, nil
		},
	)
	if !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("withInsertConflictRetry = %v, want the duplicate-key error", err)
	}
	if verifies != 0 {
		t.Errorf("verify ran %d times, want 0 -- the first attempt has no residue to check", verifies)
	}
}

// TestSessionManager_RowExistsHelpers pins the two residue checks the
// insert retry consults: each answers false (not an error) for a row that
// is not there, and true once the row is -- the refresh-token check through
// the digest the repository actually stores, never the plaintext.
func TestSessionManager_RowExistsHelpers(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := t.Context()
	m := f.svc.sessions

	if present, err := m.sessionRowExists(ctx, newID()); err != nil || present {
		t.Fatalf("sessionRowExists(missing) = %v, %v; want false, nil", present, err)
	}
	if present, err := m.refreshTokenRowExists(ctx, newID(), "no-such-digest"); err != nil || present {
		t.Fatalf("refreshTokenRowExists(missing) = %v, %v; want false, nil", present, err)
	}

	user := f.registerUser(t, "helpers@example.com", "tenant-acme")
	session, issued, err := m.Start(ctx, StartSessionInput{UserID: user.ID, TenantID: "tenant-acme"})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if present, err := m.sessionRowExists(ctx, session.ID); err != nil || !present {
		t.Fatalf("sessionRowExists(started) = %v, %v; want true, nil", present, err)
	}
	if present, err := m.refreshTokenRowExists(ctx, issued.Record.ID, issued.Record.TokenHash); err != nil || !present {
		t.Fatalf("refreshTokenRowExists(issued) = %v, %v; want true, nil", present, err)
	}
	if present, err := m.refreshTokenRowExists(ctx, issued.Record.ID, "another-digest"); err != nil || present {
		t.Fatalf("refreshTokenRowExists(wrong digest) = %v, %v; want false, nil", present, err)
	}
}

// contendedLockHold is how long the tournament below holds the file's write
// lock: a little past one whole busy window of dbkit's SQLite dialect
// (dialect/sqlite's busy_timeout default is 5000ms), so the in-flight
// requests' first attempts genuinely expire -- they lose the race and
// report SQLITE_BUSY -- while the retried attempt, already waiting in its
// own busy window, is the one that meets the release.
const contendedLockHold = 5600 * time.Millisecond

// contendedFixture builds a Service and Handler over a SQLite file whose
// path the test knows (testutil.NewDBAt), registers one account with a
// membership, and returns the pooled *sql.DB so the test can check a
// second connection out of it -- the connection that will hold the file's
// write lock independently of the handle serving requests.
func contendedFixture(t *testing.T) (*Handler, *User, *sql.DB) {
	t.Helper()

	db := testutil.NewDBAt(t, filepath.Join(t.TempDir(), "authn-contended.sqlite"))
	clock := testutil.NewClock(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	members := testutil.NewMemberships()
	keys := testutil.NewKeySource(t, "kid-test")
	bus := pkgcore.NewMemoryEventBus()

	svc, err := NewService(db, bus, pkgcore.NewMemoryKVStore(),
		WithKeySource(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithMembershipReader(members),
		WithClock(clock.Now),
		WithPasswordParams(testParams()),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	user, err := svc.Register(t.Context(), RegisterInput{
		Email: "owner@example.com", Password: testPassword, DisplayName: "Owner",
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, "tenant-acme")

	reg := componenttest.NewRegistryWithBus(bus)
	if declareErr := componenttest.Declare(reg, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(auditActions...)
	}); declareErr != nil {
		t.Fatalf("AuditActions.Add() error = %v", declareErr)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB() error = %v", err)
	}
	return NewHandler(svc, bus, reg.AuditActions), user, sqlDB
}

// TestLoginAndPreferences_SecondConnectionHoldingTheWriteLock is the
// cross-connection contention pin for the sign-in write path and the
// preferences read path: a second connection to the same SQLite file holds
// the file's write lock for longer than the dialect's bounded busy_timeout,
// so both requests' first attempts genuinely lose the race and report
// SQLITE_BUSY -- the conflict a single un-retried attempt folds into 500
// authn.internal_error -- and the bounded conflict retry carries them
// through once the holder commits. Both requests start while the lock is
// held and must answer their success statuses.
func TestLoginAndPreferences_SecondConnectionHoldingTheWriteLock(t *testing.T) {
	t.Parallel()

	h, user, sqlDB := contendedFixture(t)

	lockConn, err := sqlDB.Conn(t.Context())
	if err != nil {
		t.Fatalf("check out the lock-holding connection: %v", err)
	}
	defer func() { _ = lockConn.Close() }()
	if _, lockErr := lockConn.ExecContext(t.Context(), "BEGIN EXCLUSIVE"); lockErr != nil {
		t.Fatalf("BEGIN EXCLUSIVE: %v", lockErr)
	}

	loginBody, err := json.Marshal(api.AuthnLoginWithPasswordRequest{
		Identifier: "owner@example.com",
		Password:   testPassword,
	})
	if err != nil {
		t.Fatalf("marshal the sign-in body: %v", err)
	}

	type response struct {
		rec     *httptest.ResponseRecorder
		elapsed time.Duration
	}
	loginCh := make(chan response, 1)
	prefsCh := make(chan response, 1)

	go func() {
		start := time.Now()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", bytes.NewReader(loginBody))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		loginCh <- response{rec, time.Since(start)}
	}()
	go func() {
		start := time.Now()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/me/preferences", nil)
		req = req.WithContext(WithPrincipal(req.Context(), Principal{UserID: user.ID, TenantID: "tenant-acme"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		prefsCh <- response{rec, time.Since(start)}
	}()

	time.Sleep(contendedLockHold)
	if _, err := lockConn.ExecContext(t.Context(), "COMMIT"); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}

	login := <-loginCh
	prefs := <-prefsCh
	t.Logf("sign-in: %d after %v; preferences: %d after %v",
		login.rec.Code, login.elapsed.Round(time.Millisecond),
		prefs.rec.Code, prefs.elapsed.Round(time.Millisecond))

	if login.rec.Code != http.StatusOK {
		t.Errorf("POST /api/v1/authn/login/password under a held write lock = %d, body = %s (waited %v); want 200 -- the conflict retry must carry the sign-in through",
			login.rec.Code, login.rec.Body.String(), login.elapsed.Round(time.Millisecond))
	}
	if prefs.rec.Code != http.StatusOK {
		t.Errorf("GET /api/v1/authn/me/preferences under a held write lock = %d, body = %s (waited %v); want 200 -- the conflict retry must carry the read through",
			prefs.rec.Code, prefs.rec.Body.String(), prefs.elapsed.Round(time.Millisecond))
	}
}
