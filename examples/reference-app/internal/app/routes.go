// This file composes the app's HTTP face: the host's hand-written routes
// mounted on the mux the engine prepared, then the platform middleware chain
// derived from the registry -- every module route admitted through the
// app's route-authorization table, authn's subtree and admin's subtree
// split onto their own branches, everything else wrapped in the tenancy
// chain.

package app

import (
	"net/http"

	"github.com/vislake/speed/go/admin"
	speedchain "github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/cases"
	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// composeFace is the engine's protected-face callback: it mounts this app's
// hand-written routes on the mux (which already carries the platform
// liveness routes and the engine's ExtraRoutes), then derives the whole
// middleware chain from the registry with chain.Standard -- the route
// partition and the fixed order live there, not here.
//
// The hand-written routes are mounted FIRST and directly, exactly as a
// module's own routes would be once Standard mounts them: every one of them
// sits behind the tenancy chain (Standard wraps the mux), and each carries
// its own gate -- the route-authorization table below covers the registry's
// mounted routes only, so a hand-written route that needs a permission
// check applies it itself (team_members.go is the standing example), while
// the demo notification and integration surfaces resolve identity per
// operation.
func (b *serverBuild) composeFace(mux *http.ServeMux) (http.Handler, error) {
	// WireDemoNotification adds the reference app's demo glue on top of the
	// mounted module routes: the subscription that turns notes' note-created
	// event into a notification dispatch for the note's creator, and the
	// hand-written demo patient-message route that dispatches the demo
	// module's patient-reminder type to a verified external contact. The
	// bus is reg.EventBus() -- the same bus Bootstrap gave every module --
	// and the notificationModule services are the module's own accessors,
	// the same instances its Register validated and its HTTP handler drives.
	// The call cannot fail: nothing it does returns an error.
	demo.WireDemoNotification(mux, b.reg.EventBus(), b.notificationModule, b.reg, b.authnUserLocales)

	// wireConsult mounts go/ai-gateway's mandatory-first-consumer route
	// (consult.go): consultService shares notesModule's own database
	// connection through a fresh notes.Repository -- no new infrastructure
	// dependency is needed for this app to have a real consult surface --
	// and asks aiGatewayModule's own Gateway, the same instance
	// aiGatewayModule.Register validated.
	consultService := consult.NewService(notes.NewRepository(b.db), b.aiGatewayModule.Gateway())
	wireConsult(mux, consultService)

	// wireSmileSim mounts the image-generation half of go/ai-gateway's
	// mandatory-first-consumer routes (smilesim.go): smileSimService asks
	// aiGatewayModule's own Gateway to run an async smile simulation over a
	// patient photo already uploaded through storageModule's own HTTP
	// surface, and the job-status route polls the same standaloneQueue every
	// other async task in this app shares. billingModule.Credits() is the
	// same *billing.CreditService instance SeedDemoCredits granted the demo
	// tenants' starting balance against: smilesim.Service reserves
	// smilesim.CreditsPerSimulation credits from it before ever calling
	// Gateway.GenerateImage, and settles that reservation (Confirm/Refund)
	// once the async job reaches a terminal status. memberships rides along
	// as the recipient gate's membership answer -- the SAME store authn's
	// MembershipReader reads, attached to org in postBootstrap.
	// storageModule.ObjectService() is the simulation-content route's read
	// path for a generated image's stored bytes, the same instance the
	// cases photo routes drive.
	wireSmileSim(mux, b.smileSimService, b.standaloneQueue, b.memberships, b.storageModule.ObjectService(), b.attestationService)

	// wireSmilesimTerminalSignal subscribes smileSimService to the queue's
	// terminal signal (jobs.job.terminal): a simulation's credit
	// reservation then settles the moment its job reaches a terminal
	// status -- no client poll of the job-status route, no
	// reconciliation-sweep interval -- and a named recipient's completion
	// notification publishes then too. The publisher half of the mechanism
	// is jobs.WithEventBus(bus) on the standaloneQueue construction; the
	// install's full contract is smilesim_terminal.go's doc comment.
	wireSmilesimTerminalSignal(b.reg, b.smileSimService)

	// WireClinicName mounts this host's own tenant-identity answer
	// (clinic_name.go): the org root name of the tenant the caller's token
	// is scoped to, the name the web renders for a clinic that did not
	// exist at boot where the demo roster's static copy has no entry.
	WireClinicName(mux, b.orgModule.Tree())

	// wireTeamMembers mounts this host's own roster-with-identity answer
	// (team_members.go): the tenant's org membership roster, each row
	// enriched with the member's display identity from authn's users table
	// -- org's member rows carry opaque user ids only by its own
	// module-boundary rule. Gated on the org read permission through the
	// same rbac gate the org module route uses.
	wireTeamMembers(mux, teamMembersDeps{
		az:             b.rbacService,
		members:        b.orgModule.Members(),
		tree:           b.orgModule.Tree(),
		users:          b.authnModule.Service().Users(),
		headerDisabled: b.cfg.DisableDemoUserHeader,
	})

	// wireCasesRoutes mounts the case domain (internal/cases, mounted in
	// cases.go): the tenant-scoped Case records a web UI renders from. The
	// photo-upload and photo-content routes drive storageModule's own
	// ObjectService -- the same instance the storage module's HTTP surface
	// serves -- so the app's case surface and the module agree on what an
	// object is and which tenant's rows each read.
	wireCasesRoutes(mux, cases.NewService(b.caseRepository), demo.DemoNotesSubjectResolver{HeaderDisabled: b.cfg.DisableDemoUserHeader}, b.storageModule.ObjectService())

	// wireIntegrationAuthenticated mounts go/integration's
	// mandatory-first-consumer route (integration_authenticate.go): a
	// minimal "whoami" demo endpoint gated by the module's own
	// AuthMiddleware, with the guard wired ahead of it through
	// integration.WithAuthenticationGuard, applied inside
	// wireIntegrationAuthenticated once reg.KVStore() exists, since the
	// guard's limiter is built over this same resolved KVStore seam.
	wireIntegrationAuthenticated(mux, b.integrationModule, b.reg.KVStore())

	// The middleware chain: chain.Standard owns the fixed order --
	// authn.Middleware(verifier) outermost (the only order that verifies a
	// token exactly once), the impersonation decorator, then tenancy with
	// the pre-auth allowlist -- and splits the two structurally exempt
	// branches out of the mounted route set: admin's own route, excluded
	// from ordinary tenant resolution and identity substitution, and
	// authn's subtree, split by authn.ExemptSubtree. This call supplies the
	// host-specific half: the route-authorization table (business rules),
	// the admin module's mount prefix, the impersonation decorator built
	// from admin's own middleware, admin's tenant status gate, and the
	// three pre-auth entries a public route of this app needs.
	//
	// Session revocation rides on the verifier with no extra option: this
	// app selects immediate revocation in authnOpts
	// (WithRevocationMode(RevocationModeImmediate)), the Service attaches
	// its SessionManager as the revocation source of the verifier
	// Service().Verifier() hands out, and authn.Middleware consults that
	// source on every request whose token verifies -- so the verifier
	// handed to Standard here is exactly the enforced composition.
	orgGuardDeps := demo.OrgRouteGuardDeps{Scope: b.orgModule.Scope(), Members: b.orgModule.Members()}
	return speedchain.Standard(
		b.reg,
		b.authnModule.Service().Verifier(),
		mux,
		speedchain.WithAuthorization(b.rbacService, demo.DemoRouteRules(b.rbacService, orgGuardDeps, b.cfg.DisableDemoUserHeader)),
		speedchain.WithAdminPrefix(admin.APIPath),
		// admin.ImpersonationMiddleware is this host's own decorator for
		// the chain's impersonation seat: only this app has an admin module
		// to build one from.
		speedchain.WithImpersonation(admin.ImpersonationMiddleware(b.adminModule.Impersonation())),
		// tenancy.WithTenantStatusResolver is the suspension enforcement
		// seam (go/tenancy/tenant_status.go): admin's own tenant ledger
		// (*admin.TenantService) implements tenancy.TenantStatusResolver
		// structurally -- admin is its one real implementer, but the
		// interface itself does not know admin exists. This is what turns
		// "an operator marked a tenant suspended in admin's console" into
		// every OTHER route (notes, storage, org, ...) actually refusing
		// that tenant's requests on the very next one, rather than being a
		// ledger fact nothing downstream ever consults.
		speedchain.WithTenantStatusResolver(b.adminModule.Tenants()),
		speedchain.WithExtraAllowlist(
			// sharing.PathAccess is the one genuinely public,
			// unauthenticated route this app mounts: an anonymous visitor
			// holding a bearer share token carries no Principal and
			// therefore no tenant claim at all, by design -- sharing's own
			// handler resolves the tenant itself, from the token alone,
			// once this allowlist entry lets the request reach it at all.
			// GET only: the fragment defines no other method on this path.
			tenancy.WithAllowlist(http.MethodGet, sharing.PathAccess),
			// IntegrationWhoamiPath is the identical shape, one layer
			// removed: its caller carries an API key, not a share token,
			// and like sharing.PathAccess it resolves ITS OWN tenant --
			// integration.AuthMiddleware, mounted ahead of the tenancy
			// chain reaching this route, via Service.Authenticate.
			tenancy.WithAllowlist(http.MethodGet, IntegrationWhoamiPath),
			// OrgAcceptPath (org_acceptInvitation) is the one org route
			// this app lets through tenant resolution: the caller an
			// invitation exists for holds no membership in, and typically
			// no bearer token for, the inviting tenant, so their request
			// carries no tenant claim for tenancy.Middleware to resolve.
			// org's accept handler resolves the tenant itself, server-side,
			// from the invitation token, once this allowlist entry lets the
			// request reach it at all. POST only: the fragment defines no
			// other method on this path. Unlike sharing.PathAccess this is
			// NOT an anonymous surface: org's own per-operation
			// SubjectResolver check still refuses an unidentifiable
			// acceptor, and authn.Middleware still 401s a genuinely invalid
			// bearer.
			tenancy.WithAllowlist(http.MethodPost, demo.OrgAcceptPath),
		),
	)
}
