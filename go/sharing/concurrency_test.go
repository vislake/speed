package sharing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// TestShareRepository_GuardedWritesStaySerializedOnSQLite pins the SQLite
// half of the gate scope decision (concurrency.go's sqliteGate): the
// in-process ordering exists for SQLite's single-writer file lock and MUST
// stay engaged there -- serializeWrites is true over the SQLite test
// database, and a guarded write issued while another guarded write holds
// the gate's write side queues behind it (deterministically: the goroutine
// cannot finish until the holder releases) rather than contending for the
// file lock. The PostgreSQL half -- no in-process gate, unrelated tenants
// never block on each other -- is proven against a real server by the
// module's integration tier
// (integration_test/postgres_mutex_scope_test.go).
func TestShareRepository_GuardedWritesStaySerializedOnSQLite(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	if !repo.serializeWrites {
		t.Fatalf("serializeWrites = false over the SQLite test DB, want true -- the in-process ordering must stay engaged on the single-writer dialect")
	}

	ctx := pkgcore.WithTenant(context.Background(), testTenant)
	release, occupier := holdWriteGate(t, repo, ctx)
	defer release()

	done := make(chan error, 1)
	go func() {
		done <- repo.runGuardedWrite(ctx, func(tx *gorm.DB) error {
			return tx.Exec("SELECT 1").Error
		})
	}()
	awaitBlocked(t, "a guarded write behind a held gate", done)
	release()
	if err := awaitReleased(t, "the queued guarded write", done); err != nil {
		t.Fatalf("guarded write after release: %v", err)
	}
	if err := awaitReleased(t, "the occupying guarded write", occupier); err != nil {
		t.Fatalf("occupying guarded write: %v", err)
	}
}

// gateWindow is how long an assertion below waits for an operation to
// complete while a guarded write holds the gate. Completion in that state
// is impossible by construction -- the write side holds every other
// statement of the module off, and the goroutine cannot finish until the
// holder releases -- so a completion inside the window is a definite
// ordering failure, never a scheduling artifact. The window only has to
// outlast an unblocked operation's own latency, which over a local SQLite
// file is milliseconds.
const gateWindow = 500 * time.Millisecond

// holdWriteGate starts one real guarded write on repo whose transaction
// body blocks until the returned release is called, and returns once that
// write is genuinely holding the gate (its body has started, which happens
// only after the write side was taken). Occupying through the real
// guarded-write path -- rather than poking a mutex field -- is what keeps
// the assertions below about the behaviour that path actually produces:
// whatever orders guarded writes is exactly what is under test. release is
// idempotent; the occupying write's own completion (and error, if any) is
// reported on the returned channel.
func holdWriteGate(t *testing.T, repo *ShareRepository, ctx context.Context) (release func(), done <-chan error) {
	t.Helper()
	started := make(chan struct{})
	releaseCh := make(chan struct{})
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- repo.runGuardedWrite(ctx, func(*gorm.DB) error {
			close(started)
			<-releaseCh
			return nil
		})
	}()
	<-started
	var once sync.Once
	return func() { once.Do(func() { close(releaseCh) }) }, doneCh
}

// awaitBlocked fails the test if what completes within the gate window --
// see gateWindow's own comment for why that verdict is deterministic.
func awaitBlocked(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s completed (%v) while a guarded write held the ordering gate -- it must queue behind the gate's write side", what, err)
	case <-time.After(gateWindow):
	}
}

// awaitReleased waits for what to complete after the gate was released,
// failing the test if it does not.
func awaitReleased(t *testing.T, what string, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not complete after the gate was released", what)
		return nil
	}
}

// TestService_Get_WaitsBehindAHeldGuardedWriteOnSQLite pins the read side
// of the module's SQLite ordering: Service.Get's lookup
// (ShareRepository.findByIDGuarded) cannot proceed while one of the
// module's own guarded writes holds the gate -- it queues behind the write
// side and lands once that write releases -- so an owner-facing read can
// never meet a concurrent module write on the file's single-writer lock.
// A reader that took no in-process ordering would complete immediately
// here.
func TestService_Get_WaitsBehindAHeldGuardedWriteOnSQLite(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	release, occupier := holdWriteGate(t, svc.Shares(), testCtx())
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := svc.Get(testCtx(), created.Share.ID)
		done <- err
	}()
	awaitBlocked(t, "Get", done)
	release()
	if err := awaitReleased(t, "Get", done); err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if err := awaitReleased(t, "the occupying guarded write", occupier); err != nil {
		t.Fatalf("occupying guarded write: %v", err)
	}
}

