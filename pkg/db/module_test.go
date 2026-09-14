package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"

	"github.com/vislake/speed/pkg/db/internal/dbsuite"
)

// probeNamespace is the namespace the cases below configure. It is a third one
// rather than either shipped implementation's, so that a case about the
// machinery this package owns does not read as a statement about which engine
// is behind it.
const probeNamespace = "db.probe"

// probeSpec is an implementation of the shape every subpackage has: a name, a
// namespace, an engine, a driver binding. It runs on SQLite because the cases
// that reach a database do so through the in-memory fixture.
func probeSpec() Spec {
	return Spec{
		ModuleName:      probeNamespace,
		ConfigNamespace: probeNamespace,
		Dialect:         SQLite,
		Dialector:       func(string) gorm.Dialector { return nil },
	}
}

// TestMissingDSNDisablesWithReason pins the stance an implementation takes
// when its locator is absent.
//
// Disabling with a reason rather than failing is what lets a host import
// several engines and configure one. The reason is not decoration: it is the
// only thing a module that required the database capability has to go on, so
// it has to name the key that would turn this module on, in the namespace this
// implementation owns.
func TestMissingDSNDisablesWithReason(t *testing.T) {
	spec := probeSpec()
	stance, err := spec.prepare(readerFor(t, spec, `{}`))
	if err != nil {
		t.Fatalf("Prepare failed on a configuration that simply has no database: %v", err)
	}
	if stance.State != core.StateDisabled {
		t.Fatalf("Prepare states %v with no locator configured, want StateDisabled", stance.State)
	}
	if want := probeNamespace + "." + dsnKey; !strings.Contains(stance.Reason, want) {
		t.Errorf("the reason %q does not name %s, which is the key that would turn this "+
			"module on", stance.Reason, want)
	}
}

// TestAConfiguredImplementationStatesEnabledNotAuto pins the other stance, and
// the difference between the two words for it.
//
// Resolution stands down an exclusive provider that states StateAuto. Written
// that way, a host that configured two engines would get one of them silently
// and find half its tables in a database nobody mentioned; stating enabled
// makes that assembly fail instead. Every other case here passes with
// StateAuto in its place.
func TestAConfiguredImplementationStatesEnabledNotAuto(t *testing.T) {
	spec := probeSpec()
	stance, err := spec.prepare(readerFor(t, spec, `{"db":{"probe":{"dsn":"file::memory:"}}}`))
	if err != nil {
		t.Fatalf("Prepare failed on a configured locator: %v", err)
	}
	if stance.State != core.StateEnabled {
		t.Errorf("Prepare states %v with a locator configured, want StateEnabled", stance.State)
	}
}

// TestABlankDSNIsTheSameAsNoDSN keeps a locator of spaces from enabling the
// module. Enabled on it, the failure moves to the driver, which reports
// something about its own syntax rather than about the key that was left
// blank.
func TestABlankDSNIsTheSameAsNoDSN(t *testing.T) {
	spec := probeSpec()
	stance, err := spec.prepare(readerFor(t, spec, `{"db":{"probe":{"dsn":"   "}}}`))
	if err != nil {
		t.Fatalf("Prepare failed on a blank locator: %v", err)
	}
	if stance.State != core.StateDisabled {
		t.Errorf("a locator of spaces states %v, want StateDisabled", stance.State)
	}
}

