package chain_test

// Runnable documentation for the chain package's public API, mirroring
// go/tenancy/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to the package's public API that
// breaks the documented usage fails the build instead of only rotting in
// prose.

import (
	"context"
	"crypto"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// exampleKeySource is an authn.KeySource carrying no verification keys, so
// every token fails verification -- the anonymous-request shape these
// examples compose around. A real host hands the verifier its pki
// module's service instead.
type exampleKeySource struct{}

func (exampleKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	return nil
}

func (exampleKeySource) ActiveSigner(context.Context, string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	return "", "", nil, errors.New("exampleKeySource: signing is out of scope here")
}

func (exampleKeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	return nil, nil
}

// exampleModule is a pkgcore.Module mounting one route, so the example can
// bootstrap a registry without any business module; a real host's registry
// comes from Kernel.Bootstrap over its own module set.
type exampleModule struct{}

func (exampleModule) Name() string         { return "example" }
func (exampleModule) DependsOn() []string  { return nil }
func (exampleModule) Migrations() embed.FS { return embed.FS{} }
func (exampleModule) Locales() embed.FS    { return embed.FS{} }
func (exampleModule) OpenAPISpec() []byte  { return nil }
func (m exampleModule) Register(reg *pkgcore.ComponentRegistry) error {
	reg.RoutesSeat().Mount(app.AuthnAPIPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "authn handler: %s", r.URL.Path)
	}))
	reg.RoutesSeat().Mount("/api/v1/notes", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "notes")
	}))
	return nil
}

// ExampleStandard shows the derivation a host composes when its route
// layout is the registry's own: Standard reads the mounted routes from the
// bootstrapped registry, admits them through the host's route-authorization
// table, splits the authn subtree out, and mounts the rest on the host's
// protected face. With no rbac module in this example's registry there is
// no rule table to pass, which is why the option is omitted -- a host with
// an authorization domain adds WithAuthorization.
func ExampleStandard() {
	verifier, err := authn.NewVerifier(exampleKeySource{})
	if err != nil {
		panic(err)
	}

	reg, err := componenttest.DeclareModules(exampleModule{})
	if err != nil {
		panic(err)
	}

	// The host's protected face: the mux the application engine prepared,
	// carrying the platform liveness route and the host's own routes.
	protected := http.NewServeMux()
	protected.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})

	handler, err := chain.Standard(reg, verifier, protected)
	if err != nil {
		panic(err)
	}

	for _, path := range []string{"/api/v1/notes", "/healthz", app.AuthnAPIPath + "/register"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s: %d\n", path, rec.Code)
	}

	// Output:
	// /api/v1/notes: 403
	// /healthz: 200
	// /api/v1/authn/register: 200
}

// ExampleChain shows the composition every host is built on: the host's
// own protected mux plus authn's subtree, wrapped by the platform's fixed
// middleware chain. The example walks the three request classes the chain
// tells apart -- an ordinary route with no Principal (refused, fail
// closed), a pre-auth platform path (served), and authn's own subtree
// (dispatched ahead of tenant resolution).
func ExampleChain() {
	verifier, err := authn.NewVerifier(exampleKeySource{})
	if err != nil {
		panic(err)
	}

	// The host's own routes: an ordinary API surface plus the liveness
	// route (obs.MountLiveness(mux) and the host's guarded module routes
	// are what a real Protected carries).
	protected := http.NewServeMux()
	protected.HandleFunc("/api/v1/notes", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "notes")
	})
	protected.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "ok")
	})

	handler, err := chain.Chain(chain.Config{
		Verifier:  verifier,
		Protected: protected,
		AuthnRoutes: []pkgcore.MountedRoute{{
			Path: app.AuthnAPIPath,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// authn's own handler decides per operation whether a
				// Principal is required; it is reached with or without one.
				_, _ = fmt.Fprintf(w, "authn handler: %s", r.URL.Path)
			}),
		}},
	})
	if err != nil {
		panic(err)
	}

	for _, path := range []string{"/api/v1/notes", "/healthz", app.AuthnAPIPath + "/register"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s: %d\n", path, rec.Code)
	}

	// Output:
	// /api/v1/notes: 403
	// /healthz: 200
	// /api/v1/authn/register: 200
}
