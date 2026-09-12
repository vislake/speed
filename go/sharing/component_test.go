package sharing

import (
	"context"
	"slices"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared assets.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, sharingComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database plus every optional product, and pins that
// each optional seam reached the built module when present.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))
	reg.Put(&recordingQueue{})
	reg.Put(fakeTenantConfigReader{d: 1234, ok: true})
	reg.Put(fakeResourceResolver{mime: "text/plain", body: "hello"})

	instance, err := sharingComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *sharing.Module", instance, instance)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
	if m.cfg == nil {
		t.Error("cfg is nil, want the registry's reader wired through WithTenantConfigReader")
	}
	if m.resolver == nil {
		t.Error("resolver is nil, want the registry's resolver wired through WithResourceResolver")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal construction: only the database is needed.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))

	instance, err := sharingComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *sharing.Module", instance, instance)
	}
	if m.queue != nil || m.cfg != nil || m.resolver != nil {
		t.Errorf("optional seams = (%v, %v, %v), want all nil without providers", m.queue, m.cfg, m.resolver)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := sharingComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so its full surface -- permissions, the audit
// action, the event catalog, the configuration items, the expiry sweep's
// handler and schedule, the access-log retention participant and both HTTP
// mounts -- lands in the assembly's own seats, and the service takes the
// assembly's declaration face.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, sharingComponent,
		newTestDB(t),
		&recordingQueue{},
		&fakeTenantConfigReader{d: 1234, ok: true},
		&fakeResourceResolver{mime: "text/plain", body: "hello"},
		pkgcore.NewMemoryKVStore(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionCreate, PermissionRevoke} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	if actions := reg.AuditActions.Actions(); !slices.Contains(actions, AuditActionSensitiveShareCreate) {
		t.Errorf("AuditActions seat = %v, want the sensitive-share audit action", actions)
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	if !slices.Contains(types, EventShareAccessed) || !slices.Contains(types, EventShareCreated) {
		t.Errorf("Events seat = %v, want the module's share event declarations", types)
	}
	if items := reg.Config.Items(); len(items) == 0 {
		t.Error("Config seat is empty, want the module's configuration items")
	}
	if _, claimed := reg.Jobs.Handlers()[taskTypeExpirySweep]; !claimed {
		t.Errorf("Jobs seat = %v, want the expiry-sweep handler", reg.Jobs.Handlers())
	}
	if decls := reg.Schedules.Declarations(); len(decls) != 1 || decls[0].Type != taskTypeExpirySweep {
		t.Errorf("Schedules seat = %v, want the expiry-sweep schedule", decls)
	}
	if participants := reg.Retention.Participants(); len(participants) != 1 {
		t.Errorf("Retention seat = %v, want the access-log retention participant", participants)
	}
	if routes := componenttest.FaceOf(reg).Routes(); len(routes) != 2 {
		t.Fatalf("Init mounted %v, want the access and tenant-scoped share mounts", routes)
	}

	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
	if m.svc.host == nil {
		t.Error("the service did not take the assembly's declaration face")
	}
}
