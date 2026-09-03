package org

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// newTestTree returns a TreeService over a freshly migrated SQLite database,
// with sequential, alphabet-valid ids instead of UUIDs so a failure message
// names a node a human can find. The id shape is deliberately variable-length
// ("n1" through "n10" and beyond), which is exactly the case a materialized
// path without a trailing separator would get wrong.
func newTestTree(t *testing.T) *TreeService {
	t.Helper()
	return newTestTreeOn(t, newTestDB(t))
}

func newTestTreeOn(t *testing.T, db *gorm.DB) *TreeService {
	t.Helper()
	tree := NewTreeService(db)
	n := 0
	tree.newID = func() string {
		n++
		return fmt.Sprintf("%da", n)
	}
	return tree
}

// mustCreateRoot and mustCreateChild build fixture trees, failing the test
// rather than returning an error, so a test body reads as the shape it is
// asserting about.
func mustCreateRoot(t *testing.T, tree *TreeService, ctx context.Context, name string) *OrgNode {
	t.Helper()
	node, err := tree.CreateRoot(ctx, name, "group")
	if err != nil {
		t.Fatalf("CreateRoot(%q): %v", name, err)
	}
	return node
}

func mustCreateChild(t *testing.T, tree *TreeService, ctx context.Context, parentID, name string) *OrgNode {
	t.Helper()
	node, err := tree.CreateChild(ctx, parentID, name, "store")
	if err != nil {
		t.Fatalf("CreateChild(%q, %q): %v", parentID, name, err)
	}
	return node
}

func TestTreeService_CreateRoot(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")

	if root.ParentID != "" {
		t.Errorf("root ParentID = %q, want the empty sentinel", root.ParentID)
	}
	if !root.IsRoot() {
		t.Error("root.IsRoot() = false")
	}
	if want := buildPath("", root.ID); root.Path != want {
		t.Errorf("root Path = %q, want %q", root.Path, want)
	}
	if root.Depth != rootDepth {
		t.Errorf("root Depth = %d, want %d", root.Depth, rootDepth)
	}
	if root.Name != "Acme Dental" {
		t.Errorf("root Name = %q, want %q", root.Name, "Acme Dental")
	}
	if root.TenantID != "tenant-a" {
		t.Errorf("root TenantID = %q, want tenant-a", root.TenantID)
	}
}

func TestTreeService_CreateRoot_Twice_ReturnsRootAlreadyExists(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	mustCreateRoot(t, tree, ctx, "Acme Dental")

	_, err := tree.CreateRoot(ctx, "Second Root", "group")
	if err == nil {
		t.Fatal("a second CreateRoot succeeded, want ErrRootAlreadyExists")
	}
	assertCode(t, err, ErrRootAlreadyExists.Code)
}

// TestTreeService_CreateRoot_PerTenant_EachTenantGetsItsOwn proves the
// one-root rule is per tenant, not global: a second tenant creating its own
// root must not collide with the first tenant's.
func TestTreeService_CreateRoot_PerTenant_EachTenantGetsItsOwn(t *testing.T) {
	tree := newTestTree(t)

	rootA := mustCreateRoot(t, tree, tenantCtx("tenant-a"), "Acme Dental")
	rootB := mustCreateRoot(t, tree, tenantCtx("tenant-b"), "Acme Dental")

	if rootA.ID == rootB.ID {
		t.Fatalf("both tenants' roots share id %q", rootA.ID)
	}
	got, err := tree.Root(tenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got.ID != rootA.ID {
		t.Errorf("tenant-a Root = %q, want %q", got.ID, rootA.ID)
	}
}

func TestTreeService_CreateRoot_InvalidName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantCode string
	}{
		{name: "empty", input: "", wantCode: ErrNodeNameRequired.Code},
		{name: "whitespace only", input: "   ", wantCode: ErrNodeNameRequired.Code},
		{name: "too long", input: strings.Repeat("a", maxNameLen+1), wantCode: ErrNodeNameTooLong.Code},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := newTestTree(t)
			_, err := tree.CreateRoot(tenantCtx("tenant-a"), tc.input, "group")
			if err == nil {
				t.Fatalf("CreateRoot(%q) succeeded, want %s", tc.input, tc.wantCode)
			}
			assertCode(t, err, tc.wantCode)
		})
	}
}

