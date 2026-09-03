package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/rbac"
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

	// demoSingleTenantUserID holds demoReaderRoleKey in ONE tenant only,
	// which is what makes this example demonstrate the single most
	// important property of a grant: it is a fact about a (tenant, user)
	// PAIR, never about a user. The same id acting under the other demo
	// host holds nothing at all and is refused -- not because it is
	// unknown, but because its grant lives in a different tenant.
	demoSingleTenantUserID = "demo-acme-only"
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
var demoRouteGuards = map[string]string{
	notesRoutePath:   notesResource,
	storageRoutePath: storageResource,
	orgRoutePath:     routePublic,
	// authn's path constant lives in server.go, which owns the pre-auth
	// (method, path) allowlist under it; naming the path here through that
	// same constant keeps the two in sync the way config's entries do.
	authnAPIPath: routePublic,
	// The config module's two pre-auth endpoints, named through its own
	// exported constants so a rename cannot drift into a silently ungated
	// path here.
	config.PathPublic:         routePublic,
	config.PathSystemFeatures: routePublic,
}

// notesResource is the resource half of notes' permission strings. It is
// derived from the module's own exported constants rather than retyped, so
// this example cannot drift from the permissions notes actually declares.
var notesResource = mustResourceOf(notes.PermissionRead, notes.PermissionWrite)

// storageResource is the resource half of storage's permission strings,
// derived from its own exported constants the same way notesResource is
// derived from notes' -- so this example cannot drift from the
// permissions storage actually declares either.
var storageResource = mustResourceOf(storage.PermissionRead, storage.PermissionWrite)

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
	return rbac.RequirePermissionFunc(az, demoPermissionFor(resource),
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

		grants := []struct {
			userID  string
			roleKey string
		}{
			{userID: demoOwnerUserID, roleKey: rbac.BuiltinRoleOwner},
			{userID: demoReaderUserID, roleKey: demoReaderRoleKey},
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

// isAlreadyDefined reports whether err is rbac's duplicate-role conflict.
// Classification goes through the CODE, not errors.Is against the
// sentinel: every WithParam call derives a new *apperr.Error, so the
// exported vars are templates rather than singletons.
func isAlreadyDefined(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == rbac.ErrDuplicateRole.Code
}
