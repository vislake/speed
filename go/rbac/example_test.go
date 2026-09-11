package rbac_test

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/rbac"
)

// Example shows the whole wiring sequence a host performs for rbac: open
// and migrate the database, bootstrap the kernel over every module, then
// Attach rbac to the registry Bootstrap returned.
//
// The ordering is the point. Attach freezes the snapshot of every
// permission the host's modules declared, so it must run AFTER Bootstrap
// has given every module its turn to register -- which is why it is a
// method on the Module rather than something Register could have done.
func Example() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:rbac_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := rbac.NewModule(db)

	// Versioned SQL only -- never AutoMigrate.
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	// The module's Register and its Attach share the assembly's one Init
	// window: the seats accept writes only during Init, and Attach is what
	// freezes the catalog and installs the invalidation subscriptions.
	registry := componenttest.NewRegistry()
	var svc *rbac.Service
	if err := componenttest.DeclareAll(registry, module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			return nil
		},
	); err != nil {
		fmt.Println("declare and attach:", err)
		return
	}
	// Close stops the decision cache's janitor at shutdown.
	defer func() { _ = svc.Close() }()

	fmt.Println(registry.Permissions.Permissions())
	// Output: [rbac:manage rbac:read]
}

// ExampleService_DeclaredPermissions shows the read-only catalog
// accessor: the full permission catalog every module declared, frozen at
// Attach -- what a role-management UI
// renders as "every permission that exists" when defining a new role,
// as opposed to ListPermissions, which answers what one already-granted
// Subject currently holds.
func ExampleService_DeclaredPermissions() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:rbac_example_declared_permissions?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	module := rbac.NewModule(db)
	migrations := dbkit.NewMigrationRegistry()
	if regErr := migrations.Register(module); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := migrations.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	registry := componenttest.NewRegistry()
	var svc *rbac.Service
	if err := componenttest.DeclareAll(registry, module.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			return nil
		},
	); err != nil {
		fmt.Println("declare and attach:", err)
		return
	}
	defer func() { _ = svc.Close() }()

	// No role has been defined and nobody has been granted anything, yet
	// DeclaredPermissions still reports the whole catalog: it is a
	// question about what the platform knows how to grant, not about any
	// one subject's own bindings.
	fmt.Println(svc.DeclaredPermissions())
	// Output: [rbac:manage rbac:read]
}

// ExampleWithSubject shows how the authenticating side hands rbac an
// identity. This is the module's first no-import seam: rbac never learns
// what a user record looks like, so it never imports the module that owns
// one.
func ExampleWithSubject() {
	// In production authn builds this from the access token's claims. The
	// tenant always comes from the claims, never from a request parameter,
	// header or body.
	ctx := rbac.WithSubject(context.Background(), rbac.Subject{
		TenantID: "tenant-a",
		UserID:   "user-1",
	})

	sub, ok := rbac.SubjectFromContext(ctx)
	fmt.Println(ok, sub.TenantID, sub.UserID)

	// An incomplete subject is reported as no subject at all, so a
	// caller's "no subject, deny" branch covers both cases.
	partial := rbac.WithSubject(context.Background(), rbac.Subject{TenantID: "tenant-a"})
	_, ok = rbac.SubjectFromContext(partial)
	fmt.Println(ok)

	// Output:
	// true tenant-a user-1
	// false
}

// ExamplePathWithinSubtree shows the segment-aware matching that decides
// whether one materialized path lies inside another. It is exported and
// separately usable because a consumer filtering its own rows needs the
// same rule the evaluator applies.
func ExamplePathWithinSubtree() {
	fmt.Println(rbac.PathWithinSubtree("/g1/r2", "/g1/r2"))
	fmt.Println(rbac.PathWithinSubtree("/g1/r2/s7", "/g1/r2"))

	// The case a plain string-prefix test gets wrong, handing region 20's
	// data to whoever was granted region 2.
	fmt.Println(rbac.PathWithinSubtree("/g1/r20", "/g1/r2"))

	// Output:
	// true
	// true
	// false
}

// storeResolver is a host's SubtreeResolver: the one fact rbac is allowed
// to know about the organization tree. In a real deployment the org module
// implements this; rbac never imports it either way.
type storeResolver map[string]string

func (r storeResolver) NodePath(_ context.Context, nodeID string) (string, bool, error) {
	path, ok := r[nodeID]
	return path, ok, nil
}

