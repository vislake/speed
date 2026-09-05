package rbac

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestService_Can_NoBindings_DeniesWithoutError(t *testing.T) {
	// Deny by default, and specifically deny rather than ERROR: a subject
	// who simply holds nothing is an ordinary 403, not a failure.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "nobody"}

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if ok {
		t.Fatal("Can = true for a subject with no bindings at all")
	}
}

func TestService_Can_GrantedPermission_IsAllowed(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if !ok {
		t.Fatal("Can = false for a permission the subject's role grants")
	}
}

func TestService_Can_MatchIsExact_NotPrefixOrWildcard(t *testing.T) {
	// No wildcard grammar exists in this milestone (a grammar is a
	// security surface that needs a design decision, not an
	// implementation guess). "notes:read" must therefore not imply
	// "notes:write", and a resource that merely shares a prefix must not
	// match either.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	cases := []struct {
		action, resource string
		want             bool
	}{
		{action: "read", resource: "notes", want: true},
		{action: "write", resource: "notes", want: false},
		{action: "manage", resource: "billing", want: false},
		{action: "read", resource: "note", want: false},
		{action: "read", resource: "notes2", want: false},
		{action: "rea", resource: "notes", want: false},
		{action: "", resource: "notes", want: false},
		{action: "read", resource: "", want: false},
		{action: "*", resource: "notes", want: false},
		{action: "read", resource: "*", want: false},
	}
	for _, tc := range cases {
		got, err := svc.Can(context.Background(), sub, tc.action, tc.resource)
		if err != nil {
			t.Fatalf("Can(%q, %q): %v", tc.action, tc.resource, err)
		}
		if got != tc.want {
			t.Fatalf("Can(action=%q, resource=%q) = %v, want %v", tc.action, tc.resource, got, tc.want)
		}
	}
}

func TestService_Can_UndeclaredPermission_DeniesWithoutError(t *testing.T) {
	// A check must never turn a request into a 500 because the caller
	// asked about a name nobody declared. Grant time is where strictness
	// belongs (see TestService_DefineRole_UndeclaredPermission_IsRejected).
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	ok, err := svc.Can(context.Background(), sub, "teleport", "unicorn")
	if err != nil {
		t.Fatalf("Can on an undeclared permission: %v", err)
	}
	if ok {
		t.Fatal("Can = true for a permission no module ever declared")
	}
}

