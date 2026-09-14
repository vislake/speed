package db_test

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// pluginRecorder records the order the plugins were initialised in, which is
// the order they were installed: GORM calls Initialize as part of Use.
type pluginRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *pluginRecorder) record(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, name)
}

// recorded copies the order out.
func (r *pluginRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.order)
}

// recordingPlugin is a plugin that reports when it is installed. Reporting is
// optional: a case about which plugins arrived has nothing to say about order.
type recordingPlugin struct {
	name string
	into *pluginRecorder
}

func (p recordingPlugin) Name() string { return p.name }

func (p recordingPlugin) Initialize(*gorm.DB) error {
	if p.into != nil {
		p.into.record(p.name)
	}
	return nil
}

// pluginModule is a module that declares plugins and nothing else, which is the
// shape of a module bringing a cross-cutting capability that needs a concept
// from higher up the dependency graph.
func pluginModule(name string, plugins ...gorm.Plugin) core.Module {
	resources := make([]any, 0, len(plugins))
	for _, plugin := range plugins {
		resources = append(resources, db.Plugin{Plugin: plugin})
	}
	return core.Module{Name: name, Resources: resources}
}

// installedOn lists the plugins on a handle, sorted, so a case compares sets
// rather than the map's iteration order.
func installedOn(handle *gorm.DB) []string {
	names := make([]string, 0, len(handle.Plugins))
	for name := range handle.Plugins {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// migrationSet builds a migration declaration holding the given files under the
// dialect's subdirectory.
func migrationSet(dialect db.Dialect, files map[string]string) db.Migrations {
	tree := fstest.MapFS{}
	for name, body := range files {
		tree[path.Join(string(dialect), name)] = &fstest.MapFile{Data: []byte(body)}
	}
	return db.Migrations{FS: tree}
}

// pluginObserver is a module that takes up the database capability the way a
// dependant does, and reports the plugins on the handle at the moment it
// resolves it.
//
// The moment is the whole point. A dependant reads the handle inside its own
// New, so an implementation that installed the plugins later — after the
// capability was delivered, or in a stage after construction — is caught here,
// where a case that looked at the handle once the host had started would see
// them whichever way round it was.
type pluginObserver struct {
	seen chan []string
}

func (o *pluginObserver) module() core.Module {
	return core.Module{
		Name:     "observer",
		Requires: []core.Requirement{{Token: (*db.Database)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			instance, err := core.Resolve[db.Database](reg)
			if err != nil {
				return nil, err
			}
			o.seen <- installedOn(instance.DB())
			return nil, nil
		},
	}
}

// TestDependantSeesEveryPluginAtResolveTime pins the timing of the
// installation: a dependant that takes the capability up has a handle whose
// declared plugins are already installed on it.
//
// GORM keeps the plugin table per handle, so an implementation that installed
// them after delivery leaves a dependant holding a handle with an empty one —
// and a statement issued through it runs unfiltered, unaudited, and reports
// nothing. "A statement still runs" is exactly what such an implementation
// looks like from the outside, which is why this case reads the plugin table
// itself, at resolve time rather than after the startup.
func TestDependantSeesEveryPluginAtResolveTime(t *testing.T) {
	observer := &pluginObserver{seen: make(chan []string, 1)}
	h, err := startHost(t, sqliteSpec(), hostConfig(t),
		pluginModule("audit", recordingPlugin{name: "audit-plugin"}),
		pluginModule("catalog", recordingPlugin{name: "catalog-plugin"}),
		observer.module(),
	)
	if err != nil {
		t.Fatalf("starting a host with declared plugins: %v", err)
	}

	want := []string{"audit-plugin", "catalog-plugin"}
	if got := <-observer.seen; !slices.Equal(got, want) {
		t.Errorf("a dependant resolving the capability saw the plugins %v, want %v; every statement it "+
			"issues on that handle runs without them", got, want)
	}
	if got := installedOn(h.Instance.DB()); !slices.Equal(got, want) {
		t.Errorf("the delivered handle carries the plugins %v, want %v", got, want)
	}
}

// TestOpenedHandleCarriesTheSameAssembly pins that a connection built with Open
// carries the declared plugins too.
//
// A handle that came back from a plain gorm.Open would satisfy every observable
// the delivered one does — the same pool parameters, a pool of its own, and
// statements that run — and be missing exactly the part that does not announce
// itself: tenant filtering and audit capture go away, and the query that
// follows returns rows it should not.
//
// The expectation is the declared set rather than the delivered handle's, so
// that installing nothing at all cannot satisfy the case by making both sides
// agree on an empty list.
func TestOpenedHandleCarriesTheSameAssembly(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t),
		pluginModule("audit", recordingPlugin{name: "audit-plugin"}),
		pluginModule("catalog", recordingPlugin{name: "catalog-plugin"}),
	)
	if err != nil {
		t.Fatalf("starting a host with declared plugins: %v", err)
	}

	opened, err := h.Instance.Open(t.Context(), filepath.Join(t.TempDir(), "elsewhere.db"))
	if err != nil {
		t.Fatalf("building a second connection: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(opened); err != nil {
			t.Errorf("releasing the second connection: %v", err)
		}
	})

	want := []string{"audit-plugin", "catalog-plugin"}
	if got := installedOn(h.Instance.DB()); !slices.Equal(got, want) {
		t.Fatalf("the delivered handle carries the plugins %v, want %v", got, want)
	}
	if got := installedOn(opened); !slices.Equal(got, want) {
		t.Errorf("the connection built with Open carries the plugins %v, want %v; the caller moved a "+
			"query to another database and lost what the declarations do to it", got, want)
	}
}