// ExampleService_DataScope shows the two questions an authorization
// decision splits into, and why a handler that returns tenant data must
// ask both.
//
// Can is the coarse gate ("may this request proceed at all"). DataScope
// says over which slice of the organization tree the permission is held,
// which is what a row-level filter needs. A subject granted a role over
// one branch passes the gate and still sees only that branch.
func ExampleService_DataScope() {
	ctx := context.Background()

	svc, err := newExampleService(ctx, "rbac_example_scope", storeResolver{
		"node-region-2": "/group1/region2",
	})
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer func() { _ = svc.Close() }()

	// Seeding and granting happen under a tenant context; the tenant comes
	// from the access token's claims, never from a request parameter.
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	if err = svc.EnsureBuiltinRoles(tenantCtx); err != nil {
		fmt.Println("seed:", err)
		return
	}
	if _, err = svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "region-manager",
		DescriptionKey: "rbac.role.admin",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		fmt.Println("define:", err)
		return
	}

	// The Subject is assembled by whoever authenticated the request.
	manager := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err = svc.AssignRole(tenantCtx, manager, "region-manager", rbac.Scope{NodeID: "node-region-2"}); err != nil {
		fmt.Println("assign:", err)
		return
	}

	allowed, err := svc.Can(ctx, manager, "read", "notes")
	if err != nil {
		fmt.Println("can:", err)
		return
	}
	fmt.Println("may read notes:", allowed)

	scope, err := svc.DataScope(ctx, manager, "read", "notes")
	if err != nil {
		fmt.Println("scope:", err)
		return
	}
	fmt.Println("tenant-wide:", scope.TenantWide)
	fmt.Println("in scope /group1/region2/store7:", scope.Includes("/group1/region2/store7"))
	fmt.Println("in scope /group1/region3:", scope.Includes("/group1/region3"))

	// Output:
	// may read notes: true
	// tenant-wide: false
	// in scope /group1/region2/store7: true
	// in scope /group1/region3: false
}

// ExampleRequirePermissionFunc shows rbac's whole contribution to the HTTP
// layer: the gate that sits after authentication in the fixed middleware
// chain. This module mounts no routes
// of its own -- it hands the host a gate to wrap the host's routes in.
//
// The Func form picks the required permission per request, which is what a
// single path serving several methods needs: a read permission on GET, a
// write permission on POST. The chooser must depend only on the request's
// ROUTE -- never on a header, query parameter or body, because a
// permission the caller can choose is a permission the caller can choose
// to be one they hold.
func ExampleRequirePermissionFunc() {
	ctx := context.Background()

	svc, err := newExampleService(ctx, "rbac_example_gate", nil)
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer func() { _ = svc.Close() }()

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	if _, err = svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "note-reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		fmt.Println("define:", err)
		return
	}

	reader := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err = svc.AssignRole(tenantCtx, reader, "note-reader", rbac.Scope{}); err != nil {
		fmt.Println("assign:", err)
		return
	}

	notes := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	gate := rbac.RequirePermissionFunc(svc, func(r *http.Request) string {
		if r.Method == http.MethodGet {
			return rbac.Permission("notes", "read")
		}
		// Every other method needs write. A method the table forgot would
		// return "", which denies -- there is deliberately no "nothing
		// required" answer.
		return rbac.Permission("notes", "write")
	})(notes)

	// The authenticating side installs the Subject; the gate reads it back.
	// Neither module imports the other.
	call := func(method string) int {
		r := httptest.NewRequest(method, "/api/v1/notes", nil)
		r = r.WithContext(rbac.WithSubject(r.Context(), reader))
		rec := httptest.NewRecorder()
		gate.ServeHTTP(rec, r)
		return rec.Code
	}

	fmt.Println("GET :", call(http.MethodGet))
	fmt.Println("POST:", call(http.MethodPost))

	// A request with no Subject at all is refused the same way a denied
	// one is -- an unauthenticated caller learns nothing from the
	// difference.
	anonymous := httptest.NewRecorder()
	gate.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/api/v1/notes", nil))
	fmt.Println("anon:", anonymous.Code)

	// Output:
	// GET : 200
	// POST: 403
	// anon: 403
}

