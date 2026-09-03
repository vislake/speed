package rbac

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// testPermissions is what the host's OTHER modules declare in these tests.
// rbac's own two are added by its Register, so the frozen catalog under
// test is always a mixed one -- which is the realistic case, and the only
// one in which "a permission nobody declared" is distinguishable from "a
// permission this module did not declare".
var testPermissions = []string{"notes:read", "notes:write", "billing:manage"}

// stubResolver is the host's SubtreeResolver seam under test: an
// organization tree reduced to the one fact rbac is allowed to know.
type stubResolver struct {
	// paths maps node id to materialized path. A node absent from the map
	// is reported as not existing.
	paths map[string]string

	// err, when set, is returned instead of any answer -- the "the tree is
	// unreachable" case, which must be distinguishable from "no such node".
	err error

	// calls counts NodePath invocations, so a test can assert that the
	// resolver is NOT consulted on the paths that must not consult it.
	calls int
}

func (r *stubResolver) NodePath(_ context.Context, nodeID string) (string, bool, error) {
	r.calls++
	if r.err != nil {
		return "", false, r.err
	}
	path, ok := r.paths[nodeID]
	return path, ok, nil
}

// newTestService returns an attached Service over a private SQLite
// database and an in-memory bus, with testPermissions declared by a stand-in
// module so the frozen catalog is not just rbac's own two entries.
func newTestService(t *testing.T, opts ...Option) *Service {
	t.Helper()
	svc, _ := newTestServiceWithRegistry(t, opts...)
	return svc
}

// newTestServiceWithRegistry is newTestService plus the registry, for the
// tests that need to subscribe to the events the Service publishes.
func newTestServiceWithRegistry(t *testing.T, opts ...Option) (*Service, *pkgcore.Registry) {
	t.Helper()
	return attachTestService(t, newRBACTestDB(t), opts...)
}

// attachTestService attaches a Service to an already-open database, so two
// Services can be attached to the SAME database -- the shape a
// multi-replica deployment has, and what the cross-replica invalidation
// tests need.
func attachTestService(t *testing.T, db *gorm.DB, opts ...Option) (*Service, *pkgcore.Registry) {
	t.Helper()

	reg := newPlainRegistry()
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring the host's permissions: %v", err)
	}
	module := NewModule(db, opts...)
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return svc, reg
}

// tenantCtx is the context a host hands in after tenancy.Middleware ran.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// grant defines a role holding permissions and binds it to sub at scope,
// which is the two-step setup nearly every evaluation test needs.
func grant(t *testing.T, svc *Service, sub Subject, roleKey string, scope Scope, permissions ...string) {
	t.Helper()
	ctx := tenantCtx(sub.TenantID)
	if _, err := svc.roles.ByKey(ctx, roleKey); err != nil {
		if _, err := svc.DefineRole(ctx, RoleDefinition{
			Key:            roleKey,
			DescriptionKey: "rbac.role.member",
			Permissions:    permissions,
		}); err != nil {
			t.Fatalf("DefineRole(%q): %v", roleKey, err)
		}
	}
	if err := svc.AssignRole(ctx, sub, roleKey, scope); err != nil {
		t.Fatalf("AssignRole(%q): %v", roleKey, err)
	}
}

func TestService_Close_IsIdempotent(t *testing.T) {
	svc := newTestService(t)
	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The t.Cleanup registered by the fixture closes it a second time; a
	// third here proves the idempotence is not accidental.
	if err := svc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestService_Close_LeavesDecisionsCorrect(t *testing.T) {
	// Close only silences the janitor. A host that shuts rbac down while
	// request traffic drains must not start serving from a cache that
	// nothing expires any more.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	if err := svc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after Close: %v", err)
	}
	if !ok {
		t.Fatal("Can after Close = false, want true")
	}
}

func TestService_Can_SecondCall_IsServedFromTheCache(t *testing.T) {
	// The cache's whole purpose: docs/internal/05-identity-and-access.md
	// requires decisions to be served from a policy cache rather than
	// loaded in full on every request. Proven here by deleting the binding
	// rows behind the Service's back -- a read-through would now deny.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	ctx := context.Background()
	if ok, err := svc.Can(ctx, sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("first Can = %v, %v; want true, nil", ok, err)
	}

	deleteAllBindings(t, svc, sub.TenantID)

	if ok, err := svc.Can(ctx, sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("second Can = %v, %v; want true from the cache", ok, err)
	}
}

func TestService_Can_AfterTTL_ReadsThroughAgain(t *testing.T) {
	// The anti-loss net at the Service level: with every event lost, a
	// grant change still takes effect within one TTL.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	base := time.Now()
	svc.now = func() time.Time { return base }
	if ok, _ := svc.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("the grant was not visible at all")
	}

	deleteAllBindings(t, svc, sub.TenantID)
	svc.now = func() time.Time { return base.Add(DefaultCacheTTL) }

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after the TTL: %v", err)
	}
	if ok {
		t.Fatal("Can = true one TTL after the binding was deleted, want false (the entry must have expired)")
	}
}

func TestService_RevokeRole_InvalidatesTheCacheImmediately(t *testing.T) {
	// A stale authorization cache keeps answering "yes" after a revoke,
	// which is a security failure rather than a performance one. The
	// standalone bus delivers synchronously, so the revoke must be visible
	// on the very next call -- not one TTL later.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	ctx := context.Background()
	if ok, _ := svc.Can(ctx, sub, "read", "notes"); !ok {
		t.Fatal("the grant was not visible before the revoke")
	}

	if err := svc.RevokeRole(tenantCtx(sub.TenantID), sub, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}

	ok, err := svc.Can(ctx, sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can after the revoke: %v", err)
	}
	if ok {
		t.Fatal("Can = true immediately after a revoke, want false")
	}
}

