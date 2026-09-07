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
	"math/rand/v2"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
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

// startPostgresPersistent is startPostgresPool's counterpart for the one
// test that stops and restarts its own container mid-test
// (TestKVStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart): it
// returns the container itself alongside the pool and publishes the
// PostgreSQL port to a fixed, rather than random, host port.
// testcontainers' own random host-port allocation is re-resolved on every
// container start (the same platform detail kv/nats's own restart fixture
// documents), and pgxpool only ever redials addresses it already knows, so
// a stable advertised address is what makes the reconnect after the restart
// deterministic.
func startPostgresPersistent(t *testing.T, ctx context.Context) (*tcpostgres.PostgresContainer, *pgxpool.Pool) {
	t.Helper()

	// A random port in the high, rarely-reserved range on every run, so two
	// concurrent invocations of this one test are unlikely to collide; a
	// genuine collision fails loudly at container start (Docker refuses the
	// bind) rather than silently reusing someone else's server.
	hostPort := strconv.Itoa(40000 + rand.IntN(20000))

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
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("5432/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: hostPort}},
			}
		}),
	)
	if err != nil {
		t.Fatalf("start postgres testcontainer on fixed port %s: %v", hostPort, err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})

	dsn := "postgres://pkgcore:pkgcore@127.0.0.1:" + hostPort + "/pkgcore?sslmode=disable"
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := kvpostgres.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return container, pool
}

// waitForPostgresReady polls the pool until the restarted server answers a
// Ping, or fails the test once the deadline passes. The restart closure of
// the survives-restart proof must not return before the server accepts
// connections, or the post-restart read would fail on a dead connection
// rather than on genuinely lost data.
func waitForPostgresReady(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := pool.Ping(probeCtx)
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("postgres server did not answer a Ping within 30s of its restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