func TestTreeService_CreateChild(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	region := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, region.ID, "Store 7")

	if region.ParentID != root.ID {
		t.Errorf("region ParentID = %q, want %q", region.ParentID, root.ID)
	}
	if want := buildPath(root.Path, region.ID); region.Path != want {
		t.Errorf("region Path = %q, want %q", region.Path, want)
	}
	if region.Depth != 1 {
		t.Errorf("region Depth = %d, want 1", region.Depth)
	}
	if want := buildPath(region.Path, store.ID); store.Path != want {
		t.Errorf("store Path = %q, want %q", store.Path, want)
	}
	if store.Depth != 2 {
		t.Errorf("store Depth = %d, want 2", store.Depth)
	}
	// Depth must always agree with the path it was derived from: the two are
	// written together and nothing else may make them disagree.
	for _, n := range []*OrgNode{root, region, store} {
		if got := depthOf(n.Path); got != n.Depth {
			t.Errorf("node %q: depthOf(%q) = %d but stored Depth = %d", n.ID, n.Path, got, n.Depth)
		}
	}
}

func TestTreeService_CreateChild_UnknownParent_ReturnsParentNotFound(t *testing.T) {
	tests := []struct {
		name     string
		parentID string
	}{
		{name: "an id that does not exist", parentID: "nope"},
		{name: "the empty id, which names no node", parentID: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tree := newTestTree(t)
			ctx := tenantCtx("tenant-a")
			mustCreateRoot(t, tree, ctx, "Acme Dental")

			_, err := tree.CreateChild(ctx, tc.parentID, "Child", "store")
			if err == nil {
				t.Fatalf("CreateChild with parent %q succeeded, want ErrParentNotFound", tc.parentID)
			}
			assertCode(t, err, ErrParentNotFound.Code)
		})
	}
}

// TestTreeService_CreateChild_ParentInAnotherTenant_ReturnsParentNotFound
// pins that a cross-tenant parent id is indistinguishable from a
// nonexistent one: reporting a different error would confirm that the id
// exists somewhere, which is itself a cross-tenant leak.
func TestTreeService_CreateChild_ParentInAnotherTenant_ReturnsParentNotFound(t *testing.T) {
	tree := newTestTree(t)
	rootB := mustCreateRoot(t, tree, tenantCtx("tenant-b"), "Other Tenant")
	mustCreateRoot(t, tree, tenantCtx("tenant-a"), "Acme Dental")

	_, err := tree.CreateChild(tenantCtx("tenant-a"), rootB.ID, "Child", "store")
	if err == nil {
		t.Fatal("CreateChild under another tenant's node succeeded")
	}
	assertCode(t, err, ErrParentNotFound.Code)
}

func TestTreeService_CreateChild_DuplicateSiblingName_ReturnsDuplicateSiblingName(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	mustCreateChild(t, tree, ctx, root.ID, "North Region")

	_, err := tree.CreateChild(ctx, root.ID, "North Region", "store")
	if err == nil {
		t.Fatal("a duplicate sibling name succeeded, want ErrDuplicateSiblingName")
	}
	assertCode(t, err, ErrDuplicateSiblingName.Code)

	t.Run("the same name under a different parent is fine", func(t *testing.T) {
		region := mustCreateChild(t, tree, ctx, root.ID, "South Region")
		mustCreateChild(t, tree, ctx, region.ID, "North Region")
	})

	t.Run("names are compared after trimming, so padding is not a bypass", func(t *testing.T) {
		_, err := tree.CreateChild(ctx, root.ID, "  North Region  ", "store")
		if err == nil {
			t.Fatal("a whitespace-padded duplicate succeeded, want ErrDuplicateSiblingName")
		}
		assertCode(t, err, ErrDuplicateSiblingName.Code)
	})
}

