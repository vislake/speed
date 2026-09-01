package dbkit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
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
// registered pkgcore.Module and applies them, in dependency order, against a
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
	modules []pkgcore.Module
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
// -- a pkgcore.Module is self-describing, so callers never pass those
// separately.
//
// Register returns an error, and registers nothing, when m is nil, when
// m.Name() is empty, or when a module with the same Name was already
// registered. It deliberately does not validate DependsOn: a module may
// legitimately be Registered before the modules it depends on (Apply orders
// them by dependency, not by registration order), so a dependency naming a
// module not yet seen is not knowable as "missing" until the full set has
// been registered. Cycle and missing-dependency detection therefore both
// happen in Apply, once that is true.
func (r *MigrationRegistry) Register(m pkgcore.Module) error {
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

	if err := db.WithContext(ctx).Exec(createSchemaMigrationsTableSQL).Error; err != nil {
		return fmt.Errorf("dbkit: create %s table: %w", schemaMigrationsTable, err)
	}

	for _, m := range ordered {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("dbkit: apply stopped before module %q: %w", m.Name(), err)
		}
		if err := applyModule(ctx, db, dir, m); err != nil {
			return fmt.Errorf("dbkit: module %q: %w", m.Name(), err)
		}
	}
	return nil
}

// applyModule applies every not-yet-applied migration file m declares for
// the dialect subdirectory dir, inside one transaction: either every file
// newly applied by this call commits together with its schema_migrations
// row, or -- on any failure -- none of them do, leaving the module exactly
// as it was before this call.
func applyModule(ctx context.Context, db *gorm.DB, dir string, m pkgcore.Module) error {
	files, err := migrationFiles(m.Migrations(), dir)
	if err != nil {
		return err
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, f := range files {
			applied, err := isApplied(tx, m.Name(), f.name)
			if err != nil {
				return err
			}
			if applied {
				continue
			}
			if err := tx.Exec(string(f.contents)).Error; err != nil {
				return fmt.Errorf("apply %s: %w", f.name, err)
			}
			if err := recordApplied(tx, m.Name(), f.name); err != nil {
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
// This is the same depth-first-search, three-color traversal
// go/pkgcore/registry.go's sortModulesByDependency uses to order modules for
// Kernel.Bootstrap. dbkit reimplements it against pkgcore.Module directly,
// rather than importing that function, because it is unexported there --
// and even if it were exported, pkgcore must never gain a dependency on
// dbkit, so this one piece of logic has to be duplicated rather than
// shared. Register already rejects a duplicate module Name at registration
// time, so unlike pkgcore's version this one does not need to detect that
// case again.
func sortModulesByDependency(modules []pkgcore.Module) ([]pkgcore.Module, error) {
	byName := make(map[string]pkgcore.Module, len(modules))
	state := make(map[string]moduleVisitState, len(modules))
	for _, m := range modules {
		byName[m.Name()] = m
		state[m.Name()] = moduleUnvisited
	}

	ordered := make([]pkgcore.Module, 0, len(modules))
	visitPath := make([]string, 0, len(modules))

	var visit func(m pkgcore.Module) error
	visit = func(m pkgcore.Module) error {
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
