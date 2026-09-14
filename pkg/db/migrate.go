package db

import (
	"cmp"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
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
// Both halves are handed the connection the whole run is pinned to, and both
// have to use it. A mutex of this kind belongs to the session that took it,
// not to a transaction and not to a pool: taken on one connection and given up
// on another, it is never given up at all. The run's own statements travel on
// the same connection, so the mutex stays held across every BEGIN and COMMIT
// between the two calls without any of them touching it.
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
	// Acquire takes the mutex on conn, waiting for another process to
	// release it.
	Acquire(ctx context.Context, conn *sql.Conn) error
	// Release gives the mutex up on conn, which is the connection it was
	// taken on. It runs on the failing path too, so a migration that did not
	// apply does not strand the other replicas.
	Release(ctx context.Context, conn *sql.Conn) error
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
// One connection is borrowed from the handle's pool and kept for the whole run.
// Taking the mutex, creating the record table, applying each migration and
// giving the mutex up all travel on it, and it goes back to the pool at the
// end. A mutex an implementation supplies is scoped to a session, so it and the
// statements it exists to order have to sit on the same connection; and being
// on the same one, the run occupies one connection rather than two, which is
// what makes a pool of one enough to start on.
//
// The whole run happens under lock when the dialect supplies one. The order is
// the modules' dependency order, and file-name order within a module; only the
// subdirectory named after the running dialect is read, and a declaration
// without one contributes nothing rather than failing.
func applyMigrations(ctx context.Context, reg *core.Registry, handle *gorm.DB, dialect Dialect, lock MigrationLock) (err error) {
	pool, err := handle.DB()
	if err != nil {
		return fmt.Errorf("%w: the %s handle has no connection pool to apply migrations on: %w",
			ErrMigrationFailed, dialect, err)
	}
	conn, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%w: taking the connection to apply the %s migrations on failed: %w",
			ErrMigrationFailed, dialect, err)
	}
	// Set when the mutex would not be given up: the connection is then
	// thrown away rather than handed back. See discard.
	stranded := false
	defer func() {
		if stranded {
			err = errors.Join(err, discard(conn))
			return
		}
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"%w: handing the migration run's connection back to the pool failed: %w",
				ErrMigrationFailed, closeErr))
		}
	}()

	if lock != nil {
		if lockErr := lock.Acquire(ctx, conn); lockErr != nil {
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
			// Read before the join below changes it: whether every
			// migration went in is what the message has to say.
			applied := err == nil
			if releaseErr := lock.Release(ctx, conn); releaseErr != nil {
				stranded = true
				err = errors.Join(err, releaseFailed(dialect, applied, releaseErr))
			}
		}()
	}
	return applyUnderLock(reg, pinned(ctx, handle, conn), dialect)
}

// pinned returns a session that issues every statement on one connection.
//
// GORM reaches the database through the connection pool on the statement, and a
// *sql.Conn is one of those: pointing the session at the borrowed connection is
// what keeps the run — and the session-scoped mutex around it — on that
// connection instead of on whichever one the pool hands out next. Transactions
// the session starts begin on it too.
func pinned(ctx context.Context, handle *gorm.DB, conn *sql.Conn) *gorm.DB {
	session := handle.WithContext(ctx)
	session.Statement.ConnPool = conn
	return session
}

// releaseFailed builds the error a run reports when the mutex would not be
// given up.
//
// It says where the failure fell, because the sentinel alone does not. A run
// whose migrations all applied and then could not release the mutex reads, from
// ErrMigrationFailed on its own, as a migration that would not apply, and the
// operator goes through migration files that have nothing wrong with them. The
// other direction matters as much: after a run that had already failed, the
// text must not claim the migrations went in.
//
// Either way the startup stops, and the reason is that the mutex belongs to a
// session. A process that carried on would keep that session — for its whole
// life, in the ordinary case — with every other replica waiting out its
// allowance against it. Stopping ends the process, the session ends with it,
// and the server releases the mutex.
func releaseFailed(dialect Dialect, applied bool, cause error) error {
	if applied {
		return fmt.Errorf("%w: every migration was applied and recorded, and what failed afterwards is "+
			"giving the migration mutex on the %s database up — there is nothing wrong with the "+
			"migrations. This startup stops so that the session holding the mutex ends and the other "+
			"replicas are not left waiting for it: %w",
			ErrMigrationFailed, dialect, cause)
	}
	return fmt.Errorf("%w: the migration run did not finish, and giving the migration mutex on the %s "+
		"database up afterwards failed as well, so the other replicas wait for it until this session "+
		"ends: %w",
		ErrMigrationFailed, dialect, cause)
}

