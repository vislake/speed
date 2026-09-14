package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
	"github.com/vislake/speed/pkg/db/internal/pgtest"
	"github.com/vislake/speed/pkg/db/postgres"
)

// sharedItems are the input items every implementation declares, written out
// here rather than read from the root package so that this case states what the
// design gives a namespace instead of restating whatever the code happens to
// build.
var sharedItems = []string{
	"dsn",
	"max-open-conns",
	"max-idle-conns",
	"conn-max-lifetime",
	"encryption-key",
	"encryption-retired-keys",
}

// TestPostgresModuleShape holds the descriptor this subpackage registers to the
// shape the design gives an implementation module.
//
// Three of the assertions are the ones a host's behaviour turns on, and none of
// them is visible from any other subpackage case. The exclusivity is what makes
// a host that configured two engines fail its startup rather than run one of
// them silently; the namespace is where this implementation's items are
// mounted, and a shared one conflicts with the neighbour's declaration before
// exclusivity is ever resolved; and the migration-lock-timeout item is declared
// if and only if the mutex was handed over, so its absence is how a forgotten
// mutex would read.
func TestPostgresModuleShape(t *testing.T) {
	module := postgres.Module()

	if module.Name != "db.postgres" {
		t.Errorf("the module registers as %q, and the design names it db.postgres", module.Name)
	}
	if module.Name != postgres.ModuleName {
		t.Errorf("the registered name %q and the exported constant %q differ, and the startup "+
			"diagnostics name the module through the former", module.Name, postgres.ModuleName)
	}

	if len(module.Provides) != 1 {
		t.Fatalf("the module declares %d capabilities, and an implementation delivers exactly the "+
			"database capability", len(module.Provides))
	}
	provision := module.Provides[0]
	if want := reflect.TypeOf((*db.Database)(nil)); reflect.TypeOf(provision.Token) != want {
		t.Errorf("the module delivers %v, and the capability is %v", reflect.TypeOf(provision.Token), want)
	}
	if !provision.Exclusive {
		t.Error("the module does not claim the database capability exclusively, so a host that " +
			"configured two engines would get one of them silently and find half its tables in a " +
			"database nobody mentioned")
	}

	if len(module.Resources) != 1 {
		t.Fatalf("the module carries %d resources, and an implementation carries its input item "+
			"declaration and nothing else", len(module.Resources))
	}
	schema, ok := module.Resources[0].(config.Schema)
	if !ok {
		t.Fatalf("the module's resource is %T, and an implementation hands over a config.Schema",
			module.Resources[0])
	}
	if schema.Namespace != "db.postgres" {
		t.Errorf("the items are mounted on %q, and this implementation's own section is db.postgres",
			schema.Namespace)
	}
	if schema.Namespace != postgres.ConfigNamespace {
		t.Errorf("the schema is mounted on %q and the exported namespace is %q", schema.Namespace,
			postgres.ConfigNamespace)
	}

	declared := make(map[string]bool, len(schema.Items))
	for key := range schema.Items {
		declared[key] = true
	}
	for _, key := range sharedItems {
		if !declared[key] {
			t.Errorf("the shared item %q is missing from this implementation's declaration, and every "+
				"implementation accepts the same ones", key)
		}
	}
	if !declared[db.MigrationLockTimeoutKey] {
		t.Errorf("the item %q is missing, and this implementation is the one whose dialect owes a "+
			"cross-process mutex: the item is declared exactly when a mutex was supplied",
			db.MigrationLockTimeoutKey)
	}
	if len(declared) != len(sharedItems)+1 {
		t.Errorf("the section declares %d items and the design gives it %d",
			len(declared), len(sharedItems)+1)
	}

	if module.Prepare == nil {
		t.Error("the module has no Prepare, so it never states whether it runs: a host with no locator " +
			"configured would fail the startup instead of running without a database")
	}
	if module.New == nil {
		t.Error("the module has no New, so no connection is ever established")
	}
	if module.Migrate == nil {
		t.Error("the module has no Migrate, so the declared migration sets are never applied")
	}
	if module.Close == nil {
		t.Error("the module has no Close, so the connection pool is never released")
	}
	if module.Stop != nil {
		t.Error("the module carries a Stop callback, and this module has none: draining is each " +
			"module's own responsibility for its in-flight work, and the pool is released in Close")
	}
}

// TestTheProcessRegistrationCarriesTheModule pins the init registration.
//
// A host's available set is decided by what it imports, and importing this
// package is the whole gesture: the module reaches the registry the host runs
// on, not a registry built by hand in a case. Without the registration a host
// would get no database at all, and nothing else in this package would notice.
func TestTheProcessRegistrationCarriesTheModule(t *testing.T) {
	registered, ok := core.ProcessRegistry.Lookup(postgres.ModuleName)
	if !ok {
		t.Fatalf("importing this package did not register %q with the process registry, so a host that "+
			"imports it runs without a database", postgres.ModuleName)
	}
	if registered.Name != postgres.ModuleName {
		t.Errorf("the registry holds %q under the name %q", registered.Name, postgres.ModuleName)
	}
	if want := postgres.Module().Provides; !reflect.DeepEqual(registered.Provides, want) {
		t.Errorf("the registered descriptor delivers %v, and Module builds %v: the registration is the "+
			"descriptor hosts actually run on", registered.Provides, want)
	}
}

// dependantName is the module that declares the migration set below. Its name
// reaches the record table, so a row recorded under it is this run's work.
const dependantName = "postgres_test.dependant"

// dependantFile is the migration that module declares.
const dependantFile = "0001_widgets.sql"

