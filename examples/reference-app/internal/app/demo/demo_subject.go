package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	speedapp "github.com/vislake/speed/go/app"
)

// This file holds everything this example needs to demonstrate rbac end to
// end: where a Subject comes from, which permission gates which route, and
// the demo grants seeded at startup. It is deliberately a file of its own
// rather than more of internal/app/server.go.
//
// The "where a Subject comes from" half: a request whose access token
// verified is resolved from its Principal, the identity the authenticating
// side proved. DemoUserHeader survives as the affordance the pre-auth
// flows were built around -- its own comment says exactly how the two
// sources share the resolver -- and demo_users.go seeds real accounts
// whose grants reach this same gate through the principal path.

// DemoUserHeader names the request header this example reads an acting
// user id from.
//
// THIS IS NOT AUTHENTICATION, and it is not a pattern to copy. An
// unauthenticated header is a claim, not an identity: anyone who can reach
// the server can set it to any value and become that user. It carries the
// same warning DemoHostTenants and strictHostResolver carry in internal/app/server.go.
//
// The flows built around the header (the permission-gate and isolation
// tests, the actor model SeedDemoGrants below seeds) use it to say which
// seeded demo actor is acting. DemoSubjectResolver therefore reads it
// first, so those flows keep their meaning; a request carrying no demo
// header is resolved from the verified Principal authn.Middleware put in
// the request context -- which is how the real accounts demo_users.go
// seeds reach the same gate from a browser with no header at all.
// Removing the header together with the resolver's fallback stays undone
// until the pre-auth and demo flows move onto those real accounts.
//
// Its precedence over a verified Principal is a real hole for any
// deployment where a non-demo user might reach this binary: a caller who
// merely holds a low-privilege session can set this header to a
// higher-privileged demo actor's id and the resolver hands rbac that
// actor's Subject instead of the caller's own, no token forgery required.
// the DisableDemoUserHeader bootstrap field (internal/app/bootstrap.go) is the escape
// hatch -- an operator
// who deploys this reference app somewhere a real user might reach sets
// APP_DISABLE_DEMO_USER_HEADER and every demo identity source is disabled
// at once: DemoSubjectResolverFor makes every gated route resolve from the
// verified Principal alone, this header not consulted at all, and
// the SAME switch reaches the attribution header this app's other
// resolver family reads (DemoOrgUserHeader, "X-Demo-User-Id", internal/app/server.go)
// -- DemoOrgSubjectResolverFor and DemoNotesSubjectResolver are wired with
// headerDisabled then, so notes' create handler, the cases surface, org's
// caller-scoped endpoints and the notification surface resolve from the
// verified Principal too. Left unset (the default),
// neither header's behavior changes, which is what keeps
// every demo journey and test built around them working.
const DemoUserHeader = "X-Demo-User"

// The demo users seeded into every configured tenant. Two of them, because
// one user proves only that the gate opens: it takes a second, holding
// strictly less authority, to prove the gate also closes on a real user
// rather than only on an anonymous request.
const (
	// DemoOwnerUserID holds the built-in owner role, so it carries every
	// permission any module declared -- including both of notes'.
	DemoOwnerUserID = "demo-owner"

	// DemoReaderUserID holds demoReaderRoleKey, a custom role carrying
	// notes:read and nothing else. It may list notes and may not create
	// one, which is the difference the permission gate exists to enforce.
	DemoReaderUserID = "demo-reader"

	// demoReaderRoleKey is the tenant-scoped key of that read-only role.
	// It is defined per tenant, like every role: roles are tenant data,
	// and there is deliberately no cross-tenant template to copy from.
	demoReaderRoleKey = "note-reader"

	// DemoNotesCreatorUserID is the X-Demo-User-Id header value the flow
	// helpers that create notes send on every request. X-Demo-User-Id is a
	// different namespace from X-Demo-User (the header this file's earlier
	// const names): the latter names the seeded rbac grant the gate
	// decides against (demo-owner and friends above), while the former
	// names the user id notes' own SubjectResolver (DemoNotesSubjectResolver
	// in internal/app/server.go) attributes the CREATE to -- the value that lands in a
	// note's CreatorUserID and in the NoteCreatedPayload event every
	// subscriber reads. The value is a real-user-style id (org_flow_test's
	// "user-owner-1" is the same shape), not an rbac demo identity, exactly
	// as a real deployment's access-token subject claim would be -- which
	// is also why demo_notification.go's address table keys on it: every
	// note-created event this app's own flow helpers publish names it. An
	// event can instead name a seeded account's real user id, when the
	// account acts through its access token with no demo header at all --
	// DemoNotesSubjectResolver falls back to the verified Principal, the
	// same fallback the rbac gate's DemoSubjectResolver makes -- and that
	// id, absent from the address table, resolves to no addresses: an
	// ordinary miss, never an error.
	DemoNotesCreatorUserID = "user-creator-1"

	// DemoSingleTenantUserID holds demoReaderRoleKey in ONE tenant only,
	// which is what makes this example demonstrate the single most
	// important property of a grant: it is a fact about a (tenant, user)
	// PAIR, never about a user. The same id acting under the other demo
	// host holds nothing at all and is refused -- not because it is
	// unknown, but because its grant lives in a different tenant.
	DemoSingleTenantUserID = "demo-acme-only"

	// DemoAIGatewayTenantWriterUserID holds demoAIGatewayTenantWriterRoleKey,
	// a custom role carrying exactly aigateway.PermissionWrite (and
	// aigateway.PermissionRead) and NOT aigateway.PermissionManagePlatform.
	// It exists purely to prove the two-tier permission gate is
	// real: DemoOwnerUserID holds BuiltinRoleOwner, which -- per rbac's own
	// builtin.go doc comment -- carries EVERY permission any module
	// declared, platform-scope credential write included, so it cannot
	// demonstrate the platform write being refused. This user can set its
	// OWN tenant's BYOK credential but is refused the platform-wide write,
	// which is the distinction the two permissions exist to enforce.
	DemoAIGatewayTenantWriterUserID = "demo-aigateway-tenant-writer"

	// demoAIGatewayTenantWriterRoleKey is the tenant-scoped key of that
	// role. It is defined per tenant, like every role: roles are tenant
	// data, and there is deliberately no cross-tenant template to copy
	// from.
	demoAIGatewayTenantWriterRoleKey = "ai-gateway-tenant-writer"
)

// DemoSingleTenantID is the one tenant DemoSingleTenantUserID is granted
// in. It is a literal rather than "whichever tenant comes first" because
// map iteration order is unspecified, and a demo whose grants move between
// boots would be a poor thing to reason about.
const DemoSingleTenantID pkgcore.TenantID = "tenant-acme"

// The two action halves this example composes permission strings from. A
// permission is "<resource>:<action>" (rbac.Permission), and notes declares
// exactly notes:read and notes:write.
const (
	DemoActionRead  = "read"
	DemoActionWrite = "write"
)

