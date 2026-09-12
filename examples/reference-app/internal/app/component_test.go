package app

// This file pins the host's own component shapes: the descriptors' declared
// contracts and the plan order they produce, the message catalog the
// assembly merges its components' locale resources into, and the
// application component's listener lifecycle. The step bodies are exercised
// end to end by the suites that drive BuildServer, so a step's correctness
// is pinned once, on the one implementation both drives run.

import (
	"context"
	"embed"
	"slices"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/app/httpserve"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/integration"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/sharing"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/app/testdata/hostcatalog/alpha"
	"github.com/vislake/speed/examples/reference-app/internal/app/testdata/hostcatalog/beta"
	"github.com/vislake/speed/examples/reference-app/internal/attestation"
	demomodule "github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
)

// hostComponentOrder is the order hostComponents must return, which is the
// order the assembly plans the independent components in; the http
// component plans between the link-policy provider (its required
// dependency) and the pre-serve step (which requires its route face).
var hostComponentOrder = []string{
	"reference-app.post_bootstrap",
	"reference-app.post_attach",
	"reference-app.app",
	"http",
	"reference-app.pre_serve",
	"reference-app.worker",
}

// hostStepOrder is the order the host's own step components plan in.
var hostStepOrder = []string{
	"reference-app.post_bootstrap",
	"reference-app.post_attach",
	"reference-app.app",
	"reference-app.pre_serve",
	"reference-app.worker",
}

// registerHostComponents registers the host's step components -- plus the
// http component the pre-serve step's route requirement names -- into a
// fresh component registry and puts the composition that selects exactly
// them (strictly, so no globally registered component can be pulled in),
// which is the fixture the plan-shape tests below drive. The wiring
// components are deliberately left out: their requirements name module
// products this fixture does not select, and their own contracts are pinned
// separately (host_wiring_test.go).
func registerHostComponents(t *testing.T) *pkgcore.ComponentRegistry {
	t.Helper()
	b := newServerBuild(ServerConfig{Port: "8080"})
	reg := pkgcore.NewComponentRegistry()
	selection := pkgcore.ComponentConfig{}
	for _, c := range b.hostStepComponents() {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %q: %v", c.Name, err)
		}
		selection = selection.With(c.Name, nil)
	}
	if _, ok := pkgcore.LookupComponent(reg, httpserve.ComponentName); !ok {
		t.Fatal("the http component is not registered")
	}
	// The instance was seeded from the global registration, so the http
	// component is already registered: the composition only selects it.
	selection = selection.With(httpserve.ComponentName, nil)
	reg.Put(pkgcore.ComponentConfig{}.With("strict", true).With("components", selection))
	return reg
}

// TestHostComponents_PlanInRegistrationOrder pins the descriptors' plan
// shape: the five components assemble in the declared order -- the app
// component ahead of the pre-serve step, which requires its product -- every
// product constructs (so each descriptor's Provides declaration matches what
// its New returns), and the assembly closes cleanly without ever starting.
func TestHostComponents_PlanInRegistrationOrder(t *testing.T) {
	reg := registerHostComponents(t)
	ctx := context.Background()

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() over the host components: %v", err)
	}
	if got := pkgcore.MemberNames(reg, ""); !slices.Equal(got, hostComponentOrder) {
		t.Fatalf("the planned host components = %v, want the declared order %v", got, hostComponentOrder)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() over the host components: %v", err)
	}
	if err := reg.Verify(ctx); err != nil {
		t.Fatalf("Verify() over the host components: %v", err)
	}
	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("Stop() over an unstarted assembly: %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("Close() over the host components: %v", err)
	}
}

