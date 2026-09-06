package rbac

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// removedMember is the shape org's own MemberRemoved payload takes on the
// wire: a struct whose JSON tags are the snake_case field names a Redis
// delivery decodes to. These tests must not import org -- exactly like rbac
// itself -- so org's real struct is re-declared here, tags and all, and the
// subscriber under test is fed it as data.
type removedMember struct {
	MembershipID string `json:"membership_id"`
	UserID       string `json:"user_id"`
	NodeID       string `json:"node_id"`
}

// publishMemberRemoved delivers an org.member.removed event the way org's
// own bus.Publish would after a membership delete committed: on the
// registry's bus, under the tenant the removal happened in. The in-memory
// bus runs every subscriber synchronously inside Publish, so when this
// helper returns, every reap the event triggers has already happened.
func publishMemberRemoved(t *testing.T, reg *pkgcore.Registry, tenant pkgcore.TenantID, payload any) {
	t.Helper()
	bus := reg.Events.Bus()
	if bus == nil {
		t.Fatal("publishMemberRemoved: the registry carries no event bus")
	}
	if err := bus.Publish(pkgcore.WithTenant(context.Background(), tenant), pkgcore.Event{
		Type:     eventMemberRemoved,
		TenantID: tenant,
		Payload:  payload,
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventMemberRemoved, err)
	}
}

