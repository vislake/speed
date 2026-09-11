package org

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org/internal/testutil"
	"github.com/vislake/speed/go/org/migrations"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/testkit"
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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

	mustCreateRoot(t, tree, ctx, "Acme Dental")

	_, err := tree.CreateRoot(ctx, "Second Root", "group")
	if err == nil {
		t.Fatal("a second CreateRoot succeeded, want ErrRootAlreadyExists")
	}
	assertCode(t, err, ErrRootAlreadyExists.Code)
}

// TestTreeService_CreateRoot_ConcurrentRaces_ExactlyOneRootSurvives is the
// regression proof for the "two differently-named roots" half of the
// single-root rule: CreateRoot's single-root check is a Go-level pre-check
// (findRoot, then the insert) backed by a partial unique index on root-ness
// (migrations/{sqlite,postgres}/0007_single_root.sql) that admits at most
// one row per tenant with the empty-string parent sentinel. Without the
// index, two concurrent CreateRoot calls for one tenant -- both with
// DIFFERENT names, which the sibling-name unique index does not catch --
// could each read "no root yet" and both insert, landing a tenant with two
// roots and silently breaking every invariant that reasons from "every
// node descends from the single root" (Move's cycle check above all).
// CreateRoot translates a lost race against the index into the identical
// ErrRootAlreadyExists its own pre-check reports.
//
// # Deterministic
//
// A second connection holds an open write transaction on the org_nodes file
// (a touch of another tenant's root row -- on SQLite any writer holds the
// whole file). Each racing CreateRoot's findRoot is a read, which proceeds
// while the holder's write transaction is open, so both observe "no root"
// before either has written; each call's insert then parks behind the
// holder. The holder is released only after a fixed margin, so both inserts
// execute strictly after both reads, against the committed state -- the
// exact interleaving only the index can arbitrate. The index admits the
// first insert and refuses the second with the coded org.root_already_exists.
func TestTreeService_CreateRoot_ConcurrentRaces_ExactlyOneRootSurvives(t *testing.T) {
	ctxA := testkit.TenantCtx("tenant-a")
	ctxB := testkit.TenantCtx("tenant-b")

	dsn := filepath.Join(t.TempDir(), "createroot-race.sqlite")
	db1, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	db2, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open (holder connection): %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{db1, db2} {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
	})
	testutil.Migrate(t, db1, dbkit.DialectSQLite, moduleName, migrations.FS)

	tree := newTestTreeOn(t, db1)
	// A root for tenant-b, so the holder connection has a live row to touch
	// (any org_nodes write takes the file's write lock on SQLite, but the
	// touch must match a row to be a genuine lock acquisition).
	bRoot := mustCreateRoot(t, tree, ctxB, "other tenant root")

	// The holder: touch tenant-b's root row on db2 and hold the transaction
	// open until release -- the same holder rig the delete-race tests use.
	held := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- dbkit.WithTenantSession(ctxB, db2, func(tx *gorm.DB) error {
			res := tx.
				Where("id = ?", bRoot.ID).
				Where("deleted_at IS NULL").
				Select("DeletedBy").
				Updates(&OrgNode{DeletedBy: ""})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("holder touch matched %d rows, want 1", res.RowsAffected)
			}
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case err = <-holderErr:
		t.Fatalf("holder failed before holding its write open: %v", err)
	}

	// Both racers start only now that the file's write lock is held: each
	// findRoot read sees no tenant-a root, and each insert parks behind the
	// holder, so both inserts run after both reads once it commits.
	results := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, createErr := tree.CreateRoot(ctxA, "Race Root One", "group")
		results <- createErr
	}()
	go func() {
		<-start
		_, createErr := tree.CreateRoot(ctxA, "Race Root Two", "group")
		results <- createErr
	}()
	close(start)
	time.Sleep(200 * time.Millisecond) // both findRoot reads have certainly landed by now
	close(release)
	if err = <-holderErr; err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	var successes, alreadyExists int
	for i := 0; i < 2; i++ {
		switch err := <-results; {
		case err == nil:
			successes++
		case apperr.HasCode(err, ErrRootAlreadyExists.Code):
			alreadyExists++
		default:
			t.Fatalf("racing CreateRoot failed with %v, want success or org.root_already_exists", err)
		}
	}
	if successes != 1 || alreadyExists != 1 {
		t.Fatalf("two racing CreateRoot calls gave %d successes and %d root-already-exists, want exactly 1 and 1 -- a tenant must never end up with two roots",
			successes, alreadyExists)
	}

	// The database-level proof, independent of which racer won: exactly one
	// root row exists for tenant-a.
	var roots []OrgNode
	if err := dbkit.WithTenantSession(ctxA, db1, func(tx *gorm.DB) error {
		return tx.Where("parent_id = ?", "").Find(&roots).Error
	}); err != nil {
		t.Fatalf("counting tenant-a root rows: %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("tenant-a has %d root rows after two racing CreateRoot calls, want exactly 1", len(roots))
	}
}

// TestTreeService_CreateRoot_SecondRootAndRootUnderRoot_AreRefused is the
// regression proof for the "root under another root" half: with two roots
// on one tenant -- a state only a bypass of CreateRoot can construct, which
// is exactly how this test plants it, through the raw repository -- Move of
// one root under the other must not SUCCEED (the second root is not a
// descendant of the first, so the cycle check never fires, and the
// sibling-name pre-check looks at the target's children, which are none).
// The schema refuses the state: the partial unique index on root-ness makes
// the direct second-root insert itself fail with a duplicated key, so no
// code path -- TreeService or a host's direct repository write -- can ever
// put a second root in a position to be moved under. The assertion holds on
// both shapes: either the database refused the second root outright, or the
// tree layer had to refuse the root-under-root move because the plant
// succeeded.
func TestTreeService_CreateRoot_SecondRootAndRootUnderRoot_AreRefused(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "root")

	// Plant a second, differently-named root row DIRECTLY through the
	// repository, bypassing CreateRoot's pre-check -- the only way a second
	// root can come into existence at all, and the exact state the DB index
	// must refuse. The planted id stays inside the hex-hyphen alphabet so the
	// refusal under test is the tree invariant, never validatePath's id
	// grammar.
	planted := OrgNode{ID: "beef0001", ParentID: "", Path: "/beef0001/", Depth: 0, Name: "other root", Kind: "group"}
	plantErr := tree.repo.Create(ctx, &planted)

	switch {
	case plantErr == nil:
		// The schema admitted a second root: the tree layer must at least
		// refuse to move the real root under it.
		if _, err := tree.Move(ctx, root.ID, planted.ID); err == nil {
			t.Fatal("moving the tenant root under a second root was not refused -- root-under-root is a corrupt tree, and the move must answer a coded error")
		}
	case errors.Is(plantErr, gorm.ErrDuplicatedKey):
		// The database itself arbitrates the single-root invariant: the
		// second root never existed, so root-under-root is structurally
		// impossible.
	default:
		t.Fatalf("planting a second root row = %v, want success (pre-fix) or a duplicated-key refusal (post-fix)", plantErr)
	}

	// Whichever shape held, the tenant still has exactly its one original
	// root, still the tenant root.
	got, err := tree.Root(ctx)
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got.ID != root.ID || !got.IsRoot() {
		t.Fatalf("tenant root = %q (IsRoot %v), want the original root %q", got.ID, got.IsRoot(), root.ID)
	}
}

// TestTreeService_CreateRoot_PerTenant_EachTenantGetsItsOwn proves the
// one-root rule is per tenant, not global: a second tenant creating its own
// root must not collide with the first tenant's.
func TestTreeService_CreateRoot_PerTenant_EachTenantGetsItsOwn(t *testing.T) {
	tree := newTestTree(t)

	rootA := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-a"), "Acme Dental")
	rootB := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-b"), "Acme Dental")

	if rootA.ID == rootB.ID {
		t.Fatalf("both tenants' roots share id %q", rootA.ID)
	}
	got, err := tree.Root(testkit.TenantCtx("tenant-a"))
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
			_, err := tree.CreateRoot(testkit.TenantCtx("tenant-a"), tc.input, "group")
			if err == nil {
				t.Fatalf("CreateRoot(%q) succeeded, want %s", tc.input, tc.wantCode)
			}
			assertCode(t, err, tc.wantCode)
		})
	}
}