func TestService_RevokeOnOneReplica_InvalidatesTheOther(t *testing.T) {
	// Two Services over one database and one bus: the shape of a
	// multi-replica deployment, with the in-memory bus standing in for the
	// Redis Streams one. Replica B must not keep serving a decision
	// replica A withdrew. (The distributed leg of the same property, over
	// a real Redis, is the module's integration tier.)
	db := newRBACTestDB(t)
	reg := newPlainRegistry()
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring permissions: %v", err)
	}

	replicaA := attachReplica(t, db, reg)
	replicaB := attachReplica(t, db, reg)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, replicaA, sub, "reader", Scope{}, "notes:read")

	// Warm replica B's cache, so this is genuinely about invalidation and
	// not about B never having cached anything.
	if ok, _ := replicaB.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("replica B did not see the grant")
	}

	if err := replicaA.RevokeRole(tenantCtx(sub.TenantID), sub, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole on replica A: %v", err)
	}

	ok, err := replicaB.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can on replica B: %v", err)
	}
	if ok {
		t.Fatal("replica B still grants a permission replica A revoked")
	}
}

// attachReplica attaches one more Service to a shared database and
// registry, the way a second process would.
func attachReplica(t *testing.T, db *gorm.DB, reg *pkgcore.Registry) *Service {
	t.Helper()
	module := NewModule(db)
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return svc
}

func TestService_OnRoleBindingChanged_ForeignPayload_IsNotAnError(t *testing.T) {
	// The handler runs inside the publisher's Publish call on the
	// in-memory bus. An error here would report an already-committed write
	// as failed, so an unrecognized payload is dropped instead.
	svc := newTestService(t)
	err := svc.onRoleBindingChanged(context.Background(), pkgcore.Event{
		Type:    EventRoleBindingAssigned,
		Payload: "not a payload",
	})
	if err != nil {
		t.Fatalf("onRoleBindingChanged with a foreign payload: %v", err)
	}
	if err := svc.onRoleChanged(context.Background(), pkgcore.Event{Payload: 42}); err != nil {
		t.Fatalf("onRoleChanged with a foreign payload: %v", err)
	}
}

func TestService_OnRoleChanged_InvalidatesTheWholeTenant(t *testing.T) {
	// A role's permission set changed and the cache stores grants already
	// flattened through their roles, so every subject in the tenant is
	// suspect -- including ones whose own bindings did not change.
	svc := newTestService(t)
	subA := Subject{TenantID: "tenant-a", UserID: "user-1"}
	subB := Subject{TenantID: "tenant-a", UserID: "user-2"}
	other := Subject{TenantID: "tenant-b", UserID: "user-1"}

	grant(t, svc, subA, "reader", Scope{}, "notes:read")
	grant(t, svc, subB, "reader", Scope{}, "notes:read")
	grant(t, svc, other, "reader", Scope{}, "notes:read")

	ctx := context.Background()
	for _, sub := range []Subject{subA, subB, other} {
		if ok, _ := svc.Can(ctx, sub, "read", "notes"); !ok {
			t.Fatalf("%v did not see the grant", sub)
		}
	}

	if err := svc.onRoleChanged(ctx, pkgcore.Event{
		Type:    EventRoleChanged,
		Payload: RoleChangedEvent{TenantID: "tenant-a"},
	}); err != nil {
		t.Fatalf("onRoleChanged: %v", err)
	}

	if svc.cache.len() != 1 {
		t.Fatalf("the cache holds %d entries after a tenant-a role change, want 1 (tenant-b's)", svc.cache.len())
	}
	if _, ok := svc.cache.get(grantKey{tenant: "tenant-b", user: "user-1"}, time.Now()); !ok {
		t.Fatal("a tenant-a role change dropped tenant-b's cached grants")
	}
}

func TestService_PublishFailure_IsReportedAndTheCacheIsStillInvalidated(t *testing.T) {
	// The row is committed before the event is published. A bus failure
	// must not leave THIS process serving the obsolete decision, and must
	// still be reported so the caller knows the other replicas were not
	// told.
	db := newRBACTestDB(t)
	reg := pkgcore.NewRegistry(&failingBus{}, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring permissions: %v", err)
	}
	module := NewModule(db)
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := module.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err == nil {
		t.Fatal("DefineRole reported success although the bus rejected the announcement")
	} else if !hasCode(err, ErrStorage.Code) {
		t.Fatalf("DefineRole error = %v, want %s", err, ErrStorage.Code)
	}

	// The role row itself was written: the failure is in the announcement,
	// not the write, and the caller must be able to see that state.
	if _, err := svc.roles.ByKey(ctx, "reader"); err != nil {
		t.Fatalf("the role row was not written: %v", err)
	}
}

// failingBus is an EventBus whose Publish always fails, for the "the row
// is committed but the announcement did not get out" case.
type failingBus struct{}

var errBusDown = errors.New("bus down")

func (*failingBus) Publish(context.Context, pkgcore.Event) error { return errBusDown }

func (*failingBus) Subscribe(string, pkgcore.EventHandler) {}

// deleteAllBindings removes a tenant's binding rows behind the Service's
// back, without publishing anything. It is how a test proves an answer
// came from the cache rather than from the database.
func deleteAllBindings(t *testing.T, svc *Service, tenant pkgcore.TenantID) {
	t.Helper()
	ctx := tenantCtx(tenant)
	rows, err := svc.bindings.List(ctx)
	if err != nil {
		t.Fatalf("listing bindings: %v", err)
	}
	for _, row := range rows {
		if err := svc.bindings.Delete(ctx, row.ID); err != nil {
			t.Fatalf("deleting binding %s: %v", row.ID, err)
		}
	}
}
