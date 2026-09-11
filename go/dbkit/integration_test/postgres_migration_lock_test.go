//go:build integration

package dbkit_test

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres, exactly like postgres_tenant_isolation_test.go's
	// identical import.
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
	"github.com/vislake/speed/go/dbkit/internal/migrationfixture/basemodule"
	"github.com/vislake/speed/go/pkgcore"
)

// migrationLockReplicaCount is how many independent, real PostgreSQL
// connections TestMigrationRegistry_Apply_ConcurrentReplicas drives
// concurrently against the same fresh schema, simulating that many
// distributed-mode replicas each calling MigrationRegistry.Apply against the
// same shared database at first boot.
//
// The race this test targets -- two replicas' "already applied?" checks
// both seeing false for the same not-yet-applied file before either's
// CREATE TABLE commits -- has a narrow window: one network round trip to
// the same local Postgres container. A small replica count can leave every
// replica serializing through PostgreSQL's own catalog locking without any
// overlap, so the count is deliberately high enough that the uncoordinated
// shape collides reliably.
const migrationLockReplicaCount = 20

// migrationLockFakeModule is a minimal pkgcore.Module, local to this file,
// used only to drive MigrationRegistry.Apply with a real migration file
// (basemodule's) against a real database. It mirrors the parent package's
// own unit-tier fakeModule (migrations_test.go) exactly, redeclared here
// because that type is unexported and this package is a separate,
// black-box test binary.
type migrationLockFakeModule struct {
	migrations embed.FS
}

func (m migrationLockFakeModule) Name() string                              { return "base" }
func (m migrationLockFakeModule) DependsOn() []string                       { return nil }
func (m migrationLockFakeModule) Migrations() embed.FS                      { return m.migrations }
func (m migrationLockFakeModule) Locales() embed.FS                         { return embed.FS{} }
func (m migrationLockFakeModule) OpenAPISpec() []byte                       { return nil }
func (m migrationLockFakeModule) Register(*pkgcore.ComponentRegistry) error { return nil }

// isDuplicateObjectError reports whether err is (or wraps) a PostgreSQL
// error in the "two concurrent sessions tried to create the same object"
// family -- the failure two replicas' uncoordinated Apply calls produce
// when racing the same not-yet-applied migration file. Three distinct
// SQLSTATEs surface it, all treated the same way here:
//
//   - 42P07 "duplicate_table" -- CREATE TABLE racing itself directly.
//   - 42710 "duplicate_object" -- a handful of other DDL object kinds use
//     this code instead of 42P07.
//   - 23505 "unique_violation" -- concurrent CREATE TABLE statements for a
//     brand-new relation each insert a matching row into PostgreSQL's own
//     pg_type catalog as part of creating the table's row type, and two
//     concurrent inserts for the same type name collide on
//     pg_type's own (typname, typnamespace) unique index
//     ("pg_type_typname_nsp_index") -- the long-documented reason
//     "CREATE TABLE IF NOT EXISTS" is not actually safe under true
//     concurrency despite its name. gorm's postgres driver (TranslateError)
//     additionally wraps a 23505 as gorm.ErrDuplicatedKey ahead of the
//     original *pgconn.PgError (via fmt.Errorf's chained %w), which
//     errors.As below still unwraps down to.
func isDuplicateObjectError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "42P07", "42710", "23505":
		return true
	default:
		return false
	}
}

