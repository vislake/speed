//go:build integration

// Package nats_test holds go/pkgcore/kv/nats's integration tier: tests that
// exercise KVStore against a real NATS server with JetStream enabled. It is
// physically separate from the package's unit tests (all of which live in
// package nats itself, one file per source file, per the backend coding
// standard's testing layout rule) and carries the "integration" build tag: a
// plain "go test ./..." never compiles or runs anything in this directory;
// it is invoked explicitly with "go test -tags=integration ./...". This
// mirrors kv/redis's own integration tier exactly.
//
// Every test here spins up its own disposable NATS container and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path.
package nats_test

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// natsImage pins the container image every test in this package starts,
// matching the version testcontainers-go/modules/nats itself defaults
// RunContainer (the deprecated predecessor of Run) to, so the JetStream
// feature set this package's tests exercise -- Create, Update-by-revision,
// per-subject last-sequence enforcement -- is pinned rather than left to
// float with whatever "latest" resolves to on a given CI run.
const natsImage = "nats:2.11.7-alpine"

// startNATSConn starts a disposable NATS container with JetStream enabled
// (the module's own Run default command is "-DV -js") and returns a
// connected *nats.Conn, the client and the container already cleaned up via
// t.Cleanup on test completion (pass or fail), so nothing leaks past its
// owning test. Every integration test calls this for its own container,
// keeping tests isolated from one another at the cost of a few seconds of
// startup each -- the same trade-off kv/redis's own startRedisClient makes,
// and the same reason: kv/redis's integration tier carries an identical
// shape of helper for Redis, and this package's is not a copy of that one
// (different server, different module) so much as an independent instance of
// the same pattern.
func startNATSConn(t *testing.T, ctx context.Context) *nats.Conn {
	t.Helper()

	container, err := tcnats.Run(ctx, natsImage)
	if err != nil {
		t.Fatalf("start nats testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate nats testcontainer: %v", terminateErr)
		}
	})

	return connectToContainer(t, ctx, container)
}

// startNATSConnWithContainer is startNATSConn's counterpart for the one test
// that needs to stop and restart its own container mid-test
// (TestKVStore_ReconnectAfterServerRestart_ResumesOperation): it hands back
// the container itself alongside the connection, rather than closing over it
// invisibly, and differs from startNATSConn in two ways that test alone
// needs:
//
//   - It connects with a short PingInterval/MaxPingsOutstanding, unlike
//     startNATSConn's plain default -- nats.go's own default PingInterval is
//     2 minutes, so a client that sends nothing while the server is down
//     would not even notice the connection died within that test's own
//     timeout; the ping is what surfaces the failure quickly enough for
//     reconnection to kick in on a test-sized timescale, not a change to
//     what reconnection itself does once it notices.
//   - It publishes the container's client port to a fixed, rather than
//     random, host port. testcontainers' own random host-port allocation is
//     re-resolved on every container start (observed directly against this
//     environment's Docker Desktop: the same container's client port
//     answered on a *different* host port after Stop then Start), and
//     nats.go's reconnect logic only ever redials addresses it already
//     knows -- a client has no way to discover that the port it should
//     reconnect to just changed. A fixed host port sidesteps that platform
//     detail entirely, which is also the more realistic shape for what this
//     test means to prove: a real deployment's NATS server keeps a stable
//     advertised address across a restart, and it is exactly that stability
//     this package's reconnect behaviour depends on.
func startNATSConnWithContainer(t *testing.T, ctx context.Context) (*tcnats.NATSContainer, *nats.Conn) {
	t.Helper()

	// A random port in the high, rarely-reserved range on every run, so two
	// concurrent invocations of this one test are unlikely to collide; a
	// genuine collision fails loudly at container start (Docker refuses the
	// bind) rather than silently reusing someone else's server.
	hostPort := strconv.Itoa(40000 + rand.IntN(20000))

	fixedContainer, err := tcnats.Run(ctx, natsImage,
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("4222/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: hostPort}},
			}
		}),
	)
	if err != nil {
		t.Fatalf("start nats testcontainer on fixed port %s: %v", hostPort, err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(fixedContainer); terminateErr != nil {
			t.Errorf("terminate nats testcontainer: %v", terminateErr)
		}
	})

	return fixedContainer, connectToContainer(t, ctx, fixedContainer,
		nats.PingInterval(200*time.Millisecond), nats.MaxPingsOutstanding(2))
}

// connectToContainer dials container. opts extends nats.Connect's own
// default reconnect behaviour (reconnection enabled, a bounded number of
// attempts with a short wait between them) without replacing it: every test
// in this package either passes none (relying on the plain default) or --
// the one that does -- passes only the ping tuning
// startNATSConnWithContainer documents above.
func connectToContainer(t *testing.T, ctx context.Context, container *tcnats.NATSContainer, opts ...nats.Option) *nats.Conn {
	t.Helper()

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("nats testcontainer connection string: %v", err)
	}

	conn, err := nats.Connect(uri, opts...)
	if err != nil {
		t.Fatalf("nats.Connect(%q): %v", uri, err)
	}
	t.Cleanup(conn.Close)
	return conn
}