// TestTreeService_EnsureRoot_CreatesOnceThenReturnsTheStoredRoot pins the
// idempotence contract every boot-time caller leans on: the first call is
// what creates, every later call -- with whatever name and kind it passes
// -- returns the root already there, unchanged.
func TestTreeService_EnsureRoot_CreatesOnceThenReturnsTheStoredRoot(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	created, err := tree.EnsureRoot(ctx, "Demo Tenant", "group")
	if err != nil {
		t.Fatalf("first EnsureRoot: %v", err)
	}
	if created.Name != "Demo Tenant" || created.Kind != "group" || created.ParentID != "" {
		t.Fatalf("first EnsureRoot produced %+v, want a root named Demo Tenant of kind group", created)
	}

	// The name and kind of a repeat are consulted only when a root must be
	// created; a tenant that already has one keeps its stored values.
	again, err := tree.EnsureRoot(ctx, "Renamed Tenant", "workspace")
	if err != nil {
		t.Fatalf("second EnsureRoot: %v", err)
	}
	if again.ID != created.ID {
		t.Fatalf("second EnsureRoot root id = %q, want the first root %q", again.ID, created.ID)
	}
	if again.Name != "Demo Tenant" || again.Kind != "group" {
		t.Errorf("second EnsureRoot returned %+v, want the stored root unchanged (Demo Tenant/group)", again)
	}

	// Each tenant still gets exactly its own root.
	other, err := tree.EnsureRoot(testkit.TenantCtx("tenant-b"), "Other Tenant", "group")
	if err != nil {
		t.Fatalf("EnsureRoot(tenant-b): %v", err)
	}
	if other.ID == created.ID {
		t.Error("tenant-b's root is tenant-a's root")
	}
}

func TestTreeService_EnsureRoot_InvalidName_IsRefusedAtTheCreate(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	if _, err := tree.EnsureRoot(ctx, "", "group"); !apperr.HasCode(err, ErrNodeNameRequired.Code) {
		t.Errorf("EnsureRoot(empty name) error = %v, want org.node_name_required", err)
	}
}

func TestTreeService_CreateChild(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

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
			ctx := testkit.TenantCtx("tenant-a")
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
	rootB := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-b"), "Other Tenant")
	mustCreateRoot(t, tree, testkit.TenantCtx("tenant-a"), "Acme Dental")

	_, err := tree.CreateChild(testkit.TenantCtx("tenant-a"), rootB.ID, "Child", "store")
	if err == nil {
		t.Fatal("CreateChild under another tenant's node succeeded")
	}
	assertCode(t, err, ErrParentNotFound.Code)
}

func TestTreeService_CreateChild_DuplicateSiblingName_ReturnsDuplicateSiblingName(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

	seedNode(t, repo, ctx, OrgNode{ID: "bad", Path: "no-leading-separator", Depth: 0, Name: "corrupt"})

	_, err := tree.CreateChild(ctx, "bad", "Child", "store")
	if err == nil {
		t.Fatal("CreateChild under a corrupt parent succeeded, want ErrInternal")
	}
	assertCode(t, err, ErrInternal.Code)
}

