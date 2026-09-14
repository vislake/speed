package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db/internal/dbsuite"
)

// These cases live in package db rather than beside the others in db_test: the
// migration engine's entry point is unexported, and exporting it to reach it
// from outside would widen the module's contract for the tests' convenience.

// unrelatedAssets stands for what a module that embeds assets declares — an
// i18n bundle, a template set. The type is embed.FS, the same type a migration
// set's FS usually is, which is the whole reason Migrations is a struct.
//
//go:embed testdata/assets
var unrelatedAssets embed.FS

// createWidgets is a migration that fails loudly if it is ever applied twice:
// there is no IF NOT EXISTS on it.
const createWidgets = `CREATE TABLE widgets (id INTEGER NOT NULL, PRIMARY KEY (id))`

// forEachDialect runs a case against every dialect available on this machine,
// each on a database of its own.
func forEachDialect(t *testing.T, run func(t *testing.T, dialect Dialect, handle *gorm.DB)) {
	t.Helper()
	for _, fixture := range dbsuite.Fixtures(t) {
		t.Run(fixture.Dialect, func(t *testing.T) {
			run(t, Dialect(fixture.Dialect), fixture.Open(t))
		})
	}
}

// migrationSet builds a declaration holding the given files under the
// dialect's subdirectory.
func migrationSet(dialect Dialect, files map[string]string) Migrations {
	return Migrations{FS: filesUnder(string(dialect), files)}
}

// filesUnder builds an in-memory tree with the files under one directory.
func filesUnder(dir string, files map[string]string) fstest.MapFS {
	tree := fstest.MapFS{}
	for name, body := range files {
		tree[path.Join(dir, name)] = &fstest.MapFile{Data: []byte(body)}
	}
	return tree
}

// declaring is a module that brings migrations and nothing else, which is the
// shape of most modules with tables.
func declaring(name string, sets ...Migrations) core.Module {
	resources := make([]any, 0, len(sets))
	for _, set := range sets {
		resources = append(resources, set)
	}
	return core.Module{Name: name, Resources: resources}
}

// registryOf builds a registry holding the given modules.
func registryOf(modules ...core.Module) *core.Registry {
	reg := core.New()
	for _, m := range modules {
		reg.Register(m)
	}
	return reg
}

// hasTable reports whether a table exists, the same way in every dialect.
func hasTable(t *testing.T, handle *gorm.DB, name string) bool {
	t.Helper()
	return handle.Migrator().HasTable(name)
}

// recordRow is one row of the migration record table.
type recordRow struct {
	Module string
	File   string
}

// recordedMigrations reads the record table, and fails the test if it cannot.
func recordedMigrations(t *testing.T, handle *gorm.DB) []recordRow {
	t.Helper()
	var rows []recordRow
	if err := handle.Raw(selectRecords).Scan(&rows).Error; err != nil {
		t.Fatalf("reading the migration record table: %v", err)
	}
	return rows
}

// TestMigrationExecutionAndRecordShareOneTransaction is the observation that
// tells a one-transaction implementation apart from one that executes first and
// records second.
//
// The record table is created by hand beforehand with a fourth column that is
// NOT NULL and has no default. The engine's CREATE TABLE IF NOT EXISTS is
// therefore a no-op, its SELECT still works, and its INSERT cannot succeed. A
// migration that creates a table then runs against a record that will not
// write.
//
// With one transaction around both, the created table is rolled back with the
// record: nothing exists and nothing is recorded, so the next start applies it
// again. Executing first and recording second leaves the table behind, and that
// half-applied state only shows up on the following start, as a CREATE TABLE
// against an object that already exists — far from what caused it.
func TestMigrationExecutionAndRecordShareOneTransaction(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		blocked := fmt.Sprintf(`CREATE TABLE %s (
			module TEXT NOT NULL,
			file TEXT NOT NULL,
			applied_at TIMESTAMP NOT NULL,
			extra TEXT NOT NULL,
			PRIMARY KEY (module, file)
		)`, recordTableName)
		if err := handle.Exec(blocked).Error; err != nil {
			t.Fatalf("preparing the record table that refuses inserts: %v", err)
		}

		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, nil)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("applying a migration whose record cannot be written reported %v, want %v",
				err, ErrMigrationFailed)
		}
		if hasTable(t, handle, "widgets") {
			t.Error("the widgets table survived a migration whose record was never written: " +
				"the execution and the record are not in one transaction, and the next start " +
				"will fail on an object that already exists")
		}
		if rows := recordedMigrations(t, handle); len(rows) != 0 {
			t.Errorf("the record table holds %v after a failed migration, want nothing", rows)
		}
	})
}

