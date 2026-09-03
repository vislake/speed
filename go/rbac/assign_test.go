package rbac

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// eventRecorder collects the events a Service publishes, so a test can
// assert on the announcement as well as on the row. The in-memory bus
// delivers synchronously inside the publishing call, so no synchronization
// is needed here.
type eventRecorder struct {
	events []pkgcore.Event
}

func (r *eventRecorder) record(_ context.Context, evt pkgcore.Event) error {
	r.events = append(r.events, evt)
	return nil
}

func (r *eventRecorder) ofType(eventType string) []pkgcore.Event {
	var out []pkgcore.Event
	for _, evt := range r.events {
		if evt.Type == eventType {
			out = append(out, evt)
		}
	}
	return out
}

// recordEvents subscribes a recorder to every event this module publishes.
func recordEvents(reg *pkgcore.Registry) *eventRecorder {
	rec := &eventRecorder{}
	for _, eventType := range []string{EventRoleBindingAssigned, EventRoleBindingRevoked, EventRoleChanged} {
		reg.Events.Subscribe(eventType, rec.record)
	}
	return rec
}

func TestService_DefineRole_CreatesTheRoleAndItsPermissions(t *testing.T) {
	svc, reg := newTestServiceWithRegistry(t)
	rec := recordEvents(reg)
	ctx := tenantCtx("tenant-a")

	role, err := svc.DefineRole(ctx, RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:write", "notes:read", "notes:read"},
	})
	if err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	if role.ID == "" {
		t.Fatal("the role was created without an id")
	}
	if role.Builtin {
		t.Fatal("a role defined through the public API was marked built-in")
	}
	if role.GetTenantID() != pkgcore.TenantID("tenant-a") {
		t.Fatalf("the role belongs to %q, want tenant-a", role.GetTenantID())
	}

	rows, err := svc.rolePermissions.ByRole(ctx, role.ID)
	if err != nil {
		t.Fatalf("ByRole: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Permission)
	}
	// Sorted and de-duplicated, so that two definitions listing the same
	// permissions in different orders produce identical rows.
	if !reflect.DeepEqual(got, []string{"notes:read", "notes:write"}) {
		t.Fatalf("stored permissions = %v, want [notes:read notes:write]", got)
	}

	published := rec.ofType(EventRoleChanged)
	if len(published) != 1 {
		t.Fatalf("published %d role-changed events, want 1", len(published))
	}
	payload, ok := published[0].Payload.(RoleChangedEvent)
	if !ok {
		t.Fatalf("payload is %T, want RoleChangedEvent", published[0].Payload)
	}
	if payload.RoleKey != "reader" || payload.TenantID != "tenant-a" {
		t.Fatalf("payload = %+v, want the reader role in tenant-a", payload)
	}
	if !reflect.DeepEqual(payload.Permissions, []string{"notes:read", "notes:write"}) {
		t.Fatalf("payload permissions = %v, want the sorted set", payload.Permissions)
	}
}

func TestService_DefineRole_UndeclaredPermission_IsRejected(t *testing.T) {
	// Grant time is where strictness belongs: a typo stored as a row would
	// be a role that appears to grant something and silently grants
	// nothing, which nobody would notice until an incident.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")

	_, err := svc.DefineRole(ctx, RoleDefinition{Key: "typo", Permissions: []string{"notes:read", "notes:wirte"}})
	if !hasCode(err, ErrUnknownPermission.Code) {
		t.Fatalf("error = %v, want %s", err, ErrUnknownPermission.Code)
	}
	// Nothing was written: validation runs before the first insert, so a
	// rejected definition leaves no half-created role behind.
	if _, err := svc.roles.ByKey(ctx, "typo"); !isRoleNotFound(err) {
		t.Fatalf("ByKey after a rejected definition = %v, want role_not_found", err)
	}
}

func TestService_DefineRole_DuplicateKey_IsRejected(t *testing.T) {
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	def := RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}

	if _, err := svc.DefineRole(ctx, def); err != nil {
		t.Fatalf("first DefineRole: %v", err)
	}
	if _, err := svc.DefineRole(ctx, def); !hasCode(err, ErrDuplicateRole.Code) {
		t.Fatalf("second DefineRole error = %v, want %s", err, ErrDuplicateRole.Code)
	}
}

