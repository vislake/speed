package config_test

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so every dbkit.Open call in this package's test binary (this file and
	// its siblings -- http_test.go, model_test.go, module_test.go,
	// service_test.go, store_test.go) has a driver to build from. One
	// package-wide import suffices, since go test links every *_test.go
	// file in this directory into a single binary.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// brandModule is a business module in the shape every speed module takes:
// it declares its configuration items and feature flags into the registry
// during Register, and owns no configuration machinery of its own. The
// config module folds these declarations, together with every other
// module's, into the one schema it serves.
type brandModule struct{}

var _ pkgcore.Module = (*brandModule)(nil)

func (*brandModule) Name() string         { return "brand" }
func (*brandModule) DependsOn() []string  { return nil }
func (*brandModule) Migrations() embed.FS { return embed.FS{} }
func (*brandModule) Locales() embed.FS    { return embed.FS{} }
func (*brandModule) OpenAPISpec() []byte  { return nil }

func (*brandModule) Register(reg pkgcore.Registrar) error {
	if err := reg.ConfigSeat().Add(pkgcore.ConfigItem{
		Key:         "brand.site_name",
		Type:        "string",
		Default:     "Smile Studio",
		Public:      true,
		Description: "The name shown in the tenant's UI",
		Group:       "brand",
	}); err != nil {
		return err
	}
	return reg.FeaturesSeat().Add(pkgcore.FeatureFlag{
		Key:         "brand.custom_theme",
		Default:     false,
		Description: "Lets a tenant override the palette",
	})
}