// widgetsMigrations is the declaration the dependant hands over. The
// subdirectory is written out rather than taken from postgres.Dialect: a
// fixture that read the constant would agree with whatever it happened to be,
// and an engine whose dialect sent the run looking elsewhere would still find
// these files. The file holds two statements, the shape a migration carrying a
// table and its index has.
func widgetsMigrations() fstest.MapFS {
	return fstest.MapFS{
		"postgres/" + dependantFile: &fstest.MapFile{Data: []byte(
			"CREATE TABLE widgets (id INTEGER PRIMARY KEY, name TEXT NOT NULL);\n" +
				"CREATE INDEX widgets_name_idx ON widgets (name);\n")},
	}
}

// dependant is the module runAssembly registers beside the implementation: it
// requires the capability, declares one migration set for this engine, and
// reports what it resolved and when the run has been through Migrate.
type dependant struct {
	delivered chan db.Database
	migrated  chan struct{}
}

// module is the dependant's descriptor.
func (d *dependant) module() core.Module {
	return core.Module{
		Name:      dependantName,
		Requires:  []core.Requirement{{Token: (*db.Database)(nil)}},
		Resources: []any{db.Migrations{FS: widgetsMigrations()}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			instance, err := core.Resolve[db.Database](reg)
			if err != nil {
				return nil, err
			}
			d.delivered <- instance
			return struct{}{}, nil
		},
		Init: func(context.Context, *core.Registry, any) error {
			// Migrate is behind this stage, and no migration is applied
			// after it.
			close(d.migrated)
			return nil
		},
	}
}

// runAssembly runs a real host around the module this subpackage registers, and
// returns the capability the dependant resolved.
//
// Everything about it is real: the configuration loader over a JSON primary
// source, the registry, the lifecycle driver, and the container fixture behind
// the locator. The point is what a host that imports this package gets, and
// that is only observable through the whole of it — a call to a callback in
// isolation would not show whether the stages were wired to one another.
//
// The configuration is written under this implementation's namespace, so an
// implementation that mounted its items somewhere else is not configured at
// all: it disables itself and the dependant's requirement, not its silent
// absence, is what the case reports.
func runAssembly(t *testing.T, dsn string) (db.Database, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"db": map[string]any{"postgres": map[string]any{"dsn": dsn}},
	})
	if err != nil {
		t.Fatalf("building the primary configuration source: %v", err)
	}
	locator := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(locator, body, 0o600); err != nil {
		t.Fatalf("writing the primary configuration source: %v", err)
	}

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "SPEEDPOSTGRESTEST", DefaultLocator: "file://" + locator}},
	})
	reg.Register(postgres.Module())
	d := &dependant{delivered: make(chan db.Database, 1), migrated: make(chan struct{})}
	reg.Register(d.module())

	// The loader reads os.Args unconditionally and a test binary carries
	// arguments of its own, so they are taken away for the call. The cases
	// here therefore do not run in parallel.
	previous := os.Args
	os.Args = []string{"postgres.test"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	var instance db.Database
	select {
	case err := <-done:
		os.Args = previous
		cancel()
		return nil, err
	case instance = <-d.delivered:
		os.Args = previous
	}
	// The capability is in hand, but Migrate is still ahead of the run, and
	// returning here would let the case read the database under a migration
	// that is still in flight.
	select {
	case err := <-done:
		cancel()
		return nil, err
	case <-d.migrated:
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("the host shut down with an error: %v", err)
		}
	})
	return instance, nil
}

// TestTheRegisteredModuleAppliesThisEnginesMigrations runs the module this
// subpackage registers, end to end, on a database of its own.
//
// The registration is the deliverable here, and this is what says it is worth
// anything: a host that imports the package and configures the namespace this
// implementation owns gets a capability, resolutions succeed against it, and
// the migration sets declared for this engine are applied on it. What it pins
// beyond the shape case is the wiring — the dialect that names the migration
// subdirectory, the driver binding that opens that locator, and the migrate
// stage that reaches both. The index in the record below is the engine's third
// admission condition seen from here: the file holds two statements and the run
// hands it over whole, without splitting the text.
func TestTheRegisteredModuleAppliesThisEnginesMigrations(t *testing.T) {
	dsn := pgtest.Acquire(t)

	capability, err := runAssembly(t, dsn)
	if err != nil {
		t.Fatalf("a host that imported this implementation and configured its namespace did not start: %v", err)
	}
	if got := capability.Dialect(); got != postgres.Dialect {
		t.Errorf("the delivered capability reports the %q engine, and this subpackage binds %q",
			got, postgres.Dialect)
	}

	direct := open(t, dsn)
	if !tableExists(t, direct, "widgets") {
		t.Error("the table the dependant's migration creates is missing, so the run did not read this " +
			"engine's migration subdirectory")
	}
	// to_regclass resolves any relation, an index included. The index comes
	// from the second statement of one file, so its absence is what an engine
	// that stopped at the first statement looks like.
	if !tableExists(t, direct, "widgets_name_idx") {
		t.Error("the index the second statement of the migration creates is missing, so the run did " +
			"not carry the whole file to the driver")
	}

	type applied struct {
		Module string
		File   string
	}
	var records []applied
	if err := direct.Raw("SELECT module, file FROM db_migrations ORDER BY module, file").Scan(&records).Error; err != nil {
		t.Fatalf("reading the migration record table: %v", err)
	}
	if len(records) != 1 || records[0].Module != dependantName || records[0].File != dependantFile {
		t.Errorf("the record table holds %v, and this run should have recorded one migration, %s/%s: "+
			"the table being there without its record means the table above came from somewhere else",
			records, dependantName, dependantFile)
	}
}
