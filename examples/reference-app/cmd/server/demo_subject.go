package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// This file holds everything this example needs to demonstrate rbac end to
// end: where a Subject comes from, which permission gates which route, and
// the demo grants seeded at startup. It is deliberately a file of its own
// rather than more of server.go.
//
// The "where a Subject comes from" half changed when authn landed: a
// request whose access token verified is now resolved from its Principal,
// the identity the authenticating side proved. demoUserHeader survives as
// the affordance the pre-auth flows were built around -- its own comment
// says exactly how the two sources share the resolver -- and demo_users.go
// seeds real accounts whose grants reach this same gate through the
// principal path, which is the shape this file settles into once the
// header goes away.

// demoUserHeader names the request header this example reads an acting
// user id from.
//
// THIS IS NOT AUTHENTICATION, and it is not a pattern to copy. An
// unauthenticated header is a claim, not an identity: anyone who can reach
// the server can set it to any value and become that user. It carries the
// same warning demoHostTenants and strictHostResolver carry in server.go.
//
// It predates authn: before access tokens existed it was the only way a
// request could name a user at all, and the flows that grew up around it
// (the permission-gate and isolation tests, the actor model
// seedDemoGrants below seeds) still use it to say which seeded demo actor
// is acting. demoSubjectResolver therefore still reads it first, so those
// flows keep meaning exactly what they always meant. What authn actually
// replaced is everything else: a request carrying no demo header is now
// resolved from the verified Principal authn.Middleware put in the request
// context -- which is how the real accounts demo_users.go seeds reach the
// same gate from a browser with no header at all.
//
// The header's remaining days are numbered: once the pre-auth flows move
// onto those real accounts nothing needs it, and it goes away together
// with the resolver's fallback. The consumer-shell plan records that
// removal as deferred to the org-web round.
const demoUserHeader = "X-Demo-User"

// The demo users seeded into every configured tenant. Two of them, because
// one user proves only that the gate opens: it takes a second, holding
// strictly less authority, to prove the gate also closes on a real user
// rather than only on an anonymous request.
const (
	// demoOwnerUserID holds the built-in owner role, so it carries every
	// permission any module declared -- including both of notes'.
	demoOwnerUserID = "demo-owner"

	// demoReaderUserID holds demoReaderRoleKey, a custom role carrying
	// notes:read and nothing else. It may list notes and may not create
	// one, which is the difference the permission gate exists to enforce.
	demoReaderUserID = "demo-reader"

	// demoReaderRoleKey is the tenant-scoped key of that read-only role.
	// It is defined per tenant, like every role: roles are tenant data,
	// and there is deliberately no cross-tenant template to copy from.
	demoReaderRoleKey = "note-reader"

	// demoNotesCreatorUserID is the X-Demo-User-Id header value the flow
	// helpers that create notes send on every request. X-Demo-User-Id is a
	// different namespace from X-Demo-User (the header this file's earlier
	// const names): the latter names the seeded rbac grant the gate
	// decides against (demo-owner and friends above), while the former
	// names the user id notes' own SubjectResolver (demoNotesSubjectResolver
	// in server.go) attributes the CREATE to -- the value that lands in a
	// note's CreatorUserID and in the NoteCreatedPayload event every
	// subscriber reads. The value is a real-user-style id (org_flow_test's
	// "user-owner-1" is the same shape), not an rbac demo identity, exactly
	// as a real deployment's access-token subject claim would be -- which
	// is also why demo_notification.go's address table keys on it: every
	// note-created event this app's own flow helpers publish names it. An
	// event can instead name a seeded account's real user id, when the
	// account acts through its access token with no demo header at all --
	// demoNotesSubjectResolver falls back to the verified Principal, the
	// same fallback the rbac gate's demoSubjectResolver makes -- and that
	// id, absent from the address table, resolves to no addresses: an
	// ordinary miss, never an error.
	demoNotesCreatorUserID = "user-creator-1"

	// demoSingleTenantUserID holds demoReaderRoleKey in ONE tenant only,
	// which is what makes this example demonstrate the single most
	// important property of a grant: it is a fact about a (tenant, user)
	// PAIR, never about a user. The same id acting under the other demo
	// host holds nothing at all and is refused -- not because it is
	// unknown, but because its grant lives in a different tenant.
	demoSingleTenantUserID = "demo-acme-only"

	// demoAIGatewayTenantWriterUserID holds demoAIGatewayTenantWriterRoleKey,
	// a custom role carrying exactly aigateway.PermissionWrite (and
	// aigateway.PermissionRead) and NOT aigateway.PermissionManagePlatform.
	// It exists purely to prove this round's two-tier permission gate is
	// real: demoOwnerUserID holds BuiltinRoleOwner, which -- per rbac's own
	// builtin.go doc comment -- carries EVERY permission any module
	// declared, platform-scope credential write included, so it cannot
	// demonstrate the platform write being refused. This user can set its
	// OWN tenant's BYOK credential but is refused the platform-wide write,
	// which is the distinction the two permissions exist to enforce.
	demoAIGatewayTenantWriterUserID = "demo-aigateway-tenant-writer"

	// demoAIGatewayTenantWriterRoleKey is the tenant-scoped key of that
	// role. It is defined per tenant, like every role: roles are tenant
	// data, and there is deliberately no cross-tenant template to copy
	// from.
	demoAIGatewayTenantWriterRoleKey = "ai-gateway-tenant-writer"
)

