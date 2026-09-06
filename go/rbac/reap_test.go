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

// deletedNode is the shape org's own NodeDeleted payload takes on the wire:
// a struct whose JSON tags are the snake_case field names a Redis delivery
// decodes to. These tests must not import org -- exactly like
// removedMember's own doc comment explains for the member-removed event --
// so org's real struct is re-declared here, tags and all, and the
// subscriber under test is fed it as data. Only the field this reap reads
// is reproduced; org's RemovedCount/Path/Cascade fields carry nothing this
// probe looks at.
type deletedNode struct {
	NodeID         string   `json:"node_id"`
	DeletedNodeIds []string `json:"deleted_node_ids"`
}

// publishNodeDeleted delivers an org.node.deleted event the way org's own
// bus.Publish would after a delete committed: on the registry's bus, under
// the tenant the delete happened in. The in-memory bus runs every
// subscriber synchronously inside Publish, so when this helper returns,
// every reap the event triggers has already happened.
func publishNodeDeleted(t *testing.T, reg *pkgcore.Registry, tenant pkgcore.TenantID, payload any) {
	t.Helper()
	bus := reg.Events.Bus()
	if bus == nil {
		t.Fatal("publishNodeDeleted: the registry carries no event bus")
	}
	if err := bus.Publish(pkgcore.WithTenant(context.Background(), tenant), pkgcore.Event{
		Type:     eventNodeDeleted,
		TenantID: tenant,
		Payload:  payload,
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventNodeDeleted, err)
	}
}

