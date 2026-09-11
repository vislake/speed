// Package demo holds the reference app's demonstration layer: the fixed demo
// identities every surface resolves its caller through, the route authorization
// table the assembly applies, and the boot-time seeds that give the demo tenants
// their roles, credits, subscriptions and accounts.
package demo

import (
	"net/http"

	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
)

// DemoHostTenants is a hard-coded, obviously-temporary Host -> TenantID
// lookup. It exists only so this reference app has *some* way to render a
// tenant-specific brand on the config module's pre-auth display endpoints
// (configModule's tenancy.NewDomainResolver wiring in BuildServer below)
// without a real custom-domain table.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real Resolver must derive the tenant from a source the server itself
// controls -- never an unauthenticated, static Host map like this one,
// which anyone can trigger just by setting the Host header on an HTTP
// request. See go/tenancy/resolver.go's own Resolver doc comment for the
// same rule stated as a hard requirement on every implementation.
//
// Host does NOT select the tenant for anything else in this app. The notes
// API, and every other route this app protects, resolve their tenant from
// the caller's ACCESS TOKEN instead (authn.NewPrincipalResolver, wired
// below) -- an unauthenticated caller cannot choose a tenant just by
// setting Host, which a Host-keyed lookup like this one would otherwise
// allow. See BuildServer's middleware-chain doc comment for the full
// reasoning.
var DemoHostTenants = map[string]pkgcore.TenantID{
	"acme.demo.localhost":   "tenant-acme",
	"globex.demo.localhost": "tenant-globex",
}

// signInMemberships -- the authn.MembershipReader this app wires, whose
// customer-tenant answers read org's own memberships table live -- lives
// in internal/app/sign_in_memberships.go with its full rationale. The short version of
// why host glue must exist here at all is structural: authn never imports
// org -- the two sit at the same dependency tier, peers, neither importing
// the other -- so whatever answers authn's two membership questions must
// be supplied by the assembling application, exactly like
// DemoNotesSubjectResolver and OrgSubtreeResolverFor below are. The app's
// membership store starts empty, and who fills it depends on the boot:
//
//   - Every boot seeds the fixed demo header actors' rbac grants
//     (SeedDemoGrants) but NO memberships: those actors have no database
//     row, so nothing can sign in as them.
//   - A boot with APP_DEMO_USERS_PASSWORD set additionally registers the
//     three demo accounts of demo_users.go through the real register route
//     and places each into org's memberships table under every tenant its
//     actor model names -- which is what makes those real sign-ins succeed,
//     in this process and in any later one against the same database
//     (authn's resolveTenant refuses an account with no membership:
//     go/authn/service.go's nil-or-unseeded MembershipReader answer refuses
//     rather than allows, and the org rows are that answer now).
//   - Tests grant membership explicitly after registering an account
//     through the real HTTP surface (RegisterAndAuthenticate in
//     internal/apptest, flowtests/authn_e2e_test.go), keeping a reference to the same
//     store BuildServer itself wires.

// DemoOrgUserHeader is the header the demo identity closure
// DemoOrgSubjectResolverFor builds reads to identify the HTTP caller: the
// stand-in for the verified access-token claims a real deployment's
// resolver would read, in exactly the spirit of DemoHostTenants' own
// disclaimer above. A caller sets it to whatever user id it wants to act
// as, with no verification whatsoever -- which is fine for this reference
// app's own demonstration purposes and would be a critical vulnerability
// in any real deployment.
const DemoOrgUserHeader = "X-Demo-User-Id"

// DemoOrgSubjectResolverFor returns the demo identity closure every module
// that declares the structurally identical SubjectResolver seam resolves
// its callers through: org's two caller-scoped endpoints (creating and
// accepting an invitation), every notification endpoint (which resolves
// its caller's inbox, contacts and preferences through it) and
// integration's creator reads. Each module's own func adapter wraps it at
// the wiring site (org.SubjectResolverFunc and the two siblings -- see the
// compile-time checks below), so no host-side named type is needed to
// satisfy the seam. It exists only so this reference app has *some* way to
// demonstrate those endpoints end to end through a caller-chosen identity.
//
// It fails closed: no header, and no verified Principal in the wiring that
// reads one, reports ("", false), and the module's own per-operation
// refusal (notification.subject_unresolved,
// integration.subject_unresolved, org's sibling) is what a caller then
// sees. Resolution order in the header-enabled wiring: the X-Demo-User-Id
// header when present (the pre-auth flows' affordance); else, when
// principalFallback is set (org's wiring only), the verified Principal;
// else fail closed. In the headerDisabled wiring, the verified Principal
// alone.
//
// principalFallback is the deliberate exception for ORG's wiring alone, the
// same shape DemoNotesSubjectResolver's header-then-Principal fallback
// gives notes' and cases' creator seams: org's caller-scoped endpoints also
// serve browser-shaped callers -- a signed-in clinic owner whose requests
// carry a bearer token and no demo header -- so the org module is wired
// with principalFallback true, and a header-less request with a verified
// Principal resolves as that Principal's user. false (notification's,
// integration's and every test's wiring) keeps the header-only resolver,
// preserving the pinned refusal where it belongs.
//
// headerDisabled is the other deliberate exception to the "never falls back
// to the Principal" rule: an operator who sets APP_DISABLE_DEMO_USER_HEADER
// has declared this deployment reads no demo header at all, so the only
// identity left to resolve a caller from is the verified Principal.
// BuildServer passes cfg.DisableDemoUserHeader here, which is what extends
// the kill switch -- alone it would only reach DemoUserHeader in the rbac
// gate -- to the org, notification and integration surfaces this closure
// serves. See the DisableDemoUserHeader field's own doc comment
// (internal/app/bootstrap.go) for the full contract.
//
// This is a placeholder, not a pattern to copy into a real deployment: a
// real SubjectResolver must derive the caller from a source the server
// itself verified (a validated access token's subject claim), never an
// unauthenticated, client-supplied header like this one -- see
// org.SubjectResolver's own doc comment for the same rule stated as a hard
// requirement.
func DemoOrgSubjectResolverFor(headerDisabled, principalFallback bool) func(*http.Request) (string, bool) {
	return func(req *http.Request) (string, bool) {
		if !headerDisabled {
			if userID := req.Header.Get(DemoOrgUserHeader); userID != "" {
				return userID, true
			}
			if principalFallback {
				if principal, ok := authn.PrincipalFromContext(req.Context()); ok && principal.UserID != "" {
					return principal.UserID, true
				}
			}
			return "", false
		}
		principal, ok := authn.PrincipalFromContext(req.Context())
		if !ok || principal.UserID == "" {
			return "", false
		}
		return principal.UserID, true
	}
}

