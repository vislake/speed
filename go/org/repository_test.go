package org

import (
	"context"
	"fmt"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/org/internal/testutil"
	"github.com/vislake/speed/go/org/migrations"
)

// newTestDB returns a SQLite database with org's real migrations applied
// from zero. Every DB-backed test in this package starts here.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

// tenantCtx returns a context carrying tenant, the only way any code in this
// module ever learns which tenant it is acting for.
func tenantCtx(tenant pkgcore.TenantID) context.Context {
	return pkgcore.WithTenant(context.Background(), tenant)
}

// seedNode inserts one node directly through the repository, for tests that
// need a specific tree shape rather than TreeService's own validation.
func seedNode(t *testing.T, repo *Repository, ctx context.Context, node OrgNode) OrgNode {
	t.Helper()
	if err := repo.Create(ctx, &node); err != nil {
		t.Fatalf("seed node %q: %v", node.ID, err)
	}
	return node
}

// TestRepository_AssertIsolated runs the mandatory tenant-isolation suite
// against org_nodes. org_nodes is tenant data (docs/internal/04-data-and-
// tenancy.md), so AssertIsolated -- not AssertNotTenantScoped -- is the
// correct half of the pair: a node is meaningless outside its tenant and
// must never be readable, updatable or deletable from another one.
func TestRepository_AssertIsolated(t *testing.T) {
	repo := NewRepository(newTestDB(t))

	n := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *OrgNode {
		n++
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
		return &OrgNode{
			ID:       id,
			ParentID: "",
			Path:     buildPath("", id),
			Depth:    rootDepth,
			Name:     fmt.Sprintf("node-%d", n),
			Kind:     "group",
		}
	})
}

func TestRepository_subtree(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	// A deliberately adversarial shape: "aa" and "aab" are siblings whose
	// ids share a prefix, and "aa" has a two-level subtree beneath it.
	seedNode(t, repo, ctx, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctx, OrgNode{ID: "aa", ParentID: "r", Path: "/r/aa/", Depth: 1, Name: "aa"})
	seedNode(t, repo, ctx, OrgNode{ID: "aab", ParentID: "r", Path: "/r/aab/", Depth: 1, Name: "aab"})
	seedNode(t, repo, ctx, OrgNode{ID: "bb", ParentID: "aa", Path: "/r/aa/bb/", Depth: 2, Name: "bb"})
	seedNode(t, repo, ctx, OrgNode{ID: "cc", ParentID: "bb", Path: "/r/aa/bb/cc/", Depth: 3, Name: "cc"})

	tests := []struct {
		name   string
		prefix string
		want   []string
	}{
		{name: "whole tree from the root", prefix: "/r/", want: []string{"r", "aa", "aab", "bb", "cc"}},
		{
			name:   "a mid-tree subtree includes itself and excludes the prefix-sharing sibling",
			prefix: "/r/aa/",
			want:   []string{"aa", "bb", "cc"},
		},
		{name: "the prefix-sharing sibling stands alone", prefix: "/r/aab/", want: []string{"aab"}},
		{name: "a leaf is its own subtree", prefix: "/r/aa/bb/cc/", want: []string{"cc"}},
		{name: "an unrelated prefix matches nothing", prefix: "/zz/", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes, err := repo.subtree(ctx, tc.prefix)
			if err != nil {
				t.Fatalf("subtree(%q): %v", tc.prefix, err)
			}
			assertIDSet(t, nodes, tc.want)
		})
	}
}