// demoSingleTenantID is the one tenant demoSingleTenantUserID is granted
// in. It is a literal rather than "whichever tenant comes first" because
// map iteration order is unspecified, and a demo whose grants move between
// boots would be a poor thing to reason about.
const demoSingleTenantID pkgcore.TenantID = "tenant-acme"

// The two action halves this example composes permission strings from. A
// permission is "<resource>:<action>" (rbac.Permission), and notes declares
// exactly notes:read and notes:write.
const (
	demoActionRead  = "read"
	demoActionWrite = "write"
)

// routePublic marks a mounted route that must NOT be gated on a
// permission. It is the empty resource, spelled as a named constant so a
// reader of demoRouteGuards sees an intentional decision rather than a
// forgotten entry.
const routePublic = ""

// notesRoutePath is where the notes module mounts its route. This example
// needs the literal because the module keeps its own path unexported --
// and it does not need to keep the two in sync by hand: mountModuleRoutes
// refuses to start when a module mounts a path demoRouteGuards does not
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

// pkiRoutePath is where the pki module mounts its routes -- the same
// unexported-path situation notesRoutePath's own comment explains.
const pkiRoutePath = "/api/v1/pki"

// integrationRoutePath is where go/integration's spec-generated HTTP
// fragments mount their routes -- the same unexported-path situation
// notesRoutePath's own comment explains. One mount path serves BOTH of the
// module's gated entities: the round-5 API-key CRUD fragment and the
// round-7 webhook-subscription CRUD + recent-deliveries fragment live under
// this single path (go/integration's Handler implements both halves of the
// generated ServerInterface and module.go's Register mounts it once, at
// /api/v1/integration), so the gate below must dispatch by SUB-PATH to tell
// the two permission pairs apart -- see integrationRouteSentinel's own doc
// comment for the full argument.
//
// Renamed from integrationAPIKeyRoutePath in round 7, when the path stopped
// belonging to the API-key fragment alone. The rename also retires the
// identifier's "APIKey" stem, which is what gosec's hardcoded-credential
// heuristic (G101) used to match on -- the #nosec comment that identifier
// once carried is gone with it, since neither this name nor this value is
// credential-shaped.
const integrationRoutePath = "/api/v1/integration"

// sharingSharesRoutePath is where sharing's round-3 owner-facing operations
// (create, list, get, revoke, list access log) are mounted -- named through
// the module's own exported sharing.PathShares constant, mirroring every
// other *RoutePath constant's use of an exported module constant where one
// exists. Deliberately a DIFFERENT table entry from sharing.PathAccess
// (below, routePublic): sharing mounts the same *Handler at both paths, but
// PathAccess is genuinely unauthenticated and PathShares is an ordinary
// tenant-scoped, permission-gated surface -- see module.go's own Register
// doc comment in go/sharing for the full contrast.
const sharingSharesRoutePath = sharing.PathShares

// aiGatewayRoutePath is where go/ai-gateway's round-3 credential-write
// admin surface mounts its routes -- the same unexported-path situation
// notesRoutePath's own comment explains.
const aiGatewayRoutePath = "/api/v1/ai-gateway"

