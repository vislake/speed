package org

import (
	"encoding/json"
	"net/http"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/org/api"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// jsonContentType is the Content-Type every response below writes, matching
// notes' and config's own handler constant of the same name.
const jsonContentType = "application/json; charset=utf-8"

// Handler serves org's HTTP endpoints by implementing the spec-generated
// api.ServerInterface (see api/org-server.gen.go, regenerated from this
// module's api/openapi.yaml by task api:gen -- the compile-time assertion at
// the bottom of this file is what makes "spec changed, handler not" a
// compile failure instead of a runtime surprise).
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path:
// every method reads the tenant tenancy.Middleware already resolved into the
// request context -- via pkgcore.MustTenantFromContext, both directly here
// and, redundantly, again inside every dbkit.Repository[T] call underneath
// -- and never from a request parameter, header or body, per root CLAUDE.md's
// multi-tenant isolation rule.
//
// The three services (tree, members, invites) are the SAME instances
// Module.Register attached to the host's registry; Handler holds no data
// access of its own.
type Handler struct {
	tree    *TreeService
	members *MemberService
	invites *InviteService
	subject SubjectResolver
	mux     *http.ServeMux
}

// NewHandler returns a Handler serving tree, members and invites, resolving
// the authenticated caller through subject. subject may be nil, in which
// case every endpoint that needs a caller identity (creating or accepting an
// invitation) fails closed with ErrSubjectUnresolved rather than guessing
// one -- see SubjectResolver's own doc comment.
//
// The returned Handler's routing is registered by the generated
// api.HandlerFromMux helper: it derives this module's method+path patterns
// from the "paths:" keys of api/openapi.yaml itself, exactly as notes'
// NewHandler does for its own single route.
func NewHandler(tree *TreeService, members *MemberService, invites *InviteService, subject SubjectResolver) *Handler {
	h := &Handler{tree: tree, members: members, invites: invites, subject: subject}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// mustTenant resolves the caller's tenant, annotating the request's span.
// Unreachable in normal operation -- see notes' identical comment on its own
// equivalent call -- because a host must never allowlist org's routes, so
// tenancy.Middleware has already rejected anything that could reach here
// with no resolved tenant. Handled anyway, never assumed away: every
// Repository call underneath would otherwise fail closed with this exact
// same unwrapped error one layer down, with a less specific log line.
func mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	ctx := r.Context()
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	obs.AnnotateTenant(ctx)
	return tenant, true
}

// resolveSubject returns the authenticated caller's user id, writing
// ErrSubjectUnresolved and reporting false when no SubjectResolver is wired
// or it could not identify the caller. It never invents a default user.
func (h *Handler) resolveSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.subject == nil {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	userID, ok := h.subject.Subject(r)
	if !ok || userID == "" {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	return userID, true
}

// decodeJSON decodes r's body into dst, writing ErrInvalidRequestBody and
// reporting false on any decode failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, ErrInvalidRequestBody.WithCause(err))
		return false
	}
	return true
}

// OrgListNodes implements api.ServerInterface: GET /api/v1/org/nodes. With
// parentId, it returns that node's direct children; without it, the whole
// tree (the root together with everything beneath it), or an empty list
// when the tenant has no root yet -- a fresh tenant with nobody having
// called org_createNode is not an error.
func (h *Handler) OrgListNodes(w http.ResponseWriter, r *http.Request, params api.OrgListNodesParams) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	var nodes []OrgNode
	switch {
	case params.ParentID != nil && *params.ParentID != "":
		found, err := h.tree.Children(ctx, *params.ParentID)
		if err != nil {
			writeError(w, err)
			return
		}
		nodes = found
	default:
		root, err := h.tree.Root(ctx)
		switch {
		case hasCode(err, ErrNodeNotFound.Code):
			nodes = nil
		case err != nil:
			writeError(w, err)
			return
		default:
			found, subErr := h.tree.Subtree(ctx, root.ID)
			if subErr != nil {
				writeError(w, subErr)
				return
			}
			nodes = found
		}
	}

	items := make([]api.OrgNode, 0, len(nodes))
	for i := range nodes {
		items = append(items, toNodeResponse(&nodes[i]))
	}
	writeJSON(w, http.StatusOK, api.OrgListNodesResponse{Nodes: &items})
}