// orderingCapability stands for whatever capability one module takes up from
// another. It carries the dependency edge that decides which of two plugins is
// installed first.
type orderingCapability interface{ ordered() }

// orderingProduct is what the module providing it constructs.
type orderingProduct struct{}

func (orderingProduct) ordered() {}

// TestPluginsInstallInDependencyOrder pins the order the plugins go on in.
//
// Install order is callback order in GORM: two plugins registered to the same
// processor run in the order they were installed, so which of two modules acts
// first on a statement has to be decidable, and the dependency declarations are
// what decide it. The second case runs against the names on purpose — "alpha"
// depends on "zeta", so zeta's plugin goes on first even though its name comes
// last.
func TestPluginsInstallInDependencyOrder(t *testing.T) {
	t.Run("a dependency decides, even against the module names", func(t *testing.T) {
		order := &pluginRecorder{}
		zeta := pluginModule("zeta", recordingPlugin{name: "zeta-plugin", into: order})
		zeta.Provides = []core.Provision{{Token: (*orderingCapability)(nil)}}
		zeta.New = func(context.Context, *core.Registry) (any, error) { return orderingProduct{}, nil }

		alpha := pluginModule("alpha", recordingPlugin{name: "alpha-plugin", into: order})
		alpha.Requires = []core.Requirement{{Token: (*orderingCapability)(nil)}}

		h, err := startHost(t, sqliteSpec(), hostConfig(t), alpha, zeta)
		if err != nil {
			t.Fatalf("starting a host with dependent declarations: %v", err)
		}

		// Both on the handle, so the order below is an order between two
		// installations rather than between one and nothing.
		if got, want := installedOn(h.Instance.DB()), []string{"alpha-plugin", "zeta-plugin"}; !slices.Equal(got, want) {
			t.Fatalf("the delivered handle carries the plugins %v, want %v", got, want)
		}
		if got, want := order.recorded(), []string{"zeta-plugin", "alpha-plugin"}; !slices.Equal(got, want) {
			t.Errorf("the plugins were installed as %v, want %v: the dependant's plugin acts on a "+
				"statement before the plugin of the module it depends on", got, want)
		}
	})

	t.Run("without a dependency the module name decides", func(t *testing.T) {
		order := &pluginRecorder{}
		h, err := startHost(t, sqliteSpec(), hostConfig(t),
			pluginModule("catalog", recordingPlugin{name: "catalog-plugin", into: order}),
			pluginModule("audit", recordingPlugin{name: "audit-plugin", into: order}),
		)
		if err != nil {
			t.Fatalf("starting a host with independent declarations: %v", err)
		}

		if got, want := installedOn(h.Instance.DB()), []string{"audit-plugin", "catalog-plugin"}; !slices.Equal(got, want) {
			t.Fatalf("the delivered handle carries the plugins %v, want %v", got, want)
		}
		if got, want := order.recorded(), []string{"audit-plugin", "catalog-plugin"}; !slices.Equal(got, want) {
			t.Errorf("the plugins were installed as %v, want %v: with no dependency between the two "+
				"modules, the name order is what keeps one run looking like the next", got, want)
		}
	})
}

