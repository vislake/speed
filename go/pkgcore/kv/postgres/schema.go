package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresmigrations "github.com/vislake/speed/go/pkgcore/kv/postgres/migrations"
)

// EnsureSchema applies this package's migrations/postgres/*.sql files
// against pool, in filename order, so a host can bring pkgcore_kv_entries
// into existence before wiring NewKVStore.
//
// It is deliberately not dbkit.MigrationRegistry.Apply: go/pkgcore is the
// dependency floor every other module sits on (see root CLAUDE.md's module
// dependency direction), so it cannot import go/dbkit, which sits above it.
// EnsureSchema is this package's own, self-contained substitute, built on
// the same "no AutoMigrate, versioned SQL, idempotent" discipline (root
// CLAUDE.md's database-and-migrations rules) but without a
// schema_migrations ledger of its own: every statement in
// migrations/postgres/0001_create_pkgcore_kv_entries.sql is already
// idempotent DDL (CREATE TABLE / CREATE INDEX, always guarded with IF NOT
// EXISTS), following the exact precedent go/pkgcore/eventbus/postgres's own
// schema.go set for the identical module-boundary reason (itself following
// go/dbkit/migrations.go's own doc comment for its schema_migrations
// bootstrap table). Calling EnsureSchema more than once, or from more than
// one replica concurrently, is safe: every statement either succeeds once
// and is a silent no-op thereafter, or races another replica's identical
// statement and loses harmlessly.
//
// A host wanting its schema changes to flow through its own
// dbkit.MigrationRegistry instead may do so directly: dbkit is free to
// import pkgcore (only the reverse is forbidden), so a host can point a
// dbkit.MigrationRegistry-compatible pkgcore.Module's Migrations() at this
// package's migrations.FS itself rather than calling EnsureSchema.
// EnsureSchema exists for the host that does not want to do that wiring
// just for this one seam.
//
// EnsureSchema is not called automatically by NewKVStore: the store itself
// touches no network until its first operation (matching
// kv/redis.NewKVStore's identical contract), and schema application is an
// explicit, separate step in this codebase's convention (dbkit's own Apply,
// saasctl's "db migrate" -- see root CLAUDE.md's Reference App section).
// Call it once, before the first operation, typically right after building
// pool.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		panic("pkgcore/kv/postgres: EnsureSchema requires a non-nil *pgxpool.Pool")
	}

	entries, err := fs.ReadDir(postgresmigrations.FS, "postgres")
	if err != nil {
		return fmt.Errorf("pkgcore/kv/postgres: read embedded migrations: %w", err)
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
			return fmt.Errorf("pkgcore/kv/postgres: read migration %q: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			return fmt.Errorf("pkgcore/kv/postgres: apply migration %q: %w", name, err)
		}
	}
	return nil
}
