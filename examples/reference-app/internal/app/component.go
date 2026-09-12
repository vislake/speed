package app

// This file carries the reference app's own components: the link-policy
// component (the host's half of the HTTP face, consumed by the http
// component go/app/httpserve) plus the host's route and subscription
// wiring, and the step components wrapping the assembly steps attach.go and
// serve.go implement. A step component's callback binds the assembled
// values its body reads and then runs that body, so a step has one
// implementation.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vislake/speed/go/admin"
	"github.com/vislake/speed/go/app/httpserve"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/spa"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/tenancy"

	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/cases"
	"github.com/vislake/speed/examples/reference-app/internal/consult"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// runtimeServicesSeat is the post-bootstrap step's product token. It carries
// no data -- the step publishes the runtime services (config's schema-bearing
// service, rbac's catalog-bearing service) through the by-type context -- and
// exists so the plan orders every consumer of those services after the step:
// the link-policy component's Init fill and the pre-serve step's seed drive
// both read what the step published, and a requirement edge is what makes
// that order structural instead of a function of the composition's seed
// order (which other edges can move).
type runtimeServicesSeat struct{}

// hostStep is a step component's product: New must return one non-nil value
// and a step owns no resource of its own -- what it works on is the
// serverBuild and the view it derives from the registry.
type hostStep struct{}

// newHostStep produces the marker a step component's New returns.
func newHostStep(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
	return &hostStep{}, nil
}

// linkPolicyComponent returns the host's link-policy component: the one
// component that delivers the host's half of the HTTP face -- the verifier,
// the authorization table, the admin prefix, the impersonation decorator,
// the tenant-status gate, the extra pre-auth entries, the host's own
// hand-written routes and the SPA outer wrapper -- which the http component
// (go/app/httpserve) consumes as its required dependency and applies when
// it assembles the handler in the Serve stage.
//
// Its product is a *httpserve.LinkPolicy, delivered empty at construction:
// that delivery is what resolves the http component's required token and
// orders the two components structurally. The CONTENT is filled by the
// pre-serve step's Init turn (composeLinkPolicy), not here, because the fill
// reads runtime services the post-bootstrap step publishes and must
// therefore run after every module's Init callback -- a position the
// post-bootstrap step's own dependency is the one that owns, and which this
// component's earlier, module-pulled plan position cannot provide. One
// *LinkPolicy value travels the whole way: constructed here, filled there,
// read by the http component in its Serve stage.
func linkPolicyComponent(b *serverBuild) pkgcore.Component {
	return pkgcore.Component{
		Name:         "reference-app.app",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*httpserve.LinkPolicy)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &httpserve.LinkPolicy{}, nil
		},
	}
}

// composeLinkPolicy fills the host's link policy and installs the host's
// Init-stage subscriptions, in the order the pieces require: the
// subscriptions first (they read the assembled runtime services), then the
// policy's chain half and the host's own routes.
func (b *serverBuild) composeLinkPolicy(ctx context.Context, view assemblyView, policy *httpserve.LinkPolicy) error {
	// The two subscriptions the host installs during Init, exactly where the
	// old face composition installed them: the demo notification glue's
	// note-created subscriber (on the bus itself) and the smilesim terminal
	// signal (on the events seat). No subscription may land after a Start.
	demo.SubscribeDemoNotifications(view.bus, b.notificationModule, b.authnUserLocales)
	wireSmilesimTerminalSignal(view.events, b.smileSimService)

	orgGuardDeps := demo.OrgRouteGuardDeps{Scope: b.orgModule.Scope(), Members: b.orgModule.Members()}
	*policy = httpserve.LinkPolicy{
		Verifier:      b.authnModule.Service().Verifier(),
		Authorizer:    b.rbacService,
		RouteRules:    demo.DemoRouteRules(b.rbacService, orgGuardDeps, b.cfg.DisableDemoUserHeader),
		AdminPrefix:   admin.APIPath,
		Impersonation: admin.ImpersonationMiddleware(b.adminModule.Impersonation()),
		// tenancy.WithTenantStatusResolver is the suspension enforcement
		// seam (go/tenancy/tenant_status.go): admin's own tenant ledger
		// (*admin.TenantService) implements tenancy.TenantStatusResolver
		// structurally -- admin is its one real implementer, but the
		// interface itself does not know admin exists. This is what turns
		// "an operator marked a tenant suspended in admin's console" into
		// every OTHER route (notes, storage, org, ...) actually refusing
		// that tenant's requests on the very next one, rather than being a
		// ledger fact nothing downstream ever consults.
		TenantStatusResolver: b.adminModule.Tenants(),
		ExtraAllowlist: []tenancy.MiddlewareOption{
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
		},
		MountHostRoutes: func(mux *http.ServeMux) { b.mountHostRoutes(mux, view) },
	}
	if b.cfg.WebDistDir != "" {
		// The SPA file server is the host's outer wrapper: it stands
		// outside everything the component assembled -- the platform
		// middleware, the fixed chain and the host's own routes -- so a
		// path the frontend serves itself (a built asset, the client-route
		// index fallback) never traverses the chain's layers. The boundary
		// is pinned by TestWebDistForm_SeatLayerStaysInsideTheSPA.
		policy.OuterWrapper = func(next http.Handler) http.Handler {
			return spa.New(b.cfg.WebDistDir, next, hostSPAOptions()...)
		}
	}
	return nil
}

