package integration

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the ConfigSchema's empty-config
// decode and the token shapes.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, integrationComponent)
}

// TestComponent_NewBuildsAConfiguredModule drives the component's New over a
// registry carrying the database plus every consumed optional product, and
// pins that the configuration and the optional seams reached the built
// module.
func TestComponent_NewBuildsAConfiguredModule(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))
	reg.Put(&fakeQueue{})
	reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-1", true }))
	reg.Put(MembershipCheckerFunc(func(context.Context, string, string) (bool, error) { return true, nil }))

	instance, err := integrationComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(map[string]any{
		"max_api_key_lifetime": "720h",
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *integration.Module", instance, instance)
	}
	if m.maxLifetime != 720*time.Hour {
		t.Errorf("maxLifetime = %v, want the configured 720h", m.maxLifetime)
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithWebhookQueue")
	}
	if m.subject == nil {
		t.Error("subject is nil, want the registry's resolver wired through WithSubjectResolver")
	}
	if m.membership == nil {
		t.Error("membership is nil, want the registry's checker wired through WithMembershipChecker")
	}
}

// TestComponent_NewWithoutOptionalSeams proves the optional dependencies'
// absence is a legal construction: only the database is needed.
func TestComponent_NewWithoutOptionalSeams(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	reg.Put(newTestDB(t))

	instance, err := integrationComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *integration.Module", instance, instance)
	}
	if m.queue != nil || m.subject != nil || m.membership != nil {
		t.Errorf("optional seams = (%v, %v, %v), want all nil without providers", m.queue, m.subject, m.membership)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := integrationComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly: the module's Register runs inside the one stage
// whose seats accept writes -- permissions, audit actions, the per-event
// subscriptions, both job handlers, the expiry sweep's schedule and the
// HTTP mount all land in the assembly's own seats -- and Attach then builds
// the runtime Service over the assembly's own bus and audit registrar and
// puts it into the by-type context.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	// The func-typed seam implementations are passed by address: the
	// assembly resolves a requirement against a typed pointer to the
	// contract type, which a bare func value is not.
	lister := PermissionListerFunc(func(context.Context, string, string) ([]string, error) { return []string{"notes:read"}, nil })
	checker := MembershipCheckerFunc(func(context.Context, string, string) (bool, error) { return true, nil })
	subject := SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-1", true })
	if err := componenttest.RunInit(t, reg, integrationComponent,
		newTestDB(t),
		&lister,
		&fakeQueue{},
		&checker,
		&subject,
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionRead, PermissionManage, PermissionWebhookRead, PermissionWebhookManage} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	if actions := reg.AuditActions.Actions(); len(actions) == 0 {
		t.Error("AuditActions seat is empty, want the module's audit vocabulary")
	}
	handlers := reg.Jobs.Handlers()
	for _, want := range []string{jobTypeWebhookDeliver, jobTypeAPIKeyExpirySweep} {
		if _, claimed := handlers[want]; !claimed {
			t.Errorf("Jobs seat = %v, want the %q handler", handlers, want)
		}
	}
	if decls := reg.Schedules.Declarations(); len(decls) != 1 || decls[0].Type != jobTypeAPIKeyExpirySweep {
		t.Errorf("Schedules seat = %v, want the API-key expiry sweep", decls)
	}
	if routes := componenttest.FaceOf(reg).Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}

	// Attach ran inside the Init stage: the Service is the module's own and
	// is reachable through the by-type context.
	if m.service == nil {
		t.Fatal("the runtime Service was not built by Init")
	}
	svc, err := pkgcore.Get[*Service](reg)
	if err != nil {
		t.Fatalf("the published Service: %v", err)
	}
	if svc != m.service {
		t.Error("the published Service is not the module's own")
	}
	if svc.bus == nil || svc.auditActions == nil {
		t.Errorf("Service seams = (%v, %v), want the assembly's bus and audit registrar", svc.bus, svc.auditActions)
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
	if m.membership == nil {
		t.Error("membership is nil, want the assembly's checker wired through WithMembershipChecker")
	}
}