// notesRoutePath is where the notes module mounts its route. This example
// needs the literal because the module keeps its own path unexported --
// and it does not need to keep the two in sync by hand: mountModuleRoutes
// refuses to start when a module mounts a path DemoRouteRules does not
// name, so a path change here surfaces as a startup failure naming the new
// path, never as a silently ungated route.
const notesRoutePath = "/api/v1/notes"

// orgRoutePath is where the org module mounts its routes -- the same
// unexported-path situation notesRoutePath's own comment explains.
const orgRoutePath = "/api/v1/org"

// storageRoutePath is where the storage module mounts its routes -- the
// same unexported-path situation notesRoutePath's own comment explains.
const storageRoutePath = "/api/v1/storage"

// notificationRoutePath is where the notification module mounts its
// routes -- the same unexported-path situation notesRoutePath's own
// comment explains.
const notificationRoutePath = "/api/v1/notifications"

// PkiRoutePath is where the pki module mounts its routes -- the same
// unexported-path situation notesRoutePath's own comment explains.
const PkiRoutePath = "/api/v1/pki"

// integrationRoutePath is where go/integration's spec-generated HTTP
// fragments mount their routes -- the same unexported-path situation
// notesRoutePath's own comment explains. One mount path serves BOTH of the
// module's gated entities: the API-key CRUD fragment and the
// webhook-subscription CRUD + recent-deliveries fragment live under
// this single path (go/integration's Handler implements both halves of the
// generated ServerInterface and module.go's Register mounts it once, at
// /api/v1/integration), so integrationPermissionFor dispatches by SUB-PATH
// to tell the two permission pairs apart -- see that selector's own doc
// comment for the full argument.
const integrationRoutePath = "/api/v1/integration"

// sharingSharesRoutePath is where sharing's owner-facing operations
// (create, list, get, revoke, list access log) are mounted -- named through
// the module's own exported sharing.PathShares constant, mirroring every
// other *RoutePath constant's use of an exported module constant where one
// exists. Deliberately a DIFFERENT table entry from sharing.PathAccess
// (declared public below): sharing mounts the same *Handler at both paths,
// but PathAccess is genuinely unauthenticated and PathShares is an ordinary
// tenant-scoped, permission-gated surface -- see module.go's own Register
// doc comment in go/sharing for the full contrast.
const sharingSharesRoutePath = sharing.PathShares

// AiGatewayRoutePath is where go/ai-gateway's credential-write
// admin surface mounts its routes -- the same unexported-path situation
// notesRoutePath's own comment explains.
const AiGatewayRoutePath = "/api/v1/ai-gateway"

// BillingRoutePath is where the billing module mounts its routes -- the
// same unexported-path situation notesRoutePath's own comment explains.
const BillingRoutePath = "/api/v1/billing"

