package rbac

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// permissionsOf reads back what a role actually grants, sorted.
func permissionsOf(t *testing.T, svc *Service, ctx context.Context, key string) []string {
	t.Helper()
	role, err := svc.roles.ByKey(ctx, key)
	if err != nil {
		t.Fatalf("ByKey(%q): %v", key, err)
	}
	rows, err := svc.rolePermissions.ByRole(ctx, role.ID)
	if err != nil {
		t.Fatalf("ByRole(%q): %v", key, err)
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Permission)
	}
	sort.Strings(out)
	return out
}

func TestService_EnsureBuiltinRoles_SeedsTheThreeRoles(t *testing.T) {
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")

	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	for _, key := range []string{BuiltinRoleOwner, BuiltinRoleAdmin, BuiltinRoleMember} {
		role, err := svc.roles.ByKey(ctx, key)
		if err != nil {
			t.Fatalf("built-in role %q was not seeded: %v", key, err)
		}
		if !role.Builtin {
			t.Fatalf("role %q was seeded without the built-in flag", key)
		}
		if role.DescriptionKey == "" {
			t.Fatalf("role %q was seeded without an i18n description key", key)
		}
	}
}

func TestService_EnsureBuiltinRoles_OwnerHoldsEveryDeclaredPermission(t *testing.T) {
	// The tenant's root authority. An owner who could not do something
	// would have no way to delegate it either, since delegation is itself
	// a permission.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	want := svc.catalog.Load().permissions()
	if got := permissionsOf(t, svc, ctx, BuiltinRoleOwner); !reflect.DeepEqual(got, want) {
		t.Fatalf("owner holds %v, want the whole catalog %v", got, want)
	}
}

func TestService_EnsureBuiltinRoles_AdminHoldsEverythingExceptGrantAuthority(t *testing.T) {
	// Without this one exclusion the owner and admin roles would be
	// identical, and an admin could grant themselves anything an owner
	// has -- which makes the distinction cosmetic rather than a boundary.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	got := permissionsOf(t, svc, ctx, BuiltinRoleAdmin)
	for _, perm := range got {
		if perm == PermissionManage {
			t.Fatalf("admin holds %q, which must stay with the owner", PermissionManage)
		}
	}
	want := make([]string, 0)
	for _, perm := range svc.catalog.Load().permissions() {
		if perm != PermissionManage {
			want = append(want, perm)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admin holds %v, want the catalog minus %q: %v", got, PermissionManage, want)
	}
}

func TestService_EnsureBuiltinRoles_MemberHoldsNothing(t *testing.T) {
	// Deny by default applies to seeding too: which permissions an
	// ordinary member should hold is a product decision, and a guess here
	// would hand out access nobody chose.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	if got := permissionsOf(t, svc, ctx, BuiltinRoleMember); len(got) != 0 {
		t.Fatalf("member holds %v, want nothing", got)
	}

	// And a member binding really grants nothing, end to end.
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err := svc.AssignRole(ctx, sub, BuiltinRoleMember, Scope{}); err != nil {
		t.Fatalf("AssignRole(member): %v", err)
	}
	for _, perm := range svc.catalog.Load().permissions() {
		ok, err := svc.Can(context.Background(), sub, "read", perm)
		if err != nil {
			t.Fatalf("Can: %v", err)
		}
		if ok {
			t.Fatalf("a member was granted %q", perm)
		}
	}
}

func TestService_EnsureBuiltinRoles_IsIdempotentAndSilentOnASecondRun(t *testing.T) {
	// It is meant to run at every boot. A second run must not duplicate a
	// role, must not rewrite permission rows, and must publish nothing --
	// otherwise a fleet restart would flush every replica's decision cache
	// for no reason.
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("first EnsureBuiltinRoles: %v", err)
	}
	before := permissionsOf(t, svc, ctx, BuiltinRoleOwner)

	rec := recordEvents(reg)
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("second EnsureBuiltinRoles: %v", err)
	}

	if got := len(rec.events); got != 0 {
		t.Fatalf("a no-op reconciliation published %d events, want 0", got)
	}
	roles, err := svc.roles.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(roles) != len(builtinRoles) {
		t.Fatalf("after two runs the tenant holds %d roles, want %d", len(roles), len(builtinRoles))
	}
	if after := permissionsOf(t, svc, ctx, BuiltinRoleOwner); !reflect.DeepEqual(after, before) {
		t.Fatalf("owner's permissions changed on a no-op run: %v -> %v", before, after)
	}
}

