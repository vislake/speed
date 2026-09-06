//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/vislake/speed/go/dbkit"
)

// TestIsRetryableConflict_RealPostgresDeadlock forces a genuine, real
// PostgreSQL deadlock (SQLSTATE 40P01) out of two transactions that lock two
// rows in reverse order of each other -- the classic shape a concurrent
// org.TreeService.Move pair can hit locking overlapping subtree rows in
// different orders (see go/org/tree.go's Move doc comment) -- and proves
// dbkit.IsRetryableConflict recognizes the real error PostgreSQL's own
// deadlock detector returns, not a synthetic stand-in for it.
//
// PostgreSQL itself breaks the cycle: one of the two transactions below is
// aborted with the deadlock error, and the other proceeds and commits
// normally. Which one loses is not deterministic and is not asserted here;
// only that whichever one loses gets an error IsRetryableConflict classifies
// as retryable.
func TestIsRetryableConflict_RealPostgresDeadlock(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	const table = "retry_deadlock_probe"
	mustExec(t, db, ctx, fmt.Sprintf(`CREATE TABLE %s (id text PRIMARY KEY, v text NOT NULL)`, table))
	mustExec(t, db, ctx, fmt.Sprintf(`INSERT INTO %s (id, v) VALUES ('row-1', 'a'), ('row-2', 'a')`, table))

	// txA locks row-1 then, after a short delay to guarantee txB has locked
	// row-2 first, tries to lock row-2 -- the reverse of txB's own order.
	txA, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	txB, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}

	if _, err := txA.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET v = v WHERE id = 'row-1'`, table)); err != nil {
		t.Fatalf("A lock row-1: %v", err)
	}
	if _, err := txB.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET v = v WHERE id = 'row-2'`, table)); err != nil {
		t.Fatalf("B lock row-2: %v", err)
	}

	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errA = txA.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET v = v WHERE id = 'row-2'`, table))
	}()
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond) // let A's cross-lock attempt block first
		_, errB = txB.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET v = v WHERE id = 'row-1'`, table))
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("neither transaction resolved within 20s -- PostgreSQL's deadlock detector should have fired well before this")
	}
	_ = txA.Rollback()
	_ = txB.Rollback()

	loser := errA
	if loser == nil {
		loser = errB
	}
	if loser == nil {
		t.Fatal("both transactions succeeded -- expected PostgreSQL's deadlock detector to abort exactly one of them")
	}
	if !errors.Is(loser, context.Canceled) && !dbkit.IsRetryableConflict(loser) {
		t.Fatalf("IsRetryableConflict(%v) = false, want true for a real PostgreSQL deadlock", loser)
	}
	t.Logf("loser's real error: %v", loser)
}