// TestTreeService_CreateChild_BeyondMaxDepth_ReturnsMaxDepthExceeded builds
// the deepest legal chain, proves it is accepted, then proves the very next
// level is refused. The bound is enforced in Go precisely because SQLite
// would not enforce the column width that would otherwise catch it.
func TestTreeService_CreateChild_BeyondMaxDepth_ReturnsMaxDepthExceeded(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	node := mustCreateRoot(t, tree, ctx, "level-0")
	for depth := 1; depth <= maxDepth; depth++ {
		node = mustCreateChild(t, tree, ctx, node.ID, fmt.Sprintf("level-%d", depth))
		if node.Depth != depth {
			t.Fatalf("node at level %d reports Depth %d", depth, node.Depth)
		}
	}
	if node.Depth != maxDepth {
		t.Fatalf("deepest legal node Depth = %d, want %d", node.Depth, maxDepth)
	}

	_, err := tree.CreateChild(ctx, node.ID, "one level too deep", "store")
	if err == nil {
		t.Fatal("creating past maxDepth succeeded, want ErrMaxDepthExceeded")
	}
	assertCode(t, err, ErrMaxDepthExceeded.Code)
}

// TestTreeService_CreateChild_CorruptParentPath_ReturnsInternal proves the
// module refuses to derive a new row from a stored path that violates its own
// invariants, rather than propagating the corruption downward.
func TestTreeService_CreateChild_CorruptParentPath_ReturnsInternal(t *testing.T) {
	db := newTestDB(t)
	tree := newTestTreeOn(t, db)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-a")

	seedNode(t, repo, ctx, OrgNode{ID: "bad", Path: "no-leading-separator", Depth: 0, Name: "corrupt"})

	_, err := tree.CreateChild(ctx, "bad", "Child", "store")
	if err == nil {
		t.Fatal("CreateChild under a corrupt parent succeeded, want ErrInternal")
	}
	assertCode(t, err, ErrInternal.Code)
}

func TestTreeService_Rename(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	region := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	pathBefore := region.Path

	renamed, err := tree.Rename(ctx, region.ID, "Northern Region")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "Northern Region" {
		t.Errorf("renamed Name = %q, want %q", renamed.Name, "Northern Region")
	}
	// A rename must never touch the path: the path is built from ids, so a
	// node's identity in every subtree query survives any rename.
	if renamed.Path != pathBefore {
		t.Errorf("Rename changed Path from %q to %q", pathBefore, renamed.Path)
	}

	reloaded, err := tree.Get(ctx, region.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Name != "Northern Region" {
		t.Errorf("reloaded Name = %q, want the renamed value", reloaded.Name)
	}
}

func TestTreeService_Rename_ToItsOwnName_IsANoOp(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	region := mustCreateChild(t, tree, ctx, root.ID, "North Region")

	got, err := tree.Rename(ctx, region.ID, "  North Region  ")
	if err != nil {
		t.Fatalf("renaming a node to its own name failed: %v", err)
	}
	if got.Name != "North Region" {
		t.Errorf("Name = %q, want unchanged", got.Name)
	}
}

func TestTreeService_Rename_DuplicateSiblingName_ReturnsDuplicateSiblingName(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	mustCreateChild(t, tree, ctx, root.ID, "North Region")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")

	_, err := tree.Rename(ctx, south.ID, "North Region")
	if err == nil {
		t.Fatal("renaming onto a sibling's name succeeded")
	}
	assertCode(t, err, ErrDuplicateSiblingName.Code)
}

func TestTreeService_Rename_UnknownNode_ReturnsNodeNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")
	mustCreateRoot(t, tree, ctx, "Acme Dental")

	_, err := tree.Rename(ctx, "nope", "Whatever")
	if err == nil {
		t.Fatal("renaming an unknown node succeeded")
	}
	assertCode(t, err, ErrNodeNotFound.Code)
}

