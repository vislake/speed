package eventbustest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// This file proves the cross-instance checks AssertConforms runs have
// teeth: each check returns an error rather than failing a test directly
// (see assert_conforms.go), so these tests drive the checks — with the
// cross-instance wait budget shortened, since a rejection only happens once
// a wait runs out — against a deliberately defective pair and require the
// check to reject it. A check that accepted the pair would silently regress to the
// single-instance-only shape, so these tests are the guard against that
// regression, the same way a test that asserts an error is returned guards
// the error's existence.
//
// teethBudget bounds the probes: a genuine rejection is a wait that runs
// out, and with the real crossInstanceBudget each probe would take several
// seconds for no additional certainty.
const teethBudget = 300 * time.Millisecond

// localOnlyBus is a minimal local-synchronous EventBus: Publish invokes the
// handlers subscribed on THIS instance, in registration order, joining
// errors — and has no notion of any other instance at all. Two instances
// of it form the pair shape of an implementation whose "remote" delivery is
// silently absent: the defect class under which three of the five
// defective implementations reviewed under the deployment-composition
// retrofit were EventBus implementations (an event published on one
// instance never reaches a subscriber on another, and no single-instance
// suite can see that).
type localOnlyBus struct {
	mu       sync.Mutex
	handlers map[string][]pkgcore.EventHandler
}

func newLocalOnlyBus() *localOnlyBus {
	return &localOnlyBus{handlers: make(map[string][]pkgcore.EventHandler)}
}

func (b *localOnlyBus) Subscribe(eventType string, h pkgcore.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

func (b *localOnlyBus) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	handlers := append([]pkgcore.EventHandler(nil), b.handlers[evt.Type]...)
	b.mu.Unlock()
	var failures []error
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("local-only handler %d: %w", i, err))
		}
	}
	return errors.Join(failures...)
}

// TestCrossInstanceChecks_RejectInstancesThatNeverDeliverToEachOther pins
// that the cross-instance checks can fail: two independent localOnlyBus
// instances share no state, so an event published on the first is never
// delivered to a subscriber on the second — exactly what an implementation
// whose remote delivery is silently broken looks like from inside this
// suite. Every check that depends on cross-instance delivery must reject
// the pair.
func TestCrossInstanceChecks_RejectInstancesThatNeverDeliverToEachOther(t *testing.T) {
	newPair := func() (pkgcore.EventBus, pkgcore.EventBus) {
		return newLocalOnlyBus(), newLocalOnlyBus()
	}

	checks := []struct {
		name  string
		check func(publisher, receiver pkgcore.EventBus, eventType string, budget time.Duration) error
	}{
		{"cross-instance delivery", checkCrossInstanceDelivery},
		{"no catch-up for a late subscriber", checkNoCatchUpForLateSubscriber},
		{"panic does not wedge delivery", checkPanicDoesNotWedgeDelivery},
	}
	for _, tt := range checks {
		publisher, receiver := newPair()
		err := tt.check(publisher, receiver, subscript(conformEventType, "silent-teeth"), teethBudget)
		if err == nil {
			t.Errorf("check %q accepted two instances that never deliver to each other: the cross-instance assertions have no teeth", tt.name)
		}
	}
}

// wedgePair is a pair of instances over one shared delivery queue whose
// receiving side wedges: the receiver's reader goroutine runs its handlers
// in registration order, and a panicking handler is recovered — but the
// recovery kills the reader, so every event published after the panic
// stays queued forever and never reaches the healthy handlers subscribed
// alongside the panicking one. This is the eb-3 wedge shape: the reader of
// the receiving instance dies on a handler panic while the publishing
// instance's own local delivery (the only path a single-instance suite can
// exercise) stays flawless.
type wedgePair struct {
	publisher *wedgePublisher
	receiver  *wedgeReceiver
}

// newWedgePair builds a wedged pair and starts the receiver's reader.
func newWedgePair() *wedgePair {
	queue := make(chan pkgcore.Event, 64)
	p := &wedgePair{
		publisher: &wedgePublisher{handlers: make(map[string][]pkgcore.EventHandler), queue: queue},
		receiver:  &wedgeReceiver{handlers: make(map[string][]pkgcore.EventHandler), queue: queue},
	}
	go p.receiver.read()
	return p
}

