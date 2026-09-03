package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// newTestRegistry returns a fresh *pkgcore.Registry wired with in-memory
// infrastructure, giving this file's tests a real
// pkgcore.AuditActionRegistrar (reg.AuditActions) and a real
// pkgcore.EventBus (reg.Events.Bus()) without depending on anything this
// package cannot construct on its own -- mirroring go/config's own
// module_test.go newTestRegistry helper.
func newTestRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

func TestEmit_UnregisteredAction_ReturnsErrActionNotRegistered(t *testing.T) {
	reg := newTestRegistry()

	err := Emit(context.Background(), reg.Events.Bus(), reg.AuditActions, Input{Action: "notes.note.create"})
	if !errors.Is(err, ErrActionNotRegistered) {
		t.Fatalf("Emit() error = %v, want ErrActionNotRegistered", err)
	}
}

func TestEmit_RegisteredAction_PublishesRecordedEvent(t *testing.T) {
	reg := newTestRegistry()
	const action = "notes.note.create"
	if err := reg.AuditActions.Add(action); err != nil {
		t.Fatalf("AuditActions.Add() error = %v", err)
	}

	var got RecordedEvent
	var receivedType string
	reg.Events.Subscribe(EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		receivedType = evt.Type
		payload, ok := evt.Payload.(RecordedEvent)
		if !ok {
			t.Fatalf("Payload type = %T, want RecordedEvent", evt.Payload)
		}
		got = payload
		return nil
	})

	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	ctx = pkgcore.WithOnBehalfOf(ctx, pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"})

	in := Input{
		Action:   action,
		Resource: Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"},
		Result:   Result{Success: true},
		Changes:  &Diff{Before: map[string]any{"title": "old"}, After: map[string]any{"title": "new"}},
	}
	if err := Emit(ctx, reg.Events.Bus(), reg.AuditActions, in); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	if receivedType != EventRecorded {
		t.Errorf("published event Type = %q, want %q", receivedType, EventRecorded)
	}
	if got.Action != action {
		t.Errorf("Action = %q, want %q", got.Action, action)
	}
	if got.Resource != in.Resource {
		t.Errorf("Resource = %+v, want %+v", got.Resource, in.Resource)
	}
	if got.Result != in.Result {
		t.Errorf("Result = %+v, want %+v", got.Result, in.Result)
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("TenantID = %q, want %q", got.TenantID, "tenant-a")
	}
	if got.Actor.ID != "user-1" {
		t.Errorf("Actor.ID = %q, want %q", got.Actor.ID, "user-1")
	}
	if got.OnBehalfOf == nil || got.OnBehalfOf.ID != "admin-1" {
		t.Errorf("OnBehalfOf = %+v, want an Actor with ID=admin-1", got.OnBehalfOf)
	}
	if got.Changes == nil || got.Changes.Before["title"] != "old" || got.Changes.After["title"] != "new" {
		t.Errorf("Changes = %+v, want the before/after diff supplied on Input", got.Changes)
	}
	if got.OccurredAt.IsZero() {
		t.Errorf("OccurredAt is zero, want it populated")
	}
}

func TestEmit_NoActorOrOnBehalfOfOrTenantInContext_LeavesThemAtZeroValue(t *testing.T) {
	reg := newTestRegistry()
	const action = "notes.note.create"
	if err := reg.AuditActions.Add(action); err != nil {
		t.Fatalf("AuditActions.Add() error = %v", err)
	}

	var got RecordedEvent
	reg.Events.Subscribe(EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		got = evt.Payload.(RecordedEvent)
		return nil
	})

	if err := Emit(context.Background(), reg.Events.Bus(), reg.AuditActions, Input{Action: action}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	if got.Actor != (pkgcore.Actor{}) {
		t.Errorf("Actor = %+v, want the zero Actor", got.Actor)
	}
	if got.OnBehalfOf != nil {
		t.Errorf("OnBehalfOf = %+v, want nil", got.OnBehalfOf)
	}
	if got.TenantID != "" {
		t.Errorf("TenantID = %q, want empty", got.TenantID)
	}
}

// failingBus is a pkgcore.EventBus test double whose Publish always fails,
// used to prove Emit surfaces a publish failure to its caller rather than
// swallowing it.
type failingBus struct{ err error }

func (b failingBus) Subscribe(string, pkgcore.EventHandler) {}
func (b failingBus) Publish(context.Context, pkgcore.Event) error {
	return b.err
}

var _ pkgcore.EventBus = failingBus{}

func TestEmit_PublishFailure_ReturnsError(t *testing.T) {
	reg := newTestRegistry()
	const action = "notes.note.create"
	if err := reg.AuditActions.Add(action); err != nil {
		t.Fatalf("AuditActions.Add() error = %v", err)
	}

	publishErr := errors.New("bus unavailable")
	err := Emit(context.Background(), failingBus{err: publishErr}, reg.AuditActions, Input{Action: action})
	if !errors.Is(err, publishErr) {
		t.Fatalf("Emit() error = %v, want it to wrap %v", err, publishErr)
	}
}