func TestTreeService_Rename(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")
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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctxA := testkit.TenantCtx("tenant-a")

	rootB := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-b"), "Other Tenant")
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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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

// TestTreeService_Delete_WithCascade_MarksEveryLevelSoftDeletedAndRestorable
// is the proof for the cascade's mark-delete semantics: a 3-level subtree
// (north -> store -> room) cascade-deleted
// in one call leaves every one of the three levels invisible to Get (proven
// above by TestTreeService_Delete_WithCascade_RemovesTheWholeSubtree already)
// AND individually restorable, with its original data -- parent, path,
// depth, name -- intact. "Restorable" is the property a real physical DELETE
// could never have: a physical DELETE leaves no row
// for Restore to find, and the pre-mark-delete implementation answered
// dbkit.ErrRecordNotFound on every restore.
func TestTreeService_Delete_WithCascade_MarksEveryLevelSoftDeletedAndRestorable(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")
	room := mustCreateChild(t, tree, ctx, store.ID, "Room 1")

	if err := tree.Delete(ctx, north.ID, true); err != nil {
		t.Fatalf("Delete cascade: %v", err)
	}
	for _, id := range []string{north.ID, store.ID, room.ID} {
		if _, err := tree.Get(ctx, id); err == nil {
			t.Fatalf("node %q survived a cascading delete", id)
		}
	}

	for _, want := range []*OrgNode{north, store, room} {
		restored, err := tree.Restore(ctx, want.ID)
		if err != nil {
			t.Fatalf("Restore(%q): %v", want.Name, err)
		}
		if restored.ParentID != want.ParentID || restored.Path != want.Path ||
			restored.Depth != want.Depth || restored.Name != want.Name {
			t.Errorf("Restore(%q) = %+v, want the original parent/path/depth/name of %+v",
				want.Name, restored, want)
		}
		// The row is visible to an ordinary Get again, exactly as if it had
		// never been deleted.
		got, err := tree.Get(ctx, want.ID)
		if err != nil {
			t.Fatalf("Get(%q) after Restore: %v", want.Name, err)
		}
		if got.Name != want.Name {
			t.Errorf("Get(%q) after Restore = %+v, want the original row back", want.Name, got)
		}
	}
}

func TestTreeService_Delete_Root_ReturnsRootNotDeletable(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")
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
	ctxA := testkit.TenantCtx("tenant-a")
	ctxB := testkit.TenantCtx("tenant-b")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	ctx := testkit.TenantCtx("tenant-a")

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
	rootB := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-b"), "Other Tenant")

	_, err := tree.Get(testkit.TenantCtx("tenant-a"), rootB.ID)
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
	seeded := mustCreateRoot(t, tree, testkit.TenantCtx("tenant-a"), "Acme Dental")
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
	nodes, err := NewRepository(tree.repo.db).List(testkit.TenantCtx("tenant-a"))
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
	ctxA := testkit.TenantCtx("tenant-a")
	ctxB := testkit.TenantCtx("tenant-b")

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

// TestTreeService_Delete_WithMembers_ReturnsNodeHasMembers pins the guard
// this block adds. Without it a cascading delete would leave memberships
// pointing at rows that no longer exist, and a dangling membership is a
// person whose data scope can no longer be resolved.
func TestTreeService_Delete_WithMembers_ReturnsNodeHasMembers(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := testkit.TenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	chair, err := m.Tree().CreateChild(ctx, left.ID, "chair 1", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add(owner): %v", err)
	}
	if _, err := m.Members().Add(ctx, "u-chair", chair.ID); err != nil {
		t.Fatalf("Add(chair): %v", err)
	}

	// The member sits BELOW the node being deleted, so a check that only
	// looked at the node itself would miss them.
	for _, cascade := range []bool{false, true} {
		if err := m.Tree().Delete(ctx, left.ID, cascade); !apperr.HasCode(err, ErrNodeHasMembers.Code) {
			t.Errorf("Delete(cascade=%t) error = %v, want org.node_has_members", cascade, err)
		}
	}
	if _, err := m.Tree().Get(ctx, chair.ID); err != nil {
		t.Errorf("the refused delete removed the subtree anyway: %v", err)
	}

	// An empty branch is still deletable: the guard is about members, not
	// about structure.
	if err := m.Tree().Delete(ctx, right.ID, false); err != nil {
		t.Errorf("Delete(empty branch): %v", err)
	}

	// And once the member is moved out of the way, the delete proceeds.
	if _, err := m.Tree().Move(ctx, chair.ID, root.ID); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if err := m.Tree().Delete(ctx, left.ID, false); err != nil {
		t.Errorf("Delete after moving the member out: %v", err)
	}
}

// TestTreeService_Delete_OtherTenantsMembers_DoNotBlockADelete pins that the
// guard is tenant-scoped like everything else: another tenant's membership on
// a same-named node id cannot make this tenant's node undeletable.
func TestTreeService_Delete_OtherTenantsMembers_DoNotBlockADelete(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := testkit.TenantCtx("tenant-a")
	_, left, _ := seedTree(t, m.Tree(), ctx)

	seedMembership(t, m.Members().Repository(), testkit.TenantCtx("tenant-b"), Membership{
		ID: "20000000-0000-4000-8000-000000000020", UserID: "u-theirs",
		NodeID: left.ID, Status: MembershipStatusActive,
	})
	if err := m.Tree().Delete(ctx, left.ID, false); err != nil {
		t.Errorf("Delete = %v, want success -- another tenant's membership must not block it", err)
	}
}

// TestTreeService_Delete_WithoutARosterWired_SkipsTheGuard pins that a
// TreeService built on its own -- no roster beside it, so no membership can
// be orphaned -- keeps working exactly as it did before the guard existed.
func TestTreeService_Delete_WithoutARosterWired_SkipsTheGuard(t *testing.T) {
	tree := NewTreeService(newTestDB(t))
	ctx := testkit.TenantCtx("tenant-a")

	root, err := tree.CreateRoot(ctx, "group", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	child, err := tree.CreateChild(ctx, root.ID, "store", "store")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if tree.members != nil {
		t.Fatal("a bare TreeService has a member guard; the test proves nothing")
	}
	if err := tree.Delete(ctx, child.ID, false); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// TestTreeService_MaxDepth_IsPerServiceNotGlobal pins the WithMaxDepth
// override: the bound lives on the service, so two hosts in one process can
// disagree about it without one reconfiguring the other.
func TestTreeService_MaxDepth_IsPerServiceNotGlobal(t *testing.T) {
	shallow := NewModule(newTestDB(t), WithEmailIndexer(newTestEmailIndexer(t)), WithMaxDepth(1))
	ctx := testkit.TenantCtx("tenant-a")

	root, err := shallow.Tree().CreateRoot(ctx, "group", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	child, err := shallow.Tree().CreateChild(ctx, root.ID, "store", "store")
	if err != nil {
		t.Fatalf("CreateChild at depth 1: %v", err)
	}
	_, err = shallow.Tree().CreateChild(ctx, child.ID, "chair", "room")
	if !apperr.HasCode(err, ErrMaxDepthExceeded.Code) {
		t.Fatalf("CreateChild at depth 2 error = %v, want org.max_depth_exceeded", err)
	}
	if got := errParam(t, err, "max_depth"); got != 1 {
		t.Errorf("max_depth parameter = %v, want the configured 1", got)
	}

	// The package default is untouched by the override above.
	if maxDepth == 1 {
		t.Fatal("the package default happens to equal the override; the test proves nothing")
	}
	deep := NewTreeService(newTestDB(t))
	if deep.maxDepth != maxDepth {
		t.Errorf("a second service's maxDepth = %d, want the package default %d", deep.maxDepth, maxDepth)
	}
}

// TestTreeService_Delete_ThenCreateChild_SameSiblingName_Succeeds is the
// proof that uq_org_nodes_sibling_name's WHERE deleted_at IS NULL
// partial-index equivalent
// (migrations/{sqlite,postgres}/0004_add_soft_delete.sql) actually frees a
// mark-deleted node's (parent_id, name) slot for reuse. Against a
// full unique index this Create would fail with
// ErrDuplicateSiblingName -- the functional regression the partial index
// prevents.
func TestTreeService_Delete_ThenCreateChild_SameSiblingName_Succeeds(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	original := mustCreateChild(t, tree, ctx, root.ID, "North Region")

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
	// index still enforces uniqueness among LIVE rows, it only stopped
	// counting the mark-deleted one.
	if _, err = tree.Get(ctx, recreated.ID); err != nil {
		t.Errorf("Get(recreated): %v", err)
	}
	_, err = tree.CreateChild(ctx, root.ID, "North Region", "region")
	if !apperr.HasCode(err, ErrDuplicateSiblingName.Code) {
		t.Errorf("a second live sibling with the same name error = %v, want org.duplicate_sibling_name", err)
	}
}

// TestTreeService_Restore_UnknownID_ReturnsNodeNotFound covers the id half of
// Restore's collapsed not-found signal: an id nothing ever created.
func TestTreeService_Restore_UnknownID_ReturnsNodeNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")
	mustCreateRoot(t, tree, ctx, "Acme Dental")

	_, err := tree.Restore(ctx, "nope")
	if !apperr.HasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Restore(unknown id) error = %v, want org.node_not_found", err)
	}
}

// TestTreeService_Restore_LiveNode_ReturnsNodeNotFound covers the other half:
// an id that exists but was never deleted has nothing for Restore to undo,
// and Restore does not silently treat that as a no-op success.
func TestTreeService_Restore_LiveNode_ReturnsNodeNotFound(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")
	root := mustCreateRoot(t, tree, ctx, "Acme Dental")

	_, err := tree.Restore(ctx, root.ID)
	if !apperr.HasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Restore(live node) error = %v, want org.node_not_found", err)
	}
}

// TestTreeService_Restore_IsNotCascading pins the per-node design
// decision: restoring an ancestor never
// resurrects its cascade-deleted descendants. The caller restores each node
// explicitly by id.
func TestTreeService_Restore_IsNotCascading(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")

	if err := tree.Delete(ctx, north.ID, true); err != nil {
		t.Fatalf("Delete cascade: %v", err)
	}

	if _, err := tree.Restore(ctx, north.ID); err != nil {
		t.Fatalf("Restore(north): %v", err)
	}
	if _, err := tree.Get(ctx, north.ID); err != nil {
		t.Errorf("Get(north) after its own Restore: %v", err)
	}
	// store was cascade-deleted alongside north, but restoring north must not
	// have restored it too.
	if _, err := tree.Get(ctx, store.ID); err == nil {
		t.Error("Restore(north) also resurrected store; restore must be per-node, not cascading")
	}
	// store is independently restorable, exactly as the design decision
	// documents.
	if _, err := tree.Restore(ctx, store.ID); err != nil {
		t.Errorf("Restore(store) after Restore(north): %v, want success", err)
	}
}

// TestTreeService_Restore_DeadParent_RefusesRestore reproduces the tree
// corruption a bare, bottom-up Restore would produce: cascade-delete
// root -> north -> store, then restore ONLY store, leaving north still
// mark-deleted. Without the ErrRestoreParentNotLive guard such a call
// would succeed and leave store reachable from Subtree(root) (the prefix
// scan does not care that north is invisible) yet unreachable from
// Get(north) or any Children()-based walk, and would let a caller
// CreateChild beneath it --
// exactly the "path disagrees with the parent chain" state path.go calls
// corrupt, not supported. Restore refuses instead.
func TestTreeService_Restore_DeadParent_RefusesRestore(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	store := mustCreateChild(t, tree, ctx, north.ID, "Store 7")

	if err := tree.Delete(ctx, north.ID, true); err != nil {
		t.Fatalf("Delete cascade: %v", err)
	}

	_, err := tree.Restore(ctx, store.ID)
	if !apperr.HasCode(err, ErrRestoreParentNotLive.Code) {
		t.Fatalf("Restore(store) with north still dead error = %v, want org.restore_parent_not_live", err)
	}

	// The refusal must be a pure read: store stays exactly as dead as it
	// was, never half-restored.
	if _, getErr := tree.Get(ctx, store.ID); getErr == nil {
		t.Fatal("Restore(store) mutated store despite refusing, store is now visible")
	}

	// Restoring the ancestor first, then the descendant -- the order the
	// dead-parent refusal makes mandatory -- must still succeed.
	if _, err := tree.Restore(ctx, north.ID); err != nil {
		t.Fatalf("Restore(north): %v", err)
	}
	if _, err := tree.Restore(ctx, store.ID); err != nil {
		t.Fatalf("Restore(store) after Restore(north): %v, want success", err)
	}
}

// TestTreeService_Restore_AfterAncestorMoved_ReexpressesUnderTheCurrentParent
// is the restore-path regression proof: the four-step sequence --
// cascade-delete a subtree, restore its ancestor, move that ancestor, restore
// a descendant -- must not leave the descendant LIVE with a stale materialized
// Path naming the ancestor's OLD location. Move's rewrite carries
// WHERE deleted_at IS NULL on every row it touches, so a mark-deleted
// descendant is invisible to it and never has its path rebased when its live
// ancestor moves; Restore then cleared deleted_at/deleted_by without
// recomputing Path/Depth, resurrecting a row whose Path disagrees with its
// own ParentID chain -- the state path.go's own doc comment and model.go call
// "corrupt, not a supported state". The corruption has subtree-grant
// consequences in BOTH directions: the old branch's prefix scan still
// surfaces the row (a scope anchored under the old location covers it), while
// the real parent's subtree no longer does (a scope anchored at the restored
// ancestor misses its own live child). Restore therefore re-expresses the
// row it brings back under its live parent's CURRENT path -- the parent is
// locked and read inside the same transaction as the write -- never resurrect
// the stored, stale one.
func TestTreeService_Restore_AfterAncestorMoved_ReexpressesUnderTheCurrentParent(t *testing.T) {
	db := newTestDB(t)
	tree := newTestTreeOn(t, db)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	north := mustCreateChild(t, tree, ctx, root.ID, "North Region")
	hub := mustCreateChild(t, tree, ctx, north.ID, "Regional Hub")
	store := mustCreateChild(t, tree, ctx, hub.ID, "Store 7")
	south := mustCreateChild(t, tree, ctx, root.ID, "South Region")

	// Step 1: cascade-delete hub's subtree -- hub and store both go dead.
	if err := tree.Delete(ctx, hub.ID, true); err != nil {
		t.Fatalf("Delete(hub) cascade: %v", err)
	}
	// Step 2: restore the ancestor alone (per-node, never cascading).
	if _, err := tree.Restore(ctx, hub.ID); err != nil {
		t.Fatalf("Restore(hub): %v", err)
	}
	// Step 3: move the restored ancestor onto a new branch. The still-
	// mark-deleted store keeps its stored Path, which names hub's old
	// position.
	if _, err := tree.Move(ctx, hub.ID, south.ID); err != nil {
		t.Fatalf("Move(hub, south): %v", err)
	}
	// Step 4: restore the descendant.
	restored, err := tree.Restore(ctx, store.ID)
	if err != nil {
		t.Fatalf("Restore(store): %v", err)
	}

	hubCurrent, err := tree.Get(ctx, hub.ID)
	if err != nil {
		t.Fatalf("Get(hub) after the move: %v", err)
	}
	if want := buildPath(hubCurrent.Path, store.ID); restored.Path != want {
		t.Errorf("restored store Path = %q, want %q (hub's CURRENT path + its own id) -- the row must be re-expressed under hub's new position under %q, not resurrected with the stale one",
			restored.Path, want, south.ID)
	}
	if want := hubCurrent.Depth + 1; restored.Depth != want {
		t.Errorf("restored store Depth = %d, want %d", restored.Depth, want)
	}

	// The real parent's subtree now covers the restored row...
	hubSubtree, err := tree.Subtree(ctx, hub.ID)
	if err != nil {
		t.Fatalf("Subtree(hub): %v", err)
	}
	if !slices.Contains(nodeIDs(hubSubtree), store.ID) {
		t.Errorf("Subtree(hub) = %v, want the restored store %q included", nodeIDs(hubSubtree), store.ID)
	}
	// ...and the OLD branch's subtree no longer does: a scope anchored under
	// the old location must stop covering the restored row the moment its
	// ancestor moved away.
	northSubtree, err := tree.Subtree(ctx, north.ID)
	if err != nil {
		t.Fatalf("Subtree(north): %v", err)
	}
	if slices.Contains(nodeIDs(northSubtree), store.ID) {
		t.Errorf("Subtree(north) = %v still covers the restored store %q; a stale path must not survive restore", nodeIDs(northSubtree), store.ID)
	}

	assertNoOrphans(t, db, ctx, "four-step restore sequence")
}

// TestTreeService_Restore_WouldLandBeyondMaxDepth_Refused pins the guard that
// keeps Restore's re-expression inside the same depth envelope every other
// tree write obeys. A parent can end up at maxDepth with a deleted child the
// move that put it there never counted: Move's depth check runs over the
// live subtree only, and a mark-deleted descendant is invisible to it.
// Re-expressing that child on restore would land it at maxDepth+1 -- a live
// row no CreateChild under the same parent could ever have produced -- so
// Restore must refuse with the identical ErrMaxDepthExceeded rather than
// resurrect a row deeper than the module's own bound permits.
func TestTreeService_Restore_WouldLandBeyondMaxDepth_Refused(t *testing.T) {
	tree := newTestTree(t) // default maxDepth = 8
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "Acme Dental")
	// An 8-deep chain under the root: the deepest level maxDepth admits.
	chain := make([]*OrgNode, 0, 8)
	parent := root
	for i := 0; i < 8; i++ {
		parent = mustCreateChild(t, tree, ctx, parent.ID, fmt.Sprintf("level %d", i+1))
		chain = append(chain, parent)
	}
	leaf := chain[7] // depth 8 == maxDepth
	leafParent := chain[6]

	// A second, 7-deep chain provides the target that lets leaf's parent
	// move down one more level while the leaf is dead.
	target := root
	for i := 0; i < 7; i++ {
		target = mustCreateChild(t, tree, ctx, target.ID, fmt.Sprintf("other %d", i+1))
	}

	// Kill the leaf, then move its live parent to depth 8 -- a move Move's
	// own live-subtree depth check approves, since the dead leaf is
	// invisible to it.
	if err := tree.Delete(ctx, leaf.ID, false); err != nil {
		t.Fatalf("Delete(leaf): %v", err)
	}
	if _, err := tree.Move(ctx, leafParent.ID, target.ID); err != nil {
		t.Fatalf("Move(leafParent, %q): %v", target.ID, err)
	}

	// Restoring the leaf would re-express it at depth 9 under its now-depth-8
	// parent: refuse, exactly as CreateChild under that same parent would.
	_, err := tree.Restore(ctx, leaf.ID)
	if !apperr.HasCode(err, ErrMaxDepthExceeded.Code) {
		t.Fatalf("Restore(leaf) error = %v, want org.max_depth_exceeded", err)
	}
	// The refusal must be a pure read: leaf stays exactly as dead as it was.
	if _, getErr := tree.Get(ctx, leaf.ID); getErr == nil {
		t.Fatal("Restore(leaf) resurrected the row despite refusing")
	}
}

// TestTreeService_Restore_RestoreMoveDeleteRace_RestoresUnderCurrentParent
// is the restore-race regression proof: a competing sequence --
// Restore(child), Move(child, newParent), Delete(child) again -- racing a
// second Restore(child) that read the row BEFORE that sequence committed
// must not land child LIVE with ParentID = newParent but a materialized Path
// naming the OLD parent. restoreNodeTx writes Path/Depth conditioned only on
// id and deleted_at IS NOT NULL, never touching or checking ParentID, so its
// write can match the re-deleted row and resurrect it under a path its own
// ParentID contradicts -- the state path.go calls "corrupt, not a supported
// state", with subtree-scope consequences in both directions and a two-way
// data-scope mismatch once the row feeds rbac through ScopeService.Path.
// The stale ingredient is Restore's initial findByIDIncludingDeleted read,
// taken outside its transaction and reused by every retry: nothing
// serializes two concurrent Restores before either enters its write
// transaction, so the row can be restored, moved and re-deleted entirely
// between that read and the write that follows it. Two concurrent PURE
// restores cannot expose this (the second one's write matches nothing once
// the first made the row live); the sandwich Move and Delete are required.
//
// # Why a deterministic gate rather than racing rounds
//
// The window sits between two phases of the SAME call -- Restore's initial
// read and its write transaction -- with no blocking point between them an
// outside test could use to force the interleaving. Goroutine scheduling and
// SQLite's busy-handler timing decide who wins each file-lock handoff, and
// the corrupt outcome needs the competing path to win three consecutive
// handoffs: its own Restore locks the SAME parent the parked call later
// locks, so a first-handoff loss fails closed instead (the module's own
// "Atomicity" section in tree.go argues that half). The stress-round shape
// of this file's other TestTreeService_Concurrent* tests therefore exposes
// this only probabilistically, never on demand. Restore accordingly carries
// one test-only pause point -- the restoreGate field, invoked between the
// read and the retry, inert unless a test sets it -- so this test parks the
// racing Restore after its read, commits the competing sequence through the
// real service methods, then resumes it and asserts the landed row: the
// resumed call's in-transaction re-read must see the current parent, lock
// it, and re-express the row under it -- never under the stale parent its
// initial read captured.
func TestTreeService_Restore_RestoreMoveDeleteRace_RestoresUnderCurrentParent(t *testing.T) {
	db := newTestDB(t)
	tree := newTestTreeOn(t, db)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "root")
	p1 := mustCreateChild(t, tree, ctx, root.ID, "p1")
	p2 := mustCreateChild(t, tree, ctx, root.ID, "p2")
	child := mustCreateChild(t, tree, ctx, p1.ID, "child")
	if err := tree.Delete(ctx, child.ID, false); err != nil {
		t.Fatalf("Delete(child): %v", err)
	}

	// Park one Restore between its initial read and its write transaction.
	// Only the FIRST invocation parks (the competing path's own later
	// Restore passes straight through): a sync.Once cannot express that here,
	// because Once holds its mutex while the parked function blocks.
	readDone := make(chan struct{})
	resume := make(chan struct{})
	tree.restoreGate = func() {
		select {
		case <-readDone:
			// already parked once -- not the call this test is holding
		default:
			close(readDone)
			<-resume
		}
	}

	type restoreResult struct {
		node *OrgNode
		err  error
	}
	result := make(chan restoreResult, 1)
	go func() {
		node, err := tree.Restore(ctx, child.ID)
		result <- restoreResult{node: node, err: err}
	}()
	<-readDone // the parked Restore has read child (deleted, under p1).

	// The competing path, through the real service methods: restore child,
	// move it under p2, delete it again.
	if _, err := tree.Restore(ctx, child.ID); err != nil {
		t.Fatalf("competing Restore(child): %v", err)
	}
	if _, err := tree.Move(ctx, child.ID, p2.ID); err != nil {
		t.Fatalf("competing Move(child, p2): %v", err)
	}
	if err := tree.Delete(ctx, child.ID, false); err != nil {
		t.Fatalf("competing Delete(child): %v", err)
	}
	// The premise, pinned: child is soft-deleted again, now under p2.
	row, err := tree.repo.findByIDIncludingDeleted(ctx, child.ID)
	if err != nil {
		t.Fatalf("read child row after the competing sequence: %v", err)
	}
	if row.DeletedAt == nil || row.ParentID != p2.ID {
		t.Fatalf("premise broken: child = %+v, want soft-deleted under %s", row, p2.ID)
	}

	close(resume)

	var got restoreResult
	select {
	case got = <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("parked Restore never resumed once its gate opened")
	}
	if got.err != nil {
		t.Fatalf("parked Restore failed: %v", got.err)
	}

	p2Current, err := tree.Get(ctx, p2.ID)
	if err != nil {
		t.Fatalf("Get(p2): %v", err)
	}
	if got.node.ParentID != p2.ID {
		t.Errorf("restored child ParentID = %q, want %q", got.node.ParentID, p2.ID)
	}
	wantPath := buildPath(p2Current.Path, child.ID)
	if got.node.Path != wantPath {
		t.Errorf("restored child Path = %q, want %q (p2's CURRENT path + its own id): the row must be re-expressed under the parent the in-transaction re-read saw, not the stale parent the read-before-the-race named",
			got.node.Path, wantPath)
	}
	assertNoOrphans(t, db, ctx, "restore raced by restore->move->delete")
}

