//go:build integration

package redis_test

// Integration tests pinning the BUSYGROUP recovery: a reader whose consumer
// group already exists when it starts adopts the group and starts consuming
// instead of retrying the creation into BUSYGROUP forever -- the recovery
// the implementation's createGroup documents. The "already exists" outcome
// arises when the server executed the reader's own XGROUP CREATE but its
// success reply was lost (a dropped connection or a failover), which leaves
// the group -- the durable consumer-group state -- existing while this
// reader believes its creation never succeeded; the naming the bus uses (one
// group per stream per instance, the instance id a random 96-bit value) means
// no other actor can ever create or destroy that group, so a reader that
// retried BUSYGROUP would spin for the rest of the process's life, silently
// delivering nothing of the type while Publish kept succeeding. The
// lost-response shape cannot be produced by actually losing a reply in a
// test, so it is simulated deterministically: the group the reader is about
// to create is pre-created through a raw client under the name the reader
// will use, which puts the reader's very first creation attempt on the
// BUSYGROUP path. The group name is not observable before the reader exists
// (the instance id is random and no API exposes it), so each test first has
// the bus publish one probe entry and reads the "src" field back: the
// instance id stamped on every entry is part of the bus's documented wire
// format, the same deliberate pin the streamKey helper in this directory's
// eventbus_test.go makes for the stream prefix.

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	eventbusredis "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	"github.com/vislake/speed/go/pkgcore/redistest"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// precreateReaderGroup makes the consumer group bus's reader will create on
// the eventType stream already exist before the reader's first creation
// attempt. The probe entry stays in the stream, but the group is created at
// the live end ("$") -- exactly where the reader's own creation would put it
// -- so the probe is history for the group and is never delivered.
func precreateReaderGroup(t *testing.T, ctx context.Context, client *redis.Client, bus *eventbusredis.EventBus, eventType string) {
	t.Helper()

	if err := bus.Publish(ctx, pkgcore.Event{
		Type:     eventType,
		TenantID: pkgcore.TenantID("group-probe"),
		Payload:  map[string]any{"probe": true},
	}); err != nil {
		t.Fatalf("probe publish: %v", err)
	}
	entries, err := client.XRange(ctx, streamKey(eventType), "-", "+").Result()
	if err != nil {
		t.Fatalf("XRANGE the probe entry: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("XRANGE returned no entry for the probe publish")
	}
	src, _ := entries[0].Values["src"].(string)
	if src == "" {
		t.Fatalf("probe entry carries no src field: %+v", entries[0])
	}
	group := "pkgcore:bus:" + src
	if err := client.XGroupCreateMkStream(ctx, streamKey(eventType), group, "$").Err(); err != nil {
		t.Fatalf("pre-create group %q on stream %q: %v", group, streamKey(eventType), err)
	}
}

// TestEventBus_ReaderAdoptsAPreexistingGroup pins the lost-response recovery:
// a consumer group that already exists when the reader starts must not wedge
// the reader. Before the fix, createGroup answered BUSYGROUP by retrying the
// creation forever -- the group can never cease to exist under this reader,
// since the per-instance naming rules out every other actor -- so the reader
// never consumed, and the marker loop below timed out with zero deliveries
// while every Publish succeeded: the silent lifetime stall the recovery
// rewrite removes. After the fix the reader adopts the existing group
// (creation has effectively succeeded: the group is the durable
// consumer-group state and it exists) and events published after its start
// are delivered normally.
func TestEventBus_ReaderAdoptsAPreexistingGroup(t *testing.T) {
	ctx := context.Background()
	client := redistest.Client(t, ctx)
	busA := eventbusredis.NewEventBus(client) // publisher only: subscribes no handler
	busB := eventbusredis.NewEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
	})

	recB := testkit.NewEventRecorder()
	const paidType = "invoice.paid"

	// The lost response, simulated: the group bus B's reader is about to
	// create already exists when the reader starts.
	precreateReaderGroup(t, ctx, client, busB, paidType)
	busB.Subscribe(paidType, recB.Handler())

	// bus B is the only subscriber, so every event bus A publishes must reach
	// recB through bus B's reader. A reader wedged on BUSYGROUP delivers none
	// of them and the deadline fails; an adopting reader takes the first
	// marker published after the subscribe (the group's cursor sits at the
	// live end from the pre-creation), so the loop below succeeds within a
	// second or two.
	deadline := time.Now().Add(5 * time.Second)
	for seq := 1; recB.Total() == 0; seq++ {
		if time.Now().After(deadline) {
			t.Fatal("timed out: no event reached a reader whose consumer group pre-existed its start -- the reader wedged retrying BUSYGROUP instead of adopting the group")
		}
		if err := busA.Publish(ctx, pkgcore.Event{
			Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"),
			Payload: invoicePaid{ID: "inv-adopted", Amount: float64(seq)},
		}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil", seq, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// The reader stayed healthy past the adoption rather than delivering a
	// single straggler: one more counted event reaches it.
	recB.Clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-after-adoption", Amount: 2},
	}); err != nil {
		t.Fatalf("Publish(after adoption) error = %v, want nil", err)
	}
	testkit.EventuallyWithin(t, 5*time.Second, "the adopting reader to keep delivering", func() bool {
		return recB.Total() == 1
	})
}

