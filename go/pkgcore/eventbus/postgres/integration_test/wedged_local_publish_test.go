//go:build integration

// Wedged-local-publish regression tests. The scenario every test here
// drives: a Publish on this instance whose synchronous local delivery --
// the handler loop Publish runs on its own goroutine after the outbox
// insert commits -- never returns, because one of its handlers is wedged
// (blocked, never returning until the test releases it). The gate keeps
// the per-Type COUNT (it is what makes a fetch safe against a row
// committed while its publisher's insert has not returned and its id is
// not yet known) but adds a per-row record of the ids whose local delivery
// is in progress, and withholds the Type only while a raised count has no
// matching id recorded. A wedged publish has recorded its row's id long
// before its handlers run, so the scan proceeds, skips exactly the wedged
// row -- whose handlers are running right now -- and delivers and
// advances past every other row. The consequences a count-only gate would
// leave, each pinned by one test below:
//
//   - Rows of the same Type committed by other replicas during the wedge
//     would never be delivered on this one (the gate would return before
//     any fetch), and a PurgeOutboxBefore run would physically delete
//     them while still owed delivery -- real loss, violating at-least-once.
//     (SameTypeRowsStillDelivered, NoRowStillOwedDeliveryIsPurged.)
//
//   - A gate sitting before ensureCursor would leave a Type whose very
//     first catch-up scan was gated without its (replicaID, Type) cursor
//     row; a restart under the same replicaID would then initialize the
//     cursor at the live end and skip every row committed during the
//     stall forever. (FirstScanStillCreatesTheCursorRow.)
//
//   - locallyDelivered, whose only prune runs after a fetched row's cursor
//     advance, would grow without bound while the gate held the poller
//     out. (LocalMarksStayBounded, which lives in the package itself: it
//     asserts in-process state package postgres_test cannot reach.)
package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// wedgeOn registers a handler that wedges -- blocks until release is
// closed, or until its handler context ends so a failed test can never
// wedge a bus Close (the poller-delivery path hands handlers the bus's own
// context; the local-delivery path hands them the publish's) -- on the
// first delivery whose payload sequence is wedgeSeq, and reports that the
// wedge has provably been entered through entered. Every other delivery
// passes straight through, warm-up markers included.
func wedgeOn(t *testing.T, bus *eventbuspostgres.EventBus, eventType string, wedgeSeq float64, release <-chan struct{}, entered chan<- struct{}) {
	t.Helper()
	var enteredOnce sync.Once
	bus.Subscribe(eventType, func(handlerCtx context.Context, evt pkgcore.Event) error {
		if seq, ok := sequenceOf(evt); ok && seq == wedgeSeq {
			enteredOnce.Do(func() { close(entered) })
			select {
			case <-release:
			case <-handlerCtx.Done():
			}
		}
		return nil
	})
}

// publishWedgingLocally publishes an event of eventType through bus's own
// local-delivery path on a background goroutine -- the delivery whose
// wedged handler the tests above revolve around -- and waits until the
// wedge handler has provably entered. From before this publish's outbox
// insert, the Type's in-flight count is up (the state the delivery gate
// keys on), and it stays up until the publish returns, i.e. for the whole
// wedge. The
// returned channel carries the publish's error once the wedge is released.
func publishWedgingLocally(t *testing.T, ctx context.Context, bus *eventbuspostgres.EventBus, eventType string, seq float64, wedgeEntered <-chan struct{}) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- bus.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-wedged-local-publish"),
			Payload:  map[string]any{"sequence": seq},
		})
	}()
	select {
	case <-wedgeEntered:
	case <-time.After(convergenceDeadline):
		t.Fatal("the wedging local handler never ran: the local publish did not reach its handlers")
	}
	return done
}

// releaseWedgeAndAwaitPublish closes release and waits for the wedged
// publish (publishDone) to return, so the test's cleanup path can never
// hang on a wedge that was not released.
func releaseWedgeAndAwaitPublish(t *testing.T, release chan<- struct{}, publishDone <-chan error) {
	t.Helper()
	close(release)
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("the wedged local Publish() error = %v, want nil", err)
		}
	case <-time.After(reentrantDeadlockTimeout):
		t.Fatal("the wedged local Publish() never returned after its handler was released")
	}
}