// assertNoOrphans is the tree invariant every one of this file's concurrent
// stress tests re-checks after each round: every currently-live node's
// stored ParentID either is the empty-root sentinel or names another
// currently-live node, and every live node's Path is exactly its parent's
// Path with its own id appended -- the two invariants path.go's own doc
// comment calls "corrupt, not a supported state" when violated, and which
// the two concurrent-write hazards -- a child landing under a soft-deleted
// parent, and a moved subtree whose Path and ParentID chain disagree --
// each name as their own violation.
//
// It reads every row once, through the ordinary soft-delete auto-scope
// (never Unscoped), so only currently-live rows are ever checked -- exactly
// the shape a real consumer's own consistency job would run.
func assertNoOrphans(t *testing.T, db *gorm.DB, ctx context.Context, label string) {
	t.Helper()
	var nodes []OrgNode
	if err := dbkit.WithTenantSession(ctx, db, func(tx *gorm.DB) error {
		return tx.Order("depth, id").Find(&nodes).Error
	}); err != nil {
		t.Fatalf("%s: list nodes: %v", label, err)
	}
	byID := make(map[string]OrgNode, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, n := range nodes {
		if n.ParentID == "" {
			continue // root
		}
		parent, ok := byID[n.ParentID]
		if !ok {
			t.Fatalf("%s: node %s (path %s) has ParentID %s, which does not exist among live nodes -- orphan",
				label, n.ID, n.Path, n.ParentID)
		}
		want := buildPath(parent.Path, n.ID)
		if n.Path != want {
			t.Fatalf("%s: node %s has Path %s, want %s (parent %s's Path %s + its own id) -- Path/ParentID chain disagree",
				label, n.ID, n.Path, want, parent.ID, parent.Path)
		}
	}
}

// TestTreeService_ConcurrentCreateChildAndDelete_NeverOrphansAChild is the
// concurrent-create-vs-delete regression proof.
//
// # Why this is a stress test, not a deterministic interleaving
//
// The window this closes sits between two DIFFERENT transactions' single
// statements (deleteLeaf's own UPDATE and CreateChild's now-atomic parent
// lock), not between two statements of the SAME call this test could pause
// midway through with a hook -- there is no seam in either method's real,
// shipped code a test could deterministically suspend without adding a
// test-only instrumentation point to production code, which this test
// deliberately avoids. This test instead runs many racing iterations of
// the two operations for real, over a real file-backed SQLite database, and
// asserts the invariant (assertNoOrphans) after every single iteration:
// without lockLiveNode's shared row lock the two outcomes below could BOTH
// occur in the same iteration (CreateChild succeeding while Delete also
// succeeds), landing a live child under a soft-deleted parent; with it, at
// most one of the two operations can ever win a given iteration.
func TestTreeService_ConcurrentCreateChildAndDelete_NeverOrphansAChild(t *testing.T) {
	const rounds = 200
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		parent := mustCreateChild(t, tree, ctx, root.ID, "parent")

		var wg sync.WaitGroup
		var createErr, deleteErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, createErr = tree.CreateChild(ctx, parent.ID, "child", "store")
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteErr = tree.Delete(ctx, parent.ID, false)
		}()
		close(start)
		wg.Wait()

		if createErr == nil && deleteErr == nil {
			t.Fatalf("round %d: CreateChild AND Delete(parent) both succeeded -- "+
				"the child now lives under a soft-deleted parent", round)
		}
		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentMoveAndMove_TreeInvariantHolds is the
// concurrent-move-vs-move regression proof: two overlapping Moves --
// swapping two
// subtrees' positions under each other's own current parent -- racing for
// real over many rounds, each followed by assertNoOrphans. Move's rewrite
// runs as one transaction with lockLiveNode locking the moved node and
// the target parent before either is trusted, so two overlapping Moves
// serialize on whichever row they lock first rather than mixing per-row
// last-writer-wins; a real PostgreSQL deadlock between two such calls
// locking in opposite orders is handled by withRetry (concurrency.go) and
// separately proven in integration_test/postgres_concurrency_test.go.
func TestTreeService_ConcurrentMoveAndMove_TreeInvariantHolds(t *testing.T) {
	const rounds = 100
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		a := mustCreateChild(t, tree, ctx, root.ID, "a")
		b := mustCreateChild(t, tree, ctx, root.ID, "b")
		mustCreateChild(t, tree, ctx, a.ID, "a-child")
		mustCreateChild(t, tree, ctx, b.ID, "b-child")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Move(ctx, a.ID, b.ID) // may lose to a real deadlock/refusal; that is fine
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Move(ctx, b.ID, a.ID)
		}()
		close(start)
		wg.Wait()

		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentMoveAndCreateChild_TreeInvariantHolds is the
// second required pairing: a concurrent CreateChild targeting the exact
// node another goroutine is Move-ing. Both now lock that node's row through
// the same lockLiveNode primitive (tree.go's CreateChild locks the PARENT
// it is creating under; Move locks the NODE it is moving -- the same row
// when the two calls target each other), so whichever runs second re-reads
// the row's current Path after the lock clears rather than building on a
// stale one.
func TestTreeService_ConcurrentMoveAndCreateChild_TreeInvariantHolds(t *testing.T) {
	const rounds = 150
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		a := mustCreateChild(t, tree, ctx, root.ID, "a")
		b := mustCreateChild(t, tree, ctx, root.ID, "b")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Move(ctx, a.ID, b.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.CreateChild(ctx, a.ID, "late-child", "store")
		}()
		close(start)
		wg.Wait()

		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentMoveAndDelete_TreeInvariantHolds is the third
// required pairing: Move racing a cascade Delete of the node it is moving.
// Move's lockLiveNode on the moved node and deleteSubtree's own single
// UPDATE now contend for the identical row, so exactly one of the two wins
// each round; assertNoOrphans catches either a stranded (never soft-
// deleted) escapee or a partially-rewritten moved subtree.
func TestTreeService_ConcurrentMoveAndDelete_TreeInvariantHolds(t *testing.T) {
	const rounds = 150
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		a := mustCreateChild(t, tree, ctx, root.ID, "a")
		b := mustCreateChild(t, tree, ctx, root.ID, "b")
		mustCreateChild(t, tree, ctx, a.ID, "a-child")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Move(ctx, a.ID, b.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = tree.Delete(ctx, a.ID, true)
		}()
		close(start)
		wg.Wait()

		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentRestoreAndCascadeDelete_NeverLandsOnADeadParent
// is the restore-vs-cascade-delete regression proof: Restore of a node
// racing a cascade Delete of that same node's live ancestor, over many
// rounds, each followed by assertNoOrphans -- which catches exactly the
// corrupt state this pairing can produce, a restored node left LIVE under a
// parent that ends up soft-deleted.
//
// # Two legitimate outcomes, and the one that would not be
//
// Restore's parent-lock (lockLiveNode on existing.ParentID, held until
// Restore's own write commits) and deleteSubtree's own single UPDATE
// contend for the identical parent row, so the two calls always serialize
// on it -- there is no ordering in which Restore's write and Delete's
// bulk-touch of that row are both in flight unlocked at once. That
// serialization still allows either of two outcomes, both correct:
//
//   - Delete's cascade reaches (and soft-deletes) the parent BEFORE
//     Restore's lock attempt: Restore's own lockLiveNode then finds the
//     parent already dead and refuses (ErrRestoreParentNotLive) -- the
//     child stays exactly as dead as it was.
//   - Restore's lock succeeds first and its whole transaction (lock +
//     write) commits before Delete's blocked write resumes: Delete's
//     cascade then re-evaluates its own "path LIKE prefix%" scan against
//     the NOW-current state once unblocked (SQLite does not operate on a
//     stale snapshot after waiting out another writer's lock) and finds
//     the just-restored child live again -- so the cascade sweeps it up
//     too, soft-deleting parent AND child together. Restore reports
//     success (it did, genuinely, un-delete the row, if only for the
//     instant before the racing cascade caught up with it) and the cascade
//     also reports success; the final state has BOTH rows dead, which is
//     consistent, not corrupt.
//
// The corrupt state this test exists to rule out -- the child ending up
// LIVE while its parent ends up dead -- is not reachable under either
// ordering, which is exactly what assertNoOrphans checks for on every round:
// it is
// the sole assertion here, deliberately, rather than a check on which of
// the two calls "won".
func TestTreeService_ConcurrentRestoreAndCascadeDelete_NeverLandsOnADeadParent(t *testing.T) {
	const rounds = 150
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		parent := mustCreateChild(t, tree, ctx, root.ID, "parent")
		child := mustCreateChild(t, tree, ctx, parent.ID, "child")

		// Soft-delete the child alone first (a leaf delete), so the race is
		// specifically "restore the child" vs "cascade-delete its still-live
		// parent".
		if err := tree.Delete(ctx, child.ID, false); err != nil {
			t.Fatalf("round %d: seed delete(child): %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Restore(ctx, child.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = tree.Delete(ctx, parent.ID, true)
		}()
		close(start)
		wg.Wait()

		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentMoveAndCreateChild_InteriorDescendant_TreeInvariantHolds
// is the interior-descendant pairing the sibling test above never actually
// covers: that test's CreateChild targets the exact
// node being Moved (a.ID), a row Move already locks via lockLiveNode -- it
// never exercises a CreateChild targeting an INTERIOR DESCENDANT of the
// moved subtree, a row Move's rewrite touches only through its own subtree
// scan, never through an up-front lockLiveNode call the way the moved node
// and the new parent are.
//
// If Move's subtree scan were one plain, unlocked Find, a concurrent
// CreateChild(a-child, ...) could insert its new row -- bound to
// a-child's OLD, pre-move Path -- entirely within the gap between that scan
// and Move's own later per-row rewrite of a-child, so the new grandchild
// would simply never appear in the row set Move rewrote and would keep a
// stale Path forever. lockSubtree (repository.go) closes it by locking every
// row of the subtree, not merely the two endpoints, before trusting the set
// is complete.
//
// # Honest limits, exactly like this file's other four concurrent stress
// tests
//
// SQLite's coarse, whole-file locking does not give this test the same
// reproduction odds a real PostgreSQL server's READ COMMITTED window does;
// this SQLite form is kept for symmetry with this file's other
// TestTreeService_Concurrent* tests and as a light smoke test, while
// integration_test/postgres_concurrency_test.go's
// TestConcurrentMoveAndCreateChild_InteriorDescendant_TreeInvariantHolds_Postgres
// is the tier that actually forces the real window this test names.
func TestTreeService_ConcurrentMoveAndCreateChild_InteriorDescendant_TreeInvariantHolds(t *testing.T) {
	const rounds = 150
	for round := 0; round < rounds; round++ {
		db := newTestDB(t)
		tree := newTestTreeOn(t, db)
		ctx := testkit.TenantCtx("tenant-a")
		root := mustCreateRoot(t, tree, ctx, "root")
		a := mustCreateChild(t, tree, ctx, root.ID, "a")
		b := mustCreateChild(t, tree, ctx, root.ID, "b")
		aChild := mustCreateChild(t, tree, ctx, a.ID, "a-child")

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.Move(ctx, a.ID, b.ID)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.CreateChild(ctx, aChild.ID, "late-grandchild", "store")
		}()
		close(start)
		wg.Wait()

		assertNoOrphans(t, db, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestTreeService_ConcurrentDeleteAndMemberAdd_NeverDanglesAMembership is
// the delete-vs-member-add regression proof: TreeService.Delete's members
// guard runs as an in-transaction check inside the delete's own lock. If it
// ran as its own separate, unlocked read entirely BEFORE
// deleteLeaf/deleteSubtree ever opened their own transaction, a concurrent
// MemberService.Add binding a fresh membership to the node about to be
// deleted could land in the gap between that read and the cascade's own
// commit, leaving an active membership whose NodeID names a row that is no
// longer visible.
//
// Unlike the interior-descendant Move case above, this one is a wide-open
// TOCTOU gap with no locking on either side of the unguarded shape, so it
// reproduces reliably on plain SQLite. The lock makes both sides serialize
// on the identical lockLiveNode lock: tree.go's Delete runs the members
// check inside the same transaction that locks the node being deleted
// (memberGuardFor), and membership.go's MemberService.ensure takes that
// same lock before creating the membership, so every interleaving resolves
// to one of two consistent outcomes -- Add wins and the membership commits
// against a node Delete's own re-check then correctly refuses to remove, or
// Delete wins and Add's blocked lock attempt resumes to find the node
// mark-deleted and refuses with ErrNodeNotFound -- never both succeeding.
func TestTreeService_ConcurrentDeleteAndMemberAdd_NeverDanglesAMembership(t *testing.T) {
	const rounds = 200
	for round := 0; round < rounds; round++ {
		m, _ := newTestModule(t)
		ctx := testkit.TenantCtx("tenant-a")
		root, err := m.Tree().CreateRoot(ctx, "root", "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		branch, err := m.Tree().CreateChild(ctx, root.ID, "branch", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(branch): %v", round, err)
		}
		userID := fmt.Sprintf("u-race-%d", round)

		var wg sync.WaitGroup
		var addErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = m.Tree().Delete(ctx, branch.ID, false)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, addErr = m.Members().Add(ctx, userID, branch.ID)
		}()
		close(start)
		wg.Wait()

		if addErr != nil {
			continue // Delete won; Add correctly refused. Nothing to check.
		}
		membership, err := m.Members().Get(ctx, userID)
		if err != nil {
			t.Fatalf("round %d: Add reported success but Get failed: %v", round, err)
		}
		if _, err := m.Tree().Get(ctx, membership.NodeID); err != nil {
			t.Fatalf("round %d: membership %s is bound to node %s, which is no longer visible (%v) -- dangling membership",
				round, membership.ID, membership.NodeID, err)
		}
	}
}

// TestTreeService_ConcurrentRenameAndDelete_NeverResurrectsTheNode is the
// concurrent-rename-vs-delete regression proof. If Rename's write were a
// standalone full-field dbkit.Repository[OrgNode].Update following a
// separate read (s.Get) -- not a lockLiveNode transaction -- a concurrent
// soft-delete of the very node being renamed, landing between read and
// write, would be silently UNDONE. The Update is a full-field Save that
// writes whatever the caller's in-memory model holds back over the whole
// row, and the rename's model is a stale pre-delete snapshot: its DeletedAt
// is nil, so the Save would write the delete's committed mark
// (deleted_at/deleted_by) back to NULL, resurrecting a node the caller had
// just deleted.
//
// # Why this test is deterministic, unlike this file's other concurrent tests
//
// The window this closes sits between two statements of the SAME call --
// Rename's own read and its own write -- with no seam in shipped code a test
// could pause between, which is why the other concurrent proofs above are
// stress tests. But the resurrection does not need timing luck: it needs only the
// rename's READ to precede the delete's COMMIT, and the rename's WRITE to
// follow it. SQLite's own locking supplies that deterministically. A second
// connection executes the mark-delete and holds its transaction OPEN before
// the rename even starts: every reader (the rename's read, which sees the
// still-live committed state) is unaffected, while any writer of the file
// waits out busy_timeout behind the hold. The hold is released only after a
// fixed 200ms margin -- the same holder-then-release shape go/dbkit's own
// busy-timeout contention tests use -- so the rename's microsecond read has
// long since landed when the mark-delete commits, and the rename's blocked
// write then executes against the committed delete. On the unguarded shape
// that write would resurrect the row; with the rename's own lockLiveNode
// being the write that blocked, and
// once the delete commits it re-evaluates against the now-dead row, matches
// nothing and refuses with ErrNodeNotFound -- the row stays deleted, unnamed,
// untouched. (Had the rename started before the hold was established it
// could legitimately win the row lock first and complete before the delete --
// a valid serial order, rename then delete -- which is why this test starts
// it only after `held`.)
func TestTreeService_ConcurrentRenameAndDelete_NeverResurrectsTheNode(t *testing.T) {
	ctx := testkit.TenantCtx("tenant-a")

	// One file, two connections: db1 carries the tree; db2 plays the
	// concurrent deleter. dbkit.Open applies the busy_timeout pragma to every
	// connection of both, which is what lets the rename's write wait out the
	// hold below instead of failing immediately.
	dsn := filepath.Join(t.TempDir(), "rename-delete-race.sqlite")
	db1, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	db2, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open (deleter connection): %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{db1, db2} {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
	})
	testutil.Migrate(t, db1, dbkit.DialectSQLite, moduleName, migrations.FS)

	tree := newTestTreeOn(t, db1)
	root := mustCreateRoot(t, tree, ctx, "root")
	target := mustCreateChild(t, tree, ctx, root.ID, "original")

	// The concurrent deleter: mark-delete target's row on db2 and hold the
	// transaction open until release. The statement's own return establishes
	// the hold -- never timing luck -- exactly as in go/dbkit/dialect/sqlite's
	// dialect_sqlite_test.go rig (TestSQLiteBusyTimeout_ContendingWriterWaitsThenSucceeds).
	held := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- dbkit.WithTenantSession(ctx, db2, func(tx *gorm.DB) error {
			now := time.Now()
			res := tx.
				Where("id = ?", target.ID).
				Select("DeletedAt", "DeletedBy").
				Updates(&OrgNode{DeletedAt: &now, DeletedBy: "concurrent-deleter"})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("mark-delete matched %d rows, want 1", res.RowsAffected)
			}
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case err = <-holderErr:
		t.Fatalf("deleter failed before holding the mark-delete open: %v", err)
	}

	// The rename races the held delete: started only now that the delete
	// holds the file's write lock, its read of target still sees the live
	// pre-delete state, and its write cannot complete until the deleter
	// commits.
	renameDone := make(chan struct{})
	var renameErr error
	go func() {
		defer close(renameDone)
		_, renameErr = tree.Rename(ctx, target.ID, "renamed")
	}()
	time.Sleep(200 * time.Millisecond) // the rename's read has certainly landed by now
	close(release)
	<-renameDone
	if err = <-holderErr; err != nil {
		t.Fatalf("deleter commit: %v", err)
	}

	// The delete happened while the rename was in flight, so the rename must
	// have been refused and the node must still be gone. Without the lock, the
	// rename's stale full-field Save would have run over the committed
	// delete: the node would be live again and renamed with its delete mark
	// cleared.
	if _, getErr := tree.Get(ctx, target.ID); getErr == nil {
		t.Fatalf("node %q is visible again after a concurrent soft-delete of it -- the rename resurrected the deleted row", target.ID)
	}
	assertCode(t, renameErr, ErrNodeNotFound.Code)
	row, err := tree.repo.findByIDIncludingDeleted(ctx, target.ID)
	if err != nil {
		t.Fatalf("read back the soft-deleted row: %v", err)
	}
	if row.Name != "original" {
		t.Errorf("soft-deleted row Name = %q, want %q -- the rename wrote over a deleted row", row.Name, "original")
	}
	if row.DeletedAt == nil {
		t.Error("soft-deleted row DeletedAt = nil -- the rename cleared the delete mark")
	}
	if row.DeletedBy != "concurrent-deleter" {
		t.Errorf("soft-deleted row DeletedBy = %q, want %q", row.DeletedBy, "concurrent-deleter")
	}
}

// TestTreeService_Delete_CascadeRacingAMove_NeverSilentlyDeletesNothing is
// the cascade-vs-move regression proof. If Delete derived the cascade's path
// prefix from its own OUTER, unlocked s.Get read and passed that prefix down
// into deleteSubtree, whose transaction then locked nodeID and swept
// whatever the STALE prefix matched, a concurrent Move of the node
// committing between Delete's Get and deleteSubtree's lock would leave the
// node -- and its whole relocated subtree -- outside the stale prefix: the
// cascade's mark-delete UPDATE would match zero rows, deleteSubtree would
// report a clean (0, nil, nil), and Delete would publish org.node.deleted
// with an empty DeletedNodeIds and return success -- a silent 204 that
// deleted nothing.
//
// # Why this test is deterministic
//
// A second connection holds an open transaction that has already performed
// Move's own per-row conditional rewrites of the node and its subtree
// (uncommitted). Delete's outer Get is a read, which SQLite lets through
// while the holder's write transaction is open, so it observes the pre-move
// state; Delete's first write -- deleteSubtree's own lock on the node --
// then parks behind the holder's write lock until release. The holder is
// released only after a fixed margin, so the Get has certainly landed when
// the move commits and Delete's blocked write executes against the
// committed, post-move state: with a stale prefix the delete would match
// nothing and report success while the node stays live; with the prefix
// re-derived from the locked row's CURRENT path, the cascade removes the
// node at its new location and the node is genuinely gone. The delete
// therefore always reports one of the two legitimate outcomes -- the node
// deleted, or node_not_found -- never a silent zero-match.
func TestTreeService_Delete_CascadeRacingAMove_NeverSilentlyDeletesNothing(t *testing.T) {
	ctx := testkit.TenantCtx("tenant-a")

	dsn := filepath.Join(t.TempDir(), "delete-move-race.sqlite")
	db1, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	db2, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open (mover connection): %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{db1, db2} {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
	})
	testutil.Migrate(t, db1, dbkit.DialectSQLite, moduleName, migrations.FS)

	tree := newTestTreeOn(t, db1)
	root := mustCreateRoot(t, tree, ctx, "root")
	b := mustCreateChild(t, tree, ctx, root.ID, "b")
	a := mustCreateChild(t, tree, ctx, root.ID, "a")
	aChild := mustCreateChild(t, tree, ctx, a.ID, "a-child")

	// The concurrent mover: re-parent A (and A-child) under B with the exact
	// per-row conditional UPDATEs Move performs -- Select(Path, Depth,
	// ParentID), WHERE id AND deleted_at IS NULL, paths rebased the way
	// rebasePath does -- and hold the transaction open until release. The
	// statements' own completion closes `held`, never timing luck.
	newAPath := buildPath(b.Path, a.ID)
	held := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- dbkit.WithTenantSession(ctx, db2, func(tx *gorm.DB) error {
			res := tx.
				Where("id = ?", a.ID).
				Where("deleted_at IS NULL").
				Select("Path", "Depth", "ParentID").
				Updates(&OrgNode{Path: newAPath, Depth: depthOf(newAPath), ParentID: b.ID})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("move rewrite of %s matched %d rows, want 1", a.ID, res.RowsAffected)
			}
			resChild := tx.
				Where("id = ?", aChild.ID).
				Where("deleted_at IS NULL").
				Select("Path", "Depth").
				Updates(&OrgNode{Path: buildPath(newAPath, aChild.ID), Depth: depthOf(buildPath(newAPath, aChild.ID))})
			if resChild.Error != nil {
				return resChild.Error
			}
			if resChild.RowsAffected != 1 {
				return fmt.Errorf("move rewrite of %s matched %d rows, want 1", aChild.ID, resChild.RowsAffected)
			}
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case err = <-holderErr:
		t.Fatalf("mover failed before holding the move open: %v", err)
	}

	// The cascade races the held move: started only now that the move holds
	// the file's write lock, Delete's outer Get still sees the pre-move
	// state, and its first write cannot complete until the mover commits.
	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		deleteErr = tree.Delete(ctx, a.ID, true)
	}()
	time.Sleep(200 * time.Millisecond) // Delete's Get has certainly landed by now
	close(release)
	<-deleteDone
	if err = <-holderErr; err != nil {
		t.Fatalf("mover commit: %v", err)
	}

	if deleteErr != nil {
		t.Fatalf("Delete(A, cascade) = %v, want success (the delete may legitimately win or lose the race, but it must not fail)", deleteErr)
	}
	if _, getErr := tree.Get(ctx, a.ID); getErr == nil {
		t.Fatalf("node %q is still live after Delete(A, cascade=true) reported success -- the cascade matched zero rows under the pre-move prefix and silently deleted nothing", a.ID)
	}
	if _, getErr := tree.Get(ctx, aChild.ID); getErr == nil {
		t.Fatalf("descendant %q is still live after Delete(A, cascade=true) reported success", aChild.ID)
	}
}

// TestTreeService_Delete_CascadeOfAConcurrentlyDeletedNode_AnswersNodeNotFound
// is the vanished-node regression proof: deleteSubtree reports a vanished
// node as a
// clean (removed=0, nil error) and TreeService.Delete's cascade branch used
// to treat that as success -- publishing org.node.deleted with an empty
// DeletedNodeIds and returning nil -- where the non-cascade branch has
// always translated the identical situation into ErrNodeNotFound. The
// regression: deleting a node that a concurrent delete removed out from
// under the call must answer org.node_not_found, never success, and must
// never emit the empty-ids event.
//
// Deterministic for the same reason the vanished-node test above is: a
// second
// connection holds the mark-delete of A open (the identical
// Select(DeletedAt, DeletedBy) UPDATE deleteSubtree itself issues, held
// uncommitted). Delete's outer Get reads the still-live pre-delete state;
// its own first write parks behind the holder; release lets the concurrent
// delete commit, and Delete's resumed lock attempt fails to match the
// now-dead row. deleteSubtree reports removed=0 -- which the cascade branch
// must answer with ErrNodeNotFound instead of the success plus empty-ids
// event the unguarded shape would have produced.
func TestTreeService_Delete_CascadeOfAConcurrentlyDeletedNode_AnswersNodeNotFound(t *testing.T) {
	ctx := testkit.TenantCtx("tenant-a")

	dsn := filepath.Join(t.TempDir(), "delete-delete-race.sqlite")
	db1, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	db2, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open (deleter connection): %v", err)
	}
	t.Cleanup(func() {
		for _, db := range []*gorm.DB{db1, db2} {
			if sqlDB, dbErr := db.DB(); dbErr == nil {
				_ = sqlDB.Close()
			}
		}
	})
	testutil.Migrate(t, db1, dbkit.DialectSQLite, moduleName, migrations.FS)

	// The module wiring, so TreeService.Delete publishes onto the recording
	// bus the assertions below read -- an unwired tree's publishEvent is a
	// silent no-op (events.go), which would make the never-an-empty-ids-event
	// half of this regression vacuous.
	host := newTestHost(t)
	m := NewModule(db1,
		WithEmailIndexer(newTestEmailIndexer(t)),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	)
	m.attach(host)
	tree := m.Tree()

	root := mustCreateRoot(t, tree, ctx, "root")
	a := mustCreateChild(t, tree, ctx, root.ID, "a")

	// The concurrent deleter: mark-delete A's row on db2 and hold the
	// transaction open until release -- the same holder rig the deterministic
	// tests above use.
	held := make(chan struct{})
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- dbkit.WithTenantSession(ctx, db2, func(tx *gorm.DB) error {
			now := time.Now()
			res := tx.
				Where("id = ?", a.ID).
				Select("DeletedAt", "DeletedBy").
				Updates(&OrgNode{DeletedAt: &now, DeletedBy: "concurrent-deleter"})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("mark-delete matched %d rows, want 1", res.RowsAffected)
			}
			close(held)
			<-release
			return nil
		})
	}()

	select {
	case <-held:
	case err = <-holderErr:
		t.Fatalf("deleter failed before holding the mark-delete open: %v", err)
	}

	// The cascade races the held delete: its outer Get sees A live, and its
	// first write cannot complete until the deleter commits.
	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		deleteErr = tree.Delete(ctx, a.ID, true)
	}()
	time.Sleep(200 * time.Millisecond) // Delete's Get has certainly landed by now
	close(release)
	<-deleteDone
	if err = <-holderErr; err != nil {
		t.Fatalf("deleter commit: %v", err)
	}

	// The node was already gone by the time the cascade's lock succeeded: the
	// delete must answer node_not_found -- the identical signal the
	// non-cascade branch gives for matched == 0 -- never a silent success.
	assertCode(t, deleteErr, ErrNodeNotFound.Code)

	// And the success path was never taken, so no org.node.deleted event --
	// in particular none with an empty DeletedNodeIds -- may have been
	// published for this tenant.
	if evts := host.bus.events(EventNodeDeleted); len(evts) != 0 {
		t.Fatalf("Delete of a concurrently deleted node published %d org.node.deleted event(s); a delete that removed nothing must not announce one (pre-fix shape: success + empty-ids event)",
			len(evts))
	}
}

