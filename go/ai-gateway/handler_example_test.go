package aigateway_test

// Runnable documentation for this package's HTTP public API: Handler,
// NewHandler and the three permissions (PermissionRead, PermissionWrite,
// PermissionManagePlatform) their operations are gated on. Compiled AND
// executed by `go test`, so a change to this module's HTTP surface that
// breaks the documented usage fails the build rather than only rotting in
// prose.
//
// It drives the credential-write surface end to end through a real,
// composed net/http.Handler -- no mock standing in for api.ServerInterface
// itself: a provider with no credential at all answers 404, writing the
// platform-wide default makes it resolve, and a tenant's own BYOK write
// then overrides that platform default for the SAME tenant -- the write
// path genuinely connects to the read path CredentialService.Resolve
// serves.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers
	// dbkit.DialectSQLite so dbkit.Open below has a driver to build from.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	aigateway "github.com/vislake/speed/go/ai-gateway"
)

// ExampleHandler documents Handler, NewHandler and the module's HTTP
// surface together, since none is useful on its own: a Handler is only
// reachable by mounting it, which Module.Register does the moment a host
// bootstraps the module.
func ExampleHandler() {
	ctx := context.Background()

	// The credential column's cipher must be registered BEFORE dbkit.Open:
	// GORM resolves a model's serializer while it parses the schema.
	cipher, err := dbkit.NewCipher([]byte("01234567890123456789012345678901"))
	if err != nil {
		fmt.Println("new cipher:", err)
		return
	}
	if regErr := aigateway.RegisterCredentialAPIKeySerializer(cipher); regErr != nil {
		fmt.Println("register serializer:", regErr)
		return
	}

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:aigateway_handler_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := aigateway.NewModule(db)

	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(m); regErr != nil {
		fmt.Println("register migrations:", regErr)
		return
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		fmt.Println("apply migrations:", applyErr)
		return
	}

	reg := componenttest.NewRegistry()
	if regErr := componenttest.DeclareInto(reg, m); regErr != nil {
		fmt.Println("register module:", regErr)
		return
	}

	// reg.Routes.Routes() is exactly what a real host copies onto its own
	// mux (examples/reference-app/internal/app's mountModuleRoutes, gated
	// there on rbac's PermissionRead/PermissionWrite/
	// PermissionManagePlatform per module.go's own doc comment); this
	// example dispatches straight to the one route this module mounts,
	// since api.HandlerFromMux (handler.go's NewHandler) already routes
	// every operation this fragment declares. No permission gate runs
	// here at all -- that enforcement is the HOST's router-level concern
	// (see Handler's own doc comment), never this package's.
	var handler http.Handler
	for _, route := range reg.Routes.Routes() {
		handler = route.Handler
	}
	if handler == nil {
		fmt.Println("no route mounted")
		return
	}

	// The credential table is platform data, so a tenant context is needed
	// only for the tenant-scoped write and read below -- tenancy.Middleware's
	// job in a real host, done by hand here.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))

	do := func(ctx context.Context, method, path, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		return rec.Code, decoded
	}

	const provider = aigateway.ProviderOpenAICompatible
	credentialPath := "/api/v1/ai-gateway/credentials/" + provider

	// Nothing is configured yet: neither the tenant nor the platform has a
	// row for this provider.
	getStatus, getBody := do(tenantCtx, http.MethodGet, credentialPath, "")
	fmt.Println("get before any write: status", getStatus, "code", getBody["code"])

	// Write the platform-wide default -- gated on PermissionManagePlatform
	// in a real deployment, never PermissionWrite (module.go's own doc
	// comment on the two constants). The platform row is the operator's
	// own default, deliberately outside the tenant-baseUrl SSRF guard
	// (ssrf.go's file header), so the hostname needs no resolvability.
	platformStatus, platformBody := do(ctx, http.MethodPut, credentialPath+"/platform",
		`{"apiKey":"sk-platform-default","baseUrl":"https://api.example.com/v1"}`)
	fmt.Println("set platform credential: status", platformStatus, "scope", platformBody["scope"])

	// The tenant now resolves to the platform default -- it has no BYOK
	// row of its own yet.
	afterPlatformStatus, afterPlatformBody := do(tenantCtx, http.MethodGet, credentialPath, "")
	fmt.Println("get after platform write: status", afterPlatformStatus, "scope", afterPlatformBody["scope"])

	// Write the tenant's OWN BYOK credential -- gated on PermissionWrite,
	// an ordinary tenant-scoped permission distinct from the platform
	// write's PermissionManagePlatform above. A tenant-tier baseUrl is
	// SSRF-validated at write time (ValidateBaseURL), so the example names
	// a literal public IP: a hostname this example cannot resolve -- or
	// one resolving to a private, loopback or link-local address -- would
	// be refused with aigateway.base_url_unresolvable /
	// aigateway.base_url_blocked.
	tenantStatus, tenantBody := do(tenantCtx, http.MethodPut, credentialPath+"/tenant",
		`{"apiKey":"sk-tenant-byok","baseUrl":"https://93.184.216.34/v1"}`)
	fmt.Println("set tenant credential: status", tenantStatus, "scope", tenantBody["scope"])

	// The same tenant now resolves to its OWN row, overriding the
	// platform default -- the tenant-override-down-to-system-default
	// resolution order CredentialService.Resolve documents.
	afterTenantStatus, afterTenantBody := do(tenantCtx, http.MethodGet, credentialPath, "")
	fmt.Println("get after tenant write: status", afterTenantStatus, "scope", afterTenantBody["scope"],
		"api key ever present:", afterTenantBody["apiKey"] != nil)

	// Output:
	// get before any write: status 404 code aigateway.credential_not_found
	// set platform credential: status 200 scope system
	// get after platform write: status 200 scope system
	// set tenant credential: status 200 scope tenant
	// get after tenant write: status 200 scope tenant api key ever present: false
}
