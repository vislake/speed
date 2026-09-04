package metering

import (
	"context"
	"embed"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- metering depends on no other pkgcore.Module in the bootstrap set this round", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %v, want nil -- metering has no HTTP surface this round", got)
	}
}

func TestModule_Accessors_ReturnWiredPieces(t *testing.T) {
	m := NewModule(nil)

	if m.Summaries() == nil {
		t.Error("Summaries() = nil")
	}
	if m.Aggregator() == nil {
		t.Error("Aggregator() = nil")
	}
	if m.Recorder() == nil {
		t.Error("Recorder() = nil")
	}
	if m.Dispatcher() == nil {
		t.Error("Dispatcher() = nil")
	}
	if m.Recorder() != Recorder(m.analytics) {
		t.Error("Recorder() did not return the module's own AnalyticsRecorder")
	}
}

func TestModule_Options_WireIntoTheirTargets(t *testing.T) {
	threshold := 5.0
	m := NewModule(nil,
		WithPeriodBucket(PeriodBucketDaily),
		WithOverageThresholds(OverageThresholds{Default: &threshold}),
		WithAnalyticsBufferSize(7),
		WithDispatchInterval(9),
		WithDispatchBatchSize(11),
	)

	if m.aggregator.bucket != PeriodBucketDaily {
		t.Errorf("aggregator.bucket = %q, want %q", m.aggregator.bucket, PeriodBucketDaily)
	}
	if m.aggregator.thresholds.Default == nil || *m.aggregator.thresholds.Default != threshold {
		t.Errorf("aggregator.thresholds.Default = %v, want %v", m.aggregator.thresholds.Default, threshold)
	}
	if cap(m.analytics.events) != 7 {
		t.Errorf("analytics buffer capacity = %d, want 7", cap(m.analytics.events))
	}
	if m.dispatcher.interval != 9 {
		t.Errorf("dispatcher.interval = %v, want 9", m.dispatcher.interval)
	}
	if m.dispatcher.batchSize != 11 {
		t.Errorf("dispatcher.batchSize = %d, want 11", m.dispatcher.batchSize)
	}
}

func TestModule_Options_NonPositiveValuesAreIgnored(t *testing.T) {
	m := NewModule(nil, WithAnalyticsBufferSize(0), WithDispatchInterval(-1), WithDispatchBatchSize(-5))
	if cap(m.analytics.events) != defaultAnalyticsBufferSize {
		t.Errorf("analytics buffer capacity = %d, want the unchanged default %d", cap(m.analytics.events), defaultAnalyticsBufferSize)
	}
	if m.dispatcher.interval != defaultDispatchInterval {
		t.Errorf("dispatcher.interval = %v, want the unchanged default %v", m.dispatcher.interval, defaultDispatchInterval)
	}
	if m.dispatcher.batchSize != defaultDispatchBatchSize {
		t.Errorf("dispatcher.batchSize = %d, want the unchanged default %d", m.dispatcher.batchSize, defaultDispatchBatchSize)
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A
// migration added to one dialect and forgotten in the other fails here
// rather than at a deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil).Migrations()

	names := map[string][]string{}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(dialect)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ holds no migration files", dialect)
		}
		for _, e := range entries {
			names[dialect] = append(names[dialect], e.Name())
		}
	}
	if len(names["postgres"]) != len(names["sqlite"]) {
		t.Fatalf("dialect file counts differ: postgres %v, sqlite %v", names["postgres"], names["sqlite"])
	}
	for i, name := range names["postgres"] {
		if names["sqlite"][i] != name {
			t.Errorf("migration %q exists in postgres/ but sqlite/ has %q at the same position", name, names["sqlite"][i])
		}
	}
}

