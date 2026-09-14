package db

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
)

// MigrationLock is the cross-process mutex the migration run is held under. An
// implementation subpackage supplies one when its dialect appears in
// multi-replica deployments; a dialect that does not supplies nothing and the
// run proceeds without a mutex.
//
// What it defends against only happens on a first start against an empty
// database, and never inside one process: several replicas start at once, each
// reads the record table and finds nothing applied, each runs the same CREATE
// TABLE, and every replica after the first fails because the object already
// exists. A single-process test cannot reproduce it, which is why the seam is
// here rather than folded into the engine.
//
// Acquire waits for the mutex and reports ErrMigrationLockTimeout, unwrapped
// into no other sentinel, once the implementation's own deadline passes: a
// replica stuck holding the mutex must not hang the rest forever, and the
// response to a timeout — find out what the other replica is doing — differs
// from the response to a migration that would not apply.
//
// The mutex is taken before the record table is created and released after the
// last migration has been applied, so it never sits inside a single migration's
// transaction: it orders processes against each other and does not change what
// one migration's execution and its record are atomic with.
type MigrationLock interface {
	// Acquire takes the mutex, waiting for another process to release it.
	Acquire(ctx context.Context) error
	// Release gives the mutex up. It runs on the failing path too, so a
	// migration that did not apply does not strand the other replicas.
	Release(ctx context.Context) error
}

// recordTableName is the table that records which migrations have been applied.
// A row per applied migration, keyed by the declaring module and the file name.
//
// It is created by an unconditionally reentrant statement before every run,
// rather than by anyone's migration set: reading it is how the run learns what
// is already applied, so it has no moment of its own that could be tracked.
// Nothing in the three statements below is dialect-specific, so they are not
// split per dialect.
const recordTableName = "db_migrations"

const (
	createRecordTable = `CREATE TABLE IF NOT EXISTS db_migrations (
	module TEXT NOT NULL,
	file TEXT NOT NULL,
	applied_at TIMESTAMP NOT NULL,
	PRIMARY KEY (module, file)
)`
	selectRecords = `SELECT module, file FROM db_migrations`
	insertRecord  = `INSERT INTO db_migrations (module, file, applied_at) VALUES (?, ?, ?)`
)

// migration is one file from one module's declaration, ready to apply.
type migration struct {
	module string
	file   string
	body   string
}

// recordKey identifies an applied migration the way the record table's primary
// key does.
type recordKey struct {
	module string
	file   string
}

// applyMigrations collects every declared migration set, applies what the
// record table does not already list, and records what it applied.
//
// The whole run happens under lock when the dialect supplies one. The order is
// the modules' dependency order, and file-name order within a module; only the
// subdirectory named after the running dialect is read, and a declaration
// without one contributes nothing rather than failing.
func applyMigrations(ctx context.Context, reg *core.Registry, handle *gorm.DB, dialect Dialect, lock MigrationLock) (err error) {
	if lock != nil {
		if lockErr := lock.Acquire(ctx); lockErr != nil {
			// A timeout travels as it is. Wrapping it in
			// ErrMigrationFailed would make both sentinels match and
			// point the host at the migrations, when what it has to
			// look at is the replica holding the mutex.
			if errors.Is(lockErr, ErrMigrationLockTimeout) {
				return lockErr
			}
			return fmt.Errorf("%w: taking the migration mutex on the %s database failed: %w",
				ErrMigrationFailed, dialect, lockErr)
		}
		defer func() {
			if releaseErr := lock.Release(ctx); releaseErr != nil {
				// Joined rather than swallowed: a mutex left held
				// blocks every other replica's startup, and the
				// failure that led here is still worth reporting.
				err = errors.Join(err, fmt.Errorf(
					"%w: releasing the migration mutex on the %s database failed, and the "+
						"other replicas stay blocked until this connection drops: %w",
					ErrMigrationFailed, dialect, releaseErr))
			}
		}()
	}
	return applyUnderLock(ctx, reg, handle, dialect)
}

