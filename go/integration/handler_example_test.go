package integration_test

// Runnable documentation for round 4's new public API: Handler, NewHandler,
// SubjectResolver and WithSubjectResolver. Mirrors example_test.go's own
// convention -- compiled AND executed by `go test`, so a change to this
// module's public HTTP surface that breaks the documented usage fails the
// build rather than only rotting in prose.
//
// It drives round 1's full API-key lifecycle through a real, composed
// net/http.Handler -- no mock standing in for api.ServerInterface itself --
// covering create (the raw key is available exactly once, right in the
// response body), list (the raw key never reappears), rotate (the
// replacement's id differs from the predecessor's) and revoke.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/integration"
)

// fixedSubjectResolver satisfies integration.SubjectResolver, standing in
// for a real host's authenticating layer -- see SubjectResolver's own doc
// comment in seams.go for why this is a structurally-typed seam rather than
// an authn import. A real host answers from a verified access token's
// claims; this example answers a fixed id.
type fixedSubjectResolver struct{ userID string }

// Subject implements integration.SubjectResolver.
func (f fixedSubjectResolver) Subject(*http.Request) (string, bool) {
	return f.userID, true
}

// ExampleHandler documents Handler, NewHandler, SubjectResolver and
// WithSubjectResolver together, since none of the four is useful on its
// own: a Handler is only reachable by mounting it, which needs a
// SubjectResolver wired through WithSubjectResolver before Register runs.
func ExampleHandler() {
	ctx := context.Background()

	// A real host opens PostgreSQL in the distributed deployment mode.
	// SQLite keeps this example self-contained under `go test`.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:integration_handler_example?mode=memory&cache=shared",
	})
	if err != nil {
		fmt.Println("open:", err)
		return
	}

	m := integration.NewModule(db, integration.WithSubjectResolver(fixedSubjectResolver{userID: "user-1"}))

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
	if _, err := m.Attach(reg); err != nil {
		fmt.Println("attach:", err)
		return
	}

	// reg.Routes.Routes() is exactly what a real host copies onto its own
	// mux (examples/reference-app/cmd/server's mountModuleRoutes); this
	// example dispatches straight to the one route this module mounts,
	// since api.HandlerFromMux (handler.go's NewHandler) already routes
	// every operation this fragment declares.
	var handler http.Handler
	for _, route := range reg.Routes.Routes() {
		handler = route.Handler
	}
	if handler == nil {
		fmt.Println("no route mounted")
		return
	}

	// APIKey is tenant data, so every request needs a tenant in its
	// context -- tenancy.Middleware's job in a real host, done by hand here.
	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID("acme-dental"))

	do := func(method, path string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, http.NoBody).WithContext(tenantCtx)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var decoded map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
		return rec.Code, decoded
	}

	// Create with an empty body: every field of the request schema is
	// optional, and no PermissionLister is wired here -- neither is needed
	// for a key requesting zero scopes (see Service.Create's own doc
	// comment).
	createStatus, created := do(http.MethodPost, "/api/v1/integration/apikeys")
	fmt.Println("create status:", createStatus, "raw key present:", created["key"] != nil)

	listStatus, listed := do(http.MethodGet, "/api/v1/integration/apikeys")
	items, _ := listed["apiKeys"].([]any)
	firstRow, _ := items[0].(map[string]any)
	_, rawKeyOnListedRow := firstRow["key"]
	fmt.Println("list status:", listStatus, "count:", len(items), "raw key ever relisted:", rawKeyOnListedRow)

	keyID, _ := created["id"].(string)
	rotateStatus, rotated := do(http.MethodPost, "/api/v1/integration/apikeys/"+keyID+"/rotate")
	fmt.Println("rotate status:", rotateStatus, "new id differs from predecessor:", rotated["id"] != created["id"])

	newID, _ := rotated["id"].(string)
	revokeStatus, _ := do(http.MethodDelete, "/api/v1/integration/apikeys/"+newID)
	fmt.Println("revoke status:", revokeStatus)

	// Output:
	// create status: 201 raw key present: true
	// list status: 200 count: 1 raw key ever relisted: false
	// rotate status: 200 new id differs from predecessor: true
	// revoke status: 204
}