// TestTreeService_Move_RewritesTheWholeSubtree is the central move case: a
// three-level subtree is re-parented and every descendant's path and depth
// must follow, with the parent edges of the descendants untouched.
func TestTreeService_Move_RewritesTheWholeSubtree(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	moved, err := tree.Move(ctx, north.ID, south.ID)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}

	if moved.ParentID != south.ID {
		t.Errorf("moved ParentID = %q, want %q", moved.ParentID, south.ID)
	}
	if want := buildPath(south.Path, north.ID); moved.Path != want {
		t.Errorf("moved Path = %q, want %q", moved.Path, want)
	}
	if moved.Depth != 2 {
		t.Errorf("moved Depth = %d, want 2", moved.Depth)
	}

	tests := []struct {
		id        string
		wantDepth int
		parentID  string
	}{
		{id: north.ID, wantDepth: 2, parentID: south.ID},
		{id: store.ID, wantDepth: 3, parentID: north.ID},
		{id: room.ID, wantDepth: 4, parentID: store.ID},
	}
	for _, tc := range tests {
		got, getErr := tree.Get(ctx, tc.id)
		if getErr != nil {
			t.Fatalf("Get(%q): %v", tc.id, getErr)
		}
		if got.Depth != tc.wantDepth {
			t.Errorf("node %q Depth = %d, want %d", tc.id, got.Depth, tc.wantDepth)
		}
		if got.ParentID != tc.parentID {
			t.Errorf("node %q ParentID = %q, want %q", tc.id, got.ParentID, tc.parentID)
		}
		if depthOf(got.Path) != got.Depth {
			t.Errorf("node %q: path %q says depth %d but the row says %d",
				tc.id, got.Path, depthOf(got.Path), got.Depth)
		}
		if !strings.HasPrefix(got.Path, subtreePrefix(south.Path)) {
			t.Errorf("node %q Path = %q, want it beneath %q", tc.id, got.Path, south.Path)
		}
	}

	// The whole subtree is reachable from the new parent, and the old parent
	// keeps nothing behind.
	subtree, err := tree.Subtree(ctx, south.ID)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}
	assertIDSet(t, subtree, []string{south.ID, north.ID, store.ID, room.ID})
}

func TestTreeService_Move_IntoOwnSubtree_ReturnsCycleNotAllowed(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	tests := []struct {
		name     string
		nodeID   string
		parentID string
	}{
		{name: "into itself", nodeID: north.ID, parentID: north.ID},
		{name: "into its direct child", nodeID: north.ID, parentID: store.ID},
		{name: "into a deeper descendant", nodeID: north.ID, parentID: room.ID},
		{name: "the root into any node, since every node descends from it", nodeID: root.ID, parentID: store.ID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tree.Move(ctx, tc.nodeID, tc.parentID)
			if err == nil {
				t.Fatalf("Move(%q, %q) succeeded, want ErrCycleNotAllowed", tc.nodeID, tc.parentID)
			}
			assertCode(t, err, ErrCycleNotAllowed.Code)
		})
	}

	// Nothing was written by any of the rejected calls.
	got, err := tree.Get(ctx, north.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ParentID != root.ID || got.Path != buildPath(root.Path, north.ID) {
		t.Errorf("a rejected move changed the node: ParentID=%q Path=%q", got.ParentID, got.Path)
	}
}

func TestTreeService_Move_UnknownTarget_ReturnsParentNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")

	_, err := tree.Move(ctx, north.ID, "nope")
	if err == nil {
		t.Fatal("Move onto an unknown parent succeeded")
	}
	assertCode(t, err, ErrParentNotFound.Code)
}

// TestTreeService_Move_TargetInAnotherTenant_ReturnsParentNotFound proves a
// move cannot be used to smuggle a subtree across a tenant boundary: the
// target lookup is tenant-scoped, so another tenant's node simply does not
// exist from here.
func TestTreeService_Move_TargetInAnotherTenant_ReturnsParentNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctxA := tenantCtx("tenant-a")

	rootB := mustCreateRoot(t, tree, tenantCtx("tenant-b"), "Other Tenant")
	rootA := mustCreateRoot(t, tree, ctxA, "Acme Dental")
	north := mustCreateChild(t, tree, ctxA, rootA.ID, "North Region")

	_, err := tree.Move(ctxA, north.ID, rootB.ID)
	if err == nil {
		t.Fatal("Move onto another tenant's node succeeded")
	}
	assertCode(t, err, ErrParentNotFound.Code)
}

func TestTreeService_Move_DuplicateNameAtTarget_ReturnsDuplicateSiblingName(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")
	mustCreateChild(t, tree, ctx, south.ID, "North Region")

	_, err := tree.Move(ctx, north.ID, south.ID)
	if err == nil {
		t.Fatal("Move onto a parent that already has that name succeeded")
	}
	assertCode(t, err, ErrDuplicateSiblingName.Code)
}

