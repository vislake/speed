package flowtests

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// This file proves org's router-level permission gate end to end: without
// it, the org entry of DemoRouteRules declaring the path public would let
// ANY authenticated tenant member -- holding no org permission at all --
// create, move, rename and cascade-delete nodes and remove arbitrary
// members. See the org entry's doc comment in internal/app/demo_subject.go
// for the fixed shape this file exercises.

// orgErrorBody is the {code, ...} envelope every refusal from either layer
// of org's gate (rbac.RequirePermissionFunc's coarse Can check, or
// enforceOrgNodeScope's DataScope narrowing) writes -- the same shape
// org.Handler's own writeError produces for org's OWN errors, so one type
// reads both.
type orgErrorBody struct {
	Code string `json:"code"`
}

// orgRequestStatus issues method against srv.URL+path, authenticated as
// token, with demoUser sent as the rbac gate's X-Demo-User header (empty
// omits it, falling back to the token's own verified principal, who holds
// no rbac grant in these tests' fresh accounts). Unlike orgRequest
// (org_flow_test.go), which always sends DemoOwnerUserID, this lets a test
// choose ANY identity -- this file's own tests exist specifically to prove
// the gate closes for one identity and stays open for another, so the
// identity is the one thing every call here must vary. It never fatals on
// an unexpected status: asserting a refusal happens is the point.
func orgRequestStatus(t *testing.T, srv *httptest.Server, method, path, token, demoUser string, body any) (int, orgErrorBody) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if demoUser != "" {
		req.Header.Set(app.DemoUserHeader, demoUser)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var errBody orgErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&errBody)
	return resp.StatusCode, errBody
}

// orgRequestAs is orgRequest's (org_flow_test.go) twin with one difference:
// the rbac demo identity is an explicit parameter rather than always
// DemoOwnerUserID, for a test that must drive a request as a NARROWLY
// privileged identity (the subtree-scoped grant below) and still fatal on
// an unexpected status the way every other setup call in this suite does.
func orgRequestAs(t *testing.T, srv *httptest.Server, method, path, token, demoUser string, body, out any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if demoUser != "" {
		req.Header.Set(app.DemoUserHeader, demoUser)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want 2xx; body = %s", method, path, resp.StatusCode, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response for %s %s: %v", method, path, err)
		}
	}
}

