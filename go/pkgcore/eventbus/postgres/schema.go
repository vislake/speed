package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresmigrations "github.com/vislake/speed/go/pkgcore/eventbus/postgres/migrations"
)

// EnsureSchema applies this package's migrations/postgres/*.sql files
// against pool, in filename order, so a host can bring the outbox and
// cursor tables into existence before wiring NewEventBus.
//
// It is deliberately not dbkit.MigrationRegistry.Apply: go/pkgcore is the
// dependency floor every other module sits on (see root CLAUDE.md's module
// dependency direction), so it cannot import go/dbkit, which sits above
// it. EnsureSchema is this package's own, self-contained substitute, built
// on the same "no AutoMigrate, versioned SQL, idempotent" discipline
// (root CLAUDE.md's database-and-migrations rules) but without a
// schema_migrations ledger of its own: every statement in
// migrations/postgres/0001_create_eventbus_outbox.sql is already
// idempotent DDL (CREATE TABLE / CREATE INDEX, never CREATE TABLE without
// an existence guard), following the exact precedent
// go/dbkit/migrations.go's own doc comment describes for its
// schema_migrations bootstrap table: "the one table dbkit creates
// imperatively, with a plain, idempotent CREATE TABLE IF NOT EXISTS,
// every time Apply runs, instead of through a module's migration file."
// EnsureSchema takes that same shape for a different reason (a module
// boundary rather than a chicken-and-egg problem), so calling it more than
// once, or from more than one replica concurrently, is safe: every
// statement either succeeds once and is a silent no-op thereafter, or
// races another replica's identical statement and loses harmlessly.
//
// A host wanting its schema changes to flow through its own
// dbkit.MigrationRegistry instead may do so directly: dbkit is free to
// import pkgcore (only the reverse is forbidden), so a host can point a
// dbkit.MigrationModule's FS at this package's migrations.FS itself rather
// than calling EnsureSchema. EnsureSchema exists for the host that does
// not want to do that wiring just for this one seam.
//
// EnsureSchema is not called automatically by NewEventBus: the bus itself
// touches no network until the first Subscribe (matching
// eventbus/redis.NewEventBus's identical contract), and schema application
// is an explicit, separate step in this codebase's convention (dbkit's own
// Apply, saasctl's "db migrate" -- see root CLAUDE.md's Reference App
// section). Call it once, before the first Subscribe, typically right
// after building pool.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		panic("pkgcore/eventbus/postgres: EnsureSchema requires a non-nil *pgxpool.Pool")
	}

	entries, err := fs.ReadDir(postgresmigrations.FS, "postgres")
	if err != nil {
		return fmt.Errorf("pkgcore/eventbus/postgres: read embedded migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		contents, err := fs.ReadFile(postgresmigrations.FS, "postgres/"+name)
		if err != nil {
			return fmt.Errorf("pkgcore/eventbus/postgres: read migration %q: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			return fmt.Errorf("pkgcore/eventbus/postgres: apply migration %q: %w", name, err)
		}
	}
	return nil
}