func TestService_DefineRole_SameKeyInAnotherTenant_IsAllowed(t *testing.T) {
	// Role keys are unique WITHIN a tenant. Every tenant has an "owner";
	// rejecting the second one would make the module single-tenant.
	svc := newTestService(t)
	def := RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}

	if _, err := svc.DefineRole(tenantCtx("tenant-a"), def); err != nil {
		t.Fatalf("DefineRole in tenant-a: %v", err)
	}
	if _, err := svc.DefineRole(tenantCtx("tenant-b"), def); err != nil {
		t.Fatalf("DefineRole in tenant-b: %v", err)
	}
}

func TestService_DefineRole_WithoutATenantContext_FailsClosed(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.DefineRole(context.Background(), RoleDefinition{Key: "reader"})
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("error = %v, want pkgcore.ErrNoTenant", err)
	}
}

func TestService_AssignRole_CreatesTheBindingAndAnnouncesIt(t *testing.T) {
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	rec := recordEvents(reg)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	// An acting subject on the context, so the audit-facing actor field is
	// populated the way a host's middleware would populate it.
	actingCtx := WithSubject(ctx, Subject{TenantID: "tenant-a", UserID: "admin-9"})
	if err := svc.AssignRole(actingCtx, sub, "reader", Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	bindings, err := svc.bindings.ByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("stored %d bindings, want 1", len(bindings))
	}
	if bindings[0].NodeID != "node-7" {
		t.Fatalf("binding node = %q, want node-7", bindings[0].NodeID)
	}

	published := rec.ofType(EventRoleBindingAssigned)
	if len(published) != 1 {
		t.Fatalf("published %d assigned events, want 1", len(published))
	}
	payload := published[0].Payload.(RoleBindingChangedEvent)
	if payload.UserID != "user-1" || payload.RoleKey != "reader" || payload.NodeID != "node-7" {
		t.Fatalf("payload = %+v", payload)
	}
	if payload.ActorUserID != "admin-9" {
		t.Fatalf("payload actor = %q, want admin-9 from the acting subject on the context", payload.ActorUserID)
	}
	if published[0].TenantID != pkgcore.TenantID("tenant-a") {
		t.Fatalf("event tenant = %q, want tenant-a", published[0].TenantID)
	}
}

func TestService_AssignRole_WithNoActingSubject_LeavesTheActorEmpty(t *testing.T) {
	// The actor is best-effort: rbac takes no actor parameter and must not
	// invent one.
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	rec := recordEvents(reg)

	if err := svc.AssignRole(ctx, Subject{TenantID: "tenant-a", UserID: "user-1"}, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	payload := rec.ofType(EventRoleBindingAssigned)[0].Payload.(RoleBindingChangedEvent)
	if payload.ActorUserID != "" {
		t.Fatalf("actor = %q, want empty when no acting subject was on the context", payload.ActorUserID)
	}
}

func TestService_AssignRole_IsIdempotent(t *testing.T) {
	// Assignment widens. "It is already there" fully satisfies the
	// caller's intent, and a retry after a timeout must not fail on the
	// unique index.
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	rec := recordEvents(reg)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	for i := 0; i < 3; i++ {
		if err := svc.AssignRole(ctx, sub, "reader", Scope{}); err != nil {
			t.Fatalf("AssignRole call %d: %v", i+1, err)
		}
	}

	bindings, err := svc.bindings.ByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("three assignments produced %d bindings, want 1", len(bindings))
	}
	// And the repeats are silent on the bus: a no-op must not make every
	// replica flush its cache.
	if got := len(rec.ofType(EventRoleBindingAssigned)); got != 1 {
		t.Fatalf("published %d assigned events, want 1", got)
	}
}

func TestService_AssignRole_DifferentScopes_AreDifferentGrants(t *testing.T) {
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	for _, scope := range []Scope{{}, {NodeID: "node-7"}, {NodeID: "node-8"}} {
		if err := svc.AssignRole(ctx, sub, "reader", scope); err != nil {
			t.Fatalf("AssignRole(%+v): %v", scope, err)
		}
	}
	bindings, err := svc.bindings.ByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(bindings) != 3 {
		t.Fatalf("stored %d bindings, want 3 (one per scope)", len(bindings))
	}
}

func TestService_AssignRole_UnknownRole_IsRejected(t *testing.T) {
	svc := newTestService(t)
	err := svc.AssignRole(tenantCtx("tenant-a"), Subject{TenantID: "tenant-a", UserID: "user-1"}, "ghost", Scope{})
	if !hasCode(err, ErrRoleNotFound.Code) {
		t.Fatalf("error = %v, want %s", err, ErrRoleNotFound.Code)
	}
}

