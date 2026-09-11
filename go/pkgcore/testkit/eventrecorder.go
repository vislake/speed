package testkit

import (
	"context"
	"sync"

	"github.com/vislake/speed/go/pkgcore"
)

// EventRecorder accumulates the events a bus delivers to it, for later count
// and content assertions on what the far side of the bus actually received.
// Handlers may run on several goroutines -- a local publish path and the
// reader goroutines of a distributed bus both invoke them -- so every access
// is mutex-guarded and the type is safe for concurrent use.
type EventRecorder struct {
	mu     sync.Mutex
	events []pkgcore.Event
}

// NewEventRecorder returns an empty recorder.
func NewEventRecorder() *EventRecorder { return &EventRecorder{} }

// Record appends evt, implementing pkgcore.EventHandler so the recorder can
// be subscribed directly.
func (r *EventRecorder) Record(_ context.Context, evt pkgcore.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, evt)
	return nil
}

// Handler returns Record as a pkgcore.EventHandler value.
func (r *EventRecorder) Handler() pkgcore.EventHandler { return r.Record }

// Subscribe installs the recorder on bus for each of types.
func (r *EventRecorder) Subscribe(bus pkgcore.EventBus, types ...string) {
	for _, eventType := range types {
		bus.Subscribe(eventType, r.Record)
	}
}

// Events returns a snapshot of everything recorded so far.
func (r *EventRecorder) Events() []pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]pkgcore.Event, len(r.events))
	copy(out, r.events)
	return out
}

// Total reports how many events were recorded, of any type.
func (r *EventRecorder) Total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// Count reports how many recorded events carry eventType.
func (r *EventRecorder) Count(eventType string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, evt := range r.events {
		if evt.Type == eventType {
			n++
		}
	}
	return n
}

// CountByTenant reports how many recorded events carry tenant.
func (r *EventRecorder) CountByTenant(tenant pkgcore.TenantID) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, evt := range r.events {
		if evt.TenantID == tenant {
			n++
		}
	}
	return n
}

// First returns the earliest recorded event of eventType.
func (r *EventRecorder) First(eventType string) (pkgcore.Event, bool) {
	return r.FirstMatch(func(evt pkgcore.Event) bool { return evt.Type == eventType })
}

// FirstMatch returns the earliest recorded event match reports true for.
func (r *EventRecorder) FirstMatch(match func(pkgcore.Event) bool) (pkgcore.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, evt := range r.events {
		if match(evt) {
			return evt, true
		}
	}
	return pkgcore.Event{}, false
}

// OfType returns every recorded event of eventType, in order.
func (r *EventRecorder) OfType(eventType string) []pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []pkgcore.Event
	for _, evt := range r.events {
		if evt.Type == eventType {
			out = append(out, evt)
		}
	}
	return out
}

// Types returns the type of every recorded event, in order.
func (r *EventRecorder) Types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	types := make([]string, len(r.events))
	for i, evt := range r.events {
		types[i] = evt.Type
	}
	return types
}

// At returns the i-th recorded event. It panics on an out-of-range index,
// like any slice access.
func (r *EventRecorder) At(i int) pkgcore.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[i]
}

// Clear drops everything recorded so far.
func (r *EventRecorder) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = nil
}
