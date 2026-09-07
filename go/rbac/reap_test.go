package rbac

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

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
	structUser, ok := memberUserIDFromPayload(removedMember{UserID: "u-1"})
	if !ok || structUser != "u-1" {
		t.Fatalf("json-tagged struct payload probed as (%q, %v), want (u-1, true)", structUser, ok)
	}
	wireUser, ok := memberUserIDFromPayload(map[string]any{"user_id": "u-2"})
	if !ok || wireUser != "u-2" {
		t.Fatalf("wire map payload probed as (%q, %v), want (u-2, true)", wireUser, ok)
	}
	untaggedUser, ok := memberUserIDFromPayload(struct{ UserID string }{UserID: "u-3"})
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

// statementCounter tallies the SQL statements a code path performs against
// rbac's two tables, by registering gorm callbacks on the shared database.
// It exists to prove a performance property deterministically -- how many
// reads and writes a cascade reap performs -- which a wall-clock assertion
// could never do reliably on a machine under load. The reap runs
// synchronously inside the publish, so the counters need no locking.
type statementCounter struct {
	bindingReads  int
	roleReads     int
	bindingWrites int
}

func newStatementCounter(t *testing.T, db *gorm.DB) *statementCounter {
	t.Helper()
	c := &statementCounter{}
	if err := db.Callback().Query().After("gorm:query").Register("rbac_test:count_query", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if strings.Contains(sql, "role_bindings") {
			c.bindingReads++
		}
		if strings.Contains(sql, "roles") {
			c.roleReads++
		}
	}); err != nil {
		t.Fatalf("registering the query counter: %v", err)
	}
	if err := db.Callback().Update().After("gorm:update").Register("rbac_test:count_update", func(tx *gorm.DB) {
		if strings.Contains(tx.Statement.SQL.String(), "role_bindings") {
			c.bindingWrites++
		}
	}); err != nil {
		t.Fatalf("registering the update counter: %v", err)
	}
	return c
}

func (c *statementCounter) reset() { *c = statementCounter{} }