// wedgePublisher is the publishing half: local synchronous fan-out with
// joined errors (the path every pre-existing single-instance check
// exercises) plus a best-effort forward of the event onto the shared queue
// for the receiver's reader — the cross-instance delivery path the
// cross-instance checks exist to make visible.
type wedgePublisher struct {
	mu       sync.Mutex
	handlers map[string][]pkgcore.EventHandler
	queue    chan pkgcore.Event
}

func (b *wedgePublisher) Subscribe(eventType string, h pkgcore.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

func (b *wedgePublisher) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	handlers := append([]pkgcore.EventHandler(nil), b.handlers[evt.Type]...)
	b.mu.Unlock()
	var failures []error
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("wedge publisher handler %d: %w", i, err))
		}
	}
	select {
	case b.queue <- evt:
	default: // a wedged receiver must never block the publisher
	}
	return errors.Join(failures...)
}

// wedgeReceiver is the receiving half: its reader goroutine delivers the
// shared queue's events to the local handlers in registration order, and a
// panicking handler kills the reader (the recover happens so the panic does
// not crash the process, but the reader stops — later events stay queued).
type wedgeReceiver struct {
	mu       sync.Mutex
	handlers map[string][]pkgcore.EventHandler
	queue    chan pkgcore.Event
}

func (b *wedgeReceiver) Subscribe(eventType string, h pkgcore.EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[eventType] = append(b.handlers[eventType], h)
}

// Publish satisfies pkgcore.EventBus on the receiving half. The suite only
// ever publishes on the pair's first instance, so this method exists for
// interface completeness; its semantics mirror the publisher's local
// fan-out (minus the forward to a peer the receiver does not have).
func (b *wedgeReceiver) Publish(ctx context.Context, evt pkgcore.Event) error {
	b.mu.Lock()
	handlers := append([]pkgcore.EventHandler(nil), b.handlers[evt.Type]...)
	b.mu.Unlock()
	var failures []error
	for i, h := range handlers {
		if err := h(ctx, evt); err != nil {
			failures = append(failures, fmt.Errorf("wedge receiver handler %d: %w", i, err))
		}
	}
	return errors.Join(failures...)
}

func (b *wedgeReceiver) read() {
	wedged := false
	for evt := range b.queue {
		if wedged {
			return
		}
		b.mu.Lock()
		handlers := append([]pkgcore.EventHandler(nil), b.handlers[evt.Type]...)
		b.mu.Unlock()
		for _, h := range handlers {
			func() {
				defer func() {
					if recover() != nil {
						wedged = true
					}
				}()
				_ = h(context.Background(), evt)
			}()
		}
	}
}

// TestPanicCheck_RejectsAReaderThatWedgesOnHandlerPanic pins that the
// panic-isolation check can fail: on this pair, the first event that
// reaches the panicking handler kills the receiver's reader, so the
// follow-up event of the same type never reaches the healthy handler that
// ran before the panicking one. The delivery check, by contrast, must pass
// on this pair — its reader delivers perfectly until a panic — proving the
// rejection is the panic check's own work, not the delivery check's.
func TestPanicCheck_RejectsAReaderThatWedgesOnHandlerPanic(t *testing.T) {
	pair := newWedgePair()

	// Cross-instance delivery works on the wedged pair until a handler
	// panics: this is what makes it a wedge defect rather than a
	// delivers-nothing-remotely defect.
	err := checkCrossInstanceDelivery(pair.publisher, pair.receiver,
		subscript(conformEventType, "wedge-teeth-delivery"), teethBudget)
	if err != nil {
		t.Fatalf("cross-instance delivery check rejected a pair that delivers until a panic wedges it: %v", err)
	}

	err = checkPanicDoesNotWedgeDelivery(pair.publisher, pair.receiver,
		subscript(conformEventType, "wedge-teeth-panic"), teethBudget)
	if err == nil {
		t.Fatal("panic check accepted a pair whose reader wedges on a handler panic: the panic-isolation assertion has no teeth")
	}
}
