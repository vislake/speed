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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/vislake/speed/go/app"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite,
	// so this example's DatabaseSpec has a driver to build from. Which
	// dialect packages a binary carries is the assembling application's
	// decision, which is why the engine itself imports none.
	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy"
)

// exampleFailingResolver fails every resolution -- the tenant-less state
// the pre-auth allowlist exists for.
type exampleFailingResolver struct{}

func (exampleFailingResolver) Resolve(*http.Request) (pkgcore.TenantID, error) {
	return "", errors.New("example: no tenant resolvable")
}

// ExampleNew assembles a whole application from the engine's option list and
// serves one request through the composed handler: the host's configuration
// target, the database to open and migrate, and one hand-written route
// mounted beside the module routes. A host that wants the engine to listen
// and drain as well calls Run with the same options and a WithHTTP address.
func ExampleNew() {
	dir, err := os.MkdirTemp("", "app-example")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// The host's configuration target: the host's own keys, one field per
	// variable. The bootstrap keys of the components this binary carries are
	// declared on the components themselves -- a declaration needs no host
	// field -- and resolve on the same loader chain.
	type hostConfig struct {
		Port string `config:"env=EXAMPLE_PORT"`
	}
	host := hostConfig{Port: "8080"}

	a, err := app.New(context.Background(),
		app.WithConfig(
			app.ConfigSpec{Host: &host},
			app.ConfigArgs([]string{}),
			// The declared defaults table stands in for a component key no
			// source supplies: here the config component's declared
			// config.cipher_key, which the engine's infrastructure step
			// builds its platform cipher from. A real deployment supplies it
			// from its environment, a config file or the root-key derivation
			// instead.
			app.ConfigDevDefaults(map[string][]byte{
				"config.cipher_key": make([]byte, 32),
			}),
		),
		app.WithDatabase(app.DatabaseSpec{
			Dialect: dbkit.DialectSQLite,
			DSN:     filepath.Join(dir, "example.db"),
		}),
		app.WithHTTP(app.HTTPSpec{ExtraRoutes: []pkgcore.MountedRoute{{
			Path: "/api/v1/version",
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "v1")
			}),
		}}}),
	)
	if err != nil {
		fmt.Println("assemble:", err)
		return
	}
	defer func() { _ = a.Close(context.Background()) }()

	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/version", nil))
	fmt.Printf("GET /api/v1/version: %d %s\n", rec.Code, rec.Body.String())

	// Output:
	// GET /api/v1/version: 200 v1
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