// TestTreeService_Restore_ReusedSiblingNameSlot_AnswersDuplicateSiblingName
// is the seat-reuse regression proof: the partial unique index on
// (tenant_id, parent_id, name) WHERE deleted_at IS NULL (0004_add_soft_delete.sql)
// deliberately lets a deleted node's sibling-name slot be reused by a fresh
// CreateChild -- the whole point of narrowing the index. Restoring the
// ORIGINAL, soft-deleted node then collides with the live replacement at the
// database, and TreeService.Restore would otherwise surface that collision
// as a bare gorm.ErrDuplicatedKey. The collision is the sibling-name rule enforced by
// the database (restore-vs-reuse, exactly the race CreateChild's own
// mapWriteError translation already covers in the other direction), so it
// must answer the same coded org.duplicate_sibling_name.
func TestTreeService_Restore_ReusedSiblingNameSlot_AnswersDuplicateSiblingName(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	root := mustCreateRoot(t, tree, ctx, "root")
	original := mustCreateChild(t, tree, ctx, root.ID, "same-name")
	if err := tree.Delete(ctx, original.ID, false); err != nil {
		t.Fatalf("Delete(original): %v", err)
	}
	replacement := mustCreateChild(t, tree, ctx, root.ID, "same-name")

	// Restoring the original row would put two live same-name siblings under
	// the same parent: the database refuses, and the service must translate.
	if _, err := tree.Restore(ctx, original.ID); !apperr.HasCode(err, ErrDuplicateSiblingName.Code) {
		t.Fatalf("Restore of a deleted node whose sibling-name slot was reused = %v, want the coded org.duplicate_sibling_name, not a bare database error", err)
	}

	// Nothing changed: the replacement is still the parent's one live
	// same-name child, and the original row is still soft-deleted.
	got, err := tree.Children(ctx, root.ID)
	if err != nil {
		t.Fatalf("Children after refused Restore: %v", err)
	}
	if len(got) != 1 || got[0].ID != replacement.ID {
		t.Fatalf("live children of root = %v, want exactly the replacement %q", idsOf(got), replacement.ID)
	}
}

