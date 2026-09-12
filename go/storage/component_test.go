package storage

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/storage/internal/testutil"
	"github.com/vislake/speed/go/storage/migrations"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the ConfigSchema's empty-config
// decode and the shape of every Requires/Provides token.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, storageComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the two products it consumes at construction, with a
// configuration block covering both the scalar knobs and the boolean one,
// and pins that every value reached the built module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(testutil.NewSQLite(t, moduleName, migrations.FS))
	reg.Put(stubQueue{})

	instance, err := storageComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(map[string]any{
		"max_upload_bytes":  int64(4096),
		"upload_ttl":        "15m",
		"no_expiry_allowed": true,
		"allowed_types":     []any{"image/jpeg"},
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *storage.Module", instance, instance)
	}
	if m.maxUploadBytes != 4096 {
		t.Errorf("maxUploadBytes = %d, want the configured 4096", m.maxUploadBytes)
	}
	if m.uploadTTL != 15*time.Minute {
		t.Errorf("uploadTTL = %v, want the configured 15m", m.uploadTTL)
	}
	if !m.noExpiryAllowed {
		t.Error("noExpiryAllowed = false, want the configured true")
	}
	if len(m.allowedTypes) != 1 || m.allowedTypes[0] != "image/jpeg" {
		t.Errorf("allowedTypes = %v, want the configured [image/jpeg]", m.allowedTypes)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape: a
// registry carrying none of the component's required products fails the
// construction naming the missing one instead of building a half-wired
// module.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := storageComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes, so its full surface -- permissions, audit
// vocabulary, event catalog, both job handlers, the expiry sweep's schedule
// and the HTTP mount -- lands in the assembly's own seats, and the services
// take the assembly's own object store and bus.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := testutil.NewSQLite(t, moduleName, migrations.FS)
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, storageComponent, db, &stubQueue{}, pkgcore.NewLocalObjectStore(t.TempDir()), bus); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionWrite} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	actions := reg.AuditActions.Actions()
	for _, want := range []string{AuditActionObjectCreate, AuditActionObjectComplete, AuditActionObjectDelete} {
		if !slices.Contains(actions, want) {
			t.Errorf("AuditActions seat = %v, want the %q declaration", actions, want)
		}
	}
	var types []string
	for _, decl := range reg.Events.Published() {
		types = append(types, decl.Type)
	}
	for _, want := range []string{EventObjectCompleted, EventObjectDeleted} {
		if !slices.Contains(types, want) {
			t.Errorf("Events seat = %v, want the %q declaration", types, want)
		}
	}
	handlers := reg.Jobs.Handlers()
	for _, want := range []string{taskTypeDeriveThumbnail, taskTypeExpirySweep} {
		if _, ok := handlers[want]; !ok {
			t.Errorf("Jobs seat = %v, want the %q handler", handlers, want)
		}
	}
	if decls := reg.Schedules.Declarations(); len(decls) != 1 || decls[0].Type != taskTypeExpirySweep {
		t.Errorf("Schedules seat = %v, want the expiry-sweep schedule", decls)
	}
	if routes := componenttest.FaceOf(reg).Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}

	// The declarations reached the running services: Register hands each of
	// the three services the assembly's own view, and builds the handler
	// over them while the seats are open.
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
	if m.svc.host == nil || m.derive.host == nil || m.life.host == nil {
		t.Error("the services did not take the assembly's declaration face")
	}
}