// TestService_OnNodeDeleted_CascadeReap_NoPerBindingReReads pins the
// review finding on the node-deleted reap's cost shape: the reap runs
// synchronously inside org's own delete request (the in-memory bus
// delivers in-process), so a large-subtree cascade would drag that one
// HTTP DELETE through one full revoke round trip per binding -- each
// RevokeRole call re-resolves the role by key and re-finds the binding by
// tuple before its delete, on top of the role id resolution the reap
// already performed. The reap holds the very rows it enumerated, so the
// fix revokes them by id with the role resolved once per DISTINCT role
// per pass. Pinned by counting the statements the reap performs, never by
// timing it: a four-binding cascade over two distinct roles must cost one
// binding enumeration, exactly two role lookups (not eight), four
// mark-delete writes, and no per-binding Find or ByKey at all.
func TestService_OnNodeDeleted_CascadeReap_NoPerBindingReReads(t *testing.T) {
	db := newRBACTestDB(t)
	counts := newStatementCounter(t, db)
	svc, reg := attachTestService(t, db)

	ctx := tenantCtx("tenant-a")
	for _, roleKey := range []string{"reader", "writer"} {
		if _, err := svc.DefineRole(ctx, RoleDefinition{Key: roleKey, Permissions: []string{"notes:read", "notes:write"}}); err != nil {
			t.Fatalf("DefineRole(%s): %v", roleKey, err)
		}
	}
	// Four live bindings across the two roles: two distinct roles and four
	// rows is the shape where the per-binding re-reads the fix removes are
	// distinguishable from the reads that must stay.
	for _, grant := range []struct {
		user   string
		role   string
		nodeID string
	}{
		{"user-1", "reader", "n-1"},
		{"user-1", "reader", "n-2"},
		{"user-1", "writer", "n-3"},
		{"user-2", "writer", "n-4"},
	} {
		if err := svc.AssignRole(ctx, Subject{TenantID: "tenant-a", UserID: grant.user}, grant.role, Scope{NodeID: grant.nodeID}); err != nil {
			t.Fatalf("AssignRole(%+v): %v", grant, err)
		}
	}

	counts.reset()
	rec := recordEvents(reg)
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{
		DeletedNodeIds: []string{"n-1", "n-2", "n-3", "n-4"},
	})

	// The reap must still withdraw every binding -- the batching changes the
	// cost shape, never the outcome.
	revoked := rec.ofType(EventRoleBindingRevoked)
	if len(revoked) != 4 {
		t.Fatalf("got %d %s events, want 4 (one per reaped binding)", len(revoked), EventRoleBindingRevoked)
	}

	if got := counts.bindingReads; got != 1 {
		t.Fatalf("the reap ran %d SELECTs against role_bindings, want 1 (the enumeration); every additional read is a per-binding re-read", got)
	}
	if got := counts.roleReads; got != 2 {
		t.Fatalf("the reap ran %d SELECTs against roles, want 2 (one per distinct role, resolved once per pass)", got)
	}
	if got := counts.bindingWrites; got != 4 {
		t.Fatalf("the reap ran %d UPDATEs against role_bindings, want 4 (one mark-delete per binding)", got)
	}

	// And the rows really are gone (soft-deleted), read after the counts
	// were taken so the verification's own query is not counted.
	rows, err := svc.bindings.ByNodes(ctx, []string{"n-1", "n-2", "n-3", "n-4"})
	if err != nil {
		t.Fatalf("listing remaining bindings: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("%d bindings survived the cascade reap", len(rows))
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

// restoredMember is the shape org's own MemberRestored payload takes on the
// wire: a struct whose JSON tags are the snake_case field names a Redis
// delivery decodes to. These tests must not import org -- exactly like
// removedMember's own doc comment explains for the member-removed event --
// so org's real struct is re-declared here, tags and all, and the
// subscriber under test is fed it as data. Only the field this handler
// reads is reproduced; org's MembershipID/NodeID fields carry nothing the
// user-id probe looks at.
type restoredMember struct {
	MembershipID string `json:"membership_id"`
	UserID       string `json:"user_id"`
	NodeID       string `json:"node_id"`
}

// publishMemberRestored delivers an org.member.restored event the way org's
// own bus.Publish would after a membership restore committed: on the
// registry's bus, under the tenant the restore happened in. The in-memory
// bus runs every subscriber synchronously inside Publish, so when this
// helper returns, every re-instatement the event triggers has already
// happened.
func publishMemberRestored(t *testing.T, reg *pkgcore.Registry, tenant pkgcore.TenantID, payload any) {
	t.Helper()
	bus := reg.Events.Bus()
	if bus == nil {
		t.Fatal("publishMemberRestored: the registry carries no event bus")
	}
	if err := bus.Publish(pkgcore.WithTenant(context.Background(), tenant), pkgcore.Event{
		Type:     eventMemberRestored,
		TenantID: tenant,
		Payload:  payload,
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventMemberRestored, err)
	}
}

func TestService_OnMemberRestored_ReinstatesTheReapedBindingsAndSparesEveryoneElse(t *testing.T) {
	// The re-instatement must be precise to the one (tenant, user) the
	// event names: the restored member's revoked bindings at every scope
	// come back, while the same user id's revoked rows in another tenant
	// and in the system domain -- which the tenant-a removal reap never
	// touched and no tenant-a restore may touch either -- stay revoked. A
	// re-instatement that spilled across either boundary would restore
	// access in a tenant no org event said anything about.
	svc, reg := newTestServiceWithRegistry(t)

	restored := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	sameUserOtherTenant := Subject{TenantID: "tenant-b", UserID: "user-gone"}
	sameUserSystemDomain := Subject{TenantID: SystemDomain, UserID: "user-gone"}

	// Two scopes for the restored member -- tenant-wide plus one subtree --
	// so the re-instatement demonstrably walks bindings, not just one row.
	grant(t, svc, restored, "reader", Scope{}, "notes:read")
	grant(t, svc, restored, "writer", Scope{NodeID: "node-1"}, "notes:write")
	// Revoked rows in the two scopes the removal could never have touched:
	// the same user id with a manually revoked grant in tenant-b, and one
	// in the system domain.
	grant(t, svc, sameUserOtherTenant, "reader", Scope{}, "notes:read")
	if err := svc.RevokeRole(tenantCtx("tenant-b"), sameUserOtherTenant, "reader", Scope{}); err != nil {
		t.Fatalf("revoking the tenant-b grant: %v", err)
	}
	grant(t, svc, sameUserSystemDomain, "reader", Scope{}, "notes:read")
	if err := svc.RevokeRole(tenantCtx(SystemDomain), sameUserSystemDomain, "reader", Scope{}); err != nil {
		t.Fatalf("revoking the system-domain grant: %v", err)
	}

	// The removal itself: reaps the tenant-a member's two live bindings.
	publishMemberRemoved(t, reg, "tenant-a", removedMember{UserID: "user-gone"})
	if ok, _ := svc.Can(context.Background(), restored, "read", "notes"); ok {
		t.Fatal("the member's grant was live after the removal -- the reap did not run")
	}

	rec := recordEvents(reg)
	publishMemberRestored(t, reg, "tenant-a", restoredMember{
		MembershipID: "membership-1",
		UserID:       "user-gone",
		NodeID:       "node-1",
	})

	// The two reaped scopes are live again through the decision path, and
	// the two cross-scope revoked rows are not.
	if ok, err := svc.Can(context.Background(), restored, "read", "notes"); err != nil || !ok {
		t.Fatalf("Can(notes:read) after the restore = %v, %v; want the reaped reader grant back", ok, err)
	}
	if ok, err := svc.Can(context.Background(), restored, "write", "notes"); err != nil || !ok {
		t.Fatalf("Can(notes:write) after the restore = %v, %v; want the reaped writer grant back", ok, err)
	}
	if ok, err := svc.Can(context.Background(), sameUserOtherTenant, "read", "notes"); err != nil || ok {
		t.Fatalf("the tenant-b revoked row was re-instated: Can = %v, %v", ok, err)
	}
	if ok, err := svc.Can(context.Background(), sameUserSystemDomain, "read", "notes"); err != nil || ok {
		t.Fatalf("the system-domain revoked row was re-instated: Can = %v, %v", ok, err)
	}

	// Every re-instated binding must have travelled the full RestoreRole
	// path: one EventRoleBindingRestored per grant, each naming the tenant
	// and user the event named and the exact scope the binding was revoked
	// at, so replicas converge and the audit trail records what came back.
	restoredEvts := rec.ofType(EventRoleBindingRestored)
	if len(restoredEvts) != 2 {
		t.Fatalf("got %d %s events, want 2 (one per re-instated grant)", len(restoredEvts), EventRoleBindingRestored)
	}
	byRole := map[string]string{}
	for _, evt := range restoredEvts {
		if evt.TenantID != "tenant-a" {
			t.Errorf("restored event carries tenant %q, want tenant-a", evt.TenantID)
		}
		p, ok := evt.Payload.(RoleBindingChangedEvent)
		if !ok {
			t.Fatalf("restored event payload = %T, want RoleBindingChangedEvent", evt.Payload)
		}
		if p.UserID != "user-gone" || p.TenantID != "tenant-a" {
			t.Errorf("restored event names (%q, %q), want (tenant-a, user-gone)", p.TenantID, p.UserID)
		}
		byRole[p.RoleKey] = p.NodeID
	}
	if byRole["reader"] != "" {
		t.Errorf("the reader grant was restored at node %q, want tenant-wide (empty)", byRole["reader"])
	}
	if byRole["writer"] != "node-1" {
		t.Errorf("the writer grant was restored at node %q, want node-1", byRole["writer"])
	}

	rows, err := svc.bindings.RevokedByUser(tenantCtx("tenant-a"), "user-gone")
	if err != nil {
		t.Fatalf("listing the restored member's revoked bindings: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("the restored member still has %d revoked bindings in tenant-a", len(rows))
	}
	// And the two cross-scope revoked rows are untouched.
	for _, probe := range []struct {
		tenant pkgcore.TenantID
		user   string
	}{
		{"tenant-b", "user-gone"},
		{SystemDomain, "user-gone"},
	} {
		rows, err := svc.bindings.RevokedByUser(tenantCtx(probe.tenant), probe.user)
		if err != nil {
			t.Fatalf("listing %s's revoked bindings: %v", probe.tenant, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s's revoked rows = %d after the tenant-a restore, want 1 (untouched)", probe.tenant, len(rows))
		}
	}
}

func TestService_OnMemberRestored_RedeliveryRestoresNothingTwice(t *testing.T) {
	// An at-least-once broker can deliver the same restore event twice. The
	// first delivery re-instates every revoked binding; the second finds
	// nothing revoked left to re-instate (RevokedByUser returns only rows
	// the auto-scope would hide) and must write and announce nothing -- a
	// duplicate delivery that re-restored would churn the audit trail.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")
	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})
	if ok, _ := svc.Can(context.Background(), sub, "read", "notes"); ok {
		t.Fatal("the grant survived the removal -- the reap did not run")
	}

	rec := recordEvents(reg)
	publishMemberRestored(t, reg, sub.TenantID, restoredMember{UserID: sub.UserID})
	publishMemberRestored(t, reg, sub.TenantID, restoredMember{UserID: sub.UserID})

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("Can after the restore = %v, %v; want the grant back", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 1 {
		t.Fatalf("two deliveries of one restore event produced %d %s events, want 1", got, EventRoleBindingRestored)
	}
}

func TestService_OnMemberRestored_DeliberateRevocationPredatingTheRemoval_StaysRevoked(t *testing.T) {
	// The P0-rbac-7 regression this round closes, reproduced
	// deterministically in its simplest shape: the owner's grant is
	// deliberately revoked (Service.RevokeRole, the same path go/admin's
	// RoleService.RevokeRole delegates to), THEN the owner is removed from
	// the tenant, THEN the membership is restored. The removal reap finds
	// nothing live to reap -- the deliberate revoke predates it -- and the
	// member restore must NOT bring the owner's grant back: the revoked
	// row was never written by the removal, and undoing it would silently
	// resurrect a deliberate revocation. Before this round the row carried
	// no revoke-origin marker, the restore re-instated every revoked tuple
	// in the restored member's scope, and the owner came back with a grant
	// an administrator had explicitly taken away -- this test failed on
	// that code. The marker (0003_add_revoke_origin.sql) is what lets the
	// restore side tell the deliberate row apart: it carries the empty
	// origin, never the member-removal one, and is left revoked.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	grant(t, svc, sub, "owner", Scope{}, "notes:read")

	ctx := tenantCtx(sub.TenantID)
	if err := svc.RevokeRole(ctx, sub, "owner", Scope{}); err != nil {
		t.Fatalf("manual revoke before the removal: %v", err)
	}
	// The removal finds nothing live to reap -- the manual revoke predates
	// it -- so the revoked row below is provably not a row any reap wrote.
	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})

	rec := recordEvents(reg)
	publishMemberRestored(t, reg, sub.TenantID, restoredMember{UserID: sub.UserID})

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || ok {
		t.Fatalf("Can after the restore = %v, %v; want false (the deliberate revocation must survive the member restore)", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 0 {
		t.Fatalf("got %d %s events, want 0 (nothing the removal reaped exists to restore)", got, EventRoleBindingRestored)
	}
	rows, err := svc.bindings.RevokedByUser(ctx, sub.UserID)
	if err != nil {
		t.Fatalf("listing the restored member's revoked bindings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d revoked rows after the restore, want 1 (the deliberate revocation, untouched)", len(rows))
	}
	if rows[0].RevokeOrigin != revokeOriginDeliberate {
		t.Errorf("the deliberate revocation's revoke_origin = %q, want %q", rows[0].RevokeOrigin, revokeOriginDeliberate)
	}
}

func TestService_OnMemberRestored_ForeignPayloadsAreDroppedWithoutError(t *testing.T) {
	// The handler runs inside org's own Publish call on the in-memory bus;
	// an error here would make a committed member restore report failure.
	// Every payload rbac cannot read must therefore be dropped, and a
	// payload arriving without a tenant must leave the tenant's bindings
	// untouched. A manually revoked row before each call and a still-
	// revoked row after the whole gauntlet proves none of the drops
	// re-instated anything.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-kept"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")
	if err := svc.RevokeRole(tenantCtx("tenant-a"), sub, "reader", Scope{}); err != nil {
		t.Fatalf("revoking the grant the drops must not restore: %v", err)
	}

	// The recorder predates every delivery below, so the final zero-count
	// assertion really proves none of the dropped payloads restored
	// anything -- the in-memory bus does not replay past events to late
	// subscribers.
	rec := recordEvents(reg)

	handler := svc.onMemberRestored
	base := pkgcore.Event{Type: eventMemberRestored}
	for _, payload := range []any{
		nil,
		"not a payload",
		42,
		map[string]any{"membership_id": "m-1", "node_id": "n-1"}, // no user id at all
		map[string]any{"user_id": ""},                            // empty user id
		map[string]any{"user_id": 42},                            // user id of the wrong type
		map[string]any{"USER_ID": "user-kept"},                   // a key no accepted spelling matches
	} {
		evt := base
		evt.TenantID = "tenant-a"
		evt.Payload = payload
		if err := handler(context.Background(), evt); err != nil {
			t.Fatalf("onMemberRestored with payload %v: %v", payload, err)
		}
	}

	// A well-formed user id with no tenant: the tenant-less branch, skipped
	// at Debug -- there is no tenant whose bindings could be re-instated.
	if err := handler(context.Background(), pkgcore.Event{
		Type: eventMemberRestored, Payload: restoredMember{UserID: "user-kept"},
	}); err != nil {
		t.Fatalf("onMemberRestored with a tenant-less event: %v", err)
	}

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || ok {
		t.Fatalf("a dropped payload restored a revoked binding: Can = %v, %v", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 0 {
		t.Fatalf("dropped payloads published %d restore events", got)
	}

	// And the delivery itself must not fail either: a foreign payload
	// published through the real bus surfaces no error to the publisher.
	publishMemberRestored(t, reg, sub.TenantID, "foreign payload")
	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || ok {
		t.Fatalf("a foreign payload published on the bus restored a revoked binding: Can = %v, %v", ok, err)
	}
}

func TestService_OnMemberRestored_WireShapesAllReinstateTheSameBindings(t *testing.T) {
	// org's payload reaches rbac in three shapes: the same-process struct
	// with snake_case JSON tags, the map a Redis delivery decodes to, and --
	// for a payload struct published without tags -- the Go field-name
	// spelling. All three must re-instate, because the bus implementation
	// is the host's choice, not org's or rbac's.
	svc, reg := newTestServiceWithRegistry(t)

	// unit-level: the probe reads every accepted spelling.
	structUser, ok := memberUserIDFromPayload(restoredMember{UserID: "u-1"})
	if !ok || structUser != "u-1" {
		t.Fatalf("json-tagged struct payload probed as (%q, %v), want (u-1, true)", structUser, ok)
	}
	wireUser, ok := memberUserIDFromPayload(map[string]any{"user_id": "u-2"})
	if !ok || wireUser != "u-2" {
		t.Fatalf("wire map payload probed as (%q, %v), want (u-2, true)", wireUser, ok)
	}
	untaggedUser, ok := memberUserIDFromPayload(struct{ UserID string }{UserID: "u-3"})
	if !ok || untaggedUser != "u-3" {
		t.Fatalf("untagged struct payload probed as (%q, %v), want (u-3, true)", untaggedUser, ok)
	}

	// end to end: each shape published on the bus re-instates the member's
	// revoked grant, after a removal reaped it first.
	byShape := []struct {
		userID  string
		payload any
	}{
		{"u-1", restoredMember{MembershipID: "m-1", UserID: "u-1", NodeID: "n-1"}},
		{"u-2", map[string]any{"user_id": "u-2", "node_id": "n-1"}},
		{"u-3", struct{ UserID string }{UserID: "u-3"}},
	}
	ctx := context.Background()
	for _, shape := range byShape {
		sub := Subject{TenantID: "tenant-a", UserID: shape.userID}
		grant(t, svc, sub, "reader", Scope{}, "notes:read")
		publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: shape.userID})
		if ok, _ := svc.Can(ctx, sub, "read", "notes"); ok {
			t.Fatalf("user %s's grant survived the removal (payload %T)", shape.userID, shape.payload)
		}
		publishMemberRestored(t, reg, sub.TenantID, shape.payload)
		if ok, err := svc.Can(ctx, sub, "read", "notes"); err != nil || !ok {
			t.Fatalf("user %s (payload %T) still revoked after the restore: Can = %v, %v", shape.userID, shape.payload, ok, err)
		}
	}
}

func TestService_OnMemberRestored_OneFailedRestoreDoesNotAbortTheRest(t *testing.T) {
	// The re-instater cannot abort on a failure -- a restore delivery that
	// re-instated nine of ten grants has done real work, and surfacing an
	// error would make org's committed restore look failed. Here the
	// restored member has two revoked bindings, and the role behind one of
	// them is gone by restore time: the removal reaped BOTH while both
	// roles were still resolvable (so both rows carry the member-removal
	// origin the restore side scopes by), and only THEN is the second
	// role's row physically deleted behind the scenes (which nothing in
	// this module prevents a future delete path from doing). Resolving the
	// doomed binding's role therefore fails inside the restore pass
	// itself, after the marker has admitted the row; the other binding
	// must still be re-instated, and the handler must still return nil.
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
	// The removal reaps both bindings; both roles resolve at this point, so
	// both rows are revoked with the member-removal origin.
	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})

	// Now the second role's row is physically deleted, leaving the revoked
	// binding a dangling reference for the restore pass to fail on.
	if err = svc.roles.Delete(ctx, doomed.ID); err != nil {
		t.Fatalf("deleting the role behind the second binding: %v", err)
	}

	rec := recordEvents(reg)
	publishMemberRestored(t, reg, sub.TenantID, restoredMember{UserID: sub.UserID})

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil || !ok {
		t.Fatalf("the binding whose role resolved was not re-instated: Can = %v, %v", ok, err)
	}
	restored := rec.ofType(EventRoleBindingRestored)
	if len(restored) != 1 {
		t.Fatalf("got %d %s events, want 1 (only the resolvable binding)", len(restored), EventRoleBindingRestored)
	}
	if p := restored[0].Payload.(RoleBindingChangedEvent); p.RoleKey != "reader" {
		t.Errorf("restored event names role %q, want reader", p.RoleKey)
	}
	rows, err := svc.bindings.RevokedByUser(ctx, sub.UserID)
	if err != nil {
		t.Fatalf("listing the remaining revoked bindings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d remaining revoked bindings, want 1 (the unresolvable one)", len(rows))
	}
	if rows[0].RevokeOrigin != revokeOriginMemberRemoval {
		t.Errorf("the unresolvable row's revoke_origin = %q, want %q", rows[0].RevokeOrigin, revokeOriginMemberRemoval)
	}
}

func TestService_OnMemberRestored_RestoreOnOneReplica_ConvergesTheOther(t *testing.T) {
	// Two Services over one database and one bus: the shape of a
	// multi-replica deployment. The re-instatement runs on the replica that
	// hears the restore, and its RestoreRole writes travel to the other
	// replica through EventRoleBindingRestored -- replica B, which cached
	// the revoked (denied) decision after the removal, must answer
	// "granted" on the very next call, not one TTL later.
	db := newRBACTestDB(t)
	reg := newPlainRegistry()
	if err := reg.Permissions.Add(testPermissions...); err != nil {
		t.Fatalf("declaring permissions: %v", err)
	}
	replicaA := attachReplica(t, db, reg)
	replicaB := attachReplica(t, db, reg)

	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, replicaA, sub, "reader", Scope{}, "notes:read")
	publishMemberRemoved(t, reg, sub.TenantID, removedMember{UserID: sub.UserID})
	if ok, _ := replicaB.Can(context.Background(), sub, "read", "notes"); ok {
		t.Fatal("replica B still granted after the removal reaped the binding")
	}

	publishMemberRestored(t, reg, sub.TenantID, restoredMember{UserID: sub.UserID})

	ok, err := replicaB.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can on replica B: %v", err)
	}
	if !ok {
		t.Fatal("replica B still denies a grant the member restore re-instated on replica A")
	}
}

