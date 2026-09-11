//go:build integration

package redis_test

// The SurvivesRestart half of this implementation's declaration, verified:
// the component descriptor (component.go) declares MultiReplicaSafe |
// SurvivesRestart (component_test.go pins the descriptor's declaration to
// the exported Capabilities constant), the
// shared eventbustest suite verifies the MultiReplicaSafe half, and this
// file verifies the SurvivesRestart half against a genuine restart of the
// state-holding service -- the Redis container itself, restarted between a
// committed publish and the read-back of the committed stream state. See
// EventBus's own package doc comment for what the declaration promises (and
// what it does not): the bus keeps no state of its own; the stream entries
// and consumer-group cursors it reads and writes live inside the Redis
// server, and whether they outlive the server's restart is the server's
// operator-configured persistence, which this test's fixture forces
// ("--save 1 1") exactly the way an operator must configure a real
// deployment.

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	"github.com/vislake/speed/go/pkgcore/redistest"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// TestEventBus_DeclaredSurvivesRestart_CommittedStateSurvivesServerRestart
// is the verification the registration's SurvivesRestart declaration rests
// on: state the bus committed through the Redis server before a genuine
// restart of that server must still be there afterwards, and the live bus
// must resume ordinary operation across the restart. The proof has teeth
// against exactly the failure mode the declaration's premise names: a Redis
// server running without persistence (no RDB snapshot points, no AOF —
// "--save \"\"") loses every stream when it restarts, so the post-restart
// state assertions below fail against such a server (the stream is simply
// gone) and pass only against one whose persistence is configured: the
// fixture's forced one-second RDB snapshotting, the configuration an
// operator must provide for the declaration to be true of a real
// deployment.
func TestEventBus_DeclaredSurvivesRestart_CommittedStateSurvivesServerRestart(t *testing.T) {
	ctx := context.Background()
	container, client := redistest.Persistent(t, ctx)

	const eventType = "eventbus_redis_test.survives-restart"
	publisher := eventbusredis.NewEventBus(client)
	receiver := eventbusredis.NewEventBus(client)
	t.Cleanup(publisher.Close)
	t.Cleanup(receiver.Close)

	rec := testkit.NewEventRecorder()
	receiver.Subscribe(eventType, rec.Handler())

	// Warm up: proves the receiver's consumer group exists on the stream and
	// its reader is live (see warmUp's own doc comment for why a single
	// publish-and-wait would not), so the counted event below is delivered
	// and the group's cursor advanced before the restart.
	warmUp(t, publisher, rec, eventType)
	rec.Clear()

	// Commit the event whose survival the restart must not disturb. Its
	// delivery to the receiver is asserted first, so the state captured
	// below is the steady state a live, caught-up reader left behind.
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-restart"),
		Payload:  invoicePaid{ID: "inv-restart", Amount: 7},
	}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	testkit.EventuallyWithin(t, 5*time.Second, "the receiver to deliver the pre-restart event", func() bool {
		return rec.Total() == 1
	})

	// Capture the committed server-side state the declaration promises will
	// outlive the restart: the stream's entries and the receiver's consumer
	// group with its stored cursor. The captured values are the pre-restart
	// baseline the post-restart assertions compare against.
	stream := streamKey(eventType)
	preEntries, err := client.XRange(ctx, stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRANGE before the restart: %v", err)
	}
	if len(preEntries) == 0 {
		t.Fatal("XRANGE before the restart returned no entries, want the committed event present")
	}
	preGroups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS before the restart: %v", err)
	}
	if len(preGroups) != 1 {
		t.Fatalf("XINFO GROUPS before the restart reports %d groups, want the receiver's single one", len(preGroups))
	}

	// The genuine restart of the state-holding service. The fixture forces
	// "save 1 1": the snapshot fires within about a second of the commit.
	// Waiting it out before the stop makes the proof rest on a snapshot that
	// demonstrably landed, not on a shutdown-time save that could mask a
	// backend which only looks durable while it is running.
	time.Sleep(1500 * time.Millisecond)
	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop redis container: %v", err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatalf("restart redis container: %v", err)
	}
	redistest.WaitReady(t, ctx, client)

	// The committed stream state must still be there. These are the
	// assertions that fail against an unpersisted server: its restart drops
	// the stream (and the group with it) entirely, so the entries committed
	// before the restart are gone and a recreated group would start at the
	// live end -- the exact loss the declaration's recorded premise says an
	// operator-configured persistence prevents.
	postEntries, err := client.XRange(ctx, stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRANGE after the restart: %v", err)
	}
	if len(postEntries) != len(preEntries) {
		t.Fatalf("stream carries %d entries after the restart, want the %d committed before it: the committed stream state did not survive the server restart", len(postEntries), len(preEntries))
	}
	if !reflect.DeepEqual(postEntries, preEntries) {
		t.Errorf("stream entries after the restart differ from the entries committed before it:\nbefore: %v\nafter:  %v", preEntries, postEntries)
	}
	postGroups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XINFO GROUPS after the restart: %v", err)
	}
	if len(postGroups) != 1 {
		t.Fatalf("XINFO GROUPS after the restart reports %d groups, want the receiver's single one to have survived", len(postGroups))
	}
	if postGroups[0].Name != preGroups[0].Name || postGroups[0].LastDeliveredID != preGroups[0].LastDeliveredID {
		t.Errorf("consumer group after the restart = (name %q, last-delivered %q), want (name %q, last-delivered %q): the group's stored cursor did not survive the server restart",
			postGroups[0].Name, postGroups[0].LastDeliveredID, preGroups[0].Name, preGroups[0].LastDeliveredID)
	}

	// And the live bus must resume ordinary operation across the restart:
	// its reader survived the server's downtime (go-redis redials the fixed
	// advertised address on its own) and delivers a post-restart publish.
	rec.Clear()
	if err := publisher.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("tenant-restart"),
		Payload:  invoicePaid{ID: "inv-restart-after", Amount: 8},
	}); err != nil {
		t.Fatalf("Publish() after the restart error = %v, want nil", err)
	}
	testkit.EventuallyWithin(t, 5*time.Second, "the receiver to deliver the post-restart event", func() bool {
		return rec.Total() == 1
	})
}