func TestService_OnMemberRemoved_ReapsTheRemovedMembersBindingsAndSparesEveryoneElse(t *testing.T) {
	// The reap must be precise to the one (tenant, user) the event names:
	// the removed member's bindings at every scope go, while another user
	// in the same tenant, the same user id in another tenant, and the same
	// user id in the system domain all keep their grants untouched. A reap
	// that spilled across any of those boundaries would withdraw access it
	// had no authority to withdraw.
	svc, reg := newTestServiceWithRegistry(t)

	removed := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	kept := Subject{TenantID: "tenant-a", UserID: "user-kept"}
	sameUserOtherTenant := Subject{TenantID: "tenant-b", UserID: "user-gone"}
	sameUserSystemDomain := Subject{TenantID: SystemDomain, UserID: "user-gone"}

	// Two scopes for the removed member -- tenant-wide plus one subtree --
	// so the reap demonstrably walks bindings, not just "the user's row".
	grant(t, svc, removed, "reader", Scope{}, "notes:read")
	grant(t, svc, removed, "writer", Scope{NodeID: "node-1"}, "notes:write")
	grant(t, svc, kept, "reader", Scope{}, "notes:read")
	grant(t, svc, sameUserOtherTenant, "reader", Scope{}, "notes:read")
	grant(t, svc, sameUserSystemDomain, "reader", Scope{}, "notes:read")

	ctx := context.Background()
	for _, sub := range []Subject{removed, kept, sameUserOtherTenant, sameUserSystemDomain} {
		if ok, _ := svc.Can(ctx, sub, "read", "notes"); !ok {
			t.Fatalf("%v did not see its grant before the removal", sub)
		}
	}

	rec := recordEvents(reg)
	publishMemberRemoved(t, reg, "tenant-a", removedMember{
		MembershipID: "membership-1",
		UserID:       "user-gone",
		NodeID:       "node-1",
	})

	if ok, err := svc.Can(ctx, removed, "read", "notes"); err != nil || ok {
		t.Fatalf("removed member Can(notes:read) after the removal = %v, %v; want false", ok, err)
	}
	if ok, err := svc.Can(ctx, removed, "write", "notes"); err != nil || ok {
		t.Fatalf("removed member Can(notes:write) after the removal = %v, %v; want false", ok, err)
	}
	if ok, err := svc.Can(ctx, kept, "read", "notes"); err != nil || !ok {
		t.Fatalf("another user in the same tenant lost its grant: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(ctx, sameUserOtherTenant, "read", "notes"); err != nil || !ok {
		t.Fatalf("the same user id in another tenant lost its grant: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(ctx, sameUserSystemDomain, "read", "notes"); err != nil || !ok {
		t.Fatalf("the same user id in the system domain lost its grant: Can = %v, %v", ok, err)
	}

	// Every reaped binding must have travelled the full RevokeRole path:
	// one EventRoleBindingRevoked per binding, each naming the tenant and
	// user the event named and the exact scope the binding was revoked at,
	// so replicas converge and the audit trail records what was withdrawn.
	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 2 {
		t.Fatalf("got %d %s events, want 2 (one per reaped binding)", len(revoked), EventRoleBindingRevoked)
	}
	byRole := map[string]string{}
	for _, evt := range revoked {
		if evt.TenantID != "tenant-a" {
			t.Errorf("revoked event carries tenant %q, want tenant-a", evt.TenantID)
		}
		p, ok := evt.Payload.(RoleBindingChangedEvent)
		if !ok {
			t.Fatalf("revoked event payload = %T, want RoleBindingChangedEvent", evt.Payload)
		}
		if p.UserID != "user-gone" || p.TenantID != "tenant-a" {
			t.Errorf("revoked event names (%q, %q), want (tenant-a, user-gone)", p.TenantID, p.UserID)
		}
		byRole[p.RoleKey] = p.NodeID
	}
	if byRole["reader"] != "" {
		t.Errorf("the reader binding was revoked at node %q, want tenant-wide (empty)", byRole["reader"])
	}
	if byRole["writer"] != "node-1" {
		t.Errorf("the writer binding was revoked at node %q, want node-1", byRole["writer"])
	}

	rows, err := svc.bindings.ByUser(tenantCtx("tenant-a"), "user-gone")
	if err != nil {
		t.Fatalf("listing the removed member's remaining bindings: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the removed member still holds %d live bindings", len(rows))
	}
}

func TestService_OnMemberRemoved_RestoreInterleavingKeepsTheReapSoftAndIdempotent(t *testing.T) {
	// The restore-interleaving shape: an administrator revokes a grant and
	// restores it while a member removal is in flight, and the removal
	// lands AFTER the restore. The reap must withdraw the restored (live)
	// binding, and -- because it goes through RevokeRole's mark-delete --
	// that withdrawal must itself be restorable, exactly as if the
	// administrator had revoked it. A second delivery of the same event
	// must then find nothing live left and reap nothing: the handler is
	// idempotent under at-least-once redelivery.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	ctx := tenantCtx(sub.TenantID)
	if err := svc.RevokeRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("manual revoke before the removal: %v", err)
	}
	if err := svc.RestoreRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("manual restore before the removal: %v", err)
	}

	rec := recordEvents(reg)
	member := removedMember{MembershipID: "membership-1", UserID: sub.UserID}

	if ok, _ := svc.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("the restored grant was not live when the removal landed")
	}
	publishMemberRemoved(t, reg, sub.TenantID, member)
	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || ok {
		t.Fatalf("Can after the removal = %v, %v; want false (the restored binding must be reaped too)", ok, err)
	}

	// The reap mark-deleted rather than physically deleted: a restore
	// undoes it, which a physical delete could never offer.
	if err := svc.RestoreRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("restore after the reap: %v", err)
	}
	if ok, _ := svc.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("the grant did not come back after restoring the reaped binding")
	}

	// Redelivery: the same event arrives again, as an at-least-once broker
	// will deliver it. The live binding is reaped again; nothing else
	// happens and nothing errors.
	publishMemberRemoved(t, reg, sub.TenantID, member)
	if ok, _ := svc.Can(context.Background(), sub, "read", "notes"); ok {
		t.Fatal("Can stayed true after the redelivered removal reaped the restored binding")
	}
	if err := svc.RestoreRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("second restore: %v", err)
	}

	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (the manual revoke predates the recorder; the two reaps)", got, EventRoleBindingRevoked)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 2 {
		t.Fatalf("got %d %s events, want 2 (one per restore)", got, EventRoleBindingRestored)
	}
}