// Example shows the module's headline path end to end: a host bootstraps
// the config module beside its business modules, calls Attach exactly once
// to freeze the assembled schema, and then reads values that fall back
// system-to-tenant. The platform default is served until a tenant-scoped
// write overrides it for that tenant alone.
func Example() {
	ctx := context.Background()

	// Standalone deployment mode: in-memory SQLite, no external services.
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:config_example?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}

	// The config module owns the configs table, so its migrations must be
	// applied before Attach reads or writes anything.
	configModule := config.NewModule(db, config.WithPollInterval(0))
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(configModule); err != nil {
		panic(err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}

	// Bootstrap walks the module graph, calling Register on each; Attach is
	// called exactly once afterwards and freezes the union of everything
	// declared into a schema the service serves.
	reg, err := pkgcore.NewKernel().
		Bootstrap(ctx, &brandModule{}, configModule)
	if err != nil {
		panic(err)
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	// With nothing written, the platform default declared by brandModule is
	// what every tenant sees.
	name, err := config.GetTyped[string](svc, ctx, "brand.site_name")
	if err != nil {
		panic(err)
	}
	fmt.Println("default:", name)

	// A tenant-scoped write overrides that value for one tenant only.
	tenantCtx := pkgcore.WithTenant(ctx, "acme")
	if err = svc.Set(tenantCtx, config.ScopeTenant, "brand.site_name",
		config.Value{Data: "Acme Dental"}, "alice"); err != nil {
		panic(err)
	}
	tenantName, err := config.GetTyped[string](svc, tenantCtx, "brand.site_name")
	if err != nil {
		panic(err)
	}
	fmt.Println("acme:", tenantName)

	// Feature flags resolve through the same schema and scope tiers.
	enabled, err := svc.IsEnabled(tenantCtx, "brand.custom_theme")
	if err != nil {
		panic(err)
	}
	fmt.Println("custom_theme:", enabled)

	// Output:
	// default: Smile Studio
	// acme: Acme Dental
	// custom_theme: false
}

// ExampleService_Describe shows the two pieces that together produce a
// generated configuration reference: Describe reads back every item and
// feature flag the frozen schema carries (brandModule's own declarations,
// here), and RenderMarkdown turns that into the Markdown table a host
// writes straight to a docs/config-reference.md file -- the reference is
// generated from the live schema, never hand-written.
func ExampleService_Describe() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:config_describe_example?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}
	configModule := config.NewModule(db, config.WithPollInterval(0))
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(configModule); err != nil {
		panic(err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, &brandModule{}, configModule)
	if err != nil {
		panic(err)
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	for _, item := range svc.Describe() {
		fmt.Printf("%s: type=%s public=%v has_default=%v\n", item.Key, item.Type, item.Public, item.HasDefault)
	}

	rendered := config.RenderMarkdown(svc.Describe())
	fmt.Println(strings.Contains(rendered, "| `brand.site_name` |"))

	// Output:
	// brand.custom_theme: type=bool public=false has_default=true
	// brand.site_name: type=string public=true has_default=true
	// true
}

// shareExpiryModule declares one duration-valued config item, the shape a
// tenant-configurable duration seam (a share link's expiry default, an
// export's delivery window) reads through Service.TenantDuration.
type shareExpiryModule struct{}

var _ pkgcore.Module = (*shareExpiryModule)(nil)

func (*shareExpiryModule) Name() string         { return "share-expiry" }
func (*shareExpiryModule) DependsOn() []string  { return nil }
func (*shareExpiryModule) Migrations() embed.FS { return embed.FS{} }
func (*shareExpiryModule) Locales() embed.FS    { return embed.FS{} }
func (*shareExpiryModule) OpenAPISpec() []byte  { return nil }

func (*shareExpiryModule) Register(reg pkgcore.Registrar) error {
	return reg.ConfigSeat().Add(pkgcore.ConfigItem{
		Key:         "share.default_expiry",
		Type:        "duration",
		Default:     24 * time.Hour,
		Description: "How long a public share link stays valid",
		Group:       "share",
	})
}

// ExampleService_TenantDuration shows the read a tenant-configurable
// duration seam is built from: with no explicit row the schema default is
// reported as UNCONFIGURED (ok=false), so the caller applies its own
// fallback rather than mistaking the default for a configured value; a
// system row then serves every tenant, and a tenant row wins for that
// tenant alone. The tenant comes from the parameter, never from ctx.
func ExampleService_TenantDuration() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:config_example_tenant_duration?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}

	configModule := config.NewModule(db, config.WithPollInterval(0))
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(configModule); err != nil {
		panic(err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}
	reg, err := pkgcore.NewKernel().Bootstrap(ctx, &shareExpiryModule{}, configModule)
	if err != nil {
		panic(err)
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	_, configured, durErr := svc.TenantDuration(ctx, "share.default_expiry", "acme")
	if durErr != nil {
		panic(durErr)
	}
	if configured {
		panic("expected an unconfigured duration before any row exists")
	}
	fmt.Println("before any row: unconfigured")

	// A system row is the platform-wide fallback every tenant resolves to.
	sysCtx, err := pkgcore.WithSystemContext(ctx, pkgcore.SystemReason{
		Actor:   "example-bootstrap",
		Purpose: config.SystemPurposeSystemWrite,
	})
	if err != nil {
		panic(err)
	}
	if err = svc.Set(sysCtx, config.ScopeSystem, "share.default_expiry",
		config.Value{Data: 2 * time.Hour}, "ops-1"); err != nil {
		panic(err)
	}
	systemExpiry, configured, durErr := svc.TenantDuration(ctx, "share.default_expiry", "acme")
	if durErr != nil {
		panic(durErr)
	}
	if !configured {
		panic("expected the system row to be reported as configured")
	}
	fmt.Println("system row:", systemExpiry)

	// A tenant row overrides it for that tenant alone.
	tenantCtx := pkgcore.WithTenant(ctx, "acme")
	if err = svc.Set(tenantCtx, config.ScopeTenant, "share.default_expiry",
		config.Value{Data: 30 * time.Minute}, "alice"); err != nil {
		panic(err)
	}
	tenantExpiry, configured, durErr := svc.TenantDuration(ctx, "share.default_expiry", "acme")
	if durErr != nil || !configured {
		panic(durErr)
	}
	fmt.Println("acme's own row:", tenantExpiry)

	otherExpiry, configured, durErr := svc.TenantDuration(ctx, "share.default_expiry", "globex")
	if durErr != nil || !configured {
		panic(durErr)
	}
	fmt.Println("globex still sees:", otherExpiry)

	// Output:
	// before any row: unconfigured
	// system row: 2h0m0s
	// acme's own row: 30m0s
	// globex still sees: 2h0m0s
}

// ExampleModule_Handle shows the lazy read handle: a host captures it while
// assembling, long before the Service exists, and reads through it after
// Attach. A read in the window before Attach fails closed with the module's
// coded not-attached error rather than a zero-value answer; the same handle
// then serves the real reads.
func ExampleModule_Handle() {
	ctx := context.Background()

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     "file:config_example_handle?mode=memory&cache=shared",
	})
	if err != nil {
		panic(err)
	}

	configModule := config.NewModule(db, config.WithPollInterval(0))
	migrations := dbkit.NewMigrationRegistry()
	if err = migrations.Register(configModule); err != nil {
		panic(err)
	}
	if err = migrations.Apply(ctx, db, dbkit.DialectSQLite); err != nil {
		panic(err)
	}

	// A seam that reads configuration is wired with the handle here --
	// before Bootstrap, when no Service exists yet.
	handle := configModule.Handle()
	if _, err = handle.IsEnabled(ctx, "brand.custom_theme"); err != nil {
		appErr, _ := apperr.As(err)
		fmt.Println("before Attach:", appErr.Code)
	}

	reg, err := pkgcore.NewKernel().Bootstrap(ctx, &brandModule{}, configModule)
	if err != nil {
		panic(err)
	}
	svc, err := configModule.Attach(reg)
	if err != nil {
		panic(err)
	}
	defer svc.Close()

	enabled, err := handle.IsEnabled(ctx, "brand.custom_theme")
	if err != nil {
		panic(err)
	}
	fmt.Println("after Attach:", enabled)

	// Output:
	// before Attach: config.service_not_attached
	// after Attach: false
}