// TestSecondRunAppliesNothing pins idempotence. The migration carries no IF NOT
// EXISTS, so an implementation that re-applies it fails outright on the second
// run rather than quietly doing the work twice.
func TestSecondRunAppliesNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("first run: %v", err)
		}
		first := recordedMigrations(t, handle)
		if len(first) != 1 {
			t.Fatalf("the first run recorded %v, want one migration", first)
		}

		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("second run: %v, want the already-applied migration to be skipped", err)
		}
		if second := recordedMigrations(t, handle); len(second) != len(first) {
			t.Errorf("the second run left %v in the record table, want the %v the first run wrote",
				second, first)
		}
	})
}

// TestDisabledModuleMigrationsAreNotApplied pins the enablement filter. It runs
// the real lifecycle, because a module's verdict only exists after resolution
// and the registry reports nothing before that.
func TestDisabledModuleMigrationsAreNotApplied(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		muted := declaring("muted", migrationSet(dialect, map[string]string{
			"0001_create_muted.sql": `CREATE TABLE muted_rows (id INTEGER NOT NULL, PRIMARY KEY (id))`,
		}))
		muted.Prepare = func(context.Context, *core.Registry) (core.Enablement, error) {
			return core.Enablement{State: core.StateDisabled, Reason: "this test disables it"}, nil
		}
		catalog := declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		}))

		if err := startupWith(t, handle, dialect, muted, catalog); err != nil {
			t.Fatalf("startup: %v", err)
		}
		if hasTable(t, handle, "muted_rows") {
			t.Error("a disabled module's table was created")
		}
		if !hasTable(t, handle, "widgets") {
			t.Error("an enabled module's table was not created, so the filter caught too much")
		}
	})
}

// TestOnlyCurrentDialectSubdirectoryIsRead pins that a declaration covering
// several engines contributes only the running one's files. The subdirectories
// that are not the running dialect hold statements no engine accepts, so
// reading one of them fails the run rather than passing unnoticed.
func TestOnlyCurrentDialectSubdirectoryIsRead(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		tree := fstest.MapFS{}
		for _, other := range []Dialect{Postgres, SQLite, "mysql"} {
			body := createWidgets
			if other != dialect {
				body = `THIS IS NOT SQL IN ANY DIALECT`
			}
			tree[path.Join(string(other), "0001_create_widgets.sql")] = &fstest.MapFile{Data: []byte(body)}
		}

		reg := registryOf(declaring("catalog", Migrations{FS: tree}))
		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("applying the %s files of a multi-dialect declaration: %v", dialect, err)
		}
		if !hasTable(t, handle, "widgets") {
			t.Errorf("the %s subdirectory was not applied", dialect)
		}
	})
}

// TestMissingDialectSubdirectoryIsZeroMigrations pins that a module which does
// not support the running engine says so by leaving the subdirectory out, and
// that this is not a failure.
func TestMissingDialectSubdirectoryIsZeroMigrations(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		reg := registryOf(declaring("catalog", migrationSet("mysql", map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("a declaration without a %s subdirectory reported %v, want no error", dialect, err)
		}
		if hasTable(t, handle, "widgets") {
			t.Error("a subdirectory belonging to another dialect was applied")
		}
		if rows := recordedMigrations(t, handle); len(rows) != 0 {
			t.Errorf("the record table holds %v, want nothing", rows)
		}
	})
}