// DemoRouteRules returns this app's route-authorization table: one
// rbac.RouteRule for every path a module mounts, declaring whether the
// route is public or which permission gates it, which subject resolver the
// check evaluates, and (for org alone) the layer that narrows inside the
// gate. mountModuleRoutes hands it to rbac.GuardRoutes and mounts what
// comes back.
//
// The table is exact in both directions: a mounted path it does not name
// fails the server build, and so does an entry no module mounted. That
// direction matters. A table whose default is "ungated" quietly serves
// every route a new module adds; a table whose default is "refuse to
// start" cannot. A public route is a positive declaration --
// pkgcore.RouteAccess{Public: true} -- never an omission.
//
// Every gated entry names DemoSubjectResolverFor's resolver, so the demo
// identity seam -- header first, verified Principal otherwise -- is what
// each gate decides against, and APP_DISABLE_DEMO_USER_HEADER
// (DemoUserHeader's own doc comment) closes the header half for all of
// them at once. The two entries that deliberately deviate are pki's and
// admin's, each for its own domain reason stated on its own entry.
//
// az and orgDeps are the same instances mountModuleRoutes passes to
// rbac.GuardRoutes and BuildServer wired into the org module; the org
// entry's Layer closure needs them (see orgNodeScopeLayer).
func DemoRouteRules(az rbac.Authorizer, orgDeps OrgRouteGuardDeps, demoHeaderDisabled bool) []rbac.RouteRule {
	demo := DemoSubjectResolverFor(demoHeaderDisabled)
	return []rbac.RouteRule{
		// notes and storage are gated for real: neither module's handler
		// performs a permission check of its own -- the modules declare
		// their permissions and leave their enforcement to the host's
		// authorization layer -- so the read/write split over each
		// module's own resource is where notes:read/notes:write and
		// storage's pair are enforced. The demo grants seed storage's
		// permissions into no role but the built-in owner -- the demo
		// reader holds notes:read and nothing else -- which
		// flowtests/storage_flow_test.go relies on to prove the gate
		// closes on a user who holds another module's permissions: a
		// per-module permission is not a blanket role.
		{Path: notesRoutePath, Access: pkgcore.RouteAccess{Permission: DemoPermissionFor(NotesResource)}, SubjectResolver: demo},
		{Path: storageRoutePath, Access: pkgcore.RouteAccess{Permission: DemoPermissionFor(storageResource)}, SubjectResolver: demo},

		// org's Handler performs no PERMISSION check of its own -- it
		// resolves a caller's raw identity through SubjectResolver
		// (DemoOrgSubjectResolverFor in internal/app/server.go) for the two invitation
		// operations, org_createInvitation and org_acceptInvitation, and
		// reads only the tenant from context for every other operation --
		// so this gate is where the example enforces org's four declared
		// permissions, per operation, through orgPermissionFor: they are
		// distinguished by sub-resource (tree vs. roster vs. invitation),
		// not by method alone, so no generic read/write split expresses
		// them.
		//
		// The exemption is the accept-invitation operation: a person
		// accepting their FIRST invitation has by definition no rbac
		// grant yet in the tenant they are about to join, so gating that
		// operation on a coarse permission would refuse exactly the flow
		// org exists to demonstrate -- org.Handler's own caller resolution
		// (org.ErrSubjectUnresolved on an unidentifiable caller) is that
		// operation's whole gate, and an exempted request never reaches
		// the Layer below.
		//
		// The Layer is the subtree narrowing that runs INSIDE this gate
		// for every admitted request (enforceOrgNodeScope, see its doc
		// comment for the mechanism and its known gaps): a grant scoped to
		// one subtree cannot reach a target outside it.
		{
			Path:            orgRoutePath,
			Access:          pkgcore.RouteAccess{Permission: orgPermissionFor},
			SubjectResolver: demo,
			Exempt:          isOrgAcceptInvitationRequest,
			Layer:           orgNodeScopeLayer(az, orgDeps),
		},

		// notification's handler resolves and requires its own caller
		// identity per operation through SubjectResolver
		// (DemoOrgSubjectResolverFor in internal/app/server.go -- the same seam
		// instance org's own caller-scoped endpoints use; notes' create
		// handler resolves through its own DemoNotesSubjectResolver,
		// which additionally accepts the verified Principal, its comment
		// saying why notes alone gets that second source). Every one of
		// the module's operations, the realtime stream included, refuses
		// an unidentifiable caller with ErrSubjectUnresolved, and the
		// module declares no permissions at all -- its endpoints are a
		// user's own inbox, contacts and preferences, never a
		// cross-tenant surface -- so no rbac permission could be required
		// here, and its own per-operation subject check is where its gate
		// lives.
		{Path: notificationRoutePath, Access: pkgcore.RouteAccess{Public: true}},

		// authn's subtree never reaches this app's gated mux at all:
		// BuildServer mounts every AuthnAPIPath request on its own topMux
		// branch directly behind authn.Middleware's optional verification
		// (internal/app/server.go's composition comment), which is the only shape in
		// which the enterprise-OIDC login-start path -- its provider
		// value the dynamic "oidc:<tenant>" string no exact-match
		// allowlist entry can enumerate -- can work. The entry stays
		// public so the table keeps naming the path; authn.Handler itself
		// is the per-operation authority on who may call what
		// (requirePrincipal): the operations that must work before anyone
		// has a Principal at all -- registration, every sign-in entry
		// point, token refresh, the social authorize/callback pair -- are
		// a deliberately ungated surface, while an anonymous request to
		// any other operation is refused with authn's own coded
		// authn.authentication_required.
		{Path: speedapp.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}},

		// The config module's two pre-auth endpoints are public for the
		// same reason they are allowlisted in tenancy.Middleware (see
		// BuildServer): they are pre-auth display surfaces -- a login
		// page's brand and feature flags -- that must render before
		// anyone has signed in, and they serve only what the design marks
		// public, never tenant data. Both are named through the module's
		// own exported constants so a rename cannot drift into a silently
		// ungated path here.
		{Path: config.PathPublic, Access: pkgcore.RouteAccess{Public: true}},
		{Path: config.PathSystemFeatures, Access: pkgcore.RouteAccess{Public: true}},

		// sharing.PathAccess is a genuinely unauthenticated surface by
		// design (go/sharing's Handler doc comment), gated on nothing an
		// rbac permission check could evaluate -- an anonymous visitor
		// holding a bearer share token has no Subject at all.
		// sharing.Service.AccessPublic is where this route's real gate
		// lives (the token itself, and the tenant-and-share state it
		// resolves to), exactly as authn's and org's own per-operation
		// checks are where their own public declarations' real gates
		// live.
		{Path: sharing.PathAccess, Access: pkgcore.RouteAccess{Public: true}},

		// sharing.PathShares is gated for real: sharing's Handler
		// performs no authorization of its own for these five
		// owner-facing operations, leaving their enforcement to the
		// host's authorization layer, exactly as go/sharing's own
		// module.go Register doc comment states. sharingPermissionFor is
		// needed rather than the generic read/write split because a POST
		// here means either sharing:create or sharing:revoke -- see that
		// selector's own doc comment.
		{Path: sharingSharesRoutePath, Access: pkgcore.RouteAccess{Permission: sharingPermissionFor}, SubjectResolver: demo},

		// pki's handler performs no identity check of its own -- the
		// storage-style shape -- and its fine-grained vocabulary
		// (pki.PermissionRead plus the two revoke permissions) is not the
		// generic read/write pair, so pkiPermissionFor selects between
		// them by route. Its subject resolver is deliberately NOT the
		// demo one: pkiSubjectResolverFor pins the signing-key revoke's
		// evaluation to rbac.SystemDomain -- the platform-domain half of
		// pki's permission contract (go/pki/module.go), without which a
		// tenant's owner role (SeedDemoGrants grants every declared
		// permission in every demo tenant) would reach the platform
		// signing key and stop token issuance for every tenant at once.
		//
		// What this gate does NOT reach: PermissionIssue and
		// PermissionRotate have no HTTP operation in the fragment at all
		// (issuance and manual rotation stay Go-only per
		// go/pki/api/openapi.yaml's own header), so there is nothing yet
		// to gate for either.
		{Path: PkiRoutePath, Access: pkgcore.RouteAccess{Permission: pkiPermissionFor}, SubjectResolver: pkiSubjectResolverFor(demoHeaderDisabled)},

		// integration's handler performs no identity check of its own
		// beyond resolving a creator for the two create operations (see
		// integration.SubjectResolver), which is a different question
		// from whether the CALLER may reach the route at all. Its
		// permission strings carry three segments --
		// "integration:apikey:read"/"integration:apikey:manage" and
		// "integration:webhook:read"/"integration:webhook:manage", both
		// pairs sharing this one mount path -- which
		// integrationPermissionFor tells apart by sub-path; the gate's
		// splitter reads such a string at its last separator, so both
		// pairs ride the standard permission check.
		{Path: integrationRoutePath, Access: pkgcore.RouteAccess{Permission: integrationPermissionFor}, SubjectResolver: demo},

		// admin's handler gates by SUB-PATH (tenants, users,
		// impersonation, audit-events, roles, usage, notification
		// send-records) through adminPermissionFor (demo_admin.go), and
		// its subject resolver is deliberately NOT the demo one:
		// adminSubjectResolver evaluates every admin:* permission under
		// rbac.SystemDomain, from the verified Principal alone, because
		// admin's route does not sit behind ordinary tenant resolution --
		// see that resolver's own doc comment for the cross-tenant
		// escalation it closes.
		{Path: AdminRoutePath, Access: pkgcore.RouteAccess{Permission: adminPermissionFor}, SubjectResolver: adminSubjectResolver},

		// ai-gateway's handler performs no permission check of its own
		// (see its own doc comment), leaving enforcement to the host's
		// authorization layer. aiGatewayPermissionFor is needed rather
		// than the generic read/write split, since a non-GET request here
		// means either aigateway:write (the tenant's own BYOK write) or
		// aigateway:manage_platform (the platform-wide write) -- see that
		// selector's own doc comment.
		{Path: AiGatewayRoutePath, Access: pkgcore.RouteAccess{Permission: aiGatewayPermissionFor}, SubjectResolver: demo},

		// billing's handler performs no authorization of its own
		// (go/billing's Handler doc comment), leaving enforcement to the
		// host's authorization layer. Its permission strings carry three
		// segments ("billing:credit:read", "billing:credit:manage"),
		// which billingPermissionFor names directly and the gate's
		// splitter reads at the last separator, so billing rides the
		// standard permission check too.
		{Path: BillingRoutePath, Access: pkgcore.RouteAccess{Permission: billingPermissionFor}, SubjectResolver: demo},
	}
}

// NotesResource is the resource half of notes' permission strings. It is
// derived from the module's own exported constants rather than retyped, so
// this example cannot drift from the permissions notes actually declares.
var NotesResource = MustResourceOf(notes.PermissionRead, notes.PermissionWrite)

// storageResource is the resource half of storage's permission strings,
// derived from its own exported constants the same way NotesResource is
// derived from notes' -- so this example cannot drift from the
// permissions storage actually declares either.
var storageResource = MustResourceOf(storage.PermissionRead, storage.PermissionWrite)

