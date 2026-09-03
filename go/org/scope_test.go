package org

import (
	"context"
	"slices"
	"testing"
)

// rbacShapedScope is a LOCAL restatement of the Scope interface, declared
// here without referring to org's own Scope type at all.
//
// This is the executable form of the claim scope.go makes in prose: a
// consumer -- rbac above all -- declares this exact method set in its own
// package and accepts org's implementation structurally, so neither module
// imports the other. If a future change gave any method an org-owned type in
// its signature, rbac could no longer restate it, and this declaration would
// stop compiling here rather than in somebody else's repository.
type rbacShapedScope interface {
	Path(ctx context.Context, nodeID string) (string, error)
	DescendantIDs(ctx context.Context, nodeID string) ([]string, error)
	MemberNodeIDs(ctx context.Context, userID string) ([]string, error)
}

// compile-time proof of the no-import seam, in both directions: the locally
// declared interface is satisfied by org's implementation, and org's own
// exported interface is satisfied by the same value.
var (
	_ rbacShapedScope = (*ScopeService)(nil)
	_ Scope           = (*ScopeService)(nil)
)

// TestScope_IsSatisfiedStructurally is the runtime half of the same claim:
// a consumer holding only its own interface can call every method.
func TestScope_IsSatisfiedStructurally(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	var consumer rbacShapedScope = m.scope
	path, err := consumer.Path(ctx, root.ID)
	if err != nil {
		t.Fatalf("Path through the restated interface: %v", err)
	}
	if path != root.Path {
		t.Errorf("Path = %q, want %q", path, root.Path)
	}
}

func TestScopeService_Path(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.Tree(), ctx)

	for _, node := range []*OrgNode{root, left} {
		got, err := m.scope.Path(ctx, node.ID)
		if err != nil {
			t.Fatalf("Path(%q): %v", node.ID, err)
		}
		if got != node.Path {
			t.Errorf("Path(%q) = %q, want %q", node.ID, got, node.Path)
		}
	}

	if _, err := m.scope.Path(ctx, "00000000-0000-4000-8000-000000000000"); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Path(unknown) error = %v, want org.node_not_found", err)
	}

	// Another tenant's node is indistinguishable from a missing one.
	if _, err := m.scope.Path(tenantCtx("tenant-b"), root.ID); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("Path(other tenant's node) error = %v, want org.node_not_found", err)
	}
}