// TestTreeService_Move_SubtreeWouldExceedMaxDepth_ReturnsMaxDepthExceeded
// checks the bound against the DEEPEST node in the moved subtree, not the
// moved node itself: moving a shallow node with a deep subtree is what
// actually overflows.
func TestTreeService_Move_SubtreeWouldExceedMaxDepth_ReturnsMaxDepthExceeded(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")

	// Branch A: a chain from depth 1 down to maxDepth-1, leaving exactly one
	// level of headroom.
	branch := mustCreateChild(t, tree, ctx, root.ID, "branch")
	node := branch
	for depth := 2; depth <= maxDepth-1; depth++ {
		node = mustCreateChild(t, tree, ctx, node.ID, fmt.Sprintf("chain-%d", depth))
	}
	deepest := node

	// Branch B: a single node at depth 1. Moving branch A beneath it adds one
	// level to every node in it, pushing "deepest" to exactly maxDepth.
	sibling := mustCreateChild(t, tree, ctx, root.ID, "sibling")

	if _, err := tree.Move(ctx, branch.ID, sibling.ID); err != nil {
		t.Fatalf("a move landing exactly on maxDepth must be allowed: %v", err)
	}
	reloaded, err := tree.Get(ctx, deepest.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Depth != maxDepth {
		t.Fatalf("deepest node Depth after the move = %d, want %d", reloaded.Depth, maxDepth)
	}

	// One more level down is refused, and refused because of the SUBTREE's
	// depth: the moved node itself would land at depth 3, well inside the
	// bound.
	deeper := mustCreateChild(t, tree, ctx, sibling.ID, "deeper")
	_, err = tree.Move(ctx, branch.ID, deeper.ID)
	if err == nil {
		t.Fatal("a move overflowing maxDepth succeeded")
	}
	assertCode(t, err, ErrMaxDepthExceeded.Code)

	// The rejected move left every row untouched.
	after, err := tree.Get(ctx, deepest.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Depth != maxDepth || after.Path != reloaded.Path {
		t.Errorf("a rejected move modified the tree: Depth=%d Path=%q", after.Depth, after.Path)
	}
}

func TestTreeService_Move_ToItsCurrentParent_IsANoOp(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")

	got, err := tree.Move(ctx, north.ID, root.ID)
	if err != nil {
		t.Fatalf("moving a node to its current parent failed: %v", err)
	}
	if got.Path != north.Path || got.Depth != north.Depth {
		t.Errorf("a no-op move changed the node: Path=%q Depth=%d", got.Path, got.Depth)
	}
}

// TestTreeService_Move_SiblingSharingAnIDPrefix_IsNotDraggedAlong is the
// adversarial case the trailing separator exists for. Ids "1a" and "1aa"
// would share a string prefix without it, so the sibling's subtree would be
// silently rewritten along with the moved one.
func TestTreeService_Move_SiblingSharingAnIDPrefix_IsNotDraggedAlong(t *testing.T) {
	db := newTestDB(t)
	tree := NewTreeService(db)
	repo := NewRepository(db)
	ctx := tenantCtx("tenant-a")

	// Hex-only ids, per the alphabet path.go pins: "1a" and "1aa" are the
	// adversarial pair, "0" is the root, "3" the leaf under the short id and
	// "2" the move target.
	ids := []string{"0", "1a", "1aa", "3", "2"}
	i := 0
	tree.newID = func() string {
		id := ids[i]
		i++
		return id
	}

	root := mustCreateRoot(t, tree, ctx, "root")
	shortID := mustCreateChild(t, tree, ctx, root.ID, "short")
	longID := mustCreateChild(t, tree, ctx, root.ID, "long")
	mustCreateChild(t, tree, ctx, shortID.ID, "under-short")
	target := mustCreateChild(t, tree, ctx, root.ID, "target")

	if shortID.ID != "1a" || longID.ID != "1aa" {
		t.Fatalf("fixture ids are %q and %q, want 1a and 1aa", shortID.ID, longID.ID)
	}

	if _, err := tree.Move(ctx, shortID.ID, target.ID); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// The prefix-sharing sibling stayed exactly where it was.
	stayed, err := repo.FindByID(ctx, longID.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if stayed.Path != longID.Path || stayed.ParentID != root.ID || stayed.Depth != longID.Depth {
		t.Errorf("the prefix-sharing sibling was dragged along: Path=%q ParentID=%q Depth=%d",
			stayed.Path, stayed.ParentID, stayed.Depth)
	}
}

func TestTreeService_Delete_Leaf(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")

	if err := tree.Delete(ctx, north.ID, false); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := tree.Get(ctx, north.ID); err == nil {
		t.Fatal("the deleted node is still readable")
	}
}

// TestTreeService_Delete_WithChildren_NoCascade_ReturnsNodeHasChildren also
// covers the rollback that makes the operation safe: the DELETE statement is
// issued and only then found to have matched more than one row, so the whole
// transaction must be rolled back. Every node still being readable afterwards
// is what proves it was.
func TestTreeService_Delete_WithChildren_NoCascade_ReturnsNodeHasChildren(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	err := tree.Delete(ctx, north.ID, false)
	if err == nil {
		t.Fatal("deleting a node with children without cascade succeeded")
	}
	assertCode(t, err, ErrNodeHasChildren.Code)

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	// Two descendants, not one direct child: the count reports everything
	// that would have been removed.
	if got := appErr.Params["descendant_count"]; got != int64(2) {
		t.Errorf("descendant_count = %v (%T), want int64(2)", got, got)
	}

	// The statement rolled back: nothing was removed, and -- the point of the
	// rule -- no child was re-parented to the grandparent, which would
	// silently widen every member's scope beneath it.
	for _, id := range []string{north.ID, store.ID, room.ID} {
		if _, getErr := tree.Get(ctx, id); getErr != nil {
			t.Errorf("node %q was removed by a rejected delete: %v", id, getErr)
		}
	}
	child, err := tree.Get(ctx, store.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if child.ParentID != north.ID {
		t.Errorf("child was re-parented to %q; orphan re-parenting is deliberately not done", child.ParentID)
	}
}

func TestTreeService_Delete_WithCascade_RemovesTheWholeSubtree(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	if err := tree.Delete(ctx, north.ID, true); err != nil {
		t.Fatalf("Delete cascade: %v", err)
	}

	for _, id := range []string{north.ID, store.ID, room.ID} {
		if _, err := tree.Get(ctx, id); err == nil {
			t.Errorf("node %q survived a cascading delete", id)
		}
	}
	// The untouched sibling branch and the root are still there.
	remaining, err := tree.Subtree(ctx, root.ID)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}
	assertIDSet(t, remaining, []string{root.ID, south.ID})
}

func TestTreeService_Delete_Root_ReturnsRootNotDeletable(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")

	for _, cascade := range []bool{false, true} {
		err := tree.Delete(ctx, root.ID, cascade)
		if err == nil {
			t.Fatalf("deleting the root with cascade=%v succeeded", cascade)
		}
		assertCode(t, err, ErrRootNotDeletable.Code)
	}
	if _, err := tree.Get(ctx, root.ID); err != nil {
		t.Errorf("the root was removed anyway: %v", err)
	}
}

func TestTreeService_Delete_UnknownNode_ReturnsNodeNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")
	mustCreateRoot(t, tree, ctx, "Acme Dental")

	err := tree.Delete(ctx, "nope", true)
	if err == nil {
		t.Fatal("deleting an unknown node succeeded")
	}
	assertCode(t, err, ErrNodeNotFound.Code)
}

// TestTreeService_Delete_Cascade_LeavesOtherTenantsAlone proves a cascading
// delete stops at the tenant boundary.
//
// Note what this test can and cannot construct. Node ids are globally unique
// (org_nodes' primary key is id alone -- see model.go), and a path is built
// from ids, so two tenants can never hold byte-identical VALID paths: the
// adversarial "same path string, different tenant" case therefore cannot be
// built through TreeService at all, and is exercised one layer down, against
// hand-seeded rows, by TestRepository_deleteSubtree and
// TestRepository_subtree_OtherTenantWithIdenticalPaths_IsNotReturned.
func TestTreeService_Delete_Cascade_LeavesOtherTenantsAlone(t *testing.T) {
	tree := newTestTree(t)
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	rootA := mustCreateRoot(t, tree, ctxA, "Acme Dental")
	branchA := mustCreateChild(t, tree, ctxA, rootA.ID, "North Region")
	mustCreateChild(t, tree, ctxA, branchA.ID, "Store 7")

	rootB := mustCreateRoot(t, tree, ctxB, "Other Tenant")
	branchB := mustCreateChild(t, tree, ctxB, rootB.ID, "North Region")
	leafB := mustCreateChild(t, tree, ctxB, branchB.ID, "Store 7")

	if err := tree.Delete(ctxA, branchA.ID, true); err != nil {
		t.Fatalf("Delete cascade: %v", err)
	}

	remainingA, err := tree.Subtree(ctxA, rootA.ID)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}
	assertIDSet(t, remainingA, []string{rootA.ID})

	survivorsB, err := tree.Subtree(ctxB, rootB.ID)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}
	assertIDSet(t, survivorsB, []string{rootB.ID, branchB.ID, leafB.ID})
}

