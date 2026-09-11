package dbkit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// ErrNilModule is returned by (*MigrationRegistry).Register when m is nil.
var ErrNilModule = errors.New("dbkit: module is nil")

// ErrEmptyModuleName is returned by (*MigrationRegistry).Register when
// m.Name() is empty.
var ErrEmptyModuleName = errors.New("dbkit: module name is empty")

// ErrDuplicateModule is returned by (*MigrationRegistry).Register when a
// module with the same Name was already registered.
var ErrDuplicateModule = errors.New("dbkit: duplicate module name")

// ErrDependencyCycle is returned by (*MigrationRegistry).Apply when the
// registered modules' DependsOn declarations form a cycle.
var ErrDependencyCycle = errors.New("dbkit: module dependency cycle")

// ErrMissingDependency is returned by (*MigrationRegistry).Apply when a
// registered module depends on a module name that was never registered.
var ErrMissingDependency = errors.New("dbkit: missing module dependency")

// ErrUnknownDialect is returned by (*MigrationRegistry).Apply when dialect is
// neither DialectPostgres nor DialectSQLite.
var ErrUnknownDialect = errors.New("dbkit: unknown migration dialect")

// migratable is the shape MigrationRegistry consumes: a module name, the
// names it depends on, and its embedded migration set. It is declared here,
// structurally, instead of naming a module contract: dbkit sits above
// pkgcore in the dependency graph and reads only these three things, so any
// module type satisfies it as written, and MigrationRegistry stays usable
// from the test fixtures and migration suites that build throwaway modules.
type migratable interface {
	// Name is the module's unique name; the schema_migrations table records
	// it per applied set.
	Name() string
	// DependsOn names the modules whose sets apply before this one's.
	DependsOn() []string
	// Migrations carries the module's versioned SQL migrations, one
	// subdirectory per dialect ("postgres", "sqlite").
	Migrations() embed.FS
}

// schemaMigrationsTable is the name of the table MigrationRegistry uses to
// record which (module, filename) migration files have already been
// applied, so that re-running Apply is idempotent.
const schemaMigrationsTable = "schema_migrations"

