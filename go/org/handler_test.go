package org

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/org/api"
)

// fixedSubject is a SubjectResolver that answers the same way for every
// request, standing in for the authn-backed resolver a real host wires.
type fixedSubject struct {
	userID string
	ok     bool
}

func (f fixedSubject) Subject(_ *http.Request) (string, bool) { return f.userID, f.ok }

// compile-time check that the test double satisfies the seam.
var _ SubjectResolver = fixedSubject{}

// newTestHandler builds a Handler over a freshly wired module (the same
// arrangement newTestModule gives every other file in this package) and the
// given subject resolver, which may be nil.
func newTestHandler(t *testing.T, subject SubjectResolver) (*Handler, *Module, *testHost) {
	t.Helper()
	m, host := newTestModule(t)
	return NewHandler(m.tree, m.members, m.invites, subject), m, host
}

// doRequest sends req through h and returns the recorded response. req's
// context is always overridden with ctx, so the tenant (or its deliberate
// absence) is explicit at every call site rather than hidden in a shared
// helper default.
func doRequest(h *Handler, ctx context.Context, method, path string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBody decodes rec's JSON body into dst, failing t on any error.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(dst); err != nil {
		t.Fatalf("decode response body %q: %v", rec.Body.String(), err)
	}
}

// assertErrorCode fails t unless rec's body is an OrgError carrying code.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, code string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantStatus, rec.Body.String())
	}
	var got api.OrgError
	decodeBody(t, rec, &got)
	if got.Code == nil {
		t.Fatalf("error code = <nil>, want %q", code)
	}
	if *got.Code != code {
		t.Fatalf("error code = %q, want %q", *got.Code, code)
	}
}

func TestHandler_OrgCreateNode_RootThenChild(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes", api.OrgCreateNodeRequest{Name: "Acme Dental"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create root: status = %d, body %q", rec.Code, rec.Body.String())
	}
	var root api.OrgNode
	decodeBody(t, rec, &root)
	if root.ID == nil || *root.ID == "" {
		t.Fatal("created root carries no id")
	}
	if root.ParentID == nil || *root.ParentID != "" {
		t.Errorf("root.ParentID = %v, want an empty string", root.ParentID)
	}
	if root.Depth == nil || *root.Depth != 0 {
		t.Errorf("root.Depth = %v, want 0", root.Depth)
	}

	kind := "store"
	rec = doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes", api.OrgCreateNodeRequest{
		Name: "Downtown", Kind: &kind, ParentID: root.ID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create child: status = %d, body %q", rec.Code, rec.Body.String())
	}
	var child api.OrgNode
	decodeBody(t, rec, &child)
	if child.ParentID == nil || *child.ParentID != *root.ID {
		t.Errorf("child.ParentID = %v, want %q", child.ParentID, *root.ID)
	}
	if child.Kind == nil || *child.Kind != "store" {
		t.Errorf("child.Kind = %v, want %q", child.Kind, "store")
	}
}

func TestHandler_OrgCreateNode_SecondRoot_Returns409(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes", api.OrgCreateNodeRequest{Name: "First"})
	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes", api.OrgCreateNodeRequest{Name: "Second"})
	assertErrorCode(t, rec, http.StatusConflict, ErrRootAlreadyExists.Code)
}