func TestService_AssignRole_AnotherTenantsRole_IsNotFound(t *testing.T) {
	// The role key exists -- in another tenant. Granting it here would
	// cross the tenant boundary; reporting anything but "not found" would
	// leak that the key exists.
	svc := newTestService(t)
	if _, err := svc.DefineRole(tenantCtx("tenant-a"), RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}

	err := svc.AssignRole(tenantCtx("tenant-b"), Subject{TenantID: "tenant-b", UserID: "user-1"}, "reader", Scope{})
	if !hasCode(err, ErrRoleNotFound.Code) {
		t.Fatalf("error = %v, want %s", err, ErrRoleNotFound.Code)
	}
}

func TestService_AssignRole_IncompleteSubject_IsRejected(t *testing.T) {
	svc := newTestService(t)
	err := svc.AssignRole(tenantCtx("tenant-a"), Subject{TenantID: "tenant-a"}, "reader", Scope{})
	if !hasCode(err, ErrSubjectRequired.Code) {
		t.Fatalf("error = %v, want %s", err, ErrSubjectRequired.Code)
	}
}

func TestService_RevokeRole_RemovesTheBindingAndAnnouncesIt(t *testing.T) {
	svc, reg := newTestServiceWithRegistry(t)
	ctx := tenantCtx("tenant-a")
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")
	rec := recordEvents(reg)

	if err := svc.RevokeRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}

	bindings, err := svc.bindings.ByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ByUser: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("%d bindings survived the revoke", len(bindings))
	}
	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 1 {
		t.Fatalf("published %d revoked events, want 1", got)
	}
}

func TestService_RevokeRole_NothingToRevoke_IsReported(t *testing.T) {
	// Revocation is strict where assignment is idempotent. Succeeding
	// quietly would tell an administrator that access was withdrawn when
	// it was not.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	if err := svc.RevokeRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("first RevokeRole: %v", err)
	}
	err := svc.RevokeRole(ctx, sub, "reader", Scope{})
	if !hasCode(err, ErrBindingNotFound.Code) {
		t.Fatalf("second RevokeRole error = %v, want %s", err, ErrBindingNotFound.Code)
	}
}

func TestService_RevokeRole_WrongScope_DoesNotSilentlySucceed(t *testing.T) {
	// THE failure this strictness exists for. The grant is node-scoped;
	// the administrator revokes tenant-wide. If that reported success, the
	// user would keep the access everyone believed was withdrawn.
	svc := newTestService(t)
	ctx := tenantCtx("tenant-a")
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")

	err := svc.RevokeRole(ctx, sub, "region-reader", Scope{})
	if !hasCode(err, ErrBindingNotFound.Code) {
		t.Fatalf("error = %v, want %s", err, ErrBindingNotFound.Code)
	}

	// And the grant is untouched -- a mismatched revoke must not delete
	// the neighbouring binding as a consolation prize.
	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("Can after the mismatched revoke = %v, %v; want the grant intact", ok, err)
	}
}

func TestService_RevokeRole_AnotherTenantsBinding_IsNotFound(t *testing.T) {
	svc := newTestService(t)
	inA := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, inA, "reader", Scope{}, "notes:read")
	// The same role key in tenant-b, so the lookup gets past the role and
	// reaches the binding: the binding is what must not be found.
	if _, err := svc.DefineRole(tenantCtx("tenant-b"), RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole in tenant-b: %v", err)
	}

	inB := Subject{TenantID: "tenant-b", UserID: "user-1"}
	if err := svc.RevokeRole(tenantCtx("tenant-b"), inB, "reader", Scope{}); !hasCode(err, ErrBindingNotFound.Code) {
		t.Fatalf("error = %v, want %s", err, ErrBindingNotFound.Code)
	}
	// tenant-a's grant is untouched.
	if ok, err := svc.Can(context.Background(), inA, "read", "notes"); err != nil || !ok {
		t.Fatalf("tenant-a's grant = %v, %v; want it intact", ok, err)
	}
}

func TestService_RevokeRole_IncompleteSubject_IsRejected(t *testing.T) {
	svc := newTestService(t)
	err := svc.RevokeRole(tenantCtx("tenant-a"), Subject{UserID: "user-1"}, "reader", Scope{})
	if !hasCode(err, ErrSubjectRequired.Code) {
		t.Fatalf("error = %v, want %s", err, ErrSubjectRequired.Code)
	}
}
