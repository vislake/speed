package testkit

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// record fills r with evt through the EventHandler shape.
func record(t *testing.T, r *EventRecorder, evt pkgcore.Event) {
	t.Helper()
	if err := r.Record(context.Background(), evt); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func TestEventRecorder_CountsAndFirstOverRecordedEvents(t *testing.T) {
	r := NewEventRecorder()
	record(t, r, pkgcore.Event{Type: "notes.created", TenantID: "tenant-acme"})
	record(t, r, pkgcore.Event{Type: "notes.deleted", TenantID: "tenant-bright"})
	record(t, r, pkgcore.Event{Type: "notes.created", TenantID: "tenant-acme"})

	if got := r.Total(); got != 3 {
		t.Fatalf("Total = %d, want 3", got)
	}
	if got := r.Count("notes.created"); got != 2 {
		t.Fatalf("Count(notes.created) = %d, want 2", got)
	}
	if got := r.CountByTenant("tenant-acme"); got != 2 {
		t.Fatalf("CountByTenant(tenant-acme) = %d, want 2", got)
	}
	first, ok := r.First("notes.created")
	if !ok || first.Type != "notes.created" {
		t.Fatalf("First(notes.created) = (%v, %v), want the earliest created event", first, ok)
	}
	if _, found := r.First("notes.restored"); found {
		t.Fatal("First(notes.restored) reported an event for a type that was never recorded")
	}
	match, ok := r.FirstMatch(func(evt pkgcore.Event) bool { return evt.TenantID == "tenant-bright" })
	if !ok || match.Type != "notes.deleted" {
		t.Fatalf("FirstMatch(tenant-bright) = (%v, %v), want the deleted event", match, ok)
	}
	if got := len(r.OfType("notes.created")); got != 2 {
		t.Fatalf("OfType(notes.created) = %d events, want 2", got)
	}
	if got := r.Types(); len(got) != 3 || got[0] != "notes.created" || got[2] != "notes.created" {
		t.Fatalf("Types = %v, want the three recorded types in order", got)
	}
	if got := r.At(1).Type; got != "notes.deleted" {
		t.Fatalf("At(1).Type = %q, want %q", got, "notes.deleted")
	}
}

func TestEventRecorder_SubscribeRecordsEveryBusDelivery(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	r := NewEventRecorder()
	r.Subscribe(bus, "notes.created", "notes.deleted")

	if err := bus.Publish(context.Background(), pkgcore.Event{Type: "notes.created", TenantID: "tenant-acme"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := bus.Publish(context.Background(), pkgcore.Event{Type: "notes.restored", TenantID: "tenant-acme"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := r.Count("notes.created"); got != 1 {
		t.Fatalf("Count(notes.created) = %d, want 1 (the unsubscribed type must not be recorded)", got)
	}
}

func TestEventRecorder_EventsReturnsASnapshot(t *testing.T) {
	r := NewEventRecorder()
	record(t, r, pkgcore.Event{Type: "notes.created"})
	snapshot := r.Events()
	snapshot[0] = pkgcore.Event{Type: "mutated"}
	if got := r.At(0).Type; got != "notes.created" {
		t.Fatalf("recorded event type = %q after mutating a snapshot, want the original", got)
	}
}

func TestEventRecorder_ClearDropsEverything(t *testing.T) {
	r := NewEventRecorder()
	record(t, r, pkgcore.Event{Type: "notes.created"})
	r.Clear()
	if got := r.Total(); got != 0 {
		t.Fatalf("Total = %d after Clear, want 0", got)
	}
}

func TestEventRecorder_ConcurrentRecording_LosesNothing(t *testing.T) {
	r := NewEventRecorder()
	const goroutines, perGoroutine = 8, 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = r.Record(context.Background(), pkgcore.Event{Type: "notes.created"})
			}
		}()
	}
	wg.Wait()
	if got := r.Total(); got != goroutines*perGoroutine {
		t.Fatalf("Total = %d, want %d: concurrent Record calls must not lose events", got, goroutines*perGoroutine)
	}
}