// TestMigrationRegistry_Apply_ConcurrentReplicas_PostgreSQL_NoDuplicateObjectFailure
// pins the multi-replica first-boot contract: N processes each calling
// Apply against the same shared PostgreSQL database at the same time must
// all succeed with exactly one convergent set of applied rows. Without
// migrations.go's cross-process coordination (the advisory lock Apply
// takes), two replicas could race their own "already applied?" checks for
// the same file and the loser's CREATE TABLE would fail with a
// duplicate-object error, failing that replica's boot outright.
//
// This test drives migrationLockReplicaCount real, independent
// *gorm.DB connections -- each opened through dbkit.Open exactly as an
// independent OS process would open its own -- all pointed at the same
// fresh, empty PostgreSQL schema, releases them from a shared start barrier
// so their first statements land as close together as real concurrent
// process starts would, and asserts every one of them succeeds with no
// duplicate-object error anywhere, and that exactly one convergent set of
// rows lands (never N copies, never a partial set). See AGENTS.md's
// "MigrationRegistry" section and migrations.go's own Apply doc comment for
// the coordination under test.
func TestMigrationRegistry_Apply_ConcurrentReplicas_PostgreSQL_NoDuplicateObjectFailure(t *testing.T) {
	ctx := context.Background()
	pgContainer := startPostgresContainer(t, ctx)
	adminDSN, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("sql.Open(admin) error = %v", err)
	}
	t.Cleanup(func() { _ = adminDB.Close() })
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatalf("admin connection ping: %v", err)
	}

	const schema = "migration_lock_race"
	mustExec(t, adminDB, ctx, "CREATE SCHEMA "+schema)

	// Every replica's DSN carries the same search_path as a run-time
	// connection parameter -- pgx treats any query-string key it does not
	// itself recognize as a startup run-time parameter to send the server,
	// so this is a real per-connection "SET search_path" with no dbkit code
	// involved -- pointing every one of the migrationLockReplicaCount
	// independent connections opened below at the exact same fresh schema,
	// so they genuinely race the same schema_migrations and base_items
	// tables rather than each getting an unraced schema of its own.
	replicaDSN := adminDSN + "&search_path=" + schema

	dbs := make([]*gorm.DB, migrationLockReplicaCount)
	for i := range dbs {
		db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectPostgres, DSN: replicaDSN})
		if err != nil {
			t.Fatalf("dbkit.Open replica %d: %v", i, err)
		}
		dbs[i] = db
		t.Cleanup(func(db *gorm.DB) func() {
			return func() {
				if sqlDB, err := db.DB(); err == nil {
					_ = sqlDB.Close()
				}
			}
		}(db))
	}

	start := make(chan struct{})
	errs := make([]error, migrationLockReplicaCount)
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func(i int, db *gorm.DB) {
			defer wg.Done()
			reg := dbkit.NewMigrationRegistry()
			if err := reg.Register(migrationLockFakeModule{migrations: basemodule.Migrations}); err != nil {
				errs[i] = fmt.Errorf("register: %w", err)
				return
			}
			<-start
			errs[i] = reg.Apply(ctx, db, dbkit.DialectPostgres)
		}(i, db)
	}
	close(start)
	wg.Wait()

	var duplicateObjectErrs, otherErrs []error
	for _, err := range errs {
		switch {
		case err == nil:
		case isDuplicateObjectError(err):
			duplicateObjectErrs = append(duplicateObjectErrs, err)
		default:
			otherErrs = append(otherErrs, err)
		}
	}

	if len(otherErrs) > 0 {
		t.Fatalf("%d of %d replicas failed with an unexpected (non-duplicate-object) error: %v",
			len(otherErrs), migrationLockReplicaCount, otherErrs)
	}
	if len(duplicateObjectErrs) > 0 {
		t.Fatalf("%d of %d replicas failed with a duplicate-object error racing Apply concurrently "+
			"(dbkit-tenancy P2-1: concurrent first-boot Apply calls must not race each other's CREATE "+
			"TABLE) -- want 0, first: %v",
			len(duplicateObjectErrs), migrationLockReplicaCount, duplicateObjectErrs[0])
	}

	// Every replica reported success. Convergence check: exactly one
	// schema_migrations row per (module, filename) pair -- two rows for
	// base's two migration files, never N copies from an uncoordinated
	// race, and never zero from a transaction that silently lost its own
	// commit.
	var migCount int64
	if err := dbs[0].Table("schema_migrations").Count(&migCount).Error; err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if migCount != 2 {
		t.Errorf("schema_migrations rows = %d, want exactly 2 (base declares two migration files, applied exactly once each)", migCount)
	}

	var seedRows int64
	if err := dbs[0].Table("base_items").Where("id = ?", "seed-0002").Count(&seedRows).Error; err != nil {
		t.Fatalf("count base_items seed row: %v", err)
	}
	if seedRows != 1 {
		t.Errorf("base_items rows with id = seed-0002 = %d, want exactly 1", seedRows)
	}
}