// OrgCreateNode implements api.ServerInterface: POST /api/v1/org/nodes.
// parentId absent or "" creates the tenant's root; otherwise it creates a
// child beneath it.
func (h *Handler) OrgCreateNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	var req api.OrgCreateNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	kind := ""
	if req.Kind != nil {
		kind = *req.Kind
	}

	var (
		node *OrgNode
		err  error
	)
	if req.ParentID == nil || *req.ParentID == "" {
		node, err = h.tree.CreateRoot(ctx, req.Name, kind)
	} else {
		node, err = h.tree.CreateChild(ctx, *req.ParentID, req.Name, kind)
	}
	if err != nil {
		writeError(w, err)
		return
	}

	obs.FromContext(ctx).Info("org node created", "node_id", node.ID, "parent_id", node.ParentID)
	writeJSON(w, http.StatusCreated, toNodeResponse(node))
}

// OrgGetNode implements api.ServerInterface: GET /api/v1/org/nodes/{nodeId}.
func (h *Handler) OrgGetNode(w http.ResponseWriter, r *http.Request, nodeID api.NodeID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	node, err := h.tree.Get(ctx, nodeID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeResponse(node))
}

// OrgRenameNode implements api.ServerInterface: PATCH
// /api/v1/org/nodes/{nodeId}.
func (h *Handler) OrgRenameNode(w http.ResponseWriter, r *http.Request, nodeID api.NodeID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	var req api.OrgRenameNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	node, err := h.tree.Rename(ctx, nodeID, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toNodeResponse(node))
}

// OrgMoveNode implements api.ServerInterface: POST
// /api/v1/org/nodes/{nodeId}/move.
func (h *Handler) OrgMoveNode(w http.ResponseWriter, r *http.Request, nodeID api.NodeID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	var req api.OrgMoveNodeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	node, err := h.tree.Move(ctx, nodeID, req.ParentID)
	if err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("org node moved", "node_id", node.ID, "new_parent_id", node.ParentID)
	writeJSON(w, http.StatusOK, toNodeResponse(node))
}

// OrgDeleteNode implements api.ServerInterface: DELETE
// /api/v1/org/nodes/{nodeId}.
func (h *Handler) OrgDeleteNode(w http.ResponseWriter, r *http.Request, nodeID api.NodeID, params api.OrgDeleteNodeParams) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	cascade := params.Cascade != nil && *params.Cascade
	if err := h.tree.Delete(ctx, nodeID, cascade); err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("org node deleted", "node_id", nodeID, "cascade", cascade)
	w.WriteHeader(http.StatusNoContent)
}

// OrgListMembers implements api.ServerInterface: GET /api/v1/org/members.
// With nodeId, it returns that node's subtree roster; without it, the whole
// tenant's roster (the root's subtree), or an empty list when the tenant has
// no root yet.
func (h *Handler) OrgListMembers(w http.ResponseWriter, r *http.Request, params api.OrgListMembersParams) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	nodeID := ""
	if params.NodeID != nil {
		nodeID = *params.NodeID
	}
	if nodeID == "" {
		root, err := h.tree.Root(ctx)
		switch {
		case hasCode(err, ErrNodeNotFound.Code):
			writeJSON(w, http.StatusOK, api.OrgListMembersResponse{Members: &[]api.OrgMembership{}})
			return
		case err != nil:
			writeError(w, err)
			return
		default:
			nodeID = root.ID
		}
	}

	members, err := h.members.List(ctx, nodeID)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.OrgMembership, 0, len(members))
	for i := range members {
		items = append(items, toMembershipResponse(&members[i]))
	}
	writeJSON(w, http.StatusOK, api.OrgListMembersResponse{Members: &items})
}

// OrgRemoveMember implements api.ServerInterface: DELETE
// /api/v1/org/members/{userId}.
func (h *Handler) OrgRemoveMember(w http.ResponseWriter, r *http.Request, userID string) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	if err := h.members.Remove(ctx, userID); err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("org member removed", "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}

// OrgCreateInvitation implements api.ServerInterface: POST
// /api/v1/org/invitations. The inviter is the authenticated caller
// SubjectResolver identifies, never a value the request body supplies.
func (h *Handler) OrgCreateInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	inviterUserID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}
	var req api.OrgCreateInvitationRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	result, err := h.invites.Invite(ctx, InviteRequest{
		Email:         req.Email,
		NodeID:        req.NodeID,
		InviterUserID: inviterUserID,
		Locale:        recipientLocale(req.Locale),
	})
	if err != nil {
		writeError(w, err)
		return
	}

	// result.Token is deliberately never read here: it is a bearer
	// credential the invitee alone receives, through the message
	// InviteService.Invite already sent -- see InviteResult's own doc
	// comment. toInvitationResponse never touches it either.
	obs.FromContext(ctx).Info("org invitation created",
		"invitation_id", result.Invitation.ID, "node_id", result.Invitation.NodeID)
	writeJSON(w, http.StatusCreated, toInvitationResponse(result.Invitation))
}