func TestHandler_OrgCreateNode_MalformedBody_Returns400(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/org/nodes", strings.NewReader("{not json")).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertErrorCode(t, rec, http.StatusBadRequest, ErrInvalidRequestBody.Code)
}

func TestHandler_OrgListNodes_NoRootYet_ReturnsEmptyList(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/nodes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var resp api.OrgListNodesResponse
	decodeBody(t, rec, &resp)
	if resp.Nodes == nil || len(*resp.Nodes) != 0 {
		t.Errorf("Nodes = %v, want an empty (non-nil) list", resp.Nodes)
	}
}

func TestHandler_OrgListNodes_WithoutParentID_ReturnsWholeTree(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/nodes", nil)
	var resp api.OrgListNodesResponse
	decodeBody(t, rec, &resp)
	if resp.Nodes == nil || len(*resp.Nodes) != 3 {
		t.Fatalf("Nodes = %v, want 3 (root + 2 children)", resp.Nodes)
	}
}

func TestHandler_OrgListNodes_WithParentID_ReturnsOnlyChildren(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/nodes?parentId="+root.ID, nil)
	var resp api.OrgListNodesResponse
	decodeBody(t, rec, &resp)
	if resp.Nodes == nil || len(*resp.Nodes) != 2 {
		t.Fatalf("Nodes = %v, want the 2 children", resp.Nodes)
	}
}

func TestHandler_OrgGetNode_NotFound_Returns404(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/nodes/does-not-exist", nil)
	assertErrorCode(t, rec, http.StatusNotFound, ErrNodeNotFound.Code)
}

func TestHandler_OrgRenameNode(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodPatch, "/api/v1/org/nodes/"+root.ID, api.OrgRenameNodeRequest{Name: "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var node api.OrgNode
	decodeBody(t, rec, &node)
	if node.Name == nil || *node.Name != "Renamed" {
		t.Errorf("Name = %v, want %q", node.Name, "Renamed")
	}
}

func TestHandler_OrgMoveNode_IntoOwnSubtree_Returns409(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes/"+root.ID+"/move", api.OrgMoveNodeRequest{ParentID: left.ID})
	assertErrorCode(t, rec, http.StatusBadRequest, ErrCycleNotAllowed.Code)
}

func TestHandler_OrgMoveNode_Success(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.tree, ctx)
	_ = root

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/nodes/"+right.ID+"/move", api.OrgMoveNodeRequest{ParentID: left.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var node api.OrgNode
	decodeBody(t, rec, &node)
	if node.ParentID == nil || *node.ParentID != left.ID {
		t.Errorf("ParentID = %v, want %q", node.ParentID, left.ID)
	}
}

// TestHandler_OrgDeleteNode_WithChildrenNoCascade_Returns409 deletes a
// non-root node that has a child, since deleting the tenant root itself
// reports org.root_not_deletable regardless of whether it has children --
// see TreeService.Delete's own doc comment.
func TestHandler_OrgDeleteNode_WithChildrenNoCascade_Returns409(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	_, left, _ := seedTree(t, m.tree, ctx)
	if _, err := m.tree.CreateChild(ctx, left.ID, "grandchild", "store"); err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	rec := doRequest(h, ctx, http.MethodDelete, "/api/v1/org/nodes/"+left.ID, nil)
	assertErrorCode(t, rec, http.StatusConflict, ErrNodeHasChildren.Code)
}

// TestHandler_OrgDeleteNode_Cascade_Returns204 deletes a non-root node that
// itself has a child, since the tenant root can never be deleted at all
// (org.root_not_deletable), cascade or not -- see TreeService.Delete's own
// doc comment.
func TestHandler_OrgDeleteNode_Cascade_Returns204(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	_, left, right := seedTree(t, m.tree, ctx)
	grandchild, err := m.tree.CreateChild(ctx, left.ID, "grandchild", "store")
	if err != nil {
		t.Fatalf("CreateChild: %v", err)
	}

	rec := doRequest(h, ctx, http.MethodDelete, "/api/v1/org/nodes/"+left.ID+"?cascade=true", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if _, err := m.tree.Get(ctx, left.ID); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("the deleted node still resolves: err = %v", err)
	}
	if _, err := m.tree.Get(ctx, grandchild.ID); !hasCode(err, ErrNodeNotFound.Code) {
		t.Errorf("the cascaded grandchild still resolves: err = %v", err)
	}
	if _, err := m.tree.Get(ctx, right.ID); err != nil {
		t.Errorf("the sibling was affected by the cascade: %v", err)
	}
}

func TestHandler_OrgListMembers_NoRootYet_ReturnsEmptyList(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var resp api.OrgListMembersResponse
	decodeBody(t, rec, &resp)
	if resp.Members == nil || len(*resp.Members) != 0 {
		t.Errorf("Members = %v, want an empty (non-nil) list", resp.Members)
	}
}

// TestHandler_OrgListMembers_ScopesToSubtree is the round's dental-SaaS
// acceptance shape made a handler-level test: a member bound to one store is
// visible from the group above it and invisible from the sibling store next
// to it.
func TestHandler_OrgListMembers_ScopesToSubtree(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, left, right := seedTree(t, m.tree, ctx)

	if _, err := m.members.Add(ctx, "u-left", left.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/members?nodeId="+root.ID, nil)
	var resp api.OrgListMembersResponse
	decodeBody(t, rec, &resp)
	if resp.Members == nil || len(*resp.Members) != 1 || *(*resp.Members)[0].UserID != "u-left" {
		t.Fatalf("members from the group = %v, want exactly u-left", resp.Members)
	}

	rec = doRequest(h, ctx, http.MethodGet, "/api/v1/org/members?nodeId="+right.ID, nil)
	decodeBody(t, rec, &resp)
	if resp.Members == nil || len(*resp.Members) != 0 {
		t.Fatalf("members from the sibling store = %v, want none", resp.Members)
	}
}

func TestHandler_OrgRemoveMember_LastActiveMember_Returns409(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)
	if _, err := m.members.Add(ctx, "u-1", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := doRequest(h, ctx, http.MethodDelete, "/api/v1/org/members/u-1", nil)
	assertErrorCode(t, rec, http.StatusConflict, ErrMemberNotRemovable.Code)
}

func TestHandler_OrgRemoveMember_NotTheLastMember_Returns204(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)
	if _, err := m.members.Add(ctx, "u-1", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, err := m.members.Add(ctx, "u-2", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rec := doRequest(h, ctx, http.MethodDelete, "/api/v1/org/members/u-1", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestHandler_OrgCreateInvitation_NoSubjectResolver_Returns401(t *testing.T) {
	h, m, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/invitations", api.OrgCreateInvitationRequest{
		Email: "invitee@example.test", NodeID: root.ID,
	})
	assertErrorCode(t, rec, http.StatusUnauthorized, ErrSubjectUnresolved.Code)
}

func TestHandler_OrgCreateInvitation_UnresolvedSubject_Returns401(t *testing.T) {
	h, m, _ := newTestHandler(t, fixedSubject{ok: false})
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/invitations", api.OrgCreateInvitationRequest{
		Email: "invitee@example.test", NodeID: root.ID,
	})
	assertErrorCode(t, rec, http.StatusUnauthorized, ErrSubjectUnresolved.Code)
}

// TestHandler_OrgCreateInvitation_Success_NeverExposesEmailOrToken pins the
// PII rule this module holds itself to everywhere else (invite.go,
// invitation.go): the response is a spec-generated api.OrgInvitation, which
// has no field the plaintext address or the bearer token could occupy.
func TestHandler_OrgCreateInvitation_Success_NeverExposesEmailOrToken(t *testing.T) {
	h, m, host := newTestHandler(t, fixedSubject{userID: "u-inviter", ok: true})
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/invitations", api.OrgCreateInvitationRequest{
		Email: "invitee@example.test", NodeID: root.ID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "invitee@example.test") {
		t.Fatal("the response body leaks the invitee's plaintext address")
	}

	var inv api.OrgInvitation
	decodeBody(t, rec, &inv)
	if inv.ID == nil || *inv.ID == "" {
		t.Fatal("created invitation carries no id")
	}
	if inv.EmailIndex == nil || *inv.EmailIndex == "" {
		t.Error("EmailIndex is empty, want the blind index")
	}
	if inv.Status == nil || *inv.Status != InvitationStatusPending {
		t.Errorf("Status = %v, want %q", inv.Status, InvitationStatusPending)
	}

	messages := host.mailer.messages()
	if len(messages) != 1 {
		t.Fatalf("mailer recorded %d message(s), want 1", len(messages))
	}
	if !strings.Contains(messages[0].Text, testLinkBase) {
		t.Error("the invitation email carries no accept link")
	}
}

