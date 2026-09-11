package org

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore/testkit"
)

// rbacShapedScope is a LOCAL restatement of the Scope interface, declared
// here without referring to org's own Scope type at all.
//
// This is the executable form of the claim scope.go makes in prose: a
// consumer -- rbac above all -- declares this exact method set in its own
// package and accepts org's implementation structurally, so neither module
// imports the other. If a change gave any method an org-owned type in its
// signature, the local declaration could not restate it and would stop
// compiling here rather than in somebody else's repository.
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
	ctx := testkit.TenantCtx("tenant-a")
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