// TestEventBus_WedgedLocalPublish_SameTypeRowsStillDelivered is the
// primary regression for the per-Type in-flight gate: one local Publish
// whose handler never returns must not suppress delivery of the Type's
// other rows. deliverPendingForType's gate (b.inFlight[eventType] > 0)
// must not abandon the whole Type for as long as the wedged publish's
// count is up -- forever, since the deferred count drop only runs when
// Publish returns and the wedged handler never returns -- which would
// leave a row of the same Type committed by another replica during the
// wedge undelivered on this one. The gate withholds the Type only while a
// raised publish has not yet recorded its row's id, and a wedged
// publish's row id is recorded long before its handlers run, so the scan
// proceeds and skips exactly the wedged row, delivering everything else.
func TestEventBus_WedgedLocalPublish_SameTypeRowsStillDelivered(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "eventbus_postgres_test.wedged_same_type"
	const (
		wedgeSeq  = float64(100) // delivered by the subscriber's own local path, whose handler wedges
		remoteSeq = float64(101) // committed by the other replica while the local handler is wedged
	)

	publisher := eventbuspostgres.NewEventBus(pool, "wedged-same-type-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, "wedged-same-type-subscriber")
	closeBusWithin(t, subscriber)

	spy := testkit.NewEventRecorder()
	subscriber.Subscribe(eventType, spy.Handler())

	wedgeCtx, cancelWedge := context.WithCancel(ctx)
	defer cancelWedge()
	releaseWedge := make(chan struct{})
	wedgeEntered := make(chan struct{})
	wedgeOn(t, subscriber, eventType, wedgeSeq, releaseWedge, wedgeEntered)

	// Warm the subscriber's listener and (replicaID, eventType) cursor past
	// the first-Subscribe/live-end race (see warmUp's own doc comment): the
	// wedge row below must land after the cursor, or the listener's very
	// first catch-up scan would never fetch it. The wedge handler passes
	// the warm-up markers (sequence -1) straight through.
	warmUp(t, ctx, publisher, eventType, spy)

	// Wedge: the subscriber's own local delivery of wedgeSeq runs its
	// handlers synchronously on the publishing goroutine, and the wedge
	// handler never returns until the test releases it.
	publishDone := publishWedgingLocally(t, wedgeCtx, subscriber, eventType, wedgeSeq, wedgeEntered)

	// While the local delivery is provably wedged, another replica commits
	// a row of the SAME Type. A poller returning at the gate on every wake
	// -- NOTIFY and listenBlock timeout alike -- would leave this row
	// undelivered forever; it must arrive while the local handler is still
	// wedged.
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-wedged-same-type"),
		Payload:  map[string]any{"sequence": remoteSeq},
	}); err != nil {
		t.Fatalf("Publish() of the remote row error = %v, want nil", err)
	}

	testkit.Eventually(t, "the remote row of the wedged Type to be delivered while the local handler is still wedged", func() bool {
		counts := receivedSequenceCounts(spy)
		return counts[remoteSeq] == 1
	})

	// Release the wedge: the local publish completes, records its mark, and
	// returns.
	releaseWedgeAndAwaitPublish(t, releaseWedge, publishDone)

	// Wait out the catch-up poller's window, then assert exactly once: the
	// remote row must not be redelivered now that the wedge is gone, and
	// the wedged row must not arrive twice (its delivery was the local
	// path's).
	time.Sleep(reentrantQuiescePeriod)
	counts := receivedSequenceCounts(spy)
	for seq, want := range map[float64]int{wedgeSeq: 1, remoteSeq: 1} {
		if got := counts[seq]; got != want {
			t.Errorf("sequence %v delivered %d times, want exactly %d (no loss, no duplicate across the local and catch-up delivery paths)", seq, got, want)
		}
	}
}