// TestUnrelatedEmbedFSIsNotApplied is the end-to-end form of why Migrations is
// a struct rather than an fs.FS.
//
// Two modules declare embedded assets without wrapping them: one as the
// embed.FS an //go:embed directive produces, one as the fs.Sub view of it a
// module takes when its assets sit under a prefix. The second is the one with
// teeth — its top level is dialect-named directories, so an engine collecting
// bare fs.FS values would find a file for the running dialect and run it.
//
// The genuine declaration is there so that "nothing unrelated was applied"
// cannot pass on a run that applied nothing at all.
func TestUnrelatedEmbedFSIsNotApplied(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		assetRoot, err := fs.Sub(unrelatedAssets, "testdata/assets")
		if err != nil {
			t.Fatalf("reaching the embedded assets: %v", err)
		}

		reg := registryOf(
			core.Module{Name: "i18n", Resources: []any{unrelatedAssets}},
			core.Module{Name: "templates", Resources: []any{assetRoot}},
			declaring("catalog", migrationSet(dialect, map[string]string{
				"0001_create_widgets.sql": createWidgets,
			})),
		)

		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("applying migrations alongside unrelated embedded assets: %v", err)
		}
		if !hasTable(t, handle, "widgets") {
			t.Fatal("the declared migration was not applied, so this case proves nothing about the rest")
		}
		if hasTable(t, handle, "unrelated_asset_table") {
			t.Error("an embedded asset that was never declared as a migration set was applied")
		}
		for _, row := range recordedMigrations(t, handle) {
			if row.Module != "catalog" {
				t.Errorf("the record table holds %v, declared by no migration set", row)
			}
		}
	})
}

// TestMigrationFailureNamesModuleAndFile pins what the error has to carry: the
// module is who has to change the statement, and the file is which one. This
// module cannot know how to fix either.
//
// It also pins the chain. A %v in place of any %w along the way leaves the text
// all but identical while the sentinel and the driver's own error both become
// unreachable.
func TestMigrationFailureNamesModuleAndFile(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		const broken = `THIS IS NOT SQL IN ANY DIALECT`
		// What the driver says about this statement, taken straight from
		// it, so the case can look for that very error inside the chain
		// rather than for its text inside a message.
		reference := handle.Exec(broken).Error
		if reference == nil {
			t.Fatal("the fixture accepted a statement that is not SQL, so there is no failure to inspect")
		}

		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0007_broken.sql": broken,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, nil)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("a migration that does not parse reported %v, want %v", err, ErrMigrationFailed)
		}
		for _, want := range []string{"catalog", "0007_broken.sql"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error text %q does not name %q", err, want)
			}
		}
		carried := false
		for _, cause := range allErrors(err) {
			if cause.Error() == reference.Error() {
				carried = true
			}
		}
		if !carried {
			t.Errorf("the error %v does not carry the driver's own error, only its text: every %%w "+
				"along the way has to stay one, and a %%v leaves the message all but unchanged "+
				"while errors.Is and errors.As stop reaching what the driver said", err)
		}
	})
}

// TestRecordTableFailureIsAlsoErrMigrationFailed pins the second trigger of the
// sentinel. The record table is prepared without the file column, so reading it
// fails while the reentrant create statement passes over it.
func TestRecordTableFailureIsAlsoErrMigrationFailed(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		unusable := fmt.Sprintf(`CREATE TABLE %s (module TEXT NOT NULL)`, recordTableName)
		if err := handle.Exec(unusable).Error; err != nil {
			t.Fatalf("preparing an unusable record table: %v", err)
		}

		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, nil)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("an unreadable record table reported %v, want %v", err, ErrMigrationFailed)
		}
		if !strings.Contains(err.Error(), recordTableName) {
			t.Errorf("the error text %q does not name the record table, and there is no migration to name", err)
		}
		if hasTable(t, handle, "widgets") {
			t.Error("a migration was applied although the record table could not be read")
		}
	})
}

// probeCapability stands for whatever capability one module takes up from
// another. Its only job here is to carry a dependency edge.
type probeCapability interface{ probe() }

