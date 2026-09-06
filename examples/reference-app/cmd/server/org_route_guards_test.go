package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
)

// This file proves the org-route-guards round's fix end to end: before it,
// demoRouteGuards[orgRoutePath] was routePublic, so ANY authenticated
// tenant member -- holding no org permission at all -- could create, move,
// rename and cascade-delete nodes and remove arbitrary members. See
// demo_subject.go's demoRouteGuards doc comment on org's path, and
// go/org/AGENTS.md's own permission table, for the fixed shape this file
// exercises.

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
// (org_flow_test.go), which always sends demoOwnerUserID, this lets a test
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
		req.Header.Set(demoUserHeader, demoUser)
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
// demoOwnerUserID, for a test that must drive a request as a NARROWLY
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
		req.Header.Set(demoUserHeader, demoUser)
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
// demoReaderUserID holds notes:read alone (seedDemoGrants' demoReaderRoleKey,
// demo_subject.go), no org permission whatsoever, yet reaches org's route --
// still true today, since tenancy.Middleware and the fixed authn+tenancy
// chain never distinguished org's operations from any other tenant member's
// ordinary traffic. Before the org-route-guards fix, every case below
// SUCCEEDED (2xx): demoRouteGuards[orgRoutePath] was routePublic, so no
// permission was ever checked and the cascade-delete case actually deleted
// the tree. This test fails on pre-fix code and passes once org's route is
// gated per operation (demo_subject.go's guardOrgRoute).
func TestOrgRouteGuards_UnprivilegedCaller_CannotManageOrgTree(t *testing.T) {
	// This test's own review flagged an unreproduced, once-in-roughly-eight
	// flake where a full-package `go test ./... -race` run let demo-reader
	// reach org's business logic despite holding no org:* permission --
	// with no static logic error found in guardOrgRoute, orgPermissionFor,
	// demoSubjectResolver or rbac's own Can/RequirePermissionFunc, and no
	// reproduction across several full-package runs under real concurrent
	// load while investigating it. cfg.OnRBACReady (buildServer, mirroring
	// TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly below)
	// replaces buildTestServer here so this test can assert the rbac
	// DECISION directly -- exactly what the flake investigation checked by
	// hand with temporary debug instrumentation -- rather than only the
	// HTTP status the coarse gate produces from it. If the flake recurs,
	// this pins whether the decision itself was wrong or the bug lies
	// somewhere in the HTTP/route-table plumbing downstream of a correct
	// decision, instead of requiring that distinction to be re-diagnosed
	// from scratch.
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by buildServer")
	}

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "org-guard-caller")

	// The rbac-level assertion the HTTP cases below are supposed to be a
	// consequence of: demo-reader holds notes:read alone (seedDemoGrants'
	// demoReaderRoleKey, demo_subject.go), no org:* permission whatsoever.
	readerSub := rbac.Subject{TenantID: "tenant-acme", UserID: demoReaderUserID}
	for _, perm := range []string{
		org.PermissionRead, org.PermissionManage,
		org.PermissionInviteMember, org.PermissionRemoveMember,
	} {
		resource, action, ok := splitDemoPermission(perm)
		if !ok {
			t.Fatalf("splitDemoPermission(%q): malformed", perm)
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
			status, body := orgRequestStatus(t, srv, tc.method, tc.path, token, demoReaderUserID, tc.body)
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
// scenario's positive twin: demoOwnerUserID (BuiltinRoleOwner, every
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

	status, _ := orgRequestStatus(t, srv, http.MethodGet, "/api/v1/org/nodes/"+child.ID, token, demoOwnerUserID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("GET the deleted child as demo-owner: status = %d, want %d", status, http.StatusNotFound)
	}
}

// TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly is item 3's
// own proof: rbac's Authorizer.DataScope machinery's first REAL consumer
// (enforceOrgNodeScope, demo_subject.go). A role granted scoped to node A
// (rbac.Scope{NodeID: nodeA.ID}, never the tenant root) lets its holder
// manage exactly A's subtree -- and refuses the identical operation
// against a SIBLING subtree and an ANCESTOR, even though Can() alone
// (the coarse gate item 1+2 add) would answer true for both: the grant
// exists somewhere in the tenant, just not there. This property is real
// only because rbacModule is wired with WithSubtreeResolver onto org's own
// Scope (server.go) -- fails closed (denies, the tenant-wide behavior of
// pre-org-route-guards code) without it, per SubtreeResolver's own
// contract, so this test would fail before EITHER this round's gate or
// its SubtreeResolver wiring landed.
func TestOrgRouteGuards_SubtreeScopedGrant_ManagesOwnSubtreeOnly(t *testing.T) {
	cfg := testConfig(t)
	var rbacService *rbac.Service
	cfg.OnRBACReady = func(svc *rbac.Service) { rbacService = svc }

	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if rbacService == nil {
		t.Fatal("cfg.OnRBACReady was never called by buildServer")
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
	// (that function's own doc comment: "this example has no organization
	// tree, so it wires no rbac.SubtreeResolver either" no longer describes
	// server.go's real wiring, but every DEMO grant it seeds stays
	// tenant-wide regardless -- this test's grant is the one exception).
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