// discard throws the run's connection away instead of handing it back to the
// pool.
//
// It is what follows a mutex that could not be given up. The mutex belongs to
// the session on that connection, so ending the session is the one remaining
// way to release it: handing the connection back would keep the session alive,
// with the mutex on it, and pass it to the next caller in that state.
func discard(conn *sql.Conn) error {
	// Raw reports the error the callback returned; ErrBadConn is what asks
	// database/sql to drop the underlying connection rather than reuse it,
	// so seeing it come back is the success case. ErrConnDone is the other
	// success case: database/sql has already thrown the connection away,
	// which is the same end — the session is over and the mutex with it.
	err := conn.Raw(func(any) error { return driver.ErrBadConn })
	if err != nil && !errors.Is(err, driver.ErrBadConn) && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("%w: dropping the migration run's connection failed, and the mutex stays held "+
			"until the server drops that session: %w", ErrMigrationFailed, err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("%w: closing the migration run's connection failed: %w", ErrMigrationFailed, err)
	}
	return nil
}

// applyUnderLock is the run itself, with the mutex already held, on the session
// pinned to the connection the mutex was taken on.
func applyUnderLock(reg *core.Registry, session *gorm.DB, dialect Dialect) error {
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
//
// The file goes to the driver whole, with no bind variables, and the driver's
// own path for a text of several statements executes all of them. A file may
// hold more than one: creating a table and then its indexes is one migration,
// and splitting it across two files only scatters one change. Splitting the
// text here instead would mean parsing SQL — semicolons inside string literals
// and function bodies have to come out right — and that is not this module's
// work. Running a file of several statements is therefore the dialect's
// admission condition too, alongside putting DDL inside a transaction.
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
			if err := admit(module, dialect, entry); err != nil {
				return nil, err
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

// migrationFileExtension is the only extension a migration file carries.
const migrationFileExtension = ".sql"

// admit reports why an entry of a dialect's migration directory cannot be
// applied, or nil when it can.
//
// The directory holds regular .sql files and nothing else, and anything outside
// that is an error rather than something to pass over. Both of the wider rules
// hide a failure instead: applying every regular file sends a note left in
// there to the database to be executed, and applying the .sql files while
// ignoring the rest turns a migration whose name was typed wrong into a table
// that is simply never created — found at run time, a long way from the start
// that skipped it.
func admit(module string, dialect Dialect, entry fs.DirEntry) error {
	switch {
	case entry.IsDir():
		// Nothing descends into it, so passing over it would drop
		// whatever it holds without a word.
		return fmt.Errorf("%w: module %q has a directory %q inside its %q migrations, and only %s files "+
			"are applied. Move the migrations up into %q or drop the directory",
			ErrMigrationFailed, module, entry.Name(), dialect, migrationFileExtension, dialect)
	case !entry.Type().IsRegular():
		return fmt.Errorf("%w: %q inside module %q's %q migrations is not a regular file, and only "+
			"regular %s files are applied",
			ErrMigrationFailed, entry.Name(), module, dialect, migrationFileExtension)
	case path.Ext(entry.Name()) != migrationFileExtension:
		return fmt.Errorf("%w: module %q has %q inside its %q migrations, and only %s files are applied. "+
			"Rename it if it is a migration; move it out of the %q directory if it is not",
			ErrMigrationFailed, module, entry.Name(), dialect, migrationFileExtension, dialect)
	}
	return nil
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
