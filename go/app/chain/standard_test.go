package chain

import (
	"context"
	"embed"
	"net/http"
	"slices"
	"testing"

	"github.com/vislake/speed/go/app"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/tenancy"
)

// routesModule is a module that mounts exactly the routes a test
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
func (m *routesModule) Register(reg *pkgcore.ComponentRegistry) error {
	for _, route := range m.routes {
		if err := pkgcore.MountRoute(reg, route.Path, route.Handler); err != nil {
			return err
		}
	}
	return nil
}

// middlewareModule is a module that declares exactly the middleware a test
// hands it, through the same declaration window a real component's Init
// uses, so a Standard test can exercise the middleware face without any
// HTTP-facing module of its own.
type middlewareModule struct {
	name string
	mws  []func(http.Handler) http.Handler
}

func (m *middlewareModule) Name() string         { return m.name }
func (m *middlewareModule) DependsOn() []string  { return nil }
func (m *middlewareModule) Migrations() embed.FS { return embed.FS{} }
func (m *middlewareModule) Locales() embed.FS    { return embed.FS{} }
func (m *middlewareModule) OpenAPISpec() []byte  { return nil }
func (m *middlewareModule) Register(reg *pkgcore.ComponentRegistry) error {
	registrar, ok, err := pkgcore.GetOptional[pkgcore.MiddlewareRegistrar](reg)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return registrar.Add(m.mws...)
}

