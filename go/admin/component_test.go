package admin

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// TestComponent_WellFormed runs the descriptor contract assertions over the
// registered component: the name convention, the token shapes and the
// declared system purpose.
func TestComponent_WellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, adminComponent)
}

// TestComponent_RequiresPinsTheTokenSet pins the component's dependency
// declaration exactly: the database and the queue, the products of the four
// modules the console reads mandatorily, the rbac product whose requirement
// orders admin's Start turn after rbac's -- the Start callback reads the
// published *rbac.Service there, at its use time, so the token names the
// module product, never the service -- and the two optional module
// products. A change here is a change to what a composition must select for
// admin to assemble, so it is asserted token by token rather than by count
// alone.
func TestComponent_RequiresPinsTheTokenSet(t *testing.T) {
	want := []struct {
		token    string
		optional bool
	}{
		{"*gorm.DB", false},
		{"*jobs.Queue", false},
		{"*authn.Module", false},
		{"*org.Module", false},
		{"*compliance.Module", false},
		{"*notification.Module", false},
		{"*rbac.Module", false},
		{"*metering.Module", true},
		{"*billing.Module", true},
		{"*pkgcore.RouteRegistrar", true},
	}
	if len(adminComponent.Requires) != len(want) {
		t.Fatalf("Requires has %d entries, want %d", len(adminComponent.Requires), len(want))
	}
	for i, w := range want {
		got := adminComponent.Requires[i]
		if name := reflect.TypeOf(got.Token).String(); name != w.token {
			t.Errorf("Requires[%d].Token = %s, want %s", i, name, w.token)
		}
		if got.Optional != w.optional {
			t.Errorf("Requires[%d] (%s) Optional = %v, want %v", i, w.token, got.Optional, w.optional)
		}
	}
	if len(adminComponent.SystemPurposes) != 1 || adminComponent.SystemPurposes[0] != SystemPurposeAdminCrossTenant {
		t.Errorf("SystemPurposes = %v, want [%s]", adminComponent.SystemPurposes, SystemPurposeAdminCrossTenant)
	}
}

// TestComponent_NewBuildsTheFullFanIn drives the component's New over the
// real module graph buildTestAdminModule stands up, and pins that every
// mandatory module product, the queue and the two optional modules reached
// the built console module.
func TestComponent_NewBuildsTheFullFanIn(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(env.DB)
	reg.Put(env.Queue)
	reg.Put(env.Authn)
	reg.Put(env.Org)
	reg.Put(env.Compliance)
	reg.Put(env.Notification)
	reg.Put(env.Metering)
	reg.Put(env.Billing)

	instance, err := adminComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *admin.Module", instance, instance)
	}
	if m.authnModule != env.Authn || m.orgModule != env.Org || m.complianceModule != env.Compliance || m.notificationModule != env.Notification {
		t.Error("mandatory module products did not reach the built module")
	}
	if m.meteringModule != env.Metering || m.billingModule != env.Billing {
		t.Error("optional module products did not reach the built module")
	}
	if m.queue == nil {
		t.Error("queue is nil, want the registry's queue wired through WithQueue")
	}
}

