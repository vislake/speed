package eventbustest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the boundary the seam contract draws around handler-error
// reporting: pkgcore.EventBus's doc comment promises delivery, continuation
// past a failing handler, and containment -- but not that Publish reports a
// handler's failure. Reporting is possible only for handlers an
// implementation runs synchronously on the publisher's own goroutine; a
// broker-backed bus's deliveries to other replicas run on its own reader
// goroutines, after Publish has returned, where no publisher exists to
// receive the failure. An implementation whose delivery has that shape is
// therefore conformant, and the suite must accept it -- the same way the
// teeth tests in assert_conforms_rejects_defective_buses_test.go prove the
// suite's rejection checks can fail, this test proves its acceptance side
// does not over-assert.

// deferredDeliveryBus is a same-process EventBus whose delivery is
// asynchronous by design, the in-process miniature of a broker-backed bus's
// remote delivery path: Publish enqueues the event and returns immediately
// (the analogue of the broker append that is the commit point), and a single
// worker goroutine delivers the queue to the subscribed handlers, in
// registration order, on a context of the bus's own. A panicking handler is
// recovered and logged -- never allowed to kill the delivery goroutine, so
// the handlers registered after it still run -- and a handler's returned
// error is dropped: the worker runs after Publish has returned, so no
// publisher exists to receive the failure, the same structural impossibility
// the distributed implementations' reader goroutines give their remote
// deliveries (see eventbus/redis.EventBus.runRemoteHandler and its
// siblings). Publish's error is therefore always nil for this bus: it has no
// delivery of its own that can fail, and handler failures are not observable
// by construction.
//
// Under the seam contract this bus is a compliant non-reporter: the
// contract does not promise that Publish reports a same-instance handler's
// failure, and this bus's Publish returns nil. A suite-wide assertion that
// Publish does report it would be checkable only on implementations whose
// same-instance delivery runs handlers synchronously -- all four real ones
// happen to do so, while the path on which none of the three broker-backed
// ones can report (delivery to another replica) would never be exercised at
// all: "checked, no problems" for a property the interface promised and
// three of four implementations cannot provide. The contract keeps error
// reporting out of the seam promise, so
// the corrected suite must accept this bus.
type deferredDeliveryBus struct {
	mu       sync.Mutex
	handlers map[string][]pkgcore.EventHandler
	queue    chan pkgcore.Event
}

// newDeferredDeliveryBus builds a bus and starts its delivery worker.
func newDeferredDeliveryBus() *deferredDeliveryBus {
	b := &deferredDeliveryBus{
		handlers: make(map[string][]pkgcore.EventHandler),
		queue:    make(chan pkgcore.Event, 64),
	}
	go b.run()
	return b
}

// Subscribe registers h for the exact event type eventType, in registration
// order like every other implementation; the worker delivers in that order.
func (b *deferredDeliveryBus) Subscribe(eventType string, h pkgcore.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish enqueues evt for the delivery worker and returns nil: the event's
// handlers run later, on the worker's goroutine, so none of their failures
// can ever reach this caller -- the structural boundary the seam contract
// leaves to each implementation. A queue that has filled (the worker is
// wedged) is the one delivery failure this bus can report.
func (b *deferredDeliveryBus) Publish(_ context.Context, evt pkgcore.Event) error {
	select {
	case b.queue <- evt:
		return nil
	default:
		return errors.New("eventbustest: deferred delivery bus queue is full")
	}
}

// run is the single delivery goroutine. It pops events in enqueue order and
// invokes the type's handlers in registration order; a panicking handler is
// recovered so the delivery goroutine survives and the handlers registered
// after it still run, and a handler's returned error is dropped, exactly as
// the distributed implementations' reader goroutines drop a remote handler's
// error (runRemoteHandler and its siblings).
func (b *deferredDeliveryBus) run() {
	for evt := range b.queue {
		b.mu.Lock()
		handlers := append([]pkgcore.EventHandler(nil), b.handlers[evt.Type]...)
		b.mu.Unlock()
		for _, h := range handlers {
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Default().Error("eventbustest: deferred delivery bus contained a panicking handler",
							"event_type", evt.Type,
							"panic", r,
						)
					}
				}()
				_ = h(context.Background(), evt)
			}()
		}
	}
}

// TestAssertConforms_AcceptsBusesThatCannotReportHandlerErrors proves the
// shared suite accepts an implementation that cannot report a handler's
// failure to any publisher: the seam contract (pkgcore.EventBus's own doc
// comment) does not promise error reporting, so delivery on the
// implementation's own goroutines -- after Publish has returned -- is a
// legitimate delivery shape, not a defect. Before the contract correction
// this test failed: the suite's error-reporting subtest rejected the bus
// with "Publish() error = nil, want a non-nil error reporting the failing
// handler", the assertion that kept passing for the broker-backed
// implementations only because it exercised the one path (same-instance
// delivery) on which they all happen to run handlers synchronously. The
// corrected suite asserts only what the contract promises about a failing
// handler -- the handlers after it still run -- which this bus provides.
func TestAssertConforms_AcceptsBusesThatCannotReportHandlerErrors(t *testing.T) {
	AssertConforms(t, 0, func() (pkgcore.EventBus, pkgcore.EventBus) {
		bus := newDeferredDeliveryBus()
		return bus, bus
	})
}
