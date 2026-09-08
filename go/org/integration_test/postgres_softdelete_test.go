//go:build integration

package org_test

import (
	"testing"

	"github.com/vislake/speed/go/org"
)

// TestTreeService_Delete_ThenCreateChild_SameSiblingName_Succeeds_Postgres
// re-runs go/org's own tree_test.go case of the same name against a real
// PostgreSQL server. It is the proof that
// uq_org_nodes_sibling_name's replacement by its WHERE deleted_at IS NULL
// partial-index equivalent (migrations/{sqlite,postgres}/0004_add_soft_delete.sql)
// behaves identically on the engine whose partial-index syntax and collation
// genuinely differ from SQLite's -- the unit-tier SQLite proof alone cannot
// rule out a PostgreSQL-specific partial-index mistake (a wrong predicate, a
// missing WHERE clause the SQLite planner tolerates differently) shipping
// unnoticed. Against a full unique index this CreateChild would fail with
// ErrDuplicateSiblingName -- a real functional regression the migration
// exists to avoid, per its own header comment.
func TestTreeService_Delete_ThenCreateChild_SameSiblingName_Succeeds_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	original, err := tree.CreateChild(ctx, root.ID, "North Region", "region")
	if err != nil {
		t.Fatalf("CreateChild(original): %v", err)
	}

	if err := tree.Delete(ctx, original.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	recreated, err := tree.CreateChild(ctx, root.ID, "North Region", "region")
	if err != nil {
		t.Fatalf("CreateChild with a mark-deleted sibling's name: %v, want success", err)
	}
	if recreated.ID == original.ID {
		t.Fatal("CreateChild returned the soft-deleted row instead of inserting a new one")
	}

	// The recreated node behaves like any other live node: it is readable,
	// and it in turn blocks a THIRD node of the same name -- proving the
	// index still enforces uniqueness among LIVE rows on PostgreSQL too, it
	// only stopped counting the mark-deleted one.
	if _, err = tree.Get(ctx, recreated.ID); err != nil {
		t.Errorf("Get(recreated): %v", err)
	}
	_, err = tree.CreateChild(ctx, root.ID, "North Region", "region")
	if !hasCode(err, org.ErrDuplicateSiblingName.Code) {
		t.Errorf("a second live sibling with the same name error = %v, want org.duplicate_sibling_name", err)
	}
}

// TestTreeService_Restore_RecreatesLiveTree_Postgres re-runs go/org's own
// tree_test.go Restore round-trip against a real PostgreSQL server: a
// mark-deleted node comes back readable and visible under its former parent,
// proving the query-only auto-scope plugin (dbkit.SoftDeletable) hides and
// un-hides rows identically on both dialects.
func TestTreeService_Restore_RecreatesLiveTree_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	child, err := tree.CreateChild(ctx, root.ID, "North Region", "region")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	if err := tree.Delete(ctx, child.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := tree.Get(ctx, child.ID); !hasCode(err, org.ErrNodeNotFound.Code) {
		t.Fatalf("Get(deleted child) = %v, want org.node_not_found", err)
	}

	restored, err := tree.Restore(ctx, child.ID)
	if err != nil {
		t.Fatalf("Restore: %v, want success", err)
	}
	if restored.ID != child.ID {
		t.Fatalf("Restore returned id %q, want %q", restored.ID, child.ID)
	}

	got, err := tree.Get(ctx, child.ID)
	if err != nil {
		t.Fatalf("Get(restored child): %v, want success", err)
	}
	if got.ParentID != root.ID {
		t.Errorf("restored child's ParentID = %q, want %q", got.ParentID, root.ID)
	}
}

// TestMemberService_Remove_ThenAdd_SameUser_Succeeds_Postgres re-runs
// go/org's own membership_test.go case of the same name against a real
// PostgreSQL server. It is the proof that
// uq_memberships_tenant_user's replacement by its WHERE deleted_at IS NULL
// partial-index equivalent (migrations/{sqlite,postgres}/0004_add_soft_delete.sql)
// actually frees a removed member's seat for reuse on PostgreSQL, not only
// on SQLite -- the two engines' partial-index and collation behaviour
// genuinely differ, so a SQLite-only proof cannot rule out a
// PostgreSQL-specific regression here. Against a full unique index this
// Add would fail with ErrMembershipExists -- a real functional regression
// the migration exists to avoid.
func TestMemberService_Remove_ThenAdd_SameUser_Succeeds_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree := org.NewTreeService(db)
	members := org.NewMemberService(db, tree)
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	left, err := tree.CreateChild(ctx, root.ID, "left store", "store")
	if err != nil {
		t.Fatalf("CreateChild(left): %v", err)
	}

	if _, err := members.Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	original, err := members.Add(ctx, "u-returning", left.ID)
	if err != nil {
		t.Fatalf("Add(returning): %v", err)
	}
	if err := members.Remove(ctx, "u-returning"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	readded, err := members.Add(ctx, "u-returning", root.ID)
	if err != nil {
		t.Fatalf("Add after Remove of the same user: %v, want success", err)
	}
	if readded.ID == original.ID {
		t.Fatal("Add returned the soft-deleted row instead of creating a new one")
	}
	if readded.NodeID != root.ID {
		t.Errorf("re-added membership NodeID = %q, want %q (Add never re-binds an existing row -- this is a fresh one)",
			readded.NodeID, root.ID)
	}

	// The seat is still exclusive among LIVE rows on PostgreSQL too: a second
	// Add for the same user is still refused.
	if _, err := members.Add(ctx, "u-returning", left.ID); !hasCode(err, org.ErrMembershipExists.Code) {
		t.Errorf("second live Add error = %v, want org.membership_exists", err)
	}
}