// TestDuplicatePluginNameIsErrPluginFailed pins what two modules declaring one
// plugin name get.
//
// A handle installs one plugin per name, so the second declaration can never
// take effect and GORM refuses it. The refusal is right and the reason is not
// in it: it does not say whose name was taken, and the operator has two
// declarations in front of them and has to drop one. Both are named, and the
// GORM error stays in the chain — this is a decision about which declaration to
// remove, not a diagnosis this module is entitled to make on its own.
func TestDuplicatePluginNameIsErrPluginFailed(t *testing.T) {
	_, err := startHost(t, sqliteSpec(), hostConfig(t),
		pluginModule("audit", recordingPlugin{name: "cross-cutting"}),
		pluginModule("catalog", recordingPlugin{name: "cross-cutting"}),
	)
	if err == nil {
		t.Fatal("two modules declaring a plugin of the same name started up: one of the two " +
			"declarations was never installed, and nothing said which")
	}
	if !errors.Is(err, db.ErrPluginFailed) {
		t.Errorf("the startup reported %v, want ErrPluginFailed", err)
	}
	if !errors.Is(err, gorm.ErrRegistered) {
		t.Errorf("the startup reported %v, and GORM's refusal is not in the chain: the text has to "+
			"keep why the second one could not go on, or the two declarations look interchangeable", err)
	}
	for _, want := range []string{"audit", "catalog", "cross-cutting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error text %q does not name %q", err, want)
		}
	}
}

// disabledModule declares the two things a module can declare, and states that
// it does not run.
func disabledModule(name string, plugin gorm.Plugin, set db.Migrations) core.Module {
	return core.Module{
		Name:      name,
		Resources: []any{db.Plugin{Plugin: plugin}, set},
		Prepare: func(context.Context, *core.Registry) (core.Enablement, error) {
			return core.Enablement{State: core.StateDisabled, Reason: "this assembly does not run it"}, nil
		},
	}
}

// TestPluginFromDisabledModuleFollowsTheRuling pins what the two declarations
// of a module that resolved to disabled are worth.
//
// The migration set is filtered and the plugin is not, and the asymmetry is the
// design's: a module that does not run creates no tables, but a plugin is a
// statement about the handle rather than about the declaring module's own work,
// and dropping it would take a cross-cutting capability away from every
// dependant, silently, by way of a module that is not even constructed.
func TestPluginFromDisabledModuleFollowsTheRuling(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t),
		pluginModule("audit", recordingPlugin{name: "audit-plugin"}),
		disabledModule("optout", recordingPlugin{name: "optout-plugin"},
			migrationSet(db.SQLite, map[string]string{
				"0001_create_optout_rows.sql": `CREATE TABLE optout_rows (id INTEGER NOT NULL, PRIMARY KEY (id))`,
			})),
		pluginModule("bookkeeper", recordingPlugin{name: "bookkeeper-plugin"}),
		// A module that does run, so "the migration was not applied" cannot
		// pass on a run that applied nothing at all.
		core.Module{Name: "ledger", Resources: []any{
			migrationSet(db.SQLite, map[string]string{
				"0001_create_ledger_rows.sql": `CREATE TABLE ledger_rows (id INTEGER NOT NULL, PRIMARY KEY (id))`,
			}),
		}},
	)
	if err != nil {
		t.Fatalf("starting a host beside a module that declared itself disabled: %v", err)
	}

	handle := h.Instance.DB()
	if !handle.Migrator().HasTable("ledger_rows") {
		t.Fatal("the migration of an enabled module was not applied, so what the next assertion " +
			"observes is a run that applies nothing")
	}
	if handle.Migrator().HasTable("optout_rows") {
		t.Error("the disabled module's migration was applied although it declared that it does not run")
	}
	if got := installedOn(handle); !slices.Contains(got, "optout-plugin") {
		t.Errorf("the handle carries the plugins %v, and the declaration of the module that declared "+
			"itself disabled is not among them: a dependant would run without it and nothing would say so",
			got)
	}
}

