package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// newTestRegistry returns a fresh *pkgcore.ComponentRegistry wired with in-memory
// infrastructure, giving this file's tests a real
// pkgcore.AuditActionRegistrar (reg.AuditActions) and a real
// pkgcore.EventBus (reg.Events.Bus()), with declares driven inside the one
// Init stage the declaration seats accept writes in.
func newTestRegistry(t *testing.T, declares ...func(*pkgcore.ComponentRegistry) error) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := componenttest.NewRegistry()
	if err := componenttest.DeclareAll(reg, declares...); err != nil {
		t.Fatalf("componenttest: declare: %v", err)
	}
	return reg
}

func TestEmit_UnregisteredAction_ReturnsErrActionNotRegistered(t *testing.T) {
	reg := newTestRegistry(t)

	err := Emit(context.Background(), reg.Events.Bus(), reg.AuditActions, Input{Action: "notes.note.create"})
	if !errors.Is(err, ErrActionNotRegistered) {
		t.Fatalf("Emit() error = %v, want ErrActionNotRegistered", err)
	}
}

func TestEmit_RegisteredAction_PublishesRecordedEvent(t *testing.T) {
	const action = "notes.note.create"
	reg := newTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	})

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
	const action = "notes.note.create"
	reg := newTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	})

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
	if got.IP != "" || got.UserAgent != "" || got.TraceID != "" {
		t.Errorf("request-context fields = (%q, %q, %q), want all empty when the context carried no RequestMetadata",
			got.IP, got.UserAgent, got.TraceID)
	}
}

// TestEmit_RequestMetadataInContext_CarriesTheTrio pins Emit's read of the
// dbkit.RequestMetadata context carrier -- the write path for the three
// request-context columns (IP, UserAgent, TraceID) of audit_events (model.go):
// when the caller's context carries request metadata, the published
// RecordedEvent carries the trio, exactly as it carries Actor and TenantID,
// so the persister on the other side of the bus can store them.
func TestEmit_RequestMetadataInContext_CarriesTheTrio(t *testing.T) {
	const action = "notes.note.create"
	reg := newTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	})

	var got RecordedEvent
	reg.Events.Subscribe(EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		got = evt.Payload.(RecordedEvent)
		return nil
	})

	ctx := dbkit.WithRequestMetadata(context.Background(), dbkit.RequestMetadata{
		IP:        "203.0.113.7",
		UserAgent: "speed-client/1.0",
		TraceID:   "trace-abc",
	})
	if err := Emit(ctx, reg.Events.Bus(), reg.AuditActions, Input{Action: action}); err != nil {
		t.Fatalf("Emit() error = %v", err)
	}

	if got.IP != "203.0.113.7" {
		t.Errorf("IP = %q, want the request metadata's IP", got.IP)
	}
	if got.UserAgent != "speed-client/1.0" {
		t.Errorf("UserAgent = %q, want the request metadata's UserAgent", got.UserAgent)
	}
	if got.TraceID != "trace-abc" {
		t.Errorf("TraceID = %q, want the request metadata's TraceID", got.TraceID)
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
	const action = "notes.note.create"
	reg := newTestRegistry(t, func(r *pkgcore.ComponentRegistry) error {
		return r.AuditActions.Add(action)
	})

	publishErr := errors.New("bus unavailable")
	err := Emit(context.Background(), failingBus{err: publishErr}, reg.AuditActions, Input{Action: action})
	if !errors.Is(err, publishErr) {
		t.Fatalf("Emit() error = %v, want it to wrap %v", err, publishErr)
	}
}