// TestModulesMigrateInDependencyOrder pins that the order comes from the
// Requires declarations rather than from the module names.
//
// The dependency runs against the names: "alpha" depends on "zeta", so zeta's
// migration has to be applied first. zeta creates a table, alpha inserts into
// it. Ordered by name, alpha's insert hits a table that does not exist yet and
// the run fails — which is what makes this case tell the two apart.
func TestModulesMigrateInDependencyOrder(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		zeta := declaring("zeta", migrationSet(dialect, map[string]string{
			"0001_create_zeta_rows.sql": `CREATE TABLE zeta_rows (id INTEGER NOT NULL, PRIMARY KEY (id))`,
		}))
		zeta.Provides = []core.Provision{{Token: (*probeCapability)(nil)}}

		alpha := declaring("alpha", migrationSet(dialect, map[string]string{
			"0001_fill_zeta_rows.sql": `INSERT INTO zeta_rows (id) VALUES (1)`,
		}))
		alpha.Requires = []core.Requirement{{Token: (*probeCapability)(nil)}}

		reg := registryOf(alpha, zeta)
		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("applying in dependency order: %v — a dependant's migration ran before the "+
				"module it depends on", err)
		}

		var ids []int64
		if err := handle.Raw(`SELECT id FROM zeta_rows`).Scan(&ids).Error; err != nil {
			t.Fatalf("reading back what the dependant's migration wrote: %v", err)
		}
		if len(ids) != 1 || ids[0] != 1 {
			t.Errorf("zeta_rows holds %v, want the single row alpha's migration inserted", ids)
		}
	})
}

// TestFileOrderWithinAModuleIsLexical pins that file name is the whole of the
// order inside one module: the second file depends on the first having run.
func TestFileOrderWithinAModuleIsLexical(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0002_fill_widgets.sql":   `INSERT INTO widgets (id) VALUES (1)`,
			"0001_create_widgets.sql": createWidgets,
		})))

		if err := applyMigrations(t.Context(), reg, handle, dialect, nil); err != nil {
			t.Fatalf("applying a module's files in name order: %v", err)
		}
		rows := recordedMigrations(t, handle)
		if len(rows) != 2 || rows[0].File != "0001_create_widgets.sql" {
			t.Errorf("the record table holds %v, want the two files in name order", rows)
		}
	})
}

// TestDuplicateFileNameWithinOneModuleIsRejected pins the hole the record
// table's key leaves open. It is keyed by module and file name, so a module
// declaring the same file name in two migration sets would have the second one
// counted as applied the moment the first one is recorded — it would never run,
// and nothing would say so.
func TestDuplicateFileNameWithinOneModuleIsRejected(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		reg := registryOf(declaring("catalog",
			migrationSet(dialect, map[string]string{"0001_create.sql": createWidgets}),
			migrationSet(dialect, map[string]string{"0001_create.sql": `CREATE TABLE gadgets (id INTEGER)`}),
		))

		err := applyMigrations(t.Context(), reg, handle, dialect, nil)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("two declarations of the same file name reported %v, want %v", err, ErrMigrationFailed)
		}
		for _, want := range []string{"catalog", "0001_create.sql"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error text %q does not name %q", err, want)
			}
		}
		// Rejected while the set is being collected, before anything ran.
		// An engine that took the pair as it found them would apply the
		// first file and only trip over the duplicate key the second one
		// writes — leaving the first migration behind, and on the next
		// start counting the second one as applied without ever running it.
		if hasTable(t, handle, "widgets") {
			t.Error("the first of the two declarations was applied before the duplicate was caught: " +
				"the clash has to be found while the set is collected, not by the record table's key")
		}
		if rows := recordedMigrations(t, handle); len(rows) != 0 {
			t.Errorf("the record table holds %v although the declaration was rejected", rows)
		}
	})
}

// TestDirectoryInsideADialectDirectoryIsRejected pins that nothing is passed
// over without a word. The engine does not descend, so a directory there would
// take its migrations out of the run silently.
func TestDirectoryInsideADialectDirectoryIsRejected(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"archive/0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, nil)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("a directory among the migration files reported %v, want %v", err, ErrMigrationFailed)
		}
		if !strings.Contains(err.Error(), "archive") {
			t.Errorf("the error text %q does not name the directory that was not applied", err)
		}
	})
}