// TestPrepareCarriesTheReaderFailureOut keeps a configuration that could not
// be read from being taken as an assembly that wants no database.
func TestPrepareCarriesTheReaderFailureOut(t *testing.T) {
	sentinel := errors.New("the primary source could not be read")
	stance, err := probeSpec().prepare(&failingReader{err: sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Prepare gave %v, want the reader's own error", err)
	}
	if stance.State == core.StateDisabled {
		t.Errorf("a failed read stood the module down instead of failing the startup")
	}
}

// failingReader is a config.Reader whose section cannot be read.
type failingReader struct{ err error }

func (r *failingReader) Decode(string, any) error { return r.err }

var _ config.Reader = (*failingReader)(nil)

// TestTheDescriptorIsTheShapeTheDesignGivesIt pins what a subpackage gets for
// handing over a Spec: its own name, the database capability claimed
// exclusively, its input item declaration, and the stage callbacks this
// package fills in for every implementation.
//
// Stop is deliberately absent. Draining in-flight work belongs to the modules
// doing it, and the connections are released once, in Close.
func TestTheDescriptorIsTheShapeTheDesignGivesIt(t *testing.T) {
	module := NewModule(probeSpec())

	if module.Name != probeNamespace {
		t.Errorf("the module registers as %q, want %q", module.Name, probeNamespace)
	}
	if len(module.Provides) != 1 {
		t.Fatalf("the module declares %d capabilities, want the database capability alone",
			len(module.Provides))
	}
	provision := module.Provides[0]
	if got := reflect.TypeOf(provision.Token); got != reflect.TypeFor[*Database]() {
		t.Errorf("the module delivers %v, want *db.Database", got)
	}
	if !provision.Exclusive {
		t.Errorf("the database capability is not claimed exclusively, so a second configured " +
			"engine would be settled by resolution instead of failing the startup")
	}
	if len(module.Requires) != 0 {
		t.Errorf("the module requires %v; configuration is an implicit dependency and nothing "+
			"else is taken up at construction", module.Requires)
	}
	if module.Stop != nil {
		t.Errorf("the module carries a Stop callback, and the design gives it no work there")
	}
	for _, c := range []struct {
		stage string
		set   bool
	}{
		{"Prepare", module.Prepare != nil},
		{"Migrate", module.Migrate != nil},
		{"Close", module.Close != nil},
	} {
		if !c.set {
			t.Errorf("the module has no %s callback", c.stage)
		}
	}

	schemas := 0
	for _, resource := range module.Resources {
		schema, ok := resource.(config.Schema)
		if !ok {
			continue
		}
		schemas++
		if schema.Namespace != probeNamespace {
			t.Errorf("the declared input items hang under %q, want this implementation's own "+
				"namespace %q", schema.Namespace, probeNamespace)
		}
	}
	if schemas != 1 {
		t.Errorf("the module declares %d input item schemas, want exactly one", schemas)
	}
}

// TestADefectiveSpecIsRefusedWhereItIsWritten pins that a Spec which cannot
// produce a working module says so at the call that built it.
//
// Each of these is otherwise met much later and much further from its cause: a
// missing driver binding as a nil call inside construction, a mutex with no
// default timeout as a run that gives up on the mutex before it has waited at
// all. A subpackage fills the Spec in at init, so every one of them is a fault
// in this repository's own code rather than something a host could act on.
func TestADefectiveSpecIsRefusedWhereItIsWritten(t *testing.T) {
	for _, c := range []struct {
		name    string
		spec    func(Spec) Spec
		mention string
	}{
		{"no module name", func(s Spec) Spec { s.ModuleName = ""; return s }, "module name"},
		{"no namespace", func(s Spec) Spec { s.ConfigNamespace = ""; return s }, "namespace"},
		{"no dialect", func(s Spec) Spec { s.Dialect = ""; return s }, "dialect"},
		{"no dialector", func(s Spec) Spec { s.Dialector = nil; return s }, "dialector"},
		{"a mutex with no default timeout", func(s Spec) Spec {
			s.NewMigrationLock = func(time.Duration) MigrationLock { return nil }
			return s
		}, MigrationLockTimeoutKey},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				reported := recover()
				if reported == nil {
					t.Fatalf("NewModule accepted a spec with %s", c.name)
				}
				text, ok := reported.(string)
				if !ok {
					t.Fatalf("NewModule panicked with %T, want a message", reported)
				}
				if !strings.Contains(text, c.mention) {
					t.Errorf("the message %q does not mention %s", text, c.mention)
				}
			}()
			NewModule(c.spec(probeSpec()))
		})
	}
}

// TestAWorkingSpecWithAMutexIsAccepted is the positive control for the case
// above: a refusal that fired on everything would pass it just as well.
func TestAWorkingSpecWithAMutexIsAccepted(t *testing.T) {
	spec := probeSpec()
	spec.NewMigrationLock = func(time.Duration) MigrationLock { return nil }
	spec.DefaultMigrationLockTimeout = time.Minute
	if got := NewModule(spec).Name; got != probeNamespace {
		t.Errorf("the module registers as %q, want %q", got, probeNamespace)
	}
}

// stubDatabase is a product of the shape this module delivers: a handle and
// the engine behind it. The two methods the later work items fill in are not
// reached from the stages wired here.
type stubDatabase struct {
	handle  *gorm.DB
	dialect Dialect
}

func (d *stubDatabase) DB() *gorm.DB     { return d.handle }
func (d *stubDatabase) Dialect() Dialect { return d.dialect }
func (d *stubDatabase) BlindIndex(s string) string {
	return s
}

func (d *stubDatabase) Open(context.Context, string) (*gorm.DB, error) {
	return nil, errors.New("the stub product opens nothing")
}

var _ Database = (*stubDatabase)(nil)