// pkiSigningKeyRevokePrefix is the request-path prefix of the ONE pki
// operation whose permission must be evaluated in the platform domain --
// POST /api/v1/pki/signing-keys/{kid}/revoke. Its subject (below) pins the
// tenant to rbac.SystemDomain for exactly this prefix and nothing else.
const pkiSigningKeyRevokePrefix = PkiRoutePath + "/signing-keys/"

// pkiPermissionFor selects the permission a pki request must hold, from the
// request's route alone -- never a header, query parameter or body field,
// for the same reason DemoPermissionFor's own doc comment gives. Of pki's
// declared permissions, only PermissionRead and the two revoke permissions
// have an HTTP operation in the fragment at all (go/pki/api/openapi.yaml):
// the three GET operations export a JWKS or a CRL (PermissionRead), and
// the two POST operations revoke a signing key or a certificate -- gated on
// pki.PermissionRevokeSigningKey and pki.PermissionRevokeCertificate
// respectively (go/pki/module.go's const block documents why one
// permission spanning pki_signing_keys and pki_certificates is wrong in
// whichever single domain it is evaluated in). PermissionIssue and
// PermissionRotate stay ungated here because nothing under this path
// invokes them over HTTP yet. A request that matches neither revoke
// sub-path and is not a read is denied -- "" makes
// rbac.RequirePermissionFunc fail closed.
func pkiPermissionFor(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return pki.PermissionRead
	}
	if strings.HasPrefix(r.URL.Path, pkiSigningKeyRevokePrefix) {
		return pki.PermissionRevokeSigningKey
	}
	if strings.HasPrefix(r.URL.Path, PkiRoutePath+"/certificates/") {
		return pki.PermissionRevokeCertificate
	}
	return ""
}

// pkiSubjectResolverFor returns the subject resolver the pki entry of DemoRouteRules plugs
// into rbac.WithSubjectResolver: the demo subject (DemoSubjectResolverFor)
// with one deliberate mutation -- a request against the signing-key revoke
// operation has its subject's tenant pinned to rbac.SystemDomain.
//
// The pin is the platform-domain half of pki's permission contract
// (go/pki/module.go): pki_revokeSigningKey acts on pki_signing_keys, which
// is platform data with no tenant column at all, and evaluating its
// permission in the request tenant's domain would let a single tenant's
// grant of pki:revoke_signing_key stop token issuance for every tenant at
// once -- the demo's own owner role grants every declared permission in
// every demo tenant, which is exactly the lever that must NOT reach the
// platform signing key. Pinning the tenant makes the check ask "does this
// user hold pki:revoke_signing_key under rbac.SystemDomain", the same
// domain-shift adminSubjectResolver (demo_admin.go) applies to every
// admin:* permission. The user half is untouched: the pin only relocates
// where the user's grant must live, it never invents a user. A demo
// header user therefore cannot revoke a signing key at all -- SeedDemoGrants
// grants demo users only in their tenant domains -- and only a real
// platform-staff account holding the owner role under SystemDomain
// (SeedDemoPlatformStaff, demo_admin.go) can.
func pkiSubjectResolverFor(headerDisabled bool) func(*http.Request) (rbac.Subject, bool) {
	resolve := DemoSubjectResolverFor(headerDisabled)
	return func(r *http.Request) (rbac.Subject, bool) {
		sub, ok := resolve(r)
		if !ok {
			return rbac.Subject{}, false
		}
		if strings.HasPrefix(r.URL.Path, pkiSigningKeyRevokePrefix) {
			sub.TenantID = rbac.SystemDomain
		}
		return sub, true
	}
}

// integrationPermissionFor selects the permission a request against
// go/integration's mounted fragment must hold. Unlike pkiPermissionFor's
// and sharingPermissionFor's single-pair selectors, this one must FIRST
// pick which of the module's two permission pairs the request targets,
// because both fragments share the one mount path integrationRoutePath: a
// request whose path continues "/webhooks" is the webhook-subscription
// surface -- its GET/HEAD reads gate on integration.PermissionWebhookRead
// and its four mutations (create, update, delete, restore) on
// integration.PermissionWebhookManage, a method-only split like
// DemoPermissionFor's since the webhook fragment's two read operations are
// both GETs -- and anything else under the mount is the API-key surface,
// gated on integration.PermissionRead/PermissionManage the same way.
//
// Both pairs are declared as three-segment names
// ("integration:apikey:read", per go/integration/module.go's own doc
// comments on its constants), which the gate's splitter reads at the LAST
// separator: resource "integration:apikey", action "read". That is what
// lets this selector ride the standard permission check; see
// rbac.SplitPermission's own doc comment.
//
// The sub-path check is safe to gate on for the same reason the table
// lookup is: the path is what routed the request to this gate in the first
// place, never a value a caller supplies independently of it. A Go 1.22
// ServeMux hands a mounted handler the FULL request path
// (mountModuleRoutes registers exactly the module's own mount, so
// r.URL.Path always carries the sub-path remainder), which is the same
// precedent sharingPermissionFor's "/revoke" suffix check and
// aiGatewayPermissionFor's "/platform" suffix check already establish.
func integrationPermissionFor(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, integrationRoutePath+"/webhooks") {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return integration.PermissionWebhookRead
		default:
			return integration.PermissionWebhookManage
		}
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return integration.PermissionRead
	default:
		return integration.PermissionManage
	}
}

// sharingPermissionFor selects the permission a sharingSharesRoutePath
// request must hold, mirroring pkiPermissionFor's own reasoning: sharing's
// three-permission vocabulary (read/create/revoke) is not the generic
// read/write pair DemoPermissionFor assumes, and unlike pki's own two
// HTTP-reachable permissions, sharing's create and revoke operations are
// BOTH POST requests that a method-only split cannot tell apart --
// sharing_createShare (POST /api/v1/sharing/shares) and sharing_revokeShare
// (POST /api/v1/sharing/shares/{shareId}/revoke). This selector also checks
// the request's own path suffix, which is safe to gate on for the identical
// reason the route table's own path lookup is: the path is what routed the
// request to this selector in the first place, never a value a caller
// supplies independently of it.
func sharingPermissionFor(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return sharing.PermissionRead
	}
	if strings.HasSuffix(r.URL.Path, "/revoke") {
		return sharing.PermissionRevoke
	}
	return sharing.PermissionCreate
}

// aiGatewayPermissionFor selects the permission an AiGatewayRoutePath
// request must hold, mirroring pkiPermissionFor's and sharingPermissionFor's
// own reasoning: ai-gateway's three-permission vocabulary (read/write/
// manage_platform) is not the generic read/write pair DemoPermissionFor
// assumes. A GET is aigateway:read (aiGateway_getCredential); a PUT whose
// path ends in "/platform" is aigateway:manage_platform
// (aiGateway_setPlatformCredential), the materially more privileged
// operation, deliberately gated on its own DISTINCT permission;
// every other write (a PUT ending in "/tenant") is aigateway:write
// (aiGateway_setTenantCredential). Checking the path suffix is safe for
// the identical reason sharingPermissionFor's own doc comment gives: the
// path is what routed the request to this selector in the first place,
// never a value a caller supplies independently of it.
func aiGatewayPermissionFor(r *http.Request) string {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return aigateway.PermissionRead
	}
	if strings.HasSuffix(r.URL.Path, "/platform") {
		return aigateway.PermissionManagePlatform
	}
	return aigateway.PermissionWrite
}

