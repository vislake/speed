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
	"testing"

	natslib "github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
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