// TestHostComponents_RequiresTheRouteFace pins the plan constraints the
// host's declarations create: a composition without the http component
// fails Prepare naming the pre-serve step's missing route face, a
// composition without the link-policy provider fails naming the http
// component's missing policy, and a composition that declares the steps
// back to front still plans the dependency edges' order -- the edges, not
// the declaration order, decide.
func TestHostComponents_RequiresTheRouteFace(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	components := b.hostStepComponents()
	byName := make(map[string]pkgcore.Component, len(components))
	for _, c := range components {
		byName[c.Name] = c
	}
	if _, ok := pkgcore.LookupComponent(pkgcore.NewComponentRegistry(), httpserve.ComponentName); !ok {
		t.Fatal("the http component is not registered")
	}

	t.Run("without the http component", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		selection := pkgcore.ComponentConfig{}
		for _, name := range hostStepOrder {
			if err := reg.Register(byName[name]); err != nil {
				t.Fatalf("register %q: %v", name, err)
			}
			selection = selection.With(name, nil)
		}
		reg.Put(pkgcore.ComponentConfig{}.With("strict", true).With("components", selection))

		err := reg.Prepare(context.Background())
		if err == nil {
			t.Fatal("Prepare() without the http component succeeded, want the missing-route-face refusal")
		}
		if !strings.Contains(err.Error(), "reference-app.pre_serve") || !strings.Contains(err.Error(), "RouteRegistrar") {
			t.Fatalf("Prepare() error = %v, want it to name the pre-serve step and the missing route face", err)
		}
	})

	t.Run("without the link-policy provider", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		selection := pkgcore.ComponentConfig{}
		for _, name := range hostStepOrder {
			if name == "reference-app.app" {
				continue
			}
			if err := reg.Register(byName[name]); err != nil {
				t.Fatalf("register %q: %v", name, err)
			}
			selection = selection.With(name, nil)
		}
		selection = selection.With(httpserve.ComponentName, nil)
		reg.Put(pkgcore.ComponentConfig{}.With("strict", true).With("components", selection))

		err := reg.Prepare(context.Background())
		if err == nil {
			t.Fatal("Prepare() without the link-policy provider succeeded, want the missing-policy refusal")
		}
		if !strings.Contains(err.Error(), "http") || !strings.Contains(err.Error(), "LinkPolicy") {
			t.Fatalf("Prepare() error = %v, want it to name the http component and the missing policy", err)
		}
	})

	t.Run("declared back to front", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		selection := pkgcore.ComponentConfig{}
		for i := len(hostComponentOrder) - 1; i >= 0; i-- {
			name := hostComponentOrder[i]
			if name == httpserve.ComponentName {
				// Seeded from the global registration already.
				selection = selection.With(name, nil)
				continue
			}
			if err := reg.Register(byName[name]); err != nil {
				t.Fatalf("register %q: %v", name, err)
			}
			selection = selection.With(name, nil)
		}
		reg.Put(pkgcore.ComponentConfig{}.With("strict", true).With("components", selection))

		if err := reg.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare() over the reverse-declared composition: %v", err)
		}
		planned := pkgcore.MemberNames(reg, "")
		app := slices.Index(planned, "reference-app.app")
		httpIdx := slices.Index(planned, "http")
		preServe := slices.Index(planned, "reference-app.pre_serve")
		if app < 0 || httpIdx < 0 || preServe < 0 || app >= httpIdx || httpIdx >= preServe {
			t.Fatalf("planned order = %v, want the link policy before the http component before the pre-serve step", planned)
		}
	})
}

// TestHostComponents_DeclareTheirContracts pins what each descriptor
// declares: no host component implements a module, every one carries the New
// callback the registry requires, the link-policy component provides the
// policy the http component requires, and the Init callbacks the steps'
// bodies run in are declared.
func TestHostComponents_DeclareTheirContracts(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	components := b.hostStepComponents()
	if len(components) != len(hostStepOrder) {
		t.Fatalf("hostStepComponents() returned %d components, want %d", len(components), len(hostStepOrder))
	}
	byName := make(map[string]pkgcore.Component, len(components))
	for _, c := range components {
		byName[c.Name] = c
		if c.Module != "" {
			t.Fatalf("component %q declares module %q, want a host step that implements no module", c.Name, c.Module)
		}
		if c.New == nil {
			t.Fatalf("component %q declares no New callback", c.Name)
		}
	}

	app := byName["reference-app.app"]
	if len(app.Provides) != 1 || app.Provides[0] != (*httpserve.LinkPolicy)(nil) {
		t.Fatalf("the link-policy component Provides = %v, want the single (*httpserve.LinkPolicy) product", app.Provides)
	}
	if app.Init != nil {
		t.Fatalf("the link-policy component declares %v; the policy's content is filled by the pre-serve step, whose plan position is ordered after the runtime services exist", app)
	}
	if app.Start != nil || app.Stop != nil || app.Close != nil || app.Serve != nil {
		t.Fatalf("the link-policy component declares %v; the listener's lifecycle belongs to the http component", app)
	}

	preServe := byName["reference-app.pre_serve"]
	if len(preServe.Requires) != 2 ||
		preServe.Requires[0].Token != (*runtimeServicesSeat)(nil) || preServe.Requires[0].Optional ||
		preServe.Requires[1].Token != (*pkgcore.RouteRegistrar)(nil) || preServe.Requires[1].Optional {
		t.Fatalf("the pre-serve step Requires = %v, want the mandatory runtime-services seat and route face", preServe.Requires)
	}

	for _, name := range []string{"reference-app.post_bootstrap", "reference-app.post_attach", "reference-app.pre_serve"} {
		if c := byName[name]; c.Init == nil {
			t.Fatalf("step %q declares no Init callback, so its body would never run", name)
		}
	}
	if worker := byName["reference-app.worker"]; worker.Close == nil {
		t.Fatalf("the worker component declares %v, want its Close", worker)
	}
}