// TestCloseToleratesNilAndForeignInstance pins what the rollback path may hand
// the Close callback.
//
// A startup that failed part-way closes every module whatever stage it
// reached, so this callback runs on a module whose New never returned a
// product — and, if that New has a defect, on a nil product of its own type.
// An unguarded type assertion is green on every normal path and panics only
// there, in the middle of a rollback, replacing the failure that caused the
// rollback with a panic about the cleanup.
func TestCloseToleratesNilAndForeignInstance(t *testing.T) {
	closeCallback := NewModule(probeSpec()).Close
	for _, c := range []struct {
		name     string
		instance any
	}{
		{"nothing was constructed", nil},
		{"a nil product of this module's own type", (*stubDatabase)(nil)},
		{"a product of another module", struct{ other int }{}},
		{"a typed nil of another kind", (*gorm.DB)(nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := closeCallback(t.Context(), core.New(), c.instance); err != nil {
				t.Errorf("closing %s reported %v, want no error", c.name, err)
			}
		})
	}
}

// TestCloseReleasesTheDeliveredPool is the other half: tolerating everything
// is easy to get by doing nothing at all, and a module that never released its
// connections leaks a pool per failed startup.
func TestCloseReleasesTheDeliveredPool(t *testing.T) {
	handle := dbsuite.OpenSQLite(t)
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("taking the connection pool of the fixture: %v", err)
	}
	if err := pool.Ping(); err != nil {
		t.Fatalf("the fixture is not usable before the close: %v", err)
	}

	instance := &stubDatabase{handle: handle, dialect: SQLite}
	if err := NewModule(probeSpec()).Close(t.Context(), core.New(), instance); err != nil {
		t.Fatalf("closing a delivered product reported %v", err)
	}
	if err := pool.Ping(); err == nil {
		t.Errorf("the connection pool still answers after Close, so it was never released")
	}
}

// migrateThrough drives the Migrate stage the way the registry would, with the
// configuration this implementation reads its mutex timeout from.
func migrateThrough(t *testing.T, spec Spec, body string, handle *gorm.DB, modules ...core.Module) error {
	t.Helper()
	instance := &stubDatabase{handle: handle, dialect: spec.Dialect}
	return spec.migrate(t.Context(), registryOf(modules...), readerFor(t, spec, body), instance)
}

// TestMigrateAppliesTheDeclaredSetsToTheDeliveredHandle pins that the stage
// reaches the database behind the product it was handed.
func TestMigrateAppliesTheDeclaredSetsToTheDeliveredHandle(t *testing.T) {
	handle := dbsuite.OpenSQLite(t)
	declarer := declaring("catalogue", migrationSet(SQLite, map[string]string{
		"0001_widgets.sql": createWidgets,
	}))
	if err := migrateThrough(t, probeSpec(), `{}`, handle, declarer); err != nil {
		t.Fatalf("the Migrate stage reported %v", err)
	}
	if !hasTable(t, handle, "widgets") {
		t.Errorf("the declared migration was not applied to the handle the product carries")
	}
	if got := recordedMigrations(t, handle); len(got) != 1 {
		t.Errorf("the record table holds %v, want the one migration that was applied", got)
	}
}

// TestMigrateReadsTheSubdirectoryOfTheSpecDialect pins that the engine the
// implementation declared is the one whose files are read.
//
// A stage that took the dialect from anywhere else — a default, the first set
// it found — would apply another engine's files to this one, and SQL that
// happens to parse in both would apply silently.
func TestMigrateReadsTheSubdirectoryOfTheSpecDialect(t *testing.T) {
	handle := dbsuite.OpenSQLite(t)
	spec := probeSpec()
	spec.Dialect = Postgres
	declarer := declaring("catalogue", migrationSet(SQLite, map[string]string{
		"0001_widgets.sql": createWidgets,
	}))
	if err := migrateThrough(t, spec, `{}`, handle, declarer); err != nil {
		t.Fatalf("a declaration with no subdirectory for this dialect reported %v, want a "+
			"run that applies nothing", err)
	}
	if hasTable(t, handle, "widgets") {
		t.Errorf("a migration filed under another dialect was applied")
	}
}

// countingLock records that the run was held under it.
type countingLock struct {
	acquires int
	releases int
}

func (l *countingLock) Acquire(context.Context, *sql.Conn) error { l.acquires++; return nil }
func (l *countingLock) Release(context.Context, *sql.Conn) error { l.releases++; return nil }

var _ MigrationLock = (*countingLock)(nil)