// mountHostRoutes mounts the host's own hand-written routes on the mux the
// http component prepared (which already carries the platform liveness
// routes): every one of them sits behind the tenancy chain exactly like a
// module route, and each carries its own gate -- the route-authorization
// table covers the registry's mounted routes only, so a hand-written route
// that needs a permission check applies it itself (team_members.go is the
// standing example), while the demo notification and integration surfaces
// resolve identity per operation. kv is the assembled key-value store the
// integration whoami route's guard limiter is built over.
func (b *serverBuild) mountHostRoutes(mux *http.ServeMux, view assemblyView) {
	// The demo notification glue's route half: the demo patient-message
	// route, which dispatches the demo module's patient-reminder type to a
	// verified external contact of the caller's tenant. (Its subscription
	// half landed during Init; see composeLinkPolicy.)
	demo.MountDemoNotificationRoutes(mux, b.notificationModule, view.catalog)

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
	// MembershipReader reads, attached to org in the post-bootstrap step.
	// storageModule.ObjectService() is the simulation-content route's read
	// path for a generated image's stored bytes, the same instance the
	// cases photo routes drive.
	wireSmileSim(mux, b.smileSimService, b.standaloneQueue, b.memberships, b.storageModule.ObjectService(), b.attestationService)

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
	// wireIntegrationAuthenticated once the kv store exists, since the
	// guard's limiter is built over this same resolved KVStore seam.
	wireIntegrationAuthenticated(mux, b.integrationModule, view.kv)
}

// stepInit returns the Init callback the host's step components share: bind
// the assembled values the step bodies read, derive the assembly view from
// the registry the callback receives, and run the step's body over it. One
// adapter serves every step, so no two steps can drift into different view
// wiring.
func (b *serverBuild) stepInit(run func(ctx context.Context, view assemblyView) error) func(context.Context, *pkgcore.ComponentRegistry, any) error {
	return func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
		if err := b.bindRegistry(reg); err != nil {
			return err
		}
		if err := b.bindRuntimeServices(reg); err != nil {
			return err
		}
		view, err := b.viewFromComponents(reg)
		if err != nil {
			return err
		}
		return run(ctx, view)
	}
}

// seedHandler mounts the route face's accumulated routes on a mux of its
// own: the handler the demo seeds drive, chosen deliberately over the
// composed face -- which does not exist yet in this Init turn -- because
// the seeds' substantive requirements all live inside the modules' own
// handlers (the real register route, its password policy and its rate
// limiter), while the chain layers the composed face adds are pass-through
// for an unauthenticated pre-auth register POST.
func seedHandler(registrar pkgcore.RouteRegistrar) http.Handler {
	mux := http.NewServeMux()
	pkgcore.MountRoutes(mux, registrar.Routes()...)
	return mux
}