// assembledSeams returns a registry carrying the seam values a step's view
// derivation and registry binding need, in the combinations the adapter's
// failure paths are pinned over. The placeholders are zero-value instances
// of each product's own type: the binding only reads them back, and a
// registry value must be non-nil. What the step bodies do with them is the
// suites' (and the whole-application assembly test's) business.
func assembledSeams(withBus, withKV bool) *pkgcore.ComponentRegistry {
	reg := pkgcore.NewComponentRegistry()
	if withBus {
		reg.Put(pkgcore.NewMemoryEventBus())
	}
	if withKV {
		reg.Put(pkgcore.NewMemoryKVStore())
	}
	for _, product := range []any{
		new(gorm.DB),
		new(jobs.StandaloneQueue),
		new(config.Module),
		new(org.Module),
		new(pki.Module),
		new(authn.Module),
		new(notes.Module),
		new(audit.Module),
		new(rbac.Module),
		new(storage.Module),
		new(sharing.Module),
		new(integration.Module),
		new(demomodule.Module),
		new(notification.Module),
		new(aigateway.Module),
		new(billing.Module),
		new(metering.Module),
		new(compliance.Module),
		new(admin.Module),
		new(attestation.Service),
		new(config.Service),
		new(rbac.Service),
	} {
		reg.Put(product)
	}
	return reg
}

// TestStepInit_DerivesTheViewAndRunsTheBody pins the adapter the host's step
// components share: it hands the step body the view derived from the
// registry, refuses an assembly missing a seam the view needs before the
// body runs, and the registrar flavor mounts the route face the pre-serve
// step's own requirement provides onto the seed handler -- refusing an
// assembly without a route face.
func TestStepInit_DerivesTheViewAndRunsTheBody(t *testing.T) {
	b := newServerBuild(ServerConfig{})
	ctx := context.Background()

	full := assembledSeams(true, true)
	ran := false
	if err := b.stepInit(func(context.Context, assemblyView) error {
		ran = true
		return nil
	})(ctx, full, nil); err != nil {
		t.Fatalf("stepInit over a complete assembly: %v", err)
	}
	if !ran {
		t.Fatal("stepInit did not run its body")
	}

	t.Run("view seams", func(t *testing.T) {
		if err := b.stepInit(func(context.Context, assemblyView) error {
			t.Fatal("the step body ran on an assembly missing both view seams")
			return nil
		})(ctx, assembledSeams(false, false), nil); err == nil {
			t.Fatal("stepInit without a bus or a store succeeded, want the seam refusal")
		}
		if err := b.stepInit(func(context.Context, assemblyView) error {
			t.Fatal("the step body ran on an assembly missing the key-value store")
			return nil
		})(ctx, assembledSeams(true, false), nil); err == nil {
			t.Fatal("stepInit without a key-value store succeeded, want the seam refusal")
		}
	})
}

// TestWorkerComponent_ClosesAnUnstartedBuild pins the worker component's
// Close callback: its steps run over a build whose resources never started
// -- and over a registry carrying no runtime service to close -- so an
// assembly that failed before Start still closes cleanly.
func TestWorkerComponent_ClosesAnUnstartedBuild(t *testing.T) {
	b := newServerBuild(ServerConfig{})
	if err := b.workerComponent().Close(context.Background(), pkgcore.NewComponentRegistry(), &hostStep{}); err != nil {
		t.Fatalf("the worker component's Close over an unstarted build: %v", err)
	}
}

