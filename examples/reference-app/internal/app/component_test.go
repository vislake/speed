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
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin"
	aigateway "github.com/vislake/speed/go/ai-gateway"
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
// order the assembly plans the independent components in.
var hostComponentOrder = []string{
	"reference-app.post_bootstrap",
	"reference-app.post_attach",
	"reference-app.app",
	"reference-app.pre_serve",
	"reference-app.worker",
}

// registerHostComponents registers the host's step components into a fresh
// component registry and puts the composition that selects exactly them
// (strictly, so no globally registered component can be pulled in), which
// is the fixture the plan-shape tests below drive. The wiring components
// are deliberately left out: their requirements name module products this
// fixture does not select, and their own contracts are pinned separately
// (host_wiring_test.go).
func registerHostComponents(t *testing.T) *pkgcore.ComponentRegistry {
	t.Helper()
	b := newServerBuild(ServerConfig{Port: "8080"})
	reg := pkgcore.NewComponentRegistry()
	selection := pkgcore.ComponentConfig{}
	for _, c := range b.hostStepComponents(context.Background(), false) {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %q: %v", c.Name, err)
		}
		selection = selection.With(c.Name, nil)
	}
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

// TestHostComponents_RequiresTheComposedFace pins the pre-serve step's
// requirement as a plan constraint: a composition without the application
// component fails Prepare naming the missing product, and a composition that
// declares the pre-serve step before it still plans the application
// component first -- the edge, not the declaration order, decides.
func TestHostComponents_RequiresTheComposedFace(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	components := b.hostStepComponents(context.Background(), false)
	byName := make(map[string]pkgcore.Component, len(components))
	for _, c := range components {
		byName[c.Name] = c
	}

	t.Run("without the application component", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		selection := pkgcore.ComponentConfig{}
		for _, name := range hostComponentOrder {
			if name == "reference-app.app" {
				continue
			}
			if err := reg.Register(byName[name]); err != nil {
				t.Fatalf("register %q: %v", name, err)
			}
			selection = selection.With(name, nil)
		}
		reg.Put(pkgcore.ComponentConfig{}.With("strict", true).With("components", selection))

		err := reg.Prepare(context.Background())
		if err == nil {
			t.Fatal("Prepare() without the application component succeeded, want the missing-product refusal")
		}
		if !strings.Contains(err.Error(), "reference-app.pre_serve") || !strings.Contains(err.Error(), "hostFace") {
			t.Fatalf("Prepare() error = %v, want it to name the pre-serve step and the missing product", err)
		}
	})

	t.Run("declared back to front", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		selection := pkgcore.ComponentConfig{}
		for i := len(hostComponentOrder) - 1; i >= 0; i-- {
			name := hostComponentOrder[i]
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
		app, preServe := slices.Index(planned, "reference-app.app"), slices.Index(planned, "reference-app.pre_serve")
		if app < 0 || preServe < 0 || app > preServe {
			t.Fatalf("planned order = %v, want the application component before the pre-serve step", planned)
		}
	})
}

