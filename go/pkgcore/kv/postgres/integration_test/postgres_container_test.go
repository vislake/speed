//go:build integration

// Package postgres_test holds go/pkgcore/kv/postgres's integration tier:
// tests that exercise KVStore against a real PostgreSQL server. It is
// physically separate from the package's unit tests (all of which live in
// package postgres itself, one file per source file, per the backend
// coding standard's testing layout rule) and carries the "integration"
// build tag: a plain "go test ./..." never compiles or runs anything in
// this directory; it is invoked explicitly with "go test
// -tags=integration ./...". This mirrors kv/redis's own integration tier
// and eventbus/postgres/integration_test's identical PostgreSQL-container
// conventions.
//
// Every test here spins up its own disposable PostgreSQL container and
// requires a working Docker (or Docker-API-compatible) daemon; there is no
// fallback or skip-on-missing-Docker path, matching
// eventbus/postgres/integration_test/postgres_container_test.go exactly.
package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	kvpostgres "github.com/vislake/speed/go/pkgcore/kv/postgres"
)

// startPostgresPool starts a disposable PostgreSQL 16 container (the same
// image and wait strategy go/dbkit/dbtest.NewPostgres and
// eventbus/postgres/integration_test's own startPostgresPool use), applies
// this package's own EnsureSchema against it, and returns a ready-to-use
// *pgxpool.Pool. Container, pool and every Store a test builds off this
// pool are torn down via t.Cleanup on test completion (pass or fail).
func startPostgresPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("pkgcore"),
		tcpostgres.WithUsername("pkgcore"),
		tcpostgres.WithPassword("pkgcore"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres testcontainer connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := kvpostgres.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return pool
}