// TestOrgRouteGuards_UnprivilegedCaller_CannotManageOrgTree is THE scenario:
// DemoReaderUserID holds notes:read alone (seedDemoGrants' demoReaderRoleKey,
// internal/app/demo_subject.go) and no org permission whatsoever, yet reaches org's
// routes -- tenancy.Middleware and the fixed authn+tenancy chain never
// distinguish org's operations from any other tenant member's ordinary
// traffic, so the per-operation permission gate (the org entry of
// internal/app/demo_subject.go's DemoRouteRules) is the only layer that refuses this caller. Every case
// below must fail: without the gate all of them would succeed (2xx) -- the
// cascade-delete case would even delete the tree.
func TestOrgRouteGuards_UnprivilegedCaller_CannotManageOrgTree(t *testing.T) {
	// cfg.OnRBACReady (BuildServer, mirroring
	// TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly below)
	// replaces buildTestServer here so this test asserts the rbac DECISION
	// directly rather than only the HTTP status the coarse gate produces
	// from it -- a wrong allowance or a wrong refusal originates in the
	// decision layer, and pinning the decision here makes the failure name
	// that layer instead of leaving the HTTP/route-table plumbing
	// downstream of a correct decision to be re-diagnosed.
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by app.BuildServer")
	}

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-guard-caller")

	// The rbac-level assertion the HTTP cases below are supposed to be a
	// consequence of: demo-reader holds notes:read alone (seedDemoGrants'
	// demoReaderRoleKey, internal/app/demo_subject.go), no org:* permission whatsoever.
	readerSub := rbac.Subject{TenantID: "tenant-acme", UserID: app.DemoReaderUserID}
	for _, perm := range []string{
		org.PermissionRead, org.PermissionManage,
		org.PermissionInviteMember, org.PermissionRemoveMember,
	} {
		resource, action, ok := rbac.SplitPermission(perm)
		if !ok {
			t.Fatalf("rbac.SplitPermission(%q): malformed", perm)
		}
		allowed, err := rbacService.Can(context.Background(), readerSub, action, resource)
		if err != nil {
			t.Fatalf("rbacService.Can(demo-reader, %q): %v", perm, err)
		}
		if allowed {
			t.Fatalf("rbacService.Can(demo-reader, %q) = true, want false", perm)
		}
	}
	if perms, err := rbacService.ListPermissions(context.Background(), readerSub); err != nil {
		t.Fatalf("rbacService.ListPermissions(demo-reader): %v", err)
	} else if len(perms) != 1 || perms[0] != notes.PermissionRead {
		t.Fatalf("rbacService.ListPermissions(demo-reader) = %v, want exactly [%q]", perms, notes.PermissionRead)
	}

	// Build a real node to target, as demo-owner (BuiltinRoleOwner, seeded
	// tenant-wide by seedDemoGrants) -- orgRequest sends that identity
	// unconditionally (its own doc comment explains why).
	var root orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Guard Test Co", "kind": "group"}, &root)
	if root.ID == "" {
		t.Fatal("org_createNode returned no node id")
	}

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list_nodes", http.MethodGet, "/api/v1/org/nodes", nil},
		{
			"create_node", http.MethodPost, "/api/v1/org/nodes",
			map[string]string{"name": "unauthorized child", "kind": "store", "parentId": root.ID},
		},
		{
			"rename_node", http.MethodPatch, "/api/v1/org/nodes/" + root.ID,
			map[string]string{"name": "hijacked"},
		},
		{
			"move_node", http.MethodPost, "/api/v1/org/nodes/" + root.ID + "/move",
			map[string]string{"parentId": root.ID},
		},
		{"list_members", http.MethodGet, "/api/v1/org/members?nodeId=" + root.ID, nil},
		{"remove_member", http.MethodDelete, "/api/v1/org/members/some-other-user", nil},
		// The scenario the audit named explicitly: a cascade delete of the
		// whole subtree.
		{"delete_node_cascade", http.MethodDelete, "/api/v1/org/nodes/" + root.ID + "?cascade=true", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := orgRequestStatus(t, srv, tc.method, tc.path, token, app.DemoReaderUserID, tc.body)
			if status != http.StatusForbidden {
				t.Fatalf("%s %s as demo-reader: status = %d, want %d (body = %+v)",
					tc.method, tc.path, status, http.StatusForbidden, body)
			}
			if body.Code != rbac.ErrPermissionDenied.Code {
				t.Fatalf("%s %s as demo-reader: code = %q, want %q",
					tc.method, tc.path, body.Code, rbac.ErrPermissionDenied.Code)
			}
		})
	}

	// The refused cascade delete must have had no effect: the node is
	// still there, readable by an identity that actually holds org:read.
	var stillThere orgNode
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes/"+root.ID, token, "", nil, &stillThere)
	if stillThere.ID != root.ID {
		t.Fatalf("root node after the refused cascade delete = %+v, want it unchanged (id %q)", stillThere, root.ID)
	}
}