func TestService_Can_ABindingInAnotherTenant_NeverGrants(t *testing.T) {
	// The same user id in two tenants is the ordinary case. A grant in
	// tenant A must be invisible to the same person acting in tenant B --
	// this is the horizontal-privilege boundary the whole module exists to
	// hold.
	svc := newTestService(t)
	inA := Subject{TenantID: "tenant-a", UserID: "user-1"}
	inB := Subject{TenantID: "tenant-b", UserID: "user-1"}
	grant(t, svc, inA, "reader", Scope{}, "notes:read")

	ok, err := svc.Can(context.Background(), inB, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if ok {
		t.Fatal("a binding in tenant-a granted the same user id a permission in tenant-b")
	}
}

func TestService_Can_AmbientTenantContext_DoesNotSteerTheRead(t *testing.T) {
	// The Subject is the authorization identity the authenticating side
	// vouched for. A context tenant that disagrees with it must not be
	// able to redirect the grant lookup -- otherwise a request whose
	// tenancy middleware and token disagreed would be evaluated against
	// the wrong tenant's roles.
	svc := newTestService(t)
	inA := Subject{TenantID: "tenant-a", UserID: "user-1"}
	inB := Subject{TenantID: "tenant-b", UserID: "user-1"}
	grant(t, svc, inA, "reader", Scope{}, "notes:read")

	// Ask about the tenant-b subject while ctx insists on tenant-a.
	ok, err := svc.Can(tenantCtx("tenant-a"), inB, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if ok {
		t.Fatal("an ambient tenant-a context granted a tenant-b subject tenant-a's permission")
	}

	// And the converse: the subject's own tenant is used even when ctx
	// carries a different one, so the decision does not depend on the
	// host having set a matching tenant context at all.
	ok, err = svc.Can(tenantCtx("tenant-b"), inA, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if !ok {
		t.Fatal("the subject's own tenant was not used for the lookup")
	}
}

func TestService_Can_IncompleteSubject_IsAnError(t *testing.T) {
	// Not a denial: something upstream failed to establish identity, and
	// reporting that as a plain "no permission" would bury the bug behind
	// a plausible 403 in every log.
	svc := newTestService(t)
	for _, sub := range []Subject{{}, {TenantID: "tenant-a"}, {UserID: "user-1"}} {
		ok, err := svc.Can(context.Background(), sub, "read", "notes")
		if ok {
			t.Fatalf("Can(%+v) = true", sub)
		}
		if !hasCode(err, ErrSubjectRequired.Code) {
			t.Fatalf("Can(%+v) error = %v, want %s", sub, err, ErrSubjectRequired.Code)
		}
	}
}

func TestService_Can_NodeScopedBinding_GrantsWithoutAResolver(t *testing.T) {
	// Can answers "does this subject hold it ANYWHERE" and deliberately
	// ignores organization-tree scope, so it must not consult the resolver
	// at all. Where the grant applies is DataScope's question, and the
	// next test pins that the two answers differ here on purpose.
	resolver := &stubResolver{}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")

	ok, err := svc.Can(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("Can: %v", err)
	}
	if !ok {
		t.Fatal("a node-scoped binding did not grant the permission anywhere")
	}
	if resolver.calls != 0 {
		t.Fatalf("Can consulted the organization tree %d times, want 0", resolver.calls)
	}
}

func TestService_DataScope_NodeScopedBinding_WithoutAResolver_IsDenied(t *testing.T) {
	// The fail-closed rule, and the case where Can and DataScope
	// legitimately disagree: the grant exists, but there is no known part
	// of the tree it covers, and a narrowing that cannot be evaluated must
	// NEVER fall back to tenant-wide.
	svc := newTestService(t) // no WithSubtreeResolver
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")

	if ok, err := svc.Can(context.Background(), sub, "read", "notes"); err != nil || !ok {
		t.Fatalf("Can = %v, %v; want true, nil", ok, err)
	}

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.Denied {
		t.Fatalf("DataScope = %+v, want Denied", scope)
	}
	if scope.TenantWide {
		t.Fatal("an unresolvable node-scoped grant widened to the whole tenant")
	}
	if scope.Includes("/g1/r2") {
		t.Fatal("a denied scope included a node")
	}
}

func TestService_DataScope_NodeScopedBinding_ResolvesToItsSubtree(t *testing.T) {
	resolver := &stubResolver{paths: map[string]string{"node-7": "/g1/r2"}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if scope.Denied || scope.TenantWide {
		t.Fatalf("DataScope = %+v, want a subtree scope", scope)
	}
	if !reflect.DeepEqual(scope.SubtreePrefixes, []string{"/g1/r2"}) {
		t.Fatalf("SubtreePrefixes = %v, want [/g1/r2]", scope.SubtreePrefixes)
	}
	if !scope.Includes("/g1/r2/s7") {
		t.Fatal("the scope excluded a node inside the granted subtree")
	}
	if scope.Includes("/g1/r20") {
		t.Fatal("the scope included a sibling whose name merely starts with the granted one")
	}
}

func TestService_DataScope_SeveralNodes_AreSortedAndDeduplicated(t *testing.T) {
	resolver := &stubResolver{paths: map[string]string{
		"node-9": "/g2",
		"node-7": "/g1/r2",
		"node-8": "/g1/r2", // two nodes, one path: a de-duplication case
	}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-9"}, "notes:read")
	if err := svc.AssignRole(tenantCtx("tenant-a"), sub, "region-reader", Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("AssignRole(node-7): %v", err)
	}
	if err := svc.AssignRole(tenantCtx("tenant-a"), sub, "region-reader", Scope{NodeID: "node-8"}); err != nil {
		t.Fatalf("AssignRole(node-8): %v", err)
	}

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !reflect.DeepEqual(scope.SubtreePrefixes, []string{"/g1/r2", "/g2"}) {
		t.Fatalf("SubtreePrefixes = %v, want [/g1/r2 /g2] sorted and de-duplicated", scope.SubtreePrefixes)
	}
}

func TestService_DataScope_UnknownNode_ContributesNothing(t *testing.T) {
	// The node is gone from the tree. That binding grants nothing, but it
	// must not poison the bindings that still resolve.
	resolver := &stubResolver{paths: map[string]string{"node-7": "/g1/r2"}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")
	if err := svc.AssignRole(tenantCtx("tenant-a"), sub, "region-reader", Scope{NodeID: "deleted-node"}); err != nil {
		t.Fatalf("AssignRole(deleted-node): %v", err)
	}

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !reflect.DeepEqual(scope.SubtreePrefixes, []string{"/g1/r2"}) {
		t.Fatalf("SubtreePrefixes = %v, want the one node that still exists", scope.SubtreePrefixes)
	}
}

func TestService_DataScope_EveryNodeUnknown_IsDenied(t *testing.T) {
	resolver := &stubResolver{paths: map[string]string{}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "deleted-node"}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.Denied {
		t.Fatalf("DataScope = %+v, want Denied when no granted node exists any more", scope)
	}
}

func TestService_DataScope_ResolverError_Propagates(t *testing.T) {
	// "The tree is unreachable" is NOT "the node does not exist". The
	// correct scope is unknown, and returning a partial one would silently
	// narrow or widen it, so the error travels to the caller -- who must
	// treat it as a denial, which the Denied scope returned alongside it
	// makes hard to get wrong.
	boom := errors.New("organization tree unavailable")
	resolver := &stubResolver{err: boom}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err == nil {
		t.Fatal("DataScope succeeded although the resolver failed")
	}
	if !hasCode(err, ErrSubtreeUnresolved.Code) {
		t.Fatalf("error = %v, want %s", err, ErrSubtreeUnresolved.Code)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the resolver's own failure", err)
	}
	if !scope.Denied {
		t.Fatalf("DataScope returned %+v alongside its error, want a Denied scope", scope)
	}
}

func TestService_DataScope_TenantWideGrant_SkipsTheResolver(t *testing.T) {
	// Nothing a subtree could add to "the whole tenant", so the
	// organization-tree read must stay off the common case's hot path.
	resolver := &stubResolver{paths: map[string]string{"node-7": "/g1/r2"}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.TenantWide || scope.Denied {
		t.Fatalf("DataScope = %+v, want TenantWide", scope)
	}
	if len(scope.SubtreePrefixes) != 0 {
		t.Fatalf("a tenant-wide scope carries prefixes %v, want none", scope.SubtreePrefixes)
	}
	if resolver.calls != 0 {
		t.Fatalf("a tenant-wide decision consulted the organization tree %d times, want 0", resolver.calls)
	}
	if !scope.Includes("/anything/at/all") {
		t.Fatal("a tenant-wide scope excluded a node")
	}
}

func TestService_DataScope_TenantWideAndNodeScoped_ForTheSamePermission_IsTenantWide(t *testing.T) {
	// Two bindings, the broader one wins: a subject who holds a permission
	// tenant-wide is not narrowed by also holding it over one branch.
	resolver := &stubResolver{paths: map[string]string{"node-7": "/g1/r2"}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")
	if err := svc.AssignRole(tenantCtx("tenant-a"), sub, "reader", Scope{NodeID: "node-7"}); err != nil {
		t.Fatalf("AssignRole(node-7): %v", err)
	}

	scope, err := svc.DataScope(context.Background(), sub, "read", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.TenantWide {
		t.Fatalf("DataScope = %+v, want TenantWide", scope)
	}
}

func TestService_DataScope_PermissionNotHeld_IsDenied(t *testing.T) {
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	scope, err := svc.DataScope(context.Background(), sub, "write", "notes")
	if err != nil {
		t.Fatalf("DataScope: %v", err)
	}
	if !scope.Denied {
		t.Fatalf("DataScope = %+v, want Denied for a permission the subject does not hold", scope)
	}
}

func TestService_DataScope_IncompleteSubject_IsADeniedScopeAndAnError(t *testing.T) {
	svc := newTestService(t)
	scope, err := svc.DataScope(context.Background(), Subject{}, "read", "notes")
	if !hasCode(err, ErrSubjectRequired.Code) {
		t.Fatalf("error = %v, want %s", err, ErrSubjectRequired.Code)
	}
	if !scope.Denied {
		t.Fatalf("DataScope = %+v, want a Denied scope alongside the error", scope)
	}
}

func TestService_ListPermissions_IsFlatSortedAndDeduplicated(t *testing.T) {
	// The list a signed-in client renders navigation from. Two roles
	// granting the same permission must produce one entry, and node scope
	// is flattened away exactly as in Can.
	resolver := &stubResolver{paths: map[string]string{"node-7": "/g1/r2"}}
	svc := newTestService(t, WithSubtreeResolver(resolver))
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "writer", Scope{}, "notes:write", "notes:read")
	grant(t, svc, sub, "region-reader", Scope{NodeID: "node-7"}, "notes:read", "billing:manage")

	got, err := svc.ListPermissions(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	want := []string{"billing:manage", "notes:read", "notes:write"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListPermissions = %v, want %v", got, want)
	}
}

func TestService_ListPermissions_NoBindings_IsEmptyNotNilError(t *testing.T) {
	svc := newTestService(t)
	got, err := svc.ListPermissions(context.Background(), Subject{TenantID: "tenant-a", UserID: "nobody"})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListPermissions = %v, want empty", got)
	}
}

// TestService_DeclaredPermissions_IsTheFrozenCatalogNotOneSubjectsGrants
// distinguishes DeclaredPermissions (D8: what the platform knows how to
// grant at all) from ListPermissions (what one Subject was actually
// granted): a Subject with no bindings at all still sees every permission
// any module declared, including rbac's own PermissionRead/PermissionManage.
func TestService_DeclaredPermissions_IsTheFrozenCatalogNotOneSubjectsGrants(t *testing.T) {
	svc := newTestService(t)

	got := svc.DeclaredPermissions()

	want := []string{"billing:manage", "notes:read", "notes:write", PermissionManage, PermissionRead}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeclaredPermissions() = %v, want %v", got, want)
	}

	// No grant was ever made -- ListPermissions for any Subject is empty --
	// yet DeclaredPermissions is unaffected, since it answers a question
	// about the catalog, not about any one subject's bindings.
	perms, err := svc.ListPermissions(context.Background(), Subject{TenantID: "tenant-a", UserID: "nobody"})
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	if len(perms) != 0 {
		t.Fatalf("ListPermissions = %v, want empty for an ungranted subject", perms)
	}
}

// TestService_DeclaredPermissions_DoesNotExposeTheFrozenCatalog proves a
// caller mutating the returned slice cannot corrupt the catalog every
// later call reads from -- catalog.permissions() already copies, and this
// pins that guarantee at the Service's own public boundary too.
func TestService_DeclaredPermissions_DoesNotExposeTheFrozenCatalog(t *testing.T) {
	svc := newTestService(t)

	first := svc.DeclaredPermissions()
	first[0] = "tampered"

	second := svc.DeclaredPermissions()
	if second[0] == "tampered" {
		t.Fatalf("mutating the first result corrupted the frozen catalog: %v", second)
	}
}

func TestService_ListPermissions_DoesNotExposeTheCachedMap(t *testing.T) {
	// The cached grant set is shared across goroutines and must stay
	// immutable; a caller that received a slice aliasing it could not be
	// stopped from holding it past an invalidation.
	svc := newTestService(t)
	sub := Subject{TenantID: "tenant-a", UserID: "user-1"}
	grant(t, svc, sub, "reader", Scope{}, "notes:read")

	first, err := svc.ListPermissions(context.Background(), sub)
	if err != nil {
		t.Fatalf("ListPermissions: %v", err)
	}
	first[0] = "tampered"

	second, err := svc.ListPermissions(context.Background(), sub)
	if err != nil {
		t.Fatalf("second ListPermissions: %v", err)
	}
	if !reflect.DeepEqual(second, []string{"notes:read"}) {
		t.Fatalf("mutating the first result changed the second: %v", second)
	}
}