func TestService_OnNodeDeleted_ReapsBindingsScopedToDeletedNodesAndSparesEveryoneElse(t *testing.T) {
	// The reap must be precise to the node ids the event names: bindings at
	// the deleted node go, while a binding for the same user at a different,
	// live node, a tenant-wide binding for the same user, the same node id
	// in another tenant, and another user entirely all keep their grants.
	// A reap that spilled across any of those boundaries would withdraw
	// access it had no authority to withdraw.
	svc, reg := newTestServiceWithRegistry(t)

	atDeletedNode := Subject{TenantID: "tenant-a", UserID: "user-1"}
	sameUserOtherNode := Subject{TenantID: "tenant-a", UserID: "user-1"}
	sameUserTenantWide := Subject{TenantID: "tenant-a", UserID: "user-1"}
	otherUser := Subject{TenantID: "tenant-a", UserID: "user-2"}
	sameNodeOtherTenant := Subject{TenantID: "tenant-b", UserID: "user-3"}

	grant(t, svc, atDeletedNode, "writer", Scope{NodeID: "node-deleted"}, "notes:write")
	grant(t, svc, sameUserOtherNode, "reader", Scope{NodeID: "node-live"}, "notes:read")
	grant(t, svc, sameUserTenantWide, "reader", Scope{}, "notes:read")
	grant(t, svc, otherUser, "writer", Scope{NodeID: "node-deleted"}, "notes:write")
	grant(t, svc, sameNodeOtherTenant, "writer", Scope{NodeID: "node-deleted"}, "notes:write")

	ctx := context.Background()
	if ok, _ := svc.Can(ctx, atDeletedNode, "write", "notes"); !ok {
		t.Fatal("the deleted-node binding was not live before the delete")
	}

	rec := recordEvents(reg)
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{
		NodeID:         "node-deleted",
		DeletedNodeIds: []string{"node-deleted"},
	})

	if ok, err := svc.Can(ctx, atDeletedNode, "write", "notes"); err != nil || ok {
		t.Fatalf("Can(notes:write) at the deleted node after the delete = %v, %v; want false", ok, err)
	}
	if ok, err := svc.Can(ctx, sameUserOtherNode, "read", "notes"); err != nil || !ok {
		t.Fatalf("the same user's binding at a live node was reaped: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(ctx, otherUser, "write", "notes"); err != nil || ok {
		t.Fatalf("another user's binding at the SAME deleted node survived: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(ctx, sameNodeOtherTenant, "write", "notes"); err != nil || !ok {
		t.Fatalf("the identical node id in another tenant was reaped: Can = %v, %v", ok, err)
	}

	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 2 {
		t.Fatalf("got %d %s events, want 2 (one per binding scoped to the deleted node)", len(revoked), EventRoleBindingRevoked)
	}
	for _, evt := range revoked {
		if evt.TenantID != "tenant-a" {
			t.Errorf("revoked event carries tenant %q, want tenant-a", evt.TenantID)
		}
		p, ok := evt.Payload.(RoleBindingChangedEvent)
		if !ok {
			t.Fatalf("revoked event payload = %T, want RoleBindingChangedEvent", evt.Payload)
		}
		if p.NodeID != "node-deleted" {
			t.Errorf("revoked event names node %q, want node-deleted", p.NodeID)
		}
	}

	rows, err := svc.bindings.ByNodes(tenantCtx("tenant-a"), []string{"node-deleted"})
	if err != nil {
		t.Fatalf("listing remaining bindings at the deleted node: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d live bindings still scoped to the deleted node", len(rows))
	}
}

func TestService_OnNodeDeleted_CascadeReapsEveryBindingInOnePass(t *testing.T) {
	// A cascade delete's event carries every row the cascade removed, not
	// just the root; the reap must walk all of them in one enumeration and
	// one revoke loop, never one handler invocation per node.
	svc, reg := newTestServiceWithRegistry(t)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{NodeID: "root"}, "notes:read")
	grant(t, svc, sub, "writer", Scope{NodeID: "child-a"}, "notes:write")
	// A second, distinct user bound at the third removed node, so the
	// assertion below genuinely proves the reap is not merely scoped to one
	// user -- it walks every binding at every id the event named.
	other := Subject{TenantID: "tenant-a", UserID: "user-2"}
	grant(t, svc, other, "writer", Scope{NodeID: "child-b"}, "notes:write")
	// A sibling node outside the cascade, which must survive untouched.
	grant(t, svc, sub, "reader", Scope{NodeID: "sibling"}, "notes:read")

	rec := recordEvents(reg)
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{
		NodeID:         "root",
		DeletedNodeIds: []string{"root", "child-a", "child-b"},
	})

	// other holds exactly one binding, at child-b alone, so Can is an
	// unambiguous check for it. sub, by contrast, keeps a live binding at
	// the untouched sibling node carrying the identical notes:read
	// permission the reaped root binding also carried, so Can's own
	// tenant-wide aggregation (it does not evaluate per node) would report
	// "still granted" whether or not the root binding was actually reaped
	// -- the ByNodes check below is what actually proves sub's cascaded
	// bindings are gone, precisely because it is scoped by node.
	if ok, err := svc.Can(context.Background(), other, "write", "notes"); err != nil || ok {
		t.Fatalf("other still holds notes:write after the cascade reap: %v, %v", ok, err)
	}

	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 3 {
		t.Fatalf("got %d %s events, want 3 (one per binding across the cascaded nodes)", len(revoked), EventRoleBindingRevoked)
	}

	rows, err := svc.bindings.ByNodes(tenantCtx("tenant-a"), []string{"root", "child-a", "child-b", "sibling"})
	if err != nil {
		t.Fatalf("listing remaining bindings: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != "sibling" {
		t.Fatalf("remaining bindings = %+v, want exactly the sibling binding", rows)
	}
}

