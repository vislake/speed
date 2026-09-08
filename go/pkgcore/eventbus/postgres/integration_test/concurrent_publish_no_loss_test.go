//go:build integration

// No-loss regression tests for a gap in this package's at-least-once
// semantics that its earlier cursor-advance design left open: a replica's
// persisted cursor (pkgcore_eventbus_cursor) is a single watermark advanced
// PAST rows whose delivery has completed, and the outbox row id is
// allocated by the shared IDENTITY sequence at INSERT time, while the
// row's transaction commits later -- so under concurrent publishers, commit
// order can diverge from id order, and an advance to a row id N implicitly
// claims every row in (cursor, N] is delivered when rows smaller than N can
// genuinely still be uncommitted (or committed but not yet picked up by
// this replica's lagging listener). Such rows are never fetched again --
// the catch-up scan only ever asks for ids above the cursor -- so they are
// lost forever, violating the at-least-once contract every doc comment in
// this package claims.
//
// The delivery design has two halves, both pinned by the tests below:
//
//   - Publish serializes same-type outbox inserts with a per-event-type
//     pg_advisory_xact_lock taken inside the publish transaction (see
//     insertOutboxAndNotify in outbox.go), so same-type commit order equals
//     id order and a catch-up batch that advances to its largest row id can
//     never leap over an uncommitted smaller-id row. This is the
//     multi-writer shape TestEventBus_ConcurrentMultiReplicaPublish_
//     NoEventLostNoDuplicate drives.
//
//   - Publish's own synchronous local-delivery path never advances the
//     cursor itself: it records the row id in an in-process set instead,
//     and the listener's catch-up loop is the sole cursor advancer, simply
//     skipping a row it finds already marked rather than re-running its
//     handlers. A local publish can therefore never advance the persisted
//     cursor past rows of the same type that are committed but not yet
//     picked up by this replica's own (lagging, disconnected, or simply
//     busy) listener. This is the lagging-poller shape
//     TestEventBus_LaggingPoller_LocalPublishMustNotSkipUnseenRemoteRows
//     drives deterministically.
//
// Both tests assert delivery of every published event EXACTLY once -- no
// loss and no duplicate -- which is what the healthy (no crash, no
// connection loss) path of an at-least-once bus must provide; the crash
// windows that degrade the guarantee to at-least-once are documented on
// EventBus and covered by this package's other tests.
package postgres_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
)

// receivedSequenceCounts snapshots the events a spy has received so far
// into a per-sequence delivery count, ignoring the -1 warm-up markers
// warmUp republishes (see its own doc comment), so an assertion can check a
// whole batch of published sequences at once: every expected sequence
// present exactly once means no event was lost and none was duplicated.
func receivedSequenceCounts(spy *eventSpy) map[float64]int {
	spy.mu.Lock()
	defer spy.mu.Unlock()
	counts := make(map[float64]int)
	for _, evt := range spy.events {
		if seq, ok := sequenceOf(evt); ok && seq != -1 {
			counts[seq]++
		}
	}
	return counts
}

