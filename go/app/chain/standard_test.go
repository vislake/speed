package chain

import (
	"context"
	"embed"
	"net/http"
	"testing"

	"github.com/vislake/speed/go/app"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// routesModule is a pkgcore.Module that mounts exactly the routes a test
// hands it, so a Standard test composes a real registry without any business
// module.
type routesModule struct {
	name   string
	routes []pkgcore.MountedRoute
}

func (m *routesModule) Name() string         { return m.name }
func (m *routesModule) DependsOn() []string  { return nil }
func (m *routesModule) Migrations() embed.FS { return embed.FS{} }
func (m *routesModule) Locales() embed.FS    { return embed.FS{} }
func (m *routesModule) OpenAPISpec() []byte  { return nil }
func (m *routesModule) Register(reg pkgcore.Registrar) error {
	for _, route := range m.routes {
		reg.RoutesSeat().Mount(route.Path, route.Handler)
	}
	return nil
}

var _ pkgcore.Module = (*routesModule)(nil)

// testRegistry bootstraps a kernel over the modules, returning the registry
// Standard derives its partition from.
func testRegistry(t *testing.T, modules ...pkgcore.Module) *pkgcore.Registry {
	t.Helper()
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), modules...)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return reg
}

// countingHandler records how often it answered.
type countingHandler struct{ hits int }

func (h *countingHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.hits++
	w.WriteHeader(http.StatusOK)
}

// stubAuthorizer is an rbac.Authorizer stub: it answers with its configured
// verdict for every check and records the calls, so a test can pin that the
// gate ran without any permission machinery. The grant-management and
// scope methods are inert -- no chain test exercises them.
type stubAuthorizer struct {
	calls   int
	allowed bool
}

func (a *stubAuthorizer) Can(context.Context, rbac.Subject, string, string) (bool, error) {
	a.calls++
	return a.allowed, nil
}

func (a *stubAuthorizer) ListPermissions(context.Context, rbac.Subject) ([]string, error) {
	return nil, nil
}

func (a *stubAuthorizer) AssignRole(context.Context, rbac.Subject, string, rbac.Scope) error {
	return nil
}

func (a *stubAuthorizer) RevokeRole(context.Context, rbac.Subject, string, rbac.Scope) error {
	return nil
}

func (a *stubAuthorizer) DataScope(context.Context, rbac.Subject, string, string) (rbac.DataScope, error) {
	return rbac.DataScope{}, nil
}

// fixedPermission returns a route-permission selector answering one fixed
// permission for every request.
func fixedPermission(permission string) pkgcore.RoutePermission {
	return func(*http.Request) string { return permission }
}

// staticSubject is the resolver a rule uses to hand the gate a complete
// Subject without any authenticating side, so the weave's allowed and denied
// halves can both be exercised.
func staticSubject(*http.Request) (rbac.Subject, bool) {
	return rbac.Subject{TenantID: "tenant-a", UserID: "user-a"}, true
}

// standardFixture is the composed fixture every partition test drives.
type standardFixture struct {
	handler   http.Handler
	authn     *countingHandler
	admin     *countingHandler
	notes     *countingHandler
	siblings  *countingHandler
	protected *countingHandler
	az        *stubAuthorizer
}

// newStandardFixture composes Standard over a registry mounting an authn
// route, an admin route, a gated notes route and a public route whose path
// shares the admin prefix's textual start (the boundary case).
func newStandardFixture(t *testing.T, mutate func(*[]Option, *stubAuthorizer)) *standardFixture {
	t.Helper()
	f := &standardFixture{
		authn:     &countingHandler{},
		admin:     &countingHandler{},
		notes:     &countingHandler{},
		siblings:  &countingHandler{},
		protected: &countingHandler{},
		az:        &stubAuthorizer{allowed: true},
	}
	reg := testRegistry(t, &routesModule{name: "fixture", routes: []pkgcore.MountedRoute{
		{Path: app.AuthnAPIPath, Handler: f.authn},
		{Path: "/api/v1/admin", Handler: f.admin},
		{Path: "/api/v1/notes", Handler: f.notes},
		{Path: "/api/v1/administrators", Handler: f.siblings},
	}})

	protected := http.NewServeMux()
	protected.Handle("/", f.protected)
	protected.HandleFunc(obs.HealthzPath, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	opts := []Option{
		WithAuthorization(f.az, []rbac.RouteRule{
			{Path: app.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}},
			{Path: "/api/v1/admin", Access: pkgcore.RouteAccess{Permission: fixedPermission("admin:access")}, SubjectResolver: staticSubject},
			{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Permission: fixedPermission("notes:read")}, SubjectResolver: staticSubject},
			{Path: "/api/v1/administrators", Access: pkgcore.RouteAccess{Public: true}},
		}),
		WithAdminPrefix("/api/v1/admin"),
		// The two protected-face routes are allowlisted so a request with
		// no Principal passes the tenancy chain and reaches the mux --
		// the allowlist exempts tenant resolution only, never the route's
		// own gate, which is what the weave tests then exercise.
		WithExtraAllowlist(
			tenancy.WithAllowlist(http.MethodGet, "/api/v1/administrators"),
			tenancy.WithAllowlist(http.MethodGet, "/api/v1/notes"),
		),
	}
	if mutate != nil {
		mutate(&opts, f.az)
	}

	handler, err := Standard(reg, newTestVerifier(t), protected, opts...)
	if err != nil {
		t.Fatalf("Standard: %v", err)
	}
	f.handler = handler
	return f
}