// failingPlugin refuses to install, reporting why itself.
type failingPlugin struct {
	name  string
	cause error
}

func (p failingPlugin) Name() string { return p.name }

func (p failingPlugin) Initialize(*gorm.DB) error { return p.cause }

// TestAFailingPluginIsErrPluginFailed pins the sentinel on the plugin's own
// refusal, which is the way a plugin that needs a model convention or a column
// it cannot find fails.
//
// The plugin's error stays in the chain. It is the only account of what the
// plugin wanted: this module does not know the plugin's content, so a text that
// replaced it with "the plugin failed" would leave the operator with a name and
// nothing to fix.
func TestAFailingPluginIsErrPluginFailed(t *testing.T) {
	cause := errors.New("the model this plugin filters by has no tenant column")
	_, err := startHost(t, sqliteSpec(), hostConfig(t),
		pluginModule("audit", failingPlugin{name: "audit-plugin", cause: cause}),
	)
	if err == nil {
		t.Fatal("a plugin that refused to install did not fail the startup, so the handle was " +
			"delivered with a declaration that is not in force")
	}
	if !errors.Is(err, db.ErrPluginFailed) {
		t.Errorf("the startup reported %v, want ErrPluginFailed", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("the startup reported %v, and what the plugin said is not in the chain: it is the "+
			"only account of what the plugin needed", err)
	}
	for _, want := range []string{"audit", "audit-plugin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error text %q does not name %q", err, want)
		}
	}
}

// nilPlugin is a plugin whose methods have value receivers, so a nil pointer to
// it still satisfies gorm.Plugin — and panics on the first call.
type nilPlugin struct{}

func (nilPlugin) Name() string              { return "nil-plugin" }
func (nilPlugin) Initialize(*gorm.DB) error { return nil }

// TestAPluginDeclarationWithNothingInItIsRefused pins the two shapes of an
// empty declaration against what they would otherwise do.
//
// A nil interface panics on the first call inside GORM, and a typed nil pointer
// is not the nil interface, so it passes a plain nil test and panics one frame
// further in. Both are defects in the declaring module, and both are reported
// against the module that holds them rather than as a panic somewhere in a
// third-party call.
func TestAPluginDeclarationWithNothingInItIsRefused(t *testing.T) {
	cases := map[string]core.Module{
		"the nil interface": {Name: "audit", Resources: []any{db.Plugin{}}},
		"a typed nil pointer": {
			Name:      "audit",
			Resources: []any{db.Plugin{Plugin: (*nilPlugin)(nil)}},
		},
	}
	for name, module := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := startHost(t, sqliteSpec(), hostConfig(t), module)
			if !errors.Is(err, db.ErrPluginFailed) {
				t.Fatalf("a declaration carrying nothing reported %v, want ErrPluginFailed", err)
			}
			if !strings.Contains(err.Error(), "audit") {
				t.Errorf("the error text %q does not name the module that declared it", err)
			}
		})
	}
}