// TestTreeService_CreateRoot_AfterSoftDeletedRoot_Succeeds is the
// soft-deleted-root-slot regression proof. uq_org_nodes_single_root shipped (0007_single_root.sql)
// scoped on parent_id = "" alone, so a mark-deleted root row still occupied
// the tenant's single root slot: once a host had removed a tenant's root
// through the exported Repository surface (TreeService.Delete refuses the
// root by design), every CreateRoot for that tenant collided with the
// invisible row -- translated into ErrRootAlreadyExists -- and the tenant
// had no way back to a tree. 0008_single_root_live.sql narrows the index to
// live rows only (WHERE deleted_at IS NULL, the same predicate 0004 applied
// to the sibling-name and membership indexes), and this test pins the
// behavior that narrowing exists to restore: a soft-deleted root frees the
// root slot immediately, while one LIVE root per tenant stays enforced.
func TestTreeService_CreateRoot_AfterSoftDeletedRoot_Succeeds(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")

	original := mustCreateRoot(t, tree, ctx, "Acme Dental")

	// A host removes the tenant root through the exported Repository surface.
	// This is the only path that can mark-delete a root: TreeService.Delete
	// itself refuses (ErrRootNotDeletable) by design, and dbkit's promoted
	// Repository.Delete is host-visible, which is exactly how the row got
	// into the state the regression starts from.
	if err := tree.Repository().Delete(ctx, original.ID); err != nil {
		t.Fatalf("soft-delete the root through the repository: %v", err)
	}
	if _, err := tree.Root(ctx); !apperr.HasCode(err, ErrNodeNotFound.Code) {
		t.Fatalf("Root() after the soft-delete = %v, want org.node_not_found (the mark-deleted root is hidden)", err)
	}

	// The regression: on the 0007 index this insert collided with the
	// soft-deleted row (ErrRootAlreadyExists, forever); on the 0008-narrowed
	// index the slot is free again.
	replacement, err := tree.CreateRoot(ctx, "Acme Dental Reborn", "group")
	if err != nil {
		t.Fatalf("CreateRoot after the tenant root was soft-deleted: %v, want success -- 0007's single-root index still counted the invisible row", err)
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

	// The narrowed index still enforces the invariant among LIVE rows: a
	// second live root for the same tenant is refused, however it is
	// attempted.
	if _, secondRootErr := tree.CreateRoot(ctx, "Another Root", "group"); !apperr.HasCode(secondRootErr, ErrRootAlreadyExists.Code) {
		t.Errorf("a second live root error = %v, want org.root_already_exists", secondRootErr)
	}

	// Restoring the original root now collides with the live replacement at
	// the database, and must answer the coded slot-taken error rather than a
	// bare database error -- the reuse of the freed root slot Restore's own
	// doc comment promises once the narrowed index is in place.
	if _, restoreErr := tree.Restore(ctx, original.ID); !apperr.HasCode(restoreErr, ErrDuplicateSiblingName.Code) {
		t.Errorf("Restore of the original root whose root slot was re-taken = %v, want the coded org.duplicate_sibling_name", restoreErr)
	}
	// The row the refused restore raced stays the tenant's live one.
	still, err := tree.Root(ctx)
	if err != nil {
		t.Fatalf("Root() after the refused restore: %v", err)
	}
	if still.ID != replacement.ID {
		t.Errorf("Root() = %q after the refused restore, want %q", still.ID, replacement.ID)
	}
}

// TestTreeService_Rename_ReturnsThePostWriteUpdatedAt is the
// post-write-updated_at regression proof for Rename: Rename returns the
// node as written
// the lock -- the PRE-write snapshot, whose UpdatedAt predates the rename's
// own UPDATE (gorm's autoUpdateTime stamps the row at write time). A caller
// that rendered that returned row (the handler's PATCH response does,
// verbatim) showed an updated_at older than the very next GET answers --
// the response and the next read disagreeing about when the write happened.
func TestTreeService_Rename_ReturnsThePostWriteUpdatedAt(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")
	root := mustCreateRoot(t, tree, ctx, "root")
	node := mustCreateChild(t, tree, ctx, root.ID, "before")

	renamed, err := tree.Rename(ctx, node.ID, "after")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	got, err := tree.Get(ctx, node.ID)
	if err != nil {
		t.Fatalf("Get after the rename: %v", err)
	}
	if !renamed.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("Rename returned updated_at %v but a subsequent Get reads %v -- the response must carry the post-write row, not the pre-write snapshot",
			renamed.UpdatedAt, got.UpdatedAt)
	}
	if renamed.UpdatedAt.Equal(node.UpdatedAt) {
		t.Errorf("Rename's returned updated_at %v equals the pre-rename value -- the rename never stamped the row it returned", renamed.UpdatedAt)
	}
}