// billingPermissionFor selects the permission a BillingRoutePath request
// must hold, mirroring pkiPermissionFor's method-only split but naming
// billing's own three-segment permission strings directly. The module's
// fragment is read-only -- both operations are GETs answered
// under billing.PermissionCreditRead (billing.PermissionCreditManage
// gates nothing over HTTP yet; see go/billing/api/openapi.yaml's own
// header for the read-only decision) -- so the GET/HEAD read branch is the
// one any real request takes;
// the default branch stays the strict direction DemoPermissionFor itself
// adopts, demanding the manage permission from any method this example
// never thought about rather than guessing.
//
// The three-segment names ride the standard permission check: the gate's
// splitter cuts at the LAST separator, so "billing:credit:read" reaches
// rbac.Authorizer.Can as resource "billing:credit", action "read" -- the
// halves rbac.Permission("billing:credit", "read") composes back into the
// catalog entry the caller's role actually carries. See
// rbac.SplitPermission's own doc comment.
func billingPermissionFor(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return billing.PermissionCreditRead
	default:
		return billing.PermissionCreditManage
	}
}

// orgNodesSubPath, orgMembersSubPath and orgInvitationsSubPath are org's
// three sub-resources, relative to orgRoutePath -- the shape
// orgPermissionFor and orgNodeScopeFor both switch on. Named once here
// rather than repeating the literal in each function.
const (
	orgNodesSubPath       = "/nodes"
	orgMembersSubPath     = "/members"
	orgInvitationsSubPath = "/invitations"
	orgAcceptSuffix       = "/accept"
)

// OrgAcceptPath is the full path of org_acceptInvitation (POST
// /api/v1/org/invitations/accept) -- the one org operation this app's
// tenancy.Middleware allowlist names (internal/app/server.go's BuildServer) and the one
// org operation the org entry's Exemption lets through ungated.
// Composed here, in the same file that owns the path's three components,
// so internal/app/server.go's allowlist entry and the org entry's Exemption can never
// drift
// from orgPermissionFor's own sub-path switches.
const OrgAcceptPath = orgRoutePath + orgInvitationsSubPath + orgAcceptSuffix

// orgPermissionFor selects the org:* permission a request against
// orgRoutePath must hold, from its path and method alone -- never a header,
// a query parameter or a body field, the same rule DemoPermissionFor's own
// doc comment states.
//
// org's four permissions are distinguished by SUB-RESOURCE (tree vs.
// roster vs. invitation), not by one resource's read/write split, so this
// selector needs the request's sub-path the same way adminPermissionFor
// (demo_admin.go) and integrationPermissionFor above do for their own
// multi-permission modules:
//
//   - "/nodes" and "/nodes/{id}"[/move]: PermissionRead on GET/HEAD,
//     PermissionManage on every write (create, rename, move, delete).
//   - "/members" (list) and "/members/{userId}" (remove): PermissionRead on
//     GET/HEAD, PermissionRemoveMember on the one write this sub-resource
//     has.
//   - "/invitations" (list) and "/invitations" (create, a POST):
//     PermissionRead on GET/HEAD, PermissionInviteMember on the POST that
//     creates one.
//   - "/invitations/accept": never reaches this function at all -- the
//     route's Exempt predicate (isOrgAcceptInvitationRequest, declared on
//     the org entry of DemoRouteRules) short-circuits the whole permission
//     gate for it before orgPermissionFor is ever called. Returning a
//     permission here would be misleading dead code, so this function does
//     not attempt to handle it.
//
// A path this function does not recognize falls through to the "/nodes"
// case's read/write split -- unreachable for any path org's own Handler
// mounts (org.Handler's ServeHTTP would 404 first before this gate's
// permission choice even matters), but handled rather than assumed away,
// matching this file's posture everywhere else. The fall-through still
// requires a real permission (PermissionRead or PermissionManage), never an
// empty string, so it stays on the fail-closed side even if it were ever
// reached.
func orgPermissionFor(r *http.Request) string {
	path := strings.TrimPrefix(r.URL.Path, orgRoutePath)
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	case strings.HasPrefix(path, orgMembersSubPath):
		if isRead {
			return org.PermissionRead
		}
		return org.PermissionRemoveMember
	case strings.HasPrefix(path, orgInvitationsSubPath):
		if isRead {
			return org.PermissionRead
		}
		return org.PermissionInviteMember
	default: // orgNodesSubPath, or the bare mount root ("/nodes" or "").
		if isRead {
			return org.PermissionRead
		}
		return org.PermissionManage
	}
}

// isOrgAcceptInvitationRequest reports whether r targets
// org_acceptInvitation (POST /api/v1/org/invitations/accept) -- the one org
// operation the route table's Exempt predicate lets through with NO
// permission check at all. The reason is on the org entry of
// DemoRouteRules: a person accepting their FIRST invitation has no rbac
// grant yet, and org.Handler's own caller resolution is that operation's
// whole gate.
func isOrgAcceptInvitationRequest(r *http.Request) bool {
	return r.Method == http.MethodPost &&
		r.URL.Path == orgRoutePath+orgInvitationsSubPath+orgAcceptSuffix
}

// OrgRouteGuardDeps bundles the org-module runtime state the org entry's
// Layer (orgNodeScopeLayer wrapping enforceOrgNodeScope) needs to resolve a
// request's
// TARGET node to its materialized path and to translate OrgRemoveMember's
// target user into the node their membership binds them to -- org's own
// Scope and MemberService, the exact instances BuildServer's orgModule
// wires. Bundled into one struct so the entry's Layer closure is built
// from one value, rather than from two parameters.
type OrgRouteGuardDeps struct {
	Scope   org.Scope
	Members *org.MemberService
}

