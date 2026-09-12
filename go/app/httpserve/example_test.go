package httpserve_test

// Runnable documentation for the package's public API: the component's two
// declaration faces, the host link policy, and the composed face the Serve
// stage produces. Every example here is compiled and executed by `go test`,
// so a change to the public API that breaks the documented usage fails the
// build instead of only rotting in prose.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/app/httpserve"
	"github.com/vislake/speed/go/pkgcore"
)

// examplePolicyProvider is the host's policy component: the product that
// satisfies the http component's required (*httpserve.LinkPolicy) token.
// This example declares a chainless composition -- no authn module, so no
// fixed chain; a guarded host provides a verifier and its chain options
// (see LinkPolicy's own doc comment for the fields).
func examplePolicyProvider() pkgcore.Component {
	return pkgcore.Component{
		Name:     "example.host",
		Provides: []any{(*httpserve.LinkPolicy)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &httpserve.LinkPolicy{Chainless: true}, nil
		},
	}
}

// exampleModule mounts one route through the optional route face, the
// declaration shape every route-mounting module uses: when the composition
// selects the http component the mount lands on its face; when it does not,
// the module constructs with nothing mounted.
func exampleModule() pkgcore.Component {
	return pkgcore.Component{
		Name: "example.module",
		Requires: []pkgcore.Requirement{
			{Token: (*pkgcore.RouteRegistrar)(nil), Optional: true},
		},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return pkgcore.MountRoute(reg, "/api/v1/example", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("hello from the mounted route"))
			}))
		},
	}
}

// ExampleComponent shows the whole shape of a boot that serves HTTP: a
// composition selecting the http component (by name) and a host component
// providing the link policy, the assembly driven through the Serve stage
// (which composes the handler and, unless listen is disabled, opens the
// listener), and the composed face answering requests. The Serve stage --
// where the listener would open -- runs only after every component's Start
// callback, and Stop (the first beat) closes the listener before anything
// else is notified.
func ExampleComponent() {
	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{examplePolicyProvider(), exampleModule()} {
		if err := reg.Register(c); err != nil {
			panic(err)
		}
	}
	spec := app.LoadSpec{
		Host: &struct{}{},
		Overrides: &app.CompositionOverrides{
			Config: pkgcore.ComponentConfig{}.
				With("deployment", string(pkgcore.DeploymentModeStandalone)).
				With("strict", true).
				With("components", pkgcore.ComponentConfig{}.
					With("observability", false).
					With("example.host", nil).
					With("example.module", nil).
					With(httpserve.ComponentName, pkgcore.ComponentConfig{}.With("listen", false))),
		},
	}
	if err := app.Assemble(context.Background(), reg, spec); err != nil {
		panic(err)
	}
	defer func() { _ = app.Shutdown(context.Background(), reg) }()

	face, err := pkgcore.Get[*httpserve.Face](reg)
	if err != nil {
		panic(err)
	}

	rec := httptest.NewRecorder()
	face.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/example", nil))
	fmt.Printf("%d %s\n", rec.Code, rec.Body.String())

	// Output:
	// 200 hello from the mounted route
}