func TestService_EnsureBuiltinRoles_ConcurrentSeeds_NeitherFailsTheBoot(t *testing.T) {
	// EnsureBuiltinRoles runs at every boot, and replicas boot a freshly
	// created tenant concurrently. Both see the built-in roles absent and
	// both try to seed them; the loser's defineRole collides with
	// uq_rbac_roles_tenant_key. That check-then-create race must not fail
	// the loser's boot: both seeds derive identical definitions from the
	// same frozen catalog, so the winner's rows are exactly what the loser
	// would have written, and the loser treats the classified duplicate as
	// already-seeded. Reproduced deterministically through the
	// beforeRoleCreate hook: it fires inside replica A's first defineRole
	// -- the exact window a second replica's boot would land in -- and
	// replica B's whole real EnsureBuiltinRoles runs there, seeding all
	// three roles before A's own create.
	db := newRBACTestDB(t)
	_, replicaA, replicaB := twoReplicas(t, db)

	ctx := tenantCtx("tenant-a")
	replicaA.beforeRoleCreate = func() {
		replicaA.beforeRoleCreate = nil // run exactly once
		if err := replicaB.EnsureBuiltinRoles(ctx); err != nil {
			t.Fatalf("replica B's EnsureBuiltinRoles racing replica A's: %v", err)
		}
	}

	if err := replicaA.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("replica A's EnsureBuiltinRoles whose seed lost the race = %v, want nil", err)
	}

	// One clean seed's worth of rows, readable from either replica.
	for _, svc := range []*Service{replicaA, replicaB} {
		for _, key := range []string{BuiltinRoleOwner, BuiltinRoleAdmin, BuiltinRoleMember} {
			if _, err := svc.roles.ByKey(ctx, key); err != nil {
				t.Fatalf("replica %p cannot read %s after the concurrent seeds: %v", svc, key, err)
			}
		}
	}
	roles, err := replicaA.roles.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(roles) != len(builtinRoles) {
		t.Fatalf("after the racing seeds the tenant holds %d roles, want %d", len(roles), len(builtinRoles))
	}
	// And the reconciliation half still ran for the roles B seeded: the
	// owner carries exactly every declared permission.
	if got := permissionsOf(t, replicaA, ctx, BuiltinRoleOwner); !reflect.DeepEqual(got, replicaA.DeclaredPermissions()) {
		t.Fatalf("owner's permissions = %v, want every declared permission %v", got, replicaA.DeclaredPermissions())
	}
}

func TestService_EnsureBuiltinRoles_WidensOwnerWhenTheCatalogGrows(t *testing.T) {
	// The reason reconciliation exists rather than create-only seeding.
	// Adding a module to the host's build must widen every existing
	// tenant's owner; without this, a tenant created before that module
	// existed would have an owner who could not use it, with nothing
	// reporting why.
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	// Simulate the next release's catalog by re-freezing it with one more
	// permission, exactly as a new module's Register would produce.
	grown := append(svc.catalog.Load().permissions(), "storage:manage")
	grownCatalog := newCatalog(grown)
	svc.catalog.Store(&grownCatalog)

	rec := recordEvents(reg)
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("reconciling EnsureBuiltinRoles: %v", err)
	}

	owner := permissionsOf(t, svc, ctx, BuiltinRoleOwner)
	if !containsString(owner, "storage:manage") {
		t.Fatalf("owner holds %v, want the new permission included", owner)
	}
	// Owner and admin both widened, member did not, so exactly two
	// announcements went out.
	if got := len(rec.ofType(EventRoleChanged)); got != 2 {
		t.Fatalf("published %d role-changed events, want 2 (owner and admin)", got)
	}
}

