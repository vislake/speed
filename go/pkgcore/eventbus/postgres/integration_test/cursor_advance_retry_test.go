//go:build integration

package postgres_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// killEveryPooledConnection forces every connection currently sitting in
// pool's own idle/in-use set to die, unlike terminateAllBackendConnections
// (eventbus_test.go): that helper issues its DELETE-the-backends command
// itself through pool.Exec, which acquires and then releases a pool
// connection to run it -- and pgxpool's LIFO reuse makes that very
// just-released connection the most likely one handed straight back out to
// this test's very next pool.Exec call, silently sparing exactly the
// connection the reproduction needs dead. Dialing a dedicated connection
// OUTSIDE pool (mirroring EventBus's own connectListener in eventbus.go)
// to run the same terminate query, then closing it immediately rather than
// ever returning it to pool, guarantees every connection pool could hand
// out next is genuinely gone.
func killEveryPooledConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	connConfig := pool.Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		t.Fatalf("dial dedicated connection to terminate every pooled backend: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if _, err := conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		  WHERE datname = current_database() AND pid <> pg_backend_pid()`,
	); err != nil {
		t.Fatalf("terminate every pooled backend connection: %v", err)
	}
}

// TestEventBus_CursorAdvanceRetry_ConnectionLossBetweenHandlerAndCursorAdvance_NoDuplicate
// pins the delivery-duplicate hazard of the cursor-advance gap:
// deliverPendingForType (outbox.go) runs a catch-up row's handlers BEFORE
// persisting that the row was delivered (advanceCursorAtLeast). A
// connection failure landing in exactly that gap -- a killed backend, a
// dropped TCP session, nothing more exotic than that -- would leave the
// persisted cursor stuck behind a row whose handlers had already run, and
// the very next catch-up cycle would re-fetch and redeliver the same row:
// a genuine duplicate.
//
// The reproduction here is deterministic, not a timing race: the sabotage
// (killing every backend connection) runs SYNCHRONOUSLY inside the
// subscriber's own handler, in the same call stack deliverPendingForType
// uses to call it, so it always lands in exactly the window the bug needs
// -- after the handler returns, before advanceCursorAtLeast's first
// attempt for that row. Nothing else ever writes this subscriber's cursor
// for eventType (there is exactly one subscriber, and its own catch-up
// loop is the only writer), so there is no way for an unrelated publish to
// mask the bug by advancing the cursor past the poisoned row on its
// behalf -- the only path forward for the cursor is this loop's own
// sequential, one-row-at-a-time advance.
//
// advanceCursorAtLeast's retry (see its own doc comment in outbox.go) is
// what this test actually pins: its very next attempt, a fixed short delay
// later, lands on a freshly dialed, healthy connection -- the server
// itself was never stopped, only every existing backend session was
// killed -- so the cursor is correctly persisted before any later
// catch-up cycle can find it stale. (Reverting advanceCursorAtLeast to a
// single, non-retried attempt makes the poisoned event's delivered count
// settle at 2, not 1.)
func TestEventBus_CursorAdvanceRetry_ConnectionLossBetweenHandlerAndCursorAdvance_NoDuplicate(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "eventbus_postgres_test.cursor_advance_retry"
	const poisonSequence = 7
	const followUpSequence = 8

	publisher := eventbuspostgres.NewEventBus(pool, "cursor-advance-retry-publisher")
	t.Cleanup(publisher.Close)

	sub := eventbuspostgres.NewEventBus(pool, "cursor-advance-retry-subscriber")
	t.Cleanup(sub.Close)

	spy := &eventSpy{}
	sub.Subscribe(eventType, spy.handler())

	var sabotaged atomic.Bool
	sub.Subscribe(eventType, func(handlerCtx context.Context, evt pkgcore.Event) error {
		seq, ok := sequenceOf(evt)
		if ok && seq == poisonSequence && sabotaged.CompareAndSwap(false, true) {
			// Kill every backend connection -- including whichever pooled
			// connection advanceCursorAtLeast's very next call would
			// otherwise have reused -- SYNCHRONOUSLY, before this handler
			// returns and deliverPendingForType moves on to that call.
			// This is what makes the reproduction deterministic rather
			// than a timing race: the sabotage happens inside the same
			// call stack as the vulnerable gap.
			killEveryPooledConnection(t, handlerCtx, pool)
		}
		return nil
	})

	// Warm sub past the first-Subscribe/live-end race (see warmUp's own
	// doc comment) before publishing the real, poisoned event: without
	// this, sub's cursor for eventType might not initialize until after
	// the poisoned row already exists, which would let it start delivery
	// at the live end and skip straight past it instead of ever attempting
	// (and sabotaging) its delivery at all.
	warmUp(t, ctx, publisher, eventType, spy)

	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-cursor-advance-retry"),
		Payload:  map[string]any{"sequence": float64(poisonSequence)},
	}); err != nil {
		t.Fatalf("Publish() of the poisoned event error = %v, want nil", err)
	}

	// sub delivers the poisoned event asynchronously, through its own
	// catch-up scan (NOTIFY, or failing that its own listenBlock timeout):
	// wait for the first delivery before deciding whether a second one
	// ever follows.
	testkit.Eventually(t, "the subscriber's first delivery of the poisoned event", func() bool {
		_, ok := spy.first(func(evt pkgcore.Event) bool {
			seq, ok := sequenceOf(evt)
			return ok && seq == poisonSequence
		})
		return ok
	})

	// Give the listener goroutine's own catch-up scan every reasonable
	// chance to redeliver the poisoned row if advanceCursorAtLeast ever
	// left sub's cursor stale behind it -- comfortably more than one full
	// listenBlock cycle, since a stale cursor is retried on every single
	// one of them.
	time.Sleep(5 * time.Second)

	poisonCount := 0
	spy.mu.Lock()
	for _, evt := range spy.events {
		if seq, ok := sequenceOf(evt); ok && seq == poisonSequence {
			poisonCount++
		}
	}
	spy.mu.Unlock()

	if poisonCount != 1 {
		t.Fatalf("poisoned event delivered %d times, want exactly 1 (a connection loss landing between the handler running and the cursor advance must not redeliver it)", poisonCount)
	}

	// The bus must keep working normally afterward: a plain follow-up
	// publish of the same type must still converge, proving the sabotage
	// did not wedge sub's reader goroutine or its cursor permanently.
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-cursor-advance-retry"),
		Payload:  map[string]any{"sequence": float64(followUpSequence)},
	}); err != nil {
		t.Fatalf("Publish() of the follow-up event error = %v, want nil", err)
	}
	testkit.Eventually(t, "the follow-up event to be delivered", func() bool {
		_, ok := spy.first(func(evt pkgcore.Event) bool {
			seq, ok := sequenceOf(evt)
			return ok && seq == followUpSequence
		})
		return ok
	})
}
