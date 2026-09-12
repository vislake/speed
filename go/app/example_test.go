package app_test

// Runnable documentation for the app package's public API, mirroring
// go/tenancy/example_test.go's convention: every example here is compiled
// and executed by `go test`, so a change to the package's public API that
// breaks the documented usage fails the build instead of only rotting in
// prose. The chain package's own example (go/app/chain/example_test.go)
// documents the middleware composition built on top.

import (
	"context"
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

// ExampleAssemble drives one component assembly through the seven stages:
// the registry is populated from the global registration plus one local
// component, the code-override layer selects what the builtin composition
// defaults do not (and deselects the observability component this example
// does not want), and Assemble runs the loader and the stage drive; Shutdown
// performs the two-phase close.
func ExampleAssemble() {
	type clock struct{}
	type hostConfig struct {
		Port string
	}
	host := hostConfig{Port: "8080"}

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name: "clock",
		// The component declares the bootstrap key it consumes; the engine's
		// loader resolves it on its source chain and publishes the result as
		// the assembly's bootstrap material, which the component (or any
		// later consumer) reads by the declared path.
		BootstrapKeys: []pkgcore.BootstrapKey{{
			Key:         "clock.secret",
			Format:      "hexkey",
			Sensitive:   true,
			Description: "the key material the clock stamps its ticks with",
		}},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &clock{}, nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}
	reg.Put(app.CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With("clock", nil).With("observability", false))})

	spec := app.LoadSpec{
		Host: &host,
		Options: []app.ConfigOption{
			app.ConfigArgs([]string{}),
			// A declared key with no source falls back to the declared
			// defaults table; without an entry it is left unresolved, and a
			// consumer that needs it says so itself.
			app.ConfigDevDefaults(map[string][]byte{"clock.secret": make([]byte, 32)}),
		},
	}
	if err := app.Assemble(context.Background(), reg, spec); err != nil {
		fmt.Println("assemble:", err)
		return
	}
	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		fmt.Println("material:", err)
		return
	}
	_, declared := material.Material("clock.secret")
	fmt.Println("assembled; clock port", host.Port, "declared key resolved:", declared)
	if err := app.Shutdown(context.Background(), reg); err != nil {
		fmt.Println("shutdown:", err)
		return
	}
	fmt.Println("shut down cleanly")

	// Output:
	// assembled; clock port 8080 declared key resolved: true
	// shut down cleanly
}

// ExampleRunAssembly documents the serve-callback shape: RunAssembly drives
// the assembly, hands the live registry to the host's serve step, and runs
// the two-beat close once that step returns. The example's one-component
// serve step reads its product from the registry and returns right away, so
// the run terminates without a signal; a real host's serve step holds
// until the signalled end. Passing nil for the callback is the
// no-serve-step shape: the engine waits the context out itself.
func ExampleRunAssembly() {
	type clock struct{}
	type hostConfig struct {
		Port string
	}
	host := hostConfig{Port: "8080"}

	component := pkgcore.Component{
		Name: "clock",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &clock{}, nil
		},
	}

	err := app.RunAssembly(context.Background(), app.LoadSpec{
		Host: &host,
		Options: []app.ConfigOption{
			app.ConfigArgs([]string{}),
			app.ConfigDevDefaults(map[string][]byte{}),
		},
		Overrides: &app.CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
			pkgcore.ComponentConfig{}.With("clock", nil).With("observability", false))},
	}, func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
		if _, err := pkgcore.Get[*clock](reg); err != nil {
			return err
		}
		fmt.Println("serving the clock on port", host.Port)
		return nil
	}, component)
	if err != nil {
		fmt.Println("run:", err)
		return
	}
	fmt.Println("shut down cleanly")

	// Output:
	// serving the clock on port 8080
	// shut down cleanly
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