// orgNodeScopeLayer is the Layer the org entry of DemoRouteRules names:
// the subtree narrowing that runs INSIDE the route table's permission gate
// (rbac.RouteRule.Layer), so every request the gate admits passes
// enforceOrgNodeScope's DataScope check before org's handler runs -- and an
// exempted accept-invitation request, which bypasses the gate, bypasses
// this layer too. See enforceOrgNodeScope's own doc comment for the
// mechanism and its known gaps.
func orgNodeScopeLayer(az rbac.Authorizer, deps OrgRouteGuardDeps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if appErr := enforceOrgNodeScope(r.Context(), az, deps, r); appErr != nil {
				rbac.WriteAuthzError(w, appErr)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// orgNodeScopeTarget describes which node (if any) an org operation names
// as its target, for enforceOrgNodeScope's DataScope check.
//
// destinationNodeID is set only for OrgMoveNode, whose write touches TWO
// nodes at once: the node being moved (nodeID, the path parameter) and the
// parent it moves INTO (destinationNodeID, the request body's parentId).
// Both must lie in the caller's scope, or a subtree-scoped grant could move
// a node it does not own into a subtree it does not own either, or move a
// node it DOES own out into unmanaged territory.
//
// requireTenantWide is set when the operation has no single target node to
// check -- a listing with no parentId/nodeId query parameter, a root node
// creation with no parentId in the body, or listing invitations (which
// carries no node filter at all) -- because these act on the WHOLE tenant
// tree. A subtree-scoped grant, which by definition covers less than the
// whole tenant, is refused rather than served a silently narrower answer
// this router-level gate cannot compute: filtering LISTING ROWS by scope is
// the handler's job, and org.Handler does not do it (a known, recorded
// gap -- see enforceOrgNodeScope's own doc comment).
type orgNodeScopeTarget struct {
	applicable        bool
	requireTenantWide bool
	nodeID            string
	destinationNodeID string
	// removeMemberUserID is set only for OrgRemoveMember, whose path names
	// a USER, not a node -- enforceOrgNodeScope resolves it to the node
	// their membership binds them to through deps.Members before it has a
	// nodeID to check at all.
	removeMemberUserID string
}

// orgNodeScopeFor computes r's orgNodeScopeTarget from its path, query and
// (for the write operations that carry one) its JSON body. It never
// consumes r.Body without restoring it -- see peekJSONBody.
func orgNodeScopeFor(r *http.Request) orgNodeScopeTarget {
	path := strings.TrimPrefix(r.URL.Path, orgRoutePath)
	isRead := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	case path == orgInvitationsSubPath:
		if !isRead {
			// POST /invitations (org_createInvitation): target is the node
			// the invitee will be bound to.
			var body struct {
				NodeID string `json:"nodeId"`
			}
			if peekJSONBody(r, &body) && body.NodeID != "" {
				return orgNodeScopeTarget{applicable: true, nodeID: body.NodeID}
			}
		}
		// GET /invitations (org_listInvitations): no node filter exists on
		// this operation at all (api/openapi.yaml's OrgListInvitationsParams
		// is empty), so a subtree-scoped grant cannot be served a correctly
		// narrowed answer -- see the type's own doc comment.
		return orgNodeScopeTarget{applicable: true, requireTenantWide: true}

	case path == orgMembersSubPath:
		if nodeID := r.URL.Query().Get("nodeId"); nodeID != "" {
			return orgNodeScopeTarget{applicable: true, nodeID: nodeID}
		}
		return orgNodeScopeTarget{applicable: true, requireTenantWide: true}

	case strings.HasPrefix(path, orgMembersSubPath+"/"):
		// DELETE /members/{userId} (org_removeMember): the path names a
		// USER; enforceOrgNodeScope resolves the node through deps.Members.
		userID := strings.TrimPrefix(path, orgMembersSubPath+"/")
		return orgNodeScopeTarget{applicable: true, removeMemberUserID: userID}

	case path == orgNodesSubPath || path == "":
		if isRead {
			// GET /nodes (org_listNodes): parentId, when given, names the
			// node whose children are listed; absent, the whole tree.
			if parentID := r.URL.Query().Get("parentId"); parentID != "" {
				return orgNodeScopeTarget{applicable: true, nodeID: parentID}
			}
			return orgNodeScopeTarget{applicable: true, requireTenantWide: true}
		}
		// POST /nodes (org_createNode): parentId, when given, names the
		// node the new child is created under; absent creates the tenant's
		// ROOT, a tenant-wide structural change no subtree scope can cover.
		var body struct {
			ParentID *string `json:"parentId"`
		}
		if peekJSONBody(r, &body) && body.ParentID != nil && *body.ParentID != "" {
			return orgNodeScopeTarget{applicable: true, nodeID: *body.ParentID}
		}
		return orgNodeScopeTarget{applicable: true, requireTenantWide: true}

	default:
		// "/nodes/{id}" and "/nodes/{id}/move".
		rest := strings.TrimPrefix(path, orgNodesSubPath+"/")
		if idx := strings.Index(rest, "/"); idx >= 0 {
			// POST /nodes/{id}/move (org_moveNode): the node moving, AND the
			// parent it moves into.
			nodeID := rest[:idx]
			var body struct {
				ParentID string `json:"parentId"`
			}
			dest := ""
			if peekJSONBody(r, &body) {
				dest = body.ParentID
			}
			return orgNodeScopeTarget{applicable: true, nodeID: nodeID, destinationNodeID: dest}
		}
		// GET/PATCH/DELETE /nodes/{id} (org_getNode/org_renameNode/org_deleteNode).
		return orgNodeScopeTarget{applicable: true, nodeID: rest}
	}
}

// peekJSONBody decodes r's JSON body into dst WITHOUT consuming it: the
// handler downstream (org.Handler's own decodeJSON) must still be able to
// read the full body itself afterward. It reports whether decoding
// succeeded; a malformed or unreadable body reports false and still
// restores r.Body to its original bytes, so a bad request reaches the
// handler's own decoder either way, which answers the caller with the real
// "invalid body" error instead of a scope refusal manufactured from a
// failed peek -- a malformed body can create or move nothing, so letting it
// through this check is not a bypass of anything.
func peekJSONBody(r *http.Request, dst any) bool {
	if r.Body == nil {
		return false
	}
	raw, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// enforceOrgNodeScope is the node-scope layer the org entry of
// DemoRouteRules names (orgNodeScopeLayer): it runs AFTER rbac's coarse
// Can gate (rbac.RequirePermissionFunc) has already let the request
// through, and narrows it further for a
// subject whose org grant is scoped to one subtree rather than the whole
// tenant -- rbac's Authorizer.DataScope machinery's real, end-to-end
// consumer in this codebase.
//
// A tenant-wide grant is untouched by this function: DataScope.TenantWide
// short-circuits every branch below to "allowed" before orgNodeScopeFor is
// even consulted, which is the overwhelmingly common case -- every demo
// grant SeedDemoGrants makes is tenant-wide (rbac.Scope{}), per its own
// doc comment. Denying happens ONLY when the subject's DataScope for this
// exact action:resource is neither tenant-wide NOR includes the request's
// target node's materialized path.
//
// # Known gap: listing operations are gated, not filtered
//
// This function can only ALLOW or REFUSE a request as a whole; it cannot
// make org.Handler's own listing operations (org_listNodes,
// org_listMembers, org_listInvitations) return fewer ROWS than the handler
// itself would compute. So a subtree-scoped grant either names a specific
// node to list (allowed, checked against scope) or is refused outright when
// it does not (orgNodeScopeTarget.requireTenantWide) -- never served a
// silently truncated answer. A real per-row filter would need org.Handler
// itself to consult DataScope, which is a change to go/org, not this
// reference app's router gate; recorded here rather than half-built.
func enforceOrgNodeScope(ctx context.Context, az rbac.Authorizer, deps OrgRouteGuardDeps, r *http.Request) *apperr.Error {
	sub, ok := rbac.SubjectFromContext(ctx)
	if !ok {
		// Unreachable in practice: rbac.RequirePermissionFunc, which wraps
		// this handler, has already installed the exact Subject it decided
		// Can against onto the context before calling next (its own doc
		// comment). Handled anyway, never assumed away, matching this
		// file's posture everywhere else.
		return rbac.ErrPermissionDenied
	}
	permission := orgPermissionFor(r)
	resource, action, ok := rbac.SplitPermission(permission)
	if !ok {
		return rbac.ErrPermissionDenied.WithParam("permission", permission)
	}

	scope, err := az.DataScope(ctx, sub, action, resource)
	if err != nil {
		return rbac.ErrStorage
	}
	if scope.TenantWide {
		return nil
	}

	target := orgNodeScopeFor(r)
	if !target.applicable {
		return nil
	}
	if target.requireTenantWide {
		return rbac.ErrPermissionDenied.WithParam("permission", permission)
	}

	nodeID := target.nodeID
	if target.removeMemberUserID != "" {
		membership, memErr := deps.Members.Get(ctx, target.removeMemberUserID)
		if memErr != nil {
			// No such membership (or one in another tenant, which reads
			// identically): let the request through to org.Handler's own
			// Remove call, which answers its own not-found rather than this
			// gate inventing a scope refusal for a target that may not even
			// exist.
			return nil
		}
		nodeID = membership.NodeID
	}
	if nodeID == "" {
		return nil
	}

	if !nodeInScope(ctx, deps.Scope, scope, nodeID) {
		return rbac.ErrPermissionDenied.WithParam("permission", permission)
	}
	if target.destinationNodeID != "" && !nodeInScope(ctx, deps.Scope, scope, target.destinationNodeID) {
		return rbac.ErrPermissionDenied.WithParam("permission", permission)
	}
	return nil
}

// nodeInScope reports whether nodeID's materialized path (resolved through
// orgScope, the real tree) lies within dataScope.
//
// A node orgScope cannot resolve -- deleted, or belonging to another
// tenant, which org's own tenant-scoped repository reports identically --
// is treated as IN scope here, deliberately: this function's caller lets
// the request through to org.Handler's own lookup in that case, which
// answers the real "not found" error, rather than this gate leaking
// "exists but out of your scope" versus "does not exist" through two
// different refusal shapes.
func nodeInScope(ctx context.Context, orgScope org.Scope, dataScope rbac.DataScope, nodeID string) bool {
	path, err := orgScope.Path(ctx, nodeID)
	if err != nil {
		return true
	}
	return dataScope.Includes(path)
}

// MustResourceOf returns the shared resource half of the given permission
// strings, and panics when they do not agree on one.
//
// A panic is right here and only here: this runs at package
// initialization, before any request exists, and a disagreement means the
// permission constants this file gates on are not the ones it thinks they
// are -- an unrecoverable startup condition, which is the one case the
// backend coding standard's no-panic rule exempts. The split is rbac's
// own (SplitPermission, at the last separator), so this derivation reads a
// multi-segment resource exactly the way the gate that evaluates the
// permission later does.
func MustResourceOf(permissions ...string) string {
	var shared string
	for _, permission := range permissions {
		resource, _, ok := rbac.SplitPermission(permission)
		if !ok {
			panic(fmt.Sprintf("reference-app: %q is not a <resource>:<action> permission", permission))
		}
		if shared == "" {
			shared = resource
			continue
		}
		if resource != shared {
			panic(fmt.Sprintf("reference-app: permissions %v span more than one resource (%q and %q)", permissions, shared, resource))
		}
	}
	return shared
}

// DemoSubjectResolver is what this example plugs into
// rbac.WithSubjectResolver: the seam through which the authenticating side
// hands rbac an identity, without either module importing the other.
//
// The parts come from deliberately different places, and the differences
// are the point:
//
//   - The TENANT comes from the request context, where tenancy.Middleware
//     put it after resolving it server-side. It is never read from the
//     header below, and never from anything else the caller controls --
//     accepting a caller-supplied tenant_id is the single most common
//     horizontal-privilege-escalation bug in multi-tenant systems, and
//     forbidden here outright.
//   - The USER comes from one of two sources. DemoUserHeader comes first,
//     and that order is deliberate: the header is the affordance the
//     pre-auth demo flows were built around (it names which seeded demo
//     actor is acting -- its own comment spells out the trade-off), and
//     those flows send it alongside tokens whose accounts hold no rbac
//     grants, so the header keeps deciding first, or every one of them
//     changes meaning.
//   - Only when no demo header is present does the resolver read the
//     request context's verified Principal -- the user authn's access
//     token proved, which is where a real client's identity comes from.
//     This is the branch that lets the accounts demo_users.go seeds (real
//     users, real memberships, real grants) act from a browser that never
//     sends the header.
//
// The header's precedence over the Principal is the remaining scaffold:
// it is still an unauthenticated claim, and it still overrides a proven
// identity when both are present. It is a demo affordance only, and it
// goes away together with the header itself (see DemoUserHeader).
//
// DemoUserHeader's own doc comment names the real kill switch for
// exactly that precedence problem (the DisableDemoUserHeader bootstrap
// field, APP_DISABLE_DEMO_USER_HEADER): this function keeps the unconditional
// header-first behavior -- cmd/server/demo_subject_test.go pins it -- while
// production wiring goes through DemoSubjectResolverFor below, which is
// what actually honors the switch.
//
// It fails closed: no tenant, no user (from either source), or an
// incomplete pair reports (Subject{}, false), and rbac's gate turns that
// into a 403.
func DemoSubjectResolver(r *http.Request) (rbac.Subject, bool) {
	return DemoResolveSubject(r, false)
}

// DemoResolveSubject is DemoSubjectResolver's implementation, parameterized
// by headerDisabled so the kill switch DemoUserHeader's doc comment
// describes can turn the header off without touching the header-enabled
// behavior at all. headerDisabled=false is the header-enabled body (the
// header read first, the Principal read only when the header is absent);
// headerDisabled=true skips the header read entirely and resolves the user
// from the verified Principal alone.
//
// The TENANT half is untouched by headerDisabled either way -- it always
// comes from the request context (tenancy.Middleware's resolution), never
// from anything the caller controls, per DemoSubjectResolver's own doc
// comment.
func DemoResolveSubject(r *http.Request, headerDisabled bool) (rbac.Subject, bool) {
	tenantID, ok := pkgcore.TenantFromContext(r.Context())
	if !ok || tenantID == "" {
		return rbac.Subject{}, false
	}

	var userID string
	if !headerDisabled {
		userID = r.Header.Get(DemoUserHeader)
	}
	if userID == "" {
		principal, ok := authn.PrincipalFromContext(r.Context())
		if !ok {
			return rbac.Subject{}, false
		}
		userID = principal.UserID
	}

	sub := rbac.Subject{TenantID: tenantID, UserID: userID}
	if !sub.Valid() {
		return rbac.Subject{}, false
	}
	return sub, true
}

// DemoSubjectResolverFor returns the rbac.SubjectResolver the gated entries
// of DemoRouteRules name (all but the public ones and the two that
// deliberately deviate, pki's and admin's), chosen by headerDisabled -- the value
// BuildServer threads from cfg.DisableDemoUserHeader, itself resolved from
// APP_DISABLE_DEMO_USER_HEADER (the DisableDemoUserHeader bootstrap field,
// internal/app/bootstrap.go).
//
// headerDisabled=false (the default) returns DemoSubjectResolver itself,
// so every demo journey and test that depends on the header winning keeps
// working unchanged. headerDisabled=true returns a resolver that never
// reads
// DemoUserHeader at all -- the kill switch DemoUserHeader's own doc comment
// describes, for a deployment where a real, non-demo user might reach this
// binary.
func DemoSubjectResolverFor(headerDisabled bool) func(*http.Request) (rbac.Subject, bool) {
	if !headerDisabled {
		return DemoSubjectResolver
	}
	return func(r *http.Request) (rbac.Subject, bool) {
		return DemoResolveSubject(r, true)
	}
}

// DemoPermissionFor chooses the permission a request must hold, from the
// resource its route is guarded by and its HTTP method.
//
// It depends on the ROUTE and nothing else -- never a header, a query
// parameter or a body field, because a permission the caller can choose is
// a permission the caller can choose to be one they hold. Anything that is
// not a read method requires the write permission, which is deliberately
// the strict direction: a method this example never thought about (PATCH,
// an exotic verb) demands more authority rather than less.
func DemoPermissionFor(resource string) func(*http.Request) string {
	return func(r *http.Request) string {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return rbac.Permission(resource, DemoActionRead)
		default:
			return rbac.Permission(resource, DemoActionWrite)
		}
	}
}

// SeedDemoGrants gives every configured tenant its built-in roles and the
// demo users their grants, so `go run ./cmd/server` demonstrates a
// working gate with no setup at all.
//
// It runs once per boot and is idempotent in both halves:
// EnsureBuiltinRoles reconciles rather than recreates, and AssignRole is a
// no-op when the grant is already there. Each tenant is seeded under its
// OWN tenant context -- roles and bindings are tenant data, and nothing
// here reads or writes across a tenant boundary.
//
// The demo users seeded here are the fixed header ids of DemoUserHeader,
// with no database row behind them -- which is exactly why they cannot
// sign in, and why demo_users.go additionally registers real accounts
// whose memberships and grants mirror this same model. A real deployment
// does neither: roles are seeded when a tenant is created and grants are
// made by an administrator through the admin console.
func SeedDemoGrants(ctx context.Context, svc *rbac.Service, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seeded[tenantID]; done {
			// Two demo hosts can map to one tenant; seed it once.
			continue
		}
		seeded[tenantID] = struct{}{}

		// Every role this seed defines and grants is audited by rbac under
		// the Actor this context carries -- the seed's own system actor
		// (demoSeedCtx) -- so the rows name the seed itself rather than
		// landing with a blank attribution.
		seedCtx := demoSeedCtx(pkgcore.WithTenant(ctx, tenantID))
		if err := svc.EnsureBuiltinRoles(seedCtx); err != nil {
			return fmt.Errorf("reference-app: seed the built-in roles of %q: %w", tenantID, err)
		}
		if err := seedDemoReaderRole(seedCtx, svc); err != nil {
			return fmt.Errorf("reference-app: seed the demo reader role of %q: %w", tenantID, err)
		}
		if err := seedDemoAIGatewayTenantWriterRole(seedCtx, svc); err != nil {
			return fmt.Errorf("reference-app: seed the demo ai-gateway tenant-writer role of %q: %w", tenantID, err)
		}

		grants := []struct {
			userID  string
			roleKey string
		}{
			{userID: DemoOwnerUserID, roleKey: rbac.BuiltinRoleOwner},
			{userID: DemoReaderUserID, roleKey: demoReaderRoleKey},
			// DemoAIGatewayTenantWriterUserID is granted in EVERY tenant,
			// like DemoReaderUserID: the two-tier gate this actor
			// demonstrates (tenant BYOK write allowed, platform write
			// refused) is a property of its role's permission set, not of
			// any one tenant.
			{userID: DemoAIGatewayTenantWriterUserID, roleKey: demoAIGatewayTenantWriterRoleKey},
		}
		if tenantID == DemoSingleTenantID {
			grants = append(grants, struct {
				userID  string
				roleKey string
			}{userID: DemoSingleTenantUserID, roleKey: demoReaderRoleKey})
		}
		for _, grant := range grants {
			sub := rbac.Subject{TenantID: tenantID, UserID: grant.userID}
			// A tenant-wide Scope: this example has no organization tree,
			// so it wires no rbac.SubtreeResolver either, and a
			// node-scoped grant would correctly be denied for want of one.
			if err := svc.AssignRole(seedCtx, sub, grant.roleKey, rbac.Scope{}); err != nil {
				return fmt.Errorf("reference-app: grant %q to %q in %q: %w", grant.roleKey, grant.userID, tenantID, err)
			}
		}
	}
	return nil
}

