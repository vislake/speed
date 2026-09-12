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
	"os"

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

// ExampleAssemble drives one component assembly through the eight stages:
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

// ExampleComponentConfig shows how a component's configuration reaches it
// when the engine assembles. The schema declares how each field resolves --
// here an exposed field the command line, the environment and the config
// file may supply, and a derive field that resolves to key material the
// root key derives or a declared default stands in for -- and the engine's
// resolver merges those sources into the block before New runs. Every
// source spells the field's key path: the component declares no namespace,
// so the flag is the bare --zone and the dev-defaults entry is keyed
// "stamp".
func ExampleComponentConfig() {
	type clockConfig struct {
		Zone  string `json:"zone" config:"expose,required"`
		Stamp []byte `json:"stamp" config:"derive"`
	}
	type clock struct{ zone string }
	type hostConfig struct {
		Port string
	}
	host := hostConfig{Port: "8080"}

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "clock",
		ConfigSchema: (*clockConfig)(nil),
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c clockConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return &clock{zone: c.Zone}, nil
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
			// The flag outranks the environment and the config file; the
			// required declaration is satisfied by whichever source supplies
			// the field, and the assembly fails before New runs when none
			// does.
			app.ConfigArgs([]string{"--zone=UTC"}),
			app.ConfigDevDefaults(map[string][]byte{"stamp": make([]byte, 32)}),
		},
		Args: []string{},
	}
	if err := app.Assemble(context.Background(), reg, spec); err != nil {
		fmt.Println("assemble:", err)
		return
	}
	product, err := pkgcore.Get[*clock](reg)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Println("the clock runs in", product.zone)
	if err := app.Shutdown(context.Background(), reg); err != nil {
		fmt.Println("shutdown:", err)
		return
	}
	fmt.Println("shut down cleanly")

	// Output:
	// the clock runs in UTC
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

// exampleListerSchema is the schema ExampleRenderComponentConfigHelp describes:
// an exposed field with a pinned variable name, a derive-tagged sensitive
// key, and a plain block-only field -- the three resolution faces the help
// rendering distinguishes.
type exampleListerSchema struct {
	Addr      string `json:"addr" config:"expose,env=APP_LISTEN_ADDR,group=network"`
	CipherKey []byte `json:"cipher_key" config:"derive,required,sensitive,group=security"`
	CacheTTL  int    `json:"cache_ttl" config:"group=tuning"`
}

// ConfigDocs documents the schema the way the assembly requires a sensitive
// field to be documented; the sensitive field's Default is the cell the
// rendering replaces with the redacted marker.
func (exampleListerSchema) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"cipher_key": {
			Description: "32 bytes the lister seals its cache with",
			Default:     "documented non-secret development default",
		},
	}
}

// ExampleRenderComponentConfigHelp documents the --help rendering of the component
// configuration surface: a host collects the components' schemas -- in a
// binary's --help branch, pkgcore.GlobalComponents(), what the process
// carries -- through the same FieldDescriptor collection the generated
// configuration reference reads, and renders the result, so an operator can
// read what this binary's components take and how each field resolves. A sensitive field's documentation cells
// render as the redacted marker; its description still renders, and the
// prefix argument is the host's own loader prefix, so the environment
// spellings printed are the ones the loader reads.
func ExampleRenderComponentConfigHelp() {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "example.lister",
		ConfigSchema: (*exampleListerSchema)(nil),
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}

	surfaces, err := app.CollectComponentConfig(pkgcore.RegisteredComponents(reg))
	if err != nil {
		fmt.Println("collect:", err)
		return
	}
	// The registry also carries this binary's global registration; render
	// the one component this example registered.
	var lister []app.ComponentConfigSurface
	for _, surface := range surfaces {
		if surface.Name == "example.lister" {
			lister = append(lister, surface)
		}
	}
	if err := app.RenderComponentConfigHelp(os.Stdout, lister, "APP_"); err != nil {
		fmt.Println("render:", err)
		return
	}

	// Output:
	// Component configuration surface (each field collected through
	// pkgcore.DescribeComponentSchema, the same collection the generated
	// configuration reference reads; a key path is the address its flag,
	// environment variable, config-file entry or derivation spells):
	//
	// example.lister
	//   addr  string  group=network
	//       source: flag > environment > config file > the configuration block's value
	//       flag: --addr
	//       env: APP_LISTEN_ADDR (pinned by the declaration)
	//   cipher_key  []byte  derive required sensitive group=security
	//       source: flag > environment > config file > root-key derivation > declared default
	//       flag: --cipher_key
	//       env: APP_CIPHER_KEY
	//       description: 32 bytes the lister seals its cache with
	//       default: [redacted]
	//   cache_ttl  int  group=tuning
	//       source: the component's configuration block only
}
