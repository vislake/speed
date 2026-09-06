package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestShareRepository_GuardedWritesStaySerializedOnSQLite pins the SQLite
// half of the writeMu scope decision (finding P2-sharing-5): the
// in-process mutex exists for SQLite's single-writer file lock and MUST
// stay engaged there -- serializeWrites is true over the SQLite test
// database, and a guarded write issued while another guarded write holds
// the mutex queues behind it (deterministically: the goroutine cannot
// finish until the holder releases) rather than contending for the file
// lock. The PostgreSQL half -- no mutex, unrelated tenants never block on
// each other -- is proven against a real server by the module's
// integration tier (integration_test/postgres_mutex_scope_test.go).
func TestShareRepository_GuardedWritesStaySerializedOnSQLite(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	if !repo.serializeWrites {
		t.Fatalf("serializeWrites = false over the SQLite test DB, want true -- the mutex must stay engaged on the single-writer dialect")
	}

	ctx := pkgcore.WithTenant(context.Background(), testTenant)
	repo.writeMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- repo.runGuardedWrite(ctx, func(tx *gorm.DB) error {
			return tx.Exec("SELECT 1").Error
		})
	}()
	// Wait for the goroutine to be past its launch, then give it ample time
	// to reach (and fail to acquire) the mutex: completion is impossible
	// while the test holds writeMu, so a completed write in this window is
	// a definite ordering failure, never a scheduling artifact.
	<-started
	select {
	case err := <-done:
		t.Fatalf("guarded write completed (%v) while writeMu was held -- SQLite guarded writes must serialize behind the mutex", err)
	case <-time.After(500 * time.Millisecond):
		// Blocked behind the holder, as the SQLite ordering requires.
	}
	repo.writeMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("guarded write after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("guarded write did not complete after writeMu was released")
	}
}

// This file's tests pin withTxRetry's classification contract
// deterministically: which errors retry (dbkit.IsRetryableConflict's
// transient-contention class), which surface immediately, the attempt
// bound, and the exhaustion answer. The concurrency the retry exists to
// absorb is exercised by the real multi-connection SQLite tournaments in
// service_test.go's TestService_Access_Concurrent* family -- the -race
// suite runs those, and the plain runner's harsher scheduling (a
// GOMAXPROCS=1 run included) is where a single-attempt guarded write used
// to lose the file's write lock past the busy_timeout and fail the
// views-survive-revoke regression as sharing.internal_error. Those tests
// cannot fail deterministically on the pre-retry code the way this file's
// can; this file is the deterministic half of the pin, they are the
// end-to-end half.

// retryableConflictErr returns an error dbkit.IsRetryableConflict
// classifies as transient contention, using the same driver wording the
// classifier matches (modernc.org/sqlite renders SQLITE_BUSY with the
// result-code name parenthesized, exactly as the real error in
// go/dbkit's retry_sqlite_test.go does).
func retryableConflictErr() error {
	return errors.New("database is locked (5) (SQLITE_BUSY)")
}

// TestWithTxRetry_RetriesOnlyTransientConflicts pins the retry predicate:
// an op that fails with a retryable conflict twice and then succeeds runs
// three times and reports success -- never surfacing the transient
// failures a single-attempt write would have surfaced as a store error.
func TestWithTxRetry_RetriesOnlyTransientConflicts(t *testing.T) {
	calls := 0
	err := withTxRetry(func() error {
		calls++
		if calls < 3 {
			return retryableConflictErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withTxRetry = %v, want nil after two transient conflicts", err)
	}
	if calls != 3 {
		t.Errorf("op ran %d times, want 3 -- a transient conflict must be retried", calls)
	}
}

// TestWithTxRetry_NonRetryableErrorSurfacesImmediately pins the other half
// of the predicate: an error outside dbkit.IsRetryableConflict's class (a
// dropped table, an injected trigger failure -- the shapes this module's
// store-failure tests actually use) must return on the first attempt,
// unretried, so the retry can never mask a real failure behind repeated
// attempts.
func TestWithTxRetry_NonRetryableErrorSurfacesImmediately(t *testing.T) {
	boom := errors.New("no such table: sharing_shares")
	calls := 0
	err := withTxRetry(func() error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("withTxRetry = %v, want the original error", err)
	}
	if calls != 1 {
		t.Errorf("op ran %d times, want 1 -- a non-retryable error must not be retried", calls)
	}
}

// TestWithTxRetry_ExhaustingTheBudgetReturnsTheLastConflict pins the
// exhaustion answer: an op that conflicts on every attempt runs exactly
// txRetryBudget times and returns the last conflict error raw -- the
// caller's own ErrInternal mapping (repository.go) is what the caller
// sees, exactly as if the write had failed once without any retry.
func TestWithTxRetry_ExhaustingTheBudgetReturnsTheLastConflict(t *testing.T) {
	calls := 0
	err := withTxRetry(func() error {
		calls++
		return retryableConflictErr()
	})
	if err == nil || !dbkit.IsRetryableConflict(err) {
		t.Fatalf("withTxRetry = %v, want the last conflict error", err)
	}
	if calls != txRetryBudget {
		t.Errorf("op ran %d times, want exactly %d -- the retry must be bounded", calls, txRetryBudget)
	}
}