func TestService_OnMemberRemoved_ForeignPayloadsAreDroppedWithoutError(t *testing.T) {
	// The handler runs inside org's own Publish call on the in-memory bus;
	// an error here would make a committed member removal report failure.
	// Every payload rbac cannot read must therefore be dropped, and a
	// payload arriving without a tenant must leave the tenant's bindings
	// untouched. A live binding before each call and after the whole gaunt
	// let proves none of the drops reaped anything.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-kept"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	// The recorder predates every delivery below, so the final zero-count
	// assertion really proves none of the dropped payloads reaped anything
	// -- the in-memory bus does not replay past events to late subscribers.
	rec := recordEvents(reg)

	handler := svc.onMemberRemoved
	base := pkgcore.Event{Type: eventMemberRemoved}
	for _, payload := range []any{
		nil,
		"not a payload",
		42,
		map[string]any{"membership_id": "m-1", "node_id": "n-1"}, // no user id at all
		map[string]any{"user_id": ""},                            // empty user id
		map[string]any{"user_id": 42},                            // user id of the wrong type
		map[string]any{"USER_ID": "user-gone"},                   // a key no accepted spelling matches
	} {
		evt := base
		evt.TenantID = "tenant-a"
		evt.Payload = payload
		if err := handler(context.Background(), evt); err != nil {
			t.Fatalf("onMemberRemoved with payload %v: %v", payload, err)
		}
	}

	// A well-formed user id with no tenant: the tenant-less branch, skipped
	// at Debug -- there is no tenant whose bindings could be reaped.
	if err := handler(context.Background(), pkgcore.Event{
		Type: eventMemberRemoved, Payload: removedMember{UserID: "user-kept"},
	}); err != nil {
		t.Fatalf("onMemberRemoved with a tenant-less event: %v", err)
	}

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("a dropped payload reaped a live binding: Can = %v, %v", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 0 {
		t.Fatalf("dropped payloads published %d revoke events", got)
	}

	// And the delivery itself must not fail either: a foreign payload
	// published through the real bus surfaces no error to the publisher.
	publishMemberRemoved(t, reg, sub.TenantID, "foreign payload")
	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("a foreign payload published on the bus reaped a live binding: Can = %v, %v", ok, err)
	}
}

func TestService_OnMemberRemoved_WireShapesAllReapTheSameBindings(t *testing.T) {
	// org's payload reaches rbac in three shapes: the same-process
	// struct with snake_case JSON tags, the map a Redis delivery decodes
	// to, and -- for a payload struct published without tags -- the Go
	// field-name spelling. All three must reap, because the bus
	// implementation is the host's choice, not org's or rbac's.
	svc, reg := newTestServiceWithRegistry(t)

	// unit-level: the probe reads every accepted spelling.
	structUser, ok := memberRemovedUserIDFromPayload(removedMember{UserID: "u-1"})
	if !ok || structUser != "u-1" {
		t.Fatalf("json-tagged struct payload probed as (%q, %v), want (u-1, true)", structUser, ok)
	}
	wireUser, ok := memberRemovedUserIDFromPayload(map[string]any{"user_id": "u-2"})
	if !ok || wireUser != "u-2" {
		t.Fatalf("wire map payload probed as (%q, %v), want (u-2, true)", wireUser, ok)
	}
	untaggedUser, ok := memberRemovedUserIDFromPayload(struct{ UserID string }{UserID: "u-3"})
	if !ok || untaggedUser != "u-3" {
		t.Fatalf("untagged struct payload probed as (%q, %v), want (u-3, true)", untaggedUser, ok)
	}

	// end to end: each shape published on the bus reaps the member it names.
	byShape := []struct {
		userID  string
		payload any
	}{
		{"u-1", removedMember{MembershipID: "m-1", UserID: "u-1", NodeID: "n-1"}},
		{"u-2", map[string]any{"user_id": "u-2", "node_id": "n-1"}},
		{"u-3", struct{ UserID string }{UserID: "u-3"}},
	}
	ctx := context.Background()
	for _, shape := range byShape {
		sub := Subject{TenantID: "tenant-a", UserID: shape.userID}
		grant(t, svc, sub, "reader", Scope{}, "notes:read")
		publishMemberRemoved(t, reg, sub.TenantID, shape.payload)
		if ok, err := svc.Can(ctx, sub, "read", "notes"); err != nil || ok {
			t.Fatalf("user %s (payload %T) still granted after the removal: Can = %v, %v", shape.userID, shape.payload, ok, err)
		}
	}
}