// demoRouteGuards declares, for every path a module mounts, the resource
// whose permissions gate it -- or routePublic when the path is
// deliberately reachable without one.
//
// The map is exhaustive by construction: mountModuleRoutes fails the
// server build for any mounted path missing from it. That direction
// matters. A table whose default is "ungated" quietly serves every route a
// future module adds; a table whose default is "refuse to start" cannot.
//
// config's two paths are routePublic for the same reason they are
// allowlisted in tenancy.Middleware (see buildServer): they are pre-auth
// display surfaces -- a login page's brand and feature flags -- that must
// render before anyone has signed in, and they serve only what the design
// marks public, never tenant data.
//
// org's path is routePublic for a different reason: org.Handler already
// resolves and requires its own caller identity per operation through
// SubjectResolver (demoOrgSubjectResolver in server.go) -- org_createNode
// aside, which resolves none at all -- and org_flow_test.go's own demo
// callers (an invitee accepting an invitation, most pointedly) are never
// among the two users seedDemoGrants below seeds a role for, since a
// person accepting their first invitation has by definition no rbac grant
// yet. Gating this path on a coarse rbac permission on top of org's own
// per-operation identity check would refuse exactly the flow org exists to
// demonstrate. A real deployment layers a genuine permission check inside
// org's own handlers (or wires org's Scope into rbac's DataScope, the
// no-import seam org.md documents), not at this router gate.
//
// authn's path is routePublic for the same structural reason org's is:
// authn.Handler resolves and requires its own caller identity per
// operation through requirePrincipal (server.go's authnPreAuthAllowlist
// names the pre-auth exceptions), and the operations that must work before
// anyone has a Principal at all -- registration, every sign-in entry point,
// token refresh, the social authorize/callback pair -- are a deliberately
// ungated surface. Gating the whole path on a coarse rbac permission would
// refuse the sign-in flow this app exists to demonstrate; authn's own
// per-operation requirePrincipal is where its gate lives.
//
// storage's path is gated like notes', because storage's handlers perform
// no identity check of their own: the module declares its permissions and
// leaves their enforcement to the host's authorization layer, and this
// router gate is where the example enforces them. The demo grants seed
// storage's permissions into no role but the built-in owner -- the demo
// reader holds notes:read and nothing else -- which storage_flow_test.go
// relies on to prove the gate closes on a user who holds another module's
// permissions: a per-module permission is not a blanket role.
//
// notification's path is routePublic for the same structural reason org's
// and authn's are: notification.Handler resolves and requires its own
// caller identity per operation through SubjectResolver
// (demoOrgSubjectResolver in server.go -- the same seam instance org's
// own caller-scoped endpoints use; notes' create handler resolves through
// its own demoNotesSubjectResolver, which additionally accepts the
// verified Principal, its comment saying why notes alone gets that
// second source). Every one
// of the module's operations, the realtime stream included, refuses an
// unidentifiable caller with ErrSubjectUnresolved; the module declares no
// permissions at all -- its endpoints are a user's own inbox, contacts
// and preferences, never a cross-tenant surface -- so there is no rbac
// permission a router gate could meaningfully require, and its own
// per-operation subject check is where its gate lives, exactly as org's
// invitation endpoints' gate lives inside org.
//
// integration's path is gated for real, like storage's and pki's -- its
// Handler performs no identity check of its own beyond resolving a creator
// for the two create operations (see integration.SubjectResolver), which is
// a different question from whether the CALLER may reach the route at all.
// It is dispatched to guardIntegrationRoute, never demoPermissionFor, for
// the same class of reason pki's path needed its own permissionFor: this
// module's permission strings carry THREE segments -- round 5's
// "integration:apikey:read"/"integration:apikey:manage" and round 7's
// "integration:webhook:read"/"integration:webhook:manage", both sharing one
// mount path -- and neither demoPermissionFor's generic composition nor
// rbac.RequirePermissionFunc's own splitPermission (go/rbac/middleware.go)
// can parse a string whose action half contains a second colon. See
// integrationRouteSentinel's own doc comment for the full argument, and
// guardIntegrationRoute's for the sub-path dispatch that tells the two
// permission pairs apart.
var demoRouteGuards = map[string]string{
	notesRoutePath:   notesResource,
	storageRoutePath: storageResource,
	orgRoutePath:     routePublic,
	// notification's path constant mirrors the module's unexported
	// apiPath; naming it here through the local constant keeps the two in
	// sync the way the config entries do.
	notificationRoutePath: routePublic,
	// authn's path constant lives in server.go, which owns the pre-auth
	// (method, path) allowlist under it; naming the path here through that
	// same constant keeps the two in sync the way config's entries do.
	authnAPIPath: routePublic,
	// The config module's two pre-auth endpoints, named through its own
	// exported constants so a rename cannot drift into a silently ungated
	// path here.
	config.PathPublic:         routePublic,
	config.PathSystemFeatures: routePublic,
	// sharing.PathAccess is routePublic for the same structural reason
	// config's two are: it is a genuinely unauthenticated surface by
	// design (go/sharing's Handler doc comment), gated on nothing an rbac
	// permission check could evaluate -- an anonymous visitor holding a
	// bearer share token has no Subject at all. sharing.Service.AccessPublic
	// is where this route's real gate lives (the token itself, and the
	// tenant-and-share state it resolves to), exactly as authn's and org's
	// own per-operation checks are where their routePublic entries' real
	// gates live.
	sharing.PathAccess: routePublic,
	// sharingSharesRoutePath (sharing.PathShares) is gated like notes' and
	// storage's: sharing's Handler performs no authorization of its own for
	// these five owner-facing operations, leaving their enforcement to the
	// host's authorization layer, exactly as go/sharing's own module.go
	// Register doc comment states. It needs its own action selector rather
	// than demoPermissionFor's generic read/write split, since a POST here
	// means either sharing:create (create) or sharing:revoke (revoke) --
	// see sharingPermissionFor's own doc comment, the same pkiPermissionFor-
	// style carve-out guardModuleRoute already makes for pki's path.
	sharingSharesRoutePath: sharingResource,
	// pki's path was a KNOWN, PRE-EXISTING GAP (routePublic) when this
	// table first grew an entry for it: pki mounts a real, fine-grained
	// permission vocabulary (pki.PermissionRead/Issue/Revoke/Rotate) and
	// its handler performs no identity check of its own -- the
	// storage-style shape this table's own doc comment describes, which
	// normally means router gating -- but demoPermissionFor's binary
	// read/write split cannot express four distinct permissions, so this
	// router-level gate was simply never added when pki's HTTP surface
	// landed. It is gated for real now: pkiResource marks the path
	// non-public, and guardModuleRoute substitutes pkiPermissionFor (see
	// its own doc comment) for demoPermissionFor specifically for this
	// path, since GET (JWKS/CRL export) and POST (the two round-3 revoke
	// operations -- signing-key and certificate) need pki's own
	// Read/Revoke permissions, not a generic write bucket pki never
	// declares.
	//
	// What this narrower fix does NOT reach, left as follow-up for
	// whoever owns pki's reference-app wiring: PermissionIssue and
	// PermissionRotate are declared but have no HTTP operation in this
	// round's fragment at all (issuance and manual rotation stay Go-only
	// per go/pki/api/openapi.yaml's own header), so there is nothing yet
	// to gate for either; and pki_revokeSigningKey acts on
	// pki_signing_keys, which is platform data with no tenant column at
	// all (its own openapi.yaml description), so granting PermissionRevoke
	// to a tenant's BuiltinRoleOwner -- as seedDemoGrants does, purely to
	// demonstrate the gate closing on someone who lacks it -- hands that
	// tenant's owner a lever that reaches every OTHER tenant's signing
	// material too. A real deployment should restrict PermissionRevoke to
	// a platform-admin role instead, and a follow-up round should give
	// pki a genuine per-operation check (or a DataScope split between the
	// tenant-scoped certificate revoke and the platform-wide signing-key
	// one) rather than relying on this router-level gate alone.
	pkiRoutePath: pkiResource,

	// integration's path is one of two entries in this table whose value is
	// never read as a plain "resource" (admin's, just below, is the
	// other) -- guardModuleRoute special-cases it to
	// guardIntegrationRoute, which derives resource and action from this
	// module's own three-segment permission names directly, never from
	// rbac.Permission/splitPermission's "<resource>:<action>" round trip,
	// and dispatches between the module's TWO permission pairs by sub-path
	// (see integrationRouteSentinel's own doc comment). integrationRouteSentinel
	// exists purely so this map stays exhaustive.
	integrationRoutePath: integrationRouteSentinel,

	// admin's path is the other such entry: guardModuleRoute special-cases
	// it to guardAdminRoute (demo_admin.go), which gates by SUB-PATH
	// (tenants, users, impersonation, audit-events), not by one resource's
	// read/write split. adminRouteSentinel exists purely so this map
	// stays exhaustive -- mountModuleRoutes still refuses to start for any
	// mounted path this table does not name at all.
	adminRoutePath: adminRouteSentinel,

	// ai-gateway's path is gated for real, like storage's: the module's
	// Handler performs no permission check of its own (see its own doc
	// comment), leaving enforcement to the host's authorization layer. It
	// needs its own action selector, not demoPermissionFor's generic
	// read/write split, since a non-GET request here means either
	// aigateway:write (the tenant's own BYOK write) or
	// aigateway:manage_platform (the platform-wide write) -- see
	// aiGatewayPermissionFor's own doc comment, the same pkiPermissionFor/
	// sharingPermissionFor-style carve-out this table already makes for
	// those two paths.
	aiGatewayRoutePath: aiGatewayResource,
}