// TestTreeService_Move_ReturnsThePostWriteUpdatedAt is the
// post-write-updated_at regression proof for Move, Rename's twin: Move
// returns the moved
// node as read under the lock, before the rewrite loop's own UPDATEs
// (updated_at stamped per row at write time), so the returned row's
// UpdatedAt disagreed with the value a subsequent Get reads back.
func TestTreeService_Move_ReturnsThePostWriteUpdatedAt(t *testing.T) {
	tree := newTestTree(t)
	ctx := testkit.TenantCtx("tenant-a")
	root := mustCreateRoot(t, tree, ctx, "root")
	a := mustCreateChild(t, tree, ctx, root.ID, "a")
	b := mustCreateChild(t, tree, ctx, root.ID, "b")

	moved, err := tree.Move(ctx, b.ID, a.ID)
	if err != nil {
		t.Fatalf("Move: %v", err)
	}
	got, err := tree.Get(ctx, b.ID)
	if err != nil {
		t.Fatalf("Get after the move: %v", err)
	}
	if !moved.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("Move returned updated_at %v but a subsequent Get reads %v -- the response must carry the post-write row, not the pre-write snapshot",
			moved.UpdatedAt, got.UpdatedAt)
	}
	if moved.UpdatedAt.Equal(b.UpdatedAt) {
		t.Errorf("Move's returned updated_at %v equals the pre-move value -- the move never stamped the row it returned", moved.UpdatedAt)
	}
}