// applyUnderLock is the run itself, with the mutex already held.
func applyUnderLock(ctx context.Context, reg *core.Registry, handle *gorm.DB, dialect Dialect) error {
	session := handle.WithContext(ctx)
	if err := session.Exec(createRecordTable).Error; err != nil {
		return fmt.Errorf("%w: creating the migration record table %q failed: %w",
			ErrMigrationFailed, recordTableName, err)
	}
	applied, err := readRecords(session)
	if err != nil {
		return err
	}
	planned, err := collectMigrations(reg, dialect)
	if err != nil {
		return err
	}
	for _, m := range planned {
		if _, done := applied[recordKey{module: m.module, file: m.file}]; done {
			continue
		}
		if err := applyOne(session, m); err != nil {
			return err
		}
	}
	return nil
}

// readRecords reads what the record table already lists.
func readRecords(session *gorm.DB) (map[recordKey]struct{}, error) {
	var rows []struct {
		Module string
		File   string
	}
	if err := session.Raw(selectRecords).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: reading the migration record table %q failed: %w",
			ErrMigrationFailed, recordTableName, err)
	}
	applied := make(map[recordKey]struct{}, len(rows))
	for _, row := range rows {
		applied[recordKey{module: row.Module, file: row.File}] = struct{}{}
	}
	return applied, nil
}

// applyOne executes a migration and records it in one transaction.
//
// The two halves are inseparable: recorded but not executed would skip it
// forever, executed but not recorded would run it again on the next start and
// fail on an object that already exists — and that failure would surface on a
// later startup rather than on the one that caused it. Holding them together
// rests on the dialect putting DDL inside a transaction, which is an admission
// condition every implementation subpackage has to meet.
func applyOne(session *gorm.DB, m migration) error {
	err := session.Transaction(func(tx *gorm.DB) error {
		if execErr := tx.Exec(m.body).Error; execErr != nil {
			return fmt.Errorf("executing it failed: %w", execErr)
		}
		if recordErr := tx.Exec(insertRecord, m.module, m.file, time.Now().UTC()).Error; recordErr != nil {
			return fmt.Errorf("recording it in %q failed: %w", recordTableName, recordErr)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: migration %q declared by module %q: %w",
			ErrMigrationFailed, m.file, m.module, err)
	}
	return nil
}

// collectMigrations turns the declared resources into the list to apply, in the
// order to apply it.
//
// Modules that resolution left out are skipped: their tables are not created.
// An enabled module's migration may still reference one of those tables, and
// that shows up here as a migration that will not apply — nothing reads the
// statements, so it cannot be caught at declaration time.
func collectMigrations(reg *core.Registry, dialect Dialect) ([]migration, error) {
	rank := dependencyRank(reg)

	byModule := make(map[string][]Migrations)
	var modules []string
	for _, declared := range core.Resources[Migrations](reg) {
		if state, known := reg.Enablement(declared.Module); known && state.State == core.StateDisabled {
			continue
		}
		if _, seen := byModule[declared.Module]; !seen {
			modules = append(modules, declared.Module)
		}
		byModule[declared.Module] = append(byModule[declared.Module], declared.Value)
	}
	slices.SortFunc(modules, func(a, b string) int {
		if order := cmp.Compare(rank[a], rank[b]); order != 0 {
			return order
		}
		return cmp.Compare(a, b)
	})

	var planned []migration
	for _, module := range modules {
		files, err := filesFor(module, byModule[module], dialect)
		if err != nil {
			return nil, err
		}
		planned = append(planned, files...)
	}
	return planned, nil
}

// filesFor reads one module's migrations for the running dialect, in file-name
// order. File name is therefore the whole of the order within a module, which
// is what a 0001_ prefix exists to line up with the intended sequence.
func filesFor(module string, sets []Migrations, dialect Dialect) ([]migration, error) {
	var files []migration
	seen := make(map[string]struct{})
	for _, set := range sets {
		if set.FS == nil {
			continue
		}
		dir, err := fs.Sub(set.FS, string(dialect))
		if err != nil {
			return nil, fmt.Errorf("%w: reaching the %q migrations declared by module %q failed: %w",
				ErrMigrationFailed, dialect, module, err)
		}
		entries, err := fs.ReadDir(dir, ".")
		if err != nil {
			// No subdirectory for this dialect is zero migrations for
			// it, not a failure: that is how a module states it does
			// not support this engine.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("%w: reading the %q migrations declared by module %q failed: %w",
				ErrMigrationFailed, dialect, module, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				// Nothing descends into it, so passing over it
				// would drop whatever it holds without a word.
				return nil, fmt.Errorf("%w: module %q has a directory %q inside its %q migrations, "+
					"and only files are applied. Move the migrations up into %q or drop the directory",
					ErrMigrationFailed, module, entry.Name(), dialect, dialect)
			}
			if _, duplicate := seen[entry.Name()]; duplicate {
				// The record table is keyed by module and file
				// name, so the second one would be taken for the
				// first and never applied.
				return nil, fmt.Errorf("%w: module %q declares %q twice among its %q migrations. "+
					"The record table is keyed by module and file name, so the second one would "+
					"count as applied without ever running",
					ErrMigrationFailed, module, entry.Name(), dialect)
			}
			seen[entry.Name()] = struct{}{}
			body, err := fs.ReadFile(dir, entry.Name())
			if err != nil {
				return nil, fmt.Errorf("%w: reading migration %q declared by module %q failed: %w",
					ErrMigrationFailed, entry.Name(), module, err)
			}
			files = append(files, migration{module: module, file: entry.Name(), body: string(body)})
		}
	}
	slices.SortFunc(files, func(a, b migration) int { return cmp.Compare(a.file, b.file) })
	return files, nil
}