// restoredNode is the shape org's own NodeRestored payload takes on the
// wire: a struct whose JSON tags are the snake_case field names a Redis
// delivery decodes to, re-declared here for the identical reason
// deletedNode's own doc comment gives. Only the field this handler reads is
// reproduced; org's Path field carries nothing the node-id probe looks at.
type restoredNode struct {
	NodeID string `json:"node_id"`
}

// publishNodeRestored delivers an org.node.restored event the way org's own
// bus.Publish would after a node restore committed: on the registry's bus,
// under the tenant the restore happened in. The in-memory bus runs every
// subscriber synchronously inside Publish, so when this helper returns,
// every re-instatement the event triggers has already happened.
func publishNodeRestored(t *testing.T, reg *pkgcore.Registry, tenant pkgcore.TenantID, payload any) {
	t.Helper()
	bus := reg.Events.Bus()
	if bus == nil {
		t.Fatal("publishNodeRestored: the registry carries no event bus")
	}
	if err := bus.Publish(pkgcore.WithTenant(context.Background(), tenant), pkgcore.Event{
		Type:     eventNodeRestored,
		TenantID: tenant,
		Payload:  payload,
	}); err != nil {
		t.Fatalf("publishing %s: %v", eventNodeRestored, err)
	}
}

func TestService_OnNodeRestored_ReinstatesBindingsScopedToTheRestoredNodeAndSparesEveryoneElse(t *testing.T) {
	// The re-instatement must be precise to the node id the event names:
	// revoked bindings scoped at the restored node come back, while the
	// identical node id's revoked rows in another tenant, and a revoked
	// binding at a SECOND deleted node of the same tenant -- org restores
	// one node at a time, never a whole cascade -- stay revoked. A
	// re-instatement that spilled across either boundary would restore
	// access no org event authorized.
	svc, reg := newTestServiceWithRegistry(t)

	atRestoredNode := Subject{TenantID: "tenant-a", UserID: "user-1"}
	secondUserAtRestoredNode := Subject{TenantID: "tenant-a", UserID: "user-2"}
	atStillDeletedNode := Subject{TenantID: "tenant-a", UserID: "user-1"}
	sameNodeOtherTenant := Subject{TenantID: "tenant-b", UserID: "user-3"}

	grant(t, svc, atRestoredNode, "reader", Scope{NodeID: "node-restored"}, "notes:read")
	grant(t, svc, secondUserAtRestoredNode, "reader", Scope{NodeID: "node-restored"}, "notes:read")
	grant(t, svc, atStillDeletedNode, "writer", Scope{NodeID: "node-still-deleted"}, "notes:write")
	grant(t, svc, sameNodeOtherTenant, "reader", Scope{NodeID: "node-restored"}, "notes:read")
	// The tenant-b row is revoked manually -- the event below names the
	// same node id in tenant-a, and must not reach across.
	if err := svc.RevokeRole(tenantCtx("tenant-b"), sameNodeOtherTenant, "reader", Scope{NodeID: "node-restored"}); err != nil {
		t.Fatalf("revoking the tenant-b grant: %v", err)
	}

	// A cascade delete of tenant-a's two nodes reaps the three live
	// bindings scoped to either of them in one pass.
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{
		NodeID:         "node-restored",
		DeletedNodeIds: []string{"node-restored", "node-still-deleted"},
	})

	rec := recordEvents(reg)
	publishNodeRestored(t, reg, "tenant-a", restoredNode{NodeID: "node-restored"})

	// Every re-instated grant must have travelled the full RestoreRole
	// path: one EventRoleBindingRestored per distinct (user, role) tuple at
	// the restored node, each naming the tenant and the restored node. The
	// probes below come AFTER these assertions on purpose -- probing a live
	// binding revokes and restores it, which would pollute the count.
	restoredEvts := rec.ofType(EventRoleBindingRestored)
	if len(restoredEvts) != 2 {
		t.Fatalf("got %d %s events, want 2 (one per distinct grant at the restored node)", len(restoredEvts), EventRoleBindingRestored)
	}
	for _, evt := range restoredEvts {
		p, ok := evt.Payload.(RoleBindingChangedEvent)
		if !ok {
			t.Fatalf("restored event payload = %T, want RoleBindingChangedEvent", evt.Payload)
		}
		if evt.TenantID != "tenant-a" || p.NodeID != "node-restored" {
			t.Errorf("restored event names (%q, %q), want (tenant-a, node-restored)", evt.TenantID, p.NodeID)
		}
	}

	// Redelivery of the same event re-instates nothing twice.
	publishNodeRestored(t, reg, "tenant-a", restoredNode{NodeID: "node-restored"})
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 2 {
		t.Fatalf("a redelivered node-restored event produced %d %s events, want 2 (no re-restore)", got, EventRoleBindingRestored)
	}

	// Only the restored node's two revoked bindings are live again; the
	// still-deleted node's binding and the other tenant's row stay revoked.
	// (Probing the two live rows adds its own revoke/restore pair to the
	// recorder, which nothing after this asserts on.)
	ctx := context.Background()
	for _, check := range []struct {
		sub   Subject
		role  string
		scope Scope
		want  bool
	}{
		{atRestoredNode, "reader", Scope{NodeID: "node-restored"}, true},
		{secondUserAtRestoredNode, "reader", Scope{NodeID: "node-restored"}, true},
		{atStillDeletedNode, "writer", Scope{NodeID: "node-still-deleted"}, false},
		{sameNodeOtherTenant, "reader", Scope{NodeID: "node-restored"}, false},
	} {
		live, err := roleBindingLiveAt(ctx, svc, check.sub, check.role, check.scope)
		if err != nil {
			t.Fatalf("probing %v at %+v: %v", check.sub, check.scope, err)
		}
		if live != check.want {
			t.Fatalf("binding of %v at %+v live = %v, want %v", check.sub, check.scope, live, check.want)
		}
	}
}