// TestTheDialectPutsDDLInsideATransaction pins the admission condition every
// implementation subpackage has to meet, and it is what stands in for a
// cross-process case on a dialect that needs no mutex.
//
// A migration's execution and its record are atomic only if the engine rolls
// DDL back with the transaction around it. On a dialect where DDL takes effect
// outside the transaction, a failed record would leave the schema change behind
// and the record table's idempotence would be broken from then on.
func TestTheDialectPutsDDLInsideATransaction(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		rollback := errors.New("rolled back on purpose")
		err := handle.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(createWidgets).Error; err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("the transaction reported %v, want the error it was rolled back with", err)
		}
		if hasTable(t, handle, "widgets") {
			t.Errorf("%s left a table created inside a rolled-back transaction behind: it does not "+
				"put DDL inside transactions, and a migration's execution cannot be atomic with its "+
				"record on it", dialect)
		}
	})
}

// probeLock records what the migration run looks like from the mutex's two
// ends. It is the only way a single process can observe the seam the
// cross-process case depends on: the real failure needs several replicas
// starting at once, and no test inside one process reproduces it.
type probeLock struct {
	handle     *gorm.DB
	acquireErr error
	releaseErr error

	acquires             int
	releases             int
	recordTableAtAcquire bool
	recordsAtRelease     int
}

func (l *probeLock) Acquire(context.Context) error {
	l.acquires++
	if l.acquireErr != nil {
		return l.acquireErr
	}
	l.recordTableAtAcquire = l.handle.Migrator().HasTable(recordTableName)
	return nil
}

func (l *probeLock) Release(context.Context) error {
	l.releases++
	if l.handle.Migrator().HasTable(recordTableName) {
		var rows []recordRow
		if err := l.handle.Raw(selectRecords).Scan(&rows).Error; err == nil {
			l.recordsAtRelease = len(rows)
		}
	}
	return l.releaseErr
}

var _ MigrationLock = (*probeLock)(nil)

// TestTheMutexSpansTheWholeRun pins where the mutex is taken and released.
//
// The design puts it around everything from creating the record table to the
// last migration being applied, and that span is the whole of its value: taken
// any later, every replica has already read an empty record table and they all
// run the same statements — which is exactly the failure that shows up only on
// a first start of a multi-replica deployment.
//
// Both ends are observed from inside the mutex itself: the record table must
// not exist yet when it is taken, and every migration must be recorded by the
// time it is released. A run that took the mutex after reading the record
// table, or released it between migrations, differs in one of those two.
func TestTheMutexSpansTheWholeRun(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		lock := &probeLock{handle: handle}
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
			"0002_fill_widgets.sql":   `INSERT INTO widgets (id) VALUES (1)`,
		})))

		if err := applyMigrations(t.Context(), reg, handle, dialect, lock); err != nil {
			t.Fatalf("applying under a mutex: %v", err)
		}
		if lock.acquires != 1 || lock.releases != 1 {
			t.Errorf("the mutex was taken %d times and released %d, want once each: it covers the "+
				"whole run, not one migration at a time", lock.acquires, lock.releases)
		}
		if lock.recordTableAtAcquire {
			t.Error("the record table already existed when the mutex was taken: the run started " +
				"before the mutex, which is the window several replicas collide in")
		}
		if lock.recordsAtRelease != 2 {
			t.Errorf("%d migrations were recorded when the mutex was released, want 2: it was given "+
				"up before the run finished", lock.recordsAtRelease)
		}
	})
}

// TestTheMutexIsReleasedAfterAFailedMigration pins the other end of the span. A
// run that keeps the mutex after failing blocks every other replica's startup
// until its connection drops.
func TestTheMutexIsReleasedAfterAFailedMigration(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		lock := &probeLock{handle: handle}
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_broken.sql": `THIS IS NOT SQL IN ANY DIALECT`,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, lock)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("a failing migration under a mutex reported %v, want %v", err, ErrMigrationFailed)
		}
		if lock.releases != 1 {
			t.Errorf("the mutex was released %d times after a failed migration, want once", lock.releases)
		}
	})
}