// TestEventBus_AdoptedGroupCoexistsWithLaterReaders pins the adoption's outer
// boundary: a stream whose first reader adopted a pre-existing group stays an
// ordinary stream. A second, later reader creates its own group on it and
// both readers consume every event once each; and the adopting instance's
// Close destroys the group it adopted while sparing the later reader's, which
// keeps delivering. Before the fix this fails on the first reader's leg
// exactly like the adoption test above: the adopting reader never consumes,
// so the two-reader marker handshake below times out.
func TestEventBus_AdoptedGroupCoexistsWithLaterReaders(t *testing.T) {
	ctx := context.Background()
	client := redistest.Client(t, ctx)
	busA := eventbusredis.NewEventBus(client) // publisher only: subscribes no handler
	busB := eventbusredis.NewEventBus(client)
	busC := eventbusredis.NewEventBus(client)
	t.Cleanup(func() {
		busA.Close()
		busB.Close()
		busC.Close()
	})

	recB, recC := testkit.NewEventRecorder(), testkit.NewEventRecorder()
	const paidType = "invoice.paid"

	// bus B's group pre-exists and will be adopted; bus C's does not and will
	// be created fresh.
	precreateReaderGroup(t, ctx, client, busB, paidType)
	busB.Subscribe(paidType, recB.Handler())
	busC.Subscribe(paidType, recC.Handler())

	// Markers from bus A must reach both readers before any counted event is
	// published: bus C's group is created asynchronously after its Subscribe,
	// and a marker published before that creation is history for bus C. A
	// reader wedged on BUSYGROUP never counts a marker and fails the deadline.
	deadline := time.Now().Add(5 * time.Second)
	for seq := 1; recB.Total() == 0 || recC.Total() == 0; seq++ {
		if time.Now().After(deadline) {
			t.Fatal("timed out: no marker reached both readers -- the reader with the pre-existing group wedged on BUSYGROUP")
		}
		if err := busA.Publish(ctx, pkgcore.Event{
			Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"),
			Payload: invoicePaid{ID: "inv-coexist", Amount: float64(seq)},
		}); err != nil {
			t.Fatalf("Publish(%d) error = %v, want nil", seq, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// One counted event reaches each reader exactly once, through its own
	// group: an adopted group delivers no differently from a created one.
	recB.Clear()
	recC.Clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-counted", Amount: 3},
	}); err != nil {
		t.Fatalf("Publish(counted) error = %v, want nil", err)
	}
	testkit.EventuallyWithin(t, 5*time.Second, "both readers to deliver the counted event exactly once each", func() bool {
		return recB.Total() == 1 && recC.Total() == 1
	})

	// The adopting instance's Close destroys the group it adopted -- the
	// group is the instance's own, by the same naming that made its adoption
	// correct -- and spares the later reader's: exactly bus C's group remains
	// on the stream, and bus C keeps delivering.
	busB.Close()
	groups, err := client.XInfoGroups(ctx, streamKey(paidType)).Result()
	if err != nil {
		t.Fatalf("XInfoGroups: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("XInfoGroups reports %d groups after the adopting reader closed, want bus C's single one", len(groups))
	}
	recC.Clear()
	if err := busA.Publish(ctx, pkgcore.Event{
		Type: paidType, TenantID: pkgcore.TenantID("tenant-acme"), Payload: invoicePaid{ID: "inv-spared", Amount: 4},
	}); err != nil {
		t.Fatalf("Publish(spared) error = %v, want nil", err)
	}
	testkit.EventuallyWithin(t, 5*time.Second, "the spared reader to keep delivering", func() bool {
		return recC.Total() == 1
	})
}