// adminRouteSentinel marks demoRouteGuards' one entry that guardModuleRoute
// dispatches to guardAdminRoute instead of the generic
// demoPermissionFor(resource) gate. It is a value distinct from
// routePublic and from any real resource string, so a reader (and
// guardModuleRoute's own switch) cannot confuse it with either.
const adminRouteSentinel = "ADMIN_SPECIAL_CASED_ROUTE"

// integrationRouteSentinel marks demoRouteGuards' entry for go/integration's
// mounted spec-generated fragments. guardModuleRoute dispatches it to
// guardIntegrationRoute instead of the generic demoPermissionFor(resource)
// gate.
//
// # Why this needs its own dispatch, not just its own permissionFor (unlike pki)
//
// pki's path (above) still goes through rbac.RequirePermissionFunc -- only
// ITS CHOICE of permission per request needed a dedicated function
// (pkiPermissionFor), because pki's own permission strings
// ("pki:read"/"pki:revoke") are ordinary one-colon "<resource>:<action>"
// values that RequirePermissionFunc's splitPermission (go/rbac/
// middleware.go) parses just fine. go/integration's permissions are declared
// one segment deeper -- "integration:apikey:read"/"integration:apikey:manage"
// (round 1's constants, first driven through a real HTTP gate in round 5)
// and "integration:webhook:read"/"integration:webhook:manage" (round 2's
// constants, round 7's fragment) -- following this codebase's
// "<module>:<entity>:<verb>" convention for a module with more than one
// gated entity (go/integration/module.go's own doc comments on its
// permission constants) -- and splitPermission's own doc comment is
// explicit that a string with a second colon in its action half is refused
// outright, regardless of what the caller holds: "a:b:c" denies
// unconditionally. rbac.RequirePermissionFunc is therefore not usable here
// AT ALL, not even with a custom permissionFor, so guardIntegrationRoute
// below reimplements its fail-closed shape by hand, deriving resource and
// action directly from this module's own permission constants instead of
// round-tripping them through rbac.Permission/splitPermission.
//
// Round 4's wireIntegrationWebhooks route (webhooks.go) found and worked
// around this mismatch first, for the webhook pair, by calling az.Can
// directly in a hand-mounted demo route outside the OpenAPI machinery;
// round 5 generalized the identical fix into this router-level gate when
// the API-key fragment joined the generic mountModuleRoutes loop
// (reg.Routes.Routes()), and round 7 extended the same gate to the webhook
// pair as that fragment's operations joined the same mount -- retiring the
// hand-mounted route, whose az.Can-direct argument now lives here.
//
// # Why one gate, two permission pairs, and sub-path dispatch
//
// go/integration mounts ONE handler on ONE path, /api/v1/integration
// (module.go's apiPath), serving round 5's API-key operations and round 7's
// webhook-subscription operations through the same mount -- a Go 1.22
// ServeMux hands a mounted handler the FULL request path (mountModuleRoutes
// registers exactly the module's own mount, so r.URL.Path always carries
// the sub-path remainder), and gate's permission selector therefore sees
// whether the request targets "/webhooks" and picks the webhook pair, and
// otherwise the API-key pair. This is the same in-file precedent
// sharingPermissionFor's "/revoke" suffix check and aiGatewayPermissionFor's
// "/platform" suffix check already establish: the path is what routed the
// request to this gate in the first place, never a value a caller supplies
// independently of it.
const integrationRouteSentinel = "INTEGRATION_ROUTE_THREE_SEGMENT_PERMISSION"

