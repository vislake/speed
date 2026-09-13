package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// recorder collects the callbacks a rollback made, in order.
type recorder struct{ calls []string }

func (r *recorder) note(format string, args ...any) {
	r.calls = append(r.calls, fmt.Sprintf(format, args...))
}

// closer builds a module that records its Stop and Close, optionally failing.
func closer(rec *recorder, name string, stopErr, closeErr error) Module {
	return Module{
		Name: name,
		New: func(context.Context, *Registry) (any, error) {
			return &product{id: name}, nil
		},
		Stop: func(context.Context, *Registry, any) error {
			rec.note("stop %s", name)
			return stopErr
		},
		Close: func(context.Context, *Registry, any) error {
			rec.note("close %s", name)
			return closeErr
		},
	}
}

func constructAll(t *testing.T, reg *Registry, mods ...Module) {
	t.Helper()
	for _, m := range mods {
		reg.Register(m)
		if err := reg.construct(t.Context(), m); err != nil {
			t.Fatalf("constructing %q failed: %v", m.Name, err)
		}
	}
}

func TestRollbackIsReverseDependencyOrderStopsBeforeCloses(t *testing.T) {
	rec := &recorder{}
	reg := New()
	constructAll(t, reg, closer(rec, "first", nil, nil), closer(rec, "second", nil, nil), closer(rec, "third", nil, nil))

	if err := reg.shutdown(t.Context(), nil); err != nil {
		t.Fatalf("a clean rollback returned %v", err)
	}
	want := []string{"stop third", "stop second", "stop first", "close third", "close second", "close first"}
	if !slices.Equal(rec.calls, want) {
		t.Fatalf("rollback ran %v, want %v", rec.calls, want)
	}
}

// TestStopFailureDoesNotAbortStopPhase pins that a failed drain notice cannot
// hold back the rest of the stage or the Close that follows.
func TestStopFailureDoesNotAbortStopPhase(t *testing.T) {
	rec := &recorder{}
	reg := New()
	constructAll(t, reg,
		closer(rec, "first", nil, nil),
		closer(rec, "second", errors.New("drain refused"), nil),
		closer(rec, "third", nil, nil),
	)

	err := reg.shutdown(t.Context(), nil)
	if err == nil || !strings.Contains(err.Error(), "drain refused") {
		t.Fatalf("shutdown returned %v, want the stop failure reported", err)
	}
	want := []string{"stop third", "stop second", "stop first", "close third", "close second", "close first"}
	if !slices.Equal(rec.calls, want) {
		t.Fatalf("rollback ran %v, want %v", rec.calls, want)
	}
}

// TestPrimaryErrorSurvivesCleanupFailure pins that what terminated the startup
// is the first failure, not a secondary one on the way out.
func TestPrimaryErrorSurvivesCleanupFailure(t *testing.T) {
	rec := &recorder{}
	cleanupErr := errors.New("close refused")
	reg := New()
	constructAll(t, reg, closer(rec, "only", nil, cleanupErr))

	err := reg.shutdown(t.Context(), fmt.Errorf("%w: capability lost", ErrMissingProvider))
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("shutdown returned %v, which no longer carries the primary error", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("shutdown returned %v, which dropped the cleanup failure", err)
	}
	if !strings.HasPrefix(err.Error(), ErrMissingProvider.Error()) {
		t.Fatalf("shutdown returned %q, want the primary error first", err)
	}
}

func TestCleanCancelReturnsNil(t *testing.T) {
	rec := &recorder{}
	reg := New()
	constructAll(t, reg, closer(rec, "only", nil, nil))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := reg.shutdown(ctx, nil); err != nil {
		t.Fatalf("a clean shutdown on a cancelled context returned %v, want nil", err)
	}
}

// TestShutdownKeepsContextValues pins the other half of detaching the
// cancellation: what the host attached to the context still reaches Stop and
// Close, so only the cancellation is dropped.
func TestShutdownKeepsContextValues(t *testing.T) {
	type key struct{}
	var seen []any
	reg := New()
	m := Module{
		Name: "only",
		New:  func(context.Context, *Registry) (any, error) { return &product{id: "only"}, nil },
		Stop: func(ctx context.Context, _ *Registry, _ any) error {
			seen = append(seen, ctx.Value(key{}))
			return ctx.Err()
		},
		Close: func(ctx context.Context, _ *Registry, _ any) error {
			seen = append(seen, ctx.Value(key{}))
			return ctx.Err()
		},
	}
	constructAll(t, reg, m)

	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), key{}, "host"))
	cancel()
	if err := reg.shutdown(ctx, nil); err != nil {
		t.Fatalf("a shutdown whose callbacks honour the context returned %v, want nil", err)
	}
	if !slices.Equal(seen, []any{"host", "host"}) {
		t.Fatalf("Stop and Close saw %v, want the value the host attached in both", seen)
	}
}

// TestRollbackCallsNeverInitializedInstances pins the contract: a startup
// failure rolls back everything constructed, so Stop and Close reach instances
// that were never initialised or started.
func TestRollbackCallsNeverInitializedInstances(t *testing.T) {
	rec := &recorder{}
	reg := New()
	never := Module{
		Name: "never-initialised",
		New: func(context.Context, *Registry) (any, error) {
			return &product{id: "never-initialised"}, nil
		},
		Init: func(context.Context, *Registry, any) error {
			rec.note("init never-initialised")
			return nil
		},
		Stop: func(_ context.Context, _ *Registry, instance any) error {
			if instance == nil {
				t.Error("Stop received a nil instance for a constructed module")
			}
			rec.note("stop never-initialised")
			return nil
		},
		Close: func(context.Context, *Registry, any) error {
			rec.note("close never-initialised")
			return nil
		},
	}
	constructAll(t, reg, never)

	if err := reg.shutdown(t.Context(), nil); err != nil {
		t.Fatalf("shutdown returned %v", err)
	}
	want := []string{"stop never-initialised", "close never-initialised"}
	if !slices.Equal(rec.calls, want) {
		t.Fatalf("rollback ran %v, want %v with no Init in sight", rec.calls, want)
	}
}