// TestMemberService_Restore_ThenGet_Postgres re-runs go/org's own
// membership_test.go Restore round-trip against a real PostgreSQL server: a
// removed membership comes back active and readable through Get.
func TestMemberService_Restore_ThenGet_Postgres(t *testing.T) {
	db := newPostgres(t)
	tree := org.NewTreeService(db)
	members := org.NewMemberService(db, tree)
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	// A second member is required alongside u-member: Remove refuses to
	// remove the tenant's last active member (ErrMemberNotRemovable), so
	// u-owner exists purely to keep u-member's removal from tripping that
	// unrelated guard.
	if _, err := members.Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	membership, err := members.Add(ctx, "u-member", root.ID)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := members.Remove(ctx, "u-member"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := members.Get(ctx, "u-member"); !hasCode(err, org.ErrMembershipNotFound.Code) {
		t.Fatalf("Get(removed member) = %v, want org.membership_not_found", err)
	}

	restored, err := members.Restore(ctx, membership.ID)
	if err != nil {
		t.Fatalf("Restore: %v, want success", err)
	}
	if !restored.IsActive() {
		t.Fatalf("restored membership Status = %q, want active", restored.Status)
	}

	got, err := members.Get(ctx, "u-member")
	if err != nil {
		t.Fatalf("Get(restored member): %v, want success", err)
	}
	if got.ID != membership.ID {
		t.Errorf("Get(restored member) returned id %q, want %q", got.ID, membership.ID)
	}
}

// TestTreeService_CreateRoot_AfterSoftDeletedRoot_Succeeds_Postgres re-runs
// go/org's own tree_test.go case of the same name against a real PostgreSQL
// server. It is the soft-deleted-root-slot regression proof on the dialect
// whose partial-index behaviour genuinely differs from SQLite's: 0007's
// single-root index scoped on parent_id = "" alone keeps a mark-deleted
// root occupying its tenant's root slot, and every later CreateRoot
// collides with the invisible row (org.root_already_exists, forever);
// 0008_single_root_live.sql narrows the index to WHERE parent_id = "" AND
// deleted_at IS NULL, and this test pins that a soft-deleted root's slot
// frees immediately on real PostgreSQL.
func TestTreeService_CreateRoot_AfterSoftDeletedRoot_Succeeds_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	original, err := tree.CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}

	// A host removes the tenant root through the exported Repository surface
	// (TreeService.Delete refuses the root by design).
	if err := tree.Repository().Delete(ctx, original.ID); err != nil {
		t.Fatalf("soft-delete the root through the repository: %v", err)
	}

	replacement, err := tree.CreateRoot(ctx, "Acme Dental Reborn", "group")
	if err != nil {
		t.Fatalf("CreateRoot after the tenant root was soft-deleted: %v, want success -- 0007's single-root index still counted the invisible row on PostgreSQL", err)
	}
	if replacement.ID == original.ID {
		t.Fatal("CreateRoot returned the soft-deleted row instead of inserting a new one")
	}
	got, err := tree.Root(ctx)
	if err != nil {
		t.Fatalf("Root() after the replacement root: %v, want the new root", err)
	}
	if got.ID != replacement.ID {
		t.Errorf("Root() = %q, want the replacement %q", got.ID, replacement.ID)
	}

	// The narrowed index still enforces one LIVE root per tenant.
	if _, err := tree.CreateRoot(ctx, "Another Root", "group"); !hasCode(err, org.ErrRootAlreadyExists.Code) {
		t.Errorf("a second live root error = %v, want org.root_already_exists", err)
	}

	// Restoring the original root collides with the live replacement at the
	// database and answers the coded slot-taken error.
	if _, err := tree.Restore(ctx, original.ID); !hasCode(err, org.ErrDuplicateSiblingName.Code) {
		t.Errorf("Restore of the original root whose root slot was re-taken = %v, want the coded org.duplicate_sibling_name", err)
	}
}