// notesResource is the resource half of notes' permission strings. It is
// derived from the module's own exported constants rather than retyped, so
// this example cannot drift from the permissions notes actually declares.
var notesResource = mustResourceOf(notes.PermissionRead, notes.PermissionWrite)

// storageResource is the resource half of storage's permission strings,
// derived from its own exported constants the same way notesResource is
// derived from notes' -- so this example cannot drift from the
// permissions storage actually declares either.
var storageResource = mustResourceOf(storage.PermissionRead, storage.PermissionWrite)

// pkiResource is the resource half of pki's permission strings, derived the
// same way notesResource and storageResource are. It exists only to give
// demoRouteGuards[pkiRoutePath] a non-routePublic value; guardModuleRoute
// never calls demoPermissionFor(pkiResource) for this path -- it uses
// pkiPermissionFor instead, since pki's real action vocabulary
// (Read/Issue/Revoke/Rotate) is not the generic read/write pair
// demoPermissionFor assumes.
var pkiResource = mustResourceOf(pki.PermissionRead, pki.PermissionRevoke)

// pkiPermissionFor selects the permission a pki request must hold, mirroring
// demoPermissionFor's GET/HEAD-vs-everything-else split but naming pki's own
// two round-3 HTTP-reachable permissions directly rather than a generic
// write bucket pki never declares. Of pki's four declared permissions, only
// PermissionRead and PermissionRevoke have an HTTP operation in this
// round's fragment at all (go/pki/api/openapi.yaml): the three GET
// operations export a JWKS or a CRL (PermissionRead), and the two POST
// operations both revoke something -- a signing key or a certificate
// (PermissionRevoke). PermissionIssue and PermissionRotate stay ungated
// here because nothing under this path invokes them over HTTP yet -- see
// demoRouteGuards' own pkiRoutePath entry for the full reasoning and what
// remains open.
func pkiPermissionFor(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return pki.PermissionRead
	default:
		return pki.PermissionRevoke
	}
}

// integrationPermissionFor selects the permission a request against
// go/integration's mounted fragment must hold. Unlike pkiPermissionFor's
// and sharingPermissionFor's single-pair selectors, this one must FIRST pick
// which of the module's two permission pairs the request targets, because
// both fragments share the one mount path integrationRoutePath: a request
// whose path continues "/webhooks" is round 7's webhook-subscription
// surface -- its GET/HEAD reads gate on integration.PermissionWebhookRead
// and its four mutations (create, update, delete, restore) on
// integration.PermissionWebhookManage, a method-only split like
// demoPermissionFor's since the webhook fragment's two read operations are
// both GETs -- and anything else under the mount is round 5's API-key
// surface, gated on integration.PermissionRead/PermissionManage the same
// way. See integrationRouteSentinel's own doc comment for why neither
// demoPermissionFor's generic composition nor rbac.RequirePermissionFunc
// itself can be used for either pair, and guardIntegrationRoute's for why
// the sub-path check is safe to gate on.
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

