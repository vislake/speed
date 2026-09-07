package integration_test

// Runnable documentation for round 6's new public API: Service.Authenticate,
// AuthMiddleware, NewAuthMiddleware, HeaderAPIKey and
// AuthenticatedAPIKeyFromContext. Mirrors handler_example_test.go's own
// convention -- compiled AND executed by `go test`, so a change to this
// module's own authentication surface that breaks the documented usage
// fails the build rather than only rotting in prose.
//
// It drives the exact property go/integration/AGENTS.md's round-5 section
// named as unverifiable until this round shipped: a key authenticates, its
// OLD raw value is refused after Rotate, and the NEW one authenticates in
// its place -- entirely through a real, composed net/http.Handler (no mock
// standing in for AuthMiddleware or Service.Authenticate).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"

	"github.com/vislake/speed/go/integration"
)

// ExampleModule_WithAuthenticationGuard demonstrates the pre-auth rate-limit
// gate: WithAuthenticationGuard wires an HTTPGuard that AuthMiddleware
// applies BEFORE it authenticates anything, so a request bearing a forged
// X-API-Key -- which would otherwise reach Service.Authenticate's lookups
// with no bound at all -- pays the guard's budget and, once it is spent, is
// refused with 429 without any authentication running (the layering
// argument lives in middleware.go's own doc comment).
func ExampleWithAuthenticationGuard() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:integration_authguard_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	// A guard over the module's own three-layer limiter, budgeted at two
	// global hits per minute for the demo (the smallest budget
	// go/ratelimit accepts: a Rate of 1 is refused as un-honourable -- see
	// integration.LayeredLimits' own doc comment). Its Extractor runs
	// BEFORE any authentication, so it cannot read resolved tenant/key
	// identifiers from the request context; the empty-identifier answer it
	// returns here charges the global layer (and, were they enabled, the
	// shared anonymous counters of the tenant/key layers) -- see
	// WithAuthenticationGuard's own doc comment.
	guard := integration.NewHTTPGuard(
		integration.NewLayeredLimiter(ratelimit.New(pkgcore.NewMemoryKVStore()), integration.LayeredLimits{
			Global: ratelimit.Limit{Rate: 2, Per: time.Minute},
		}),
		"example-auth-guard",
		func(*http.Request) (tenantKey, apiKeyID string) { return "", "" },
	)
	m := integration.NewModule(db, integration.WithAuthenticationGuard(guard))

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if regErr := m.Register(reg); regErr != nil {
		fmt.Println("register module:", regErr)
		return
	}
	if _, attachErr := m.Attach(reg); attachErr != nil {
		fmt.Println("attach:", attachErr)
		return
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := integration.NewAuthMiddleware(m).Middleware(ok)

	forge := func() int {
		req := httptest.NewRequest(http.MethodGet, "/whoami", http.NoBody)
		req.Header.Set(integration.HeaderAPIKey, "sk_this-was-never-issued")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// The first two forged requests pass the guard's two-hit budget and are
	// refused by authentication itself; the third is refused BY THE GUARD
	// -- 429 -- before any Authenticate call runs.
	fmt.Println("forged request 1:", forge())
	fmt.Println("forged request 2:", forge())
	fmt.Println("forged request 3:", forge())

	// Output:
	// forged request 1: 401
	// forged request 2: 401
	// forged request 3: 429
}

// ExampleAuthMiddleware demonstrates the full inbound-authentication path:
// a demo "whoami" handler, mounted behind AuthMiddleware, answers the
// resolved tenant and key id from AuthenticatedAPIKeyFromContext -- no
// tenant needs to be known ahead of the request, since AuthMiddleware
// resolves it from the presented key itself (see Service.Authenticate's own
// "Tenant resolution" doc comment section for why that differs from every
// other Service method).
func ExampleAuthMiddleware() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:integration_authenticate_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := integration.NewModule(db)

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if regErr := m.Register(reg); regErr != nil {
		fmt.Println("register module:", regErr)
		return
	}
	svc, err := m.Attach(reg)
	if err != nil {
		fmt.Println("attach:", err)
		return
	}

	// whoami is the minimal demo handler AuthMiddleware gates: it reads what
	// Middleware attached to the request context and echoes the resolved
	// tenant back, never touching go/integration's Service directly --
	// exactly the shape a real host's own inbound route would take. lastKeyID
	// captures the resolved key's own id so the Example below can compare it
	// against a known value without printing a non-deterministic uuid.
	var lastKeyID string
	whoami := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated, _ := integration.AuthenticatedAPIKeyFromContext(r.Context())
		lastKeyID = authenticated.KeyID
		tenant, _ := pkgcore.TenantFromContext(r.Context())
		fmt.Fprintf(w, "tenant=%s", tenant)
	})
	handler := integration.NewAuthMiddleware(m).Middleware(whoami)

	do := func(rawKey string) (int, string) {
		lastKeyID = ""
		req := httptest.NewRequest(http.MethodGet, "/whoami", http.NoBody)
		if rawKey != "" {
			req.Header.Set(integration.HeaderAPIKey, rawKey)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// A key issued under a resolved tenant context (Service.Create still
	// requires one, unlike Authenticate) authenticates over HTTP with no
	// tenant known ahead of the request at all.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))
	created, err := svc.Create(tenantCtx, integration.CreateInput{CreatedBy: "user-1"})
	if err != nil {
		fmt.Println("create:", err)
		return
	}
	status, body := do(created.Key)
	fmt.Println("authenticated:", status, body, "resolved the right key:", lastKeyID == created.ID)

	// No credential at all is refused -- the same outward answer a wrong one
	// gets.
	status, _ = do("")
	fmt.Println("no credential:", status)

	// Rotate: the OLD raw key value is now refused, and the NEW one
	// authenticates in its place -- this is the exact property that had no
	// lookup path to prove before this round.
	rotated, err := svc.Rotate(tenantCtx, created.ID)
	if err != nil {
		fmt.Println("rotate:", err)
		return
	}
	status, _ = do(created.Key)
	fmt.Println("old key after rotate:", status)
	status, body = do(rotated.Key)
	fmt.Println("new key after rotate:", status, body, "resolved the new key:", lastKeyID == rotated.ID)

	// Output:
	// authenticated: 200 tenant=acme-dental resolved the right key: true
	// no credential: 401
	// old key after rotate: 401
	// new key after rotate: 200 tenant=acme-dental resolved the new key: true
}