// TestReleaseFailureIsReported pins that a mutex left held is not swallowed: it
// blocks every other replica, and a run that reported success would leave the
// host with nothing to go on.
func TestReleaseFailureIsReported(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		lock := &probeLock{handle: handle, releaseErr: errors.New("the connection went away")}
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, lock)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("a mutex that would not be released reported %v, want %v", err, ErrMigrationFailed)
		}
		if !errors.Is(err, lock.releaseErr) {
			t.Errorf("the error %v does not carry the reason the mutex stayed held", err)
		}
	})
}

// TestLockTimeoutKeepsItsOwnSentinel pins that a timeout is not reclassified.
//
// The two sentinels exist apart because the responses differ: a timeout sends
// the host to the replica holding the mutex, a migration failure to the module
// that declared the statement. Wrapping the timeout in ErrMigrationFailed would
// make both match and point at the wrong thing.
func TestLockTimeoutKeepsItsOwnSentinel(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		lock := &probeLock{
			handle:     handle,
			acquireErr: fmt.Errorf("%w: waited 1s for another replica", ErrMigrationLockTimeout),
		}
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, lock)
		if !errors.Is(err, ErrMigrationLockTimeout) {
			t.Fatalf("a mutex that timed out reported %v, want %v", err, ErrMigrationLockTimeout)
		}
		if errors.Is(err, ErrMigrationFailed) {
			t.Error("the timeout also matches ErrMigrationFailed, so the host cannot tell a replica " +
				"holding the mutex from a migration that will not apply")
		}
		if lock.releases != 0 {
			t.Errorf("the mutex was released %d times although it was never taken", lock.releases)
		}
		if hasTable(t, handle, recordTableName) || hasTable(t, handle, "widgets") {
			t.Error("the run touched the database although the mutex was never taken")
		}
	})
}

// TestFailingToTakeTheMutexIsErrMigrationFailed pins the other acquire failure.
// A mutex that could not be taken for any reason other than the timeout is a
// startup failure of the migration kind.
func TestFailingToTakeTheMutexIsErrMigrationFailed(t *testing.T) {
	forEachDialect(t, func(t *testing.T, dialect Dialect, handle *gorm.DB) {
		lock := &probeLock{handle: handle, acquireErr: errors.New("no connection to take it on")}
		reg := registryOf(declaring("catalog", migrationSet(dialect, map[string]string{
			"0001_create_widgets.sql": createWidgets,
		})))

		err := applyMigrations(t.Context(), reg, handle, dialect, lock)
		if !errors.Is(err, ErrMigrationFailed) {
			t.Fatalf("a mutex that could not be taken reported %v, want %v", err, ErrMigrationFailed)
		}
		if !errors.Is(err, lock.acquireErr) {
			t.Errorf("the error %v does not carry why the mutex could not be taken", err)
		}
	})
}

// startupWith drives the real lifecycle over the given modules, with a stand-in
// for the database module applying the migrations in its Migrate stage. It is
// how a case reaches a module's enablement verdict, which only exists after
// resolution has run.
func startupWith(t *testing.T, handle *gorm.DB, dialect Dialect, modules ...core.Module) error {
	t.Helper()
	reg := registryOf(modules...)
	reg.Register(core.Module{
		Name: "db.fixture",
		Migrate: func(ctx context.Context, reg *core.Registry, _ any) error {
			return applyMigrations(ctx, reg, handle, dialect, nil)
		},
	})
	served := make(chan struct{})
	reg.Register(core.Module{
		Name:  "startup.probe",
		Serve: func(context.Context, *core.Registry, any) error { close(served); return nil },
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	select {
	case err := <-done:
		return err
	case <-served:
	case <-time.After(30 * time.Second):
		t.Fatal("the startup never reached the Serve stage")
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
		return nil
	}
}

// allErrors flattens an error tree, following both the single-error and the
// multi-error form of Unwrap.
func allErrors(err error) []error {
	if err == nil {
		return nil
	}
	out := []error{err}
	if next := errors.Unwrap(err); next != nil {
		return append(out, allErrors(next)...)
	}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		for _, cause := range joined.Unwrap() {
			out = append(out, allErrors(cause)...)
		}
	}
	return out
}