// TestRepository_subtree_OtherTenantWithIdenticalPaths_IsNotReturned is the
// isolation case a path-based query most needs: two tenants can hold rows
// whose path strings are byte-identical, so the prefix scan alone would
// happily return both. Only the injected tenant filter keeps them apart.
func TestRepository_subtree_OtherTenantWithIdenticalPaths_IsNotReturned(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedNode(t, repo, ctxA, OrgNode{ID: "a-r", Path: "/shared/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxA, OrgNode{ID: "a-c", ParentID: "a-r", Path: "/shared/child/", Depth: 1, Name: "child"})
	seedNode(t, repo, ctxB, OrgNode{ID: "b-r", Path: "/shared/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxB, OrgNode{ID: "b-c", ParentID: "b-r", Path: "/shared/child/", Depth: 1, Name: "child"})

	got, err := repo.subtree(ctxA, "/shared/")
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	assertIDSet(t, got, []string{"a-r", "a-c"})
}

// TestRepository_subtree_NoTenantContext_FailsClosed pins the fail-closed
// rule for the query shapes Repository[T] does not cover: an unscoped
// context must error, never silently return every tenant's rows.
func TestRepository_subtree_NoTenantContext_FailsClosed(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	seedNode(t, repo, tenantCtx("tenant-a"), OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})

	got, err := repo.subtree(context.Background(), "/r/")
	if err == nil {
		t.Fatalf("subtree on an unscoped context returned %d rows and no error, want a failure", len(got))
	}
	if len(got) != 0 {
		t.Errorf("subtree on an unscoped context returned %d rows, want none", len(got))
	}
}

func TestRepository_children(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	seedNode(t, repo, ctx, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctx, OrgNode{ID: "b", ParentID: "r", Path: "/r/b/", Depth: 1, Name: "beta"})
	seedNode(t, repo, ctx, OrgNode{ID: "a", ParentID: "r", Path: "/r/a/", Depth: 1, Name: "alpha"})
	seedNode(t, repo, ctx, OrgNode{ID: "g", ParentID: "a", Path: "/r/a/g/", Depth: 2, Name: "grandchild"})

	t.Run("direct children only, ordered by name", func(t *testing.T) {
		nodes, err := repo.children(ctx, "r")
		if err != nil {
			t.Fatalf("children: %v", err)
		}
		if len(nodes) != 2 || nodes[0].ID != "a" || nodes[1].ID != "b" {
			t.Fatalf("children(r) = %v, want [a b] ordered by name", idsOf(nodes))
		}
	})

	t.Run("a leaf has none", func(t *testing.T) {
		nodes, err := repo.children(ctx, "g")
		if err != nil {
			t.Fatalf("children: %v", err)
		}
		if len(nodes) != 0 {
			t.Errorf("children(g) = %v, want none", idsOf(nodes))
		}
	})
}

func TestRepository_findRoot(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedNode(t, repo, ctxA, OrgNode{ID: "a-r", Path: "/a-r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxA, OrgNode{ID: "a-c", ParentID: "a-r", Path: "/a-r/a-c/", Depth: 1, Name: "child"})

	t.Run("returns the tenant's own root", func(t *testing.T) {
		root, err := repo.findRoot(ctxA)
		if err != nil {
			t.Fatalf("findRoot: %v", err)
		}
		if root.ID != "a-r" {
			t.Errorf("findRoot = %q, want a-r", root.ID)
		}
	})

	t.Run("a tenant with no tree reports node_not_found", func(t *testing.T) {
		_, err := repo.findRoot(ctxB)
		if err == nil {
			t.Fatal("findRoot on an empty tenant returned no error")
		}
		assertCode(t, err, ErrNodeNotFound.Code)
	})
}

func TestRepository_bySiblingName(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	seedNode(t, repo, ctx, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctx, OrgNode{ID: "a", ParentID: "r", Path: "/r/a/", Depth: 1, Name: "North"})
	seedNode(t, repo, ctx, OrgNode{ID: "b", ParentID: "a", Path: "/r/a/b/", Depth: 2, Name: "North"})

	t.Run("finds a sibling by exact name", func(t *testing.T) {
		node, err := repo.bySiblingName(ctx, "r", "North")
		if err != nil {
			t.Fatalf("bySiblingName: %v", err)
		}
		if node.ID != "a" {
			t.Errorf("bySiblingName = %q, want a", node.ID)
		}
	})

	t.Run("the same name under a different parent is a different node", func(t *testing.T) {
		node, err := repo.bySiblingName(ctx, "a", "North")
		if err != nil {
			t.Fatalf("bySiblingName: %v", err)
		}
		if node.ID != "b" {
			t.Errorf("bySiblingName = %q, want b", node.ID)
		}
	})

	t.Run("an unused name reports node_not_found", func(t *testing.T) {
		_, err := repo.bySiblingName(ctx, "r", "South")
		if err == nil {
			t.Fatal("bySiblingName for an unused name returned no error")
		}
		assertCode(t, err, ErrNodeNotFound.Code)
	})
}

func TestRepository_byIDs(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedNode(t, repo, ctxA, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxA, OrgNode{ID: "m", ParentID: "r", Path: "/r/m/", Depth: 1, Name: "mid"})
	seedNode(t, repo, ctxB, OrgNode{ID: "other", Path: "/other/", Depth: 0, Name: "root"})

	t.Run("returns the requested rows ordered by depth", func(t *testing.T) {
		nodes, err := repo.byIDs(ctxA, []string{"m", "r"})
		if err != nil {
			t.Fatalf("byIDs: %v", err)
		}
		if len(nodes) != 2 || nodes[0].ID != "r" || nodes[1].ID != "m" {
			t.Fatalf("byIDs = %v, want [r m] ordered by depth", idsOf(nodes))
		}
	})

	t.Run("an empty slice touches nothing", func(t *testing.T) {
		nodes, err := repo.byIDs(ctxA, nil)
		if err != nil {
			t.Fatalf("byIDs(nil): %v", err)
		}
		if len(nodes) != 0 {
			t.Errorf("byIDs(nil) = %v, want none", idsOf(nodes))
		}
	})

	t.Run("another tenant's id is silently absent, never returned", func(t *testing.T) {
		nodes, err := repo.byIDs(ctxA, []string{"r", "other"})
		if err != nil {
			t.Fatalf("byIDs: %v", err)
		}
		assertIDSet(t, nodes, []string{"r"})
	})
}

func TestRepository_deleteSubtree(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedNode(t, repo, ctxA, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxA, OrgNode{ID: "a", ParentID: "r", Path: "/r/a/", Depth: 1, Name: "a"})
	seedNode(t, repo, ctxA, OrgNode{ID: "ab", ParentID: "r", Path: "/r/ab/", Depth: 1, Name: "ab"})
	seedNode(t, repo, ctxA, OrgNode{ID: "c", ParentID: "a", Path: "/r/a/c/", Depth: 2, Name: "c"})
	seedNode(t, repo, ctxB, OrgNode{ID: "b-r", Path: "/r/", Depth: 0, Name: "root"})

	affected, err := repo.deleteSubtree(ctxA, "/r/a/")
	if err != nil {
		t.Fatalf("deleteSubtree: %v", err)
	}
	if affected != 2 {
		t.Errorf("deleteSubtree removed %d rows, want 2", affected)
	}

	remaining, err := repo.subtree(ctxA, "/r/")
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	assertIDSet(t, remaining, []string{"r", "ab"})

	// The other tenant's identically-pathed tree is untouched.
	otherRemaining, err := repo.subtree(ctxB, "/r/")
	if err != nil {
		t.Fatalf("subtree: %v", err)
	}
	assertIDSet(t, otherRemaining, []string{"b-r"})
}

// TestRepository_deleteLeaf covers the guard that makes a non-cascading
// delete safe: the row count is checked inside the transaction, so a prefix
// matching more than the one node rolls the DELETE back rather than
// orphaning whatever it also matched.
func TestRepository_deleteLeaf(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	seedNode(t, repo, ctx, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctx, OrgNode{ID: "a", ParentID: "r", Path: "/r/a/", Depth: 1, Name: "a"})
	seedNode(t, repo, ctx, OrgNode{ID: "b", ParentID: "a", Path: "/r/a/b/", Depth: 2, Name: "b"})
	seedNode(t, repo, ctx, OrgNode{ID: "leaf", ParentID: "r", Path: "/r/leaf/", Depth: 1, Name: "leaf"})

	t.Run("a genuine leaf is removed", func(t *testing.T) {
		matched, err := repo.deleteLeaf(ctx, "/r/leaf/")
		if err != nil {
			t.Fatalf("deleteLeaf: %v", err)
		}
		if matched != 1 {
			t.Errorf("matched = %d, want 1", matched)
		}
	})

	t.Run("a node with descendants is reported and rolled back", func(t *testing.T) {
		matched, err := repo.deleteLeaf(ctx, "/r/a/")
		if err != nil {
			t.Fatalf("deleteLeaf: %v", err)
		}
		if matched != 2 {
			t.Errorf("matched = %d, want 2 (the node plus its one descendant)", matched)
		}
		// The rollback is the whole point: both rows must still be there.
		remaining, err := repo.subtree(ctx, "/r/a/")
		if err != nil {
			t.Fatalf("subtree: %v", err)
		}
		assertIDSet(t, remaining, []string{"a", "b"})
	})

	t.Run("a prefix matching nothing removes nothing and reports zero", func(t *testing.T) {
		matched, err := repo.deleteLeaf(ctx, "/r/gone/")
		if err != nil {
			t.Fatalf("deleteLeaf: %v", err)
		}
		if matched != 0 {
			t.Errorf("matched = %d, want 0", matched)
		}
	})
}

// TestRepository_SiblingNameUniqueness_IsEnforcedByTheDatabase proves the
// UNIQUE(tenant_id, parent_id, name) index is real -- it is the backstop
// behind TreeService's own pre-check, and the only thing that closes the
// race between two concurrent creates.
func TestRepository_SiblingNameUniqueness_IsEnforcedByTheDatabase(t *testing.T) {
	repo := NewRepository(newTestDB(t))
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	seedNode(t, repo, ctxA, OrgNode{ID: "r", Path: "/r/", Depth: 0, Name: "root"})
	seedNode(t, repo, ctxA, OrgNode{ID: "a", ParentID: "r", Path: "/r/a/", Depth: 1, Name: "North"})

	t.Run("a duplicate name under the same parent is rejected", func(t *testing.T) {
		dup := OrgNode{ID: "a2", ParentID: "r", Path: "/r/a2/", Depth: 1, Name: "North"}
		if err := repo.Create(ctxA, &dup); err == nil {
			t.Fatal("creating a duplicate sibling name succeeded, want a unique-constraint failure")
		}
	})

	t.Run("the same name under a different parent is allowed", func(t *testing.T) {
		ok := OrgNode{ID: "a3", ParentID: "a", Path: "/r/a/a3/", Depth: 2, Name: "North"}
		if err := repo.Create(ctxA, &ok); err != nil {
			t.Fatalf("creating the same name under a different parent failed: %v", err)
		}
	})

	t.Run("the same name in a different tenant is allowed", func(t *testing.T) {
		seedNode(t, repo, ctxB, OrgNode{ID: "b-r", Path: "/b-r/", Depth: 0, Name: "root"})
		ok := OrgNode{ID: "b-a", ParentID: "b-r", Path: "/b-r/b-a/", Depth: 1, Name: "North"}
		if err := repo.Create(ctxB, &ok); err != nil {
			t.Fatalf("creating the same name in another tenant failed: %v", err)
		}
	})

	t.Run("two roots cannot share a name, because the sentinel is not NULL", func(t *testing.T) {
		// This is the whole reason parent_id is an empty-string sentinel
		// rather than NULL: under NULL, both engines treat the two rows'
		// index entries as distinct and this insert would succeed.
		dupRoot := OrgNode{ID: "r2", ParentID: "", Path: "/r2/", Depth: 0, Name: "root"}
		if err := repo.Create(ctxA, &dupRoot); err == nil {
			t.Fatal("a second root named the same succeeded, want a unique-constraint failure")
		}
	})
}

// idsOf extracts nodes' ids, for readable failure messages.
func idsOf(nodes []OrgNode) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

// assertIDSet fails t unless nodes hold exactly want's ids, ignoring order.
func assertIDSet(t *testing.T, nodes []OrgNode, want []string) {
	t.Helper()
	got := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		got[n.ID] = true
	}
	if len(got) != len(want) {
		t.Fatalf("node ids = %v, want %v", idsOf(nodes), want)
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("node ids = %v, want %v", idsOf(nodes), want)
		}
	}
}
