//go:build integration

// Package memcached_test holds go/pkgcore/kv/memcached's integration tier:
// tests that exercise KVStore against a real Memcached server. It is
// physically separate from the package's unit tests (all of which live in
// package memcached itself, one file per source file, per the backend
// coding standard's testing layout rule) and carries the "integration" build
// tag: a plain "go test ./..." never compiles or runs anything in this
// directory; it is invoked explicitly with "go test -tags=integration
// ./...". This mirrors kv/redis's own integration_test/ layout exactly.
//
// Every test here spins up its own disposable Memcached container and
// requires a working Docker (or Docker-API-compatible) daemon; there is no
// fallback or skip-on-missing-Docker path.
package memcached_test

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	tcmemcached "github.com/testcontainers/testcontainers-go/modules/memcached"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// startMemcachedClient starts a disposable Memcached container and returns a
// gomemcache client connected to it, the client's underlying connections and
// the container already cleaned up via t.Cleanup on test completion (pass or
// fail), so nothing leaks past its owning test. Every integration test calls
// this for its own container, keeping tests isolated from one another at the
// cost of a few seconds of startup each -- the same trade kv/redis's own
// startRedisClient makes.
func startMemcachedClient(t *testing.T, ctx context.Context) *memcache.Client {
	t.Helper()

	hostPort := startMemcachedContainer(t, ctx)
	client := memcache.New(hostPort)
	t.Cleanup(func() { client.Close() })
	return client
}

// startMemcachedContainer starts a disposable Memcached container, registers
// its termination on t.Cleanup, and returns its externally reachable
// host:port address. startMemcachedClient and startMemcachedClientPair both
// build their clients from it; the container is terminated once, on the
// owning test's completion.
func startMemcachedContainer(t *testing.T, ctx context.Context) string {
	t.Helper()

	_, hostPort := startMemcachedContainerWithHandle(t, ctx)
	return hostPort
}

// startMemcachedPersistent is startMemcachedContainer's counterpart for the
// one test that stops and restarts its own container mid-test
// (TestKVStore_DataDoesNotSurviveRestart_ConsistentWithItsHonestDeclaration):
// it hands back the container itself alongside the host:port address and
// publishes the Memcached port to a fixed, rather than random, host port.
// testcontainers' own random host-port allocation is re-resolved on every
// container start (the same platform detail kv/nats's own restart fixture
// documents), and a gomemcache client only ever redials addresses it
// already knows, so a stable advertised address is what makes the
// post-restart probe deterministic.
func startMemcachedPersistent(t *testing.T, ctx context.Context) (*tcmemcached.Container, string) {
	t.Helper()

	// testutil.StartOnFreeHostPort picks the pinned port: it draws one the
	// OS reports free and retries with a fresh draw when a concurrent
	// container start binds that port first, so parallel invocations of
	// this tier can never collide on the pin (free_host_port.go's doc
	// comment has the mechanism).
	var started *tcmemcached.Container
	hostPort := testutil.StartOnFreeHostPort(t, "memcached testcontainer", func(p string) error {
		var err error
		started, err = tcmemcached.Run(ctx, "memcached:1.6-alpine",
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort("11211/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: p}},
				}
			}),
		)
		return err
	})
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(started); terminateErr != nil {
			t.Errorf("terminate memcached testcontainer: %v", terminateErr)
		}
	})

	return started, net.JoinHostPort("127.0.0.1", hostPort)
}

// startMemcachedContainerWithHandle starts a disposable Memcached container,
// registers its termination on t.Cleanup, and returns the container and its
// externally reachable host:port address.
func startMemcachedContainerWithHandle(t *testing.T, ctx context.Context) (*tcmemcached.Container, string) {
	t.Helper()

	container, err := tcmemcached.Run(ctx, "memcached:1.6-alpine")
	if err != nil {
		t.Fatalf("start memcached testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate memcached testcontainer: %v", terminateErr)
		}
	})

	hostPort, err := container.HostPort(ctx)
	if err != nil {
		t.Fatalf("memcached testcontainer host port: %v", err)
	}
	return container, hostPort
}

// waitForMemcachedReady polls with a fresh client until the restarted server
// answers a read (a cache miss is an answer; a connection error is not), or
// fails the test once the deadline passes. The restart closure of the
// does-not-survive-restart proof must not return before the server accepts
// connections, or the post-restart read would fail on a dead connection
// rather than on genuinely lost data. ctx is accepted for signature
// symmetry with the other wait helpers; gomemcache's client carries no
// context on its plain Get.
func waitForMemcachedReady(t *testing.T, ctx context.Context, hostPort string) {
	t.Helper()
	probe := memcache.New(hostPort)
	defer probe.Close()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := probe.Get("memcached-ready-probe")
		if err == nil || errors.Is(err, memcache.ErrCacheMiss) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("memcached server did not answer a read within 30s of its restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// startMemcachedClientPair starts one disposable Memcached container and
// returns two independent gomemcache clients connected to it -- standing in
// for two replicas of a deployment sharing one server. The shared contract
// suite (kvstoretest.AssertConforms) builds its stores through a
// two-instance factory, and its cross-instance assertions only mean
// anything when the two instances are genuinely independent connections,
// which is what this pair provides; the container and both clients are
// cleaned up via t.Cleanup on test completion.
func startMemcachedClientPair(t *testing.T, ctx context.Context) (*memcache.Client, *memcache.Client) {
	t.Helper()

	hostPort := startMemcachedContainer(t, ctx)
	clientA := memcache.New(hostPort)
	clientB := memcache.New(hostPort)
	t.Cleanup(func() { clientA.Close() })
	t.Cleanup(func() { clientB.Close() })
	return clientA, clientB
}
