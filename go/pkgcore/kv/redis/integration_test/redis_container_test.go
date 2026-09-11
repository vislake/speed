//go:build integration

// Package redis_test holds go/pkgcore/kv/redis's integration tier: tests
// that exercise KVStore against a real Redis server. It is physically
// separate from the package's unit tests (all of which live in package
// redis itself, one file per source file, per the backend coding standard's
// testing layout rule) and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...", the same
// layout-and-build-tag convention go/jobs/integration_test follows.
//
// Every test here spins up its own disposable Redis container and requires
// a working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path.
package redis_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// startRedisClient starts a disposable Redis 7 container and returns a
// go-redis client connected to it, the client and the container already
// cleaned up via t.Cleanup on test completion (pass or fail), so nothing
// leaks past its owning test. Every integration test calls this for its own
// container, keeping tests isolated from one another at the cost of a few
// seconds of startup each.
//
// eventbus/redis's own integration tier carries an identical copy of this
// helper: the two packages' integration tiers are independent of each other
// and neither owns a shared package worth introducing just for a dozen
// lines (the same packaging reasoning behind the duplicated construction
// helper each package carries).
func startRedisClient(t *testing.T, ctx context.Context) *redis.Client {
	t.Helper()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	options, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("redis.ParseURL(%q): %v", uri, err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	return client
}

// startRedisClientPair starts one disposable Redis 7 container and returns
// two independent go-redis clients connected to it -- standing in for two
// replicas of a deployment sharing one server. The shared contract suite
// (kvstoretest.AssertConforms) builds its stores through a two-instance
// factory, and its cross-instance assertions only mean anything when the
// two instances are genuinely independent connections, which is what this
// pair provides; the container and both clients are cleaned up via
// t.Cleanup on test completion.
func startRedisClientPair(t *testing.T, ctx context.Context) (*redis.Client, *redis.Client) {
	t.Helper()

	primary := startRedisClient(t, ctx)
	peer := redis.NewClient(primary.Options())
	t.Cleanup(func() { peer.Close() })
	return primary, peer
}

// startRedisPersistent starts a disposable Redis 7 container configured for
// the one test that stops and restarts its own container mid-test
// (TestKVStore_DeclaredSurvivesRestart_ProvenAgainstContainerRestart) and
// returns the container and a client connected to it. It differs from
// startRedisClient in the two ways that test alone needs:
//
//   - RDB snapshotting is forced to every second with at least one change
//     ("--save 1 1"): the stock container's snapshot cadence (a save once
//     an hour's changes accumulate) would not persist a single test key
//     before the restart, and the restart proof must rest on real
//     persistence, not on a shutdown-time save that could mask a backend
//     that only looks durable.
//   - The client port is published to a fixed, rather than random, host
//     port. testcontainers' own random host-port allocation is re-resolved
//     on every container start (the same platform detail kv/nats's own
//     restart fixture documents), and go-redis only ever redials addresses
//     it already knows, so a stable advertised address is what makes the
//     reconnect after the restart deterministic.
//
// The container and the client are cleaned up via t.Cleanup on test
// completion.
func startRedisPersistent(t *testing.T, ctx context.Context) (*tcredis.RedisContainer, *redis.Client) {
	t.Helper()

	// testutil.StartOnFreeHostPort picks the pinned port: it draws one the
	// OS reports free and retries with a fresh draw when a concurrent
	// container start binds that port first, so parallel invocations of
	// this tier can never collide on the pin (free_host_port.go's doc
	// comment has the mechanism).
	var started *tcredis.RedisContainer
	hostPort := testutil.StartOnFreeHostPort(t, "redis testcontainer", func(p string) error {
		var err error
		started, err = tcredis.Run(ctx, "redis:7-alpine",
			tcredis.WithSnapshotting(1, 1),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort("6379/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: p}},
				}
			}),
		)
		return err
	})
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(started); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	client := redis.NewClient(&redis.Options{Addr: net.JoinHostPort("127.0.0.1", hostPort)})
	t.Cleanup(func() { client.Close() })
	return started, client
}

// waitForRedisReady polls with a fresh client until the restarted server
// answers a PING, or fails the test once the deadline passes. The restart
// closure of the survives-restart proof must not return before the server
// accepts connections, or the post-restart read would fail on a dead
// connection rather than on genuinely lost data.
func waitForRedisReady(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()
	probe := redis.NewClient(client.Options())
	defer probe.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := probe.Ping(ctx).Err(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("redis server did not answer a PING within 30s of its restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