// TestComponent_NewWithoutOptionalModules proves the optional module
// products' absence is a legal construction: metering and billing are the
// only dependencies not needed for a boot.
func TestComponent_NewWithoutOptionalModules(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	reg.Put(env.DB)
	reg.Put(env.Queue)
	reg.Put(env.Authn)
	reg.Put(env.Org)
	reg.Put(env.Compliance)
	reg.Put(env.Notification)

	instance, err := adminComponent.New(context.Background(), reg, pkgcore.NewComponentConfig(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m, ok := instance.(*Module)
	if !ok || m == nil {
		t.Fatalf("New returned %T (%v), want a non-nil *admin.Module", instance, instance)
	}
	if m.meteringModule != nil || m.billingModule != nil {
		t.Errorf("optional modules = (%v, %v), want both nil without providers", m.meteringModule, m.billingModule)
	}
}

// TestComponent_NewFailsWithoutTheDatabase pins the fail-closed shape.
func TestComponent_NewFailsWithoutTheDatabase(t *testing.T) {
	instance, err := adminComponent.New(context.Background(), pkgcore.NewComponentRegistry(), pkgcore.NewComponentConfig(nil))
	if err == nil {
		t.Fatalf("New = %v, nil error; want the missing database reported", instance)
	}
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor's Init
// through a real assembly over the module graph buildTestAdminModule stands
// up: the module's Register runs inside the one stage whose seats accept
// writes -- permissions, audit vocabulary, the impersonation notification
// type, the audit-export handler and the HTTP mount all land in the
// assembly's own seats. The rbac binding is deliberately NOT part of this
// stage: both the role and the impersonation services still hold nothing
// when the stage closes, fail-closed exactly as they stay for a host that
// never wires the seam; the Start test drives the assembly's next stage,
// where the descriptor binds the published *rbac.Service.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, adminComponent,
		env.DB,
		env.Queue,
		env.Authn,
		env.Org,
		env.Compliance,
		env.Notification,
		env.RBACModule,
		env.RBAC,
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	perms := reg.Permissions.Permissions()
	for _, want := range []string{PermissionAccess, PermissionImpersonate, PermissionAuditExport} {
		if !slices.Contains(perms, want) {
			t.Errorf("Permissions seat = %v, want the %q declaration", perms, want)
		}
	}
	actions := reg.AuditActions.Actions()
	for _, want := range []string{AuditActionImpersonationStarted, AuditActionAuditExport} {
		if !slices.Contains(actions, want) {
			t.Errorf("AuditActions seat = %v, want the %q declaration", actions, want)
		}
	}
	var typeKeys []string
	for _, typ := range reg.Notifications.Types() {
		typeKeys = append(typeKeys, typ.Key)
	}
	if !slices.Contains(typeKeys, NotificationTypeImpersonationStarted) {
		t.Errorf("Notifications seat = %v, want the impersonation-started type", typeKeys)
	}
	if _, claimed := reg.Jobs.Handlers()[jobTypeAuditExport]; !claimed {
		t.Errorf("Jobs seat = %v, want the audit-export handler", reg.Jobs.Handlers())
	}
	if routes := componenttest.FaceOf(reg).Routes(); len(routes) != 1 || routes[0].Path != APIPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, APIPath)
	}

	// AttachRBAC is the Start stage's binding, not Init's: even with the
	// assembly's own *rbac.Service already in the by-type context, both
	// services still hold nothing at the close of Init -- fail-closed
	// exactly as they stay for a host that never wires the seam.
	if m.roles.svc != nil {
		t.Error("RoleService took an rbac Service during Init; the binding is the Start callback's own turn")
	}
	if m.impersonation.rbacSvc != nil {
		t.Error("ImpersonationService took an rbac Service during Init; the binding is the Start callback's own turn")
	}
	if m.handler == nil {
		t.Error("the module's HTTP handler was not built by Init")
	}
}

// TestComponent_StartBindsTheRBACService drives the descriptor through the
// Start stage RunInit leaves ready: rbac's published *rbac.Service sits in
// the by-type context from its own Init turn, and Start's one job is
// binding it onto the role and impersonation services -- the turn the
// requirement on rbac's product orders after rbac's own Start, which
// completed the catalog snapshot the service decides through.
func TestComponent_StartBindsTheRBACService(t *testing.T) {
	env := buildTestAdminModule(t)

	reg := pkgcore.NewComponentRegistry()
	if err := componenttest.RunInit(t, reg, adminComponent,
		env.DB,
		env.Queue,
		env.Authn,
		env.Org,
		env.Compliance,
		env.Notification,
		env.RBACModule,
		env.RBAC,
		pkgcore.NewMemoryEventBus(),
	); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}

	if err := reg.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if m.roles.svc != env.RBAC {
		t.Error("RoleService did not take the assembly's rbac Service at Start")
	}
	if m.impersonation.rbacSvc != env.RBAC {
		t.Error("ImpersonationService did not take the assembly's rbac Service at Start")
	}
}
