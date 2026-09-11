//go:build integration

package org_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is org's PostgreSQL proof for the tree's three concurrent-write
// hazard classes -- a child landing under a soft-deleted parent, a moved
// subtree whose Path and ParentID chain disagree, and a restored node left
// under a parent that ends up dead -- the module's own unit tier's
// tree_test.go carries the SQLite forms of these same concurrency
// regression tests; this file exists because a SQLite proof alone cannot
// exercise PostgreSQL's real READ COMMITTED window. Each test races two operations many times against a real
// PostgreSQL 16 server and re-checks the tree's structural invariant after
// every round through assertTreeInvariant -- deliberately the ONLY
// assertion each one makes, for the same reason the unit tier's own
// TestTreeService_ConcurrentRestoreAndCascadeDelete_NeverLandsOnADeadParent
// doc comment gives: which of the two racing calls "won" a given round can
// legitimately go either way even on fully correct code, so asserting on
// that split would be a false-positive risk rather than a real regression
// signal. The invariant itself -- a live node's ParentID names another live
// node, and its Path is exactly that parent's with its own id appended -- is
// what actually distinguishes a merely-surprising outcome from the genuine
// corruption these hazard classes name.
func assertTreeInvariant(t *testing.T, tree *org.TreeService, ctx context.Context, label string) {
	t.Helper()
	root, err := tree.Root(ctx)
	if err != nil {
		t.Fatalf("%s: Root: %v", label, err)
	}
	nodes, err := tree.Subtree(ctx, root.ID)
	if err != nil {
		t.Fatalf("%s: Subtree(root): %v", label, err)
	}
	byID := make(map[string]org.OrgNode, len(nodes))
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
		want := parent.Path + n.ID + "/"
		if n.Path != want {
			t.Fatalf("%s: node %s has Path %s, want %s (parent %s's Path %s + its own id) -- Path/ParentID chain disagree",
				label, n.ID, n.Path, want, parent.ID, parent.Path)
		}
	}
}

func TestConcurrentCreateChildAndDelete_NeverOrphansAChild_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 20
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		parent, err := tree.CreateChild(ctx, root.ID, "parent", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(parent): %v", round, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = tree.CreateChild(ctx, parent.ID, "child", "store")
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = tree.Delete(ctx, parent.ID, false)
		}()
		close(start)
		wg.Wait()

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}

func TestConcurrentMoveAndMove_TreeInvariantHolds_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 15
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(a): %v", round, err)
		}
		b, err := tree.CreateChild(ctx, root.ID, "b", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(b): %v", round, err)
		}
		if _, err := tree.CreateChild(ctx, a.ID, "a-child", "store"); err != nil {
			t.Fatalf("round %d: CreateChild(a-child): %v", round, err)
		}
		if _, err := tree.CreateChild(ctx, b.ID, "b-child", "store"); err != nil {
			t.Fatalf("round %d: CreateChild(b-child): %v", round, err)
		}

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
			_, _ = tree.Move(ctx, b.ID, a.ID)
		}()
		close(start)
		wg.Wait()

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}

func TestConcurrentRestoreAndCascadeDelete_NeverLandsOnADeadParent_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 20
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		parent, err := tree.CreateChild(ctx, root.ID, "parent", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(parent): %v", round, err)
		}
		child, err := tree.CreateChild(ctx, parent.ID, "child", "store")
		if err != nil {
			t.Fatalf("round %d: CreateChild(child): %v", round, err)
		}
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

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestConcurrentMoveAndCreateChild_TreeInvariantHolds_Postgres is the
// second required pairing (see the unit tier's identically-named test's own
// doc comment): a concurrent CreateChild targeting the exact node another
// goroutine is Move-ing.
func TestConcurrentMoveAndCreateChild_TreeInvariantHolds_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 15
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(a): %v", round, err)
		}
		b, err := tree.CreateChild(ctx, root.ID, "b", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(b): %v", round, err)
		}

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

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestConcurrentMoveAndCreateChild_InteriorDescendant_TreeInvariantHolds_Postgres
// is the interior-descendant pairing that TestConcurrentMoveAndCreateChild_TreeInvariantHolds_Postgres
// above never actually covers: that test's CreateChild targets the exact
// node being Moved (a.ID), a row Move already locks via lockLiveNode -- never
// an INTERIOR DESCENDANT of the moved subtree (a-child), a row Move's
// rewrite would touch only through its own plain, unlocked subtree scan.
// Only a real PostgreSQL server reproduces this window: READ COMMITTED
// semantics let CreateChild's insert land, and commit, entirely within the
// gap between Move's scan and that scan's later per-row rewrite of a-child,
// while SQLite's coarser whole-file locking masks it -- which is why the
// unit tier's SQLite twin of this test (tree_test.go) cannot reproduce it
// within its own round budget. lockSubtree (repository.go) closes it by
// locking every row of the subtree before trusting the scanned set is
// complete, not merely the two endpoints Move already locked.
func TestConcurrentMoveAndCreateChild_InteriorDescendant_TreeInvariantHolds_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 60
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(a): %v", round, err)
		}
		b, err := tree.CreateChild(ctx, root.ID, "b", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(b): %v", round, err)
		}
		aChild, err := tree.CreateChild(ctx, a.ID, "a-child", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(a-child): %v", round, err)
		}

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

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}

// TestConcurrentMoveAndDelete_TreeInvariantHolds_Postgres is the third
// required pairing: Move racing a cascade Delete of the node it is moving.
func TestConcurrentMoveAndDelete_TreeInvariantHolds_Postgres(t *testing.T) {
	db := newPostgres(t)

	const rounds = 15
	for round := 0; round < rounds; round++ {
		ctx := tenantCtx(pkgcore.TenantID(fmt.Sprintf("tenant-%d", round)))
		tree := org.NewTreeService(db)
		root, err := tree.CreateRoot(ctx, fmt.Sprintf("root-%d", round), "group")
		if err != nil {
			t.Fatalf("round %d: CreateRoot: %v", round, err)
		}
		a, err := tree.CreateChild(ctx, root.ID, "a", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(a): %v", round, err)
		}
		b, err := tree.CreateChild(ctx, root.ID, "b", "group")
		if err != nil {
			t.Fatalf("round %d: CreateChild(b): %v", round, err)
		}
		if _, err := tree.CreateChild(ctx, a.ID, "a-child", "store"); err != nil {
			t.Fatalf("round %d: CreateChild(a-child): %v", round, err)
		}

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

		assertTreeInvariant(t, tree, ctx, fmt.Sprintf("round %d", round))
	}
}
