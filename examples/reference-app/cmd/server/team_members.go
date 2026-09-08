package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"

	obs "github.com/vislake/speed/go/observability"
)

// team_members.go mounts the reference-app's own "who works in this
// clinic, and who each of them is" answer: the caller's tenant's org
// membership roster, each row enriched with the display identity of the
// person behind it.
//
// Why the answer lives HERE, composed by the host, rather than inside
// go/org: org's member object carries userId and nothing else, by its own
// module-boundary rule (go/org stores no other identity data about a
// user -- "An id in authn's users table, carried as an opaque string",
// its membership model's own doc comment). The name therefore has to come
// from authn's users table -- the account that joined -- and the
// assembling application is the only place both sides are reachable
// (authn and org are peers; neither imports the other, and the app above
// both composes them). This route is that composition, the clinic-name
// answer's sibling (clinic_name.go): an app-owned route mounted beside
// the other hand-written host routes, deliberately not part of any
// module's OpenAPI fragment, because no module owns the join.
//
// What the enrichment draws from: authn's own users row, read by user id
// through authn.Service.Users().FindByID -- the same identity-domain read
// path clinicRootNameFor already documents for this app (self_service.go),
// ordinary business code asking "does this one identifier resolve to an
// account". DisplayName is the name the account registered with (the
// register form's optional display-name field), Email the address it
// registered (always present for an email-registered account). The source
// therefore holds for EVERY member, the demo-seeded accounts and the
// colleagues a self-registered clinic invited alike: a membership in this
// app is created only for an account that exists in authn's users table,
// so "member" implies "has an authn row to name them". Nothing here reads
// the demo layer's own static roster or names -- this answer never knows
// which members the boot seeded and which joined through an invitation,
// which is exactly what makes it honest for the population the surface
// exists to serve rather than only for the demo's seeds.
//
// What the roster answers can therefore name is bounded by what authn
// holds: a member who registered with a display name is named by it, a
// member who registered without one (the demo seeds register without, the
// same shape a colleague who skipped the optional field has) is named by
// their email, and a member whose account row carries neither -- no
// account in this app's flows, kept on the roster only by a vanished row
// that cannot name itself -- answers with empty identity fields and the
// surface's fallback label rather than with their raw user id. The email
// is PII, and showing it here is a real disclosure, deliberately made:
// the roster is the clinic's own "who works here" surface, read only by
// members holding tenant-wide org:read (the gate below), the same people
// an invitation's address already travels among -- and an unnamed roster
// is the exact defect this route exists to fix.
//
// The gate mirrors the org module route the web's roster used to read:
// GET /api/v1/org/members answers org:read (guardOrgRoute,
// demo_subject.go), so this answer requires the same permission through
// the same rbac gate and the same demo subject resolver, and adds the
// same tenant-wide narrowing that route applies to a node-less roster
// listing (enforceOrgNodeScope's requireTenantWide): a caller whose
// org:read grant covers one subtree of the clinic cannot read the whole
// clinic's roster -- with names or without. A caller without org:read at
// all (the demo's note-reader holds notes:read and nothing else) is
// refused with the same 403 rbac.permission_denied the org route answers,
// which is what the web's team surface gate is keyed on.
//
// The wire shape and path are hand-kept in step with the web's own typed
// access (src/team-api.ts) and the demo server that stands in for this
// whole composed stack in web suites (test-utils/demo-server.ts) -- the
// same parity relationship clinic_name.go and tenant-name.ts hold, kept
// honest by the flow test in team_members_test.go that drives this route
// through the real composed stack and by the web suites that drive the
// surface over it.
const teamMembersPath = "/api/reference-app/team-members"

