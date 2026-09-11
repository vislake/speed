// Package redistest provides disposable Redis containers for integration
// tests: any module's tests (or a consuming application's) that need a
// real Redis server build their fixture through this package's
// constructors, so the image pin, the readiness handling and the cleanup
// discipline live in one place and every tier exercises the same server
// behavior.
//
// Every constructor starts its own container and terminates it via
// t.Cleanup when the test completes, pass or fail, so nothing leaks past
// its owning test; tests stay isolated from one another at the cost of a
// few seconds of container startup each. Callers need a working Docker
// (or Docker-API-compatible) daemon, and these constructors carry no
// skip-on-missing-Docker path: a tier that spins up real infrastructure
// fails loudly when it is missing rather than passing vacuously. (This
// package's own unit suite probes for a daemon and skips instead, so a
// plain `go test ./...` run without Docker stays green.)
//
// The package sits in pkgcore -- underneath every module -- so any
// module's integration tier can import it, and it deliberately holds
// nothing but container plumbing: the assertions a tier makes about the
// behavior under test stay in that tier.
package redistest

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

// Image pins the Redis release every container in this package runs. One
// pin for every tier means a behavior any tier observes is a behavior of
// the same server.
const Image = "redis:7-alpine"

// Start starts a disposable Redis container and returns it, terminated
// via t.Cleanup when the test completes (pass or fail). Callers that need
// their own client (or the container itself, e.g. to stop and restart it)
// take it from here; the common case is Client below.
func Start(t *testing.T, ctx context.Context) *tcredis.RedisContainer {
	t.Helper()

	container, err := tcredis.Run(ctx, Image)
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})
	return container
}

// Addr returns the container's "host:port" address as the Redis wire
// address callers configure into their service under test.
func Addr(t *testing.T, ctx context.Context, container *tcredis.RedisContainer) string {
	t.Helper()

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis testcontainer mapped port: %v", err)
	}
	return net.JoinHostPort(host, mappedPort.Port())
}

// Client starts a disposable Redis container and returns a go-redis
// client connected to it, the client and the container already cleaned up
// via t.Cleanup when the test completes.
func Client(t *testing.T, ctx context.Context) *redis.Client {
	t.Helper()

	container := Start(t, ctx)
	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	options, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("redis.ParseURL(%q): %v", uri, err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// ClientPair starts one disposable Redis container and returns two
// independent go-redis clients connected to it -- standing in for two
// replicas of a deployment sharing one server. Cross-instance assertions
// only mean anything when the two instances are genuinely independent
// connections, which is what this pair provides; the container and both
// clients are cleaned up via t.Cleanup when the test completes.
func ClientPair(t *testing.T, ctx context.Context) (*redis.Client, *redis.Client) {
	t.Helper()

	primary := Client(t, ctx)
	peer := redis.NewClient(primary.Options())
	t.Cleanup(func() { _ = peer.Close() })
	return primary, peer
}

// Persistent starts a disposable Redis container configured for tests
// that stop and restart their own container mid-test (a
// survives-restart proof) and returns the container and a client
// connected to it. It differs from Start in the two ways such a test
// needs:
//
//   - RDB snapshotting is forced to every second with at least one change
//     ("--save 1 1"): the stock container's snapshot cadence (a save once
//     an hour's changes accumulate) would not persist a single test key
//     before the restart, and the restart proof must rest on real
//     persistence, not on a shutdown-time save that could mask a backend
//     that only looks durable.
//   - The client port is published to a fixed, rather than random, host
//     port. testcontainers' own random host-port allocation is
//     re-resolved on every container start, and go-redis only ever
//     redials addresses it already knows, so a stable advertised address
//     is what makes the reconnect after the restart deterministic (the
//     retry loop that keeps the pin collision-free is
//     testutil.StartOnFreeHostPort's).
//
// The container and the client are cleaned up via t.Cleanup when the
// test completes.
func Persistent(t *testing.T, ctx context.Context) (*tcredis.RedisContainer, *redis.Client) {
	t.Helper()

	var started *tcredis.RedisContainer
	hostPort := testutil.StartOnFreeHostPort(t, "redis testcontainer", func(p string) error {
		var err error
		started, err = tcredis.Run(ctx, Image,
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
	t.Cleanup(func() { _ = client.Close() })
	return started, client
}

// WaitReady polls with a fresh probe client until the server answers a
// PING, or fails the test once the deadline passes. A restart closure
// must not return before the server accepts connections, or the
// post-restart read would fail on a dead connection rather than on
// genuinely lost data.
func WaitReady(t *testing.T, ctx context.Context, client *redis.Client) {
	t.Helper()

	probe := redis.NewClient(client.Options())
	defer func() { _ = probe.Close() }()

	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := probe.Ping(ctx).Err(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("redis server did not answer a PING within 30s")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
