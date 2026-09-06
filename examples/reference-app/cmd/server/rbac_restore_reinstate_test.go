package main

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// This file is the real cross-module regression for the RESTORE side of the
// org-rbac reap pair: a role binding that org's member removal or node
// deletion reaped (rbac's own onMemberRemoved / onNodeDeleted subscribers,
// proven end to end by rbac_node_deleted_reap_test.go for the deletion half)
// must come back when org restores what the removal or delete took away --
// the membership through org's own real MemberService.Restore, the node
// through the real TreeService.Restore -- delivered to rbac's own real
// Service through the actual event bus. No mock, no fake payload, no direct
// call into either module's internals: these tests reuse
// newOrgRBACReapHarness, the same two-module kernel (org + rbac over one
// real bus and one SQLite file) rbac_node_deleted_reap_test.go builds, and
// belong in the reference app for the identical reason that file documents:
// this app is the one place that can import both go/org and go/rbac without
// adding a dependency edge between the modules themselves.
//
// PRE-FIX, these tests failed for real: go/rbac carried no subscriber for
// org.member.restored or org.node.restored at all, so the bindings the
// removal and deletion reaps had soft-revoked stayed revoked forever --
// silently, with no restore-side event ever undoing them (the gap the
// fix's reap.go header comment now documents). Running this file against
// go/rbac before that fix failed every test with the binding still revoked
// after org's restore had committed, which is the exact defect this round
// closes.

// TestOrgRBACRestore_MemberRestored_ReinstatesTheReapedBindings is the
// member leg: a member with role bindings at two scopes is removed through
// org's real MemberService (rbac's member-removed subscriber reaps both),
// then the removed membership is restored through org's real
// MemberService.Restore. Once the real org.member.restored event has
// travelled the real bus, both bindings must be live again -- rbac's
// restore-side subscriber undoing exactly the reaped grants, and only
// those of the restored member.
func TestOrgRBACRestore_MemberRestored_ReinstatesTheReapedBindings(t *testing.T) {
	tree, members, rbacService := newOrgRBACReapHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	team, err := tree.CreateChild(ctx, root.ID, "team", "team")
	if err != nil {
		t.Fatalf("CreateChild(team): %v", err)
	}
	// user-1 sits in the team node; user-2 sits at the root so the tenant
	// never hits org's last-active-member refusal when user-1 is removed.
	memberOne, err := members.Add(ctx, "user-1", team.ID)
	if err != nil {
		t.Fatalf("members.Add(user-1): %v", err)
	}
	if _, err = members.Add(ctx, "user-2", root.ID); err != nil {
		t.Fatalf("members.Add(user-2): %v", err)
	}

	if _, err = rbacService.DefineRole(ctx, rbac.RoleDefinition{
		Key: "reader", Permissions: []string{"org:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	// Two scopes -- a tenant-wide grant and one scoped to the member's seat
	// node -- so the cycle demonstrably walks bindings, not just one row.
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{}); err != nil {
		t.Fatalf("AssignRole (tenant-wide): %v", err)
	}
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: team.ID}); err != nil {
		t.Fatalf("AssignRole at the team node: %v", err)
	}

	for _, scope := range []rbac.Scope{{}, {NodeID: team.ID}} {
		var live bool
		live, err = isReaderLiveAtScope(ctx, rbacService, sub, scope)
		if err != nil || !live {
			t.Fatalf("binding at scope %+v was not live before the removal: live = %v, %v", scope, live, err)
		}
	}

	// The real removal, through org's real MemberService -- not a fake event.
	if err = members.Remove(ctx, "user-1"); err != nil {
		t.Fatalf("members.Remove: %v", err)
	}
	for _, scope := range []rbac.Scope{{}, {NodeID: team.ID}} {
		var live bool
		live, err = isReaderLiveAtScope(ctx, rbacService, sub, scope)
		if err != nil || live {
			t.Fatalf("binding at scope %+v survived the removal: live = %v, %v", scope, live, err)
		}
	}

	// The real restore of the very membership Remove hid, by its own id.
	if _, err = members.Restore(ctx, memberOne.ID); err != nil {
		t.Fatalf("members.Restore: %v", err)
	}
	for _, scope := range []rbac.Scope{{}, {NodeID: team.ID}} {
		var live bool
		live, err = isReaderLiveAtScope(ctx, rbacService, sub, scope)
		if err != nil || !live {
			t.Fatalf("binding at scope %+v stayed revoked after the membership restore: live = %v, %v", scope, live, err)
		}
	}
}