// roleBindingLiveAt answers whether sub holds a live binding for roleKey at
// exactly scope, probing through RevokeRole's strictness: the probe revokes
// a live binding and immediately restores it (RestoreRole is idempotent per
// its own contract, so the probe leaves no trace), and answers "not live"
// when RevokeRole reports ErrBindingNotFound -- with nothing written, since
// a revoked binding cannot be revoked again. It is the scope-generalized
// probe the node tests above need, where Can's own tenant-wide aggregation
// cannot distinguish "re-instated at this one node" from "still granted at
// some other scope entirely".
func roleBindingLiveAt(ctx context.Context, svc *Service, sub Subject, roleKey string, scope Scope) (bool, error) {
	switch err := svc.RevokeRole(ctx, sub, roleKey, scope); {
	case err == nil:
		if restoreErr := svc.RestoreRole(ctx, sub, roleKey, scope); restoreErr != nil {
			return false, restoreErr
		}
		return true, nil
	case isBindingNotFound(err):
		return false, nil
	default:
		return false, err
	}
}

func TestService_OnNodeRestored_ForeignPayloadsAreDroppedWithoutError(t *testing.T) {
	// The handler runs inside org's own Publish call on the in-memory bus;
	// an error here would make a committed node restore report failure.
	// Every payload rbac cannot read must therefore be dropped, and a
	// payload arriving without a tenant must leave the tenant's bindings
	// untouched. A revoked row before each call and a still-revoked row
	// after the whole gauntlet proves none of the drops re-instated
	// anything.
	svc, reg := newTestServiceWithRegistry(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{NodeID: "node-1"}, "notes:read")
	if err := svc.RevokeRole(tenantCtx("tenant-a"), sub, "reader", Scope{NodeID: "node-1"}); err != nil {
		t.Fatalf("revoking the grant the drops must not restore: %v", err)
	}

	rec := recordEvents(reg)

	handler := svc.onNodeRestored
	base := pkgcore.Event{Type: eventNodeRestored}
	for _, payload := range []any{
		nil,
		"not a payload",
		42,
		map[string]any{"path": "/root/node-1"}, // no node id at all
		map[string]any{"node_id": ""},          // empty node id
		map[string]any{"node_id": 42},          // node id of the wrong type
		map[string]any{"NODE_ID": "node-1"},    // a key no accepted spelling matches
	} {
		evt := base
		evt.TenantID = "tenant-a"
		evt.Payload = payload
		if err := handler(context.Background(), evt); err != nil {
			t.Fatalf("onNodeRestored with payload %v: %v", payload, err)
		}
	}

	// A well-formed node id with no tenant: the tenant-less branch, skipped
	// at Debug -- there is no tenant whose bindings could be re-instated.
	if err := handler(context.Background(), pkgcore.Event{
		Type: eventNodeRestored, Payload: restoredNode{NodeID: "node-1"},
	}); err != nil {
		t.Fatalf("onNodeRestored with a tenant-less event: %v", err)
	}

	if ok, err := roleBindingLiveAt(context.Background(), svc, sub, "reader", Scope{NodeID: "node-1"}); err != nil || ok {
		t.Fatalf("a dropped payload restored a revoked binding: live = %v, %v", ok, err)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 0 {
		t.Fatalf("dropped payloads published %d restore events", got)
	}

	// And the delivery itself must not fail either: a foreign payload
	// published through the real bus surfaces no error to the publisher.
	publishNodeRestored(t, reg, sub.TenantID, "foreign payload")
	if ok, err := roleBindingLiveAt(context.Background(), svc, sub, "reader", Scope{NodeID: "node-1"}); err != nil || ok {
		t.Fatalf("a foreign payload published on the bus restored a revoked binding: live = %v, %v", ok, err)
	}
}

func TestService_OnNodeRestored_WireShapesAllReinstateTheSameBindings(t *testing.T) {
	// org's payload reaches rbac in three shapes: the same-process struct
	// with snake_case JSON tags, the map a Redis delivery decodes to, and --
	// for a payload struct published without tags -- the Go field-name
	// spelling. All three must re-instate, because the bus implementation
	// is the host's choice, not org's or rbac's.
	svc, reg := newTestServiceWithRegistry(t)

	// unit-level: the probe reads every accepted spelling.
	structID, ok := nodeRestoredNodeIDFromPayload(restoredNode{NodeID: "n-1"})
	if !ok || structID != "n-1" {
		t.Fatalf("json-tagged struct payload probed as (%q, %v), want (n-1, true)", structID, ok)
	}
	wireID, ok := nodeRestoredNodeIDFromPayload(map[string]any{"node_id": "n-2"})
	if !ok || wireID != "n-2" {
		t.Fatalf("wire map payload probed as (%q, %v), want (n-2, true)", wireID, ok)
	}
	untaggedID, ok := nodeRestoredNodeIDFromPayload(struct{ NodeID string }{NodeID: "n-3"})
	if !ok || untaggedID != "n-3" {
		t.Fatalf("untagged struct payload probed as (%q, %v), want (n-3, true)", untaggedID, ok)
	}

	// end to end: each shape published on the bus re-instates the binding
	// at the node it names, after a delete reaped it first.
	byShape := []struct {
		nodeID  string
		payload any
	}{
		{"n-1", restoredNode{NodeID: "n-1"}},
		{"n-2", map[string]any{"node_id": "n-2"}},
		{"n-3", struct{ NodeID string }{NodeID: "n-3"}},
	}
	for _, shape := range byShape {
		sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
		grant(t, svc, sub, "reader", Scope{NodeID: shape.nodeID}, "notes:read")
		publishNodeDeleted(t, reg, sub.TenantID, deletedNode{DeletedNodeIds: []string{shape.nodeID}})
		publishNodeRestored(t, reg, sub.TenantID, shape.payload)
		rows, err := svc.bindings.RevokedByNodes(tenantCtx("tenant-a"), []string{shape.nodeID})
		if err != nil {
			t.Fatalf("listing revoked bindings at %s: %v", shape.nodeID, err)
		}
		if len(rows) != 0 {
			t.Fatalf("node %s (payload %T) still has %d revoked bindings after the restore", shape.nodeID, shape.payload, len(rows))
		}
	}
}

func TestService_OnNodeRestored_MemberRemovedWhileNodeDeleted_IsNotReinstated(t *testing.T) {
	// The P0-rbac-8 regression this round closes, in its hardest ordering:
	// the node is deleted FIRST (the node-deletion reap writes the
	// member's row with the node-deletion origin), THEN the member is
	// removed from the tenant -- the member-removal reap finds nothing
	// live to withdraw, but its claim step re-attributes the still-revoked
	// row to the member-removal -- and THEN the node is restored. The node
	// restore must NOT re-instate the removed member's row: doing so would
	// rebuild live authorization for a holder who is no longer a member,
	// with no membership behind it. Before this round the row carried no
	// origin, the removal recorded nothing (there was nothing live to
	// reap), and the node restore resurrected the grant -- this test
	// failed on that code, with the removed member's Can answering true.
	// A second member who was never removed, whose row the same deletion
	// reaped alongside the first, IS re-instated -- the mechanism keeps
	// working for its intended case, and the two rows differ only in the
	// claim.
	svc, reg := newTestServiceWithRegistry(t)

	removed := Subject{TenantID: "tenant-a", UserID: "user-removed"}
	kept := Subject{TenantID: "tenant-a", UserID: "user-kept"}
	ctx := tenantCtx("tenant-a")
	grant(t, svc, removed, "reader", Scope{NodeID: "node-1"}, "notes:read")
	grant(t, svc, kept, "reader", Scope{NodeID: "node-1"}, "notes:read")

	// Node deletion reaps both live bindings at node-1 in one pass.
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{DeletedNodeIds: []string{"node-1"}})
	rows, err := svc.bindings.RevokedByNodes(ctx, []string{"node-1"})
	if err != nil {
		t.Fatalf("listing revoked bindings at the deleted node: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d revoked bindings after the delete, want 2", len(rows))
	}
	for _, row := range rows {
		if row.RevokeOrigin != revokeOriginNodeDeletion {
			t.Errorf("the node-deletion reap wrote revoke_origin %q, want %q", row.RevokeOrigin, revokeOriginNodeDeletion)
		}
	}

	// The member leaves while the node is gone: nothing live to reap, but
	// the claim step re-attributes her still-revoked row to the
	// member-removal origin.
	publishMemberRemoved(t, reg, "tenant-a", removedMember{UserID: removed.UserID})
	claimed, err := svc.bindings.RevokedByUser(ctx, removed.UserID)
	if err != nil {
		t.Fatalf("listing the removed member's revoked bindings: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("got %d revoked rows for the removed member, want 1", len(claimed))
	}
	if claimed[0].RevokeOrigin != revokeOriginMemberRemoval {
		t.Errorf("the removed member's row carries revoke_origin %q, want %q (the claim must re-attribute it)",
			claimed[0].RevokeOrigin, revokeOriginMemberRemoval)
	}

	// The node comes back: only the member who is still in the tenant is
	// re-instated. The removed member's Can must stay false -- pre-fix it
	// was restored and counted into Can, which is exactly the escalation.
	rec := recordEvents(reg)
	publishNodeRestored(t, reg, "tenant-a", restoredNode{NodeID: "node-1"})

	if ok, canErr := svc.Can(context.Background(), kept, "read", "notes"); canErr != nil || !ok {
		t.Fatalf("the member who was never removed was not re-instated: Can = %v, %v", ok, canErr)
	}
	if ok, canErr := svc.Can(context.Background(), removed, "read", "notes"); canErr != nil || ok {
		t.Fatalf("the removed member regained authorization with the node: Can = %v, %v; want false", ok, canErr)
	}
	rows, err = svc.bindings.RevokedByNodes(ctx, []string{"node-1"})
	if err != nil {
		t.Fatalf("listing revoked bindings at the restored node: %v", err)
	}
	if len(rows) != 1 || rows[0].UserID != removed.UserID {
		t.Fatalf("revoked rows at the restored node = %+v, want exactly the removed member's row", rows)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 1 {
		t.Fatalf("got %d %s events, want 1 (only the kept member's grant)", got, EventRoleBindingRestored)
	}
}

func TestService_OnNodeRestored_MemberRemovedBeforeTheNodeDeleted_IsNotReinstated(t *testing.T) {
	// The P0-rbac-8 regression in its second ordering: the member is
	// removed FIRST, while her binding at node-1 is live -- the
	// member-removal reap revokes it with the member-removal origin -- and
	// only THEN is the node deleted (its reap finds nothing live left to
	// withdraw) and restored. The node restore enumerates the revoked rows
	// at the restored node, and the member-origin row must not be among
	// those it re-instates: the row belongs to the member's own removal,
	// not to the node's deletion, and the member is not coming back (no
	// org.member.restored ever fires for her). Before this round the
	// restore re-instated every revoked row at the node regardless of who
	// wrote the revoke, and the removed member's grant silently returned --
	// this test failed on that code.
	svc, reg := newTestServiceWithRegistry(t)

	removed := Subject{TenantID: "tenant-a", UserID: "user-removed"}
	grant(t, svc, removed, "reader", Scope{NodeID: "node-1"}, "notes:read")

	publishMemberRemoved(t, reg, "tenant-a", removedMember{UserID: removed.UserID})
	if ok, _ := svc.Can(context.Background(), removed, "read", "notes"); ok {
		t.Fatal("the removed member's grant survived the removal")
	}

	publishNodeDeleted(t, reg, "tenant-a", deletedNode{DeletedNodeIds: []string{"node-1"}})

	rec := recordEvents(reg)
	publishNodeRestored(t, reg, "tenant-a", restoredNode{NodeID: "node-1"})

	if ok, err := svc.Can(context.Background(), removed, "read", "notes"); err != nil || ok {
		t.Fatalf("the removed member regained authorization with the node: Can = %v, %v; want false", ok, err)
	}
	rows, err := svc.bindings.RevokedByUser(tenantCtx("tenant-a"), removed.UserID)
	if err != nil {
		t.Fatalf("listing the removed member's revoked bindings: %v", err)
	}
	if len(rows) != 1 || rows[0].NodeID != "node-1" {
		t.Fatalf("revoked rows of the removed member = %+v, want exactly the node-1 row", rows)
	}
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 0 {
		t.Fatalf("got %d %s events, want 0 (the node restore had nothing of its own to re-instate)", got, EventRoleBindingRestored)
	}
}

func TestService_OnNodeRestored_ReinstatesOnlyRowsTheNodeDeletionItselfReaped(t *testing.T) {
	// The P0-rbac-8 scoping rule at its most literal: a node restore may
	// re-instate exactly the rows THIS node's deletion reaped -- the rows
	// carrying the node-deletion origin -- never a deliberate RevokeRole
	// revocation at the same node that predates the deletion. An
	// administrator who revoked a grant while the node was still alive,
	// and never re-granted it, made a decision the node's return must not
	// undo; before this round the restore could not tell the deliberate
	// row from the reaped one and undid both.
	svc, reg := newTestServiceWithRegistry(t)

	deliberate := Subject{TenantID: "tenant-a", UserID: "user-deliberate"}
	reaped := Subject{TenantID: "tenant-a", UserID: "user-reaped"}
	ctx := tenantCtx("tenant-a")
	grant(t, svc, deliberate, "reader", Scope{NodeID: "node-1"}, "notes:read")
	grant(t, svc, reaped, "reader", Scope{NodeID: "node-1"}, "notes:read")

	// The deliberate revocation predates the deletion: the row is
	// soft-deleted with the deliberate origin and the deletion reap (which
	// only ever sees live rows) never touches it.
	if err := svc.RevokeRole(ctx, deliberate, "reader", Scope{NodeID: "node-1"}); err != nil {
		t.Fatalf("manual revoke before the delete: %v", err)
	}
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{DeletedNodeIds: []string{"node-1"}})

	rec := recordEvents(reg)
	publishNodeRestored(t, reg, "tenant-a", restoredNode{NodeID: "node-1"})

	// The count assertion comes BEFORE the live probes: probing a live
	// binding revokes and restores it, which would pollute the recorder.
	if got := len(rec.ofType(EventRoleBindingRestored)); got != 1 {
		t.Fatalf("got %d %s events, want 1 (only the reaped binding)", got, EventRoleBindingRestored)
	}
	if ok, err := roleBindingLiveAt(context.Background(), svc, reaped, "reader", Scope{NodeID: "node-1"}); err != nil || !ok {
		t.Fatalf("the reaped binding was not re-instated: live = %v, %v", ok, err)
	}
	if ok, err := roleBindingLiveAt(context.Background(), svc, deliberate, "reader", Scope{NodeID: "node-1"}); err != nil || ok {
		t.Fatalf("the deliberate revocation was re-instated by the node restore: live = %v, %v; want false", ok, err)
	}
}

func TestService_OnMemberRemoved_ClaimsOnlyTheNodeReapedRows(t *testing.T) {
	// The claim step's precision: a member removal re-attributes the
	// removed member's still-revoked node-deletion rows to the
	// member-removal origin (so only her own member restore can resurrect
	// them) and leaves every OTHER kind of revoked row exactly as it was.
	// The removed member here holds three revoked rows when the removal
	// lands: one the node-deletion reap wrote (must flip), one a
	// deliberate RevokeRole wrote (must NOT flip -- no org event may ever
	// resurrect a deliberate row, so there is nothing to claim), and one
	// the removal itself revokes in this very pass (already member-origin,
	// stays). A second user's node-reaped row in the same tenant is
	// untouched too: it belongs to a different member's story.
	svc, reg := newTestServiceWithRegistry(t)

	sub := Subject{TenantID: "tenant-a", UserID: "user-gone"}
	other := Subject{TenantID: "tenant-a", UserID: "user-other"}
	ctx := tenantCtx("tenant-a")

	// Row at node-1: the node-deletion reap writes it (node deleted while
	// the member still held the grant).
	grant(t, svc, sub, "reader", Scope{NodeID: "node-1"}, "notes:read")
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{DeletedNodeIds: []string{"node-1"}})

	// Row at the tenant root: a deliberate revocation.
	grant(t, svc, sub, "writer", Scope{}, "notes:write")
	if err := svc.RevokeRole(ctx, sub, "writer", Scope{}); err != nil {
		t.Fatalf("manual revoke: %v", err)
	}

	// Row at node-2: still live when the removal below lands, so the
	// member-removal reap revokes it with the member-removal origin in the
	// same pass whose claim this test inspects.
	grant(t, svc, sub, "reader", Scope{NodeID: "node-2"}, "notes:read")

	// Control: another user's node-reaped row, which the claim must not
	// touch.
	grant(t, svc, other, "reader", Scope{NodeID: "node-3"}, "notes:read")
	publishNodeDeleted(t, reg, "tenant-a", deletedNode{DeletedNodeIds: []string{"node-3"}})

	publishMemberRemoved(t, reg, "tenant-a", removedMember{UserID: sub.UserID})

	rows, err := svc.bindings.RevokedByUser(ctx, sub.UserID)
	if err != nil {
		t.Fatalf("listing the removed member's revoked bindings: %v", err)
	}
	byNode := map[string]string{}
	for _, row := range rows {
		byNode[row.NodeID] = row.RevokeOrigin
	}
	if byNode["node-1"] != revokeOriginMemberRemoval {
		t.Errorf("the node-reaped row's origin after the removal = %q, want %q (claimed)", byNode["node-1"], revokeOriginMemberRemoval)
	}
	if byNode[""] != revokeOriginDeliberate {
		t.Errorf("the deliberate row's origin after the removal = %q, want %q (never claimed)", byNode[""], revokeOriginDeliberate)
	}
	if byNode["node-2"] != revokeOriginMemberRemoval {
		t.Errorf("the row this removal reaped has origin %q, want %q", byNode["node-2"], revokeOriginMemberRemoval)
	}
	otherRows, err := svc.bindings.RevokedByUser(ctx, other.UserID)
	if err != nil {
		t.Fatalf("listing the other user's revoked bindings: %v", err)
	}
	if len(otherRows) != 1 || otherRows[0].RevokeOrigin != revokeOriginNodeDeletion {
		t.Errorf("the other user's node-reaped row after the removal = %+v, want one row with origin %q",
			otherRows, revokeOriginNodeDeletion)
	}
}