func TestHandler_OrgListInvitations_ReturnsPending(t *testing.T) {
	h, m, _ := newTestHandler(t, fixedSubject{userID: "u-inviter", ok: true})
	ctx := tenantCtx("tenant-a")
	root, _, _ := seedTree(t, m.tree, ctx)

	doRequest(h, ctx, http.MethodPost, "/api/v1/org/invitations", api.OrgCreateInvitationRequest{
		Email: "invitee@example.test", NodeID: root.ID,
	})

	rec := doRequest(h, ctx, http.MethodGet, "/api/v1/org/invitations", nil)
	var resp api.OrgListInvitationsResponse
	decodeBody(t, rec, &resp)
	if resp.Invitations == nil || len(*resp.Invitations) != 1 {
		t.Fatalf("Invitations = %v, want exactly 1", resp.Invitations)
	}
}

func TestHandler_OrgAcceptInvitation_NoSubjectResolver_Returns401(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)
	ctx := tenantCtx("tenant-a")

	rec := doRequest(h, ctx, http.MethodPost, "/api/v1/org/invitations/accept", api.OrgAcceptInvitationRequest{Token: "whatever"})
	assertErrorCode(t, rec, http.StatusUnauthorized, ErrSubjectUnresolved.Code)
}

// TestHandler_OrgAcceptInvitation_Success is the invite/accept round trip
// through the HTTP surface: an invitation created by one caller is accepted
// by another, producing a membership bound to the invited node.
func TestHandler_OrgAcceptInvitation_Success(t *testing.T) {
	inviteH, m, host := newTestHandler(t, fixedSubject{userID: "u-inviter", ok: true})
	ctx := tenantCtx("tenant-a")
	root, left, _ := seedTree(t, m.tree, ctx)
	// The inviter must be a member for org's own invariants to hold once
	// rbac exists; today nothing enforces it, so seeding it costs nothing
	// and keeps this test's tenant shape realistic.
	if _, err := m.members.Add(ctx, "u-inviter", root.ID); err != nil {
		t.Fatalf("Add: %v", err)
	}

	createRec := doRequest(inviteH, ctx, http.MethodPost, "/api/v1/org/invitations", api.OrgCreateInvitationRequest{
		Email: "invitee@example.test", NodeID: left.ID,
	})
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create invitation: status = %d, body %q", createRec.Code, createRec.Body.String())
	}

	messages := host.mailer.messages()
	if len(messages) != 1 {
		t.Fatalf("mailer recorded %d message(s), want 1", len(messages))
	}
	idx := strings.Index(messages[0].Text, testLinkBase)
	if idx < 0 {
		t.Fatal("no accept link found in the invitation email")
	}
	token := strings.TrimSpace(messages[0].Text[idx+len(testLinkBase):])
	token, _, _ = strings.Cut(token, "\n")

	acceptH := NewHandler(m.tree, m.members, m.invites, fixedSubject{userID: "u-invitee", ok: true})
	acceptRec := doRequest(acceptH, ctx, http.MethodPost, "/api/v1/org/invitations/accept", api.OrgAcceptInvitationRequest{Token: token})
	if acceptRec.Code != http.StatusOK {
		t.Fatalf("accept: status = %d, body %q", acceptRec.Code, acceptRec.Body.String())
	}
	var membership api.OrgMembership
	decodeBody(t, acceptRec, &membership)
	if membership.UserID == nil || *membership.UserID != "u-invitee" {
		t.Errorf("UserID = %v, want %q", membership.UserID, "u-invitee")
	}
	if membership.NodeID == nil || *membership.NodeID != left.ID {
		t.Errorf("NodeID = %v, want %q", membership.NodeID, left.ID)
	}
}

func TestHandler_MustTenant_NoTenantInContext_ReturnsInternalError(t *testing.T) {
	h, _, _ := newTestHandler(t, nil)

	rec := doRequest(h, context.Background(), http.MethodGet, "/api/v1/org/nodes", nil)
	assertErrorCode(t, rec, http.StatusInternalServerError, ErrInternal.Code)
}

func TestFirstAcceptLanguage(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"zh-CN", "zh-CN"},
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh-CN"},
		{"  en-US ;q=0.9", "en-US"},
	}
	for _, tc := range tests {
		if got := firstAcceptLanguage(tc.header); got != tc.want {
			t.Errorf("firstAcceptLanguage(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// TestWriteError_UnclassifiedError_FoldsIntoInternal pins that a bare, non-
// *apperr.Error never reaches the wire as raw Go error text: writeError
// folds it into ErrInternal, matching notes' and config's own writeError
// convention.
func TestWriteError_UnclassifiedError_FoldsIntoInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, errNoCatalog)

	assertErrorCode(t, rec, http.StatusInternalServerError, ErrInternal.Code)
}