// TestEventBus_WedgedLocalPublish_NoRowStillOwedDeliveryIsPurged pins the
// loss half of the delivery guarantee: rows of a wedged Type committed by
// other replicas are owed delivery on this one, and this package's
// documented retention mechanism, PurgeOutboxBefore, deletes by age alone
// -- so rows a count-only gate left undelivered for the wedge's whole
// duration would be physically deleted by a purge whose window the wedge
// outlasts: real loss, violating at-least-once. The rows must be delivered
// by the poller while the wedge is still on, well within the sleep below,
// so by the time the purge runs nothing owed is left to delete -- the
// delivered rows are simply aged
// out afterwards, exactly as the purge is documented to.
func TestEventBus_WedgedLocalPublish_NoRowStillOwedDeliveryIsPurged(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const eventType = "eventbus_postgres_test.wedged_purge"
	const (
		wedgeSeq = float64(100)
		firstSeq = float64(101)
		lastSeq  = float64(103)
	)
	// purgeWindow is what the host-scheduled PurgeOutboxBefore call below
	// cuts off against; the wedge-era rows must age past it before the
	// purge runs, while the poller's delivery of them (one wake at most,
	// the notify their commits fire) must be long done. The sleep
	// below, a full reentrantQuiescePeriod past the window, is comfortable
	// for both.
	const purgeWindow = 2 * time.Second

	publisher := eventbuspostgres.NewEventBus(pool, "wedged-purge-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, "wedged-purge-subscriber")
	closeBusWithin(t, subscriber)

	spy := testkit.NewEventRecorder()
	subscriber.Subscribe(eventType, spy.Handler())

	wedgeCtx, cancelWedge := context.WithCancel(ctx)
	defer cancelWedge()
	releaseWedge := make(chan struct{})
	wedgeEntered := make(chan struct{})
	wedgeOn(t, subscriber, eventType, wedgeSeq, releaseWedge, wedgeEntered)

	warmUp(t, ctx, publisher, eventType, spy)

	publishDone := publishWedgingLocally(t, wedgeCtx, subscriber, eventType, wedgeSeq, wedgeEntered)

	// Rows of the wedged Type committed by another replica while the local
	// delivery is wedged.
	for seq := firstSeq; seq <= lastSeq; seq++ {
		if err := publisher.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-wedged-purge"),
			Payload:  map[string]any{"sequence": seq},
		}); err != nil {
			t.Fatalf("Publish() of wedge-era row %v error = %v, want nil", seq, err)
		}
	}

	// Age the wedge-era rows past the purge window, then run the
	// host-scheduled purge -- a purge that removed rows this replica still
	// owes delivery of would be the loss under test.
	time.Sleep(purgeWindow + reentrantQuiescePeriod)
	if _, err := eventbuspostgres.PurgeOutboxBefore(ctx, pool, purgeWindow); err != nil {
		t.Fatalf("PurgeOutboxBefore() error = %v, want nil", err)
	}

	releaseWedgeAndAwaitPublish(t, releaseWedge, publishDone)

	// Every wedge-era row must still be delivered exactly once: they are
	// delivered while the wedge is on, so the purge only ages out
	// already-handled rows -- rows the purge deleted while still owed
	// would never arrive.
	testkit.Eventually(t, "every wedge-era row to be delivered despite PurgeOutboxBefore having run once they were older than its window", func() bool {
		counts := receivedSequenceCounts(spy)
		for seq := firstSeq; seq <= lastSeq; seq++ {
			if counts[seq] != 1 {
				return false
			}
		}
		return true
	})
	time.Sleep(reentrantQuiescePeriod)
	counts := receivedSequenceCounts(spy)
	for seq := wedgeSeq; seq <= lastSeq; seq++ {
		if got := counts[seq]; got != 1 {
			t.Errorf("sequence %v delivered %d times, want exactly 1 (no loss to the purge, no duplicate)", seq, got)
		}
	}
}