// TestOrgRBACRestore_NodeRestored_ReinstatesTheReapedBinding is the node
// leg, single-node shape: a role binding scoped to a node with no org
// member ever bound to it is reaped by org's real TreeService.Delete, then
// the node is restored through org's real TreeService.Restore. Once the
// real org.node.restored event has travelled the real bus, the binding must
// be live again -- and a live binding at a sibling node, which the delete
// never touched, must stay untouched throughout.
func TestOrgRBACRestore_NodeRestored_ReinstatesTheReapedBinding(t *testing.T) {
	tree, _, rbacService := newOrgRBACReapHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	leaf, err := tree.CreateChild(ctx, root.ID, "leaf", "team")
	if err != nil {
		t.Fatalf("CreateChild(leaf): %v", err)
	}
	sibling, err := tree.CreateChild(ctx, root.ID, "sibling", "team")
	if err != nil {
		t.Fatalf("CreateChild(sibling): %v", err)
	}

	if _, err = rbacService.DefineRole(ctx, rbac.RoleDefinition{
		Key: "reader", Permissions: []string{"org:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: leaf.ID}); err != nil {
		t.Fatalf("AssignRole at the leaf: %v", err)
	}
	if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: sibling.ID}); err != nil {
		t.Fatalf("AssignRole at the sibling: %v", err)
	}

	if err = tree.Delete(ctx, leaf.ID, false); err != nil {
		t.Fatalf("Delete(leaf): %v", err)
	}
	var live bool
	live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: leaf.ID})
	if err != nil || live {
		t.Fatalf("the leaf's binding survived its node's deletion: live = %v, %v", live, err)
	}

	// The real restore of the very node Delete removed, by its own id.
	if _, err = tree.Restore(ctx, leaf.ID); err != nil {
		t.Fatalf("tree.Restore(leaf): %v", err)
	}
	live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: leaf.ID})
	if err != nil || !live {
		t.Fatalf("the leaf's binding stayed revoked after the node restore: live = %v, %v", live, err)
	}
	live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: sibling.ID})
	if err != nil || !live {
		t.Fatalf("the sibling's binding was disturbed by the restore cycle: live = %v, %v", live, err)
	}
}

// TestOrgRBACRestore_NodeRestored_IsPerNodeNotCascading is the sharpest
// node leg: a cascade delete reaps bindings across a whole subtree, and
// org's Restore is deliberately per-node, never cascading -- so restoring
// the subtree's parent must re-instate only the bindings scoped to that
// one restored node. A binding on a descendant that stays mark-deleted
// must stay revoked: rbac's restore-side subscriber mirrors org's own
// per-node restore shape exactly, never resurrecting a grant on a node the
// tree still does not have.
func TestOrgRBACRestore_NodeRestored_IsPerNodeNotCascading(t *testing.T) {
	tree, _, rbacService := newOrgRBACReapHarness(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	parent, err := tree.CreateChild(ctx, root.ID, "parent", "team")
	if err != nil {
		t.Fatalf("CreateChild(parent): %v", err)
	}
	childB, err := tree.CreateChild(ctx, parent.ID, "child-b", "team")
	if err != nil {
		t.Fatalf("CreateChild(child-b): %v", err)
	}
	if _, err = tree.CreateChild(ctx, parent.ID, "child-a", "team"); err != nil {
		t.Fatalf("CreateChild(child-a): %v", err)
	}

	if _, err = rbacService.DefineRole(ctx, rbac.RoleDefinition{
		Key: "reader", Permissions: []string{"org:read"},
	}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	sub := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	for _, nodeID := range []string{parent.ID, childB.ID} {
		if err = rbacService.AssignRole(ctx, sub, "reader", rbac.Scope{NodeID: nodeID}); err != nil {
			t.Fatalf("AssignRole at %s: %v", nodeID, err)
		}
	}

	// The cascade delete takes parent, child-a and child-b together; the
	// reap must cover the bindings at parent and child-b in that one pass.
	if err = tree.Delete(ctx, parent.ID, true); err != nil {
		t.Fatalf("Delete(cascade): %v", err)
	}
	for _, nodeID := range []string{parent.ID, childB.ID} {
		var live bool
		live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: nodeID})
		if err != nil || live {
			t.Fatalf("the binding at %s survived the cascade delete: live = %v, %v", nodeID, live, err)
		}
	}

	// Restoring the parent restores exactly the parent -- child-b stays
	// mark-deleted, and its reaped binding must stay revoked with it.
	if _, err = tree.Restore(ctx, parent.ID); err != nil {
		t.Fatalf("tree.Restore(parent): %v", err)
	}
	var live bool
	live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: parent.ID})
	if err != nil || !live {
		t.Fatalf("the parent's binding stayed revoked after the parent restore: live = %v, %v", live, err)
	}
	live, err = isReaderLiveAtScope(ctx, rbacService, sub, rbac.Scope{NodeID: childB.ID})
	if err != nil || live {
		t.Fatalf("the child's binding was re-instated by its parent's restore -- restore-side re-instatement must mirror org's per-node restore: live = %v, %v", live, err)
	}
}

// isReaderLiveAtScope answers whether sub still holds a live "reader"
// binding at exactly scope -- the scope-generalized sibling of
// rbac_node_deleted_reap_test.go's isReaderLiveAtNode, needed because the
// member-restore cycle's precision assertions cover a tenant-wide grant
// alongside node-scoped ones. The probe is RevokeRole itself, for the
// identical reason that file documents: RevokeRole is strict, so a binding
// the reaps already soft-revoked answers ErrBindingNotFound ("not live")
// with nothing written, and a genuinely live binding is revoked by the
// probe call and immediately put back with RestoreRole -- idempotent, per
// RestoreRole's own contract -- so checking one scope never disturbs the
// assertion this file is about to make about another.
func isReaderLiveAtScope(ctx context.Context, svc *rbac.Service, sub rbac.Subject, scope rbac.Scope) (bool, error) {
	switch err := svc.RevokeRole(ctx, sub, "reader", scope); {
	case err == nil:
		if restoreErr := svc.RestoreRole(ctx, sub, "reader", scope); restoreErr != nil {
			return false, restoreErr
		}
		return true, nil
	case isRBACBindingNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