func TestService_OnNodeDeleted_ForeignPayloadsAreDroppedWithoutError(t *testing.T) {
	// The handler runs inside org's own Publish call on the in-memory bus;
	// an error here would make a committed node delete report failure.
	// Every payload rbac cannot read must therefore be dropped, and a
	// payload arriving without a tenant must leave the tenant's bindings
	// untouched.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{NodeID: "node-1"}, "notes:read")

	rec := recordEvents(reg)

	handler := svc.onNodeDeleted
	base := pkgcore.Event{Type: eventNodeDeleted}
	for _, payload := range []any{
		nil,
		"not a payload",
		42,
		map[string]any{"node_id": "node-1"}, // no deleted-ids field at all
		map[string]any{"deleted_node_ids": []any{}},         // empty list
		map[string]any{"deleted_node_ids": "node-1"},        // wrong type (not a list)
		map[string]any{"deleted_node_ids": []any{42}},       // a list of the wrong element type
		map[string]any{"DELETED_NODE_IDS": []any{"node-1"}}, // a key no accepted spelling matches
	} {
		evt := base
		evt.TenantID = "tenant-a"
		evt.Payload = payload
		if err := handler(context.Background(), evt); err != nil {
			t.Fatalf("onNodeDeleted with payload %v: %v", payload, err)
		}
	}

	// A well-formed id list with no tenant: the tenant-less branch, skipped
	// at Debug -- there is no tenant whose bindings could be reaped.
	if err := handler(context.Background(), pkgcore.Event{
		Type: eventNodeDeleted, Payload: deletedNode{DeletedNodeIds: []string{"node-1"}},
	}); err != nil {
		t.Fatalf("onNodeDeleted with a tenant-less event: %v", err)
	}

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("a dropped payload reaped a live binding: Can = %v, %v", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRevoked)); got != 0 {
		t.Fatalf("dropped payloads published %d revoke events", got)
	}

	// And the delivery itself must not fail either: a foreign payload
	// published through the real bus surfaces no error to the publisher.
	publishNodeDeleted(t, reg, sub.TenantID, "foreign payload")
	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("a foreign payload published on the bus reaped a live binding: Can = %v, %v", ok, err)
	}
}

func TestService_OnNodeDeleted_WireShapesAllReapTheSameBindings(t *testing.T) {
	// org's payload reaches rbac in three shapes: the same-process struct
	// with snake_case JSON tags, the map a Redis delivery decodes to, and --
	// for a payload struct published without tags -- the Go field-name
	// spelling. All three must reap, because the bus implementation is the
	// host's choice, not org's or rbac's.
	svc, reg := newTestServiceWithRegistry(t)

	// unit-level: the probe reads every accepted spelling.
	structIDs, ok := nodeDeletedIDsFromPayload(deletedNode{DeletedNodeIds: []string{"n-1"}})
	if !ok || len(structIDs) != 1 || structIDs[0] != "n-1" {
		t.Fatalf("json-tagged struct payload probed as (%v, %v), want ([n-1], true)", structIDs, ok)
	}
	wireIDs, ok := nodeDeletedIDsFromPayload(map[string]any{"deleted_node_ids": []any{"n-2"}})
	if !ok || len(wireIDs) != 1 || wireIDs[0] != "n-2" {
		t.Fatalf("wire map payload probed as (%v, %v), want ([n-2], true)", wireIDs, ok)
	}
	untaggedIDs, ok := nodeDeletedIDsFromPayload(struct{ DeletedNodeIds []string }{DeletedNodeIds: []string{"n-3"}})
	if !ok || len(untaggedIDs) != 1 || untaggedIDs[0] != "n-3" {
		t.Fatalf("untagged struct payload probed as (%v, %v), want ([n-3], true)", untaggedIDs, ok)
	}

	// end to end: each shape published on the bus reaps the node it names.
	byShape := []struct {
		nodeID  string
		payload any
	}{
		{"n-1", deletedNode{NodeID: "n-1", DeletedNodeIds: []string{"n-1"}}},
		{"n-2", map[string]any{"deleted_node_ids": []any{"n-2"}}},
		{"n-3", struct{ DeletedNodeIds []string }{DeletedNodeIds: []string{"n-3"}}},
	}
	for _, shape := range byShape {
		sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
		grant(t, svc, sub, "reader", Scope{NodeID: shape.nodeID}, "notes:read")
		publishNodeDeleted(t, reg, sub.TenantID, shape.payload)
		rows, err := svc.bindings.ByNodes(tenantCtx("tenant-a"), []string{shape.nodeID})
		if err != nil {
			t.Fatalf("listing bindings at %s: %v", shape.nodeID, err)
		}
		if len(rows) != 0 {
			t.Fatalf("node %s (payload %T) still holds %d live bindings after the delete", shape.nodeID, shape.payload, len(rows))
		}
	}
}