// TestService_Create_WaitsBehindAHeldGuardedWriteOnSQLite pins the same
// ordering for the module's ordinary create write: the share-plus-index
// insert pair (ShareRepository.createWithTokenIndex) queues behind the
// gate's write side like every other module write.
func TestService_Create_WaitsBehindAHeldGuardedWriteOnSQLite(t *testing.T) {
	svc, _ := newTestService(t, nil)

	release, occupier := holdWriteGate(t, svc.Shares(), testCtx())
	defer release()

	done := make(chan error, 1)
	go func() {
		_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-2"})
		done <- err
	}()
	awaitBlocked(t, "Create", done)
	release()
	if err := awaitReleased(t, "Create", done); err != nil {
		t.Fatalf("Create after release: %v", err)
	}
	if err := awaitReleased(t, "the occupying guarded write", occupier); err != nil {
		t.Fatalf("occupying guarded write: %v", err)
	}
}

// TestService_WriteAccessLog_WaitsBehindAHeldGuardedWriteOnSQLite pins the
// ordering of the denied-access trail write (writeAccessLog ->
// AccessLogRepository.createWithRetry), and with it the shared gate: the
// occupying write is on the module's SHARE repository while the append
// runs through the module's ACCESS-LOG repository, so the append can only
// queue behind the other repository's write if NewService put both behind
// one gate. The append carries no share state, but it is still a write of
// this module to the same file.
func TestService_WriteAccessLog_WaitsBehindAHeldGuardedWriteOnSQLite(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "storage:obj-3"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	release, occupier := holdWriteGate(t, svc.Shares(), testCtx())
	defer release()

	done := make(chan error, 1)
	go func() {
		entry := svc.accessLogEntry(testTenant, created.Share.ID, AccessOutcomeDenied, AccessParams{})
		done <- svc.writeAccessLog(testCtx(), entry)
	}()
	awaitBlocked(t, "the denied-access log append", done)
	release()
	if err := awaitReleased(t, "the denied-access log append", done); err != nil {
		t.Fatalf("writeAccessLog after release: %v", err)
	}
	if err := awaitReleased(t, "the occupying guarded write", occupier); err != nil {
		t.Fatalf("occupying guarded write: %v", err)
	}
}

// This file's tests pin deterministically what the rest of the module
// exercises end to end: withTxRetry's classification contract (which
// errors retry -- dbkit.IsRetryableConflict's transient-contention class --
// which surface immediately, the attempt bound, and the exhaustion
// answer), and the SQLite ordering gate's coverage (the tests above: a
// held write side queues every other statement of the module behind it,
// reads included). The concurrency the retry absorbs is exercised end to
// end by the real multi-connection SQLite tournaments in service_test.go's
// TestService_Access_Concurrent* family; those tournaments cannot fail
// deterministically the way this file's gate windows can. This file is
// the deterministic half of the pin, they are the end-to-end half.

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

// TestShareRepository_ConcurrentReservesOnOneShare_SingleFlightWins is the
// reservation shape's concurrency proof: many goroutines racing the single
// in-flight reservation of one MaxViews-limited share (the route's
// concurrent-fetch shape, driven here through the guarded write itself)
// produce exactly one winner -- the database-arbitrated single-flight
// guarantee no read-then-decide can fake -- and the losers' later
// resolution attempts are all harmless no-ops, so exactly one view is
// ever spent. Runs under -race in the standard suite; on SQLite the
// gate's write-side ordering makes the outcome deterministic.
func TestShareRepository_ConcurrentReservesOnOneShare_SingleFlightWins(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), testTenant)

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}

	const racers = 16
	var wg sync.WaitGroup
	wins := make(chan bool, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, reserveErr := repo.tryReserveView(ctx, fresh, now)
			if reserveErr != nil {
				t.Errorf("tryReserveView: %v", reserveErr)
				wins <- false
				return
			}
			wins <- won
		}()
	}
	wg.Wait()
	close(wins)
	winnerCount := 0
	for won := range wins {
		if won {
			winnerCount++
		}
	}
	if winnerCount != 1 {
		t.Fatalf("%d of %d concurrent reservations won, want exactly 1 -- a limited share serves one viewer at a time", winnerCount, racers)
	}

	// The single winner's view resolves exactly once: the row shows one
	// reservation standing, one confirm spends it, and no other resolution
	// path can spend or release anything further.
	standing, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if standing.ViewsReserved != 1 || standing.ViewCount != 0 {
		t.Fatalf("row after the tournament = ViewsReserved %d / ViewCount %d, want 1 / 0", standing.ViewsReserved, standing.ViewCount)
	}
	if won, confirmErr := repo.tryConfirmView(ctx, standing, now, nil); confirmErr != nil || !won {
		t.Fatalf("tryConfirmView (winner): won=%v err=%v", won, confirmErr)
	}
	if won, refundErr := repo.tryRefundView(ctx, share.ID, now); refundErr != nil || won {
		t.Fatalf("tryRefundView (after the confirm) = won=%v err=%v, want won=false -- nothing stands to refund", won, refundErr)
	}
	final, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if final.ViewCount != 1 || final.ViewsReserved != 0 {
		t.Errorf("row after the confirm = ViewCount %d / ViewsReserved %d, want 1 / 0", final.ViewCount, final.ViewsReserved)
	}
}
