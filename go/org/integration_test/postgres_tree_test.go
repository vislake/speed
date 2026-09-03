//go:build integration

package org_test

import (
	"sort"
	"testing"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// nodeIDs projects a node slice onto its ids, sorted, so a set comparison
// does not depend on the storage engine's own row order.
func nodeIDs(nodes []org.OrgNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return out
}

func assertIDSet(t *testing.T, nodes []org.OrgNode, want []string) {
	t.Helper()
	got := nodeIDs(nodes)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

// TestSubtree_SiblingSharingAnIDPrefix_Postgres re-runs go/org's own
// repository_test.go "a mid-tree subtree includes itself and excludes the
// prefix-sharing sibling" case against a real PostgreSQL server -- this is
// the load-bearing half of the A1 dialect-identity proof (path.go's own doc
// comment, property 3): SQLite's LIKE is ASCII-case-insensitive by default
// and PostgreSQL's is case-sensitive, so this is the one property that could
// silently diverge between the two engines if the all-lowercase id alphabet
// were ever violated. Seeding through the repository directly (rather than
// through TreeService, whose real uuid.NewString ids will not collide on a
// prefix in any believable test run) is what makes the adversarial shape
// (ids "aa" and "aab") reproducible on demand.
func TestSubtree_SiblingSharingAnIDPrefix_Postgres(t *testing.T) {
	db := newPostgres(t)
	repo := org.NewRepository(db)
	tree := org.NewTreeService(db)
	ctx := tenantCtx("tenant-a")

	// Ids are all-lowercase hex, the alphabet path.go's dialect-identity
	// proof pins ("00", not "r"): a non-hex id would fail TreeService's own
	// path validation before ever exercising the property under test.
	seed := []org.OrgNode{
		{ID: "00", ParentID: "", Path: "/00/", Depth: 0, Name: "root"},
		{ID: "aa", ParentID: "00", Path: "/00/aa/", Depth: 1, Name: "aa"},
		{ID: "aab", ParentID: "00", Path: "/00/aab/", Depth: 1, Name: "aab"},
		{ID: "bb", ParentID: "aa", Path: "/00/aa/bb/", Depth: 2, Name: "bb"},
	}
	for i := range seed {
		if err := repo.Create(ctx, &seed[i]); err != nil {
			t.Fatalf("seed %q: %v", seed[i].ID, err)
		}
	}

	subtree, err := tree.Subtree(ctx, "aa")
	if err != nil {
		t.Fatalf("Subtree(aa): %v", err)
	}
	assertIDSet(t, subtree, []string{"aa", "bb"})

	whole, err := tree.Subtree(ctx, "00")
	if err != nil {
		t.Fatalf("Subtree(00): %v", err)
	}
	assertIDSet(t, whole, []string{"00", "aa", "aab", "bb"})
}

// TestTreeService_Move_RewritesTheWholeSubtree_Postgres re-runs go/org's own
// tree_test.go case of the same name against a real PostgreSQL server: A1's
// property 6, the rewrite done entirely in Go with zero dialect-specific SQL
// (no replace(), no string function), must produce identical rows on both
// engines.
func TestTreeService_Move_RewritesTheWholeSubtree_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	branchA, err := tree.CreateChild(ctx, root.ID, "branch-a", "region")
	if err != nil {
		t.Fatalf("CreateChild(branch-a): %v", err)
	}
	branchB, err := tree.CreateChild(ctx, root.ID, "branch-b", "region")
	if err != nil {
		t.Fatalf("CreateChild(branch-b): %v", err)
	}
	moving, err := tree.CreateChild(ctx, branchA.ID, "moving", "store")
	if err != nil {
		t.Fatalf("CreateChild(moving): %v", err)
	}
	leaf, err := tree.CreateChild(ctx, moving.ID, "leaf", "store")
	if err != nil {
		t.Fatalf("CreateChild(leaf): %v", err)
	}

	moved, err := tree.Move(ctx, moving.ID, branchB.ID)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	if moved.ParentID != branchB.ID {
		t.Errorf("moved.ParentID = %q, want %q", moved.ParentID, branchB.ID)
	}

	updatedLeaf, err := tree.Get(ctx, leaf.ID)
	if err != nil {
		t.Fatalf("Get(leaf) after move: %v", err)
	}
	wantLeafPath := branchB.Path + moving.ID + "/" + leaf.ID + "/"
	if updatedLeaf.Path != wantLeafPath {
		t.Errorf("leaf.Path after move = %q, want %q", updatedLeaf.Path, wantLeafPath)
	}
	if updatedLeaf.Depth != 3 {
		t.Errorf("leaf.Depth after move = %d, want 3", updatedLeaf.Depth)
	}

	// The un-moved sibling branch is untouched -- proof that Move's Go-side
	// rewrite touches exactly the moved subtree's own rows.
	untouched, err := tree.Get(ctx, branchA.ID)
	if err != nil {
		t.Fatalf("Get(branchA) after move: %v", err)
	}
	if untouched.Path != branchA.Path {
		t.Errorf("branchA.Path changed by an unrelated move: got %q, want %q", untouched.Path, branchA.Path)
	}
}

// TestTreeService_Delete_WithCascade_RemovesTheWholeSubtree_Postgres re-runs
// go/org's own tree_test.go case of the same name against a real PostgreSQL
// server: the cascading delete is a single statement (subtreePrefix's LIKE
// scan bound as one parameter), and must remove exactly the subtree's rows
// on both engines.
func TestTreeService_Delete_WithCascade_RemovesTheWholeSubtree_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "root", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	branch, err := tree.CreateChild(ctx, root.ID, "branch", "region")
	if err != nil {
		t.Fatalf("CreateChild(branch): %v", err)
	}
	sibling, err := tree.CreateChild(ctx, root.ID, "sibling", "region")
	if err != nil {
		t.Fatalf("CreateChild(sibling): %v", err)
	}
	if _, err := tree.CreateChild(ctx, branch.ID, "leaf", "store"); err != nil {
		t.Fatalf("CreateChild(leaf): %v", err)
	}

	if err := tree.Delete(ctx, branch.ID, true); err != nil {
		t.Fatalf("Delete(cascade): %v", err)
	}

	if _, err := tree.Get(ctx, branch.ID); !hasCode(err, org.ErrNodeNotFound.Code) {
		t.Errorf("Get(branch) after cascade delete: err = %v, want org.node_not_found", err)
	}
	if remaining, err := tree.Children(ctx, root.ID); err != nil || len(remaining) != 1 || remaining[0].ID != sibling.ID {
		t.Errorf("root's children after cascade delete = %v (err %v), want exactly [sibling]", remaining, err)
	}
}