// dependencyRank places every registered module in dependency order and reports
// each one's position.
//
// The order comes from the Requires declarations the modules already carry: a
// module whose tables reference another module's tables declares a dependency
// on that module's capability, and gets its migrations applied after it. There
// is no second ordering mechanism, and without that declaration there is no
// order between the two.
//
// core plans construction from the same relation but does not export the
// result, so the topological sort is repeated here. Ties go to the module name,
// so the same set of modules yields the same order between builds.
func dependencyRank(reg *core.Registry) map[string]int {
	modules := reg.Modules()
	names := make([]string, 0, len(modules))
	byName := make(map[string]core.Module, len(modules))
	providers := make(map[reflect.Type][]string)
	for _, m := range modules {
		names = append(names, m.Name)
		byName[m.Name] = m
		for _, provision := range m.Provides {
			if ct := capabilityOf(provision.Token); ct != nil {
				providers[ct] = append(providers[ct], m.Name)
			}
		}
	}
	slices.Sort(names)

	edges := make(map[string]map[string]bool, len(names))
	indegree := make(map[string]int, len(names))
	for _, name := range names {
		edges[name] = make(map[string]bool)
		indegree[name] = 0
	}
	for _, name := range names {
		for _, requirement := range byName[name].Requires {
			ct := capabilityOf(requirement.Token)
			if ct == nil {
				continue
			}
			for _, provider := range providers[ct] {
				if provider == name || edges[provider][name] {
					continue
				}
				edges[provider][name] = true
				indegree[name]++
			}
		}
	}

	ready := make([]string, 0, len(names))
	for _, name := range names {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	rank := make(map[string]int, len(names))
	placed := make(map[string]bool, len(names))
	for len(ready) > 0 {
		slices.Sort(ready)
		name := ready[0]
		ready = ready[1:]
		rank[name] = len(rank)
		placed[name] = true
		for _, to := range slices.Sorted(maps.Keys(edges[name])) {
			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
			}
		}
	}
	// A cycle cannot reach this point through the lifecycle — core refuses
	// to construct one — but a registry assembled by hand can hold one, and
	// ordering is not the place to fail a startup over it. Whatever is left
	// keeps its name order behind everything that was placed.
	for _, name := range names {
		if !placed[name] {
			rank[name] = len(rank)
		}
	}
	return rank
}

// capabilityOf gives the interface type a token designates, or nil for a token
// core would have refused at registration.
func capabilityOf(token core.Token) reflect.Type {
	rt := reflect.TypeOf(token)
	if rt == nil || rt.Kind() != reflect.Pointer {
		return nil
	}
	return rt.Elem()
}
