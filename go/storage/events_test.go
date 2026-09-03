package storage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestEventPayloads_JSONTags pins the wire shape of the two event payloads:
// snake_case keys, so the JSON a subscriber receives and the attributes a
// logger extracts use the project-wide attribute vocabulary, not Go field
// names.
func TestEventPayloads_JSONTags(t *testing.T) {
	completed := ObjectCompletedPayload{ObjectID: "obj-1", Size: 2048, MIME: "image/png"}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatalf("marshal ObjectCompletedPayload: %v", err)
	}
	if got := string(raw); got != `{"object_id":"obj-1","size":2048,"mime":"image/png"}` {
		t.Errorf("ObjectCompletedPayload JSON = %s", got)
	}

	deleted := ObjectDeletedPayload{ObjectID: "obj-1"}
	raw, err = json.Marshal(deleted)
	if err != nil {
		t.Fatalf("marshal ObjectDeletedPayload: %v", err)
	}
	if got := string(raw); got != `{"object_id":"obj-1"}` {
		t.Errorf("ObjectDeletedPayload JSON = %s", got)
	}
}

// TestServiceHost_RequireStore_FailsClosedWithoutARegistry pins the seam's
// fail-closed contract: a service that never had Register attach a registry
// (host nil), and a registry that was bootstrapped without an object store,
// both report storage.store_unavailable instead of panicking on a nil
// store -- a nil ObjectStore method call would panic, which is exactly what
// the guard exists to prevent.
func TestServiceHost_RequireStore_FailsClosedWithoutARegistry(t *testing.T) {
	var h serviceHost

	if _, err := h.requireStore(); err == nil {
		t.Fatal("requireStore without any attached registry succeeded, want storage.store_unavailable")
	} else {
		assertCode(t, err, "storage.store_unavailable")
	}

	h.host = &fakeHost{} // a registry-shaped host carrying no store
	if _, err := h.requireStore(); err == nil {
		t.Fatal("requireStore on a store-less host succeeded, want storage.store_unavailable")
	} else {
		assertCode(t, err, "storage.store_unavailable")
	}
}

// TestServiceHost_RequireStore_ReturnsTheAttachedStore pins the happy side
// of the seam: once a host carrying a store is attached, requireStore hands
// that store back, read at call time rather than captured at attach.
func TestServiceHost_RequireStore_ReturnsTheAttachedStore(t *testing.T) {
	store := newFakeStore()
	h := serviceHost{host: &fakeHost{store: store}}

	got, err := h.requireStore()
	if err != nil {
		t.Fatalf("requireStore with an attached store: %v", err)
	}
	if got != pkgcore.ObjectStore(store) {
		t.Errorf("requireStore returned %T, want the attached fakeStore", got)
	}
}

// TestServiceHost_Publish_FailsClosedWithoutABus pins the seam's other
// failure mode: publishing on a service whose registry was never attached,
// or whose registry carries no event bus, is an internal wiring anomaly and
// is reported as such -- callers treat it as a logged, recovered anomaly,
// never as a reason to fail the durable work the event announces.
func TestServiceHost_Publish_FailsClosedWithoutABus(t *testing.T) {
	ctx := context.Background()
	evt := pkgcore.Event{Type: EventObjectCompleted, Payload: ObjectCompletedPayload{ObjectID: "obj-1"}}

	var h serviceHost
	if err := h.publish(ctx, evt); err == nil || err.Error() != "storage: no host registry wired" {
		t.Errorf("publish without an attached registry = %v, want the no-host error", err)
	}

	h.host = &fakeHost{} // a registry-shaped host carrying no bus
	if err := h.publish(ctx, evt); err == nil || err.Error() != "storage: registry carries no event bus" {
		t.Errorf("publish on a bus-less host = %v, want the no-bus error", err)
	}
}

// TestServiceHost_Publish_DeliversOnTheAttachedBus pins the happy side:
// publish sends the event on the bus the attached host carries, verbatim.
func TestServiceHost_Publish_DeliversOnTheAttachedBus(t *testing.T) {
	ctx := context.Background()
	bus := newRecordingBus()
	h := serviceHost{host: &fakeHost{bus: bus}}
	evt := pkgcore.Event{
		Type:     EventObjectDeleted,
		TenantID: "tenant-1",
		Payload:  ObjectDeletedPayload{ObjectID: "obj-1"},
	}

	if err := h.publish(ctx, evt); err != nil {
		t.Fatalf("publish on the attached bus: %v", err)
	}
	if len(bus.events) != 1 {
		t.Fatalf("bus received %d events, want 1", len(bus.events))
	}
	if bus.events[0].Type != evt.Type || bus.events[0].TenantID != evt.TenantID {
		t.Errorf("bus event = %+v, want %+v", bus.events[0], evt)
	}
}