// createSchemaMigrationsTableSQL creates dbkit's own migration-bookkeeping
// table.
//
// Bootstrapping exception: every other table in the system is created by a
// versioned migration file living under a module's own
// migrations/{postgres,sqlite}/ directory and applied by this very registry.
// schema_migrations cannot be brought into existence that way, because Apply
// needs the table to already exist before it can look up which migration
// files have already run -- there is no earlier point at which a
// "migration" for the tracking table itself could have been tracked. It is
// therefore the one table dbkit creates imperatively, with a plain,
// idempotent CREATE TABLE IF NOT EXISTS, every time Apply runs, instead of
// through a module's migration file. The statement is written to be
// portable across both supported dialects (VARCHAR/TIMESTAMP, no
// PostgreSQL- or SQLite-specific syntax), so this one necessary exception
// never needs a dialect branch of its own.
const createSchemaMigrationsTableSQL = `CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable + ` (
	module     VARCHAR(255) NOT NULL,
	filename   VARCHAR(255) NOT NULL,
	applied_at TIMESTAMP NOT NULL,
	PRIMARY KEY (module, filename)
)`

// schemaMigration is one row of dbkit's migration-bookkeeping table. It is
// an internal implementation detail of MigrationRegistry, not a
// tenant-scoped or platform-data model in the sense the rest of dbkit deals
// with: it carries no tenant_id, because it describes the shape of the
// schema itself, which is identical for every tenant.
type schemaMigration struct {
	Module    string    `gorm:"column:module;primaryKey"`
	Filename  string    `gorm:"column:filename;primaryKey"`
	AppliedAt time.Time `gorm:"column:applied_at"`
}

// TableName pins schemaMigration to schemaMigrationsTable, so it does not
// depend on GORM's pluralization of the (unexported) type name.
func (schemaMigration) TableName() string { return schemaMigrationsTable }

// MigrationRegistry aggregates the SQL migrations declared by every
// registered migratable and applies them, in dependency order, against a
// target database.
//
// Each registered module's Migrations() embed.FS is expected to contain SQL
// files under "postgres/*.sql" and "sqlite/*.sql", one subdirectory per
// dialect, named so that a plain lexical sort gives the intended apply order
// (the "0001_", "0002_", ... convention already used by every module's
// fixtures). Apply reads only the subdirectory matching the Dialect it is
// called with; a module with no subdirectory at all for that dialect is
// treated as declaring zero migrations for it, not as an error.
//
// The zero value is not ready to use; construct one with NewMigrationRegistry.
// A *MigrationRegistry is safe for concurrent use.
type MigrationRegistry struct {
	mu      sync.Mutex
	modules []migratable
	byName  map[string]struct{}
}

// NewMigrationRegistry returns an empty MigrationRegistry, ready to accept
// modules through Register.
func NewMigrationRegistry() *MigrationRegistry {
	return &MigrationRegistry{byName: make(map[string]struct{})}
}

// Register adds m to the registry.
//
// It reads m.Name(), m.DependsOn() and m.Migrations() itself, at Apply time
// -- the value is self-describing, so callers never pass those separately.
//
// Register returns an error, and registers nothing, when m is nil, when
// m.Name() is empty, or when a module with the same Name was already
// registered. It deliberately does not validate DependsOn: a module may
// legitimately be Registered before the modules it depends on (Apply orders
// them by dependency, not by registration order), so a dependency naming a
// module not yet seen is not knowable as "missing" until the full set has
// been registered. Cycle and missing-dependency detection therefore both
// happen in Apply, once that is true.
func (r *MigrationRegistry) Register(m migratable) error {
	if m == nil {
		return ErrNilModule
	}
	name := m.Name()
	if name == "" {
		return ErrEmptyModuleName
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateModule, name)
	}
	r.byName[name] = struct{}{}
	r.modules = append(r.modules, m)
	return nil
}

// Apply brings db up to date with every migration file declared by the
// modules registered so far, for the given dialect.
//
// It proceeds in four steps:
//
//  1. The registered modules are topologically sorted by DependsOn, so
//     every module is applied only after every module it depends on. A
//     dependency cycle, or a DependsOn entry naming a module that was never
//     registered, fails the whole call -- wrapping ErrDependencyCycle or
//     ErrMissingDependency respectively -- before a single statement runs.
//  2. dbkit's own schema_migrations bookkeeping table is created if it does
//     not already exist. See createSchemaMigrationsTableSQL for why this
//     one table is created imperatively rather than through a module's
//     migration file.
//  3. For each module, in the dependency order from step 1, its dialect
//     subdirectory ("postgres/*.sql" or "sqlite/*.sql") is read and its
//     files are applied in filename order -- the "0001_", "0002_", ...
//     convention.
//  4. A file already recorded in schema_migrations for that (module,
//     filename) pair is skipped rather than re-executed, which is what
//     makes calling Apply again, once nothing has changed, a no-op. A
//     module's not-yet-applied files execute together with their
//     schema_migrations rows in a single transaction, so that module ends
//     up either fully applied or, on any failure, entirely unchanged. Each
//     module gets its own, independent transaction, so a later module's
//     failure never rolls back a module an earlier iteration already
//     committed.
//
// ctx is checked before each module, so a cancelled context stops Apply
// between modules rather than only once every module has been attempted;
// ctx must not be nil.
//
// Cross-process coordination (PostgreSQL only). A real multi-replica
// distributed-mode boot can have N processes calling Apply against the same
// shared PostgreSQL database at the same time on first startup. Without
// coordination, two replicas' step-4 "already applied?" checks can both see
// "no" for the same not-yet-applied file before either's CREATE TABLE
// commits, so the second replica's statement fails with a duplicate-object
// error and that replica's boot fails -- a genuine first-boot hazard, not a
// theoretical one. When dialect is DialectPostgres, Apply therefore pins a
// single physical connection (gorm.DB.Connection) for the whole call and
// takes out a PostgreSQL session-level advisory lock on it before step 2
// (see acquireMigrationLock), releasing it after step 4 win or lose (see
// releaseMigrationLock); a second process's concurrent Apply call for the
// same database simply waits its turn -- bounded, so a genuinely stuck
// first replica produces a named ErrMigrationLockTimeout rather than an
// indefinite hang -- instead of racing this one. The lock is taken out
// around steps 2-4 as a whole, never inside a single module's own
// transaction, so it adds coordination without changing step 4's
// all-or-nothing-per-module guarantee in any way.
//
// SQLite gets no such lock: no supported composition ever runs two Apply
// calls concurrently against one SQLite database. The ordinary SQLite
// callers in this codebase -- the reference app's own startup Apply and
// saasctl's "db migrate" command -- are one process per file. The one
// composition that does run two processes against one shared SQLite file,
// the reference app's distributed-mode integration tier (its server
// hard-codes the SQLite dialect, so
// examples/reference-app/integration_test/distributed_mode_test.go shares a
// single APP_DB_PATH file across its two test replicas), staggers the two
// replicas' boots by design -- replica A boots fully, its startup Apply
// included, before replica B even starts, so B's own Apply always runs
// against an already-migrated schema and no-ops. Shared-file SQLite is not
// the database of any real multi-replica deployment shape here, so a lock
// on this path would protect against a hazard no supported composition can
// reach.
func (r *MigrationRegistry) Apply(ctx context.Context, db *gorm.DB, dialect Dialect) error {
	dir, err := dialectDir(dialect)
	if err != nil {
		return err
	}
	if db == nil {
		return errors.New("dbkit: Apply requires a non-nil *gorm.DB")
	}

	r.mu.Lock()
	modules := slices.Clone(r.modules)
	r.mu.Unlock()

	ordered, err := sortModulesByDependency(modules)
	if err != nil {
		return err
	}

	sources := make([]migrationSource, 0, len(ordered))
	for _, m := range ordered {
		sources = append(sources, migrationSource{name: m.Name(), fs: m.Migrations()})
	}
	return applySources(ctx, db, dialect, dir, sources)
}

// ApplyMigrations brings the database in reg's by-type context up to date
// with every selected component's migration set: it resolves the *gorm.DB
// the assembly constructed (the db component's product), derives the
// dialect from the connection's own dialector name, and applies the
// selected components' Assets -- which pkgcore returns in dependency order,
// so the sets land in the order the components were planned -- through the
// same machinery MigrationRegistry.Apply uses: dbkit's schema_migrations
// ledger, one transaction per component, and the PostgreSQL advisory lock
// around the whole run. A selected component carrying no migrations is
// skipped; the ledger key is the component's name, mirroring the module
// name a MigrationRegistry registration records under.
//
// It is the migration-application step of a db component's Verify callback:
// every product exists by then, so the assembled set is complete, and a
// dependency-free db component runs first in the stage, before any other
// component's Verify sees the schema.
func ApplyMigrations(ctx context.Context, reg *pkgcore.ComponentRegistry) error {
	if reg == nil {
		return errors.New("dbkit: ApplyMigrations requires a non-nil *pkgcore.ComponentRegistry")
	}
	db, err := pkgcore.Get[*gorm.DB](reg)
	if err != nil {
		return fmt.Errorf("dbkit: apply migrations: %w", err)
	}

	dialect := Dialect(db.Name())
	dir, err := dialectDir(dialect)
	if err != nil {
		return fmt.Errorf("dbkit: apply migrations: %w", err)
	}

	var zeroFS embed.FS
	var sources []migrationSource
	for _, asset := range pkgcore.Assets(reg) {
		if asset.Migrations == zeroFS {
			continue
		}
		sources = append(sources, migrationSource{name: asset.Name, fs: asset.Migrations})
	}

	return applySources(ctx, db, dialect, dir, sources)
}

// migrationSource is one named migration set to apply: the name the
// schema_migrations ledger records its files under, and the embed.FS
// carrying them. Both registration paths produce sources -- Apply converts
// each registered migratable (name from Name(), set from Migrations()),
// ApplyMigrations each selected component's Asset (name and set as the
// component declared them) -- so the ledger semantics are identical
// whichever path a boot takes.
type migrationSource struct {
	name string
	fs   embed.FS
}

// applySources runs the shared application sequence against db: on
// PostgreSQL the whole run executes on one pinned connection under the
// session-level advisory lock (see Apply's own doc comment for why), on
// every other dialect directly against db.
func applySources(ctx context.Context, db *gorm.DB, dialect Dialect, dir string, sources []migrationSource) error {
	if dialect != DialectPostgres {
		return applyOrdered(ctx, db, dir, sources)
	}
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
		if err := acquireMigrationLock(ctx, conn); err != nil {
			return err
		}
		// releaseMigrationLock runs on the same pinned connection with a
		// context that has ctx's values but never its cancellation -- see
		// its own doc comment for why an already-cancelled ctx here must
		// never stop the unlock statement itself from running.
		defer releaseMigrationLock(context.WithoutCancel(ctx), conn)
		return applyOrdered(ctx, conn, dir, sources)
	})
}

// applyOrdered creates dbkit's own schema_migrations bookkeeping table (see
// createSchemaMigrationsTableSQL) if it does not already exist, then applies
// every source in ordered, in order, via applyModule. It is the shared
// step 2-4 body of Apply and ApplyMigrations, factored out so the
// PostgreSQL branch can run it inside the single physical connection its
// advisory lock requires (see acquireMigrationLock), while the
// non-PostgreSQL branch runs it directly against db with no such wrapping.
func applyOrdered(ctx context.Context, db *gorm.DB, dir string, ordered []migrationSource) error {
	if err := db.WithContext(ctx).Exec(createSchemaMigrationsTableSQL).Error; err != nil {
		return fmt.Errorf("dbkit: create %s table: %w", schemaMigrationsTable, err)
	}

	for _, s := range ordered {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("dbkit: apply stopped before module %q: %w", s.name, err)
		}
		if err := applyModule(ctx, db, dir, s); err != nil {
			return fmt.Errorf("dbkit: module %q: %w", s.name, err)
		}
	}
	return nil
}

// migrationLockNamespace is dbkit's own fixed identifier for the PostgreSQL
// session-level advisory lock Apply takes out for the whole duration of one
// PostgreSQL migration run (see acquireMigrationLock). It is a constant,
// deliberately never derived from anything a caller supplies:
// pg_advisory_lock's key space (a single 64-bit integer) is shared by every
// advisory-lock user of the target database, so a caller-suppliable key
// could collide with an unrelated advisory-lock user; a fixed, dbkit-owned
// namespace string hashed into the key guarantees it never does.
const migrationLockNamespace = "dbkit.migrations.apply.v1"

// migrationAdvisoryLockKey deterministically derives the single bigint
// pg_advisory_lock / pg_try_advisory_lock / pg_advisory_unlock key from
// migrationLockNamespace -- the same key on every call, in every process,
// against every database -- via FNV-1a, a wide, well-distributed
// non-cryptographic hash. The resulting 64-bit pattern is reinterpreted as a
// signed int64 (the type pg_advisory_lock's own parameter takes); advisory
// lock keys have no notion of sign, so this is a plain bit reinterpretation,
// never a range reduction that would narrow the key space.
func migrationAdvisoryLockKey() int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(migrationLockNamespace))
	return int64(h.Sum64()) //nolint:gosec // deliberate bit reinterpretation, not a narrowing conversion
}

// ErrMigrationLockTimeout is returned (wrapped) by Apply when a PostgreSQL
// migration run could not acquire the migration advisory lock (see
// acquireMigrationLock) within migrationLockMaxWait, or ctx was done first.
// It means a concurrent replica genuinely held the lock for the whole wait
// -- most often because it is still running its own first-boot Apply, more
// rarely because it is stuck -- and this replica's boot must be retried
// rather than silently proceeding unlocked.
var ErrMigrationLockTimeout = errors.New("dbkit: timed out waiting for the migration advisory lock")

const (
	// migrationLockRetryInitialDelay is the first backoff delay between
	// failed pg_try_advisory_lock attempts.
	migrationLockRetryInitialDelay = 50 * time.Millisecond
	// migrationLockRetryMaxDelay caps the exponential backoff between
	// attempts, so a long wait still polls at a bounded rate rather than
	// slowing down without limit.
	migrationLockRetryMaxDelay = 2 * time.Second
	// migrationLockMaxWait is the internal ceiling acquireMigrationLock
	// waits before giving up with ErrMigrationLockTimeout, applied in
	// addition to -- never instead of -- ctx's own deadline: a caller
	// passing context.Background() (no deadline of its own) still gets a
	// bounded wait rather than a true indefinite hang if the first replica
	// is genuinely stuck. This is the deliberate reason Apply's PostgreSQL
	// coordination is a bounded pg_try_advisory_lock retry loop rather than
	// a single blocking pg_advisory_lock call: a blocking call's only bound
	// would be ctx cancellation, which depends on the database driver
	// correctly propagating a context cancellation into an abandoned server
	// wait -- true of this codebase's pgx driver, but not a property this
	// mechanism should have to rely on to fail safely. A bounded retry loop
	// is simpler to prove correct (a fixed, inspectable polling loop) and
	// guarantees a named, diagnosable error surfaces here even against a
	// caller that never sets a ctx deadline at all.
	migrationLockMaxWait = 2 * time.Minute
)

// acquireMigrationLock blocks until db's connection holds the PostgreSQL
// session-level advisory lock keyed by migrationAdvisoryLockKey, retrying
// pg_try_advisory_lock with exponential backoff (migrationLockRetryInitialDelay
// up to migrationLockRetryMaxDelay), or returns a wrapped
// ErrMigrationLockTimeout once migrationLockMaxWait elapses or ctx is done,
// whichever comes first.
//
// db must be a *gorm.DB whose ConnPool is pinned to one physical connection
// for the whole surrounding call (gorm.DB.Connection's own contract, which
// Apply uses for exactly this reason): pg_advisory_lock and
// pg_advisory_unlock are session-level primitives, so acquiring the lock on
// one pooled connection and later checking or releasing it on a different
// connection taken from the same pool would not observe the same lock at
// all -- database/sql's own pooling can otherwise hand out any physical
// connection for any statement.
func acquireMigrationLock(ctx context.Context, db *gorm.DB) error {
	waitCtx, cancel := context.WithTimeout(ctx, migrationLockMaxWait)
	defer cancel()

	key := migrationAdvisoryLockKey()
	delay := migrationLockRetryInitialDelay
	for {
		var acquired bool
		row := db.WithContext(waitCtx).Raw("SELECT pg_try_advisory_lock(?)", key).Row()
		if err := row.Scan(&acquired); err != nil {
			return fmt.Errorf("dbkit: acquire migration advisory lock: %w", err)
		}
		if acquired {
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrMigrationLockTimeout, waitCtx.Err())
		case <-timer.C:
		}
		if delay < migrationLockRetryMaxDelay {
			delay *= 2
			if delay > migrationLockRetryMaxDelay {
				delay = migrationLockRetryMaxDelay
			}
		}
	}
}

// releaseMigrationLock releases the advisory lock acquireMigrationLock took
// out, on the same pinned connection. Apply calls it, unconditionally, from
// its own defer once acquireMigrationLock has already succeeded -- including
// when applyOrdered itself failed -- so a failed migration run never leaves
// the lock held for a later Apply call, in this process or another, to wait
// out needlessly.
//
// ctx is expected to be a context.WithoutCancel-derived copy of the
// original call's context, not that context itself: the unlock statement
// must still be able to run even when the original ctx is already done (a
// timed-out or cancelled Apply call is exactly when releasing the lock
// matters most), and issuing it against an already-done context would make
// the unlock itself fail before it ever reached PostgreSQL, leaving the
// session -- and therefore the lock, until its pooled connection eventually
// closes for good -- stuck.
//
// A release failure is reported as a warning via log/slog (mirroring
// audit_capture.go's auditPublishFailed idiom, for the identical reason --
// dbkit cannot depend on go/observability) rather than returned: by this
// point Apply's own result is already determined, and there is nothing
// left to roll back. An operator seeing this warning knows the advisory
// lock may
// still be held by this session until its pooled connection is closed for
// good, and should investigate rather than assume a future Apply call that
// blocks on this lock will simply resolve itself.
func releaseMigrationLock(ctx context.Context, db *gorm.DB) {
	key := migrationAdvisoryLockKey()
	var released bool
	row := db.WithContext(ctx).Raw("SELECT pg_advisory_unlock(?)", key).Row()
	if err := row.Scan(&released); err != nil {
		slog.Default().WarnContext(ctx, "dbkit: migration advisory lock release failed", "error", err)
		return
	}
	if !released {
		slog.Default().WarnContext(ctx, "dbkit: migration advisory lock release reported not held")
	}
}

// applyModule applies every not-yet-applied migration file s declares for
// the dialect subdirectory dir, inside one transaction: either every file
// newly applied by this call commits together with its schema_migrations
// row, or -- on any failure -- none of them do, leaving the module exactly
// as it was before this call.
func applyModule(ctx context.Context, db *gorm.DB, dir string, s migrationSource) error {
	files, err := migrationFiles(s.fs, dir)
	if err != nil {
		return err
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, f := range files {
			applied, err := isApplied(tx, s.name, f.name)
			if err != nil {
				return err
			}
			if applied {
				continue
			}
			if err := tx.Exec(string(f.contents)).Error; err != nil {
				return fmt.Errorf("apply %s: %w", f.name, err)
			}
			if err := recordApplied(tx, s.name, f.name); err != nil {
				return fmt.Errorf("record %s as applied: %w", f.name, err)
			}
		}
		return nil
	})
}

// migrationFile is one migration file read from a module's Migrations()
// embed.FS.
type migrationFile struct {
	name     string
	contents []byte
}

// migrationFiles returns fsys's *.sql files under the top-level directory
// dir, sorted by filename so that the "0001_", "0002_", ... naming
// convention determines apply order. A module with no dir subdirectory at
// all declares zero migrations for that dialect, which is not an error; any
// other failure to read the directory or a file is.
func migrationFiles(fsys embed.FS, dir string) ([]migrationFile, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dbkit: read %s migrations: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		names = append(names, entry.Name())
	}
	slices.Sort(names)

	files := make([]migrationFile, 0, len(names))
	for _, name := range names {
		p := path.Join(dir, name)
		contents, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("dbkit: read %s: %w", p, err)
		}
		files = append(files, migrationFile{name: name, contents: contents})
	}
	return files, nil
}

// isApplied reports whether filename was already recorded as applied for
// module within tx.
func isApplied(tx *gorm.DB, module, filename string) (bool, error) {
	var count int64
	err := tx.Model(&schemaMigration{}).
		Where("module = ? AND filename = ?", module, filename).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("dbkit: check %s for %s/%s: %w", schemaMigrationsTable, module, filename, err)
	}
	return count > 0, nil
}

// recordApplied inserts a schema_migrations row marking filename as applied
// for module, timestamped with the current time.
func recordApplied(tx *gorm.DB, module, filename string) error {
	row := schemaMigration{Module: module, Filename: filename, AppliedAt: time.Now().UTC()}
	if err := tx.Create(&row).Error; err != nil {
		return fmt.Errorf("dbkit: insert %s row for %s/%s: %w", schemaMigrationsTable, module, filename, err)
	}
	return nil
}

// dialectDir validates dialect and returns the name of the subdirectory,
// under a module's Migrations() embed.FS, holding that dialect's SQL files.
// It is currently identical to the Dialect value itself ("postgres" or
// "sqlite"), which is why Apply needs no separate mapping table between the
// two; the validation still matters on its own, so that an unrecognized or
// zero-value Dialect is rejected with ErrUnknownDialect instead of silently
// looking for a directory that can never exist.
func dialectDir(dialect Dialect) (string, error) {
	switch dialect {
	case DialectPostgres, DialectSQLite:
		return string(dialect), nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownDialect, dialect)
	}
}

// moduleVisitState tracks a module's position in the depth-first traversal
// sortModulesByDependency runs to produce dependency order.
type moduleVisitState int

const (
	moduleUnvisited moduleVisitState = iota
	moduleVisiting
	moduleVisited
)

// sortModulesByDependency returns modules ordered so that every module
// appears after every module named in its own DependsOn. Input order breaks
// ties among modules with no dependency relationship, which keeps the apply
// order stable across runs given the same registrations.
//
// The component assembly orders components by their Requires edges, a
// different graph entirely; this traversal orders the registered migration
// sets by module dependency, so dbkit carries its own copy against the
// migratable shape rather than sharing the assembly's planner. Register
// already rejects a duplicate module Name at registration time, so unlike
// the assembly's planner this one does not need to detect that case
// again.
func sortModulesByDependency(modules []migratable) ([]migratable, error) {
	byName := make(map[string]migratable, len(modules))
	state := make(map[string]moduleVisitState, len(modules))
	for _, m := range modules {
		byName[m.Name()] = m
		state[m.Name()] = moduleUnvisited
	}

	ordered := make([]migratable, 0, len(modules))
	visitPath := make([]string, 0, len(modules))

	var visit func(m migratable) error
	visit = func(m migratable) error {
		name := m.Name()
		switch state[name] {
		case moduleVisited:
			return nil
		case moduleVisiting:
			return fmt.Errorf("%w: %s", ErrDependencyCycle, formatCycle(visitPath, name))
		case moduleUnvisited:
		}

		state[name] = moduleVisiting
		visitPath = append(visitPath, name)
		for _, dep := range m.DependsOn() {
			depModule, ok := byName[dep]
			if !ok {
				return fmt.Errorf("%w: module %q depends on %q, which is not registered", ErrMissingDependency, name, dep)
			}
			if err := visit(depModule); err != nil {
				return err
			}
		}
		visitPath = visitPath[:len(visitPath)-1]
		state[name] = moduleVisited
		ordered = append(ordered, m)
		return nil
	}

	for _, m := range modules {
		if err := visit(m); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// formatCycle renders the traversal path from the first occurrence of name
// back to name, so the error names every module that participates in the
// cycle, not just the two modules where it was detected.
func formatCycle(visitPath []string, name string) string {
	start := slices.Index(visitPath, name)
	if start < 0 {
		start = 0
	}
	cycle := append(slices.Clone(visitPath[start:]), name)
	return strings.Join(cycle, " -> ")
}