func TestTreeService_Ancestors(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	t.Run("root first, self excluded", func(t *testing.T) {
		got, err := tree.Ancestors(ctx, room.ID)
		if err != nil {
			t.Fatalf("Ancestors: %v", err)
		}
		want := []string{root.ID, north.ID, store.ID}
		if len(got) != len(want) {
			t.Fatalf("Ancestors = %v, want %v", idsOf(got), want)
		}
		for i, id := range want {
			if got[i].ID != id {
				t.Fatalf("Ancestors = %v, want %v in that order", idsOf(got), want)
			}
		}
	})

	t.Run("the root has none", func(t *testing.T) {
		got, err := tree.Ancestors(ctx, root.ID)
		if err != nil {
			t.Fatalf("Ancestors: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Ancestors(root) = %v, want none", idsOf(got))
		}
	})

	t.Run("an unknown node reports node_not_found", func(t *testing.T) {
		_, err := tree.Ancestors(ctx, "nope")
		if err == nil {
			t.Fatal("Ancestors of an unknown node succeeded")
		}
		assertCode(t, err, ErrNodeNotFound.Code)
	})
}

// TestTreeService_DescendantsAndSubtree_WideAndDeep exercises a shape that
// is both wide and deep at once, so an off-by-one in the prefix or an
// accidental depth filter shows up as a missing or extra branch.
func TestTreeService_DescendantsAndSubtree_WideAndDeep(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")

	const branches, leavesPerBranch = 4, 3
	var wantAll []string
	var wantUnderFirst []string
	var firstBranchID string
	for b := 0; b < branches; b++ {
		branch := mustCreateChild(t, tree, ctx, root.ID, fmt.Sprintf("region-%d", b))
		wantAll = append(wantAll, branch.ID)
		if b == 0 {
			firstBranchID = branch.ID
		}
		for l := 0; l < leavesPerBranch; l++ {
			leaf := mustCreateChild(t, tree, ctx, branch.ID, fmt.Sprintf("store-%d-%d", b, l))
			wantAll = append(wantAll, leaf.ID)
			if b == 0 {
				wantUnderFirst = append(wantUnderFirst, leaf.ID)
			}
		}
	}

	t.Run("descendants of the root are every node but the root", func(t *testing.T) {
		got, err := tree.Descendants(ctx, root.ID)
		if err != nil {
			t.Fatalf("Descendants: %v", err)
		}
		assertIDSet(t, got, wantAll)
	})

	t.Run("subtree of the root includes the root", func(t *testing.T) {
		got, err := tree.Subtree(ctx, root.ID)
		if err != nil {
			t.Fatalf("Subtree: %v", err)
		}
		assertIDSet(t, got, append([]string{root.ID}, wantAll...))
	})

	t.Run("one branch sees only its own leaves", func(t *testing.T) {
		got, err := tree.Descendants(ctx, firstBranchID)
		if err != nil {
			t.Fatalf("Descendants: %v", err)
		}
		assertIDSet(t, got, wantUnderFirst)
	})

	t.Run("a leaf has no descendants but is its own subtree", func(t *testing.T) {
		leafID := wantUnderFirst[0]
		descendants, err := tree.Descendants(ctx, leafID)
		if err != nil {
			t.Fatalf("Descendants: %v", err)
		}
		if len(descendants) != 0 {
			t.Errorf("Descendants(leaf) = %v, want none", idsOf(descendants))
		}
		subtree, err := tree.Subtree(ctx, leafID)
		if err != nil {
			t.Fatalf("Subtree: %v", err)
		}
		assertIDSet(t, subtree, []string{leafID})
	})
}