// TestTheConfiguredTimeoutReachesTheMutex pins the path the timeout travels:
// the configuration, this package's schema, and into the mutex the
// implementation builds.
//
// The value is what bounds the wait for another replica. Built from the
// default instead, a host that raised the limit because its first migration
// takes ten minutes still gives up after five, and the startup that fails says
// it waited for a mutex — not that the number it waited for was ignored.
func TestTheConfiguredTimeoutReachesTheMutex(t *testing.T) {
	lock := &countingLock{}
	var given time.Duration
	spec := probeSpec()
	spec.DefaultMigrationLockTimeout = 5 * time.Minute
	spec.NewMigrationLock = func(timeout time.Duration) MigrationLock {
		given = timeout
		return lock
	}

	handle := dbsuite.OpenSQLite(t)
	body := `{"db":{"probe":{"dsn":"file::memory:","migration-lock-timeout":"9s"}}}`
	declarer := declaring("catalogue", migrationSet(SQLite, map[string]string{
		"0001_widgets.sql": createWidgets,
	}))
	if err := migrateThrough(t, spec, body, handle, declarer); err != nil {
		t.Fatalf("the Migrate stage reported %v", err)
	}
	if given != 9*time.Second {
		t.Errorf("the mutex was built with %v, want the configured 9s", given)
	}
	if lock.acquires != 1 || lock.releases != 1 {
		t.Errorf("the mutex was taken %d times and given up %d, want once each",
			lock.acquires, lock.releases)
	}
	if !hasTable(t, handle, "widgets") {
		t.Errorf("the run under the mutex applied nothing")
	}
}

// TestAnImplementationWithoutAMutexRunsWithoutOne pins that the absence of a
// mutex is a supported shape rather than a missing dependency: an engine that
// does not appear in multi-replica deployments has nothing to order.
func TestAnImplementationWithoutAMutexRunsWithoutOne(t *testing.T) {
	spec := probeSpec()
	if spec.lock(spec.defaults()) != nil {
		t.Errorf("an implementation that supplied no mutex builds one anyway")
	}
}

// TestMigrateReportsAProductItCannotApplyOn keeps a defect in this package
// from reaching the engine as a nil dereference. Every constructed module
// reaches this stage, and this one delivers the database capability, so a
// product that is not one is a fault here.
func TestMigrateReportsAProductItCannotApplyOn(t *testing.T) {
	spec := probeSpec()
	err := spec.migrate(t.Context(), core.New(), &pathRecordingReader{}, struct{ other int }{})
	if !errors.Is(err, ErrMigrationFailed) {
		t.Fatalf("a product that is not a database gave %v, want ErrMigrationFailed", err)
	}
	if !strings.Contains(err.Error(), probeNamespace) {
		t.Errorf("the failure does not name the module it came from: %v", err)
	}
}

// startupOver drives a real lifecycle over the given modules, with the
// configuration loader reading body. It is how a case reaches a verdict that
// only exists after resolution: the stance of every module is taken first, and
// the failures below are raised before any module is constructed.
func startupOver(t *testing.T, body string, modules ...core.Module) error {
	t.Helper()
	locator := writeConfig(t, body)

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "SPEEDDBTEST", DefaultLocator: "file://" + locator}},
	})
	for _, m := range modules {
		reg.Register(m)
	}

	previous := os.Args
	os.Args = []string{"db.test"}
	defer func() { os.Args = previous }()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return reg.Run(ctx)
}

// TestTwoConfiguredImplementationsFailTheStartup pins the outcome of a host
// that gave two engines a locator.
//
// Both are enabled, both claim the capability exclusively, and resolution
// refuses to choose. The alternative is not a working process: the dependants
// would take up whichever one was left standing, and the tables of an
// application would be split across two databases with nothing said.
func TestTwoConfiguredImplementationsFailTheStartup(t *testing.T) {
	first, second := probeSpec(), probeSpec()
	second.ModuleName = "db.other"
	second.ConfigNamespace = "db.other"

	body := `{"db":{"probe":{"dsn":"file::memory:"},"other":{"dsn":"file::memory:"}}}`
	err := startupOver(t, body, NewModule(first), NewModule(second))
	if !errors.Is(err, core.ErrExclusiveViolated) {
		t.Fatalf("two configured implementations started up with %v, want "+
			"core.ErrExclusiveViolated", err)
	}
}

// TestADependantIsToldWhyTheDatabaseIsMissing pins that the reason a stance
// carries reaches the module that needed the capability.
//
// This is the whole value of disabling with a reason rather than failing: the
// host is not told that some capability is unavailable, it is told which key
// would have provided it.
func TestADependantIsToldWhyTheDatabaseIsMissing(t *testing.T) {
	dependant := core.Module{
		Name:     "reports",
		Requires: []core.Requirement{{Token: (*Database)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			return core.Resolve[Database](reg)
		},
	}
	err := startupOver(t, `{}`, NewModule(probeSpec()), dependant)
	if !errors.Is(err, core.ErrMissingProvider) {
		t.Fatalf("a dependant of an unconfigured database started up with %v, want "+
			"core.ErrMissingProvider", err)
	}
	if want := probeNamespace + "." + dsnKey; !strings.Contains(err.Error(), want) {
		t.Errorf("the startup failure does not name %s, so the host is left to guess which "+
			"key it forgot: %v", want, err)
	}
}