// TestHostComponents_DeclareTheirContracts pins what each descriptor
// declares: no host component implements a module, every one carries the New
// callback the registry requires, the application component provides the
// face the pre-serve step requires, and the lifecycle callbacks the faces'
// serving and draining depend on are declared.
func TestHostComponents_DeclareTheirContracts(t *testing.T) {
	b := newServerBuild(ServerConfig{Port: "8080"})
	components := b.hostStepComponents(context.Background(), false)
	if len(components) != len(hostComponentOrder) {
		t.Fatalf("hostComponents() returned %d components, want %d", len(components), len(hostComponentOrder))
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
	if len(app.Provides) != 1 || app.Provides[0] != (*hostFace)(nil) {
		t.Fatalf("the application component Provides = %v, want the single (*hostFace) product", app.Provides)
	}
	if app.Init == nil || app.Start == nil || app.Stop == nil || app.Close == nil {
		t.Fatalf("the application component declares %v, want Init, Start, Stop and Close", app)
	}

	preServe := byName["reference-app.pre_serve"]
	if len(preServe.Requires) != 1 || preServe.Requires[0].Token != (*hostFace)(nil) || preServe.Requires[0].Optional {
		t.Fatalf("the pre-serve step Requires = %v, want one mandatory (*hostFace) requirement", preServe.Requires)
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
// failure paths are pinned over. The typed nils are the products binding
// only reads back -- what the step bodies do with them is the suites' (and
// the whole-application assembly test's) business.
func assembledSeams(withBus, withKV bool) *pkgcore.ComponentRegistry {
	reg := pkgcore.NewComponentRegistry()
	if withBus {
		reg.Put(pkgcore.NewMemoryEventBus())
	}
	if withKV {
		reg.Put(pkgcore.NewMemoryKVStore())
	}
	for _, product := range []any{
		(*gorm.DB)(nil),
		(*jobs.StandaloneQueue)(nil),
		(*config.Module)(nil),
		(*org.Module)(nil),
		(*pki.Module)(nil),
		(*authn.Module)(nil),
		(*notes.Module)(nil),
		(*audit.Module)(nil),
		(*rbac.Module)(nil),
		(*storage.Module)(nil),
		(*sharing.Module)(nil),
		(*integration.Module)(nil),
		(*demomodule.Module)(nil),
		(*notification.Module)(nil),
		(*aigateway.Module)(nil),
		(*billing.Module)(nil),
		(*metering.Module)(nil),
		(*compliance.Module)(nil),
		(*admin.Module)(nil),
		(*attestation.Service)(nil),
		(*config.Service)(nil),
		(*rbac.Service)(nil),
	} {
		reg.Put(product)
	}
	return reg
}

// TestStepInit_DerivesTheViewAndRunsTheBody pins the adapter the host's step
// components share: it hands the step body the view derived from the
// registry, refuses an assembly missing a seam the view needs before the
// body runs, and the face flavor reads the composed face the pre-serve
// step's own requirement provides -- refusing an assembly without one.
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

	face := &hostFace{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	full.Put(face)
	ran = false
	if err := b.stepInitWithFace(func(_ context.Context, _ assemblyView, got *hostFace) error {
		ran = true
		if got != face {
			t.Errorf("stepInitWithFace handed the body %p, want the composed face %p", got, face)
		}
		return nil
	})(ctx, full, nil); err != nil {
		t.Fatalf("stepInitWithFace over a complete assembly: %v", err)
	}
	if !ran {
		t.Fatal("stepInitWithFace did not run its body")
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

	t.Run("the composed face", func(t *testing.T) {
		if err := b.stepInitWithFace(func(context.Context, assemblyView, *hostFace) error {
			t.Fatal("the body ran on an assembly with no composed face")
			return nil
		})(ctx, assembledSeams(true, true), nil); err == nil {
			t.Fatal("stepInitWithFace without a composed face succeeded, want the face refusal")
		}
		if err := b.stepInitWithFace(func(context.Context, assemblyView, *hostFace) error {
			t.Fatal("the body ran on an assembly missing a view seam")
			return nil
		})(ctx, func() *pkgcore.ComponentRegistry {
			reg := assembledSeams(false, false)
			reg.Put(&hostFace{})
			return reg
		}(), nil); err == nil {
			t.Fatal("stepInitWithFace over a face without view seams succeeded, want the seam refusal")
		}
	})
}

// TestAppComponent_RefusesAForeignInstance pins the application component's
// product check: every lifecycle callback reports an instance that is not
// the component's own *hostFace by name instead of panicking on it.
func TestAppComponent_RefusesAForeignInstance(t *testing.T) {
	component := appComponent(newServerBuild(ServerConfig{}), context.Background(), true)
	foreign := &hostStep{}
	for name, callback := range map[string]func(context.Context, *pkgcore.ComponentRegistry, any) error{
		"Init":  component.Init,
		"Start": component.Start,
		"Stop":  component.Stop,
		"Close": component.Close,
	} {
		if err := callback(context.Background(), nil, foreign); err == nil {
			t.Fatalf("the application component's %s callback accepted a %T instance, want the product refusal", name, foreign)
		}
	}
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
		// The overridden modules' locale resources merge unconditionally
		// (they ride no component), so a nil asset list still yields their
		// languages and ids.
		catalog, err := hostCatalog(nil)
		if err != nil {
			t.Fatalf("hostCatalog(nil): %v", err)
		}
		if got := catalog.Locales(); !slices.Equal(got, []string{"en-US", "zh-CN"}) {
			t.Fatalf("catalog languages = %v, want the overridden modules' en-US/zh-CN pair", got)
		}
		if _, err := catalog.Lookup(i18n.LocaleENUS, "authn.invalid_credentials", nil); err != nil {
			t.Fatalf("lookup authn.invalid_credentials: %v, want the overridden module's ids merged", err)
		}
	})
}

// TestHostFace_ServesThenDrains pins the application component's listener
// lifecycle over a plain handler: Start serves the composed face on a real
// listener and records its address, Stop stops accepting and the drain
// completes, and Close reports the drained state. The request path goes
// through the same observability middleware the composed face is served
// behind.
func TestHostFace_ServesThenDrains(t *testing.T) {
	face := &hostFace{
		handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "served")
		}),
		addr:    "127.0.0.1:0",
		baseCtx: context.Background(),
	}
	if err := face.start(context.Background()); err != nil {
		t.Fatalf("start(): %v", err)
	}
	if face.addr == "127.0.0.1:0" {
		t.Fatal("start() did not record the listener's own address")
	}
	if face.Handler() == nil {
		t.Fatal("Handler() = nil after start, want the composed handler")
	}

	resp, err := http.Get("http://" + face.addr + "/")
	if err != nil {
		t.Fatalf("request to the started face: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read the response body: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "served" {
		t.Fatalf("response = %d %q, want the composed handler's 200 \"served\"", resp.StatusCode, body)
	}

	face.stop(context.Background())
	select {
	case <-face.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain Stop began did not finish within the shutdown timeout")
	}
	if _, err := http.Get("http://" + face.addr + "/"); err == nil {
		t.Fatal("the listener still accepts requests after Stop, want the drain to have stopped it")
	}
	if err := face.close(context.Background()); err != nil {
		t.Fatalf("close() after the drain: %v", err)
	}
}

// TestHostFace_ClosesWithoutStop pins the other shutdown order: a face that
// is closed before it ever stopped accepting drains synchronously, and a
// face that never started releases nothing.
func TestHostFace_ClosesWithoutStop(t *testing.T) {
	face := &hostFace{
		handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		addr:    "127.0.0.1:0",
		baseCtx: context.Background(),
	}
	if err := face.start(context.Background()); err != nil {
		t.Fatalf("start(): %v", err)
	}
	if err := face.close(context.Background()); err != nil {
		t.Fatalf("close() without a preceding Stop: %v", err)
	}

	unstarted := &hostFace{}
	if err := unstarted.close(context.Background()); err != nil {
		t.Fatalf("close() on a face that never started: %v", err)
	}
	unstarted.stop(context.Background())
}