// TestHostCatalog_MergesEveryComponentsAssets pins the catalog merge: each
// asset contributes its locale files under its own name, so ids from two
// components resolve in one catalog that serves exactly the languages the
// assets ship.
func TestHostCatalog_MergesEveryComponentsAssets(t *testing.T) {
	catalog, err := hostCatalog([]pkgcore.Asset{
		{Name: "alpha", Locales: alpha.FS},
		{Name: "beta", Locales: beta.FS},
	})
	if err != nil {
		t.Fatalf("hostCatalog() over two locale assets: %v", err)
	}
	if got := catalog.Locales(); !slices.Equal(got, []string{"en-US", "zh-CN"}) {
		t.Fatalf("catalog languages = %v, want the shipped en-US/zh-CN pair", got)
	}

	english, err := catalog.Lookup(i18n.LocaleENUS, "alpha.greeting", nil)
	if err != nil {
		t.Fatalf("lookup alpha.greeting (en-US): %v", err)
	}
	chinese, err := catalog.Lookup(i18n.LocaleZHCN, "alpha.greeting", nil)
	if err != nil {
		t.Fatalf("lookup alpha.greeting (zh-CN): %v", err)
	}
	if english == chinese || !strings.Contains(chinese, "alpha") {
		t.Fatalf("alpha.greeting = %q (en-US) / %q (zh-CN), want the two languages' own renderings", english, chinese)
	}
	if _, err := catalog.Lookup(i18n.LocaleENUS, "beta.notice", nil); err != nil {
		t.Fatalf("lookup beta.notice: %v, want the second component's ids merged too", err)
	}
}

// TestHostCatalog_Refusals pins the merge's failure paths: an asset whose
// message ids do not carry the name it is registered under fails naming the
// component, the same name twice fails, and an asset with no locale
// resources is skipped rather than treated as a component with messages.
func TestHostCatalog_Refusals(t *testing.T) {
	t.Run("ids missing the component name", func(t *testing.T) {
		_, err := hostCatalog([]pkgcore.Asset{{Name: "gamma", Locales: alpha.FS}})
		if err == nil {
			t.Fatal("hostCatalog() with an asset whose ids carry another prefix succeeded, want the prefix refusal")
		}
		if !strings.Contains(err.Error(), "gamma") {
			t.Fatalf("hostCatalog() error = %v, want it to name the component", err)
		}
	})

	t.Run("the same component twice", func(t *testing.T) {
		_, err := hostCatalog([]pkgcore.Asset{
			{Name: "alpha", Locales: alpha.FS},
			{Name: "alpha", Locales: alpha.FS},
		})
		if err == nil || !strings.Contains(err.Error(), "alpha") {
			t.Fatalf("hostCatalog() error = %v, want the duplicate-component refusal naming alpha", err)
		}
	})

	t.Run("no locale assets", func(t *testing.T) {
		// The dotted name would be refused by the builder itself, so the
		// clean merge proves the asset was skipped, not accepted.
		catalog, err := hostCatalog([]pkgcore.Asset{
			{Name: "no.locales", Locales: embed.FS{}},
			{Name: "alpha", Locales: alpha.FS},
		})
		if err != nil {
			t.Fatalf("hostCatalog() over an asset carrying no locale resources: %v", err)
		}
		if got := catalog.Locales(); !slices.Equal(got, []string{"en-US", "zh-CN"}) {
			t.Fatalf("catalog languages = %v, want only the locale-carrying asset's", got)
		}
	})

	t.Run("no assets at all", func(t *testing.T) {
		// Locale resources ride the selected components' descriptors --
		// override copies included -- so an empty asset list merges
		// nothing: there is no second, unconditional source of messages.
		catalog, err := hostCatalog(nil)
		if err != nil {
			t.Fatalf("hostCatalog(nil): %v", err)
		}
		if got := catalog.Locales(); len(got) != 0 {
			t.Fatalf("catalog languages = %v, want none without any asset", got)
		}
	})

	t.Run("a module prefix over a renamed component", func(t *testing.T) {
		// The id prefix is the MODULE the component implements: an override
		// component (host name, module's own resources) merges under the
		// module name, exactly as the module's own descriptor would.
		catalog, err := hostCatalog([]pkgcore.Asset{
			{Name: "test-host.alpha", Module: "alpha", Locales: alpha.FS},
		})
		if err != nil {
			t.Fatalf("hostCatalog() over a renamed component carrying its module's locales: %v", err)
		}
		if _, err := catalog.Lookup(i18n.LocaleENUS, "alpha.greeting", nil); err != nil {
			t.Fatalf("lookup alpha.greeting: %v, want the renamed component's resources merged under the module name", err)
		}
	})
}
