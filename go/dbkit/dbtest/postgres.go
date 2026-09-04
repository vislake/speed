package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	// Blank-imported for its init side effect: registers
	// dbkit.DialectPostgres with dbkit's dialect registry so the dbkit.Open
	// call below has a driver to build a gorm.Dialector from. dbtest is a
	// test-only package every module's tests import, so bundling the
	// driver here is correct and intentional -- test binaries are never a
	// consumer's production dependency (see go/dbkit/AGENTS.md's "One
	// dependency" section).
	_ "github.com/vislake/speed/go/dbkit/dialect/postgres"
)

// NewPostgres returns a *gorm.DB (via dbkit.Open) backed by a real,
// per-call PostgreSQL instance started with testcontainers-go, for tests
// that need PostgreSQL-specific behavior (RLS, real constraint
// enforcement, dialect-specific SQL). Skips the test via t.Skip if Docker
// is not available in the environment -- callers should not need their
// own Docker-availability check. The container and connection are
// automatically torn down via t.Cleanup.
//
// NewPostgres applies no migration; see this package's doc comment for
// why, and for the pattern a caller uses to apply its own.
func NewPostgres(t *testing.T) *gorm.DB {
	t.Helper()

	ctx := context.Background()

	if err := dockerAvailable(ctx); err != nil {
		t.Skipf("dbtest: no Docker (or Docker-API-compatible) daemon available, skipping PostgreSQL-backed test: %v", err)
	}

	dsn := startPostgresContainer(t, ctx)

	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectPostgres,
		DSN:     dsn,
	})
	if err != nil {
		t.Fatalf("dbtest: dbkit.Open(DialectPostgres): %v", err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return
		}
		_ = sqlDB.Close()
	})

	return db
}

// startPostgresContainer starts a disposable PostgreSQL 16 container,
// registers its termination via t.Cleanup, and returns its connection
// string.
//
// This generalizes the container-startup logic dbkit's own
// integration_test/postgres_tenant_isolation_test.go already established
// (startPostgresContainer there) into a reusable, publicly importable
// form, rather than reinventing it: same image, same wait strategy, same
// disposable-container-per-call shape. It is called only after
// dockerAvailable has already confirmed a daemon is reachable, so a
// failure here is a real problem (a bad image, a broken daemon, ...), not
// "Docker is absent" -- that case never reaches this function at all.
func startPostgresContainer(t *testing.T, ctx context.Context) string {
	t.Helper()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("dbkit"),
		postgres.WithUsername("dbkit"),
		postgres.WithPassword("dbkit"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("dbtest: start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if termErr := testcontainers.TerminateContainer(pgContainer); termErr != nil {
			t.Errorf("dbtest: terminate postgres testcontainer: %v", termErr)
		}
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dbtest: postgres testcontainer connection string: %v", err)
	}
	return dsn
}