// teamMemberRow is one roster row's wire shape: the membership facts
// org's own OrgMembership answer carries (the frontend's status and
// joined columns render from them, and userId stays the row's identity
// key -- the "You" naming of one's own row compares against it
// client-side), plus the two identity fields this host's composition
// adds. displayName and email are the account's own values, each the
// empty string when the account has none; the web renders displayName
// when non-empty, email when only that exists, and its fallback label
// when neither does.
type teamMemberRow struct {
	MembershipID string    `json:"membershipId"`
	UserID       string    `json:"userId"`
	NodeID       string    `json:"nodeId"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	DisplayName  string    `json:"displayName"`
	Email        string    `json:"email"`
}

// teamMembersResponse is the 200 answer's wire shape.
type teamMembersResponse struct {
	Members []teamMemberRow `json:"members"`
}

// teamMemberErrInternal folds an error that is not itself an *apperr.Error
// into a stable code, the same fallback writeClinicNameError's own
// reference_app.internal_error applies -- a caller never sees raw Go error
// text either way.
var teamMemberErrInternal = apperr.Internal("reference_app.internal_error")

// teamMemberError is the answer's failure shape, the same code-plus-params
// envelope every module's generated handlers write (clinic_name.go's own
// clinicNameError is the in-file precedent).
type teamMemberError struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeTeamMemberError writes err as the envelope answer: an *apperr.Error
// keeps its own code and status (org's coded storage errors pass through
// as themselves, the same codes org's own routes answer), anything else
// collapses to the internal fallback above.
func writeTeamMemberError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = teamMemberErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(teamMemberError{Code: appErr.Code, Params: appErr.Params})
}

// writeTeamMembersJSON writes the 200 answer. A nil slice answers the
// empty array, never a null -- the wire shape org's own list answers
// promise, which a roster reader treats as "no members", the same
// convention the frontend's `?? []` relies on.
func writeTeamMembersJSON(w http.ResponseWriter, members []teamMemberRow) {
	if members == nil {
		members = []teamMemberRow{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(teamMembersResponse{Members: members})
}

// teamMembersDeps bundles the org-module and authn state serveTeamMembers
// composes, mirroring orgRouteGuardDeps' own bundling shape (demo_subject.go):
// one struct so wireTeamMembers' signature grows by one field rather than
// by one parameter per service this composition reaches into.
type teamMembersDeps struct {
	// az is the rbac Authorizer whose gate guards the route (the coarse
	// org:read check and the tenant-wide narrowing both go through it).
	az rbac.Authorizer
	// members and tree are the org-module services whose roster rows this
	// answer enriches -- the same instances every other org-backed surface
	// in this app drives.
	members *org.MemberService
	tree    *org.TreeService
	// users is authn's identity-domain user repository, the row source of
	// the display identity -- authnModule.Service().Users(), the same
	// accessor clinicRootNameFor reads through (self_service.go).
	users *authn.UserRepository
	// headerDisabled carries cfg.DisableDemoUserHeader into the subject
	// resolver, exactly as guardModuleRoute threads it (demo_subject.go):
	// the demo header, when enabled, names who acts for the gate.
	headerDisabled bool
}

// teamMembersPermissionFor selects the permission a teamMembersPath
// request must hold: org's own read permission, the same permission the
// org module route the web's roster used to read gates its node-less
// member listing on (orgPermissionFor, demo_subject.go). Only reads
// exist on this answer, so any other method answers "" -- and an empty
// selector is refused by RequirePermissionFunc before the handler runs,
// the same 403 rbac.permission_denied the org route answers, a refusal
// no caller passes with a write method, org:read held or not. The
// handler's own method check (wireTeamMembers) is thus unreachable in
// this wiring; it stays as defense in depth -- it costs nothing and
// covers a caller that mounts the handler without the gate -- but the
// honest expectation is that the branch is a no-op in production
// traffic, and it would answer the 400-level
// reference_app.method_not_allowed envelope, not a 405, if it were ever
// reached.
func teamMembersPermissionFor(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return org.PermissionRead
	default:
		return ""
	}
}

// wireTeamMembers mounts teamMembersPath on mux, behind the rbac gate
// teamMembersPermissionFor selects -- rbac.RequirePermissionFunc with the
// same demo subject resolver every module route uses, so a caller without
// org:read is refused with the exact 403 rbac.permission_denied envelope
// the org module route answers -- and the composition handler below.
//
// The route sits behind authn.Middleware and tenancy.Middleware like every
// non-allowlisted route: an anonymous caller is refused before this gate
// runs (tenancy.tenant_unresolved), and the tenant comes from the access
// token's claims in the request context, never from a parameter, header
// or body.
func wireTeamMembers(mux *http.ServeMux, deps teamMembersDeps) {
	gated := rbac.RequirePermissionFunc(deps.az, teamMembersPermissionFor,
		rbac.WithSubjectResolver(demoSubjectResolverFor(deps.headerDisabled)),
	)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			writeTeamMemberError(w, apperr.Invalid("reference_app.method_not_allowed"))
			return
		}
		// HEAD rides the GET answer exactly as every GET-only module
		// route's ServeMux pattern serves it (net/http strips the body):
		// the read permission the selector grants is the read permission
		// this read needs.
		serveTeamMembers(w, r, deps)
	}))
	mux.Handle(teamMembersPath, gated)
}

// serveTeamMembers answers teamMembersPath.
//
// The roster read mirrors org.Handler.OrgListMembers' own tenant-wide
// shape exactly: the tenant's root node resolves first and the roster is
// the root's subtree (the whole tenant -- the demo clinics are
// single-root practices), a tenant with no root yet answers an empty
// roster rather than an error, and org's coded storage errors pass
// through as themselves. The one deliberate difference is what this route
// exists for: each roster row is enriched with its user's display
// identity from authn's users table before it is answered.
//
// The enrichment is best-effort per row, in the same spirit clinicRootNameFor
// documents for its own authn read: a member whose account row cannot be
// read (a vanished account -- authn ships no delete path, so in practice
// a row that was never written or one a concurrent erasure removed)
// answers with empty identity fields and a logged warning, never failing
// the whole roster or inventing an identifier -- the surface's fallback
// label names the row, and the row itself stays a member the caller can
// see is a member. One lookup per distinct member is the whole cost of
// this route: the roster is a clinic's people, a handful of rows, each
// resolved by one primary-key read on the identity table.
func serveTeamMembers(w http.ResponseWriter, r *http.Request, deps teamMembersDeps) {
	ctx := r.Context()

	// The tenant-wide narrowing, mirroring the org module route's own
	// requireTenantWide refusal for a node-less roster listing
	// (enforceOrgNodeScope, demo_subject.go): this answer is the WHOLE
	// clinic's roster, with names -- a grant that covers one subtree of
	// the clinic cannot read it, exactly as it cannot read the org route's
	// node-less member list. rbac.RequirePermissionFunc has already
	// installed the Subject it decided Can against onto the context
	// (enforceOrgNodeScope's own doc comment states the same contract).
	sub, ok := rbac.SubjectFromContext(ctx)
	if !ok {
		writeRBACGateError(w, rbac.ErrPermissionDenied)
		return
	}
	resource, action, ok := splitDemoPermission(org.PermissionRead)
	if !ok {
		writeRBACGateError(w, rbac.ErrPermissionDenied.WithParam("permission", org.PermissionRead))
		return
	}
	scope, err := deps.az.DataScope(ctx, sub, action, resource)
	if err != nil {
		writeRBACGateError(w, rbac.ErrStorage)
		return
	}
	if !scope.TenantWide {
		writeRBACGateError(w, rbac.ErrPermissionDenied.WithParam("permission", org.PermissionRead))
		return
	}

	root, err := deps.tree.Root(ctx)
	switch {
	case err == nil:
	case orgCodeIs(err, org.ErrNodeNotFound.Code):
		// A tenant with no org tree has no members to name: the empty
		// roster, exactly as org's own handler answers it.
		writeTeamMembersJSON(w, nil)
		return
	default:
		writeTeamMemberError(w, err)
		return
	}

	members, err := deps.members.List(ctx, root.ID)
	if err != nil {
		writeTeamMemberError(w, err)
		return
	}

	rows := make([]teamMemberRow, 0, len(members))
	for i := range members {
		member := &members[i]
		displayName, email := "", ""
		user, err := deps.users.FindByID(ctx, member.UserID)
		if err != nil {
			obs.FromContext(ctx).Warn(
				"reference-app could not read a roster member's account for its display identity",
				"user_id", member.UserID,
				"error", err)
		} else {
			// DisplayName is trimmed exactly as clinicRootNameFor trims it:
			// a name of only whitespace is no name, and the surface renders
			// "empty" as its fallback rather than a string of spaces. Email
			// was already canonicalized at registration.
			displayName = strings.TrimSpace(user.DisplayName)
			email = user.Email
		}
		rows = append(rows, teamMemberRow{
			MembershipID: member.ID,
			UserID:       member.UserID,
			NodeID:       member.NodeID,
			Status:       member.Status,
			CreatedAt:    member.CreatedAt,
			DisplayName:  displayName,
			Email:        email,
		})
	}
	writeTeamMembersJSON(w, rows)
}