// TestOrgRouteGuards_PrivilegedCaller_CanStillManageOrgTree is THE
// scenario's positive twin: DemoOwnerUserID (BuiltinRoleOwner, every
// permission any module declared) can still perform every operation the
// unprivileged test above proved refused -- the gate closes for one
// identity and stays open for another, never a blunt admin-only wall over
// the whole mount.
func TestOrgRouteGuards_PrivilegedCaller_CanStillManageOrgTree(t *testing.T) {
	srv, cfg, _ := buildTestServer(t)
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-guard-owner")

	var root, child orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Owner Test Co", "kind": "group"}, &root)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Owner Test Branch", "kind": "store", "parentId": root.ID}, &child)
	if root.ID == "" || child.ID == "" {
		t.Fatalf("created nodes = %+v, %+v, want two non-empty ids", root, child)
	}

	var renamed orgNode
	orgRequest(t, srv, http.MethodPatch, "/api/v1/org/nodes/"+child.ID, token, "",
		map[string]string{"name": "Owner Test Branch Renamed"}, &renamed)
	if renamed.Name != "Owner Test Branch Renamed" {
		t.Fatalf("renamed node = %+v, want the new name applied", renamed)
	}

	var moved orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes/"+child.ID+"/move", token, "",
		map[string]string{"parentId": root.ID}, &moved)

	var listed orgListMembersResponse
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/members?nodeId="+root.ID, token, "", nil, &listed)

	orgRequest(t, srv, http.MethodDelete, "/api/v1/org/nodes/"+child.ID+"?cascade=true", token, "", nil, nil)

	status, _ := orgRequestStatus(t, srv, http.MethodGet, "/api/v1/org/nodes/"+child.ID, token, app.DemoOwnerUserID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET the deleted child as demo-owner: status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly is item 3's
// own proof: rbac's Authorizer.DataScope machinery's first REAL consumer
// (enforceOrgNodeScope, internal/app/demo_subject.go). A role granted scoped to node A
// (rbac.Scope{NodeID: nodeA.ID}, never the tenant root) lets its holder
// manage exactly A's subtree -- and refuses the identical operation
// against a SIBLING subtree and an ANCESTOR, even though Can() alone
// (the coarse gate item 1+2 add) would answer true for both: the grant
// exists somewhere in the tenant, just not there. This property is real
// only because rbacModule is wired with WithSubtreeResolver onto org's own
// Scope (internal/app/server.go) -- fails closed (denies, the tenant-wide behavior of
// pre-org-route-guards code) without it, per SubtreeResolver's own
// contract, so this test would fail before EITHER the router gate or
// its SubtreeResolver wiring landed.
func TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly(t *testing.T) {
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by app.BuildServer")
	}

	const tenant = pkgcore.TenantID("tenant-acme")
	token := registerAndAuthenticate(t, srv, cfg, tenant, "subtree-scope-owner")

	// root -> {nodeA, nodeB}, as demo-owner (tenant-wide, seeded at boot).
	var root, nodeA, nodeB orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Subtree Root", "kind": "group"}, &root)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Subtree A", "kind": "store", "parentId": root.ID}, &nodeA)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Subtree B", "kind": "store", "parentId": root.ID}, &nodeB)
	if root.ID == "" || nodeA.ID == "" || nodeB.ID == "" || nodeA.ID == nodeB.ID {
		t.Fatalf("built tree = root %+v, A %+v, B %+v, want three distinct non-empty ids", root, nodeA, nodeB)
	}

	// A role carrying every org permission, assigned to a fresh identity
	// SCOPED TO NODE A ALONE -- never the tenant root, which is what makes
	// this grant narrower than every seedDemoGrants grant in this app
	// (every demo grant it seeds stays tenant-wide; this test's grant is
	// the one exception).
	const subtreeAdminRoleKey = "org-subtree-admin"
	const subtreeAdminUserID = "demo-org-subtree-admin"
	tenantCtx := pkgcore.WithTenant(context.Background(), tenant)
	if _, defErr := rbacService.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            subtreeAdminRoleKey,
		DescriptionKey: "rbac.role.member",
		Permissions: []string{
			org.PermissionRead, org.PermissionManage,
			org.PermissionInviteMember, org.PermissionRemoveMember,
		},
	}); defErr != nil {
		t.Fatalf("DefineRole(%q): %v", subtreeAdminRoleKey, defErr)
	}
	sub := rbac.Subject{TenantID: tenant, UserID: subtreeAdminUserID}
	if assignErr := rbacService.AssignRole(tenantCtx, sub, subtreeAdminRoleKey, rbac.Scope{NodeID: nodeA.ID}); assignErr != nil {
		t.Fatalf("AssignRole(%q, node %q): %v", subtreeAdminRoleKey, nodeA.ID, assignErr)
	}

	// IN scope: renaming node A itself succeeds.
	var renamedA orgNode
	orgRequestAs(t, srv, http.MethodPatch, "/api/v1/org/nodes/"+nodeA.ID, token, subtreeAdminUserID,
		map[string]string{"name": "Subtree A Renamed"}, &renamedA)
	if renamedA.Name != "Subtree A Renamed" {
		t.Fatalf("renamed A = %+v, want the new name applied", renamedA)
	}

	// IN scope: creating a child UNDER A succeeds.
	var childOfA orgNode
	orgRequestAs(t, srv, http.MethodPost, "/api/v1/org/nodes", token, subtreeAdminUserID,
		map[string]string{"name": "Under A", "kind": "team", "parentId": nodeA.ID}, &childOfA)
	if childOfA.ParentID != nodeA.ID {
		t.Fatalf("child of A = %+v, want parentId %q", childOfA, nodeA.ID)
	}

	// OUT of scope: renaming the SIBLING node B is refused, even though
	// Can() alone would allow it -- this subject's grant exists, just not
	// there.
	status, body := orgRequestStatus(t, srv, http.MethodPatch, "/api/v1/org/nodes/"+nodeB.ID, token, subtreeAdminUserID,
		map[string]string{"name": "hijacked sibling"})
	if status != http.StatusForbidden {
		t.Fatalf("PATCH sibling B as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}
	if body.Code != rbac.ErrPermissionDenied.Code {
		t.Fatalf("PATCH sibling B as subtree admin: code = %q, want %q", body.Code, rbac.ErrPermissionDenied.Code)
	}

	// OUT of scope: creating a child UNDER the sibling B is refused.
	status, body = orgRequestStatus(t, srv, http.MethodPost, "/api/v1/org/nodes", token, subtreeAdminUserID,
		map[string]string{"name": "Under B", "kind": "team", "parentId": nodeB.ID})
	if status != http.StatusForbidden {
		t.Fatalf("create under sibling B as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}

	// OUT of scope: renaming the ANCESTOR root is refused too -- a
	// subtree grant does not widen upward any more than it widens
	// sideways.
	status, body = orgRequestStatus(t, srv, http.MethodPatch, "/api/v1/org/nodes/"+root.ID, token, subtreeAdminUserID,
		map[string]string{"name": "hijacked root"})
	if status != http.StatusForbidden {
		t.Fatalf("PATCH ancestor root as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}

	// Confirm the sibling was never actually touched.
	var stillB orgNode
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes/"+nodeB.ID, token, "", nil, &stillB)
	if stillB.Name != "Subtree B" {
		t.Fatalf("sibling B after the refused attempts = %+v, want its name unchanged", stillB)
	}
}

// TestOrgRouteGuards_SubtreeGrant_MovesWithinItsSubtreeOnly proves the
// node-scope gate's move handling: OrgMoveNode names TWO nodes at once --
// the node being moved (the path parameter) and the parent it moves INTO
// (the body's parentId), and enforceOrgNodeScope must hold BOTH inside a
// subtree-scoped grant. Allowing only the source would let the holder move
// a node they do not own into a subtree they do not own either; allowing
// only the destination would let them move a node they DO own out into
// unmanaged territory. Only a subtree-scoped subject reaches this branch
// at all -- a tenant-wide grant answers enforceOrgNodeScope before any
// target is computed (see that function's own doc comment).
func TestOrgRouteGuards_SubtreeGrant_MovesWithinItsSubtreeOnly(t *testing.T) {
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by app.BuildServer")
	}

	const tenant = pkgcore.TenantID("tenant-acme")
	token := registerAndAuthenticate(t, srv, cfg, tenant, "subtree-move-owner")

	// root -> {A -> {A1, A2}, B}, as demo-owner (tenant-wide, seeded at boot).
	var root, nodeA, nodeB, nodeA1, nodeA2 orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Move Root", "kind": "group"}, &root)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Move A", "kind": "store", "parentId": root.ID}, &nodeA)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Move B", "kind": "store", "parentId": root.ID}, &nodeB)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Move A1", "kind": "store", "parentId": nodeA.ID}, &nodeA1)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Move A2", "kind": "team", "parentId": nodeA.ID}, &nodeA2)
	if root.ID == "" || nodeA.ID == "" || nodeB.ID == "" || nodeA1.ID == "" || nodeA2.ID == "" {
		t.Fatalf("built tree = root %+v, A %+v, B %+v, A1 %+v, A2 %+v, want five non-empty ids",
			root, nodeA, nodeB, nodeA1, nodeA2)
	}

	// The same narrowly-scoped grant the sibling test defines: every org
	// permission, assigned to a fresh identity scoped to node A alone.
	const moverRoleKey = "org-subtree-mover"
	const moverUserID = "demo-org-subtree-mover"
	tenantCtx := pkgcore.WithTenant(context.Background(), tenant)
	if _, defErr := rbacService.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            moverRoleKey,
		DescriptionKey: "rbac.role.member",
		Permissions: []string{
			org.PermissionRead, org.PermissionManage,
			org.PermissionInviteMember, org.PermissionRemoveMember,
		},
	}); defErr != nil {
		t.Fatalf("DefineRole(%q): %v", moverRoleKey, defErr)
	}
	sub := rbac.Subject{TenantID: tenant, UserID: moverUserID}
	if assignErr := rbacService.AssignRole(tenantCtx, sub, moverRoleKey, rbac.Scope{NodeID: nodeA.ID}); assignErr != nil {
		t.Fatalf("AssignRole(%q, node %q): %v", moverRoleKey, nodeA.ID, assignErr)
	}

	// IN scope: moving A2 under its sibling A1 -- both ends inside A's
	// subtree -- succeeds, and the response proves the move landed.
	var moved orgNode
	orgRequestAs(t, srv, http.MethodPost, "/api/v1/org/nodes/"+nodeA2.ID+"/move", token, moverUserID,
		map[string]string{"parentId": nodeA1.ID}, &moved)
	if moved.ParentID != nodeA1.ID {
		t.Fatalf("moved A2 = %+v, want parentId %q", moved, nodeA1.ID)
	}

	// OUT of scope: moving A1 INTO the sibling subtree B is refused -- the
	// destination is outside the grant even though the source is inside it.
	status, body := orgRequestStatus(t, srv, http.MethodPost, "/api/v1/org/nodes/"+nodeA1.ID+"/move", token, moverUserID,
		map[string]string{"parentId": nodeB.ID})
	if status != http.StatusForbidden {
		t.Fatalf("move A1 into sibling B as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}
	if body.Code != rbac.ErrPermissionDenied.Code {
		t.Fatalf("move A1 into sibling B as subtree admin: code = %q, want %q", body.Code, rbac.ErrPermissionDenied.Code)
	}

	// OUT of scope: moving the sibling B INTO A's subtree is refused too --
	// the source is outside the grant even though the destination is inside.
	status, body = orgRequestStatus(t, srv, http.MethodPost, "/api/v1/org/nodes/"+nodeB.ID+"/move", token, moverUserID,
		map[string]string{"parentId": nodeA1.ID})
	if status != http.StatusForbidden {
		t.Fatalf("move sibling B into A as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}
	if body.Code != rbac.ErrPermissionDenied.Code {
		t.Fatalf("move sibling B into A as subtree admin: code = %q, want %q", body.Code, rbac.ErrPermissionDenied.Code)
	}

	// Neither refused move took effect: A1 and B still hang where they did.
	var fetchedA1, fetchedB orgNode
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes/"+nodeA1.ID, token, "", nil, &fetchedA1)
	orgRequest(t, srv, http.MethodGet, "/api/v1/org/nodes/"+nodeB.ID, token, "", nil, &fetchedB)
	if fetchedA1.ParentID != nodeA.ID {
		t.Fatalf("A1 after the refused moves = %+v, want parentId %q", fetchedA1, nodeA.ID)
	}
	if fetchedB.ParentID != root.ID {
		t.Fatalf("B after the refused moves = %+v, want parentId %q", fetchedB, root.ID)
	}
}

// TestOrgRouteGuards_SubtreeGrant_InvitesAndRemovesItsOwnSubtreeMember
// proves the gate's other two body/identity-shaped targets hold the same
// subtree line: OrgCreateInvitation names its target node in the REQUEST
// BODY (nodeId), and OrgRemoveMember names a USER in the path, whose node
// enforceOrgNodeScope resolves through org's MemberService to the
// membership the user actually holds. A grant scoped to node A must be
// able to invite into A, must be refused when it names a node outside A,
// must be able to remove a member bound to A, and must never be answered a
// scope refusal invented for a user who has no membership at all -- that
// latter case is the handler's own not-found to answer (see
// enforceOrgNodeScope's own doc comment).
func TestOrgRouteGuards_SubtreeGrant_InvitesAndRemovesItsOwnSubtreeMember(t *testing.T) {
	cfg := testConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by app.BuildServer")
	}

	const tenant = pkgcore.TenantID("tenant-acme")
	token := registerAndAuthenticate(t, srv, cfg, tenant, "subtree-invite-owner")

	// root -> {A, B}, as demo-owner (tenant-wide, seeded at boot).
	var root, nodeA, nodeB orgNode
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Invite Root", "kind": "group"}, &root)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Invite A", "kind": "store", "parentId": root.ID}, &nodeA)
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/nodes", token, "",
		map[string]string{"name": "Invite B", "kind": "store", "parentId": root.ID}, &nodeB)
	if root.ID == "" || nodeA.ID == "" || nodeB.ID == "" {
		t.Fatalf("built tree = root %+v, A %+v, B %+v, want three non-empty ids", root, nodeA, nodeB)
	}

	const inviterRoleKey = "org-subtree-inviter"
	const inviterUserID = "demo-org-subtree-inviter"
	tenantCtx := pkgcore.WithTenant(context.Background(), tenant)
	if _, defErr := rbacService.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            inviterRoleKey,
		DescriptionKey: "rbac.role.member",
		Permissions: []string{
			org.PermissionRead, org.PermissionManage,
			org.PermissionInviteMember, org.PermissionRemoveMember,
		},
	}); defErr != nil {
		t.Fatalf("DefineRole(%q): %v", inviterRoleKey, defErr)
	}
	sub := rbac.Subject{TenantID: tenant, UserID: inviterUserID}
	if assignErr := rbacService.AssignRole(tenantCtx, sub, inviterRoleKey, rbac.Scope{NodeID: nodeA.ID}); assignErr != nil {
		t.Fatalf("AssignRole(%q, node %q): %v", inviterRoleKey, nodeA.ID, assignErr)
	}

	// OUT of scope: inviting into the sibling subtree B is refused.
	const inviteeEmail = "subtree-invitee@example.com"
	status, body := orgRequestStatus(t, srv, http.MethodPost, "/api/v1/org/invitations", token, inviterUserID,
		map[string]string{"email": inviteeEmail, "nodeId": nodeB.ID})
	if status != http.StatusForbidden {
		t.Fatalf("invite into sibling B as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusForbidden, body)
	}
	if body.Code != rbac.ErrPermissionDenied.Code {
		t.Fatalf("invite into sibling B as subtree admin: code = %q, want %q", body.Code, rbac.ErrPermissionDenied.Code)
	}

	// IN scope: the same invitation naming A succeeds, and the invitee
	// accepts it through org's real accept route -- the token recovered
	// from the mail org sent, exactly as org_flow_test.go's own invitation
	// journey does.
	var invitation struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		NodeID string `json:"nodeId"`
	}
	orgRequestAs(t, srv, http.MethodPost, "/api/v1/org/invitations", token, inviterUserID,
		map[string]string{"email": inviteeEmail, "nodeId": nodeA.ID}, &invitation)
	if invitation.Status != "pending" || invitation.NodeID != nodeA.ID {
		t.Fatalf("invitation = %+v, want status \"pending\" on node %q", invitation, nodeA.ID)
	}

	inviteeUserID := registerFreshAccount(t, srv, inviteeEmail, testPassword)
	acceptToken := tokenFromMail(t, mailer.last(t))
	var membership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", "", inviteeUserID,
		map[string]string{"token": acceptToken}, &membership)
	if membership.UserID != inviteeUserID || membership.NodeID != nodeA.ID {
		t.Fatalf("membership after accept = %+v, want userId %q bound to node %q", membership, inviteeUserID, nodeA.ID)
	}

	// A second member bound to A, so the removal below is not org's own
	// last-active-member refusal (ErrMemberNotRemovable): the demo
	// identities hold rbac grants but no membership rows, so the accepted
	// invitee would otherwise be the tenant's only active member.
	const secondInviteeEmail = "subtree-invitee-two@example.com"
	var secondInvitation struct {
		Status string `json:"status"`
		NodeID string `json:"nodeId"`
	}
	orgRequestAs(t, srv, http.MethodPost, "/api/v1/org/invitations", token, inviterUserID,
		map[string]string{"email": secondInviteeEmail, "nodeId": nodeA.ID}, &secondInvitation)
	if secondInvitation.Status != "pending" || secondInvitation.NodeID != nodeA.ID {
		t.Fatalf("second invitation = %+v, want status \"pending\" on node %q", secondInvitation, nodeA.ID)
	}
	secondInviteeUserID := registerFreshAccount(t, srv, secondInviteeEmail, testPassword)
	secondAcceptToken := tokenFromMail(t, mailer.last(t))
	var secondMembership orgMembership
	orgRequest(t, srv, http.MethodPost, "/api/v1/org/invitations/accept", "", secondInviteeUserID,
		map[string]string{"token": secondAcceptToken}, &secondMembership)
	if secondMembership.UserID != secondInviteeUserID || secondMembership.NodeID != nodeA.ID {
		t.Fatalf("second membership after accept = %+v, want userId %q bound to node %q", secondMembership, secondInviteeUserID, nodeA.ID)
	}

	// An unknown user carries no membership to resolve, so the gate lets
	// the request through to org's own lookup, which answers its own
	// not-found -- deliberately NOT a 403 manufactured for a user who may
	// not exist (see enforceOrgNodeScope's own doc comment).
	status, body = orgRequestStatus(t, srv, http.MethodDelete, "/api/v1/org/members/never-a-member", token, inviterUserID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("remove an unknown member as subtree admin: status = %d, want %d (body = %+v)", status, http.StatusNotFound, body)
	}
	if body.Code != org.ErrMembershipNotFound.Code {
		t.Fatalf("remove an unknown member as subtree admin: code = %q, want %q", body.Code, org.ErrMembershipNotFound.Code)
	}

	// IN scope: removing the member bound to A succeeds, and a second
	// attempt proves the first one actually landed.
	orgRequestAs(t, srv, http.MethodDelete, "/api/v1/org/members/"+inviteeUserID, token, inviterUserID, nil, nil)
	status, body = orgRequestStatus(t, srv, http.MethodDelete, "/api/v1/org/members/"+inviteeUserID, token, inviterUserID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("remove the same member twice as subtree admin: second status = %d, want %d (body = %+v)", status, http.StatusNotFound, body)
	}
	if body.Code != org.ErrMembershipNotFound.Code {
		t.Fatalf("remove the same member twice as subtree admin: second code = %q, want %q", body.Code, org.ErrMembershipNotFound.Code)
	}
}