func TestService_EnsureBuiltinRoles_NarrowsWhenAPermissionDisappears(t *testing.T) {
	// The other direction, and the more important one: a permission
	// removed from the catalog must stop being granted, rather than the
	// role quietly keeping authority nobody declares any more.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	shrunk := make([]string, 0)
	for _, perm := range svc.catalog.Load().permissions() {
		if perm != "billing:manage" {
			shrunk = append(shrunk, perm)
		}
	}
	shrunkCatalog := newCatalog(shrunk)
	svc.catalog.Store(&shrunkCatalog)

	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("reconciling EnsureBuiltinRoles: %v", err)
	}
	if owner := permissionsOf(t, svc, ctx, BuiltinRoleOwner); containsString(owner, "billing:manage") {
		t.Fatalf("owner still holds a permission removed from the catalog: %v", owner)
	}
}

func TestService_EnsureBuiltinRoles_SeedsOneTenantOnly(t *testing.T) {
	// The seed reads and writes within a single tenant; there is
	// deliberately no cross-tenant template to copy from.
	svc := newTestService(t)
	if err := svc.EnsureBuiltinRoles(tenantCtx("tenant-a")); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}

	roles, err := svc.roles.List(tenantCtx("tenant-b"))
	if err != nil {
		t.Fatalf("List in tenant-b: %v", err)
	}
	if len(roles) != 0 {
		t.Fatalf("seeding tenant-a created %d roles in tenant-b", len(roles))
	}
}

func TestService_EnsureBuiltinRoles_SeedsTheSystemPseudoTenant_WithNoSpecialCase(t *testing.T) {
	// Platform-operations grants are rows with tenant_id = "system".
	// Platform operations reuse this very engine through a "system"
	// pseudo-tenant rather than getting an authorization system of their
	// own, so the grant must travel the identical code path -- which is
	// exactly what this test asserts: the same call, the same result, no
	// branch.
	svc := newTestService(t)
	ctx := pkgcore.WithTenant(context.Background(), SystemDomain)

	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles in the system domain: %v", err)
	}
	operator := Subject{TenantID: SystemDomain, UserID: "operator-1"}
	if err := svc.AssignRole(ctx, operator, BuiltinRoleOwner, Scope{}); err != nil {
		t.Fatalf("AssignRole in the system domain: %v", err)
	}

	ok, err := svc.Can(context.Background(), operator, "manage", "billing")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if !ok {
		t.Fatal("a system-domain owner was denied a declared permission")
	}
	// And the platform grant does not leak into an ordinary tenant.
	inTenant := Subject{TenantID: "tenant-a", UserID: "operator-1"}
	if ok, err := svc.Can(context.Background(), inTenant, "manage", "billing"); err != nil || ok {
		t.Fatalf("the system-domain grant leaked into tenant-a: %v, %v", ok, err)
	}
}

func TestService_EnsureBuiltinRoles_WithoutATenantContext_FailsClosed(t *testing.T) {
	svc := newTestService(t)
	if err := svc.EnsureBuiltinRoles(context.Background()); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("error = %v, want pkgcore.ErrNoTenant", err)
	}
}

func TestService_EnsureBuiltinRoles_ThenOwnerCanEverything(t *testing.T) {
	// The end-to-end shape a host gets at boot: seed, bind the tenant's
	// creator to owner, and every declared permission is available.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if err := svc.EnsureBuiltinRoles(ctx); err != nil {
		t.Fatalf("EnsureBuiltinRoles: %v", err)
	}
	owner := Subject{TenantID: "tenant-a", UserID: "founder"}
	if err := svc.AssignRole(ctx, owner, BuiltinRoleOwner, Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	got, err := svc.ListPermissions(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if !reflect.DeepEqual(got, svc.catalog.Load().permissions()) {
		t.Fatalf("owner holds %v, want the whole catalog %v", got, svc.catalog.Load().permissions())
	}
	scope, err := svc.DataScope(context.Background(), owner, "write", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.TenantWide {
		t.Fatalf("owner's scope = %+v, want TenantWide", scope)
	}
}