func TestService_OnMemberRemoved_OneFailedReapDoesNotAbortTheRest(t *testing.T) {
	// The reaper cannot abort on a failure -- a removal that reaped nine of
	// ten bindings has done real work, and surfacing an error would make
	// org's committed removal look failed. Here the removed member holds
	// two bindings, and the role behind one of them is gone (its row was
	// physically deleted behind the scenes, which nothing in this module
	// prevents a future delete path from doing). Resolving that binding's
	// role fails; the other binding must still be reaped, and the handler
	// must still return nil.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-gone"}

	ctx := tenantCtx(sub.TenantID)
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole(reader): %v", err)
	}
	doomed, err := svc.DefineRole(ctx, RoleDefinition{Key: "doomed", Permissions: []string{"notes:write"}})
	if err != nil {
		t.Fatalf("DefineRole(doomed): %v", err)
	}
	if err = svc.AssignRole(ctx, sub, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole(reader): %v", err)
	}
	if err = svc.AssignRole(ctx, sub, "doomed", Scope{}); err != nil {
		t.Fatalf("AssignRole(doomed): %v", err)
	}
	// Remove the role row the second binding names, leaving the binding a
	// dangling reference. Role rows have no delete API in this module, but
	// the repository's physical Delete works, which is all the anomaly
	// needs to exist.
	if err = svc.roles.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("deleting the role behind the second binding: %v", err)
	}

	rec := recordEvents(reg)
	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil || ok {
		t.Fatalf("the binding whose role resolved was not reaped: Can = %v, %v", ok, err)
	}
	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 1 {
		t.Fatalf("got %d %s events, want 1 (only the resolvable binding)", len(revoked), EventRoleBindingRevoked)
	}
	if p := revoked[0].Payload.(RoleBindingChangedEvent); p.RoleKey != "reader" {
		t.Errorf("revoked event names role %q, want reader", p.RoleKey)
	}
	rows, err := svc.bindings.ByUser(ctx, sub.UserID)
	if err != nil {
		t.Fatalf("listing the remaining bindings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d remaining live bindings, want 1 (the unresolvable one)", len(rows))
	}
}

func TestService_OnMemberRemoved_ReapOnOneReplica_ConvergesTheOther(t *testing.T) {
	// Two Services over one database and one bus: the shape of a
	// multi-replica deployment. The reaper runs on the replica that hears
	// the removal, and its revokes travel to the other replica through
	// EventRoleBindingRevoked -- replica B, which cached the grant and does
	// not run the reap itself (nothing is left to reap by the time the
	// event reaches it), must stop answering "granted" on the very next
	// call, not one TTL later.
	db := newRBACTestDB(t)
	reg := newPlainRegistry()
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring permissions: %v", err)
	}
	replicaA := attachReplica(t, db, reg)
	replicaB := attachReplica(t, db, reg)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, replicaA, sub, "reader", Scope{}, "notes:read")
	if ok, _ := replicaB.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("replica B did not see the grant")
	}

	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})

	ok, err := replicaB.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can on replica B: %v", err)
	}
	if ok {
		t.Fatal("replica B still grants a permission the member removal reaped on replica A")
	}
}