// seedDemoReaderRole defines the read-only demo role in the tenant ctx
// carries, tolerating the role already existing from an earlier boot.
//
// rbac's DefineRole is create-only by design, so "already there" comes
// back as a conflict rather than as success; this is the caller-side
// idempotence that create-only API implies.
func seedDemoReaderRole(ctx context.Context, svc *rbac.Service) error {
	_, err := svc.DefineRole(ctx, rbac.RoleDefinition{
		Key: demoReaderRoleKey,
		// An i18n message id, never display prose: a role row must not
		// carry user-facing text in one language.
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{notes.PermissionRead},
	})
	if err != nil && !apperr.HasCode(err, rbac.ErrDuplicateRole.Code) {
		return err
	}
	return nil
}

// seedDemoAIGatewayTenantWriterRole defines the role demoAIGatewayTenantWriterRoleKey
// names, in the tenant ctx carries, tolerating the role already existing
// from an earlier boot -- the identical caller-side idempotence
// seedDemoReaderRole spells out.
//
// The role carries exactly aigateway.PermissionRead and
// aigateway.PermissionWrite -- the tenant-scoped half of the two-tier
// gate -- and DELIBERATELY NOT aigateway.PermissionManagePlatform.
// DemoOwnerUserID (BuiltinRoleOwner) holds every declared permission, so it
// cannot demonstrate the platform write being refused; this role is the
// tenant-writer that can set its own tenant's BYOK credential over HTTP and
// is refused the platform-wide write, which is the distinction the two
// permissions exist to enforce (see DemoAIGatewayTenantWriterUserID's own
// comment and go/ai-gateway/module.go's doc comment on the two constants).
func seedDemoAIGatewayTenantWriterRole(ctx context.Context, svc *rbac.Service) error {
	_, err := svc.DefineRole(ctx, rbac.RoleDefinition{
		Key:            demoAIGatewayTenantWriterRoleKey,
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{aigateway.PermissionRead, aigateway.PermissionWrite},
	})
	if err != nil && !apperr.HasCode(err, rbac.ErrDuplicateRole.Code) {
		return err
	}
	return nil
}
