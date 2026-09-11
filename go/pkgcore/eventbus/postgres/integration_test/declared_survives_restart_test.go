//go:build integration

package postgres_test

// The SurvivesRestart half of this implementation's declaration, verified
// at the level pkgcore.Capability's own doc comment demands: the
// component descriptor (component.go) declares MultiReplicaSafe |
// SurvivesRestart (component_test.go pins the descriptor's declaration to
// the exported Capabilities constant), the
// shared eventbustest suite verifies the MultiReplicaSafe half, and this
// file verifies the SurvivesRestart half against a genuine restart of the
// state-holding service. The catch-up proofs elsewhere in this directory
// (TestEventBus_CatchUp_MissedNotifyIsDeliveredAfterReconnect and its
// severed-listener sibling) close and reopen the BUS -- a consumer-process
// restart, which proves the durable-cursor-across-consumer-restart property
// and, per pkgcore.Capability's own definition, nothing about the
// PostgreSQL server behind the bus. The test in this file is the
// service-restart half of that pair: the same durable-cursor protocol with
// a genuine stop/start of the PostgreSQL container itself between the
// downtime publishes and the reconnecting replica.

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// TestEventBus_DeclaredSurvivesRestart_DurableCursorAcrossServerRestart is
// the verification the registration's SurvivesRestart declaration rests on:
// the outbox rows and the per-replicaID cursor row this bus reads and
// writes live in the PostgreSQL server, and committed rows must survive a
// genuine restart of that server. The proof is the file's consumer-restart
// catch-up protocol with a real container restart at the point the
// protocol needs one: a replica's cursor is established at the
// outbox's live end and the replica closes, events are published into the
// downtime window (each Publish commits an outbox row), the PostgreSQL
// container stops and starts again, and a fresh bus under the SAME
// replicaID must catch up every downtime event from its persisted cursor --
// which only survives if the server restart dropped neither the outbox rows
// nor the cursor row. A server that lost committed data (the way an
// unpersisted Redis loses its streams) fails here: the reconnecting replica
// would deliver nothing.
func TestEventBus_DeclaredSurvivesRestart_DurableCursorAcrossServerRestart(t *testing.T) {
	ctx := context.Background()
	container, pool := startPostgresPersistent(t, ctx)

	const eventType = "eventbus_postgres_test.survives-restart"
	const replicaID = "survives-restart-replica"

	// Step 1: bring this replicaID's cursor for eventType into existence at
	// the live end, then close the bus -- a replica that has run before and
	// is now down, the same as a real restart window. The cursor row is
	// created by the listener goroutine's first catch-up scan (see
	// ensureCursor), and the warm-up below can only converge through that
	// scan (see warmUp's own doc comment), so a completed warm-up proves the
	// row exists. Waiting until the persisted cursor has reached the
	// outbox's live end before closing keeps the scan from being severed
	// mid-batch, which would redeliver a marker into the final count (see
	// TestEventBus_CatchUp_MissedNotifyIsDeliveredAfterReconnect's own
	// account of the same wait).
	publisher := eventbuspostgres.NewEventBus(pool, "survives-restart-publisher")
	t.Cleanup(publisher.Close)

	warm := eventbuspostgres.NewEventBus(pool, replicaID)
	warmSpy := testkit.NewEventRecorder()
	warm.Subscribe(eventType, warmSpy.Handler())
	warmUp(t, ctx, publisher, eventType, warmSpy)
	testkit.Eventually(t, "warm's cursor to reach the live end of the outbox before it closes", func() bool {
		var caughtUp bool
		err := pool.QueryRow(ctx,
			`SELECT last_delivered_id >= (SELECT COALESCE(MAX(id), 0) FROM pkgcore_eventbus_outbox WHERE event_type = $1)
			   FROM pkgcore_eventbus_cursor WHERE replica_id = $2 AND event_type = $1`,
			eventType, replicaID,
		).Scan(&caughtUp)
		return err == nil && caughtUp
	})
	warm.Close()

	// Step 2: with no EventBus for replicaID open at all, publish through
	// the unrelated publisher instance. Each Publish commits an outbox row
	// the reconnecting replica below must catch up on.
	const missedDuringDowntime = 3
	for i := 1; i <= missedDuringDowntime; i++ {
		if err := publisher.Publish(ctx, pkgcore.Event{
			Type:     eventType,
			TenantID: pkgcore.TenantID("tenant-restart"),
			Payload:  map[string]any{"sequence": float64(i)},
		}); err != nil {
			t.Fatalf("Publish() during the downtime window error = %v, want nil", err)
		}
	}

	// Step 3: the genuine restart of the state-holding service -- the
	// PostgreSQL container itself, not merely the bus that talks to it. The
	// stop/start keeps the container's data directory, the way a real
	// server restart keeps its on-disk state; what must survive is exactly
	// the committed outbox rows and the replicaID's cursor row.
	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop postgres container: %v", err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatalf("restart postgres container: %v", err)
	}
	waitForPostgresReady(t, ctx, pool)

	// Step 4: reconnect under the SAME replicaID and Subscribe again. The
	// catch-up scan the reconnecting instance's readLoop runs must deliver
	// every one of the missedDuringDowntime events, proving the persisted
	// cursor -- and the outbox rows it points past -- survived the server's
	// genuine restart.
	reconnected := eventbuspostgres.NewEventBus(pool, replicaID)
	t.Cleanup(reconnected.Close)
	spy := testkit.NewEventRecorder()
	reconnected.Subscribe(eventType, spy.Handler())

	testkit.Eventually(t, "the catch-up scan to deliver every event committed before the server restart", func() bool {
		return spy.Total() >= missedDuringDowntime
	})
	if got := spy.Total(); got != missedDuringDowntime {
		t.Fatalf("reconnected replica received %d events, want exactly %d (no duplicate, no loss)", got, missedDuringDowntime)
	}
	for i := 1; i <= missedDuringDowntime; i++ {
		if _, ok := spy.FirstMatch(func(evt pkgcore.Event) bool {
			seq, ok := sequenceOf(evt)
			return ok && seq == float64(i)
		}); !ok {
			t.Errorf("reconnected replica never received the pre-restart event with sequence %d", i)
		}
	}
}