func TestScopeService_DescendantIDs(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	chair, err := m.Tree().CreateChild(ctx, left.ID, "chair 1", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	tests := []struct {
		name string
		node string
		want []string
	}{
		{"the root covers everything", root.ID, []string{root.ID, left.ID, right.ID, chair.ID}},
		{"a mid-tree node covers itself and below", left.ID, []string{left.ID, chair.ID}},
		{"a leaf covers itself", chair.ID, []string{chair.ID}},
		{"a sibling branch is never included", right.ID, []string{right.ID}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := m.scope.DescendantIDs(ctx, tc.node)
			if err != nil {
				t.Fatalf("DescendantIDs: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("DescendantIDs(%q) = %v, want %v", tc.node, got, tc.want)
			}
			for _, want := range tc.want {
				if !slices.Contains(got, want) {
					t.Errorf("DescendantIDs(%q) = %v, missing %q", tc.node, got, want)
				}
			}
			// The set always includes the node itself: standing somewhere
			// covers that place.
			if !slices.Contains(got, tc.node) {
				t.Errorf("DescendantIDs(%q) does not include the node itself", tc.node)
			}
		})
	}
}

// TestScopeService_DescendantIDs_DeepChain exercises a hierarchy deeper than
// the two levels a group-and-store shape would prove, up to the depth bound.
func TestScopeService_DescendantIDs_DeepChain(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")

	root, err := m.Tree().CreateRoot(ctx, "level 0", "group")
	if err != nil {
		t.Fatalf("CreateRoot: %v", err)
	}
	chain := []*OrgNode{root}
	for level := 1; level <= maxDepth; level++ {
		child, err := m.Tree().CreateChild(ctx, chain[level-1].ID, "level "+string(rune('0'+level)), "group")
		if err != nil {
			t.Fatalf("CreateChild(level %d): %v", level, err)
		}
		chain = append(chain, child)
	}

	for level, node := range chain {
		got, err := m.scope.DescendantIDs(ctx, node.ID)
		if err != nil {
			t.Fatalf("DescendantIDs(level %d): %v", level, err)
		}
		want := len(chain) - level
		if len(got) != want {
			t.Errorf("DescendantIDs at level %d returned %d ids, want %d", level, len(got), want)
		}
	}
}

// TestScopeService_MemberNodeIDs_MidTreeMember_ExcludesSiblingBranch is the
// data-visibility assertion the whole seam exists for: a manager standing at
// one store sees that store and everything under it, and never the store next
// door.
func TestScopeService_MemberNodeIDs_MidTreeMember_ExcludesSiblingBranch(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	chair, err := m.Tree().CreateChild(ctx, left.ID, "chair 1", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	if _, addErr := m.Members().Add(ctx, "u-manager", left.ID); addErr != nil {
		t.Fatalf("Add: %v", addErr)
	}

	got, err := m.scope.MemberNodeIDs(ctx, "u-manager")
	if err != nil {
		t.Fatalf("MemberNodeIDs: %v", err)
	}
	if len(got) != 2 || !slices.Contains(got, left.ID) || !slices.Contains(got, chair.ID) {
		t.Fatalf("MemberNodeIDs = %v, want exactly [%q %q]", got, left.ID, chair.ID)
	}
	if slices.Contains(got, right.ID) {
		t.Error("the member can see the sibling branch")
	}
	if slices.Contains(got, root.ID) {
		t.Error("the member can see their own parent node")
	}
}

func TestScopeService_MemberNodeIDs_RootMember_SeesEverything(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)

	if _, err := m.Members().Add(ctx, "u-owner", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := m.scope.MemberNodeIDs(ctx, "u-owner")
	if err != nil {
		t.Fatalf("MemberNodeIDs: %v", err)
	}
	for _, want := range []string{root.ID, left.ID, right.ID} {
		if !slices.Contains(got, want) {
			t.Errorf("MemberNodeIDs = %v, missing %q", got, want)
		}
	}
}

// TestScopeService_MemberNodeIDs_NoMembership_IsEmptyNotAnError pins the
// contract that keeps a consumer's listing filter simple: "this person sees
// nothing here" is an ordinary answer, and an empty IN list is the
// fail-closed outcome.
func TestScopeService_MemberNodeIDs_NoMembership_IsEmptyNotAnError(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	got, err := m.scope.MemberNodeIDs(ctx, "u-stranger")
	if err != nil {
		t.Fatalf("MemberNodeIDs = %v, want a nil error", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("MemberNodeIDs = %v, want an empty non-nil slice", got)
	}
}

// TestScopeService_MemberNodeIDs_SuspendedMember_SeesNothing pins that only
// an ACTIVE membership grants visibility.
func TestScopeService_MemberNodeIDs_SuspendedMember_SeesNothing(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.Tree(), ctx)

	seedMembership(t, m.Members().Repository(), ctx, Membership{
		ID: "20000000-0000-4000-8000-000000000009", UserID: "u-suspended",
		NodeID: root.ID, Status: MembershipStatusSuspended,
	})
	got, err := m.scope.MemberNodeIDs(ctx, "u-suspended")
	if err != nil {
		t.Fatalf("MemberNodeIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a suspended member sees %v, want nothing", got)
	}
}

// TestScopeService_MemberNodeIDs_CrossTenantProbe_IsEmpty pins that asking
// about a user who is a member of ANOTHER tenant reveals nothing.
func TestScopeService_MemberNodeIDs_CrossTenantProbe_IsEmpty(t *testing.T) {
	m, _ := newTestModule(t)
	ctxA := tenantCtx("tenant-a")
	ctxB := tenantCtx("tenant-b")

	rootA, _, _ := seedTree(t, m.Tree(), ctxA)
	if _, err := m.Members().Add(ctxA, "u-shared", rootA.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := m.Tree().CreateRoot(ctxB, "their group", "group"); err != nil {
		t.Fatalf("CreateRoot(tenant-b): %v", err)
	}

	got, err := m.scope.MemberNodeIDs(ctxB, "u-shared")
	if err != nil {
		t.Fatalf("MemberNodeIDs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("probing from another tenant returned %v, want nothing", got)
	}
}

// TestScopeService_MemberNodeIDs_DanglingMembership_FailsClosed pins the safe
// reading of a row whose node is gone: sees nothing, not everything, and not
// an error that would fail the consumer's whole request.
func TestScopeService_MemberNodeIDs_DanglingMembership_FailsClosed(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.Tree(), ctx)

	seedMembership(t, m.Members().Repository(), ctx, Membership{
		ID: "20000000-0000-4000-8000-000000000010", UserID: "u-dangling",
		NodeID: "00000000-0000-4000-8000-000000000000", Status: MembershipStatusActive,
	})
	got, err := m.scope.MemberNodeIDs(ctx, "u-dangling")
	if err != nil {
		t.Fatalf("MemberNodeIDs = %v, want a nil error", err)
	}
	if len(got) != 0 {
		t.Errorf("a dangling membership resolved to %v, want nothing", got)
	}
}

// TestScopeService_MemberNodeIDs_AfterAMove_FollowsTheSubtree pins that the
// scope is derived from the tree at read time rather than cached: moving a
// member's node changes what they see, which is exactly why org.node.moved
// exists for anybody who does cache it.
func TestScopeService_MemberNodeIDs_AfterAMove_FollowsTheSubtree(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.Tree(), ctx)
	chair, err := m.Tree().CreateChild(ctx, right.ID, "chair 1", "room")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}
	if _, addErr := m.Members().Add(ctx, "u-manager", left.ID); addErr != nil {
		t.Fatalf("Add: %v", addErr)
	}

	before, err := m.scope.MemberNodeIDs(ctx, "u-manager")
	if err != nil {
		t.Fatalf("MemberNodeIDs before: %v", err)
	}
	if slices.Contains(before, chair.ID) {
		t.Fatal("the manager already sees the chair before the move")
	}

	if _, moveErr := m.Tree().Move(ctx, chair.ID, left.ID); moveErr != nil {
		t.Fatalf("Move: %v", moveErr)
	}
	after, err := m.scope.MemberNodeIDs(ctx, "u-manager")
	if err != nil {
		t.Fatalf("MemberNodeIDs after: %v", err)
	}
	if !slices.Contains(after, chair.ID) {
		t.Errorf("after the move the manager sees %v, want the chair %q included", after, chair.ID)
	}
	if slices.Contains(after, root.ID) {
		t.Error("the move widened the scope upwards")
	}
}

func TestScopeService_NoTenantContext_FailsClosed(t *testing.T) {
	m, _ := newTestModule(t)
	ctx := context.Background()

	if _, err := m.scope.Path(ctx, "n-1"); err == nil {
		t.Error("Path without a tenant succeeded; it must fail closed")
	}
	if _, err := m.scope.DescendantIDs(ctx, "n-1"); err == nil {
		t.Error("DescendantIDs without a tenant succeeded; it must fail closed")
	}
	if _, err := m.scope.MemberNodeIDs(ctx, "u-1"); err == nil {
		t.Error("MemberNodeIDs without a tenant succeeded; it must fail closed")
	}
}
