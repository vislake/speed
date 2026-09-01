package pkgcore

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bounds for the tests that would hang rather than fail if the bus deadlocked,
// and for the amount of concurrency the race test drives through it.
const (
	deadlockTimeout       = 5 * time.Second
	concurrentSubscribers = 8
	concurrentPublishers  = 8
	publishesPerPublisher = 50
	// preRegisteredHandlers counts the handlers subscribed before the goroutines
	// start, so every publish is guaranteed to reach at least this many.
	preRegisteredHandlers = 1
)

// Event types used across the tests. Handlers match on exact strings, so the
// isolation tests need at least two distinct ones.
const (
	orderCreatedEvent = "order.created"
	orderShippedEvent = "order.shipped"
	unrelatedEvent    = "billing.invoiced"
)

func TestMemoryEventBusPublish(t *testing.T) {
	t.Parallel()

	errFirst := errors.New("first handler exploded")
	errSecond := errors.New("second handler exploded")

	// subscription describes one handler to register before publishing: which
	// event type it listens to, the name it records when invoked, and the error
	// it returns (nil for a handler that succeeds).
	type subscription struct {
		eventType string
		name      string
		err       error
	}

	tests := []struct {
		name          string
		subscriptions []subscription
		publishType   string
		wantCalled    []string
		wantErrs      []error
	}{
		{
			name:        "no subscribers at all is a no-op",
			publishType: orderCreatedEvent,
		},
		{
			name: "publishing a type nobody subscribed to leaves other handlers untouched",
			subscriptions: []subscription{
				{eventType: orderCreatedEvent, name: "created"},
				{eventType: orderShippedEvent, name: "shipped"},
			},
			publishType: unrelatedEvent,
		},
		{
			name: "the single subscriber of the type is invoked",
			subscriptions: []subscription{
				{eventType: orderCreatedEvent, name: "created"},
			},
			publishType: orderCreatedEvent,
			wantCalled:  []string{"created"},
		},
		{
			name: "every subscriber of the same type is invoked in registration order",
			subscriptions: []subscription{
				{eventType: orderCreatedEvent, name: "first"},
				{eventType: orderCreatedEvent, name: "second"},
				{eventType: orderCreatedEvent, name: "third"},
			},
			publishType: orderCreatedEvent,
			wantCalled:  []string{"first", "second", "third"},
		},
		{
			name: "a failing handler does not stop the handlers after it",
			subscriptions: []subscription{
				{eventType: orderCreatedEvent, name: "first", err: errFirst},
				{eventType: orderCreatedEvent, name: "second", err: errSecond},
				{eventType: orderCreatedEvent, name: "third"},
			},
			publishType: orderCreatedEvent,
			wantCalled:  []string{"first", "second", "third"},
			wantErrs:    []error{errFirst, errSecond},
		},
		{
			name: "handlers of a different type are never invoked",
			subscriptions: []subscription{
				{eventType: orderCreatedEvent, name: "created"},
				{eventType: orderShippedEvent, name: "shipped", err: errFirst},
				{eventType: orderCreatedEvent, name: "created-too"},
			},
			publishType: orderCreatedEvent,
			wantCalled:  []string{"created", "created-too"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bus := NewMemoryEventBus()

			// Publish is synchronous on the calling goroutine, so recording the
			// call order without a lock is safe here and asserts that property.
			var called []string
			for _, sub := range tt.subscriptions {
				bus.Subscribe(sub.eventType, func(_ context.Context, _ Event) error {
					called = append(called, sub.name)
					return sub.err
				})
			}

			err := bus.Publish(context.Background(), Event{Type: tt.publishType})

			if !slices.Equal(called, tt.wantCalled) {
				t.Errorf("handlers called = %v, want %v", called, tt.wantCalled)
			}

			if len(tt.wantErrs) == 0 {
				if err != nil {
					t.Fatalf("Publish() error = %v, want nil", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("Publish() error = nil, want the failures %v", tt.wantErrs)
			}
			for _, wantErr := range tt.wantErrs {
				if !errors.Is(err, wantErr) {
					t.Errorf("Publish() error = %v, want it to wrap %v", err, wantErr)
				}
				if !strings.Contains(err.Error(), wantErr.Error()) {
					t.Errorf("Publish() error message %q does not mention %q", err, wantErr)
				}
			}
			if !strings.Contains(err.Error(), tt.publishType) {
				t.Errorf("Publish() error message %q does not mention the event type %q", err, tt.publishType)
			}
		})
	}
}

func TestMemoryEventBusPublishDeliversEventUnchanged(t *testing.T) {
	t.Parallel()

	// TenantID is left at its zero value: this event is not tenant-scoped, and
	// the assertion compares the whole struct, so any field the bus mangled
	// would show up here.
	want := Event{Type: orderCreatedEvent, Payload: map[string]int{"orderID": 42}}

	bus := NewMemoryEventBus()
	var got Event
	var calls int
	bus.Subscribe(orderCreatedEvent, func(_ context.Context, evt Event) error {
		got = evt
		calls++
		return nil
	})

	if err := bus.Publish(context.Background(), want); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	if calls != 1 {
		t.Fatalf("handler invoked %d times, want exactly 1", calls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("handler received %+v, want %+v", got, want)
	}
}

func TestMemoryEventBusPublishPassesCallerContext(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	const wantValue = "trace-1"

	bus := NewMemoryEventBus()
	var gotValue any
	bus.Subscribe(orderCreatedEvent, func(ctx context.Context, _ Event) error {
		gotValue = ctx.Value(ctxKey{})
		return nil
	})

	ctx := context.WithValue(context.Background(), ctxKey{}, wantValue)
	if err := bus.Publish(ctx, Event{Type: orderCreatedEvent}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	if gotValue != wantValue {
		t.Errorf("handler saw context value %v, want %q", gotValue, wantValue)
	}
}

func TestMemoryEventBusSubscribeIgnoresNilHandler(t *testing.T) {
	t.Parallel()

	bus := NewMemoryEventBus()
	bus.Subscribe(orderCreatedEvent, nil)

	var called bool
	bus.Subscribe(orderCreatedEvent, func(_ context.Context, _ Event) error {
		called = true
		return nil
	})

	// A nil handler must neither be stored nor panic the publisher; the real
	// handler registered after it must still run.
	if err := bus.Publish(context.Background(), Event{Type: orderCreatedEvent}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if !called {
		t.Error("the handler registered after a nil handler was not invoked")
	}
}

func TestMemoryEventBusHandlerMaySubscribeAndPublish(t *testing.T) {
	t.Parallel()

	bus := NewMemoryEventBus()
	var shippedCalls int
	bus.Subscribe(orderCreatedEvent, func(ctx context.Context, _ Event) error {
		// Re-entrant calls from inside a handler deadlock any implementation
		// that invokes handlers while still holding its lock.
		bus.Subscribe(unrelatedEvent, func(_ context.Context, _ Event) error { return nil })
		return bus.Publish(ctx, Event{Type: orderShippedEvent})
	})
	bus.Subscribe(orderShippedEvent, func(_ context.Context, _ Event) error {
		shippedCalls++
		return nil
	})

	done := make(chan error, 1)
	go func() {
		done <- bus.Publish(context.Background(), Event{Type: orderCreatedEvent})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Publish() error = %v, want nil", err)
		}
	case <-time.After(deadlockTimeout):
		t.Fatalf("Publish() deadlocked: a handler could not call Subscribe or Publish within %v", deadlockTimeout)
	}

	if shippedCalls != 1 {
		t.Errorf("nested handler invoked %d times, want exactly 1", shippedCalls)
	}
}

func TestMemoryEventBusConcurrentSubscribeAndPublish(t *testing.T) {
	t.Parallel()

	bus := NewMemoryEventBus()

	var mu sync.Mutex
	var handlerCalls int
	countingHandler := func(_ context.Context, _ Event) error {
		mu.Lock()
		defer mu.Unlock()
		handlerCalls++
		return nil
	}

	// One handler is registered up front so the publishers below have real work
	// to do even before the subscribing goroutines get scheduled.
	bus.Subscribe(orderCreatedEvent, countingHandler)

	var wg sync.WaitGroup
	for range concurrentSubscribers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bus.Subscribe(orderCreatedEvent, countingHandler)
			bus.Subscribe(orderShippedEvent, countingHandler)
		}()
	}
	for range concurrentPublishers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range publishesPerPublisher {
				if err := bus.Publish(context.Background(), Event{Type: orderCreatedEvent}); err != nil {
					t.Errorf("Publish() error = %v, want nil", err)
					return
				}
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(deadlockTimeout):
		t.Fatalf("concurrent Subscribe and Publish did not finish within %v", deadlockTimeout)
	}

	// Every publish saw at least the handler registered before the goroutines
	// started, and never more than the full set of subscribers.
	mu.Lock()
	defer mu.Unlock()
	minCalls := concurrentPublishers * publishesPerPublisher
	maxCalls := minCalls * (concurrentSubscribers + preRegisteredHandlers)
	if handlerCalls < minCalls || handlerCalls > maxCalls {
		t.Errorf("handlers invoked %d times, want between %d and %d", handlerCalls, minCalls, maxCalls)
	}
}