func TestTreeService_Children(t *testing.T) {
	tree := newTestTree(t)
	ctx := tenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")
	mustCreateChild(t, tree, ctx, north.ID, "Store 7")

	t.Run("direct children only", func(t *testing.T) {
		got, err := tree.Children(ctx, root.ID)
		if err != nil {
			t.Fatalf("Children: %v", err)
		}
		assertIDSet(t, got, []string{north.ID, south.ID})
	})

	t.Run("an unknown node reports node_not_found rather than an empty list", func(t *testing.T) {
		_, err := tree.Children(ctx, "nope")
		if err == nil {
			t.Fatal("Children of an unknown node succeeded")
		}
		assertCode(t, err, ErrNodeNotFound.Code)
	})
}

// TestTreeService_Get_OtherTenantsNode_ReturnsNodeNotFound pins that every
// read path is tenant-scoped, and that a cross-tenant id is indistinguishable
// from a nonexistent one.
func TestTreeService_Get_OtherTenantsNode_ReturnsNodeNotFound(t *testing.T) {
	tree := newTestTree(t)
	rootB := mustCreateRoot(t, tree, tenantCtx("tenant-b"), "Other Tenant")

	_, err := tree.Get(tenantCtx("tenant-a"), rootB.ID)
	if err == nil {
		t.Fatal("reading another tenant's node succeeded")
	}
	assertCode(t, err, ErrNodeNotFound.Code)
}