// TestTreeService_MaxDepth_Postgres re-runs go/org's own
// TestTreeService_CreateChild_BeyondMaxDepth_ReturnsMaxDepthExceeded case
// against a real PostgreSQL server: the depth bound is enforced in Go
// (path.go's own doc comment, property 7) because SQLite does not enforce
// VARCHAR(n) under type affinity -- this proves the same bound also holds on
// the engine that DOES enforce column width, so the two deployment modes
// never disagree about how deep a tree may go.
func TestTreeService_MaxDepth_Postgres(t *testing.T) {
	tree := org.NewTreeService(newPostgres(t))
	ctx := tenantCtx("tenant-a")

	node, err := tree.CreateRoot(ctx, "root", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	// rootDepth is 0; org.maxDepth (unexported) is 8 per path.go, so 8 more
	// creations land exactly at the bound and the 9th must fail.
	for i := 0; i < 8; i++ {
		node, err = tree.CreateChild(ctx, node.ID, "child", "group")
		if err != nil {
			t.Fatalf("CreateChild at depth %d: %v", i+1, err)
		}
	}
	if _, err := tree.CreateChild(ctx, node.ID, "too-deep", "group"); !hasCode(err, org.ErrMaxDepthExceeded.Code) {
		t.Fatalf("CreateChild past the bound: err = %v, want org.max_depth_exceeded", err)
	}
}

// hasCode reports whether err is, or wraps, an *apperr.Error with the given
// code -- the org_test package's own copy of org's unexported helper of the
// same name (errors.go), since this package cannot import it.
func hasCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