// compile-time checks that the closure DemoOrgSubjectResolverFor returns
// satisfies the identical SubjectResolver seam the three modules it serves
// declare -- wrapped by each module's own func adapter, exactly as the
// wiring sites below pass it.
var (
	_ org.SubjectResolver          = org.SubjectResolverFunc(DemoOrgSubjectResolverFor(false, false))
	_ notification.SubjectResolver = notification.SubjectResolverFunc(DemoOrgSubjectResolverFor(false, false))
	_ integration.SubjectResolver  = integration.SubjectResolverFunc(DemoOrgSubjectResolverFor(false, false))
)

// DemoNotesSubjectResolver is what notes' create handler resolves the
// creating user from -- the notes.NewModule option BuildServer wires
// below. It is DemoOrgSubjectResolverFor's behavior plus one source: like
// its sibling it reads the X-Demo-User-Id header first, the attribution
// affordance every flow helper in this package sends and the namespace
// demo_notification.go's address table keys on; and only when no header
// is present does it fall back to the verified Principal authn.Middleware
// left in the request context. The fallback is what lets a browser-shaped
// caller with no demo header create notes: the accounts demo_users.go
// seeds (real users acting through their access tokens) are attributed
// through it, exactly as they pass the rbac gate through DemoSubjectResolver's
// own fallback, and the note-created events their creates publish name
// their real user ids -- which resolve to no notification addresses, an
// ordinary skip (see demo_notification.go's DemoUserAddresses).
//
// Notes' creator seam is not alone in having this second source:
// org's caller-scoped endpoints share it -- the org module is wired with
// DemoOrgSubjectResolverFor's principalFallback true, giving org the
// identical header-then-Principal shape (see DemoOrgSubjectResolverFor's own
// doc comment). The notification module's caller-scoped endpoints keep the
// header-only read, because its subject-less refusal is a pinned behaviour
// of this app's rig -- and a request that reaches those surfaces without
// the header stays refused rather than acting as the principal's user id.
//
// This is a placeholder, not a pattern to copy into a real deployment:
// the header is exactly as unverifiable here as in DemoOrgSubjectResolverFor,
// and a real deployment's resolver reads the creating user from the
// verified token the notes create handler already stands behind.
//
// headerDisabled carries the value of cfg.DisableDemoUserHeader
// (APP_DISABLE_DEMO_USER_HEADER) BuildServer wired this resolver with --
// the sibling of DemoOrgSubjectResolverFor's own headerDisabled argument, and
// what extends the kill switch to this resolver: without it, a caller
// could still name any creator through X-Demo-User-Id on notes' and
// cases' surfaces while the rbac header alone was disabled. The
// zero value keeps the header-then-Principal resolver;
// with headerDisabled set, Subject skips the header read entirely
// and resolves the caller from the verified authn Principal alone, failing
// closed exactly like the default shape when no Principal exists.
type DemoNotesSubjectResolver struct {
	// HeaderDisabled carries cfg.DisableDemoUserHeader
	// (APP_DISABLE_DEMO_USER_HEADER): when true, Subject skips the
	// header read and resolves the verified authn Principal alone,
	// failing closed without one -- see the type's own doc comment for
	// the full contract. The zero value keeps the header-first resolver.
	HeaderDisabled bool
}

// Subject implements notes.SubjectResolver (and the cases package's
// identical copy of the seam -- see the compile-time check at the bottom
// of internal/app/cases.go). It fails closed: no header (and, in the
// headerDisabled wiring, no verified Principal) reports ("", false), and
// the module's own per-operation refusal (notes.subject_unresolved,
// cases.subject_unresolved) is what a caller then sees.
func (r DemoNotesSubjectResolver) Subject(req *http.Request) (string, bool) {
	userID := ""
	if !r.HeaderDisabled {
		userID = req.Header.Get(DemoOrgUserHeader)
	}
	if userID == "" {
		principal, ok := authn.PrincipalFromContext(req.Context())
		if !ok || principal.UserID == "" {
			return "", false
		}
		userID = principal.UserID
	}
	return userID, true
}

// compile-time check that DemoNotesSubjectResolver satisfies notes' own
// copy of the seam.
var _ notes.SubjectResolver = DemoNotesSubjectResolver{}