// postBootstrapComponent returns the post-bootstrap step as a component: its
// Init binds the assembled values the step reads and runs the
// runPostBootstrap body, which publishes the two runtime services the later
// steps bind (bindRuntimeServices).
func (b *serverBuild) postBootstrapComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         "reference-app.post_bootstrap",
		Capabilities: pkgcore.MultiReplicaSafe,
		Provides:     []any{(*runtimeServicesSeat)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &runtimeServicesSeat{}, nil
		},
		Init: func(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			if err := b.bindRegistry(reg); err != nil {
				return err
			}
			view, err := b.viewFromComponents(reg)
			if err != nil {
				return err
			}
			// The merged catalog is published into the by-type context
			// here: rendering a message (org's invitation mail, a
			// notification template, a verification code) reads it through
			// the registry, and this step runs before anything a request --
			// or the demo seeds below -- can render.
			reg.Put(view.catalog)
			return b.runPostBootstrap(ctx, reg, view)
		},
	}
}

// postAttachComponent returns the post-attach step as a component: its Init
// binds the assembled values the step reads and runs the runPostAttach body.
func (b *serverBuild) postAttachComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         "reference-app.post_attach",
		Capabilities: pkgcore.MultiReplicaSafe,
		New:          newHostStep,
		Init:         b.stepInit(b.runPostAttach),
	}
}

// preServeComponent returns the pre-serve step as a component: the last
// host step whose Init turn runs before the listener opens. It fills the
// host's link policy, installs the host's Init-stage subscriptions (no
// subscription may land after a Start), and then runs the runPreServe body
// (the demo seeds over the seed handler, then the self-service chain). Its
// two requirements are what make the turn's position structural: the
// runtime-services edge orders it after the post-bootstrap step, and the
// route-face edge orders it after the http component -- so the route table
// the seeds drive is complete and the self-service subscription lands after
// the seeds' registrations, the discriminator that keeps the demo path
// byte-identical.
func (b *serverBuild) preServeComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         "reference-app.pre_serve",
		Capabilities: pkgcore.MultiReplicaSafe,
		Requires: []pkgcore.Requirement{
			{Token: (*runtimeServicesSeat)(nil)},
			{Token: (*pkgcore.RouteRegistrar)(nil)},
		},
		New:  newHostStep,
		Init: b.preServeStep,
	}
}

// preServeStep is the pre-serve step's Init callback, in the one order the
// pieces require: bind the assembled values, fill the policy and install
// the subscriptions (composeLinkPolicy), then run the seeds.
func (b *serverBuild) preServeStep(ctx context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
	registrar, err := pkgcore.Get[pkgcore.RouteRegistrar](reg)
	if err != nil {
		return fmt.Errorf("reference-app: read the route face: %w", err)
	}
	policy, err := pkgcore.Get[*httpserve.LinkPolicy](reg)
	if err != nil {
		return fmt.Errorf("reference-app: read the host link policy: %w", err)
	}
	if err := b.bindRegistry(reg); err != nil {
		return err
	}
	if err := b.bindRuntimeServices(reg); err != nil {
		return err
	}
	view, err := b.viewFromComponents(reg)
	if err != nil {
		return err
	}
	if err := b.composeLinkPolicy(ctx, view, policy); err != nil {
		return err
	}
	return b.runPreServe(ctx, view, seedHandler(registrar))
}

// hostComponents returns the host's own components in the registration order
// the assembly plans by: the override and provider components first (their
// seams are what the modules' descriptors read while constructing), then the
// post-bootstrap step (it runs after every module's Init turn has published
// what it reads), the post-attach step, the link-policy component, the
// pre-serve step -- ordered after the http component by its route-face
// requirement, not by registration alone -- and the worker last.
// Independent components take their plan order from the registration order
// (the assembly's own tie rule), so this order is the plan order; the module
// components' own descriptors are the composition's business, not this
// list's.
func (b *serverBuild) hostComponents(reg *pkgcore.ComponentRegistry) ([]pkgcore.Component, error) {
	wiring, err := b.hostWiringComponents(reg)
	if err != nil {
		return nil, err
	}
	return append(wiring, b.hostStepComponents()...), nil
}

// hostStepComponents returns the host's assembly-step components alone, in
// the registration order the assembly plans by (see hostComponents for the
// ordering rationale).
func (b *serverBuild) hostStepComponents() []pkgcore.Component {
	components := []pkgcore.Component{
		b.postBootstrapComponent(),
		b.postAttachComponent(),
		linkPolicyComponent(b),
		b.preServeComponent(),
		b.workerComponent(),
	}
	return components
}