// TestStandard_PartitionsTheMountedRoutes pins the derivation's three
// destinations: authn's subtree and the admin prefix are dispatched ahead of
// the tenancy chain on their own branches, and every other route is mounted
// on the protected face.
func TestStandard_PartitionsTheMountedRoutes(t *testing.T) {
	f := newStandardFixture(t, nil)

	if rec := do(f.handler, http.MethodGet, app.AuthnAPIPath+"/register", nil); f.authn.hits != 1 {
		t.Fatalf("GET %s/register: status %d, authn hits %d; the authn subtree must be dispatched ahead of the tenancy chain", app.AuthnAPIPath, rec.Code, f.authn.hits)
	}
	if rec := do(f.handler, http.MethodGet, "/api/v1/admin/tenants", nil); f.admin.hits != 1 {
		t.Fatalf("GET /api/v1/admin/tenants: status %d, admin hits %d; the admin prefix must be dispatched ahead of tenancy and impersonation", rec.Code, f.admin.hits)
	}
	if rec := do(f.handler, http.MethodGet, "/api/v1/administrators", nil); f.siblings.hits != 1 {
		t.Fatalf("GET /api/v1/administrators: status %d, hits %d; a path sharing the admin prefix's textual start is not below it and must stay on the protected face", rec.Code, f.siblings.hits)
	}
	if rec := do(f.handler, http.MethodGet, "/api/v1/notes", nil); f.notes.hits != 1 {
		t.Fatalf("GET /api/v1/notes: status %d, notes hits %d; a non-exempt route belongs on the protected face", rec.Code, f.notes.hits)
	}
	if f.protected.hits != 0 {
		t.Fatalf("the protected fallback ran %d times; every fixture path is served by a mounted route", f.protected.hits)
	}
}

// routesProbe is the trivial product of the component-registry fixture.
type routesProbe struct{}

// TestStandard_AcceptsAComponentRegistry pins the route source's second
// shape: the component assembly's registry answers the same route reading as
// the module registry, so a host whose assembly produced that registry
// composes the same chain from the same declarations.
func TestStandard_AcceptsAComponentRegistry(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	authn := &countingHandler{}
	err := reg.Register(pkgcore.Component{
		Name: "fixture.routes",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &routesProbe{}, nil
		},
		Init: func(_ context.Context, r *pkgcore.ComponentRegistry, _ any) error {
			r.Routes.Mount(app.AuthnAPIPath, authn)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register the fixture component: %v", err)
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"fixture.routes": nil},
	}))

	ctx := context.Background()
	if err = reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err = reg.Construct(ctx); err != nil {
		t.Fatalf("Construct: %v", err)
	}
	if err = reg.Verify(ctx); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if err = reg.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	handler, err := Standard(reg, newTestVerifier(t), http.NewServeMux())
	if err != nil {
		t.Fatalf("Standard over a component registry: %v", err)
	}
	if rec := do(handler, http.MethodGet, app.AuthnAPIPath+"/register", nil); authn.hits != 1 {
		t.Fatalf("GET %s/register: status %d, authn hits %d; the component registry's routes must partition like the module registry's", app.AuthnAPIPath, rec.Code, authn.hits)
	}
}

// TestStandard_WeavesTheAuthorizationTable pins the rbac weave: a gated
// route's handler runs only when the authorizer admits the request, and the
// refusal is rbac's own fail-closed 403 -- so the gate demonstrably sits
// between the mux and the module handler.
func TestStandard_WeavesTheAuthorizationTable(t *testing.T) {
	f := newStandardFixture(t, func(_ *[]Option, az *stubAuthorizer) { az.allowed = false })

	rec := do(f.handler, http.MethodGet, "/api/v1/notes", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/v1/notes with a denying authorizer: status %d, want %d", rec.Code, http.StatusForbidden)
	}
	if f.notes.hits != 0 {
		t.Fatalf("the gated handler ran %d times under a denying authorizer", f.notes.hits)
	}
	if f.az.calls == 0 {
		t.Fatal("the authorizer was never consulted; the route table's gate is not woven into the mounted route")
	}

	admitted := newStandardFixture(t, nil)
	if rec := do(admitted.handler, http.MethodGet, "/api/v1/notes", nil); admitted.notes.hits != 1 {
		t.Fatalf("GET /api/v1/notes with an admitting authorizer: status %d, hits %d; the guard must pass an admitted request through to the handler", rec.Code, admitted.notes.hits)
	}
}

