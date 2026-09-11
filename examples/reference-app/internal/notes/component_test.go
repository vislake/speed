package notes

import (
	"context"
	"slices"
	"testing"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared assets.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, notesComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database and the optional creator resolver, and
// pins that both reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))
	reg.Put(stubSubjectResolver{userID: "component-test-creator"})

	instance, err := notesComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *notes.Module", instance, instance)
	}
	if m.subject == nil {
		t.Error("subject is nil, want the registry's resolver wired through WithSubjectResolver")
	}
}

// TestComponent_NewWithoutTheResolver proves the optional dependency's
// absence is a legal construction: creates then fail closed at call time.
func TestComponent_NewWithoutTheResolver(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(dbtest.NewSQLite(t))

	instance, err := notesComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *notes.Module", instance, instance)
	}
	if m.subject != nil {
		t.Errorf("subject = %v, want nil without a provider", m.subject)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := notesComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so the audit action, permissions, event
// catalog, notification type and HTTP mount all land in the assembly's own
// seats, and the handler is built over the assembly's own bus and audit
// registrar.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, notesComponent,
		dbtest.NewSQLite(t),
		&stubSubjectResolver{userID: "component-test-creator"},
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	if actions := reg.AuditActions.Actions(); !slices.Contains(actions, AuditActionNoteCreate) {
		t.Errorf("AuditActions seat = %v, want the note-create action", actions)
	}
	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionWrite} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	if !slices.Contains(types, EventNoteCreated) {
		t.Errorf("Events seat = %v, want the note-created declaration", types)
	}
	var typeKeys []string
	for _, typ := range reg.Notifications.Types() {
		typeKeys = append(typeKeys, typ.Key)
	}
	if !slices.Contains(typeKeys, noteCreatedNotificationType.Key) {
		t.Errorf("Notifications seat = %v, want the note-created type", typeKeys)
	}
	if routes := reg.Routes.Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
}