// testFace drives the assembly over the modules through a FaceRecorder --
// the stand-in for the http component's product -- and returns that
// recorder: the modules' declaration bodies mount and declare on it, and
// Standard derives its partition and its outermost middleware layer from
// the very same value, exactly the shape a real boot has.
func testFace(t *testing.T, modules ...componenttest.Declarer) *componenttest.FaceRecorder {
	t.Helper()
	face := componenttest.NewFaceRecorder()
	reg := componenttest.NewRegistry()
	reg.Put(face)
	if err := componenttest.DeclareInto(reg, modules...); err != nil {
		t.Fatalf("assembly: %v", err)
	}
	return face
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
	face := testFace(t, &routesModule{name: "fixture", routes: []pkgcore.MountedRoute{
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

	handler, err := Standard(face, newTestVerifier(t), protected, opts...)
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

// TestStandard_AcceptsAComponentProduct pins the route source's shape: the
// http component's product answers the route reading, so a host whose
// assembly produced that product composes the chain from the declarations
// the component accumulated -- here a fixture component declaring through
// the same optional dependency a real module declares.
func TestStandard_AcceptsAComponentProduct(t *testing.T) {
	face := componenttest.NewFaceRecorder()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(face)
	authn := &countingHandler{}
	err := reg.Register(pkgcore.Component{
		Name: "fixture.routes",
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &routesProbe{}, nil
		},
		Init: func(_ context.Context, r *pkgcore.ComponentRegistry, _ any) error {
			return pkgcore.MountRoute(r, app.AuthnAPIPath, authn)
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

	handler, err := Standard(face, newTestVerifier(t), http.NewServeMux())
	if err != nil {
		t.Fatalf("Standard over a component product: %v", err)
	}
	if rec := do(handler, http.MethodGet, app.AuthnAPIPath+"/register", nil); authn.hits != 1 {
		t.Fatalf("GET %s/register: status %d, authn hits %d; the component product's routes must partition like the module registry's", app.AuthnAPIPath, rec.Code, authn.hits)
	}
}

// probeMiddleware returns a middleware recording its enter and exit under
// name into order, so a test can observe where the face's layer wrapped and
// how several registered middlewares nested.
func probeMiddleware(order *[]string, name string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*order = append(*order, name+":in")
			next.ServeHTTP(w, r)
			*order = append(*order, name+":out")
		})
	}
}

// TestStandard_AppliesTheMiddlewareFaceOutermost pins the declared layer's
// position and nesting: the middleware declared on the middleware face wraps
// the finished chain OUTSIDE authn.Middleware -- a request authn itself
// refuses (an invalid bearer) still passes through every probe -- on both
// the protected face and the structurally exempt branches, and the first
// registered middleware wraps outermost.
func TestStandard_AppliesTheMiddlewareFaceOutermost(t *testing.T) {
	var order []string
	authn := &countingHandler{}
	face := testFace(t,
		&routesModule{name: "fixture", routes: []pkgcore.MountedRoute{
			{Path: app.AuthnAPIPath, Handler: authn},
			{Path: "/api/v1/notes", Handler: &countingHandler{}},
		}},
		&middlewareModule{name: "fixture.middleware", mws: []func(http.Handler) http.Handler{
			probeMiddleware(&order, "outer"),
			probeMiddleware(&order, "inner"),
		}},
	)
	protected := http.NewServeMux()
	protected.Handle("/", http.NotFoundHandler())

	handler, err := Standard(face, newTestVerifier(t), protected)
	if err != nil {
		t.Fatalf("Standard: %v", err)
	}

	// The invalid bearer 401s inside authn.Middleware, before anything else
	// in the chain runs; the probes still saw the request, so the face's
	// layer must sit outside authn -- and it must nest first-registered-
	// outermost.
	rec := do(handler, http.MethodGet, "/api/v1/notes", map[string]string{"Authorization": "Bearer not-a-token"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/notes with an invalid bearer: status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	want := []string{"outer:in", "inner:in", "inner:out", "outer:out"}
	if !slices.Equal(order, want) {
		t.Fatalf("probe order for a request authn refused = %v, want %v: the face's middleware wraps outside authn.Middleware, first registered outermost", order, want)
	}

	// The structurally exempt branch passes through the same layer: the
	// probes see a request served by the authn subtree too.
	order = nil
	if rec := do(handler, http.MethodGet, app.AuthnAPIPath+"/register", nil); authn.hits != 1 {
		t.Fatalf("GET %s/register: status %d, authn hits %d; the exempt branch must serve the request", app.AuthnAPIPath, rec.Code, authn.hits)
	}
	if !slices.Equal(order, want) {
		t.Fatalf("probe order for an exempt-branch request = %v, want %v: every request passes through the face's layer", order, want)
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
		face := testFace(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(face, verifier, nil, WithAdminPrefix("")); err == nil {
			t.Fatal("Standard accepted a nil protected mux")
		}
	})
	t.Run("authorization half declared", func(t *testing.T) {
		face := testFace(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(face, verifier, http.NewServeMux(), WithAuthorization(&stubAuthorizer{}, nil)); err == nil {
			t.Fatal("Standard accepted an authorizer with no rule table")
		}
		if _, err := Standard(face, verifier, http.NewServeMux(), WithAuthorization(nil, []rbac.RouteRule{{Path: app.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}}})); err == nil {
			t.Fatal("Standard accepted a rule table with no authorizer")
		}
	})
	t.Run("admin prefix nothing mounts", func(t *testing.T) {
		face := testFace(t, &routesModule{name: "authn-only", routes: []pkgcore.MountedRoute{{Path: app.AuthnAPIPath, Handler: http.NewServeMux()}}})
		if _, err := Standard(face, verifier, http.NewServeMux(), WithAdminPrefix("/api/v1/admin")); err == nil {
			t.Fatal("Standard accepted an admin prefix no mounted route lies below")
		}
	})
	t.Run("no authn route", func(t *testing.T) {
		face := testFace(t, &routesModule{name: "empty"})
		if _, err := Standard(face, verifier, http.NewServeMux()); err == nil {
			t.Fatal("Standard accepted a registry with no authn subtree")
		}
	})
}

// TestStandard_FailedCompositionLeavesTheProtectedMuxUntouched pins that a
// refused composition mounts nothing: the derivation splits and validates the
// whole partition before it mounts the first route, so an error return leaves
// the caller's protected mux exactly as it was. Both refusals below fire after
// the partition split -- an admin prefix matching no route, and a registry with
// no authn route -- the window in which an eager mounting pass would have left
// the non-exempt routes behind.
func TestStandard_FailedCompositionLeavesTheProtectedMuxUntouched(t *testing.T) {
	cases := []struct {
		name        string
		dropAuthn   bool
		adminPrefix bool
	}{
		{name: "admin prefix matches nothing", adminPrefix: true},
		{name: "no authn route", dropAuthn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			notes := &countingHandler{}
			routes := []pkgcore.MountedRoute{
				{Path: app.AuthnAPIPath, Handler: &countingHandler{}},
				{Path: "/api/v1/notes", Handler: notes},
			}
			if tc.dropAuthn {
				routes = routes[1:]
			}
			face := testFace(t, &routesModule{name: "fixture", routes: routes})

			var opts []Option
			if tc.adminPrefix {
				opts = append(opts, WithAdminPrefix("/api/v1/admin"))
			}
			protected := http.NewServeMux()
			if _, err := Standard(face, newTestVerifier(t), protected, opts...); err == nil {
				t.Fatal("Standard accepted a composition it must refuse")
			}

			rec := do(protected, http.MethodGet, "/api/v1/notes", nil)
			if notes.hits != 0 {
				t.Fatalf("GET /api/v1/notes on the mux after a refused Standard: status %d, hits %d; the refusal must leave the mux untouched", rec.Code, notes.hits)
			}
		})
	}
}

// TestStandard_UndeclaredRouteFailsTheGuard pins that the table's
// exhaustiveness is enforced through the derivation: a mounted route the
// table does not name refuses the composition rather than being mounted
// ungated.
func TestStandard_UndeclaredRouteFailsTheGuard(t *testing.T) {
	face := testFace(t, &routesModule{name: "fixture", routes: []pkgcore.MountedRoute{
		{Path: app.AuthnAPIPath, Handler: http.NewServeMux()},
		{Path: "/api/v1/notes", Handler: http.NewServeMux()},
	}})
	_, err := Standard(face, newTestVerifier(t), http.NewServeMux(), WithAuthorization(&stubAuthorizer{}, []rbac.RouteRule{
		{Path: app.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}},
	}))
	if err == nil {
		t.Fatal("Standard mounted a route the rule table does not name")
	}
}