// TestEventBus_LaggingPoller_LocalPublishMustNotSkipUnseenRemoteRows
// deterministically reproduces the "local advance skips rows the poller
// has not delivered yet" loss shape: while this instance's listener
// goroutine is busy inside a slow handler (and therefore cannot fetch
// anything), another replica commits a row of the same type, and then THIS
// instance's own Publish path runs its local handlers for a later row and
// advances the persisted cursor past it. A cursor advance leaping over
// the still-undelivered remote row in one step would mean the catch-up
// scan never fetches it again (it only asks for ids above the cursor),
// and the remote event would be lost forever. The local Publish does not
// advance the cursor at all -- the row is marked in
// memory and the listener, once its slow handler returns, delivers the
// remote row and skips the already-locally-delivered one.
//
// The reproduction is deterministic, not a timing race: the listener is
// parked inside a handler for a blocking row it has already fetched, so
// the remote row's commit provably happens after that fetch's snapshot and
// provably before the local publish's cursor advance -- the exact window
// the bug needs, with no scheduling luck involved. The listener's handler
// unblocks on the handler context's cancellation too, so a test failure
// can never wedge Close().
func TestEventBus_LaggingPoller_LocalPublishMustNotSkipUnseenRemoteRows(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "eventbus_postgres_test.lagging_poller_local_advance"
	const (
		blockSeq  = float64(100) // delivered by the poller, whose handler parks the listener goroutine
		remoteSeq = float64(101) // committed by the other replica while the listener is parked
		localSeq  = float64(102) // published locally (and delivered synchronously) while the listener is parked
	)

	publisher := eventbuspostgres.NewEventBus(pool, "lagging-poller-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, "lagging-poller-subscriber")
	closeBusWithin(t, subscriber)

	spy := &eventSpy{}
	subscriber.Subscribe(eventType, spy.handler())

	blockEntered := make(chan struct{})
	releaseBlockedHandler := make(chan struct{})
	var enteredOnce sync.Once
	subscriber.Subscribe(eventType, func(handlerCtx context.Context, evt pkgcore.Event) error {
		if seq, ok := sequenceOf(evt); ok && seq == blockSeq {
			enteredOnce.Do(func() { close(blockEntered) })
			// Park the listener goroutine INSIDE this handler -- which is to
			// say, between its fetch of this row and its cursor advance for
			// it -- until the test releases it (or the bus closes, which
			// cancels handlerCtx and must also unblock this select so a
			// failed test cannot wedge Close).
			select {
			case <-releaseBlockedHandler:
			case <-handlerCtx.Done():
			}
		}
		return nil
	})

	// Warm the subscriber's listener and (replicaID, eventType) cursor past
	// the first-Subscribe/live-end race (see warmUp's own doc comment): the
	// blocking row below must land AFTER the cursor, or the listener's very
	// first catch-up scan would never fetch it.
	warmUp(t, ctx, publisher, eventType, spy)

	// 1. The blocking row: the subscriber's listener fetches it and parks
	// inside its handler, holding the listener goroutine out of every
	// further fetch.
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-lagging-poller"),
		Payload:  map[string]any{"sequence": blockSeq},
	}); err != nil {
		t.Fatalf("Publish() of the blocking row error = %v, want nil", err)
	}
	select {
	case <-blockEntered:
	case <-time.After(convergenceDeadline):
		t.Fatalf("the blocking handler never ran: the subscriber's listener did not fetch the blocking row")
	}

	// 2. While the listener is provably parked: another replica commits a
	// row the subscriber's listener has never fetched (its fetch snapshot
	// for the blocking row predates this commit).
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-lagging-poller"),
		Payload:  map[string]any{"sequence": remoteSeq},
	}); err != nil {
		t.Fatalf("Publish() of the remote row error = %v, want nil", err)
	}

	// 3. ... and THIS instance publishes a later row of the same type
	// through its own local-delivery path. That path must not advance the
	// persisted cursor past its row in one GREATEST step, leaping over the
	// remote row committed a moment earlier -- the loss this test exists
	// to pin -- so it only marks the row in memory.
	if err := subscriber.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-lagging-poller"),
		Payload:  map[string]any{"sequence": localSeq},
	}); err != nil {
		t.Fatalf("Publish() of the local row error = %v, want nil", err)
	}

	// Release the parked handler: the listener finishes the blocking row,
	// advances past it, and -- on the fixed code -- picks up the remote row
	// on its next catch-up scan, skipping the already-marked local row.
	close(releaseBlockedHandler)

	eventually(t, "the remote row to be delivered despite the local publish that advanced the cursor past it", func() bool {
		counts := receivedSequenceCounts(spy)
		return counts[blockSeq] == 1 && counts[remoteSeq] == 1 && counts[localSeq] == 1
	})

	// Wait out the catch-up poller's window, then assert each row was
	// delivered exactly once: the local row must not be redelivered by the
	// poller now that its cursor covers it (the in-memory mark must hold),
	// and the remote row must not arrive twice.
	time.Sleep(reentrantQuiescePeriod)
	counts := receivedSequenceCounts(spy)
	for seq, want := range map[float64]int{blockSeq: 1, remoteSeq: 1, localSeq: 1} {
		if got := counts[seq]; got != want {
			t.Errorf("sequence %v delivered %d times, want exactly %d (no loss, no duplicate across the local and catch-up delivery paths)", seq, got, want)
		}
	}
}

// TestEventBus_ConcurrentMultiReplicaPublish_NoEventLostNoDuplicate is the
// regression shape for concurrent publishers: two replicas publishing
// the same
// event type concurrently, the subscribing replica among them (so its own
// local-delivery path interleaves with the catch-up poller) -- with enough
// overlapping transactions that commit order provably diverges from id
// order without serialization. When that divergence lands inside a
// catch-up batch -- the batch's snapshot sees a larger-id row committed
// while a
// smaller-id row of the same type is still uncommitted -- an advance
// to the batch's largest id would skip the smaller row forever. The
// per-type advisory transaction lock serializes same-type
// commits into id order, so no catch-up advance can ever leap over an
// uncommitted row, and the local-delivery path never advances the cursor
// at all. Every concurrently published event must therefore reach the
// subscribing replica's handler exactly once.
func TestEventBus_ConcurrentMultiReplicaPublish_NoEventLostNoDuplicate(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "eventbus_postgres_test.concurrent_multi_writer"
	const (
		remoteWorkers   = 12 // goroutines publishing on the non-subscribing replica
		localWorkers    = 12 // goroutines publishing on the subscribing replica itself
		eventsPerWorker = 6
	)
	totalEvents := (remoteWorkers + localWorkers) * eventsPerWorker

	publisher := eventbuspostgres.NewEventBus(pool, "concurrent-multi-writer-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, "concurrent-multi-writer-subscriber")
	closeBusWithin(t, subscriber)

	spy := &eventSpy{}
	subscriber.Subscribe(eventType, spy.handler())
	warmUp(t, ctx, publisher, eventType, spy)

	var nextSeq atomic.Int64
	var wg sync.WaitGroup
	errCh := make(chan error, totalEvents)
	publishLoop := func(bus *eventbuspostgres.EventBus) {
		defer wg.Done()
		for i := 0; i < eventsPerWorker; i++ {
			seq := float64(nextSeq.Add(1))
			if err := bus.Publish(ctx, pkgcore.Event{
				Type:     eventType,
				TenantID: pkgcore.TenantID("tenant-concurrent-multi-writer"),
				Payload:  map[string]any{"sequence": seq},
			}); err != nil {
				errCh <- err
				return
			}
		}
	}
	for i := 0; i < remoteWorkers; i++ {
		wg.Add(1)
		go publishLoop(publisher)
	}
	for i := 0; i < localWorkers; i++ {
		wg.Add(1)
		go publishLoop(subscriber)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Publish() error = %v, want nil", err)
	}
	if t.Failed() {
		return
	}

	eventually(t, "every concurrently published event to be delivered exactly once across both replicas", func() bool {
		counts := receivedSequenceCounts(spy)
		if len(counts) != totalEvents {
			return false
		}
		for _, delivered := range counts {
			if delivered != 1 {
				return false
			}
		}
		return true
	})
}