// TestTreeService_NoTenantContext_EveryOperationFailsClosed proves the whole
// service fails closed on an unscoped context. A tree operation that quietly
// worked without a tenant would be the exact shape of a cross-tenant leak.
func TestTreeService_NoTenantContext_EveryOperationFailsClosed(t *testing.T) {
	tree := newTestTree(t)
	seeded := mustCreateRoot(t, tree, tenantCtx("tenant-a"), "Acme Dental")
	ctx := context.Background()

	operations := []struct {
		name string
		run  func() error
	}{
		{name: "CreateRoot", run: func() error { _, err := tree.CreateRoot(ctx, "X", "group"); return err }},
		{name: "CreateChild", run: func() error { _, err := tree.CreateChild(ctx, seeded.ID, "X", "store"); return err }},
		{name: "Get", run: func() error { _, err := tree.Get(ctx, seeded.ID); return err }},
		{name: "Root", run: func() error { _, err := tree.Root(ctx); return err }},
		{name: "Children", run: func() error { _, err := tree.Children(ctx, seeded.ID); return err }},
		{name: "Rename", run: func() error { _, err := tree.Rename(ctx, seeded.ID, "X"); return err }},
		{name: "Move", run: func() error { _, err := tree.Move(ctx, seeded.ID, seeded.ID); return err }},
		{name: "Delete", run: func() error { return tree.Delete(ctx, seeded.ID, true) }},
		{name: "Ancestors", run: func() error { _, err := tree.Ancestors(ctx, seeded.ID); return err }},
		{name: "Descendants", run: func() error { _, err := tree.Descendants(ctx, seeded.ID); return err }},
		{name: "Subtree", run: func() error { _, err := tree.Subtree(ctx, seeded.ID); return err }},
	}
	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			if err := op.run(); err == nil {
				t.Fatalf("%s succeeded on a context carrying no tenant", op.name)
			}
		})
	}

	// And nothing was written along the way.
	nodes, err := NewRepository(tree.repo.db).List(tenantCtx("tenant-a"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertIDSet(t, nodes, []string{seeded.ID})
}

// TestTreeService_TenantIsNeverATreeServiceParameter is a structural guard,
// not a behavioural one: every exported TreeService method must take the
// tenant from the context alone. A parameter through which a caller could
// name a tenant would violate the API rule that tenant_id is never accepted
// from the request.
func TestTreeService_TenantIsNeverATreeServiceParameter(t *testing.T) {
	tree := newTestTree(t)
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	rootA := mustCreateRoot(t, tree, ctxA, "Acme Dental")

	// The only way to reach tenant-a's node is a tenant-a context. Setting
	// the tenant on a struct handed to the service cannot change that: the
	// repository overwrites it from the context on every write.
	forged := OrgNode{
		ID:          "forged",
		TenantModel: dbkit.TenantModel{TenantID: "tenant-a"},
		Path:        "/forged/",
		Depth:       rootDepth,
		Name:        "forged",
	}
	if err := NewRepository(tree.repo.db).Create(ctxB, &forged); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if pkgcore.TenantID(forged.TenantID) != "tenant-b" {
		t.Errorf("a forged TenantID survived Create: got %q, want tenant-b", forged.TenantID)
	}
	if _, err := tree.Get(ctxB, rootA.ID); err == nil {
		t.Error("tenant-b reached tenant-a's root")
	}
}