// TestModule_Locales_ShipsBothLanguages pins the i18n rule at the module
// boundary: exactly the two languages the catalog serves, no more and no
// fewer, since Kernel.Bootstrap rejects a module that ships a file for a
// language the others do not.
func TestModule_Locales_ShipsBothLanguages(t *testing.T) {
	entries, err := NewModule(nil).Locales().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"zh-CN.toml", "en-US.toml"} {
		if !got[want] {
			t.Errorf("locales are missing %s", want)
		}
	}
	if len(entries) != 2 {
		t.Errorf("locales hold %d files, want exactly zh-CN.toml and en-US.toml", len(entries))
	}
}

// TestModule_Register_DeclaresItsSurface bootstraps metering through the
// real kernel -- the same path a host takes -- and asserts every
// declaration arrives on the registry. Bootstrapping rather than calling
// Register against a hand-built Registry is deliberate: it also proves
// metering's locale files survive i18n.Builder.AddModule's parity
// validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	m := NewModule(newTestDB(t))
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	t.Run("config items", func(t *testing.T) {
		var keys []string
		for _, item := range reg.Config.Items() {
			keys = append(keys, item.Key)
			if item.Description == "" {
				t.Errorf("config item %q is declared without a description", item.Key)
			}
			if item.Sensitive {
				t.Errorf("config item %q is marked Sensitive; metering declares no sensitive items", item.Key)
			}
		}
		assertContainsAll(t, keys, []string{ConfigPeriodBucketSize, ConfigDefaultOverageThreshold})
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
			if decl.Description == "" {
				t.Errorf("event %q is declared without a description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventOverageThresholdCrossed})
	})

	t.Run("no permissions are declared", func(t *testing.T) {
		// This round has no HTTP surface for rbac to gate -- see
		// AGENTS.md's Known limitations.
		if got := reg.Permissions.Permissions(); len(got) != 0 {
			t.Errorf("Register declared %d permission(s), want 0 this round", len(got))
		}
	})

	t.Run("no audit actions are declared", func(t *testing.T) {
		if got := reg.AuditActions.Actions(); len(got) != 0 {
			t.Errorf("Register declared %d audit action(s), want 0 this round", len(got))
		}
	})

	t.Run("no routes are mounted", func(t *testing.T) {
		if got := reg.Routes.Routes(); len(got) != 0 {
			t.Errorf("Register mounted %d route(s), want 0 -- metering has no HTTP surface this round", len(got))
		}
	})

	t.Run("aggregator's EventBus is wired", func(t *testing.T) {
		if m.aggregator.bus == nil {
			t.Error("aggregator.bus is nil after Register; Ingest could never publish an overage event")
		}
	})
}

// TestModule_Register_CoexistsWithAnotherModule proves metering bootstraps
// alongside a module that declares its own config and events -- the real
// host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(newTestDB(t)), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	var keys []string
	for _, item := range reg.Config.Items() {
		keys = append(keys, item.Key)
	}
	assertContainsAll(t, keys, []string{ConfigPeriodBucketSize, "neighbour.some_setting"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database,
// which is what pkgcore.Module's "it only declares" contract requires to
// be safe: any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := NewModule(nil)
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

// TestModule_StartStop_IsIdempotent proves Start/Stop reach both
// background pipelines and remain safe to call repeatedly, mirroring
// AnalyticsRecorder's and Dispatcher's own identical contracts.
func TestModule_StartStop_IsIdempotent(t *testing.T) {
	m := NewModule(newTestDB(t))
	ctx := context.Background()
	m.Start(ctx)
	m.Start(ctx)
	m.Stop()
	m.Stop()
}

// assertContainsAll fails the test for every want element missing from
// got.
func assertContainsAll(t *testing.T, got []string, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, s := range got {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing %q; got %v", w, got)
		}
	}
}

// neighbourModule is a minimal second pkgcore.Module, used only to prove
// metering bootstraps correctly alongside another module's own
// declarations.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	return reg.Config.Add(pkgcore.ConfigItem{
		Key:         "neighbour.some_setting",
		Type:        "bool",
		Default:     false,
		Description: "A neighbouring module's own setting, unrelated to metering.",
		Group:       "neighbour",
	})
}

var _ pkgcore.Module = neighbourModule{}