// ExampleGuardRoutes shows the platform's route-authorization table: a host
// declares one decision per mounted route, GuardRoutes wraps each gated
// route in the permission gate, and a mounted route the table does not name
// fails the assembly rather than being served undecided -- so "we forgot"
// and "we meant it public" can never look alike.
func ExampleGuardRoutes() {
	ctx := context.Background()

	svc, err := newExampleService(ctx, "rbac_example_guard_routes", nil)
	if err != nil {
		fmt.Println("setup:", err)
		return
	}
	defer func() { _ = svc.Close() }()

	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")
	if _, err = svc.DefineRole(tenantCtx, rbac.RoleDefinition{
		Key:            "note-reader",
		DescriptionKey: "rbac.role.member",
		Permissions:    []string{"notes:read"},
	}); err != nil {
		fmt.Println("define:", err)
		return
	}
	reader := rbac.Subject{TenantID: "tenant-a", UserID: "user-1"}
	if err = svc.AssignRole(tenantCtx, reader, "note-reader", rbac.Scope{}); err != nil {
		fmt.Println("assign:", err)
		return
	}

	// What the modules mounted (reg.Routes.Routes() in a real host)...
	mounted := []pkgcore.MountedRoute{
		{Path: "/api/v1/notes", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})},
		{Path: "/api/v1/config/public", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})},
	}
	// ...and the host's decision for each.
	guarded, err := rbac.GuardRoutes(svc, mounted, []rbac.RouteRule{
		{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Permission: func(r *http.Request) string {
			if r.Method == http.MethodGet {
				return rbac.Permission("notes", "read")
			}
			return rbac.Permission("notes", "write")
		}}},
		{Path: "/api/v1/config/public", Access: pkgcore.RouteAccess{Public: true}},
	})
	if err != nil {
		fmt.Println("guard:", err)
		return
	}

	mux := http.NewServeMux()
	pkgcore.MountRoutes(mux, guarded...)

	call := func(subject *rbac.Subject, method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		if subject != nil {
			r = r.WithContext(rbac.WithSubject(r.Context(), *subject))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		return rec.Code
	}

	fmt.Println("reader GET  notes:", call(&reader, http.MethodGet, "/api/v1/notes"))
	fmt.Println("reader POST notes:", call(&reader, http.MethodPost, "/api/v1/notes"))
	// The public declaration is what admits the anonymous request; no
	// Subject exists for it at all.
	fmt.Println("anon config:", call(nil, http.MethodGet, "/api/v1/config/public"))

	// A mounted route the table does not decide fails assembly.
	undecided := []pkgcore.MountedRoute{{Path: "/api/v1/invoices", Handler: http.NotFoundHandler()}}
	_, err = rbac.GuardRoutes(svc, append(append([]pkgcore.MountedRoute{}, mounted...), undecided...), []rbac.RouteRule{
		{Path: "/api/v1/notes", Access: pkgcore.RouteAccess{Public: true}},
		{Path: "/api/v1/config/public", Access: pkgcore.RouteAccess{Public: true}},
	})
	fmt.Println("undecided route refused:", err != nil)

	// Output:
	// reader GET  notes: 200
	// reader POST notes: 403
	// anon config: 200
	// undecided route refused: true
}

// ExampleSplitPermission shows the exported splitter a consumer uses to take
// a permission apart the way rbac's own gate does. The cut is at the LAST
// separator, so a module naming its permissions "<module>:<entity>:<verb>"
// round-trips through Permission exactly.
func ExampleSplitPermission() {
	resource, action, ok := rbac.SplitPermission("integration:apikey:read")
	fmt.Println(resource, action, ok)
	fmt.Println(rbac.Permission(resource, action))

	// A string with no separator names no permission and is refused.
	_, _, ok = rbac.SplitPermission("notes")
	fmt.Println(ok)

	// Output:
	// integration:apikey read true
	// integration:apikey:read
	// false
}

// newExampleService performs the wiring Example already walks through,
// with one stand-in module declaring the "notes:read" permission the
// example grants. It keeps the example above about authorization rather
// than about bootstrap.
func newExampleService(ctx context.Context, dsn string, resolver rbac.SubtreeResolver) (*rbac.Service, error) {
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:" + dsn + "?mode=memory&cache=shared",
	})
	if err != nil {
		return nil, err
	}

	module := rbac.NewModule(db, rbac.WithSubtreeResolver(resolver))
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(module); err != nil {
		return nil, err
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		return nil, err
	}

	registry := componenttest.NewRegistry()
	var svc *rbac.Service
	if err = componenttest.DeclareAll(registry, module.Register, notesLikeModule{}.Register,
		func(r *pkgcore.ComponentRegistry) error {
			attached, attachErr := module.Attach(r)
			if attachErr != nil {
				return attachErr
			}
			svc = attached
			return nil
		},
	); err != nil {
		return nil, err
	}
	return svc, nil
}

// notesLikeModule stands in for a business module that declares a
// permission of its own, so the frozen catalog the example grants from is
// not just rbac's own two entries.
type notesLikeModule struct{}

func (notesLikeModule) Name() string         { return "notes" }
func (notesLikeModule) DependsOn() []string  { return nil }
func (notesLikeModule) Migrations() embed.FS { return embed.FS{} }
func (notesLikeModule) Locales() embed.FS    { return embed.FS{} }
func (notesLikeModule) OpenAPISpec() []byte  { return nil }

func (notesLikeModule) Register(reg *pkgcore.ComponentRegistry) error {
	return reg.PermissionsSeat().Add("notes:read", "notes:write")
}

// ExampleSubtreeResolverFunc wires rbac's organization-tree seam with a
// closure instead of a named type: the adapter gives a plain function the
// NodePath method WithSubtreeResolver takes, so a host whose resolver is
// one closure over its own tree needs no type declaration at all.
func ExampleSubtreeResolverFunc() {
	resolver := rbac.SubtreeResolverFunc(func(_ context.Context, nodeID string) (string, bool, error) {
		// A real host resolves the node against its own tree here -- the
		// reference app's implementation reads org.Scope.Path -- and folds
		// its "no such node" answer into ok=false: rbac denies a
		// node-scoped binding it cannot resolve rather than widening it.
		if nodeID == "store7" {
			return "/g1/r2/store7", true, nil
		}
		return "", false, nil
	})

	var seam rbac.SubtreeResolver = resolver
	path, ok, err := seam.NodePath(context.Background(), "store7")
	fmt.Println(path, ok, err)
	_, ok, err = seam.NodePath(context.Background(), "gone")
	fmt.Println(ok, err)
	// Output:
	// /g1/r2/store7 true <nil>
	// false <nil>
}