func TestService_OnNodeDeleted_OneFailedReapDoesNotAbortTheRest(t *testing.T) {
	// The reaper cannot abort on a failure -- a cascade that reaped one of
	// two bindings has done real work, and surfacing an error would make
	// org's committed delete look failed. Here two nodes were deleted, and
	// the role behind one node's binding is gone (its row was physically
	// deleted behind the scenes, which nothing in this module prevents a
	// future delete path from doing). Resolving that binding's role fails;
	// the other binding must still be reaped, and the handler must still
	// return nil.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}

	ctx := tenantCtx(sub.TenantID)
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole(reader): %v", err)
	}
	doomed, err := svc.DefineRole(ctx, RoleDefinition{Key: "doomed", Permissions: []string{"notes:write"}})
	if err != nil {
		t.Fatalf("DefineRole(doomed): %v", err)
	}
	if err = svc.AssignRole(ctx, sub, "reader", Scope{NodeID: "node-ok"}); err != nil {
		t.Fatalf("AssignRole(reader): %v", err)
	}
	if err = svc.AssignRole(ctx, sub, "doomed", Scope{NodeID: "node-doomed"}); err != nil {
		t.Fatalf("AssignRole(doomed): %v", err)
	}
	// Remove the role row the second binding names, leaving the binding a
	// dangling reference.
	if err = svc.roles.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("deleting the role behind the second binding: %v", err)
	}

	rec := recordEvents(reg)
	publishNodeDeleted(t, reg, sub.TenantID, deletedNode{
		DeletedNodeIds: []string{"node-ok", "node-doomed"},
	})

	rows, err := svc.bindings.ByNodes(ctx, []string{"node-ok", "node-doomed"})
	if err != nil {
		t.Fatalf("listing remaining bindings: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != "node-doomed" {
		t.Fatalf("remaining bindings = %+v, want exactly the unresolvable one at node-doomed", rows)
	}
	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 1 {
		t.Fatalf("got %d %s events, want 1 (only the resolvable binding)", len(revoked), EventRoleBindingRevoked)
	}
	if p := revoked[0].Payload.(RoleBindingChangedEvent); p.NodeID != "node-ok" {
		t.Errorf("revoked event names node %q, want node-ok", p.NodeID)
	}
}

func TestService_OnNodeDeleted_ReapOnOneReplica_ConvergesTheOther(t *testing.T) {
	// Two Services over one database and one bus: the shape of a
	// multi-replica deployment. The reaper runs on the replica that hears
	// the delete, and its revokes travel to the other replica through
	// EventRoleBindingRevoked -- replica B, which cached the grant and does
	// not run the reap itself, must stop answering "granted" on the very
	// next call, not one TTL later.
	db := newRBACTestDB(t)
	reg := newPlainRegistry()
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring permissions: %v", err)
	}
	replicaA := attachReplica(t, db, reg)
	replicaB := attachReplica(t, db, reg)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, replicaA, sub, "reader", Scope{NodeID: "node-1"}, "notes:read")
	if ok, _ := replicaB.Can(context.Background(), sub, "read", "notes"); !ok {
		t.Fatal("replica B did not see the grant")
	}

	publishNodeDeleted(t, reg, sub.TenantID, deletedNode{DeletedNodeIds: []string{"node-1"}})

	ok, err := replicaB.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can on replica B: %v", err)
	}
	if ok {
		t.Fatal("replica B still grants a permission the node delete reaped on replica A")
	}
}
