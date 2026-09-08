//go:build integration

// Package postgres_test holds go/pkgcore/eventbus/postgres's integration
// tier: tests that exercise EventBus against a real PostgreSQL server. It
// is physically separate from the package's unit tests (all of which live
// in package postgres itself, one file per source file, per the backend
// coding standard's testing layout rule) and carries the "integration"
// build tag: a plain "go test ./..." never compiles or runs anything in
// this directory; it is invoked explicitly with "go test
// -tags=integration ./...". This mirrors eventbus/redis's own integration
// tier and go/dbkit/dbtest's real-PostgreSQL container conventions.
//
// Every test here spins up its own disposable PostgreSQL container and
// requires a working Docker (or Docker-API-compatible) daemon; there is no
// fallback or skip-on-missing-Docker path, matching eventbus/redis's
// integration_test/redis_container_test.go exactly.
package postgres_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	eventbuspostgres "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// startPostgresPool starts a disposable PostgreSQL 16 container (the same
// image and wait strategy go/dbkit/dbtest.NewPostgres uses), applies this
// package's own EnsureSchema against it, and returns a ready-to-use
// *pgxpool.Pool. Container, pool and every EventBus a test builds off this
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

	if err := eventbuspostgres.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return pool
}

// startPostgresPersistent is startPostgresPool's counterpart for the one
// test that stops and restarts its own container mid-test
// (TestEventBus_DeclaredSurvivesRestart_DurableCursorAcrossServerRestart):
// it returns the container itself alongside the pool and publishes the
// PostgreSQL port to a fixed, rather than random, host port.
// testcontainers' own random host-port allocation is re-resolved on every
// container start (the same platform detail kv/nats's own restart fixture
// documents), and pgxpool only ever redials addresses it already knows, so
// a stable advertised address is what makes the reconnect after the restart
// deterministic. kv/postgres's own integration tier carries an identical
// fixture for its own restart proof.
func startPostgresPersistent(t *testing.T, ctx context.Context) (*tcpostgres.PostgresContainer, *pgxpool.Pool) {
	t.Helper()

	// testutil.StartOnFreeHostPort picks the pinned port: it draws one the
	// OS reports free and retries with a fresh draw when a concurrent
	// container start binds that port first, so parallel invocations of
	// this tier can never collide on the pin (free_host_port.go's doc
	// comment has the mechanism).
	var started *tcpostgres.PostgresContainer
	hostPort := testutil.StartOnFreeHostPort(t, "postgres testcontainer", func(p string) error {
		var err error
		started, err = tcpostgres.Run(ctx,
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
					network.MustParsePort("5432/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: p}},
				}
			}),
		)
		return err
	})
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(started); terminateErr != nil {
			t.Errorf("terminate postgres testcontainer: %v", terminateErr)
		}
	})

	dsn := "postgres://pkgcore:pkgcore@127.0.0.1:" + hostPort + "/pkgcore?sslmode=disable"
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := eventbuspostgres.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return started, pool
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
