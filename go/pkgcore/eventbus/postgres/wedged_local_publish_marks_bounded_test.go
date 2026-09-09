//go:build integration

// The (d) half of the wedged-local-publish regression suite (the rest lives
// in integration_test/, which is package postgres_test): a local Publish
// whose handler never returns must not make the in-process locallyDelivered
// mark map grow without bound. The bound this pins: a per-Type in-flight
// gate that also sat between the poller and its mark pruning --
// deliverPendingForType's only prune of locallyDelivered runs after a
// fetched row's cursor advance, out of reach while the gate held the
// poller out -- would leave every local Publish that completed while a
// same-Type handler was wedged with a mark behind and nothing to prune it
// for as long as the wedge lasted. The poller keeps fetching and advancing
// during the wedge, and each completed local publish's mark is pruned by
// the advance past its own row.
//
// The test asserts the map directly, so it must live in package postgres
// rather than in the integration tier's own postgres_test package; it
// carries the same "integration" build tag and spins the same disposable
// PostgreSQL container (mirroring integration_test/postgres_container_
// test.go's helper, which a different test package cannot share), so a
// plain "go test ./..." never compiles or runs it.
package postgres

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/pkgcore"
)

func TestEventBus_WedgedLocalPublish_LocalMarksStayBounded(t *testing.T) {
	ctx := context.Background()
	pool := startInPackagePostgresPool(t, ctx)

	const (
		eventType = "eventbus_postgres_test.wedged_marks_internal"
		replicaID = "wedged-marks-internal-subscriber"
		wedgeSeq  = float64(100)
	)
	// completedLocalPublishes marks must land while the wedge holds the
	// in-flight gate; markBound is the ceiling the poller's pruning keeps
	// the set under (a completed publish's mark can sit unpruned for at
	// most the instant before the poller's next cycle passes its row).
	const (
		completedLocalPublishes = 15
		markBound               = 3
		firstCompletedSeq       = float64(200)
	)
	// quiescePeriod mirrors the integration tier's reentrantQuiescePeriod
	// (reentrant_publish_test.go): the window a test waits after the last
	// delivery before its exactly-once assertion, long enough that the
	// poller's catch-up cycles (every NOTIFY, every listenBlock timeout)
	// would have surfaced any duplicate.
	const quiescePeriod = 5 * time.Second

	bus := NewEventBus(pool, replicaID)
	t.Cleanup(bus.Close)

	var deliveries atomic.Int64
	bus.Subscribe(eventType, func(context.Context, pkgcore.Event) error {
		deliveries.Add(1)
		return nil
	})

	wedgeCtx, cancelWedge := context.WithCancel(ctx)
	defer cancelWedge()
	releaseWedge := make(chan struct{})
	wedgeEntered := make(chan struct{})
	var enteredOnce sync.Once
	bus.Subscribe(eventType, func(handlerCtx context.Context, evt pkgcore.Event) error {
		if seq, ok := evt.Payload.(map[string]any); ok {
			if s, ok := seq["sequence"].(float64); ok && s == wedgeSeq {
				enteredOnce.Do(func() { close(wedgeEntered) })
				select {
				case <-releaseWedge:
				case <-handlerCtx.Done():
				}
			}
		}
		return nil
	})

	// The poller's pruning advances the cursor per fetched row, so the
	// marks this test leaves behind must land strictly AFTER this replica's
	// cursor exists -- a cursor initialized later at the live end would
	// cover them without the poller ever fetching their rows. The cursor
	// row is created by the listener goroutine's first catch-up scan
	// (ensureCursor), which runs as soon as the listener connects, so
	// waiting for the row's existence is waiting for exactly the right
	// moment -- no warm-up publishes needed.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var exists bool
		err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM pkgcore_eventbus_cursor WHERE replica_id = $1 AND event_type = $2)`,
			replicaID, eventType,
		).Scan(&exists)
		if err == nil && exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the (replicaID, eventType) cursor row was never created: the listener's first catch-up scan did not run (query error: %v)", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Wedge: this instance's own local delivery of wedgeSeq never returns
	// until the test releases it, so the Type's in-flight count stays up
	// for the whole wedge -- the state a count-only gate would withhold
	// on, from before the publish's insert onward.
	publishDone := make(chan error, 1)
	go func() {
		publishDone <- bus.Publish(wedgeCtx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-wedged-marks-internal"),
			Payload:  map[string]any{"sequence": wedgeSeq},
		})
	}()
	select {
	case <-wedgeEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the wedging local handler never ran: the local publish did not reach its handlers")
	}

	// Local publishes that complete while the wedge is on: each runs the
	// Type's handlers synchronously (the wedge handler passes every
	// sequence but wedgeSeq straight through), records its mark in
	// locallyDelivered, and returns. No poller cycle may prune those marks
	// while the wedge lasts -- a count-only gate would stop every cycle;
	// each mark is pruned by the poller's own advance past its row within
	// a wake of the publish.
	for i := 0; i < completedLocalPublishes; i++ {
		if err := bus.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-wedged-marks-internal"),
			Payload:  map[string]any{"sequence": firstCompletedSeq + float64(i)},
		}); err != nil {
			t.Fatalf("Publish() of completed local row %d error = %v, want nil", i, err)
		}
	}

	deadline = time.Now().Add(10 * time.Second)
	for {
		bus.deliverMu.Lock()
		marks := len(bus.locallyDelivered[eventType])
		bus.deliverMu.Unlock()
		if marks <= markBound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("locallyDelivered held %d marks while local publishes completed during the wedge, want at most %d: the poller's pruning never reached them", marks, markBound)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Release the wedge and let the wedged publish complete its mark.
	close(releaseWedge)
	select {
	case err := <-publishDone:
		if err != nil {
			t.Fatalf("the wedged local Publish() error = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wedged local Publish() never returned after its handler was released")
	}

	// Wait out the poller's window, then assert every delivery was exactly
	// once: the wedged row once (its local delivery), each completed local
	// row once -- a duplicate would mean a mark was wrongly pruned while
	// its row was still owed.
	time.Sleep(quiescePeriod)
	bus.deliverMu.Lock()
	marks := len(bus.locallyDelivered[eventType])
	bus.deliverMu.Unlock()
	if got := deliveries.Load(); got != completedLocalPublishes+1 {
		t.Errorf("handlers ran %d times, want exactly %d (no duplicate from the catch-up path)", got, completedLocalPublishes+1)
	}
	if marks > markBound {
		t.Errorf("locallyDelivered held %d marks after the wedge was released, want at most %d", marks, markBound)
	}
}

// startInPackagePostgresPool starts a disposable PostgreSQL 16 container
// and applies this package's own EnsureSchema against it, mirroring
// integration_test/postgres_container_test.go's startPostgresPool exactly
// (that helper is package postgres_test's and cannot be shared with this
// in-package file).
func startInPackagePostgresPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("pkgcore"),
		tcpostgres.WithUsername("pkgcore"),
		tcpostgres.WithPassword("pkgcore"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return pool
}