// guardIntegrationRoute wraps go/integration's mounted fragment routes in
// rbac's permission gate, reproducing rbac.RequirePermissionFunc's own
// fail-closed shape (nil-safety aside -- az and demoSubjectResolver are both
// always non-nil in this app's own wiring, unlike the general-purpose
// middleware) by hand instead of calling it, for the reason
// integrationRouteSentinel's own doc comment gives in full: this module's
// permission strings do not fit rbac.RequirePermissionFunc's
// "<resource>:<action>" contract at all. resource and action are derived by
// cutting integrationPermissionFor's answer at its LAST colon --
// "integration:apikey" and "read"/"manage", or "integration:webhook" and
// the webhook pair's own two verbs -- the identical split round 4's
// wireIntegrationWebhooks route (webhooks.go) hand-derived for this
// module's webhook pair, generalized here into a reusable middleware since
// these fragments' routes are mounted through the generic mountModuleRoutes
// loop rather than hand-mounted one at a time. Round 5's version guarded
// only the API-key half, with a method-only permission choice; round 7's
// change is confined to integrationPermissionFor's sub-path dispatch between
// the two pairs above -- this fail-closed shape is pair-agnostic and
// unchanged.
func guardIntegrationRoute(az rbac.Authorizer, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		permission := integrationPermissionFor(r)
		idx := strings.LastIndex(permission, ":")
		if idx <= 0 || idx == len(permission)-1 {
			// Unreachable for any permission integrationPermissionFor can
			// actually return -- all four are known-good constants -- but
			// handled anyway rather than assumed away, the same "never trust
			// a string shape silently" posture splitPermission itself takes.
			writeIntegrationError(w, rbac.ErrPermissionDenied.WithParam("permission", permission))
			return
		}
		resource, action := permission[:idx], permission[idx+1:]

		sub, ok := demoSubjectResolver(r)
		if !ok {
			writeIntegrationError(w, rbac.ErrPermissionDenied.WithParam("permission", permission))
			return
		}
		allowed, err := az.Can(r.Context(), sub, action, resource)
		if err != nil {
			writeIntegrationError(w, rbac.ErrStorage)
			return
		}
		if !allowed {
			writeIntegrationError(w, rbac.ErrPermissionDenied.WithParam("permission", permission))
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// integrationErrInternal folds any error that is not itself an *apperr.Error
// into go/integration's stable internal code -- the fallback the error
// writer below applies, the same shape consult.go's own writeConsultError
// gives its own module. Round 4's webhooks.go declared it next to its
// hand-mounted subscription route; that route retired in round 7 (see
// webhooks.go's package doc), and the helper moved here with the rest of
// the integration gate glue it serves.
var integrationErrInternal = apperr.Internal("integration.internal_error")

// writeIntegrationError writes err to w as a JSON {code, params} body, the
// same structured-error envelope shape consult.go's own writeConsultError
// produces -- a stable code plus structured parameters, never localized text
// (backend coding standard §6.2). It is the one error writer shared by the
// integration gate above (guardIntegrationRoute) and round 6's
// integrationWhoamiPath handler (integration_authenticate.go), which
// translate coded integration/rbac errors into HTTP responses; it moved
// here from webhooks.go when round 7 retired that file's hand-mounted
// route.
func writeIntegrationError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = integrationErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	envelope := map[string]any{"code": appErr.Code}
	if appErr.Params != nil {
		envelope["params"] = appErr.Params
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

// sharingResource is the resource half of sharing's owner-facing permission
// strings, derived from its own exported constants the same way
// notesResource and storageResource are -- so this example cannot drift
// from the permissions sharing actually declares. Unlike pkiResource,
// sharing's own three permissions (read/create/revoke) all genuinely share
// one resource half, so all three feed mustResourceOf here.
var sharingResource = mustResourceOf(sharing.PermissionRead, sharing.PermissionCreate, sharing.PermissionRevoke)

// sharingPermissionFor selects the permission a sharingSharesRoutePath
// request must hold, mirroring pkiPermissionFor's own reasoning: sharing's
// three-permission vocabulary (read/create/revoke) is not the generic
// read/write pair demoPermissionFor assumes, and unlike pki's own two
// HTTP-reachable permissions, sharing's create and revoke operations are
// BOTH POST requests that a method-only split cannot tell apart --
// sharing_createShare (POST /api/v1/sharing/shares) and sharing_revokeShare
// (POST /api/v1/sharing/shares/{shareId}/revoke). This selector also checks
// the request's own path suffix, which is safe to gate on for the identical
// reason demoRouteGuards' own table lookup is: the path is what routed the
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

// aiGatewayResource is the resource half of ai-gateway's permission
// strings, derived from its own exported constants the same way
// notesResource and storageResource are -- so this example cannot drift
// from the permissions ai-gateway actually declares. All three of its
// permissions genuinely share one resource half ("ai-gateway"), unlike
// integration's two-entity vocabulary.
var aiGatewayResource = mustResourceOf(aigateway.PermissionRead, aigateway.PermissionWrite, aigateway.PermissionManagePlatform)

// aiGatewayPermissionFor selects the permission an aiGatewayRoutePath
// request must hold, mirroring pkiPermissionFor's and sharingPermissionFor's
// own reasoning: ai-gateway's three-permission vocabulary (read/write/
// manage_platform) is not the generic read/write pair demoPermissionFor
// assumes. A GET is aigateway:read (aiGateway_getCredential); a PUT whose
// path ends in "/platform" is aigateway:manage_platform
// (aiGateway_setPlatformCredential), the materially more privileged
// operation this round's own brief requires a DISTINCT permission for;
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

// mustResourceOf returns the shared resource half of the given permission
// strings, and panics when they do not agree on one.
//
// A panic is right here and only here: this runs at package
// initialization, before any request exists, and a disagreement means the
// permission constants this file gates on are not the ones it thinks they
// are -- an unrecoverable startup condition, which is the one case the
// backend coding standard's no-panic rule exempts.
func mustResourceOf(permissions ...string) string {
	var shared string
	for _, permission := range permissions {
		resource, _, ok := splitDemoPermission(permission)
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

// splitDemoPermission divides "<resource>:<action>" the way rbac's own
// gate does. rbac keeps its splitter unexported -- a consumer composes
// permissions with rbac.Permission and rarely takes one apart -- so this
// example carries the four lines rather than asking for a public API it is
// the only caller of.
func splitDemoPermission(permission string) (resource, action string, ok bool) {
	resource, action, found := strings.Cut(permission, ":")
	if !found || resource == "" || action == "" {
		return "", "", false
	}
	return resource, action, true
}

// demoSubjectResolver is what this example plugs into
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
//     root CLAUDE.md forbids it outright.
//   - The USER comes from one of two sources. demoUserHeader comes first,
//     and that order is deliberate: the header is the affordance the
//     pre-auth demo flows were built around (it names which seeded demo
//     actor is acting -- its own comment spells out the history), and
//     those flows send it alongside tokens whose accounts hold no rbac
//     grants, so the header must keep deciding exactly as it always did
//     or every one of them changes meaning.
//   - Only when no demo header is present does the resolver read the
//     request context's verified Principal -- the user authn's access
//     token proved, which is where a real client's identity comes from.
//     This is the branch that lets the accounts demo_users.go seeds (real
//     users, real memberships, real grants) act from a browser that never
//     sends the header.
//
// The header's precedence over the Principal is the remaining scaffold:
// it is still an unauthenticated claim, and it still overrides a proven
// identity when both are present. It is only a demo affordance, and its
// removal is the org-web round's deferred work (see demoUserHeader).
//
// It fails closed: no tenant, no user (from either source), or an
// incomplete pair reports (Subject{}, false), and rbac's gate turns that
// into a 403.
func demoSubjectResolver(r *http.Request) (rbac.Subject, bool) {
	tenantID, ok := pkgcore.TenantFromContext(r.Context())
	if !ok || tenantID == "" {
		return rbac.Subject{}, false
	}

	userID := r.Header.Get(demoUserHeader)
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

// demoPermissionFor chooses the permission a request must hold, from the
// resource its route is guarded by and its HTTP method.
//
// It depends on the ROUTE and nothing else -- never a header, a query
// parameter or a body field, because a permission the caller can choose is
// a permission the caller can choose to be one they hold. Anything that is
// not a read method requires the write permission, which is deliberately
// the strict direction: a method this example never thought about (PATCH,
// an exotic verb) demands more authority rather than less.
func demoPermissionFor(resource string) func(*http.Request) string {
	return func(r *http.Request) string {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			return rbac.Permission(resource, demoActionRead)
		default:
			return rbac.Permission(resource, demoActionWrite)
		}
	}
}

// guardModuleRoute wraps one mounted module route in rbac's permission
// gate, or returns it untouched when demoRouteGuards marks the path
// public. A path the table does not name is an error, so buildServer fails
// to start rather than serving it ungated.
func guardModuleRoute(az rbac.Authorizer, path string, handler http.Handler) (http.Handler, error) {
	resource, declared := demoRouteGuards[path]
	if !declared {
		return nil, fmt.Errorf(
			"reference-app: a module mounted %q, which demoRouteGuards does not name; add it with the resource that gates it, or with routePublic if it is deliberately unauthenticated",
			path)
	}
	if resource == routePublic {
		return handler, nil
	}
	if resource == adminRouteSentinel {
		return guardAdminRoute(az, handler), nil
	}
	if resource == integrationRouteSentinel {
		// Not just a different action selector (like pki below) -- a
		// wholly different gate, bypassing rbac.RequirePermissionFunc
		// entirely. See integrationRouteSentinel's own doc comment for why.
		return guardIntegrationRoute(az, handler), nil
	}
	// pki, sharing and ai-gateway all need their own action selector, not
	// demoPermissionFor's generic read/write split -- see pkiPermissionFor's,
	// sharingPermissionFor's and aiGatewayPermissionFor's own doc comments.
	permissionFor := demoPermissionFor(resource)
	switch path {
	case pkiRoutePath:
		permissionFor = pkiPermissionFor
	case sharingSharesRoutePath:
		permissionFor = sharingPermissionFor
	case aiGatewayRoutePath:
		permissionFor = aiGatewayPermissionFor
	}
	return rbac.RequirePermissionFunc(az, permissionFor,
		rbac.WithSubjectResolver(demoSubjectResolver),
	)(handler), nil
}

// seedDemoGrants gives every configured tenant its built-in roles and the
// demo users their grants, so `go run ./cmd/server` demonstrates a
// working gate with no setup at all.
//
// It runs once per boot and is idempotent in both halves:
// EnsureBuiltinRoles reconciles rather than recreates, and AssignRole is a
// no-op when the grant is already there. Each tenant is seeded under its
// OWN tenant context -- roles and bindings are tenant data, and nothing
// here reads or writes across a tenant boundary.
//
// The demo users seeded here are the fixed header ids of demoUserHeader,
// with no database row behind them -- which is exactly why they cannot
// sign in, and why demo_users.go additionally registers real accounts
// whose memberships and grants mirror this same model. A real deployment
// does neither: roles are seeded when a tenant is created and grants are
// made by an administrator through the admin console.
func seedDemoGrants(ctx context.Context, svc *rbac.Service, tenants map[string]pkgcore.TenantID) error {
	seeded := make(map[pkgcore.TenantID]struct{}, len(tenants))
	for _, tenantID := range tenants {
		if _, done := seeded[tenantID]; done {
			// Two demo hosts can map to one tenant; seed it once.
			continue
		}
		seeded[tenantID] = struct{}{}

		tenantCtx := pkgcore.WithTenant(ctx, tenantID)
		if err := svc.EnsureBuiltinRoles(tenantCtx); err != nil {
			return fmt.Errorf("reference-app: seed the built-in roles of %q: %w", tenantID, err)
		}
		if err := seedDemoReaderRole(tenantCtx, svc); err != nil {
			return fmt.Errorf("reference-app: seed the demo reader role of %q: %w", tenantID, err)
		}
		if err := seedDemoAIGatewayTenantWriterRole(tenantCtx, svc); err != nil {
			return fmt.Errorf("reference-app: seed the demo ai-gateway tenant-writer role of %q: %w", tenantID, err)
		}

		grants := []struct {
			userID  string
			roleKey string
		}{
			{userID: demoOwnerUserID, roleKey: rbac.BuiltinRoleOwner},
			{userID: demoReaderUserID, roleKey: demoReaderRoleKey},
			// demoAIGatewayTenantWriterUserID is granted in EVERY tenant,
			// like demoReaderUserID: the two-tier gate this actor
			// demonstrates (tenant BYOK write allowed, platform write
			// refused) is a property of its role's permission set, not of
			// any one tenant.
			{userID: demoAIGatewayTenantWriterUserID, roleKey: demoAIGatewayTenantWriterRoleKey},
		}
		if tenantID == demoSingleTenantID {
			grants = append(grants, struct {
				userID  string
				roleKey string
			}{userID: demoSingleTenantUserID, roleKey: demoReaderRoleKey})
		}
		for _, grant := range grants {
			sub := rbac.Subject{TenantID: tenantID, UserID: grant.userID}
			// A tenant-wide Scope: this example has no organization tree,
			// so it wires no rbac.SubtreeResolver either, and a
			// node-scoped grant would correctly be denied for want of one.
			if err := svc.AssignRole(tenantCtx, sub, grant.roleKey, rbac.Scope{}); err != nil {
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
	if err != nil && !isAlreadyDefined(err) {
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
// aigateway.PermissionWrite -- the tenant-scoped half of this round's
// two-tier gate -- and DELIBERATELY NOT aigateway.PermissionManagePlatform.
// demoOwnerUserID (BuiltinRoleOwner) holds every declared permission, so it
// cannot demonstrate the platform write being refused; this role is the
// tenant-writer that can set its own tenant's BYOK credential over HTTP and
// is refused the platform-wide write, which is the distinction the two
// permissions exist to enforce (see demoAIGatewayTenantWriterUserID's own
// comment and go/ai-gateway/module.go's doc comment on the two constants).
func seedDemoAIGatewayTenantWriterRole(ctx context.Context, svc *rbac.Service) error {
	_, err := svc.DefineRole(ctx, rbac.RoleDefinition{
		Key:            demoAIGatewayTenantWriterRoleKey,
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{aigateway.PermissionRead, aigateway.PermissionWrite},
	})
	if err != nil && !isAlreadyDefined(err) {
		return err
	}
	return nil
}

// isAlreadyDefined reports whether err is rbac's duplicate-role conflict.
// Classification goes through the CODE, not errors.Is against the
// sentinel: every WithParam call derives a new *apperr.Error, so the
// exported vars are templates rather than singletons.
func isAlreadyDefined(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == rbac.ErrDuplicateRole.Code
}
