package app_test

// Runnable documentation for the app package's public API, mirroring
// go/tenancy/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to the package's public API that
// breaks the documented usage fails the build instead of only rotting in
// prose. The chain package's own example (go/app/chain/example_test.go)
// documents the middleware composition built on top.

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// exampleFailingResolver fails every resolution -- the tenant-less state
// the pre-auth allowlist exists for.
type exampleFailingResolver struct{}

func (exampleFailingResolver) Resolve(*http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("example: no tenant resolvable")
}

// ExamplePreAuthAllowlist shows the platform's pre-auth surface: the paths
// that must work before a Principal exists pass the tenancy chain under
// both GET and HEAD, while any other method on the same path stays
// refused. A host adds its own pre-auth routes beside this set
// (chain.Config.ExtraAllowlist is the place).
func ExamplePreAuthAllowlist() {
	protected := tenancy.Middleware(exampleFailingResolver{}, app.PreAuthAllowlist()...)(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	)

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/healthz"},
		{http.MethodHead, "/metrics"},
		{http.MethodGet, "/api/v1/config/public"},
		{http.MethodPost, "/healthz"},
	} {
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, httptest.NewRequest(probe.method, probe.path, nil))
		fmt.Printf("%s %s: %d\n", probe.method, probe.path, rec.Code)
	}

	// Output:
	// GET /healthz: 200
	// HEAD /metrics: 200
	// GET /api/v1/config/public: 200
	// POST /healthz: 403
}