// TestEventBus_WedgedLocalPublish_FirstScanStillCreatesTheCursorRow pins
// the cursor-row creation guarantee: the delivery gate must not sit BEFORE
// ensureCursor (deliverPendingForType's only call site) -- a Type whose
// very first catch-up scan was gated by a wedged local publish would never
// get its (replicaID, eventType) cursor row created at all, and a restart
// under the same replicaID would then initialize the cursor at the live
// end (see ensureCursor), skipping every row committed during the wedge
// forever: the cross-restart durability guarantee this backend exists for,
// silently voided by the wedge. The gate passes once the wedged publish's
// row id is recorded, so the first scan -- which the wedged publish's own
// NOTIFY wakes -- reaches ensureCursor even while the handler is wedged.
//
// The wedge must precede the Type's first-ever scan, or the cursor row
// could exist for the ordinary reason and the test would prove nothing. The ordering is made deterministic by phasing, not by
// timing luck: the subscriber's listener is warmed on an unrelated Type,
// a phase marker then pins the moment of the listener's most recent wake,
// and Subscribe of the wedged Type plus the wedging local publish complete
// microseconds later -- well inside the listener's following listenBlock
// wait, whose timeout is the only wake that could intervene (no other
// publish fires between the phase marker and the wedge row's own commit).
// The first wake that scans the wedged Type is therefore the wedged
// publish's own NOTIFY, which by construction fires after that publish
// raised its in-flight count.
func TestEventBus_WedgedLocalPublish_FirstScanStillCreatesTheCursorRow(t *testing.T) {
	ctx := context.Background()
	pool := startPostgresPool(t, ctx)

	const (
		warmType    = "eventbus_postgres_test.wedged_cursor_warm"
		eventType   = "eventbus_postgres_test.wedged_cursor_first_scan"
		replicaID   = "wedged-cursor-subscriber"
		wedgeSeq    = float64(1)
		phaseMarker = float64(-2) // never collides with warmUp's -1 or the wedge row
	)

	publisher := eventbuspostgres.NewEventBus(pool, "wedged-cursor-publisher")
	closeBusWithin(t, publisher)

	subscriber := eventbuspostgres.NewEventBus(pool, replicaID)
	closeBusWithin(t, subscriber)

	// Start and warm the subscriber's listener on an unrelated Type, purely
	// as a clock: the warm spy's delivery of the phase marker below proves
	// the listener was awake at that moment, and its next wake is at least
	// one listenBlock away.
	warmSpy := testkit.NewEventRecorder()
	subscriber.Subscribe(warmType, warmSpy.Handler())
	warmUp(t, ctx, publisher, warmType, warmSpy)

	markersBeforePhase := warmSpy.Total()
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:    warmType,
		Payload: map[string]any{"sequence": phaseMarker},
	}); err != nil {
		t.Fatalf("Publish() of the phase marker error = %v, want nil", err)
	}
	testkit.Eventually(t, "the phase marker to be delivered (pinning the listener's most recent wake)", func() bool {
		return warmSpy.Total() > markersBeforePhase
	})

	// Now the wedge, on the Type whose cursor row must still be created.
	// Subscribe and the wedging publish run back-to-back here, within the
	// listener's current listenBlock wait, so no wake can scan eventType
	// before the wedge row's own NOTIFY does.
	spy := testkit.NewEventRecorder()
	subscriber.Subscribe(eventType, spy.Handler())

	wedgeCtx, cancelWedge := context.WithCancel(ctx)
	defer cancelWedge()
	releaseWedge := make(chan struct{})
	wedgeEntered := make(chan struct{})
	wedgeOn(t, subscriber, eventType, wedgeSeq, releaseWedge, wedgeEntered)
	publishDone := publishWedgingLocally(t, wedgeCtx, subscriber, eventType, wedgeSeq, wedgeEntered)

	// While the local handler is provably wedged -- the state in which a
	// count-only gate would pin the poller -- the (replicaID, eventType)
	// cursor row must still come into existence. The wedged publish's own
	// NOTIFY wakes the listener and the gate lets that first scan reach
	// ensureCursor; a scan that returned at the gate would leave the row
	// never created, so a restart under the same replicaID would
	// initialize the cursor at the live end and skip the wedge-era rows
	// forever.
	testkit.Eventually(t, "the cursor row to exist even though the Type's first-ever catch-up scan is blocked by a wedged local publish", func() bool {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pkgcore_eventbus_cursor WHERE replica_id = $1 AND event_type = $2)`,
			replicaID, eventType,
		).Scan(&exists)
		return err == nil && exists
	})

	releaseWedgeAndAwaitPublish(t, releaseWedge, publishDone)

	// Wait out the catch-up poller's window, then assert the wedged row was
	// delivered exactly once, by its own local path.
	time.Sleep(reentrantQuiescePeriod)
	if got := spy.Total(); got != 1 {
		t.Errorf("the wedged row's handler ran %d times, want exactly 1", got)
	}
}
