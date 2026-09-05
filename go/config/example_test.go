package config_test

import (
	"context"
	"embed"
	"fmt"
	"strings"

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

func (*brandModule) Register(reg *pkgcore.Registry) error {
	if err := reg.Config.Add(pkgcore.ConfigItem{
		Key:         "brand.site_name",
		Type:        "string",
		Default:     "Smile Studio",
		Public:      true,
		Description: "The name shown in the tenant's UI",
		Group:       "brand",
	}); err != nil {
		return err
	}
	return reg.Features.Add(pkgcore.FeatureFlag{
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
// writes straight to a docs/config-reference.md file -- never hand-written,
// per docs/internal/13-documentation-standards.md's must-have doc list.
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