// OrgListInvitations implements api.ServerInterface: GET
// /api/v1/org/invitations.
func (h *Handler) OrgListInvitations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	invitations, err := h.invites.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.OrgInvitation, 0, len(invitations))
	for i := range invitations {
		items = append(items, toInvitationResponse(&invitations[i]))
	}
	writeJSON(w, http.StatusOK, api.OrgListInvitationsResponse{Invitations: &items})
}

// OrgAcceptInvitation implements api.ServerInterface: POST
// /api/v1/org/invitations/accept. The accepting user is the authenticated
// caller SubjectResolver identifies, never a value the request body
// supplies.
func (h *Handler) OrgAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}
	var req api.OrgAcceptInvitationRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	membership, err := h.invites.Accept(ctx, req.Token, userID)
	if err != nil {
		writeError(w, err)
		return
	}
	obs.FromContext(ctx).Info("org invitation accepted",
		"membership_id", membership.ID, "node_id", membership.NodeID)
	writeJSON(w, http.StatusOK, toMembershipResponse(membership))
}

// recipientLocale reads the RECIPIENT's locale off an
// OrgCreateInvitationRequest, or "" when the caller supplied none.
//
// This is deliberately NOT read from the request's own Accept-Language
// header: that header belongs to the authenticated inviter/operator making
// THIS HTTP call, and the invitee -- who has made no request of their own
// yet, and may not even be a user -- has no channel to reach the server
// through at invite-creation time. An earlier version of this handler read
// Accept-Language here, which silently rendered every invitation email in
// the ADMIN's own browser language, directly contradicting the "renders in
// the recipient's locale" contract this endpoint documents (root CLAUDE.md's
// i18n rule; org/api/openapi.yaml's org_createInvitation description). See
// TestHandler_OrgCreateInvitation_LocaleIsFromRequestBody_NeverAcceptLanguage.
//
// An empty return is not an error: InviteService.Invite negotiates it
// through negotiateLocale (mail.go), which falls back to the platform
// default for an empty or unrecognized locale exactly as it does for any
// other unrecognized tag.
func recipientLocale(locale *string) string {
	if locale == nil {
		return ""
	}
	return *locale
}

// toNodeResponse converts node to its spec-generated JSON response type.
// Every field of api.OrgNode is optional (hence pointer-typed) in the
// generated model; this handler always sets all of them, so every field is
// always present on the wire.
func toNodeResponse(node *OrgNode) api.OrgNode {
	return api.OrgNode{
		ID:        &node.ID,
		ParentID:  &node.ParentID,
		Path:      &node.Path,
		Depth:     &node.Depth,
		Name:      &node.Name,
		Kind:      &node.Kind,
		CreatedAt: &node.CreatedAt,
		UpdatedAt: &node.UpdatedAt,
	}
}

// toMembershipResponse converts m to its spec-generated JSON response type.
func toMembershipResponse(m *Membership) api.OrgMembership {
	return api.OrgMembership{
		MembershipID: &m.ID,
		UserID:       &m.UserID,
		NodeID:       &m.NodeID,
		Status:       &m.Status,
		CreatedAt:    &m.CreatedAt,
	}
}

// toInvitationResponse converts inv to its spec-generated JSON response
// type. It deliberately never touches inv.Email: the address is PII, and
// this module's convention (invitation.go, invite.go) is never to echo it
// into anything that leaves the process boundary -- only EmailIndex, the
// non-reversible HMAC digest, is exposed.
func toInvitationResponse(inv *Invitation) api.OrgInvitation {
	return api.OrgInvitation{
		ID:         &inv.ID,
		NodeID:     &inv.NodeID,
		EmailIndex: &inv.EmailIndex,
		Status:     &inv.Status,
		ExpiresAt:  &inv.ExpiresAt,
		CreatedAt:  &inv.CreatedAt,
	}
}

// writeJSON writes v to w as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.OrgError, the same structured-error envelope notes' and
// config's own writeError produce. An err that is not an *apperr.Error --
// meaning something below this handler did not classify it -- is folded
// into ErrInternal so a caller never sees raw Go error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.OrgError{Code: &appErr.Code}
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow (docs/internal/21-api-contract.md): add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