// TestStandard_PublicRuleRunsNoCheck pins the public half of the table: a
// route declared public reaches its handler without a permission check at
// all (it still needs the pre-auth allowlist to pass the tenancy chain when
// the request carries no Principal).
func TestStandard_PublicRuleRunsNoCheck(t *testing.T) {
	f := newStandardFixture(t, nil)

	if rec := do(f.handler, http.MethodGet, "/api/v1/administrators", nil); f.siblings.hits != 1 {
		t.Fatalf("GET /api/v1/administrators: status %d, hits %d", rec.Code, f.siblings.hits)
	}
	if f.az.calls != 0 {
		t.Fatalf("the authorizer was consulted %d times for public routes only", f.az.calls)
	}
}

// TestStandard_ImpersonationDecoratorSitsBehindTheExemptBranches pins the
// decorator's position in the derived chain: every protected-half request
// passes through it, no authn or admin request does.
func TestStandard_ImpersonationDecoratorSitsBehindTheExemptBranches(t *testing.T) {
	calls := 0
	marker := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			next.ServeHTTP(w, r)
		})
	}
	f := newStandardFixture(t, func(opts *[]Option, _ *stubAuthorizer) {
		*opts = append(*opts, WithImpersonation(marker))
	})

	if rec := do(f.handler, http.MethodGet, obs.HealthzPath, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d, want %d", obs.HealthzPath, rec.Code, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("the impersonation decorator ran %d times for one protected-half request, want 1", calls)
	}

	do(f.handler, http.MethodGet, app.AuthnAPIPath+"/register", nil)
	do(f.handler, http.MethodGet, "/api/v1/admin/tenants", nil)
	if calls != 1 {
		t.Fatalf("the impersonation decorator ran %d times after requests to the exempt branches", calls)
	}
}

// TestWithTenantStatusResolver_InstallsTheResolver pins the option's wiring:
// the resolver lands in the chain configuration the tenancy middleware is
// built from. What a wired resolver then DOES -- consulted for every
// successfully resolved request, failing closed on anything but active -- is
// tenancy.Middleware's own contract (pinned by go/tenancy's suite), and this
// fixture's no-key verifier resolves no tenant for any request, so the gate's
// request-level calls stay unobservable here.
func TestWithTenantStatusResolver_InstallsTheResolver(t *testing.T) {
	resolver := &activeStatusResolver{}
	var cfg standardConfig
	WithTenantStatusResolver(resolver)(&cfg)
	if cfg.tenantStatusResolver != resolver {
		t.Fatal("WithTenantStatusResolver did not install the resolver into the chain configuration")
	}
}

// TestStandard_RejectsMissingRequiredPieces pins the refusals: a nil
// registry or protected mux, a half-declared authorization pair, an admin
// prefix nothing mounts, and a registry with no authn route.
func TestStandard_RejectsMissingRequiredPieces(t *testing.T) {
	verifier := newTestVerifier(t)

	t.Run("nil registry", func(t *testing.T) {
		if _, err := Standard(nil, verifier, http.NewServeMux()); err == nil {
			t.Fatal("Standard accepted a nil registry")
		}
	})
	t.Run("nil protected", func(t *testing.T) {
		reg := testRegistry(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(reg, verifier, nil, WithAdminPrefix("")); err == nil {
			t.Fatal("Standard accepted a nil protected mux")
		}
	})
	t.Run("authorization half declared", func(t *testing.T) {
		reg := testRegistry(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(reg, verifier, http.NewServeMux(), WithAuthorization(&stubAuthorizer{}, nil)); err == nil {
			t.Fatal("Standard accepted an authorizer with no rule table")
		}
		if _, err := Standard(reg, verifier, http.NewServeMux(), WithAuthorization(nil, []rbac.RouteRule{{Path: app.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}}})); err == nil {
			t.Fatal("Standard accepted a rule table with no authorizer")
		}
	})
	t.Run("admin prefix nothing mounts", func(t *testing.T) {
		reg := testRegistry(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(reg, verifier, http.NewServeMux(), WithAdminPrefix("/api/v1/admin")); err == nil {
			t.Fatal("Standard accepted an admin prefix no mounted route lies below")
		}
	})
	t.Run("no authn route", func(t *testing.T) {
		reg := testRegistry(t, &routesModule{name: "empty"})
		if _, err := Standard(reg, verifier, http.NewServeMux()); err == nil {
			t.Fatal("Standard accepted a registry with no authn subtree")
		}
	})
}

// TestStandard_UndeclaredRouteFailsTheGuard pins that the table's
// exhaustiveness is enforced through the derivation: a mounted route the
// table does not name refuses the composition rather than being mounted
// ungated.
func TestStandard_UndeclaredRouteFailsTheGuard(t *testing.T) {
	reg := testRegistry(t, &routesModule{name: "fixture", routes: []pkgcore.MountedRoute{
		{Path: app.AuthnAPIPath, Handler: http.NewServeMux()},
		{Path: "/api/v1/notes", Handler: http.NewServeMux()},
	}})
	_, err := Standard(reg, newTestVerifier(t), http.NewServeMux(), WithAuthorization(&stubAuthorizer{}, []rbac.RouteRule{
		{Path: app.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}},
	}))
	if err == nil {
		t.Fatal("Standard mounted a route the rule table does not name")
	}
}
