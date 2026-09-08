//go:build integration

// Package nats_test holds go/pkgcore/eventbus/nats's integration tier: tests
// that exercise EventBus against a real, JetStream-enabled NATS server. It is
// physically separate from the package's unit tests (all of which live in
// package nats itself, one file per source file, per the backend coding
// standard's testing layout rule) and carries the "integration" build tag: a
// plain "go test ./..." never compiles or runs anything in this directory;
// it is invoked explicitly with "go test -tags=integration ./...". This
// mirrors eventbus/redis/integration_test's identical convention exactly.
//
// Every test here spins up its own disposable NATS container and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path.
package nats_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	natslib "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// natsImage is the official NATS server image this tier runs. The
// testcontainers module's Run always adds "-js" to the container command
// (see its own nats.go), so every container this helper starts has
// JetStream enabled without this file naming the flag itself.
const natsImage = "nats:2.11.7"

// startNATSConn starts a disposable, JetStream-enabled NATS container and
// returns a *nats.Conn connected to it, the connection and the container
// already cleaned up via t.Cleanup on test completion (pass or fail), so
// nothing leaks past its owning test. Each call dials a fresh, independent
// client connection -- the direct analogue of eventbus/redis's
// startRedisClient, except every test that wants "two replicas" here calls
// this twice against the same container rather than sharing one connection,
// since nats.go's connection object (unlike go-redis's client) is the
// natural unit of "one replica's own link to the broker" the task's fan-out
// proof asks for.
func startNATSConn(t *testing.T, ctx context.Context) *natslib.Conn {
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

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("nats testcontainer connection string: %v", err)
	}
	conn, err := natslib.Connect(url)
	if err != nil {
		t.Fatalf("nats.Connect(%q): %v", url, err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// startNATSConnPair starts one disposable JetStream-enabled NATS container
// and returns two independent client connections to it -- standing in for
// two replicas of a deployment sharing one broker, the shape every test in
// this file that proves a cross-replica property needs.
func startNATSConnPair(t *testing.T, ctx context.Context) (*natslib.Conn, *natslib.Conn) {
	t.Helper()
	conns := startNATSConnN(t, ctx, 2)
	return conns[0], conns[1]
}

// startNATSConnN starts one disposable JetStream-enabled NATS container and
// returns n independent client connections to it -- standing in for n
// replicas of a deployment sharing one broker, all reachable through the
// same broker instance. A test that needs a connection distinct from every
// replica under test (a third-party publisher standing in for yet another
// replica, proving delivery does not merely retrace one bus's own
// synchronous local-publish path) asks for one more connection here rather
// than starting a second, unconnected container.
func startNATSConnN(t *testing.T, ctx context.Context, n int) []*natslib.Conn {
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

	url, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("nats testcontainer connection string: %v", err)
	}

	conns := make([]*natslib.Conn, n)
	for i := range conns {
		conn, err := natslib.Connect(url)
		if err != nil {
			t.Fatalf("nats.Connect(%q) [%d]: %v", url, i, err)
		}
		t.Cleanup(conn.Close)
		conns[i] = conn
	}
	return conns
}

// startNATSConnPairWithContainer is startNATSConnPair's counterpart for the
// tests that stop and restart their own container mid-test (the
// SurvivesRestart proofs, see declared_survives_restart_test.go): it hands
// back the container itself alongside two independent connections to it and
// differs from startNATSConnPair in the two ways those tests alone need:
//
//   - It connects with a short PingInterval/MaxPingsOutstanding, unlike
//     startNATSConnPair's plain default -- nats.go's own default
//     PingInterval is 2 minutes, so a client that sends nothing while the
//     server is down would not even notice the connection died within that
//     test's own timeout; the ping is what surfaces the failure quickly
//     enough for reconnection to kick in on a test-sized timescale, not a
//     change to what reconnection itself does once it notices. kv/nats's
//     own restart fixture connects the identical way, for the identical
//     reason.
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
func startNATSConnPairWithContainer(t *testing.T, ctx context.Context) (*tcnats.NATSContainer, *natslib.Conn, *natslib.Conn) {
	t.Helper()

	// testutil.StartOnFreeHostPort picks the pinned port: it draws one the
	// OS reports free and retries with a fresh draw when a concurrent
	// container start binds that port first, so parallel invocations of
	// this tier can never collide on the pin (free_host_port.go's doc
	// comment has the mechanism).
	var started *tcnats.NATSContainer
	hostPort := testutil.StartOnFreeHostPort(t, "nats testcontainer", func(p string) error {
		var err error
		started, err = tcnats.Run(ctx, natsImage,
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort("4222/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: p}},
				}
			}),
		)
		return err
	})
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(started); terminateErr != nil {
			t.Errorf("terminate nats testcontainer: %v", terminateErr)
		}
	})

	addr := "nats://" + net.JoinHostPort("127.0.0.1", hostPort)
	conns := make([]*natslib.Conn, 2)
	for i := range conns {
		conn, err := natslib.Connect(addr,
			natslib.PingInterval(200*time.Millisecond), natslib.MaxPingsOutstanding(2))
		if err != nil {
			t.Fatalf("nats.Connect(%q) [%d]: %v", addr, i, err)
		}
		t.Cleanup(conn.Close)
		conns[i] = conn
	}
	return started, conns[0], conns[1]
}

// restartNATSContainer stops and starts container and blocks until conn has
// reconnected to it, failing the test if the reconnect does not happen
// within 30s. The survives-restart proofs drive their container restart
// through this helper: the post-restart assertions must run against a
// reconnected connection, or they would fail on the dead connection rather
// than on genuinely lost data.
func restartNATSContainer(t *testing.T, ctx context.Context, container *tcnats.NATSContainer, conn *natslib.Conn) {
	t.Helper()

	reconnected := make(chan struct{}, 1)
	conn.SetReconnectHandler(func(*natslib.Conn) {
		select {
		case reconnected <- struct{}{}:
		default:
		}
	})

	if err := container.Stop(ctx, nil); err != nil {
		t.Fatalf("stop nats container: %v", err)
	}
	if err := container.Start(ctx); err != nil {
		t.Fatalf("restart nats container: %v", err)
	}

	select {
	case <-reconnected:
	case <-time.After(30 * time.Second):
		t.Fatal("client did not reconnect within 30s of the server restarting")
	}
}
