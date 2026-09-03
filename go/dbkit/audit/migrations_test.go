package audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// localPostgresDSN is the conventional local devcontainer/testcontainer
// Postgres endpoint this file opportunistically probes -- the same
// convention, and the same reasoning for using it, as go/dbkit's own
// migrations_test.go (see that file's runOnEachAvailableDialect doc
// comment): this file carries no build tag, so it must stay fast and
// hermetic on a machine with no local Postgres, which is the common case
// for a plain "go test ./...". dbkit's own copy of this helper is
// unexported and therefore unreachable from this subpackage, hence the
// duplication.
const localPostgresDSN = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"

// openLocalPostgresForTest attempts to open and ping localPostgresDSN
// within a short timeout, returning ok=false on any failure. See
// go/dbkit's own copy for the fuller rationale.
func openLocalPostgresForTest(t *testing.T) (db *gorm.DB, ok bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	db, err := gorm.Open(postgres.Open(localPostgresDSN), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, false
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, false
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, false
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db, true
}

// prepareMigrationsPostgresSchema creates a fresh, uniquely named Postgres
// schema and points db's search_path at it, so this test's tables --
// including schema_migrations itself -- can never collide with another
// test's, or with anything already present on a long-lived local Postgres
// instance. Mirrors go/dbkit's own migrations_test.go helper of the same
// shape.
func prepareMigrationsPostgresSchema(t *testing.T, db *gorm.DB) {
	t.Helper()

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	schema := fmt.Sprintf("dbkit_audit_migrations_test_%d", auditTestDBSeq.Add(1))
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if err := db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	if err := db.Exec("SET search_path TO " + schema).Error; err != nil {
		t.Fatalf("set search_path to %s: %v", schema, err)
	}
}

// newRawSQLiteForMigrationTest opens a private in-memory SQLite database
// with NO dbkit.Open wrapping -- no tenant-scoping plugin, no connection
// pool wiring -- because this file's own job is to prove the migration SQL
// files themselves apply cleanly, independent of whatever else dbkit.Open
// installs (which repository_test.go's openAuditTestDB already exercises
// together with real repository use).
func newRawSQLiteForMigrationTest(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:audit_migrations_test_%d?mode=memory&cache=shared", auditTestDBSeq.Add(1))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// runMigrationRoundTrip applies fakeAuditModule's migrations against db
// through the real dbkit.MigrationRegistry, then proves the resulting
// audit_events table is genuinely usable by driving a real Repository
// Insert/Get round trip through it -- not merely that Apply returned nil.
func runMigrationRoundTrip(t *testing.T, db *gorm.DB, dialect dbkit.Dialect) {
	t.Helper()

	reg := dbkit.NewMigrationRegistry()
	if err := reg.Register(fakeAuditModule{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := reg.Apply(context.Background(), db, dialect); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	// Calling Apply a second time must be a no-op, not a "table already
	// exists" failure -- this is what makes migrations safe to (re)run on
	// every process start.
	if err := reg.Apply(context.Background(), db, dialect); err != nil {
		t.Fatalf("second Apply() error = %v, want nil (already-applied files must be skipped, not re-executed)", err)
	}

	repo := NewRepository(db)
	evt := &AuditEvent{Action: "notes.note.create", TenantID: "tenant-a"}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "user-1", DisplayName: "Ada"})
	evt.SetResource(Resource{Type: "note", ID: "note-1", DisplayName: "Meeting notes"})
	evt.SetResult(Result{Success: true})

	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("Insert() against the migrated %s schema error = %v", dialect, err)
	}
	got, err := repo.Get(context.Background(), evt.ID)
	if err != nil {
		t.Fatalf("Get() against the migrated %s schema error = %v", dialect, err)
	}
	if got == nil || got.Action != evt.Action {
		t.Fatalf("Get() = %+v, want the inserted event back", got)
	}
}

func TestMigrations_Apply_SQLite_RoundTrip(t *testing.T) {
	runMigrationRoundTrip(t, newRawSQLiteForMigrationTest(t), dbkit.DialectSQLite)
}

// TestMigrations_Apply_Postgres_RoundTrip is the dual-dialect half of this
// round trip. Postgres is opportunistic, not required (see
// openLocalPostgresForTest's own doc comment): when none is reachable the
// subtest reports itself skipped, not failed, and the SQLite coverage
// above still ran and still proves the migration SQL is syntactically and
// semantically usable end to end. This mirrors go/dbkit's own
// migrations_test.go precedent for the identical reason.
func TestMigrations_Apply_Postgres_RoundTrip(t *testing.T) {
	db, ok := openLocalPostgresForTest(t)
	if !ok {
		t.Skip("no local Postgres reachable at the conventional local dev endpoint; " +
			"skipping (SQLite coverage above still ran)")
	}
	prepareMigrationsPostgresSchema(t, db)
	runMigrationRoundTrip(t, db, dbkit.DialectPostgres)
}
